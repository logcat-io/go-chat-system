// 데스크 화면 배선 테스트.
//
// desk.js 를 그대로 띄운다. 화면은 쓰는 만큼만 흉내 낸 가짜 요소이고, 서버(fetch)·소켓(WebSocket)·저장소 올리기(XMLHttpRequest)도 가짜다.
// 단언은 상담원이 보는 결과다 — 작성칸에 무엇이 보이는가, 입력칸에 무엇이 남는가, 무엇이 어느 대화로 나가는가, 어떤 알림이 뜨는가.
// ui.js 의 업로드 표·보낸 글 기억이 맞아도 desk.js 가 대화를 옮길 때·거절을 받을 때 그것을 부르지 않으면 화면은 그대로다. 그 연결을 본다.
import { test, describe, before, after, mock } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

register(
  "data:text/javascript," +
    encodeURIComponent(`export async function resolve(spec, ctx, next) {
      if (spec === "/chat.js" || spec === "/ui.js") return next(new URL("." + spec, ctx.parentURL).href, ctx);
      return next(spec, ctx);
    }`),
);

// ── 가짜 문서 ─────────────────────────────────────────────────

class FakeClassList {
  constructor() { this.s = new Set(); }
  add(...c) { c.forEach((x) => this.s.add(x)); }
  remove(...c) { c.forEach((x) => this.s.delete(x)); }
  toggle(c, force) {
    const on = force === undefined ? !this.s.has(c) : !!force;
    if (on) this.s.add(c);
    else this.s.delete(c);
    return on;
  }
  contains(c) { return this.s.has(c); }
}

// FakeEl 은 요소 하나다. innerHTML 은 글자로 기억하고, querySelector 는 그 글자에 있는 선택자면 작은 가짜 요소를 준다.
// querySelectorAll 은 받은함 줄(.conv-row)만 흉내 낸다. 그린 HTML 에서 줄마다 새 요소를 만들고 rows 에 기억한다.
class FakeEl {
  constructor(tag = "div", id = "") {
    this.tagName = tag.toUpperCase();
    this.id = id;
    this.children = [];
    this.dataset = {};
    this.attrs = {};
    this.listeners = {};
    this.classList = new FakeClassList();
    this.style = {};
    this.hidden = false;
    this.disabled = false;
    this.value = "";
    this.textContent = "";
    this.files = [];
    this.scrollTop = 0;
    this.scrollHeight = 0;
    this.clientHeight = 0;
    this._html = "";
    this._q = new Map();
  }
  get innerHTML() { return this._html; }
  set innerHTML(v) {
    this._html = String(v);
    this._q = new Map();
  }
  insertAdjacentHTML(pos, html) { this._html = pos === "afterbegin" ? html + this._html : this._html + html; }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  removeEventListener() {}
  dispatch(type, ev = {}) { for (const fn of this.listeners[type] || []) fn({ type, preventDefault() {}, stopPropagation() {}, target: this, ...ev }); }
  click() { this.dispatch("click"); }
  focus() {}
  before() {}
  requestSubmit() { this.dispatch("submit"); }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return this.attrs[k] ?? null; }
  appendChild(c) { this.children.push(c); c.parentNode = this; return c; }
  remove() {
    const list = this.parentNode?.children;
    if (list && list.includes(this)) list.splice(list.indexOf(this), 1);
  }
  querySelector(sel) {
    const token = sel.replace(/^[.#]/, "");
    if (!this.any && !this._html.includes(token)) return null;
    if (!this._q.has(sel)) {
      const stub = new FakeEl("span");
      stub.any = true;
      this._q.set(sel, stub);
    }
    return this._q.get(sel);
  }
  querySelectorAll(sel) {
    if (sel === "button" && /^menu/.test(this.className || "")) {
      this.buttons = [...this._html.matchAll(/<button[^>]*>([^<]*)<\/button>/g)].map(([, label]) => Object.assign(new FakeEl("button"), { textContent: label }));
      return this.buttons;
    }
    if (sel !== ".conv-row") return [];
    this.rows = [...this._html.matchAll(/class="conv-row[^"]*" data-id="([^"]+)"/g)].map(([, id]) => {
      const row = new FakeEl("button");
      row.dataset.id = id;
      return row;
    });
    return this.rows;
  }
  contains() { return false; }
  closest() { return null; }
  // 메뉴(ui.js menu)는 단추의 부모에 붙는다. 부모가 없으면 하나 만들어 준다.
  get parentElement() { return this.parentNode || (this._parent ||= new FakeEl("div")); }
}
// 메뉴는 그린 HTML 의 단추마다 요소를 만든다. 테스트는 menuButton("나에게") 로 찾고, 메뉴가 닫혔는지는 menuOpen() 으로 본다.
const menus = () => created.filter((e) => /^menu/.test(e.className || ""));
const menuOpen = (m) => !!m?.parentNode?.children.includes(m);

// created 는 문서가 만든 요소 전부다. 알림의 뿌리는 ui.js 가 처음 띄운 문서에 붙어 있어 문서마다 찾지 않고 여기서 찾는다.
const created = [];
const toastEls = () => created.filter((e) => /^toast /.test(e.className || ""));

function installDocument() {
  const ids = new Map();
  const doc = {
    referrer: "",
    visibilityState: "visible",
    body: new FakeEl("body"),
    documentElement: new FakeEl("html"),
    getElementById: (id) => {
      if (!ids.has(id)) ids.set(id, new FakeEl("div", id));
      return ids.get(id);
    },
    createElement: (tag) => {
      const e = new FakeEl(tag);
      created.push(e);
      return e;
    },
    querySelectorAll: () => [],
    addEventListener() {},
    removeEventListener() {},
  };
  globalThis.document = doc;
  // 받은함 필터 단추. desk.js 가 처음 뜰 때 이 단추들에 처리기를 단다. 테스트는 filterButton("deleted").click() 으로 바꾼다.
  const filters = doc.getElementById("filters");
  filters.buttons = ["all", "deleted"].map((f) => Object.assign(new FakeEl("button"), { dataset: { filter: f } }));
  filters.querySelectorAll = (sel) => (sel === "button[data-filter]" ? filters.buttons : []);
  return { doc, el: (id) => doc.getElementById(id) };
}

// ── 가짜 서버·소켓·저장소 ───────────────────────────────────────

const store = new Map();
globalThis.localStorage = {
  get length() { return store.size; },
  key: (i) => [...store.keys()][i] ?? null,
  getItem: (k) => (store.has(k) ? store.get(k) : null),
  setItem: (k, v) => store.set(k, String(v)),
  removeItem: (k) => store.delete(k),
};
globalThis.addEventListener = () => {};
globalThis.window = {};
globalThis.location = { search: "", origin: "http://chat.test", protocol: "http:", host: "chat.test" };
const clipboard = [];
Object.defineProperty(globalThis, "navigator", { value: { clipboard: { writeText: async (t) => clipboard.push(t) } }, configurable: true, writable: true });

const sockets = [];
globalThis.WebSocket = class {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  constructor(url) {
    this.url = url;
    this.readyState = 0;
    this.sent = [];
    sockets.push(this);
  }
  send(raw) { this.sent.push(JSON.parse(raw)); }
  close() {
    this.readyState = 3;
    this.onclose?.({ code: 1000 });
  }
};
const socket = () => sockets.at(-1);
function openSocket() {
  const ws = socket();
  ws.readyState = 1;
  ws.onopen?.();
  return ws;
}
const frame = (f) => socket().onmessage({ data: JSON.stringify(f) });
const sentOfType = (type) => sockets.flatMap((ws) => ws.sent).filter((f) => f.type === type);
const clearSent = () => sockets.forEach((ws) => (ws.sent.length = 0));

const xhrs = [];
globalThis.XMLHttpRequest = class {
  constructor() {
    this.upload = {};
    this.abortCalled = false;
    this.ignoreAbort = false;
    xhrs.push(this);
  }
  open(method, url) { this.url = url; }
  send(form) { this.form = form; }
  abort() {
    this.abortCalled = true;
    if (!this.ignoreAbort) this.onabort?.();
  }
  finish(status = 204) {
    this.status = status;
    this.responseText = "";
    this.onload?.();
  }
};

const REJECT = "첨부를 찾을 수 없거나 이미 보낸 첨부입니다";
const CONVS = [
  { id: "A", customer_id: "cA", customer_name: "가고객", state: "OPEN" },
  { id: "B", customer_id: "cB", customer_name: "나고객", state: "OPEN" },
];
// openFail[id] 이 있으면 그 대화 열기(GET /api/conversations/{id})가 그 상태로 실패한다. openHold[id] 가 있으면 그 약속이 풀릴 때까지 답하지 않는다.
const server = { tickets: 0, previews: [], attachments: 0, posted: [], openFail: {}, openHold: {}, deletedHold: {}, assignBodies: [], assignReply: null, gets: [], priorFail: 0, priorMessages: [], priorLimits: [] };
const json = (status, body, headers = {}) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...headers } });
globalThis.fetch = async (path, init = {}) => {
  const method = init.method || "GET";
  const p = path.split("?")[0];
  if (method === "GET") server.gets.push(p);
  // 소켓 티켓(D46). 세션 토큰 대신 소켓 주소에 실린다.
  // server.ticketExpired 면 세션이 서버에서 끝난 것이다(유휴 12시간·로그아웃·비활성) — 티켓 요청이 401 이다.
  if (p === "/api/ws-ticket" && server.ticketExpired) return json(401, { error: { code: "unauthorized", message: "세션이 만료되었거나 토큰이 올바르지 않습니다" } });
  // server.front401 이면 앞단(인증 게이트웨이·WAF)이 티켓과 받은함 요청을 401 HTML 로 막는다. 우리 서버의 판정이 아니다(감사 22).
  if ((p === "/api/ws-ticket" || p === "/api/agent/conversations") && server.front401) {
    return new Response("<html><body>401 Authorization Required</body></html>", { status: 401, headers: { "Content-Type": "text/html" } });
  }
  if (p === "/api/ws-ticket") return json(200, { ticket: `dwst-${++server.tickets}` });
  // 초대 미리보기는 본문으로 토큰을 받는다(D46).
  if (p === "/api/agent/invites/preview") {
    server.previews.push({ method, body: init.body ? JSON.parse(init.body) : null });
    return json(200, { name: "신입", role: "STAFF", email: "new@example.com" });
  }
  // 로그인. 로그인한 상담원은 server.meId(없으면 ag1)다.
  if (p === "/api/agent/login") return json(200, { token: `tok-${server.meId || "ag1"}-${++server.tickets}` });
  if (p === "/api/logout") return new Response(null, { status: 204 });
  if (p === "/api/me") return json(200, { agent_id: server.meId || "ag1", name: "나상담", permissions: server.mePermissions || ["conversation.assign"], workspace: { name: "W" } });
  if (p === "/api/agent/agents") return json(200, { agents: [] });
  if (p === "/api/agent/snippets") return json(200, { snippets: [] });
  if (p === "/api/agent/conversations") return json(200, { conversations: CONVS });
  if (p === "/api/agent/inbox/counts") return json(200, { todo: 0 });
  if (p === "/api/agent/tags") return json(200, { tags: [] });
  // 배정. server.assignReply 가 있으면 그 상태와 오류로 답한다(409 = 그 사이 바뀌었다). 없으면 그대로 바꾼다.
  let m = /^\/api\/agent\/conversations\/([^/]+)\/assign$/.exec(p);
  if (m) {
    const body = JSON.parse(init.body);
    server.assignBodies.push(body);
    if (server.assignReply) return json(server.assignReply.status, { error: server.assignReply.error }, server.assignReply.headers);
    const c = CONVS.find((x) => x.id === m[1]);
    return json(200, { ...c, assignee_agent_id: body.agent_id, assignee_name: body.agent_id ? "나상담" : undefined });
  }
  // 삭제한 상담 보기. server.deletedHold[id] 가 있으면 그 약속이 풀릴 때까지 답하지 않는다.
  m = /^\/api\/agent\/conversations\/([^/]+)\/deleted$/.exec(p);
  if (m) {
    if (server.deletedHold[m[1]]) await server.deletedHold[m[1]];
    return json(200, { conversation: CONVS.find((c) => c.id === m[1]), messages: [], deletion: { deleted_by_name: "관리자" } });
  }
  m = /^\/api\/conversations\/([^/]+)$/.exec(p);
  if (m && server.openHold[m[1]]) await server.openHold[m[1]];
  if (m && server.openFail[m[1]]) return json(server.openFail[m[1]], { error: { code: "internal", message: "처리하지 못했습니다. 잠시 후 다시 시도해 주세요" } });
  if (m) return json(200, CONVS.find((c) => c.id === m[1]));
  // 이전 상담. server.priorFail 이 남아 있으면 그만큼 500 으로 실패하고, 아니면 server.priorMessages 를 준다.
  if (/^\/api\/agent\/conversations\/[^/]+\/prior$/.test(p)) {
    server.priorLimits.push(Number(new URLSearchParams(path.split("?")[1] || "").get("limit")));
    if (server.priorFail > 0) {
      server.priorFail--;
      return json(500, { error: { code: "internal", message: "처리하지 못했습니다" } });
    }
    return json(200, { messages: server.priorMessages || [] });
  }
  if (/^\/api\/agent\/conversations\/[^/]+\/followers$/.test(p)) return json(200, { followers: [] });
  m = /^\/api\/customers\/([^/]+)$/.exec(p);
  if (m) return json(200, { id: m[1], display_name: m[1], conversations: 1, created_at: "2026-10-05T00:00:00Z" });
  m = /^\/api\/conversations\/([^/]+)\/attachments$/.exec(p);
  if (m) return json(201, { attachment: { id: `att-${++server.attachments}` }, upload: { url: "/files/upload", fields: { token: "t" } } });
  m = /^\/api\/attachments\/([^/]+)\/complete$/.exec(p);
  if (m) return json(201, { attachment: { id: m[1], filename: "clip.mp4", content_type: "video/mp4", byte_size: 100 } });
  m = /^\/api\/conversations\/([^/]+)\/messages$/.exec(p);
  if (m && method === "POST") {
    const body = JSON.parse(init.body);
    // BAD 로 시작하는 글은 서버가 영구 거절한다(남의 대화 첨부를 실은 경우와 같은 답)
    if (/^BAD/.test(body.body)) return json(400, { error: { code: "invalid", message: REJECT } });
    server.posted.push([m[1], body.body]);
    return json(201, { message: null });
  }
  return json(200, { messages: [] });
};

const settle = async () => {
  for (let i = 0; i < 30; i++) await new Promise((r) => setImmediate(r));
};

// 간격 타이머(받은함 수 세기·전송 재시도)는 멈춰 둔다. 테스트가 직접 부른다.
mock.timers.enable({ apis: ["setInterval"] });
store.set("gcs.agentToken", "tok-ag1");

// ── 데스크 ───────────────────────────────────────────────────────

describe("데스크 화면", () => {
  const { el } = installDocument();

  before(async () => {
    await import("./desk.js");
    await settle();
    openSocket();
  });
  after(() => {
    mock.timers.reset();
    mock.timers.enable({ apis: ["setInterval"] });
  });

  test("소켓 주소에는 상담원 세션 토큰이 실리지 않고 일회용 티켓만 실린다(D46)", () => {
    assert.ok(sockets.length >= 1, "소켓을 열지 않았다");
    for (const ws of sockets) {
      assert.match(ws.url, /\/ws\?ticket=dwst-\d+$/);
      assert.doesNotMatch(ws.url, /tok-ag1/, "세션 토큰이 주소에 실렸다 — 앞단 접근 로그에 남는다");
    }
  });

  // 좁은 화면(정책 826~828). 칸이 보이는지는 CSS 가 정한다(design-system.test.mjs). 여기서는 desk.js 가 거는 상태 — 데스크의 클래스,
  // 여는 단추의 aria-expanded, 막, 초점 — 를 본다.
  const desk = () => el("deskView").classList;
  test("좁은 화면은 처음에 받은함 목록이고 대화를 고르면 대화 칸으로 넘어가며 목록으로 단추를 누르면 목록으로 돌아온다", async () => {
    assert.ok(desk().contains("show-list"), "들어오자마자 목록이 아니다 — 900px 이하에서 목록에 닿을 길이 없다");
    await openConv("A");
    assert.ok(!desk().contains("show-list"), "대화를 골랐는데 목록에 머문다");
    el("backToList").click();
    assert.ok(desk().contains("show-list"), "목록으로 단추가 목록을 보이지 않는다");
  });

  test("메뉴 열기 단추는 받은함 필터 서랍을 열고 첫 항목으로 초점을 옮기며 Esc 로 닫으면 단추로 초점을 돌려준다 — 필터를 고르거나 막을 누르면 닫히고 고른 목록이 보인다", async () => {
    el("settingsMain").hidden = true; // 마크업처럼 처음에는 받은함 화면이다
    const focused = [];
    el("navToggle").focus = () => focused.push("navToggle");
    const first = Object.assign(el("filters").buttons[0], { focus: () => focused.push("first") });
    el("filters").querySelector = (sel) => (/^button/.test(sel) ? first : null);
    assert.equal(el("navToggle").getAttribute("aria-expanded"), null, "처음에는 닫혀 있다(마크업의 false)");

    el("navToggle").click();
    assert.ok(desk().contains("nav-open"), "서랍이 열리지 않는다");
    assert.equal(el("navToggle").getAttribute("aria-expanded"), "true");
    assert.equal(el("drawerScrim").hidden, false, "뒤를 덮는 막이 없다");
    assert.deepEqual(focused, ["first"], "초점이 서랍 첫 항목으로 가지 않는다");

    el("filters").dispatch("keydown", { key: "Escape" });
    assert.ok(!desk().contains("nav-open"), "Esc 로 닫히지 않는다");
    assert.equal(el("navToggle").getAttribute("aria-expanded"), "false");
    assert.equal(el("drawerScrim").hidden, true);
    assert.equal(focused.at(-1), "navToggle", "닫은 뒤 초점이 여는 단추로 돌아오지 않는다");

    // 대화를 보던 중 필터를 고르면 서랍이 닫히고 그 목록이 보인다
    await openConv("A");
    el("navToggle").click();
    first.click();
    await settle();
    assert.ok(!desk().contains("nav-open"), "필터를 골라도 서랍이 열려 있다");
    assert.ok(desk().contains("show-list"), "고른 필터의 목록이 보이지 않는다");

    el("navToggle").click();
    el("drawerScrim").click();
    assert.ok(!desk().contains("nav-open"), "막을 눌러도 닫히지 않는다");
    el("navToggle").click();
    el("navToggle").click();
    assert.ok(!desk().contains("nav-open"), "단추를 다시 눌러도 닫히지 않는다");
  });

  test("고객 정보 단추는 고객 정보 서랍을 열고 Esc 로 닫으면 단추로 초점을 돌려준다 — 서랍은 한 번에 하나만 열린다", () => {
    const focused = [];
    el("navToggle").focus = () => {};
    el("profileToggle").focus = () => focused.push("profileToggle");
    el("profileToggle").click();
    assert.ok(desk().contains("profile-open"), "고객 정보 서랍이 열리지 않는다");
    assert.equal(el("profileToggle").getAttribute("aria-expanded"), "true");

    el("navToggle").click();
    assert.ok(desk().contains("nav-open") && !desk().contains("profile-open"), "서랍 둘이 같이 열렸다");
    assert.equal(el("profileToggle").getAttribute("aria-expanded"), "false");
    el("navToggle").dispatch("keydown", { key: "Escape" });
    assert.ok(!desk().contains("nav-open"), "여는 단추에 초점이 있을 때 Esc 로 닫히지 않는다");

    el("profileToggle").click();
    el("profile").dispatch("keydown", { key: "Escape" });
    assert.ok(!desk().contains("profile-open"), "Esc 로 닫히지 않는다");
    assert.equal(el("profileToggle").getAttribute("aria-expanded"), "false");
    assert.equal(focused.at(-1), "profileToggle");
  });

  async function openConv(id) {
    await settle();
    const row = el("convList").rows?.find((r) => r.dataset.id === id);
    assert.ok(row, `받은함에 ${id} 가 없다`);
    row.click();
    await settle();
  }
  function pickFile() {
    el("file").files = [new File(["\x00\x00\x00\x18ftypisom"], "clip.mp4", { type: "video/mp4" })];
    el("file").dispatch("change");
  }
  function submit(text) {
    el("input").value = text;
    el("composer").dispatch("submit");
  }

  test("업로드 취소 단추를 누르면 올리기를 멈추고 작성칸이 원래대로 돌아간다", async () => {
    await openConv("A");
    pickFile();
    await settle();
    assert.match(el("composerHint").innerHTML, /id="cancelUpload"/, "올리는 중에 취소 단추가 있다");
    const xhr = xhrs.at(-1);
    el("cancelUpload").click();
    await settle();
    assert.equal(xhr.abortCalled, true, "저장소 올리기를 끊는다");
    assert.equal(el("composerHint").innerHTML, "", "작성칸이 원래대로 돌아간다");
    xhr.finish(204);
    await settle();
    assert.doesNotMatch(el("composerHint").innerHTML, /clip\.mp4/, "취소한 첨부가 붙는다");
  });

  test("업로드 중 다른 상담으로 옮기면 그 첨부는 옮긴 상담의 작성칸에 붙지 않고 업로드를 멈춘다", async () => {
    pickFile();
    await settle();
    const xhr = xhrs.at(-1);
    xhr.ignoreAbort = true; // 이미 다 보낸 뒤라 끊어도 끝까지 가는 경우
    await openConv("B");
    assert.equal(xhr.abortCalled, true, "옮기면 올리던 것을 멈춘다");
    assert.equal(el("composerHint").innerHTML, "", "앞 대화의 올리는 중 표시가 옮긴 대화 작성칸에 남는다");
    xhr.finish(204);
    await settle();
    assert.doesNotMatch(el("composerHint").innerHTML, /clip\.mp4/, "A 의 첨부가 B 작성칸에 붙는다");
    clearSent();
    submit("B 고객에게 보내는 답");
    await settle();
    const sends = sentOfType("send");
    assert.equal(sends.length, 1);
    assert.equal(sends[0].conversation_id, "B");
    assert.equal(sends[0].attachment_id, undefined, "A 의 첨부를 B 메시지에 싣는다");
    frame({ type: "ack", conversation_id: "B", client_msg_id: sends[0].client_msg_id, seq: 1 });
  });

  test("보내기가 영구 거절되면 쓴 글을 비어 있는 입력칸에 되돌리고 거절 까닭을 보인다", async () => {
    clearSent();
    submit("BAD B 고객에게 쓴 긴 답");
    await settle();
    const [sent] = sentOfType("send");
    assert.equal(el("input").value, "", "보내면 입력칸이 빈다");
    frame({ type: "error", code: "invalid", conversation_id: "B", client_msg_id: sent.client_msg_id, message: REJECT });
    assert.equal(el("input").value, "BAD B 고객에게 쓴 긴 답", "쓴 답이 사라진다");
    assert.match(toastEls().at(-1).innerHTML, new RegExp(`${REJECT}.*입력칸에 되돌렸습니다`));
    el("input").value = "";
  });

  test("다른 대화로 옮긴 뒤 앞 대화 글의 거절이 소켓으로 오면 까닭만 적힌 알림을 띄우지 않는다 — 그 글은 다시 나가고 거절되면 글과 함께 한 번 알린다", async () => {
    await openConv("A");
    clearSent();
    submit("BAD A 고객에게 쓴 답");
    await settle();
    const [sent] = sentOfType("send");
    assert.equal(sent.conversation_id, "A");
    await openConv("B");
    const before = toastEls().length;
    frame({ type: "error", code: "invalid", conversation_id: "A", client_msg_id: sent.client_msg_id, message: REJECT });
    assert.equal(toastEls().length, before, "글 없이 까닭만 적힌 알림이 뜬다");
    mock.timers.tick(3000); // REST 주기가 A 큐를 다시 민다. 서버가 영구 거절한다
    await settle();
    const after = toastEls().slice(before);
    assert.equal(after.length, 1, "거절을 한 번만 알린다");
    assert.match(after[0].innerHTML, new RegExp(`${REJECT}.*다시 열면`));
  });

  // 가장 무거운 경우다. A 고객에게 쓴 답이 B 작성칸에 들어가면 상담원이 모르고 B 고객에게 보낸다.
  test("다른 대화에서 거절된 글은 그 대화를 열 때 입력칸에 들어가고 지금 입력칸에는 들어가지 않는다", async () => {
    assert.equal(el("input").value, "", "A 고객에게 쓴 답이 B 입력칸에 들어간다");
    clearSent();
    submit("B 고객에게 쓰는 새 답");
    await settle();
    assert.deepEqual(sentOfType("send").map((f) => [f.conversation_id, f.body]), [["B", "B 고객에게 쓰는 새 답"]], "A 의 답이 B 고객에게 나간다");
    frame({ type: "ack", conversation_id: "B", client_msg_id: sentOfType("send")[0].client_msg_id, seq: 2 });
    await openConv("A");
    assert.equal(el("input").value, "BAD A 고객에게 쓴 답", "A 를 열면 쓴 답이 입력칸에 돌아온다");
    el("input").value = "";
  });

  // 감사 9, A2-0580: 열기가 500 으로 실패하자 머리·작성칸·구독이 A 에 남았다. 상담원은 B 를 눌렀으니 B 고객에게 쓴다고 믿는다.
  // 불변: 화면에 보이는 대화와 보내기 대상이 다를 수 없다. 보이는 대화가 없으면 아무것도 나가지 않는다.
  const subscribedTo = () => sentOfType("subscribe").at(-1)?.conversation_id;
  test("대화 열기가 실패하면 앞 대화로 되돌리지 않는다 — 머리·작성칸·구독을 비우고 오류와 다시 시도를 둔다", async () => {
    await openConv("A");
    assert.equal(el("threadTitle").textContent, "가고객");
    server.openFail.B = 500;
    await openConv("B");
    assert.notEqual(el("threadTitle").textContent, "가고객", "머리가 A 에 남는다");
    assert.match(el("threadTitle").textContent, /열지 못했습니다/);
    assert.equal(el("input").disabled, true, "작성칸이 A 에게 열려 있다");
    assert.equal(el("send").disabled, true);
    assert.match(el("msgs").innerHTML, /다시 시도/);
    clearSent();
    submit("B 고객에게 쓴다고 믿는 답");
    await settle();
    assert.deepEqual(sentOfType("send"), [], "실패한 뒤 쓴 답이 앞 대화(A) 고객에게 나간다");
    frame({ type: "message", conversation_id: "A", seq: 9, id: "a9", kind: "MESSAGE", sender_kind: "CUSTOMER", sender_name: "가고객", body: "A 의 새 글", created_at: new Date().toISOString() });
    assert.doesNotMatch(el("msgs").innerHTML, /A 의 새 글/, "놓은 대화의 메시지가 빈 화면에 붙는다");
    // 다시 시도 — 이번엔 열린다
    delete server.openFail.B;
    el("input").value = "";
    el("msgs").querySelector("#retryOpen").click();
    await settle();
    assert.equal(el("threadTitle").textContent, "나고객");
    assert.equal(el("input").disabled, false);
    assert.equal(subscribedTo(), "B");
    clearSent();
    submit("B 고객에게 쓰는 답");
    await settle();
    assert.deepEqual(sentOfType("send").map((f) => f.conversation_id), ["B"]);
    frame({ type: "ack", conversation_id: "B", client_msg_id: sentOfType("send")[0].client_msg_id, seq: 3 });
  });

  test("열기를 시작하면 응답이 올 때까지 작성칸을 잠그고 머리를 불러오는 중으로 둔다", async () => {
    let release;
    server.openHold.A = new Promise((r) => (release = r));
    const row = el("convList").rows?.find((r) => r.dataset.id === "A");
    row.click();
    await settle();
    assert.match(el("threadTitle").textContent, /불러오는 중/, "응답 전인데 머리가 B 다");
    assert.equal(el("input").disabled, true, "응답 전인데 B 에게 보낼 수 있다");
    clearSent();
    submit("아직 열리지 않은 대화에 쓴 답");
    await settle();
    assert.deepEqual(sentOfType("send"), [], "열리는 중에 쓴 답이 B 고객에게 나간다");
    delete server.openHold.A;
    release();
    await settle();
    assert.equal(el("threadTitle").textContent, "가고객");
    assert.equal(subscribedTo(), "A");
  });

  test("연속으로 눌렀을 때 늦게 온 앞 대화의 응답은 버린다 — 마지막에 누른 대화가 열린다", async () => {
    let release;
    server.openHold.A = new Promise((r) => (release = r));
    await openConv("B"); // A 를 다시 연 뒤 B 로 — 아래에서 A 를 다시 누른다
    const a = el("convList").rows?.find((r) => r.dataset.id === "A");
    a.click(); // A 응답은 묶여 있다
    await settle();
    await openConv("B"); // B 는 바로 열린다
    assert.equal(el("threadTitle").textContent, "나고객");
    delete server.openHold.A;
    release(); // 이제 A 의 응답이 늦게 온다
    await settle();
    assert.equal(el("threadTitle").textContent, "나고객", "늦게 온 A 응답이 머리를 덮는다");
    assert.equal(subscribedTo(), "B", "늦게 온 A 응답이 구독을 가져간다");
    clearSent();
    submit("B 고객에게 보내는 답");
    await settle();
    assert.deepEqual(sentOfType("send").map((f) => f.conversation_id), ["B"], "보이는 대화는 B 인데 다른 대화로 나간다");
    frame({ type: "ack", conversation_id: "B", client_msg_id: sentOfType("send")[0].client_msg_id, seq: 4 });
    // 늦게 온 앞 대화의 응답이 실패여도 같다 — 열린 B 를 "열지 못했다" 로 덮지 않는다(감사 10, N29)
    let releaseFail;
    server.openHold.A = new Promise((r) => (releaseFail = r));
    server.openFail.A = 500;
    el("convList").rows?.find((r) => r.dataset.id === "A").click();
    await settle();
    await openConv("B");
    delete server.openHold.A;
    releaseFail();
    await settle();
    delete server.openFail.A;
    assert.equal(el("threadTitle").textContent, "나고객", "늦게 온 A 의 실패가 열린 B 의 머리를 덮는다");
    assert.doesNotMatch(el("msgs").innerHTML, /다시 시도/, "늦게 온 A 의 실패가 B 화면을 오류로 바꾼다");
    assert.equal(el("input").disabled, false);
  });

  test("보낸 답이 재시도 상한으로 포기되면 아직 보내지 않은 첨부를 같은 대화 작성칸에 다시 붙이고, 붙일 수 없으면 파일 이름을 들어 알린다", async () => {
    await openConv("B");
    pickFile();
    await settle();
    xhrs.at(-1).finish(204);
    await settle();
    assert.match(el("composerHint").innerHTML, /clip\.mp4/);
    clearSent();
    submit("파일 보낸다");
    await settle();
    const [sent] = sentOfType("send");
    assert.ok(sent.attachment_id);
    for (let i = 0; i < 20; i++) frame({ type: "error", code: "conflict", conversation_id: "B", client_msg_id: sent.client_msg_id, message: "이미 있습니다" });
    assert.equal(el("input").value, "파일 보낸다");
    assert.match(el("composerHint").innerHTML, /clip\.mp4/, "첨부를 잃는다");
    clearSent();
    el("composer").dispatch("submit");
    await settle();
    const [again] = sentOfType("send");
    assert.equal(again.attachment_id, sent.attachment_id);
    frame({ type: "error", code: "invalid", conversation_id: "B", client_msg_id: again.client_msg_id, message: REJECT });
    assert.doesNotMatch(el("composerHint").innerHTML, /clip\.mp4/, "쓸 수 없는 첨부를 다시 붙인다");
    assert.match(toastEls().at(-1).innerHTML, /파일 clip\.mp4 은 보내지 못했습니다\. 다시 올려 주세요/);
    el("input").value = "";
  });

  test("보낸 답이 오래 ack 를 못 받으면 보내는 중 줄 아래에 다시 연결되면 보낸다고 알린다", async () => {
    // B 의 저장된 큐에 오래된 답이 있다(끊긴 동안 쓴 것). 소켓을 끊고 B 를 다시 연다
    store.set("gcs.v1.agent:ag1.B", JSON.stringify({ lastSeq: 4, pending: [{ client_msg_id: "old-1", body: "끊긴 동안 쓴 답", at: Date.now() - 60000, tries: 0 }] }));
    socket().close();
    await openConv("A");
    await openConv("B");
    assert.match(el("pending").innerHTML, /끊긴 동안 쓴 답/);
    assert.match(el("pending").innerHTML, /연결이 끊겨 있습니다\. 다시 연결되면 보냅니다/, "오래 기다리는 답에 상태 안내가 없다");
    openSocket();
    frame({ type: "ack", conversation_id: "B", client_msg_id: "old-1", seq: 5 });
    assert.doesNotMatch(el("pending").innerHTML, /다시 연결되면/, "보낸 뒤에도 안내가 남는다");
  });

  // DEF-01(실사용 v0.0.2 U295): 일시 오류에 "잠시 뒤 다시 시도해라" 토스트가 떠 사람이 다시 쓰자 같은 글이 두 줄 저장됐다.
  test("일시 오류 프레임을 받은 글은 토스트 없이 보내는 중으로 남고 다시 나가 정확히 한 번 저장된다 — 영구 거절은 지금처럼 알리고 글을 돌려준다", async () => {
    await openConv("B");
    el("input").value = "";
    const before = toastEls().length;
    clearSent();
    submit("DB 가 잠깐 끊긴 순간 쓴 답");
    await settle();
    const item = sentOfType("send").at(-1);
    assert.equal(item.conversation_id, "B");
    frame({ type: "error", code: "internal", conversation_id: "B", client_msg_id: item.client_msg_id, message: "처리하지 못했습니다. 잠시 후 다시 시도해 주세요" });
    await settle();
    assert.equal(toastEls().length, before, "포기하지 않은 답에 실패 토스트를 띄우면 상담원이 다시 써서 두 번 저장된다");
    assert.match(el("pending").innerHTML, /DB 가 잠깐 끊긴 순간 쓴 답/, "보내는 중으로 남는다");
    assert.equal(el("input").value, "");
    // 소켓이 다시 준비되면 같은 client_msg_id 로 다시 나간다. 서버는 client_msg_id 로 멱등이라 한 번만 저장한다
    frame({ type: "ready", conversation_id: "B", last_seq: 5 });
    await settle();
    const sends = sentOfType("send").filter((f) => f.body === "DB 가 잠깐 끊긴 순간 쓴 답");
    assert.ok(sends.length >= 2, "다시 나가야 한다");
    assert.deepEqual([...new Set(sends.map((f) => f.client_msg_id))], [item.client_msg_id], "다른 client_msg_id 로 나가면 두 번 저장된다");
    frame({ type: "ack", conversation_id: "B", client_msg_id: item.client_msg_id, seq: 6 });
    await settle();
    assert.doesNotMatch(el("pending").innerHTML, /DB 가 잠깐 끊긴 순간 쓴 답/);
    assert.equal(toastEls().length, before, "끝까지 실패 알림이 없다");

    // 영구 거절은 지금처럼 알리고 글을 돌려준다
    submit("BAD 영구 거절될 답");
    await settle();
    const bad = sentOfType("send").at(-1);
    frame({ type: "error", code: "invalid", conversation_id: "B", client_msg_id: bad.client_msg_id, message: REJECT });
    await settle();
    assert.equal(toastEls().length, before + 1);
    assert.equal(el("input").value, "BAD 영구 거절될 답");
    el("input").value = "";
  });

  test("글과 무관한 일시 오류 프레임은 토스트를 띄우지 않는다 — 구독이 붙지 않았으면 다시 구독하고 읽음은 다시 보낸다", async () => {
    const before = toastEls().length;
    frame({ type: "error", code: "internal", conversation_id: "B", message: "처리하지 못했습니다. 잠시 후 다시 시도해 주세요" });
    frame({ type: "error", code: "too_many", message: "요청이 너무 많다" });
    await settle();
    assert.equal(toastEls().length, before);
  });

  // DEF-72(감사 13): 대화를 여는 순간 DB 가 300ms 끊겨 구독이 일시 오류로 실패했다. 서버는 연결을 유지한 채 구독을 붙이지 않았고,
  // 클라이언트는 다시 구독하지 않아 복구 뒤 고객이 보낸 글이 화면에 영영 오지 않았다(25초 넘게).
  test("구독이 일시 오류로 실패해도 복구 뒤 그 대화의 새 메시지가 화면에 온다", async () => {
    // 간격 타이머는 이미 흉내 중이다. 시계만 앞으로 돌린다
    const realNow = Date.now;
    let skew = 0;
    Date.now = () => realNow() + skew;
    try {
      const before = toastEls().length;
      await openConv("A");
      clearSent();
      await openConv("B");
      assert.equal(sentOfType("subscribe").filter((f) => f.conversation_id === "B").length, 1);
      frame({ type: "error", code: "internal", message: "처리하지 못했습니다. 잠시 후 다시 시도해 주세요" }); // 권한 확인이 DB 오류로 실패했다
      await settle();
      // DB 가 돌아왔다. 1초마다 확인하는 주기가 백오프(3초 안팎) 뒤에 다시 구독한다
      for (let i = 0; i < 4; i++) {
        skew += 1000;
        mock.timers.tick(1000);
        await settle();
      }
      const subs = sentOfType("subscribe").filter((f) => f.conversation_id === "B");
      assert.equal(subs.length, 2, "구독이 실패한 뒤 다시 구독하지 않는다 — 그 대화의 새 메시지가 영영 오지 않는다");
      const last = subs[1].after_seq;
      frame({ type: "ready", conversation_id: "B", last_seq: last });
      frame({ type: "message", conversation_id: "B", seq: last + 1, id: "b-late", kind: "MESSAGE", sender_kind: "CUSTOMER", sender_name: "나고객", body: "복구 뒤 고객 글", created_at: new Date().toISOString() });
      await settle();
      assert.match(el("msgs").innerHTML, /복구 뒤 고객 글/, "복구 뒤 쓴 고객 글이 화면에 오지 않는다");
      assert.equal(toastEls().length, before, "다시 시도하는 실패에 토스트를 띄운다");
    } finally {
      Date.now = realNow;
    }
  });

  // 정책 661 의 데스크 절반. 감사 19 W9: ui.js 의 createLog 기본 시각을 24시간으로 바꿔도 테스트가 통과했다(위젯 쪽만 봤다).
  // 데스크는 createLog 에 시각 함수를 넘기지 않으므로 기본값이 데스크의 모양이다. 상담원 화면이 받아 그린 말풍선을 본다.
  test("위젯 말풍선의 시각은 24시간 14:15 꼴이다 — 데스크는 오전·오후 그대로다", async () => {
    await openConv("A");
    clearSent();
    await openConv("B");
    const last = sentOfType("subscribe").filter((f) => f.conversation_id === "B").at(-1).after_seq;
    frame({ type: "ready", conversation_id: "B", last_seq: last });
    const pm = new Date(2026, 9, 6, 14, 15).toISOString(); // 이 기기 시간대의 오후 2시 15분
    const am = new Date(2026, 9, 6, 9, 5).toISOString();
    frame({ type: "message", conversation_id: "B", seq: last + 1, id: "b-pm", kind: "MESSAGE", sender_kind: "CUSTOMER", sender_name: "나고객", body: "오후 글", created_at: pm });
    frame({ type: "message", conversation_id: "B", seq: last + 2, id: "b-am", kind: "MESSAGE", sender_kind: "AGENT", sender_id: "a9", sender_name: "다른상담", body: "오전 글", created_at: am });
    await settle();
    const html = el("msgs").innerHTML;
    assert.match(html, /<div class="meta"><span>오후 02:15<\/span>/, "데스크 말풍선 시각이 오전·오후 꼴이 아니다");
    assert.match(html, /<span>오전 09:05<\/span>/);
    assert.doesNotMatch(html, /<span>14:15<\/span>/, "데스크가 위젯의 24시간 꼴을 쓴다");
  });

  test("동료 상담원 글에는 수정·삭제를 열지 않아 ⋯ 를 그리지 않고 내 글에만 그린다 — 메시지 관리 권한이 없을 때, 서버가 자기 글만 고치고 지우게 한다", async () => {
    await openConv("B");
    const last = sentOfType("subscribe").filter((f) => f.conversation_id === "B").at(-1).after_seq;
    frame({ type: "ready", conversation_id: "B", last_seq: last });
    const at = new Date(2026, 9, 6, 9, 5).toISOString();
    frame({ type: "message", conversation_id: "B", seq: last + 1, id: "b-me", kind: "MESSAGE", sender_kind: "AGENT", sender_id: "ag1", sender_name: "나상담", body: "내 글", created_at: at });
    frame({ type: "message", conversation_id: "B", seq: last + 2, id: "b-peer", kind: "MESSAGE", sender_kind: "AGENT", sender_id: "a9", sender_name: "다른상담", body: "동료 글", created_at: at });
    await settle();
    const html = el("msgs").innerHTML;
    assert.match(html, /class="msg-menu" data-msg="b-me"/, "내 글에 ⋯ 가 없다");
    assert.doesNotMatch(html, /data-msg="b-peer"/, "동료 글에 서버가 거절할 수정·삭제 메뉴가 뜬다");
  });

  // 정책 824·825 는 desk.js 가 createLog 에 넘기는 설정(outKinds·selfId)으로 정해진다. 설정을 테스트에 베껴 두면 desk.js 가 바뀌어도
  // 통과했다(감사 27 J17·J18). 실제 desk.js 가 그린 줄의 클래스와 이름 칸을 본다. 줄마다 [클래스 목록, 이름 칸, 아바타, 읽음 칸, ⋯] 이다.
  const drawnRows = (html) => [...html.matchAll(/<div class="row ([^"]*)"[^>]*>([\s\S]*?)<\/div><\/div>/g)].map(([, cls, inner]) => ({
    cls: cls.split(" "), who: /<div class="who">([^<]*)<\/div>/.exec(inner)?.[1] ?? null, avatar: /class="avatar/.test(inner),
    read: /class="read"/.test(inner), menu: /class="msg-menu"/.test(inner),
  }));
  async function drawLines(convId, lines) {
    await openConv(convId);
    const last = sentOfType("subscribe").filter((f) => f.conversation_id === convId).at(-1).after_seq;
    frame({ type: "ready", conversation_id: convId, last_seq: last });
    const at = new Date(2026, 9, 6, 9, 5).toISOString();
    const before = drawnRows(el("msgs").innerHTML).length;
    lines.forEach(([sender_kind, extra], i) => frame({ type: "message", conversation_id: convId, seq: last + 1 + i, id: `${convId}-l${i}`, kind: "MESSAGE",
      sender_kind, body: `글${i}`, created_at: at, ...extra }));
    await settle();
    return drawnRows(el("msgs").innerHTML).slice(before);
  }

  test("데스크에서 고객 글은 왼쪽이고 상담원·AI·자동 글은 오른쪽이며 시스템 줄은 가운데다 — 위젯은 고객 글만 오른쪽이다", async () => {
    const before = (el("msgs").innerHTML.match(/<div class="row system/g) || []).length;
    const [customer, ai, bot, me, memo] = await drawLines("A", [
      ["CUSTOMER", { sender_id: "cA", sender_name: "가고객" }],
      ["AI", { sender_name: "AI 상담원" }],
      ["BOT", { sender_name: "시스템" }],
      ["AGENT", { sender_id: "ag1", sender_name: "나상담" }],
      ["AGENT", { sender_id: "ag1", sender_name: "나상담", visibility: "PRIVATE" }],
      ["SYSTEM", {}],
    ]);
    assert.ok(!customer.cls.includes("out") && customer.cls.includes("theirs"), "고객 글이 왼쪽(theirs)이 아니다");
    for (const [name, r] of [["AI", ai], ["자동", bot], ["내 글", me], ["내부 메모", memo]]) assert.ok(r.cls.includes("out"), `${name} 줄이 오른쪽(out)이 아니다`);
    assert.ok(memo.cls.includes("internal"), "내부 메모 모양이 빠졌다");
    assert.deepEqual([ai.who, bot.who], ["AI 상담원", "시스템"], "AI·자동 줄은 이름으로 사람과 구분한다");
    assert.ok(ai.avatar && bot.avatar && ai.cls.includes("bot"), "AI·자동 줄은 AI·자동 아바타를 단다");
    const systems = el("msgs").innerHTML.match(/<div class="row system[^"]*"/g) || [];
    assert.equal(systems.length, before + 1, "시스템 줄이 가운데(system)가 아니다");
    assert.doesNotMatch(systems.at(-1), /\bout\b/);
  });

  test("동료 상담원 글은 내 글과 같은 오른쪽이고 이름과 아바타를 보인다 — 내 글에는 이름이 없고 메뉴의 내 것 판정과 읽음 표시는 그대로다", async () => {
    const [me, peer, customer] = await drawLines("B", [
      ["AGENT", { sender_id: "ag1", sender_name: "나상담" }],
      ["AGENT", { sender_id: "ag-2", sender_name: "상담원2" }],
      ["CUSTOMER", { sender_id: "cB", sender_name: "나고객" }],
    ]);
    assert.ok(me.cls.includes("out") && peer.cls.includes("out"), "상담원 글이 오른쪽이 아니다");
    assert.equal(me.who, null, "내 글에 이름이 붙었다");
    assert.ok(!me.avatar);
    assert.ok(peer.cls.includes("peer"), "동료 글을 내 글과 구분하지 않는다");
    assert.equal(peer.who, "상담원2");
    assert.ok(peer.avatar, "동료 글에 아바타가 없다");
    assert.ok(!customer.cls.includes("peer"));
    // "내 것"(mine) 은 상담원 글 전부다 — 읽음 표시는 그 판정을 쓴다. ⋯ 는 내 글에만(정책 829)
    assert.ok(me.cls.includes("mine") && peer.cls.includes("mine"));
    assert.ok(me.read && peer.read && !customer.read, "읽음 칸은 상담원 글에만 있다");
    assert.ok(me.menu && !peer.menu, "메뉴에 동료 글이 내 글로 갔다");

    // 보내는 중 줄은 언제나 내 글이다(이름 없음)
    sockets.at(-1).readyState = 3; // ack 가 오지 않게 끊는다
    submit("보내는 중인 답");
    await settle();
    assert.match(el("pending").innerHTML, /class="row mine out /);
    assert.doesNotMatch(el("pending").innerHTML, /class="who"|peer/);
    openSocket();
    frame({ type: "ready", conversation_id: "B", last_seq: 0 });
    await settle();
    const sent = sentOfType("send").filter((f) => f.body === "보내는 중인 답").at(-1);
    frame({ type: "ack", conversation_id: "B", client_msg_id: sent.client_msg_id });
    await settle();
    assert.equal(el("pending").innerHTML.includes("보내는 중인 답"), false);
  });

  // 감사 14 A1: ready 의 대화 id 확인을 빼도 테스트가 통과했다. 빼면 떠난 대화의 늦은 ready 를 지금 대화의 구독으로 보고(다시 구독하지 않는다)
  // 떠난 대화의 번호로 지금 대화에 읽음을 보낸다.
  test("A 를 열고 ready 전에 B 로 옮기면 늦게 온 A 의 ready 는 버린다 — B 를 구독된 것으로 보지 않고 A 의 대화와 번호를 B 에 쓰지 않는다", async () => {
    const realNow = Date.now;
    let skew = 0;
    Date.now = () => realNow() + skew;
    try {
      await openConv("A");
      clearSent();
      await openConv("B");
      frame({ type: "ready", conversation_id: "A", last_seq: 42, conversation: { ...CONVS[0], customer_name: "가고객" }, reads: [] });
      await settle();
      assert.deepEqual(sentOfType("read"), [], "떠난 대화의 번호로 읽음을 보냈다");
      assert.equal(el("threadTitle").textContent, "나고객", "떠난 대화를 머리에 그렸다");
      // B 의 ready 는 끝내 오지 않는다. 10초를 넘기면 백오프 뒤 B 를 다시 구독한다
      for (let i = 0; i < 15; i++) {
        skew += 1000;
        mock.timers.tick(1000);
        await settle();
      }
      const subs = sentOfType("subscribe").map((f) => f.conversation_id);
      assert.deepEqual(subs, ["B", "B"], `B 를 다시 구독하지 않는다 — 떠난 대화의 ready 를 B 의 것으로 봤다: ${subs}`);
      assert.ok(sentOfType("read").every((f) => f.seq !== 42), "떠난 대화의 번호로 읽음을 보냈다");
      frame({ type: "ready", conversation_id: "B", last_seq: 5 });
      await settle();
    } finally {
      Date.now = realNow;
    }
  });

  // DEF-78(감사 13·14): 대화를 여는 순간 DB 가 끊기면 이전 상담 불러오기가 실패해 "처리하지 못했다" 토스트가 떴다. 상담원은 지금 대화가 실패한 것으로 읽는다.
  test("이전 상담을 불러오지 못하면 토스트 없이 그 자리에 조용한 안내와 다시 시도를 두고 누르면 다시 부른다", async () => {
    await openConv("A");
    const before = toastEls().length;
    server.priorFail = 1;
    server.priorMessages = [{ conversation_id: "P0", seq: 1, id: "p-1", kind: "MESSAGE", sender_kind: "CUSTOMER", sender_name: "나고객", body: "지난 상담의 질문", created_at: "2026-10-01T00:00:00Z" }];
    try {
      await openConv("B");
      assert.equal(toastEls().length, before, "이전 상담 실패에 토스트를 띄웠다 — 지금 대화가 실패한 것으로 보인다");
      const btn = created.filter((e) => e.id === "loadPrior").at(-1);
      assert.match(btn?.textContent || "", /이전 상담을 불러오지 못했습니다/, "실패한 자리에 안내가 없다");
      assert.match(btn.textContent, /다시 시도/);
      assert.doesNotMatch(el("msgs").innerHTML, /지난 상담의 질문/);
      btn.click();
      await settle();
      assert.match(el("msgs").innerHTML, /지난 상담의 질문/, "다시 시도가 이전 상담을 부르지 않는다");
      assert.equal(toastEls().length, before);
    } finally {
      server.priorFail = 0;
      server.priorMessages = [];
    }
  });

  // 감사 15 N2·N3: 다시 시도가 같은 크기로 부르는 것, 성공하면 실패 문구가 사라지는 것을 지키는 테스트가 없었다.
  test("이전 상담 다시 시도는 실패한 크기 그대로 부르고 성공하면 실패 문구를 거둔다", async () => {
    const page = (n) => Array.from({ length: n }, (_, i) => ({ conversation_id: "P0", seq: n - i, id: `p-${i}`, kind: "MESSAGE", sender_kind: "CUSTOMER", sender_name: "나고객", body: `지난 상담 ${i}`, created_at: "2026-10-01T00:00:00Z" }));
    await openConv("A");
    server.priorFail = 1;
    server.priorLimits.length = 0;
    try {
      await openConv("B");
      assert.deepEqual(server.priorLimits, [30], "대화를 열 때 이전 상담은 30줄을 먼저 부른다");
      const failed = created.filter((e) => e.id === "loadPrior").at(-1);
      assert.match(failed.textContent, /불러오지 못했습니다/);
      server.priorMessages = page(30); // 꽉 찬 한 쪽이 온다 — 그 앞이 더 있다
      failed.click();
      await settle();
      assert.deepEqual(server.priorLimits, [30, 30], "다시 시도가 실패한 크기(30)가 아니라 다른 크기로 불렀다 — 빈틈이나 겹침이 생긴다");
      const after = created.filter((e) => e.id === "loadPrior").at(-1);
      assert.notEqual(after, failed, "성공한 뒤 단추를 다시 그리지 않았다");
      assert.equal(after.textContent, "이전 상담 더 보기", "성공했는데 실패 문구가 남았다");
      assert.doesNotMatch(after.className, /prior-failed/);
    } finally {
      server.priorFail = 0;
      server.priorMessages = [];
    }
  });

  // DEF-83: 서버가 태그를 읽지 못하면 대화 프레임에 tags 를 싣지 않는다("모름"). 화면은 가진 태그를 그대로 둔다. 빈 목록은 "태그 없음" 이다.
  test("태그를 싣지 않은 대화 프레임은 화면의 태그를 지우지 않고 빈 목록이면 지운다", async () => {
    CONVS[0].tags = [{ id: "t1", name: "배송" }];
    try {
      await openConv("A");
      assert.match(el("tagChips").innerHTML, /배송/, "열 때 태그가 그려지지 않는다");
      const frame0 = { id: "A", customer_id: "cA", customer_name: "가고객", state: "OPEN", subject: "상태가 바뀌었다" };
      frame({ type: "conversation", conversation_id: "A", conversation: frame0 });
      assert.match(el("tagChips").innerHTML, /배송/, "태그를 모르는 프레임이 화면의 태그를 지웠다");
      frame({ type: "conversation", conversation_id: "A", conversation: { ...frame0, tags: null } });
      assert.match(el("tagChips").innerHTML, /배송/, "tags 가 null 인 프레임도 모름이다 — 태그를 지웠다");
      frame({ type: "conversation", conversation_id: "A", conversation: { ...frame0, tags: [] } });
      assert.doesNotMatch(el("tagChips").innerHTML, /배송/, "빈 목록(태그 없음)인데 앞 태그가 남았다");
    } finally {
      delete CONVS[0].tags;
    }
  });

  // ── 담당 (D39·D40, 감사 10) ─────────────────────────────────
  const conversationFrame = (c) => frame({ type: "conversation", conversation_id: c.id, conversation: c });
  const menuButton = (label) => menus().at(-1).buttons.find((b) => b.textContent.trim() === label);
  const A_ROW = () => ({ id: "A", customer_id: "cA", customer_name: "가고객", state: "OPEN", tags: [] });

  test("담당 메뉴는 그릴 때 본 담당자를 expected_assignee 로 싣는다 — 미배정을 보고 누른 나에게는 빈 문자열이다", async () => {
    await openConv("A");
    server.assignBodies.length = 0;
    el("assignBtn").click();
    assert.ok(menuOpen(menus().at(-1)), "담당 메뉴가 열리지 않는다");
    menuButton("나에게").click();
    await settle();
    assert.deepEqual(server.assignBodies, [{ agent_id: "ag1", expected_assignee: "" }], "화면이 본 담당자를 싣지 않으면 동시에 누른 둘이 다 성공한다");
    assert.match(el("threadSub").textContent, /담당 나상담/);
  });

  test("담당이 해제된 대화 프레임은 담당자 필드가 없어도 미배정으로 그리고 풀린 보류도 지운다", async () => {
    // 서버는 빈 담당자·보류 시각을 싣지 않는다(omitempty). 앞 값과 합치면 해제한 담당자가 남는다.
    conversationFrame(A_ROW());
    assert.doesNotMatch(el("threadSub").textContent, /담당/, "해제 프레임 뒤에도 앞 담당자가 머리에 남는다");
    assert.match(el("assignBtn").innerHTML, /담당 지정/);
    conversationFrame({ ...A_ROW(), state: "SNOOZED", snoozed_until: new Date(Date.now() + 3600e3).toISOString() });
    assert.match(el("threadSub").textContent, /까지 보류/);
    conversationFrame(A_ROW());
    assert.doesNotMatch(el("threadSub").textContent, /보류/, "보류가 풀렸는데 보류 시각이 남는다");
  });

  test("고객이 직접 닫은 상담은 머리와 목록 줄의 상태 옆에 고객 종료가 보이고 다시 열리면 사라진다", async () => {
    await openConv("A");
    conversationFrame({ ...A_ROW(), state: "CLOSED", closed_by: "CUSTOMER" });
    assert.match(el("threadSub").textContent, /^완료 · 고객 종료/, "상태 옆에 고객 종료가 없다");
    Object.assign(CONVS[0], { state: "CLOSED", closed_by: "CUSTOMER" });
    frame({ type: "inbox_changed" });
    await new Promise((r) => setTimeout(r, 800)); // 받은함 신호는 0.7초 묶어 한 번 다시 읽는다
    await settle();
    const rowA = () => (el("convList").innerHTML.match(/<button class="conv-row[^"]*" data-id="A"[\s\S]*?<\/button>/) || [""])[0];
    assert.match(rowA(), /<span class="chip" data-closed-by="customer">고객 종료<\/span>/, "목록 줄의 상태 옆에 고객 종료가 없다");
    CONVS[0].state = "OPEN";
    delete CONVS[0].closed_by;
    frame({ type: "inbox_changed" });
    await new Promise((r) => setTimeout(r, 800)); // 받은함 신호는 0.7초 묶어 한 번 다시 읽는다
    await settle();
    assert.doesNotMatch(rowA(), /고객 종료/, "다시 열린 상담의 목록 줄에 고객 종료가 남았다");
    assert.equal(el("closeBtn").textContent, "다시 열기", "상담원은 고객이 닫은 상담을 다시 열 수 있다");
    // 다시 열린 프레임은 closed_by 를 싣지 않는다(omitempty) — 앞 값이 남으면 안 된다
    conversationFrame(A_ROW());
    assert.doesNotMatch(el("threadSub").textContent, /고객 종료/, "다시 열렸는데 고객 종료가 남는다");
    conversationFrame({ ...A_ROW(), state: "CLOSED", closed_by: "AGENT" });
    assert.doesNotMatch(el("threadSub").textContent, /고객 종료/, "상담원이 닫은 상담에 고객 종료를 붙였다");
    conversationFrame(A_ROW());
  });

  test("담당 메뉴가 열린 동안 담당자가 바뀌면 메뉴를 닫고, 낡은 메뉴로 누른 것은 그릴 때 본 담당자를 조건으로 보내 409 를 받으면 안내하고 목록과 머리를 다시 읽는다", async () => {
    // A 는 미배정이다. 메뉴를 연 사이 다른 상담원이 가져갔다(실시간 프레임)
    el("assignBtn").click();
    const m = menus().at(-1);
    const stale = menuButton("나에게");
    CONVS[0].assignee_agent_id = "ag2";
    CONVS[0].assignee_name = "다른상담";
    conversationFrame({ ...A_ROW(), assignee_agent_id: "ag2", assignee_name: "다른상담" });
    assert.equal(menuOpen(m), false, "담당자가 바뀌었는데 낡은 메뉴가 열려 있다");
    // 그래도 낡은 단추가 눌렸다(닫히는 순간의 클릭). 서버가 가려 준다
    server.assignReply = { status: 409, error: { code: "conflict", message: "다른 상담원이 먼저 가져갔습니다" } };
    server.assignBodies.length = 0;
    server.gets.length = 0;
    stale.click();
    await settle();
    assert.deepEqual(server.assignBodies, [{ agent_id: "ag1", expected_assignee: "" }], "누른 순간의 담당자(이긴 사람)를 조건으로 보내면 정상 이관이 된다");
    assert.match(toastEls().at(-1).innerHTML, /다른 상담원이 먼저 가져갔습니다/);
    assert.ok(server.gets.includes("/api/agent/conversations"), "409 뒤에 받은함을 다시 읽지 않는다");
    assert.ok(server.gets.includes("/api/conversations/A"), "409 뒤에 머리를 서버 상태로 다시 읽지 않는다");
    assert.match(el("threadSub").textContent, /담당 다른상담/);
  });

  test("배정이 403 으로 거절되면 409 와 같이 까닭과 문의 번호를 알리고 목록과 머리를 다시 읽는다", async () => {
    // 응대 권한만 있으면 남이 맡은 상담은 바꿀 수 없다. 화면이 본 뒤 남이 가져갔으면 403 이 온다.
    el("assignBtn").click();
    server.assignReply = { status: 403, error: { code: "forbidden", message: "권한이 없다" }, headers: { "X-Trace-Id": "0a1b2c3d4e5f60718293a4b5c6d7e8f9" } };
    server.gets.length = 0;
    menuButton("담당 해제").click();
    await settle();
    assert.match(toastEls().at(-1).innerHTML, /권한이 없다/);
    // D47: 서버 거절 알림에는 응답의 X-Trace-Id 앞 8자리를 문의 번호로 붙이고, 번호 복사는 32자리 전체다.
    assert.match(toastEls().at(-1).innerHTML, /문의 번호 0a1b2c3d</, "알림에 문의 번호가 없다");
    toastEls().at(-1).querySelector(".toast-trace-copy").click();
    await settle();
    assert.equal(clipboard.at(-1), "0a1b2c3d4e5f60718293a4b5c6d7e8f9", "번호 복사는 trace id 전체를 복사한다");
    assert.ok(server.gets.includes("/api/agent/conversations"), "403 뒤에 받은함을 다시 읽지 않는다");
    assert.ok(server.gets.includes("/api/conversations/A"), "403 뒤에 머리를 다시 읽지 않는다");
    server.assignReply = null;
    delete CONVS[0].assignee_agent_id;
    delete CONVS[0].assignee_name;
  });

  // ── 쓰다 만 글 (감사 10) ─────────────────────────────────────
  test("대화를 떠나면 쓰던 글은 그 대화의 초안으로 남고 입력칸은 비운다 — 열기가 실패한 뒤 다시 시도로 연 대화에 앞 대화의 글이 들어가지 않는다", async () => {
    await openConv("A");
    el("input").value = "A 고객에게 쓰던 답";
    server.openFail.B = 500;
    await openConv("B");
    assert.equal(el("input").value, "", "열기가 실패했는데 앞 대화의 글이 입력칸에 남는다");
    delete server.openFail.B;
    el("msgs").querySelector("#retryOpen").click();
    await settle();
    assert.equal(el("threadTitle").textContent, "나고객");
    assert.equal(el("input").value, "", "A 고객에게 쓰던 글이 B 입력칸에 들어간다");
    clearSent();
    el("composer").dispatch("submit");
    await settle();
    assert.deepEqual(sentOfType("send"), [], "A 고객에게 쓰던 글이 B 고객에게 나간다");
    el("input").value = "B 고객에게 쓰던 답";
    await openConv("A");
    assert.equal(el("input").value, "A 고객에게 쓰던 답", "돌아오면 그 대화에 쓰던 글이 입력칸에 있다");
    await openConv("B");
    assert.equal(el("input").value, "B 고객에게 쓰던 답");
    el("input").value = "";
  });

  // ── 삭제한 상담 보기 (D40, 감사 10 N30) ─────────────────────────
  test("삭제한 상담 보기도 연속으로 눌렀을 때 늦게 온 앞 상담의 응답은 버린다 — 마지막에 누른 상담이 열린다", async () => {
    const filterButton = (f) => el("filters").buttons.find((b) => b.dataset.filter === f);
    filterButton("deleted").click();
    await settle();
    let release;
    server.deletedHold.A = new Promise((r) => (release = r));
    el("convList").rows.find((r) => r.dataset.id === "A").click();
    await settle();
    el("convList").rows.find((r) => r.dataset.id === "B").click();
    await settle();
    assert.equal(el("threadTitle").textContent, "나고객");
    assert.match(el("threadSub").textContent, /삭제됨/);
    delete server.deletedHold.A;
    release();
    await settle();
    assert.equal(el("threadTitle").textContent, "나고객", "늦게 온 앞 상담의 응답이 열린 상담을 덮는다");
    filterButton("all").click();
    await settle();
  });
});

// ── 새로고침한 데스크 ─────────────────────────────────────────────

describe("데스크 화면 — 새로고침 뒤", () => {
  let el;
  // 새로고침 전에 보냈지만 ack 를 못 받은 답이 저장소 큐에 남아 있다. 새 화면은 이 답을 기억하지 못하고, 아직 연 대화도 없다.
  const RELOADED = "BAD 새로고침 전에 A 고객에게 쓴 답입니다. 다시 보냈는데 서버가 영구 거절하고 마흔 자를 넘는다";

  before(async () => {
    store.set("gcs.v1.agent:ag1.A", JSON.stringify({ lastSeq: 0, pending: [{ client_msg_id: "reload-1", body: RELOADED, at: Date.now(), tries: 0 }] }));
    ({ el } = installDocument());
    await import("./desk.js?reload");
    await settle();
    openSocket();
    await settle();
  });
  after(() => mock.timers.reset());

  test("새로고침 뒤 다시 보낸 글이 영구 거절되면 그 글을 복사 단추와 함께 남기고 그 대화를 열면 입력칸에 돌려준다", async () => {
    const t = toastEls().find((e) => e.innerHTML.includes("BAD 새로고침 전에 A 고객에게"));
    assert.ok(t, "다시 보낸 답이 거절되면 그 답이 어디에도 없다");
    assert.match(t.innerHTML, new RegExp(REJECT));
    assert.match(t.innerHTML, /class="toast-copy">복사</, "복사 단추가 없다");
    t.querySelector(".toast-copy").click();
    await settle();
    assert.equal(clipboard.at(-1), RELOADED, "복사하면 답 전부가 나온다");
    assert.equal(el("input").value, "");
    const row = el("convList").rows?.find((r) => r.dataset.id === "A");
    assert.ok(row, "받은함에 A 가 없다");
    row.click();
    await settle();
    assert.equal(el("input").value, RELOADED, "그 대화를 열면 입력칸에 돌아온다");
  });
});

// ── 초대 링크(D46) ────────────────────────────────────────────────

describe("데스크 화면 — 초대 링크", () => {
  const replaced = [];
  let gets;
  before(() => {
    globalThis.history = { replaceState: (_s, _t, url) => replaced.push(url) };
    gets = server.gets.length;
  });
  after(() => {
    globalThis.location = { search: "", origin: "http://chat.test", protocol: "http:", host: "chat.test" };
    mock.timers.reset();
  });

  test("초대 토큰은 주소 조각(#invite=)에서 읽고 미리보기는 POST 본문으로 보낸다 — 토큰이 요청 주소에 실리지 않는다", async () => {
    installDocument();
    globalThis.location = { search: "", hash: "#invite=frag-token", origin: "http://chat.test", protocol: "http:", host: "chat.test" };
    const before = server.previews.length;
    await import("./desk.js?invite-fragment");
    await settle();
    const preview = server.previews.slice(before);
    assert.deepEqual(preview, [{ method: "POST", body: { token: "frag-token" } }]);
    assert.ok(server.gets.slice(gets).every((p) => !p.includes("frag-token")), "토큰을 GET 주소에 실었다");
    assert.deepEqual(replaced, [], "조각 꼴 링크는 주소를 바꿀 필요가 없다");
  });

  test("옛 초대 링크(?invite=)도 받고 곧바로 주소를 조각 꼴로 바꿔 쿼리를 지운다", async () => {
    installDocument();
    globalThis.location = { search: "?invite=old-token", hash: "", origin: "http://chat.test", protocol: "http:", host: "chat.test" };
    const before = server.previews.length;
    await import("./desk.js?invite-legacy");
    await settle();
    assert.deepEqual(replaced, ["/desk#invite=old-token"], "쿼리에 토큰이 남는다");
    assert.deepEqual(server.previews.slice(before), [{ method: "POST", body: { token: "old-token" } }]);
  });
});

// ── 세션 만료(D46, 감사 20 A21) ─────────────────────────────────────

describe("데스크 화면 — 세션 만료", () => {
  // 앞 묶음이 흉내 시계를 걷었다. 이 데스크의 간격 타이머(받은함 세기·전송 재시도)도 흉내로 묶어야 테스트가 끝난다.
  before(() => mock.timers.enable({ apis: ["setInterval"] }));
  after(() => {
    server.ticketExpired = false;
    server.front401 = false;
    mock.timers.reset();
  });

  test("앞단의 401 HTML 은 세션 만료가 아니라 데스크가 로그인 화면으로 가지 않고 상담원 토큰을 지우지 않는다", async () => {
    const { el } = installDocument();
    store.set("gcs.agentToken", "tok-ag1");
    server.front401 = true;
    await import("./desk.js?front401");
    await settle();
    assert.equal(el("deskView").hidden, false, "앞단의 401 HTML 로 로그인을 버렸다");
    assert.equal(store.get("gcs.agentToken"), "tok-ag1", "앞단의 401 HTML 로 상담원 토큰을 지웠다");
    server.front401 = false;
  });

  test("소켓 티켓이 401 이면 로그인 화면으로 돌아가고 저장한 상담원 토큰을 지운다", async () => {
    const { el } = installDocument();
    store.set("gcs.agentToken", "tok-ag1");
    server.ticketExpired = true;
    await import("./desk.js?expired");
    await settle();
    assert.equal(el("loginView").hidden, false, "만료된 세션으로 데스크에 남는다 — 티켓 401 을 8초마다 되풀이한다");
    assert.equal(el("deskView").hidden, true);
    assert.equal(store.get("gcs.agentToken"), undefined, "만료된 토큰을 남겼다");
  });
});

// ── 로그인 만료와 저장하지 못한 글(DEF-176) ──────────────────────────
// 저장이 막힌 브라우저에서 저장하지 못한 답은 메모리에만 있다. 로그인이 만료되면 데스크는 세션을 버리고 다시 로그인할 때 새로 만든다 —
// 전에는 그 글도 같이 버려 같은 상담원이 다시 로그인해도 나가지 않았다(감사 29 desk29 expire: 서버 0건). 다른 상담원·로그아웃이면 버린다.
describe("데스크 화면 — 로그인 만료와 저장하지 못한 글", () => {
  before(() => mock.timers.enable({ apis: ["setInterval"] }));
  after(() => {
    server.ticketExpired = false;
    server.meId = "";
    localStorage.setItem = (k, v) => store.set(k, String(v));
    mock.timers.reset();
  });

  // expireWithUnsaved 는 데스크를 띄워 A 에 저장하지 못한 답을 남긴 뒤 로그인 만료(소켓 티켓 401)로 로그인 화면에 간다.
  async function expireWithUnsaved(tag, text) {
    const { el } = installDocument();
    store.set("gcs.agentToken", "tok-ag1");
    server.meId = "ag1";
    await import(`./desk.js?${tag}`);
    await settle();
    openSocket();
    await settle();
    el("convList").rows.find((r) => r.dataset.id === "A").click();
    await settle();
    localStorage.setItem = (k, v) => {
      if (k.startsWith("gcs.q1.")) throw new Error("QuotaExceededError");
      store.set(k, String(v));
    };
    server.ticketExpired = true;
    socket().close(); // 끊겼다. 다시 붙으려는 티켓 요청이 401 이다
    el("input").value = text;
    el("composer").dispatch("submit");
    await settle();
    await new Promise((r) => setTimeout(r, 900)); // 다시 붙기 백오프(0.5초 + 흔들기)
    await settle();
    assert.equal(el("loginView").hidden, false, "로그인 만료로 로그인 화면에 가지 않았다(흉내가 틀렸다)");
    assert.ok(!server.posted.some(([, b]) => b === text), "만료 전에 글이 나갔다(흉내가 틀렸다)");
    server.ticketExpired = false;
    return el;
  }
  async function login(el, agentId) {
    server.meId = agentId;
    el("email").value = `${agentId}@example.com`;
    el("password").value = "pw";
    el("loginForm").dispatch("submit");
    await settle();
    openSocket();
    await settle();
  }
  const postedOf = (text) => server.posted.filter(([, b]) => b === text);

  test("로그인이 만료된 뒤 같은 상담원이 다시 로그인하면 저장하지 못한 답이 한 번 나가고 다른 상담원이 로그인하거나 로그아웃하면 버린다", async () => {
    const SAME = "만료 전에 쓴 답(같은 상담원)";
    let el = await expireWithUnsaved("unsaved-same", SAME);
    await login(el, "ag1");
    assert.equal(el("deskView").hidden, false);
    assert.deepEqual(postedOf(SAME), [["A", SAME]], "같은 상담원이 다시 로그인했는데 저장하지 못한 답이 나가지 않았다");

    const OTHER = "만료 전에 쓴 답(다른 상담원이 들어옴)";
    el = await expireWithUnsaved("unsaved-other", OTHER);
    await login(el, "ag2");
    assert.deepEqual(postedOf(OTHER), [], "다른 상담원 이름으로 앞 상담원의 답이 나갔다");
    el("meBtn").click();
    menus().at(-1).buttons.find((b) => b.textContent.trim() === "로그아웃").click();
    await settle();
    await login(el, "ag1");
    assert.deepEqual(postedOf(OTHER), [], "다른 상담원이 들어왔을 때 버리지 않고 남겨 두었다");
  });

  test("로그아웃을 누르면 저장하지 못한 답을 버려 같은 상담원이 다시 로그인해도 보내지 않는다", async () => {
    const TEXT = "로그아웃 전에 쓴 답";
    const { el } = installDocument();
    store.set("gcs.agentToken", "tok-ag1");
    server.meId = "ag1";
    await import("./desk.js?unsaved-explicit");
    await settle();
    openSocket();
    await settle();
    el("convList").rows.find((r) => r.dataset.id === "A").click();
    await settle();
    localStorage.setItem = (k, v) => {
      if (k.startsWith("gcs.q1.")) throw new Error("QuotaExceededError");
      store.set(k, String(v));
    };
    socket().close();
    el("input").value = TEXT;
    el("composer").dispatch("submit");
    await settle();
    el("meBtn").click();
    menus().at(-1).buttons.find((b) => b.textContent.trim() === "로그아웃").click();
    await settle();
    assert.equal(el("loginView").hidden, false);
    await login(el, "ag1");
    assert.deepEqual(postedOf(TEXT), [], "로그아웃을 눌렀는데 저장하지 못한 답이 다음 로그인에서 나갔다");
  });

  // 감사 30 N1: 데스크 로그아웃 누름의 clearToasts 를 지워도 통과했다(정책 866 은 위젯만 봤다). 앞 상담원의 알림(보내지 못한 답의 복사 단추 등)을
  // 다음 사람이 누르면 안 된다.
  const attached = (t) => !!t?.parentNode?.children.includes(t);
  const lastToast = (text) => toastEls().filter((e) => e.innerHTML.includes(text)).at(-1);

  test("데스크에서 로그아웃을 누르면 떠 있는 알림을 모두 치운다", async () => {
    const { toast } = await import("./ui.js");
    const { el } = installDocument();
    store.set("gcs.agentToken", "tok-ag1");
    server.meId = "ag1";
    await import("./desk.js?toast-logout");
    await settle();
    openSocket();
    await settle();
    toast("앞 상담원의 알림 — 로그아웃");
    const t = lastToast("앞 상담원의 알림 — 로그아웃");
    assert.ok(attached(t), "알림이 뜨지 않았다(흉내가 틀렸다)");
    el("meBtn").click();
    menus().at(-1).buttons.find((b) => b.textContent.trim() === "로그아웃").click();
    await settle();
    assert.equal(el("loginView").hidden, false);
    assert.ok(!attached(t), "로그아웃을 눌렀는데 앞 상담원의 알림이 남았다");
  });

  // 감사 30 desktoast2: 로그인이 만료된 뒤(로그아웃 누름이 아니다) 다른 상담원이 로그인하면 앞 상담원의 "보내지 못한 글" 본문 알림이 그 사람 화면에 남았다.
  // 같은 상담원이면 그 알림(복사 단추)이 아직 쓸모 있어 남긴다.
  test("로그인이 만료된 뒤 같은 상담원이 다시 로그인하면 떠 있는 알림이 남고 다른 상담원이 로그인하면 앞 상담원의 알림을 치운다", async () => {
    const { toast } = await import("./ui.js");
    let el = await expireWithUnsaved("toast-same", "만료 전 답(알림 같은 상담원)");
    toast("만료 전 알림(같은 상담원)");
    const same = lastToast("만료 전 알림(같은 상담원)");
    await login(el, "ag1");
    assert.ok(attached(same), "같은 상담원이 다시 들어왔는데 알림을 치웠다");

    el = await expireWithUnsaved("toast-other", "만료 전 답(알림 다른 상담원)");
    toast("앞 상담원의 비밀 답 알림");
    const other = lastToast("앞 상담원의 비밀 답 알림");
    assert.ok(attached(other), "알림이 뜨지 않았다(흉내가 틀렸다)");
    await login(el, "ag2");
    assert.equal(el("deskView").hidden, false);
    assert.ok(!attached(other), "다른 상담원이 로그인했는데 앞 상담원의 알림이 남았다");
  });
});

// ── 메시지 관리 권한(감사 28) ─────────────────────────────────────

// 관리자(메시지 관리·이력 조회 권한)의 ⋯ 메뉴. 그리는 쪽(canMenu)만 보면 여는 쪽이 넘기는 "내 글" 판정(ui.js 의 클릭 위임)이
// 동료 줄을 내 글로 봐도 통과했다(감사 28 A11). 그린 ⋯ 를 실제로 눌러 연 메뉴의 항목을 본다.
describe("데스크 화면 — 메시지 관리 권한", () => {
  let el;
  before(async () => {
    mock.timers.enable({ apis: ["setInterval"] });
    server.mePermissions = ["conversation.assign", "message.moderate", "message.revisions"];
    store.set("gcs.agentToken", "tok-ag1");
    ({ el } = installDocument());
    mountMenuButtons(el("msgs"));
    await import("./desk.js?moderator");
    await settle();
    openSocket();
    await settle();
  });
  after(() => {
    server.mePermissions = null;
    mock.timers.reset();
  });

  // 가짜 문서는 innerHTML 을 해석하지 않는다. 그린 줄의 ⋯ 단추(.msg-menu)를 ui.js 의 위임(bindMenus)이 찾을 수 있게,
  // 줄마다 단추 요소와 그 줄(클래스·data-sender·말풍선 글자)을 만들어 준다. 한 번 만든 단추는 다시 주지 않는다(data-bound 와 같다).
  function mountMenuButtons(msgs) {
    const made = new Map();
    msgs.querySelectorAll = (sel) => {
      if (!sel.startsWith(".msg-menu")) return [];
      const out = [];
      for (const [, cls, sender, inner] of msgs.innerHTML.matchAll(/<div class="row ([^"]*)" data-sender="([^"]*)"[^>]*>([\s\S]*?)<\/div><\/div>/g)) {
        const id = /class="msg-menu" data-msg="([^"]+)"/.exec(inner)?.[1];
        if (!id || made.has(id)) continue;
        const row = new FakeEl("div");
        for (const c of cls.split(" ")) row.classList.add(c);
        row.dataset.sender = sender;
        const text = /<div class="bubble">([^<]*)<\/div>/.exec(inner)?.[1] ?? "";
        row.querySelector = (q) => (q === ".bubble" ? { textContent: text } : null);
        const btn = new FakeEl("button");
        btn.dataset.msg = id;
        btn.closest = (q) => (q === ".row" ? row : null);
        made.set(id, btn);
        out.push(btn);
      }
      return out;
    };
    msgs.menuButton = (id) => made.get(id);
  }

  async function drawLines(convId, lines) {
    await settle();
    el("convList").rows.find((r) => r.dataset.id === convId).click();
    await settle();
    const last = sentOfType("subscribe").filter((f) => f.conversation_id === convId).at(-1).after_seq;
    frame({ type: "ready", conversation_id: convId, last_seq: last });
    const at = new Date(2026, 9, 6, 9, 5).toISOString();
    lines.forEach(([name, sender_kind, extra], i) => frame({ type: "message", conversation_id: convId, seq: last + 1 + i, id: `mod-${name}`, kind: "MESSAGE",
      sender_kind, body: `${name} 글`, created_at: at, ...extra }));
    await settle();
  }
  // openMenu 는 그 줄의 ⋯ 를 눌러 연 메뉴의 항목 이름이다. ⋯ 가 없으면 null 이다.
  function openMenu(name) {
    const btn = el("msgs").menuButton(`mod-${name}`);
    if (!btn) return null;
    btn.click();
    const m = menus().at(-1);
    return m.buttons.map((b) => b.textContent.trim());
  }

  test("메시지 관리 권한이 있으면 동료·AI·자동 글의 ⋯ 메뉴에 삭제와 수정 이력이 있고 고객 글에는 수정 이력만 있다 — 수정은 내 글에만 있다", async () => {
    await drawLines("A", [
      ["내", "AGENT", { sender_id: "ag1", sender_name: "나상담" }],
      ["동료", "AGENT", { sender_id: "ag-2", sender_name: "상담원2" }],
      ["AI", "AI", { sender_name: "AI 상담원" }],
      ["자동", "BOT", { sender_name: "자동 응답" }],
      ["고객", "CUSTOMER", { sender_id: "cA", sender_name: "가고객" }],
    ]);
    assert.deepEqual(openMenu("내"), ["수정", "삭제", "수정 이력"]);
    assert.deepEqual(openMenu("동료"), ["삭제", "수정 이력"]);
    assert.deepEqual(openMenu("AI"), ["삭제", "수정 이력"], "관리자가 AI 글을 지우지 못한다");
    assert.deepEqual(openMenu("자동"), ["삭제", "수정 이력"]);
    assert.deepEqual(openMenu("고객"), ["수정 이력"], "고객 글에 서버가 거절할 삭제가 뜬다 — 고객 글은 고객의 것이다");

    // 수정 이력을 누르면 그 글의 이력을 묻는다
    const gets = server.gets.length;
    openMenu("고객");
    menus().at(-1).buttons.find((b) => b.textContent === "수정 이력").click();
    await settle();
    assert.ok(server.gets.slice(gets).includes("/api/agent/conversations/A/messages/mod-고객/revisions"), "수정 이력이 이력을 묻지 않는다");
  });

  test("관리자가 동료 상담원 글의 ⋯ 를 눌러 연 메뉴에는 수정이 없다 — 누른 줄을 내 글로 보지 않는다", async () => {
    await drawLines("B", [["동료B", "AGENT", { sender_id: "ag-3", sender_name: "상담원3" }]]);
    assert.ok(el("msgs").menuButton("mod-동료B"), "관리 권한이 있는데 동료 글에 ⋯ 가 없다");
    assert.deepEqual(openMenu("동료B"), ["삭제", "수정 이력"], "동료 글을 고치는 메뉴가 열린다 — 서버가 거절한다(자기 글만 고친다)");
  });
});
