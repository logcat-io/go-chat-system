// 위젯 화면 배선 테스트.
//
// widget.js 를 그대로 띄운다. 화면은 쓰는 만큼만 흉내 낸 가짜 요소이고, 서버(fetch)·소켓(WebSocket)·저장소 올리기(XMLHttpRequest)도 가짜다.
// 단언은 사람이 보는 결과다 — 작성칸에 무엇이 보이는가, 입력칸에 무엇이 남는가, 무엇이 서버로 나가는가, 어떤 색이 칠해지는가.
// widget-view.js·ui.js 의 순수 함수가 맞아도 widget.js 가 그것을 부르지 않으면 화면은 그대로다. 그 연결을 본다.
import { test, describe, before, after, mock } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

register(
  "data:text/javascript," +
    encodeURIComponent(`export async function resolve(spec, ctx, next) {
      if (spec === "/chat.js" || spec === "/ui.js" || spec === "/widget-view.js") return next(new URL("." + spec, ctx.parentURL).href, ctx);
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
class FakeEl {
  constructor(tag = "div", id = "") {
    this.tagName = tag.toUpperCase();
    this.id = id;
    this.children = [];
    this.dataset = {};
    this.attrs = {};
    this.listeners = {};
    this.classList = new FakeClassList();
    const props = {};
    this.style = { props, setProperty: (k, v) => (props[k] = v), getPropertyValue: (k) => props[k] || "" };
    this.hidden = false;
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
  dispatch(type, ev = {}) { for (const fn of this.listeners[type] || []) fn({ type, preventDefault() {}, target: this, ...ev }); }
  click() { this.dispatch("click"); }
  // 초점은 문서의 activeElement 로 남긴다(뒤로가기가 초점을 어디로 돌려주는지 본다).
  focus() { if (globalThis.document) globalThis.document.activeElement = this; }
  requestSubmit() { this.dispatch("submit"); }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return this.attrs[k] ?? null; }
  appendChild(c) { this.children.push(c); c.parentNode = this; return c; }
  remove() {
    const list = this.parentNode?.children;
    if (list) list.splice(list.indexOf(this), 1);
  }
  // 모달(ui.js 의 modal)은 막(scrim) 안의 첫 요소에 단추를 건다. 막 자신을 돌려줘 그 단추를 찾을 수 있게 한다(고객 종료 확인 창, D48).
  get firstElementChild() { return this.className === "scrim" ? this : undefined; }
  querySelector(sel) {
    // [data-act="ok"] 같은 속성 선택자는 괄호를 벗겨 그린 글자에서 찾는다.
    const token = sel.startsWith("[") ? sel.slice(1, -1) : sel.replace(/^[.#]/, "");
    if (!this.any && !this._html.includes(token)) return null;
    if (!this._q.has(sel)) {
      const stub = new FakeEl("span");
      stub.any = true; // 안쪽 요소까지는 흉내 내지 않는다. 무엇을 찾든 준다.
      this._q.set(sel, stub);
    }
    return this._q.get(sel);
  }
  querySelectorAll() { return []; }
  contains() { return false; }
  closest() { return null; }
}

// created 는 문서가 만든 요소 전부다. 알림(toast)의 뿌리는 ui.js 가 처음 띄운 문서에 붙어 있어 문서마다 찾지 않고 여기서 찾는다.
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
    // 대화 목록 줄(.conv-item)만 흉내 낸다. 그린 HTML 에서 줄마다 새 요소를 만들어 주고, 테스트가 누를 수 있게 기억한다.
    querySelectorAll: (sel) => {
      if (sel !== ".conv-item") return [];
      const list = [];
      for (const e of ids.values()) {
        for (const [, id] of e.innerHTML.matchAll(/class="conv-item" data-id="([^"]+)"/g)) {
          const row = new FakeEl("button");
          row.dataset.id = id;
          list.push(row);
        }
      }
      doc.convItems = list;
      return list;
    },
    addEventListener() {},
    removeEventListener() {},
  };
  globalThis.document = doc;
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
// queuedIn 은 저장소에 남은 그 대화의 전송 대기 글이다. 글마다 키가 따로이고 커서와 이름 공간이 다르다
// (커서 `gcs.v1.<주인>.<대화>`, 글 `gcs.q1.<주인>.<대화>|<client_msg_id>`, DEF-158·165). 옛 형식(커서 키의 pending 배열)에 남은 글도 센다.
const queuedIn = (prefix, convId) => {
  const out = [];
  const q = prefix.replace(/^gcs\.v1\./, "gcs.q1.");
  for (const [k, v] of store) if (k.startsWith(`${q}${convId}|`)) out.push(JSON.parse(v));
  for (const p of JSON.parse(store.get(prefix + convId) || "{}").pending || []) {
    if (!out.some((q) => q.client_msg_id === p.client_msg_id)) out.push(p);
  }
  return out;
};
// returnedStored 는 저장소에 남은 그 고객의 돌려받은 글 본문이다(글마다 키, gcs.returned.v2.<고객>.<상담>|<글 id>, DEF-179). 없으면 빈 문자열이다.
const returnedKeys = (cust) => [...store.keys()].filter((k) => k.startsWith(`gcs.returned.v2.${cust}.`));
const returnedStored = (cust) => returnedKeys(cust).map((k) => JSON.parse(store.get(k)).body).join("\n");
// 창의 이벤트(저장소·보임 상태)는 기억해 두고 테스트가 fireGlobal 로 보낸다. 브라우저처럼 저장소 이벤트는 다른 탭이 썼을 때만 온다 — 테스트가 직접 보낸다.
const globalListeners = {};
globalThis.addEventListener = (type, fn) => (globalListeners[type] ||= []).push(fn);
const fireGlobal = (type, ev = {}) => (globalListeners[type] || []).forEach((fn) => fn({ type, ...ev }));
// 알림의 "복사" 단추가 쓰는 클립보드
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
// socketToken 은 소켓이 어느 세션으로 붙었는지다. 주소에는 티켓만 있고(D46) 티켓을 받은 세션 토큰은 가짜 서버가 기억한다.
const socketToken = (ws) => server.ticketOf[new URL(ws.url).searchParams.get("ticket")] || "";
function openSocket() {
  const ws = socket();
  ws.readyState = 1;
  ws.onopen?.();
  return ws;
}
const frame = (f) => socket().onmessage({ data: JSON.stringify(f) });
const sentOfType = (type) => sockets.flatMap((ws) => ws.sent).filter((f) => f.type === type);

// 저장소 올리기. finish 를 부르기 전까지 끝나지 않는다. ignoreAbort 면 끊어도(이미 다 보낸 뒤처럼) 끝까지 간다.
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
  progress(loaded, total) { this.upload.onprogress?.({ lengthComputable: true, loaded, total }); }
  finish(status = 204) {
    this.status = status;
    this.responseText = "";
    this.onload?.();
  }
};

const server = {
  tickets: 0,
  ticketAsks: [], // 티켓을 달라고 한 세션 토큰들(순서대로)
  deadTokens: new Set(), // 서버에서 끝난 세션 토큰. 티켓 요청이 401 이다
  expiredMembers: new Set(), // exp 가 지난 회원 토큰. member-session 이 400 이다
  ticketOf: {}, // 티켓 → 그 티켓을 받은 세션 토큰. 소켓이 누구로 붙었는지 본다
  logouts: [],
  customer: "c1",
  title: "둠둠디라 상담", // 관리자가 정한 위젯 이름. 빈 글자면 설정에 이름이 없는 것이다
  color: "#e8590c",
  // 관리자가 정한 자주 묻는 질문 셋(홈에는 알약, 자주 묻는 질문 화면에는 아코디언 세 줄)
  faq: [{ question: "배송", answer: "이틀" }, { question: "교환", answer: "신청" }, { question: "환불", answer: "사흘" }],
  convs: [],
  posted: [],
  attachments: 0,
  sessionCalls: [],
  holdSessions: false,
  attachPaths: [], // 첨부 올리기를 시작한 주소(어느 상담으로 올렸나)
  listExpired: 0, // 0 보다 크면 상담 목록 요청이 그 횟수만큼 401(토큰 만료)로 답한다
  listFront401: 0, // 0 보다 크면 상담 목록 요청이 그 횟수만큼 앞단(인증 게이트웨이)의 401 HTML 로 답한다
  listCalls: 0, // 상담 목록을 받은 횟수(GET)
  holdList: null, // 프라미스면 목록 응답을 그것이 풀릴 때까지 묶는다(받는 중에 이벤트가 오는 경우)
  history: {}, // 상담 id → 지난 글 다시 받기(GET …/messages?after_seq=)가 돌려줄 줄
  ratingView: { closed: true, can_rate: true, current: false, rating: null }, // GET …/rating 의 답(D45)
  ratingPuts: [], // PUT …/rating 으로 낸 별점·의견
  closes: [], // 고객 종료(D48) — { conv, postedBefore: 그때까지 저장한 글 수 }
};
const json = (status, body, headers = {}) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...headers } });
globalThis.fetch = async (path, init = {}) => {
  const method = init.method || "GET";
  // 소켓 티켓(D46). 세션 토큰 대신 소켓 주소에 실린다.
  if (path === "/api/ws-ticket") {
    const held = (init.headers?.Authorization || "").replace(/^Bearer /, "");
    server.ticketAsks.push(held);
    // 서버에서 끝난 세션(회원 토큰 exp·비밀 재발급)의 토큰이다
    if (server.deadTokens.has(held)) return json(401, { error: { code: "unauthorized", message: "세션이 만료되었거나 토큰이 올바르지 않습니다" } });
    const ticket = `wst-${++server.tickets}`;
    server.ticketOf[ticket] = held;
    return json(200, { ticket });
  }
  // 로그아웃. 누구의 세션을 끊었는지 기억한다.
  if (path === "/api/logout") {
    server.logouts.push(init.headers?.Authorization || "");
    return new Response(null, { status: 204 });
  }
  if (path === "/api/widget/settings") return json(200, { title: server.title, greeting: "안녕하세요", faq: server.faq, open_now: true, color: server.color, position: "right" });
  // 세션 입구. server.holdSessions 면 응답을 묶어 두고 테스트가 순서를 정해 푼다(sessionCalls[i].release()).
  if (path === "/api/customer/session" || path === "/api/customer/member-session") {
    const member = path.endsWith("member-session") ? JSON.parse(init.body).member_token : "";
    const call = { kind: member ? `member:${member}` : "anon", token: JSON.parse(init.body).token || "" };
    server.sessionCalls.push(call);
    if (server.holdSessions) await new Promise((r) => (call.release = r));
    // memberReply 가 있으면 회원 입구가 그 답을 한다(던지면 네트워크 실패다). 앞단(WAF·프록시)의 답이나 일시 실패를 흉내 낸다.
    if (member && server.memberReply) return server.memberReply();
    if (member.startsWith("bad")) return json(401, { error: { code: "unauthorized", message: "회원 토큰이 올바르지 않다" } }, { "X-Trace-Id": "feedc0de00000000feedc0de00000000" });
    // 만료된 회원 토큰은 실제 서버처럼 400 invalid 다(app.VerifyMember)
    if (server.expiredMembers.has(member)) return json(400, { error: { code: "invalid", message: "회원 토큰이 만료되었습니다" } });
    // 같은 회원의 새 회원 토큰은 "signed-<회원>~<판>" 이다. 고객은 같고 세션 토큰은 새것이다.
    const id = member ? member.replace(/^signed-/, "").replace(/~.*$/, "") : server.customer;
    return json(200, { token: member ? `tok-${member.replace(/^signed-/, "")}` : `tok-${id}`, customer: { id }, conversation: null });
  }
  const closeMatch = /^\/api\/customer\/conversations\/([^/]+)\/close$/.exec(path);
  if (closeMatch && method === "POST") {
    if (server.holdClose) await server.holdClose;
    server.closes.push({ conv: closeMatch[1], postedBefore: server.posted.length });
    const c = server.convs.find((x) => x.id === closeMatch[1]);
    if (c) Object.assign(c, { state: "CLOSED", closed_by: "CUSTOMER" });
    return json(200, { conversation: { id: closeMatch[1], state: "CLOSED", closed_by: "CUSTOMER" } });
  }
  if (path === "/api/customer/conversations" && method === "POST") {
    const c = { id: `conv-${server.convs.length + 1}`, state: "OPEN" };
    server.convs.unshift(c);
    return json(201, { conversation: c });
  }
  if (path === "/api/customer/conversations" && server.listFront401 > 0) {
    server.listFront401--;
    return new Response("<html><body>401 Authorization Required</body></html>", { status: 401, headers: { "Content-Type": "text/html" } });
  }
  if (path === "/api/customer/conversations" && server.listExpired > 0) {
    server.listExpired--;
    if (server.holdList) await server.holdList; // 401 도 묶을 수 있다(멈춘 뒤에 늦게 온 401)
    return json(401, { error: { code: "unauthorized", message: "세션이 만료됐다" } });
  }
  if (path === "/api/customer/conversations") {
    server.listCalls++;
    const res = json(200, { conversations: server.convs }); // 요청을 받은 때의 목록이다
    if (server.holdList) await server.holdList;
    return res;
  }
  let m = /^\/api\/conversations\/([^/]+)\/attachments$/.exec(path);
  if (m) {
    server.attachPaths.push(path);
    const id = `att-${++server.attachments}`;
    return json(201, { attachment: { id }, upload: { url: "/files/upload", fields: { token: "t" } } });
  }
  m = /^\/api\/attachments\/([^/]+)\/complete$/.exec(path);
  if (m && server.holdComplete) await server.holdComplete;
  if (m) return json(201, { attachment: { id: m[1], filename: "clip.mp4", content_type: "video/mp4", byte_size: 100 } });
  m = /^\/api\/conversations\/([^/]+)\/rating$/.exec(path);
  if (m && method === "PUT") {
    const body = JSON.parse(init.body);
    server.ratingPuts.push({ conv: m[1], ...body });
    return json(200, { closed: true, can_rate: true, current: true, rating: { score: body.score, comment: body.comment } });
  }
  if (m) return json(200, server.ratingView);
  m = /^\/api\/conversations\/([^/]+)\/messages/.exec(path);
  // 지난 글 다시 받기(after_seq). server.history[상담 id] 에 있는 줄을 그 번호 뒤부터 준다.
  if (m && method === "GET" && server.history[m[1]]) {
    const after = Number(new URL(path, "http://chat.test").searchParams.get("after_seq") || 0);
    return json(200, { messages: server.history[m[1]].filter((x) => x.seq > after) });
  }
  if (m && method === "POST") {
    const body = JSON.parse(init.body);
    // BAD 로 시작하는 글은 서버가 영구 거절한다(남의 대화 첨부를 실은 경우와 같은 답)
    if (/^BAD/.test(body.body)) return json(400, { error: { code: "invalid", message: "첨부를 찾을 수 없거나 이미 보낸 첨부입니다" } });
    server.posted.push(body);
  }
  return json(200, { messages: [] });
};

const settle = async () => {
  for (let i = 0; i < 30; i++) await new Promise((r) => setImmediate(r));
};

// 간격 타이머(재조회·설정 다시 받기·전송 재시도)는 멈춰 둔다. 테스트가 직접 부른다.
mock.timers.enable({ apis: ["setInterval"] });

const toastsIn = (doc) => doc.body.children.flatMap((root) => root.children.map((t) => t.innerHTML));
const lastToast = (doc) => toastsIn(doc).at(-1) || "";

// ── 일반 위젯(직접 연 /widget) ───────────────────────────────────

describe("위젯 화면 — 직접 연 /widget", () => {
  const { doc, el } = installDocument();
  globalThis.location = { search: "", origin: "http://chat.test", protocol: "http:", host: "chat.test" };
  globalThis.window = { parent: null, innerWidth: 1280, addEventListener() {} };
  // 앞 고객(c0)이 이 브라우저에서 받은 상담원 묶음이 남아 있다
  store.set("gcs.recentAgents", JSON.stringify({ owner: "c0", names: ["남의상담원"] }));

  before(async () => {
    await import("./widget.js");
    await settle();
    openSocket(); // 세션이 연 소켓을 열린 상태로 만든다
  });
  // 다음 위젯을 띄우기 전에 이 위젯의 간격 타이머를 걷는다. 안 걷으면 설정 다시 받기가 다음 위젯의 색을 덮는다.
  after(() => {
    mock.timers.reset();
    mock.timers.enable({ apis: ["setInterval"] });
  });

  async function startChat() {
    el("startChat").click();
    await settle();
    return server.convs[0].id;
  }
  function pickFile(name = "clip.mp4") {
    el("file").files = [new File(["\x00\x00\x00\x18ftypisom"], name, { type: "video/mp4" })];
    el("file").dispatch("change");
  }
  function submit(text) {
    el("input").value = text;
    el("composer").dispatch("submit");
  }

  test("소켓 주소에는 고객 세션 토큰이 실리지 않고 일회용 티켓만 실린다(D46)", () => {
    assert.ok(sockets.length >= 1, "소켓을 열지 않았다");
    for (const ws of sockets) {
      assert.match(ws.url, /\/ws\?ticket=wst-\d+$/);
      assert.doesNotMatch(ws.url, /tok-/, "세션 토큰이 주소에 실렸다");
    }
  });

  test("다른 고객으로 세션이 바뀌면 상담원 묶음을 비운다 — 앞 고객이 받은 상담원 이름을 보이지 않는다", () => {
    assert.doesNotMatch(el("agentStack").innerHTML, /남/, "앞 고객의 상담원이 보인다");
    assert.equal(store.get("gcs.recentAgents"), undefined, "앞 고객의 묶음을 지운다");
  });

  // 홈의 질문 알약(data-i)을 누른 것처럼 칸에 클릭을 보낸다(이벤트 위임).
  const clickPill = (i) => el("faq").dispatch("click", { target: { closest: (sel) => (sel === ".faq-pill" ? { dataset: { i: String(i) } } : null) } });
  const clickQuestion = (i) => el("faqList").dispatch("click", { target: { closest: (sel) => (sel === ".accordion-trigger" ? el(`wFaqQ${i}`) : null) } });
  // 가짜 문서는 innerHTML 을 해석하지 않는다. 그려진 아코디언의 단추 속성(aria-controls·aria-expanded)과 답 칸의 hidden 을 그 id 의 가짜 요소에 옮긴다.
  function mountFaq() {
    for (const [, q, exp, a] of el("faqList").innerHTML.matchAll(/id="(wFaqQ\d+)" aria-expanded="(\w+)" aria-controls="(wFaqA\d+)"/g)) {
      if (el(q).getAttribute("aria-controls")) continue;
      el(q).setAttribute("aria-controls", a);
      el(q).setAttribute("aria-expanded", exp);
      el(a).hidden = true;
    }
  }
  const expanded = () => [0, 1, 2].map((i) => el(`wFaqQ${i}`).getAttribute("aria-expanded"));
  const panelsHidden = () => [0, 1, 2].map((i) => el(`wFaqA${i}`).hidden);

  test("홈의 질문 알약을 누르면 자주 묻는 질문 화면이 열려 그 질문이 펼쳐지고 뒤로가기는 홈이다", () => {
    mountFaq();
    assert.deepEqual(expanded(), ["false", "false", "false"], "처음에는 모두 닫혀 있다");
    clickPill(1);
    assert.equal(el("viewFaq").hidden, false, "자주 묻는 질문 화면이 열리지 않는다");
    assert.equal(el("viewHome").hidden, true);
    assert.equal(el("headTitle").textContent, "자주 묻는 질문");
    assert.equal(el("back").hidden, false, "뒤로가기가 없다");
    assert.deepEqual(expanded(), ["false", "true", "false"], "누른 질문만 펼쳐져야 한다");
    assert.deepEqual(panelsHidden(), [true, false, true]);
    assert.equal(doc.activeElement, el("wFaqQ1"), "초점이 펼친 질문으로 가지 않는다");
    // 화면의 질문 줄을 누르면 닫히고 다시 누르면 열린다
    clickQuestion(1);
    assert.equal(el("wFaqQ1").getAttribute("aria-expanded"), "false");
    clickQuestion(1);
    assert.equal(el("wFaqQ1").getAttribute("aria-expanded"), "true");
    el("back").click();
    assert.equal(el("viewHome").hidden, false, "뒤로가기가 홈으로 가지 않는다");
    assert.equal(el("viewFaq").hidden, true);
  });

  test("자주 묻는 질문 화면에 다시 들어오면 앞서 펼친 질문은 닫혀 있다 — 알약은 그 질문 하나만, 전체보기는 모두 닫힌 채다", () => {
    clickPill(0);
    clickQuestion(2); // 화면 안에서 하나 더 펼친다
    assert.deepEqual(expanded(), ["true", "false", "true"]);
    el("back").click();
    // 다른 알약으로 다시 들어온다 — 그 질문 하나만 열린다
    clickPill(1);
    assert.deepEqual(expanded(), ["false", "true", "false"], "앞서 펼친 질문이 남아 있다");
    assert.deepEqual(panelsHidden(), [true, false, true]);
    el("back").click();
    // 전체보기는 아무것도 펼치지 않는다
    el("faqAll").click();
    assert.equal(el("viewFaq").hidden, false);
    assert.deepEqual(expanded(), ["false", "false", "false"], "전체보기인데 펼친 질문이 있다");
    assert.deepEqual(panelsHidden(), [true, true, true]);
    el("back").click();
  });

  test("자주 묻는 질문에서 뒤로 가면 초점은 들어온 알약이나 전체보기로 돌아간다", () => {
    clickPill(2);
    assert.equal(doc.activeElement, el("wFaqQ2"));
    el("back").click();
    assert.equal(el("viewHome").hidden, false);
    assert.equal(doc.activeElement, el("wFaqPill2"), "누른 알약으로 초점이 돌아오지 않는다");
    el("faqAll").click();
    el("faqAll").focus(); // 전체보기를 누른 뒤 화면 안을 돌아다닌다
    el("wFaqQ0").focus();
    el("back").click();
    assert.equal(doc.activeElement, el("faqAll"), "전체보기로 초점이 돌아오지 않는다");
    // 화면에 그려진 알약의 id 가 돌아올 자리와 같다
    assert.match(el("faq").innerHTML, /id="wFaqPill2"/);
  });

  test("대화 머리는 홈과 같은 로고 옆 이름이고 상태는 진행중·종료로 보인다 — 최근 문의 카드는 채팅·절대 시각·마지막 글·상태다", async () => {
    el("back").click();
    assert.equal(el("headTitle").textContent, "둠둠디라 상담", "홈 머리가 관리자가 정한 이름이 아니다");
    const id = await startChat();
    assert.equal(el("viewChat").hidden, false);
    assert.equal(el("headTitle").textContent, "둠둠디라 상담", "대화 머리가 홈과 다른 이름이다");
    assert.equal(el("headSub").textContent, "진행중");
    const at = new Date(2026, 9, 6, 14, 15).toISOString();
    Object.assign(server.convs.find((c) => c.id === id), { last_body: "<b>주문</b> 확인", last_message_at: at, subject: "" });
    mock.timers.tick(15000); // 목록 다시 받기
    await settle();
    const card = el("recentList").innerHTML;
    assert.match(card, /<span class="w-conv-kind">채팅<\/span><span class="when">2026\.10\.06 14:15<\/span>/);
    // 상담 제목이 없으면 마지막 글(이스케이프)이 제목이고 보조 줄은 그리지 않는다
    assert.match(card, /<span class="title">&lt;b&gt;주문&lt;\/b&gt; 확인<\/span>/, "마지막 글이 제목이 아니다");
    assert.doesNotMatch(card, /w-conv-sub/, "상담 제목이 없는데 보조 줄을 그린다");
    assert.match(card, /<span class="chip brand">진행중<\/span>/);
    // 상담 제목이 있으면 그것이 제목이고 마지막 글은 회색 보조 줄이다
    server.convs.find((c) => c.id === id).subject = "배송 문의";
    mock.timers.tick(15000);
    await settle();
    const titled = el("recentList").innerHTML;
    assert.match(titled, /<span class="title">배송 문의<\/span>\s*<span class="w-conv-sub">&lt;b&gt;주문&lt;\/b&gt; 확인<\/span>/, "제목 아래 마지막 글 보조 줄이 없다");
    server.convs.find((c) => c.id === id).subject = "";
    el("back").click();
  });

  test("업로드 진행률을 받아 작성칸에 백분율로 보이고 파일 이름은 이스케이프한다", async () => {
    await startChat();
    pickFile(`<img src=x onerror=alert(1)>.mp4`);
    await settle();
    assert.doesNotMatch(el("attachHint").innerHTML, /<img src=x/, "파일 이름을 HTML 로 그린다");
    assert.match(el("attachHint").innerHTML, /&lt;img src=x onerror=alert\(1\)&gt;\.mp4/);
    const xhr = xhrs.at(-1);
    assert.ok(xhr?.form, "진행률을 받으려면 XMLHttpRequest 로 올려야 한다(onProgress 를 넘긴다)");
    xhr.progress(50, 100);
    assert.match(el("attachHint").querySelector(".w-pill-sub").textContent, /50%/);
    xhr.progress(100, 100);
    assert.match(el("attachHint").querySelector(".w-pill-sub").textContent, /확인 중/);
    xhr.finish(204);
    await settle();
    assert.match(el("attachHint").innerHTML, /보낼 준비가 되었습니다/);
    el("dropAttach").click();
  });

  test("업로드 취소 단추를 누르면 올리기를 멈추고 작성칸이 원래대로 돌아간다", async () => {
    const before = server.attachments;
    pickFile();
    await settle();
    assert.match(el("attachHint").innerHTML, /id="cancelUpload"/, "올리는 중에 취소 단추가 있다");
    el("cancelUpload").click();
    await settle();
    assert.equal(xhrs.at(-1).abortCalled, true, "저장소 올리기를 끊는다");
    assert.equal(el("attachHint").innerHTML, "");
    assert.equal(el("attachRow").hidden, true);
    assert.equal(server.attachments, before + 1);
    assert.doesNotMatch(lastToast(doc), /취소했다|aborted/, "취소는 오류로 알리지 않는다");
    // 다시 올리면 된다
    pickFile();
    await settle();
    xhrs.at(-1).finish(204);
    await settle();
    assert.match(el("attachHint").innerHTML, /보낼 준비가 되었습니다/);
    el("dropAttach").click();
  });

  test("업로드 중 다른 상담으로 옮기면 그 첨부는 옮긴 상담의 작성칸에 붙지 않고 업로드를 멈춘다", async () => {
    const a = server.convs[0].id;
    pickFile();
    await settle();
    const xhr = xhrs.at(-1);
    xhr.ignoreAbort = true; // 이미 다 보낸 뒤라 끊어도 끝까지 가는 경우
    const b = await startChat();
    assert.notEqual(a, b);
    assert.equal(xhr.abortCalled, true, "옮기면 올리던 것을 멈춘다");
    assert.equal(el("attachHint").innerHTML, "", "옮긴 상담의 작성칸은 비어 있다");
    xhr.finish(204);
    await settle();
    assert.doesNotMatch(el("attachHint").innerHTML, /보낼 준비/, "A 의 첨부가 B 작성칸에 붙는다");

    // 저장소 올리기는 끝났고 서버가 파일을 확인하는 사이에 옮긴 경우 — 확인 결과가 늦게 와도 지금 작성칸에 붙지 않는다
    let release;
    server.holdComplete = new Promise((r) => (release = r));
    pickFile();
    await settle();
    xhrs.at(-1).finish(204);
    await settle(); // 완료 확인을 기다리는 중
    const c = await startChat();
    release();
    server.holdComplete = null;
    await settle();
    assert.doesNotMatch(el("attachHint").innerHTML, /보낼 준비/, "확인이 늦게 끝난 앞 상담의 첨부가 지금 작성칸에 붙는다");
    sockets.forEach((ws) => (ws.sent.length = 0));
    submit("B 에 보내는 중요한 글");
    await settle();
    const sends = sentOfType("send");
    assert.equal(sends.length, 1);
    assert.equal(sends[0].conversation_id, c);
    assert.equal(sends[0].attachment_id, undefined, "A 의 첨부를 B 메시지에 싣는다");
  });

  test("보내기가 영구 거절되면 쓴 글을 비어 있는 입력칸에 되돌리고 거절 까닭을 보인다", async () => {
    const b = server.convs[0].id;
    const [sent] = sentOfType("send");
    assert.equal(el("input").value, "", "보내면 입력칸이 빈다");
    frame({ type: "error", code: "invalid", conversation_id: b, client_msg_id: sent.client_msg_id, message: "첨부를 찾을 수 없거나 이미 보낸 첨부입니다" });
    assert.equal(el("input").value, "B 에 보내는 중요한 글", "쓴 글이 사라진다");
    assert.match(lastToast(doc), /첨부를 찾을 수 없거나 이미 보낸 첨부입니다/, "거절 까닭을 보인다");

    // 새로 쓰던 글이 있으면 덮지 않고, 보내지 못한 글을 안내에 보인다
    el("input").value = "";
    submit("두 번째 글");
    await settle();
    const second = sentOfType("send").at(-1);
    el("input").value = "새로 쓰는 중";
    frame({ type: "error", code: "invalid", conversation_id: b, client_msg_id: second.client_msg_id, message: "본문이 너무 길다" });
    assert.equal(el("input").value, "새로 쓰는 중");
    assert.match(lastToast(doc), /본문이 너무 길다.*두 번째 글/);
    el("input").value = "";
  });

  test("일시 오류 알림에는 글을 되돌리지 않는다 — 큐에 남아 다시 나간다", async () => {
    const b = server.convs[0].id;
    submit("다시 나갈 글");
    await settle();
    const item = sentOfType("send").at(-1);
    frame({ type: "error", code: "internal", conversation_id: b, client_msg_id: item.client_msg_id, message: "잠시 뒤 다시 시도해라" });
    assert.equal(el("input").value, "", "큐에 남은 글을 입력칸에도 넣으면 두 번 나간다");
    frame({ type: "ack", conversation_id: b, client_msg_id: item.client_msg_id, seq: 1 });
  });

  // DEF-01(실사용 v0.0.2 U295): 일시 오류에 "잠시 뒤 다시 시도해라" 토스트가 떠 사람이 다시 쓰자 같은 글이 두 줄 저장됐다.
  test("일시 오류 프레임을 받은 글은 토스트 없이 보내는 중으로 남고 다시 나가 정확히 한 번 저장된다 — 영구 거절은 지금처럼 알리고 글을 돌려준다", async () => {
    const b = server.convs[0].id;
    const before = toastEls().length;
    sockets.forEach((ws) => (ws.sent.length = 0));
    submit("DB 가 잠깐 끊긴 순간 쓴 글");
    await settle();
    const item = sentOfType("send").at(-1);
    frame({ type: "error", code: "internal", conversation_id: b, client_msg_id: item.client_msg_id, message: "처리하지 못했습니다. 잠시 후 다시 시도해 주세요" });
    await settle();
    assert.equal(toastEls().length, before, "포기하지 않은 글에 실패 토스트를 띄우면 고객이 다시 써서 두 번 저장된다");
    assert.match(el("pending").innerHTML, /DB 가 잠깐 끊긴 순간 쓴 글/, "보내는 중으로 남는다");
    assert.equal(el("input").value, "");
    // 소켓이 다시 준비되면 같은 client_msg_id 로 다시 나간다. 서버는 client_msg_id 로 멱등이라 한 번만 저장한다
    frame({ type: "ready", conversation_id: b, last_seq: 1 });
    await settle();
    const sends = sentOfType("send").filter((f) => f.body === "DB 가 잠깐 끊긴 순간 쓴 글");
    assert.ok(sends.length >= 2, "다시 나가야 한다");
    assert.deepEqual([...new Set(sends.map((f) => f.client_msg_id))], [item.client_msg_id], "다른 client_msg_id 로 나가면 두 번 저장된다");
    frame({ type: "ack", conversation_id: b, client_msg_id: item.client_msg_id, seq: 2 });
    await settle();
    assert.doesNotMatch(el("pending").innerHTML, /DB 가 잠깐 끊긴 순간 쓴 글/);
    assert.equal(toastEls().length, before, "끝까지 실패 알림이 없다");

    // 영구 거절은 지금처럼 알리고 글을 돌려준다
    submit("BAD 영구 거절될 글");
    await settle();
    const bad = sentOfType("send").at(-1);
    frame({ type: "error", code: "invalid", conversation_id: b, client_msg_id: bad.client_msg_id, message: "첨부를 찾을 수 없거나 이미 보낸 첨부입니다" });
    await settle();
    assert.equal(toastEls().length, before + 1);
    assert.equal(el("input").value, "BAD 영구 거절될 글");
    el("input").value = "";
  });

  test("글과 무관한 일시 오류 프레임은 토스트를 띄우지 않는다 — 구독이 붙지 않았으면 다시 구독하고 읽음은 다시 보낸다", async () => {
    const b = server.convs[0].id;
    const before = toastEls().length;
    frame({ type: "error", code: "internal", conversation_id: b, message: "처리하지 못했습니다. 잠시 후 다시 시도해 주세요" });
    frame({ type: "error", code: "too_many", message: "요청이 너무 많다" });
    await settle();
    assert.equal(toastEls().length, before);
  });

  test("상담원 답을 받으면 홈의 상담원 묶음에 그 이름이 앞에 온다", async () => {
    const b = server.convs[0].id;
    frame({ type: "message", conversation_id: b, seq: 1, id: "m1", kind: "MESSAGE", sender_kind: "AGENT", sender_id: "a1", sender_name: "한상담", body: "안녕하세요", created_at: new Date().toISOString() });
    await settle();
    assert.match(el("agentStack").innerHTML, />한</, "답한 상담원이 묶음에 없다");
    assert.deepEqual(JSON.parse(store.get("gcs.recentAgents")), { owner: "c1", names: ["한상담"] });
  });

  // 날짜 구분선과 말풍선 시각은 widget.js 가 createLog 에 넘긴 함수가 정한다(dayStamp·clockTime). 넘기지 않으면 데스크 모양("오늘", "오후 02:15")이 나온다.
  test("대화의 날짜 구분선은 2026년 10월 06일 꼴이고 오늘도 날짜로 쓴다", async () => {
    const conv = await startChat();
    const now = new Date();
    frame({ type: "message", conversation_id: conv, seq: 1, id: "d1", kind: "MESSAGE", sender_kind: "AGENT", sender_id: "a1", sender_name: "한상담", body: "오늘 글", created_at: now.toISOString() });
    await settle();
    const today = `${now.getFullYear()}년 ${String(now.getMonth() + 1).padStart(2, "0")}월 ${String(now.getDate()).padStart(2, "0")}일`;
    assert.match(el("msgs").innerHTML, new RegExp(`<div class="divider">${today}</div>`), "오늘 글의 구분선이 날짜가 아니다");
    assert.doesNotMatch(el("msgs").innerHTML, /<div class="divider">(오늘|어제)/);
  });

  test("위젯 말풍선의 시각은 24시간 14:15 꼴이다 — 데스크는 오전·오후 그대로다", async () => {
    const conv = await startChat();
    const at = new Date(2026, 9, 6, 14, 15).toISOString();
    frame({ type: "message", conversation_id: conv, seq: 1, id: "t1", kind: "MESSAGE", sender_kind: "AGENT", sender_id: "a1", sender_name: "한상담", body: "오후 글", created_at: at });
    await settle();
    assert.match(el("msgs").innerHTML, /<div class="meta"><span>14:15<\/span>/, "말풍선 시각이 24시간 꼴이 아니다");
    assert.doesNotMatch(el("msgs").innerHTML, /오후 02:15/);
    assert.match(el("msgs").innerHTML, /<div class="divider">2026년 10월 06일<\/div>/);
  });

  // 정책 824 의 위젯 쪽. 실제 widget.js 가 그린 줄의 쪽(.out)을 본다 — 데스크 쪽은 desk-screen 이 실제 desk.js 로 본다(감사 27).
  test("데스크에서 고객 글은 왼쪽이고 상담원·AI·자동 글은 오른쪽이며 시스템 줄은 가운데다 — 위젯은 고객 글만 오른쪽이다", async () => {
    const conv = await startChat();
    const at = new Date(2026, 9, 6, 9, 5).toISOString();
    const lines = [["CUSTOMER", "c1", "나"], ["AGENT", "a1", "한상담"], ["AI", "", "AI 상담원"], ["BOT", "", "시스템"]];
    lines.forEach(([kind, id, name], i) => frame({ type: "message", conversation_id: conv, seq: 1 + i, id: `side-${i}`, kind: "MESSAGE",
      sender_kind: kind, sender_id: id || undefined, sender_name: name, body: `쪽${i}`, created_at: at }));
    await settle();
    const sides = [...el("msgs").innerHTML.matchAll(/<div class="row ([^"]*)" data-sender="([A-Z]+)"/g)].map(([, cls, kind]) => [kind, cls.split(" ").includes("out")]);
    assert.deepEqual(sides.slice(-4), [["CUSTOMER", true], ["AGENT", false], ["AI", false], ["BOT", false]], "위젯은 고객 글만 오른쪽이다");
  });

  test("같은 분 같은 사람 줄이 화면에 붙으면 앞 줄의 시간을 숨긴다", () => {
    const row = (classes, time) => {
      const r = new FakeEl();
      r.classList.add("row", ...classes);
      r.querySelector = (sel) => (sel === ".meta > span" ? { textContent: time } : null);
      return r;
    };
    const msgs = el("msgs");
    msgs.children = [row(["theirs"], "오전 10:01"), row(["theirs", "same"], "오전 10:01"), row(["mine"], "오전 10:01")];
    const obs = observers.find((o) => o.target === msgs);
    assert.ok(obs, "말풍선 칸의 변화를 지켜보지 않는다");
    obs.cb([]);
    assert.deepEqual(msgs.children.map((r) => r.classList.contains("t-grouped")), [true, false, false]);
  });

  const openFromList = async (convId) => {
    await settle();
    const row = doc.convItems?.find((r) => r.dataset.id === convId);
    assert.ok(row, `목록에 ${convId} 가 없다`);
    row.click();
    await settle();
  };

  test("다른 대화로 옮긴 뒤 앞 대화 글의 거절이 소켓으로 오면 까닭만 적힌 알림을 띄우지 않는다 — 그 글은 다시 나가고 거절되면 글과 함께 한 번 알린다", async () => {
    const a = server.convs[0].id;
    el("input").value = "";
    submit("BAD A 상담에 쓴 글");
    await settle();
    const sent = sentOfType("send").at(-1);
    assert.equal(sent.conversation_id, a);
    const b = await startChat();
    assert.notEqual(a, b);
    const before = toastEls().length;
    frame({ type: "error", code: "invalid", conversation_id: a, client_msg_id: sent.client_msg_id, message: "첨부를 찾을 수 없거나 이미 보낸 첨부입니다" });
    assert.equal(toastEls().length, before, "글 없이 까닭만 적힌 알림이 뜬다");
    mock.timers.tick(3000); // REST 주기가 A 큐를 다시 민다. 서버가 영구 거절한다
    await settle();
    const after = toastEls().slice(before);
    assert.equal(after.length, 1, "거절을 한 번만 알린다");
    assert.match(after[0].innerHTML, /첨부를 찾을 수 없거나 이미 보낸 첨부입니다.*다시 열면/);
  });

  test("다른 대화에서 거절된 글은 그 대화를 열 때 입력칸에 들어가고 지금 입력칸에는 들어가지 않는다", async () => {
    const [b, a] = [server.convs[0].id, server.convs[1].id];
    assert.equal(el("input").value, "", "A 상담에 쓴 글이 B 입력칸에 들어간다");
    sockets.forEach((ws) => (ws.sent.length = 0));
    submit("B 에 쓰는 새 글");
    await settle();
    assert.deepEqual(sentOfType("send").map((f) => [f.conversation_id, f.body]), [[b, "B 에 쓰는 새 글"]], "A 의 글이 B 로 나간다");
    frame({ type: "ack", conversation_id: b, client_msg_id: sentOfType("send")[0].client_msg_id, seq: 2 });
    await openFromList(a);
    assert.equal(el("input").value, "BAD A 상담에 쓴 글", "A 를 열면 쓴 글이 입력칸에 돌아온다");
    el("input").value = "";
  });

  test("열린 상담이 없을 때 거절된 글은 복사 단추와 함께 남기고 그 상담을 열면 입력칸에 돌려준다", async () => {
    const a = server.convs[1].id; // 지금 연 상담
    submit("BAD 목록으로 나간 사이 거절될 긴 글입니다. 마흔 자를 넘겨서 알림에는 앞부분만 보이고 복사하면 전부 나와야 한다");
    await settle();
    const sent = sentOfType("send").at(-1);
    el("back").click(); // 목록으로 — 열린 상담이 화면에 없다
    frame({ type: "error", code: "invalid", conversation_id: a, client_msg_id: sent.client_msg_id, message: "첨부를 찾을 수 없거나 이미 보낸 첨부입니다" });
    await settle();
    assert.equal(el("input").value, "", "보이지 않는 입력칸에 넣지 않는다");
    const t = toastEls().at(-1);
    assert.match(t.innerHTML, /보내지 못한 글: BAD 목록으로 나간 사이/);
    assert.match(t.innerHTML, /class="toast-copy">복사</, "복사 단추가 없다");
    t.querySelector(".toast-copy").click();
    await settle();
    assert.equal(clipboard.at(-1), sent.body, "복사하면 글 전부가 나온다");
    await openFromList(a);
    assert.equal(el("input").value, sent.body, "그 상담을 열면 입력칸에 돌아온다");
    el("input").value = "";
  });

  // 감사 10 N26: 화면에 남은 줄이 가리키는 상담이 그 사이 목록에서 빠졌다. 앞 상담을 current 로 남기면 머리·업로드는 앞 상담, 보내기는 이 상담이다.
  test("목록에 없는 상담을 열면 앞 상담을 current 로 남기지 않는다 — 올리는 파일은 연 상담으로 간다", async () => {
    const [b, a] = [server.convs[0].id, server.convs[1].id];
    await openFromList(b);
    const staleRow = doc.convItems.find((r) => r.dataset.id === a);
    const saved = server.convs;
    server.convs = saved.filter((c) => c.id !== a);
    await openFromList(b); // 목록을 다시 읽는다 — a 가 빠졌다
    staleRow.click(); // 아직 화면에 남아 있던 줄
    await settle();
    server.attachPaths.length = 0;
    pickFile();
    await settle();
    assert.deepEqual(server.attachPaths, [`/api/conversations/${a}/attachments`], "연 상담이 아니라 앞 상담으로 파일을 올린다");
    xhrs.at(-1).finish(204);
    await settle();
    server.convs = saved;
  });

  test("고객이 글을 지우거나 고치면 상담 카드의 제목이 곧바로 바뀐다", async () => {
    const conv = await startChat();
    const row = server.convs.find((c) => c.id === conv);
    const at = new Date().toISOString();
    Object.assign(row, { last_body: "환불 문의", last_sender_kind: "CUSTOMER", last_message_at: at });
    frame({ type: "message", conversation_id: conv, seq: 1, id: "e1", kind: "MESSAGE", sender_kind: "CUSTOMER", sender_id: "c1", body: "환불 문의", created_at: at });
    Object.assign(row, { last_body: "잘못 보냈어요" });
    frame({ type: "message", conversation_id: conv, seq: 2, id: "e2", kind: "MESSAGE", sender_kind: "CUSTOMER", sender_id: "c1", body: "잘못 보냈어요", created_at: at });
    await settle();
    assert.match(el("recentList").innerHTML, /<span class="title">잘못 보냈어요<\/span>/);

    // 지우면 서버의 마지막 글은 그 앞 글이다(정책 665). 간격 타이머(15초)를 기다리지 않고 삭제 이벤트로 다시 받는다 — 간격 타이머는 멈춰 있다.
    // 몇 번 받는지는 정하지 않는다(정책 668·669 가 묶는다). 보는 것은 카드의 제목이다.
    row.last_body = "환불 문의";
    frame({ type: "message", conversation_id: conv, seq: 3, id: "e3", kind: "DELETE", ref_message_id: "e2", sender_kind: "CUSTOMER", sender_id: "c1", body: "", created_at: at });
    await settle();
    assert.match(el("recentList").innerHTML, /<span class="title">환불 문의<\/span>/, "지운 뒤 카드가 앞 글로 바뀌지 않는다");

    // 고치면 고친 글이 제목이다.
    row.last_body = "부분 환불 문의";
    frame({ type: "message", conversation_id: conv, seq: 4, id: "e4", kind: "EDIT", ref_message_id: "e1", sender_kind: "CUSTOMER", sender_id: "c1", body: "부분 환불 문의", created_at: at });
    await settle();
    assert.match(el("recentList").innerHTML, /<span class="title">부분 환불 문의<\/span>/, "고친 뒤 카드가 고친 글로 바뀌지 않는다");

    // 새 글도 곧바로 제목이 된다.
    row.last_body = "하나 더";
    frame({ type: "message", conversation_id: conv, seq: 5, id: "e5", kind: "MESSAGE", sender_kind: "CUSTOMER", sender_id: "c1", body: "하나 더", created_at: at });
    await settle();
    assert.match(el("recentList").innerHTML, /<span class="title">하나 더<\/span>/, "새 글이 카드 제목이 되지 않는다");
    Object.assign(row, { last_body: "", last_sender_kind: "", last_message_at: undefined });
  });

  // 정책 668. 지난 글 다시 받기(chat.js backfill)는 줄마다 onMessage 를 부른다. 줄마다 목록을 받으면 106줄 상담을 열 때 목록 요청이 108번 나갔다(감사 19 실측).
  test("지난 글을 다시 받는 동안 상담 목록을 줄마다 다시 받지 않는다 — 다 받고 나면 카드는 마지막 글이다", async () => {
    const conv = await startChat();
    const row = server.convs.find((c) => c.id === conv);
    const at = new Date().toISOString();
    const rows = [];
    for (let seq = 1; seq <= 100; seq++) rows.push({ conversation_id: conv, seq, id: `h${seq}`, kind: "MESSAGE", sender_kind: "AGENT", sender_id: "a1", sender_name: "한상담", body: `지난 글 ${seq}`, created_at: at });
    for (let seq = 101; seq <= 106; seq++) rows.push({ conversation_id: conv, seq, id: `h${seq}`, kind: "EDIT", ref_message_id: "h100", sender_kind: "AGENT", sender_id: "a1", body: `고친 글 ${seq}`, created_at: at });
    server.history[conv] = rows;
    Object.assign(row, { last_body: "고친 글 106", last_sender_kind: "AGENT", last_message_at: at });
    const before = server.listCalls;
    // 107번이 먼저 와서 1~106 의 구멍이 보인다 — chat.js 가 REST 로 메운다(재연결·다시 열기와 같은 길).
    frame({ type: "message", conversation_id: conv, seq: 107, id: "h107", kind: "MESSAGE", sender_kind: "AGENT", sender_id: "a1", sender_name: "한상담", body: "새 글 107", created_at: at });
    await new Promise((r) => setTimeout(r, 5)); // 구멍 메우기는 setTimeout(0) 으로 돈다
    await settle();
    assert.match(el("msgs").innerHTML, /지난 글 1</, "지난 글을 다시 받지 않았다 — 이 테스트의 전제가 틀렸다");
    assert.match(el("msgs").innerHTML, /새 글 107/);
    const calls = server.listCalls - before;
    assert.ok(calls >= 1, "다 받은 뒤 목록을 다시 받지 않는다");
    assert.ok(calls <= 3, `107줄을 받는 동안 목록을 ${calls}번 받았다 — 줄 수만큼 늘면 안 된다`);
    assert.match(el("recentList").innerHTML, /<span class="title">고친 글 106<\/span>/, "다 받은 뒤 카드가 마지막 글이 아니다");
    delete server.history[conv];
    Object.assign(row, { last_body: "", last_sender_kind: "", last_message_at: undefined });
  });

  test("목록을 받는 중에 글이 바뀌면 받은 뒤 한 번 더 받아 카드는 마지막 글이다 — 늦게 온 앞 응답이 카드를 덮지 않는다", async () => {
    const conv = await startChat();
    const row = server.convs.find((c) => c.id === conv);
    const at = new Date().toISOString();
    Object.assign(row, { last_body: "첫 글", last_sender_kind: "CUSTOMER", last_message_at: at });
    let release;
    server.holdList = new Promise((r) => (release = r));
    frame({ type: "message", conversation_id: conv, seq: 1, id: "q1", kind: "MESSAGE", sender_kind: "CUSTOMER", sender_id: "c1", body: "첫 글", created_at: at });
    await settle(); // 첫 목록 요청이 "첫 글" 목록을 들고 묶여 있다
    // 받는 중에 상담원이 연달아 스무 번 고친다. 서버의 마지막 글은 마지막 편집이다.
    for (let i = 1; i <= 20; i++) {
      row.last_body = `고친 글 ${i}`;
      frame({ type: "message", conversation_id: conv, seq: 1 + i, id: `q${1 + i}`, kind: "EDIT", ref_message_id: "q1", sender_kind: "CUSTOMER", sender_id: "c1", body: `고친 글 ${i}`, created_at: at });
      await settle();
    }
    const before = server.listCalls;
    server.holdList = null;
    release();
    await settle();
    assert.match(el("recentList").innerHTML, /<span class="title">고친 글 20<\/span>/, "받는 중에 바뀐 글이 카드에 오지 않는다");
    assert.ok(server.listCalls - before <= 2, `받는 중에 온 편집 스무 번에 목록을 ${server.listCalls - before}번 더 받았다`);
    Object.assign(row, { last_body: "", last_sender_kind: "", last_message_at: undefined });
  });
  // D45: 종료된 상담의 대화 끝에 별점 카드가 붙는다. 별을 고르고 보내면 그 상담으로 PUT 이 나가고 카드는 고맙다는 말로 바뀐다.
  test("상담이 종료되면 대화 안에 별점 카드가 보이고 별과 한 줄 의견을 그 상담으로 보낸다 — 다시 열리면 카드가 사라진다", async () => {
    const convId = await startChat();
    assert.equal(el("csat").hidden, true, "진행 중에는 카드가 없다");
    frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "CLOSED" } });
    await settle();
    assert.equal(el("csat").hidden, false, "종료되면 카드가 보인다");
    assert.match(el("csat").innerHTML, /상담은 어떠셨나요\?/);
    assert.match(el("csat").innerHTML, /id="csatSubmit" disabled/, "별을 고르기 전에는 보내기가 잠겨 있다");
    const pick = (n) => el("csat").dispatch("click", { target: { closest: (sel) => (sel === "[data-score]" ? { dataset: { score: String(n) } } : null) } });
    pick(4);
    assert.match(el("csat").innerHTML, /id="csatStar4" data-score="4" aria-checked="true"/);
    assert.doesNotMatch(el("csat").innerHTML, /id="csatSubmit" disabled/);
    el("csatComment").value = "빨리 답해 주셨어요";
    el("csat").dispatch("submit");
    await settle();
    assert.deepEqual(server.ratingPuts.at(-1), { conv: convId, score: 4, comment: "빨리 답해 주셨어요" });
    assert.match(el("csat").innerHTML, /평가해 주셔서 감사합니다/);
    assert.match(el("csat").innerHTML, /5점 만점에 4점/);
    assert.match(el("csat").innerHTML, /data-action="edit"/, "24시간 안에는 고칠 수 있다");
    frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "OPEN" } });
    await settle();
    assert.equal(el("csat").hidden, true, "다시 열린 상담은 조사를 받지 않는다");
  });

  // ── 고객 종료(D48) ──
  const lastScrim = () => created.filter((e) => e.className === "scrim").at(-1);
  const pendingIn = (convId) => queuedIn("gcs.v1.customer:c1.", convId);

  test("진행 중인 상담의 머리에 상담 종료가 있고 확인 창을 거쳐 닫으면 입력창 대신 새 문의하기가 보인다 — 답이 없던 상담이라 별점은 없다", async () => {
    const saved = server.ratingView;
    server.ratingView = { closed: true, can_rate: false, current: false, rating: null }; // 서버: 답 없이 고객이 닫았다(no_reply)
    const convId = await startChat();
    assert.equal(el("endChat").hidden, false, "진행 중인 상담에 상담 종료가 없다");
    assert.equal(el("composer").hidden, false);
    assert.equal(el("closedFoot").hidden, true);

    el("endChat").click();
    await settle();
    const dialog = lastScrim();
    assert.match(dialog.innerHTML, /상담을 종료할까요\?/);
    assert.match(dialog.innerHTML, /종료하면 이 상담에는 더 이상 메시지를 보낼 수 없습니다\. 새로 문의하려면 새 문의하기를 눌러 주세요\./);
    assert.doesNotMatch(dialog.innerHTML, /data-act="ok" disabled/, "보낼 글이 없는데 확인 단추가 잠겼다");
    dialog.querySelector('[data-act="ok"]').click();
    await settle();

    assert.deepEqual(server.closes.map((c) => c.conv), [convId], "닫기 요청이 그 상담으로 한 번 나간다");
    assert.equal(el("composer").hidden, true, "닫은 상담에 입력창이 남아 있다");
    assert.equal(el("closedFoot").hidden, false, "새 문의하기가 보이지 않는다");
    assert.equal(el("endChat").hidden, true, "종료된 상담에 상담 종료가 남아 있다");
    assert.equal(el("headSub").textContent, "종료");
    assert.equal(el("csat").hidden, true, "답 없이 닫은 상담에 별점을 띄웠다");
    assert.equal(doc.activeElement, el("newInquiry"), "닫은 뒤 초점은 새 문의하기로 간다");

    const before = sentOfType("send").length + server.posted.length;
    submit("닫은 뒤 쓴 글");
    await settle();
    assert.equal(sentOfType("send").length + server.posted.length, before, "닫은 상담에 글을 보냈다");

    el("newInquiry").click();
    await settle();
    assert.notEqual(server.convs[0].id, convId, "새 문의하기가 새 상담을 열지 않았다");
    assert.equal(el("composer").hidden, false, "새 상담에는 입력창이 있다");
    assert.equal(el("closedFoot").hidden, true);
    assert.equal(el("endChat").hidden, false);
    server.ratingView = saved;
  });

  test("위젯에서 닫을 때 보내지 못한 글이 있으면 그 글이 먼저 저장된 뒤 닫혀 큐에 남은 글이 없다", async () => {
    const convId = await startChat();
    submit("닫기 직전에 쓴 글"); // 소켓으로 나갔지만 ack 가 아직 없다
    await settle();
    assert.equal(pendingIn(convId).length, 1);
    el("endChat").click();
    await settle();
    lastScrim().querySelector('[data-act="ok"]').click();
    await settle();
    const closed = server.closes.at(-1);
    assert.equal(closed.conv, convId);
    assert.equal(server.posted.at(closed.postedBefore - 1)?.body, "닫기 직전에 쓴 글", "글이 닫기 전에 저장되지 않았다");
    assert.deepEqual(pendingIn(convId), [], "닫은 뒤 큐에 글이 남았다");
    assert.equal(el("closedFoot").hidden, false);
  });

  test("보내지 못한 글이 있는데 연결이 끊겨 있으면 확인 단추를 잠그고 까닭을 보인다 — 닫지 않는다", async () => {
    const convId = await startChat();
    socket().close(); // 끊겼다
    submit("끊긴 채 쓴 글");
    await settle();
    const closesBefore = server.closes.length;
    el("endChat").click();
    await settle();
    const dialog = lastScrim();
    assert.match(dialog.innerHTML, /data-act="ok" disabled/, "끊긴 채 보낼 글이 있는데 확인 단추가 열려 있다");
    assert.match(dialog.innerHTML, /role="alert">보내지 못한 메시지가 1개 있어 지금은 종료할 수 없습니다/);
    dialog.querySelector('[data-act="cancel"]').click();
    await settle();
    assert.equal(server.closes.length, closesBefore, "닫기 요청이 나갔다");
    assert.equal(el("composer").hidden, false);
    assert.equal(pendingIn(convId).length, 1, "글은 큐에 남는다");
    // 다음 테스트를 위해 다시 붙여 글을 보낸다
    openSocket();
    frame({ type: "ack", conversation_id: convId, client_msg_id: pendingIn(convId)[0].client_msg_id });
    await settle();
  });

  test("닫힌 뒤 늦게 나간 글이 closed_by_customer 로 거절되면 숨은 입력칸이 아니라 보이는 아래 칸에 본문과 복사 단추가 남고 새 문의하기가 새 상담의 입력칸으로 옮긴다 — 다시 보내지 않는다", async () => {
    const LATE = "닫힌 줄 모르고 다른 탭에서 쓴 글입니다. 사라지면 안 된다";
    const convId = await startChat();
    submit(LATE); // 소켓으로 나갔지만 ack 가 아직 없다
    await settle();
    const id = pendingIn(convId)[0].client_msg_id;
    const toastsBefore = toastEls().length;
    // 다른 탭이 먼저 닫았다. 이 탭은 대화 프레임과 거절을 차례로 받는다
    frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "CLOSED", closed_by: "CUSTOMER" } });
    frame({ type: "error", code: "closed_by_customer", conversation_id: convId, client_msg_id: id, message: "종료한 상담에는 메시지를 보낼 수 없습니다. 새로 문의하려면 새 문의하기를 눌러 주세요" });
    await settle();

    assert.equal(el("composer").hidden, true, "닫힌 상담에 입력창이 보인다");
    assert.equal(el("closedFoot").hidden, false);
    assert.equal(el("returned").hidden, false, "돌려받은 글 칸이 보이지 않는다");
    assert.equal(el("returnedBody").textContent, LATE, "보이는 화면에 본문이 없다");
    assert.equal(el("input").value, "", "숨은 입력칸에 넣었다 — 보이지 않고 새로고침하면 사라진다");
    assert.equal(toastEls().length, toastsBefore, "보고 있는 상담이면 알림을 띄우지 않는다 — 새 문의하기를 가린다");
    assert.deepEqual(pendingIn(convId), [], "거절된 글은 큐에서 빠진다");
    assert.match(returnedStored("c1"), /닫힌 줄 모르고/, "새로고침 뒤에도 찾을 수 있게 저장소에 남는다");

    el("returnedCopy").click();
    await settle();
    assert.equal(clipboard.at(-1), LATE, "복사 단추가 본문 전체를 복사한다");

    const postedBefore = server.posted.length;
    el("newInquiry").click();
    await settle();
    assert.notEqual(server.convs[0].id, convId);
    assert.equal(el("input").value, LATE, "새 문의의 입력칸으로 옮기지 않았다");
    assert.equal(el("returned").hidden, true);
    assert.equal(server.posted.length, postedBefore, "돌려받은 글을 저절로 다시 보냈다");
    assert.ok(!sentOfType("send").some((f) => f.body === LATE && f.conversation_id !== convId), "돌려받은 글이 새 상담으로 저절로 나갔다");
    // 입력칸으로 옮겨도 보낼 때까지 저장소에 남는다(DEF-178) — 이 탭을 보내지 않고 닫아도 글이 사라지지 않는다
    assert.match(returnedStored("c1"), /닫힌 줄 모르고/, "입력칸으로 옮기자마자 저장소에서 지웠다 — 보내지 않고 탭을 닫으면 사라진다");
    submit(el("input").value);
    await settle();
    assert.equal(returnedStored("c1"), "", "입력칸에서 보냈는데 저장소에 남았다");
    assert.ok(sentOfType("send").some((f) => f.body === LATE && f.conversation_id !== convId), "입력칸의 글이 새 상담으로 나가지 않았다");
    el("input").value = "";
  });

  test("상담원이 다시 열었는데 입력칸에 쓰던 글이 있으면 돌려받은 글을 덮지 않고 상자에 남긴다 — 지우기를 눌러야 지우고 저절로 지우지 않는다", async () => {
    const LATE = "다른 탭에서 늦게 보낸 글";
    const DRAFT = "이 탭 입력칸에 쓰던 글";
    const convId = await startChat();
    submit(LATE);
    await settle();
    const id = pendingIn(convId)[0].client_msg_id;
    el("input").value = DRAFT; // 이 탭에서 쓰던 글(아직 보내지 않았다)
    frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "CLOSED", closed_by: "CUSTOMER" } });
    frame({ type: "error", code: "closed_by_customer", conversation_id: convId, client_msg_id: id, message: "종료한 상담에는 메시지를 보낼 수 없습니다" });
    await settle();
    assert.equal(el("returned").hidden, false);

    frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "OPEN" } }); // 상담원이 다시 열었다
    await settle();
    assert.equal(el("composer").hidden, false);
    assert.equal(el("input").value, DRAFT, "쓰던 글을 덮었다");
    assert.equal(el("returned").hidden, false, "옮기지 못했는데 상자를 숨겨 글이 화면에 없다");
    assert.equal(el("returnedBody").textContent, LATE);
    assert.match(el("returnedHint").textContent, /옮기지 못했습니다/);
    el("returnedCopy").click();
    await settle();
    assert.equal(clipboard.at(-1), LATE, "글 복사가 본문을 복사하지 않는다");
    frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "OPEN" } }); // 다시 그려도
    await settle();
    assert.equal(el("returned").hidden, false);
    assert.match(returnedStored("c1"), /늦게 보낸 글/, "저절로 지웠다");

    const scrimsBefore = created.filter((e) => e.className === "scrim").length;
    el("returnedClear").click(); // 확인 없이 지운다
    await settle();
    assert.equal(el("returned").hidden, true);
    assert.equal(returnedStored("c1"), "", "지우기를 눌렀는데 남았다");
    assert.equal(el("input").value, DRAFT, "지우기가 입력칸의 글까지 지웠다");
    assert.equal(created.filter((e) => e.className === "scrim").length, scrimsBefore, "지우기가 확인 창을 띄웠다");
    assert.equal(doc.activeElement, el("input"), "사라진 상자 대신 입력칸으로 초점이 가지 않는다 — 초점이 문서 처음으로 떨어진다");
    el("input").value = "";
  });

  // returnLate 는 지금 상담에서 보낸 글이 고객 종료(다른 탭)로 돌아오게 한다. 상자에 그 글이 보이는 닫힌 상담에서 끝난다.
  async function returnLate(text) {
    const convId = await startChat();
    submit(text);
    await settle();
    const id = pendingIn(convId)[0].client_msg_id;
    frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "CLOSED", closed_by: "CUSTOMER" } });
    frame({ type: "error", code: "closed_by_customer", conversation_id: convId, client_msg_id: id, message: "종료한 상담에는 메시지를 보낼 수 없습니다" });
    await settle();
    assert.equal(el("returned").hidden, false, "돌려받은 글 상자가 없다(흉내가 틀렸다)");
    return convId;
  }

  // 감사 28 감독 결정: 지우기는 확인 창 없이 바로 지우고, 잘못 눌렀을 때를 위해 되돌리기 알림을 띄운다.
  test("돌려받은 글을 지우면 보내지 못한 글을 지웠습니다 알림에 되돌리기가 있고 누르면 저장소와 상자에 글이 돌아온다 — 닫힌 상담이면 초점은 새 문의하기다", async () => {
    const LATE = "지웠다가 되돌릴 글";
    await returnLate(LATE);
    el("returnedClear").click();
    await settle();
    assert.equal(el("returned").hidden, true);
    assert.equal(returnedStored("c1"), "", "지우기를 눌렀는데 저장소에 남았다 — 바로 지운다");
    assert.equal(doc.activeElement, el("newInquiry"), "닫힌 상담에서 지운 뒤 초점이 새 문의하기로 가지 않는다");
    const t = toastEls().filter((e) => e.innerHTML.includes("보내지 못한 글을 지웠습니다")).at(-1);
    assert.ok(t, "되돌리기 알림이 없다");
    assert.match(t.innerHTML, /class="toast-action">되돌리기</);
    t.querySelector(".toast-action").click();
    await settle();
    assert.equal(el("returned").hidden, false, "되돌렸는데 상자가 없다");
    assert.equal(el("returnedBody").textContent, LATE);
    assert.match(returnedStored("c1"), /지웠다가 되돌릴 글/, "되돌렸는데 저장소에 없다 — 새로고침하면 사라진다");
    el("returnedClear").click(); // 다음 테스트를 위해 치운다
    await settle();
  });

  // 감사 28 ret2: 같은 상담을 연 두 탭이 다시 열림을 같이 받으면 입력칸이 빈 뒤쪽(숨은) 탭이 글을 옮기고 저장소에서 지웠다.
  // 사람은 그 글을 보지 못했고 그 탭을 닫자 글이 사라졌다. 저절로 옮기는 것은 보이는 탭뿐이다.
  test("숨은 탭은 상담원이 다시 열어도 돌려받은 글을 입력칸으로 옮기지 않고 저장소를 건드리지 않으며 보이게 되면 그때 옮긴다", async () => {
    const LATE = "숨은 탭에서 돌려받은 글";
    const convId = await returnLate(LATE);
    doc.visibilityState = "hidden";
    try {
      frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "OPEN" } }); // 상담원이 다시 열었다
      await settle();
      assert.equal(el("composer").hidden, false);
      assert.equal(el("input").value, "", "숨은 탭이 글을 입력칸으로 옮겼다 — 아무도 보지 않는 곳이다");
      assert.equal(el("returned").hidden, false, "숨은 탭이 상자를 치웠다");
      assert.match(returnedStored("c1"), /숨은 탭에서 돌려받은 글/, "숨은 탭이 저장소의 글을 지웠다");
    } finally {
      doc.visibilityState = "visible";
    }
    fireGlobal("visibilitychange");
    await settle();
    assert.equal(el("input").value, LATE, "보이게 됐는데 입력칸으로 옮기지 않았다");
    assert.equal(el("returned").hidden, true);
    assert.match(returnedStored("c1"), /숨은 탭에서 돌려받은 글/, "옮기자마자 저장소에서 지웠다 — 보낼 때 지운다(DEF-178)");
    submit(LATE);
    await settle();
    assert.equal(returnedStored("c1"), "", "입력칸에서 보냈는데 저장소에 남았다");
    el("input").value = "";
  });

  test("다른 탭이 돌려받은 글을 옮기거나 지우면 이 탭의 상자도 저장소를 따라 사라지고 다른 탭이 받은 글은 나타난다", async () => {
    const LATE = "두 탭이 같이 보는 글";
    await returnLate(LATE);
    const [key] = returnedKeys("c1");
    const kept = store.get(key);
    store.delete(key); // 다른 탭이 보냈거나 지웠다
    fireGlobal("storage", { key });
    await settle();
    assert.equal(el("returned").hidden, true, "저장소에 없는 글을 메모리로 계속 보인다 — 새로고침하면 사라지는 상자다");
    assert.equal(el("returnedBody").textContent, "");
    store.set(key, kept); // 다른 탭이 돌려받았다
    fireGlobal("storage", { key });
    await settle();
    assert.equal(el("returned").hidden, false, "다른 탭이 돌려받은 글이 이 탭에 보이지 않는다");
    assert.equal(el("returnedBody").textContent, LATE);
    el("returnedClear").click();
    await settle();
  });

  // 감사 29 ret2 twotabs: 두 탭이 모두 보일 때 입력칸이 빈 탭이 글을 옮기며 저장소에서 지웠고, 그 탭을 보내지 않고 닫자 글이 사라졌다.
  // 지금은 옮겨도 저장소에 남는다. 입력칸에서 보내 전송 큐에 들어갈 때 지운다. 다른 탭이 보내면 이 탭은 상자를 맞추고 입력칸은 건드리지 않는다.
  test("돌려받은 글을 입력칸으로 옮겨도 보낼 때까지 저장소에 남고 다른 탭이 보내 지우면 이 탭의 상자는 저장소를 따르되 입력칸의 글은 그대로다", async () => {
    const LATE = "두 탭이 같이 입력칸에 가져간 글";
    const convId = await returnLate(LATE);
    frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "OPEN" } }); // 상담원이 다시 열었다
    await settle();
    assert.equal(el("input").value, LATE, "보이는 탭이 입력칸으로 옮기지 않았다");
    assert.equal(el("returned").hidden, true, "입력칸에 옮긴 글을 상자에도 보인다");
    assert.match(returnedStored("c1"), /두 탭이 같이/, "옮기자마자 저장소에서 지웠다 — 이 탭을 보내지 않고 닫으면 글이 사라진다");
    frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "OPEN" } }); // 다시 그려도
    await settle();
    assert.equal(el("input").value, LATE, "다시 그리며 입력칸에 같은 글을 또 넣었다");

    const [key] = returnedKeys("c1");
    store.delete(key); // 같은 글을 입력칸에 가져간 다른 탭이 보냈다
    fireGlobal("storage", { key });
    await settle();
    assert.equal(el("input").value, LATE, "다른 탭이 보냈다고 이 탭 입력칸의 글을 지웠다 — 보낼지는 고객이 정한다");
    assert.equal(el("returned").hidden, true);
    el("input").value = "";
  });

  // 감사 30 erase-other: 옮긴 뒤 입력칸을 비우고 전혀 다른 글을 보내면 돌려받은 글 키를 지웠다 — 그 글은 저장소에도 서버에도 없었다.
  // 지금은 보낸 글이 옮긴 글에서 비롯됐을 때만 지운다(입력칸이 옮긴 글과 이어져 있었거나 본문이 같다, D48).
  const reopenInto = async (convId, text) => {
    frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "OPEN" } }); // 상담원이 다시 열었다
    await settle();
    assert.equal(el("input").value, text, "보이는 탭이 입력칸으로 옮기지 않았다(흉내가 틀렸다)");
  };
  const dropAllReturned = () => {
    for (const k of [...store.keys()]) if (k.startsWith("gcs.returned.")) store.delete(k);
  };
  const typeInto = (text) => {
    el("input").value = text;
    el("input").dispatch("input");
  };

  test("옮긴 돌려받은 글은 입력칸을 비우고 다른 글을 보내면 저장소에 남고 같은 글을 다시 보내면 지운다", async () => {
    const LATE = "옮긴 뒤 비우고 다른 글을 보낸 돌려받은 글";
    await reopenInto(await returnLate(LATE), LATE);
    typeInto("");
    typeInto("전혀 다른 글");
    el("composer").dispatch("submit");
    await settle();
    assert.ok(sentOfType("send").some((f) => f.body === "전혀 다른 글"), "다른 글이 나가지 않았다(흉내가 틀렸다)");
    assert.match(returnedStored("c1"), /옮긴 뒤 비우고/, "입력칸을 비우고 다른 글을 보냈는데 돌려받은 글을 지웠다 — 저장소에도 서버에도 없다");

    submit(LATE); // 복사해 둔 같은 글을 다시 붙여 보냈다
    await settle();
    assert.equal(returnedStored("c1"), "", "옮긴 글과 같은 글을 보냈는데 저장소에 남았다 — 새로고침하면 또 입력칸에 온다");
  });

  test("옮긴 돌려받은 글을 보내면 그 글만 지우고 앞서 옮겼다가 다른 글을 보낸 글은 남긴다", async () => {
    const FIRST = "앞서 옮겼다가 다른 글을 보낸 돌려받은 글";
    await reopenInto(await returnLate(FIRST), FIRST);
    typeInto("");
    typeInto("상관없는 글");
    el("composer").dispatch("submit");
    await settle();
    const SECOND = "나중에 옮겨 보낸 돌려받은 글";
    await reopenInto(await returnLate(SECOND), SECOND);
    el("composer").dispatch("submit");
    await settle();
    assert.doesNotMatch(returnedStored("c1"), /나중에 옮겨/, "옮긴 글을 보냈는데 저장소에 남았다");
    assert.match(returnedStored("c1"), /앞서 옮겼다가/, "다른 글을 보냈던 앞의 돌려받은 글까지 지웠다 — 저장소에도 서버에도 없다");
    dropAllReturned();
  });

  test("옮긴 돌려받은 글을 입력칸에서 고쳐 보내면 저장소에서 지운다", async () => {
    const LATE = "고쳐 보낼 돌려받은 글";
    await reopenInto(await returnLate(LATE), LATE);
    typeInto(`${LATE} — 조금 덧붙임`);
    el("composer").dispatch("submit");
    await settle();
    assert.ok(sentOfType("send").some((f) => f.body === `${LATE} — 조금 덧붙임`));
    assert.equal(returnedStored("c1"), "", "옮긴 글을 고쳐 보냈는데 저장소에 남았다");
  });

  // 감사 30 N4: 지우기를 send 성공 확인(item) 앞으로 옮겨도 통과했다. 그리고 용량이 꽉 차 보낸 글이 이 탭의 메모리 큐에만 있는 동안 지우면
  // 새로고침에 둘 다 사라진다(감사 30 의 1초 창). 그동안은 남기고 ack 로 큐에서 빠지면 지운다.
  test("옮긴 돌려받은 글은 보낸 글이 전송 큐에 들어간 뒤에만 지운다 — 한도를 넘어 못 들어가거나 저장소가 꽉 차 메모리에만 있으면 남기고 ack 를 받으면 지운다", async () => {
    const LATE = "전송 큐에 들어가야 지울 돌려받은 글";
    const conv = await returnLate(LATE);
    await reopenInto(conv, LATE);
    typeInto(`${LATE}${"가".repeat(4000)}`); // 4000자 한도를 넘어 큐에 못 들어간다
    el("composer").dispatch("submit");
    await settle();
    assert.match(returnedStored("c1"), /전송 큐에 들어가야/, "큐에 못 들어간 보내기에 돌려받은 글을 지웠다");

    const FIXED = `${LATE} 고침`;
    typeInto(FIXED);
    const realSet = localStorage.setItem;
    localStorage.setItem = (k, v) => {
      if (String(k).startsWith("gcs.q1.") || String(k).startsWith("gcs.v1.")) throw new Error("QuotaExceededError");
      return realSet.call(localStorage, k, v);
    };
    let id;
    try {
      el("composer").dispatch("submit");
      await settle();
      id = sentOfType("send").filter((f) => f.body === FIXED).at(-1)?.client_msg_id;
      assert.ok(id, "고친 글이 나가지 않았다(흉내가 틀렸다)");
      assert.equal(pendingIn(conv).some((p) => p.client_msg_id === id), false, "흉내가 틀렸다 — 큐가 저장소에 써졌다");
      assert.match(returnedStored("c1"), /전송 큐에 들어가야/, "보낸 글이 메모리에만 있는데 돌려받은 글을 지웠다 — 새로고침하면 둘 다 사라진다");
      frame({ type: "conversation", conversation_id: conv, conversation: { id: conv, state: "OPEN" } }); // ack 전에 다시 그려도
      await settle();
      assert.match(returnedStored("c1"), /전송 큐에 들어가야/, "ack 전에 다시 그리며 메모리에만 있는 글의 돌려받은 글을 지웠다");
    } finally {
      localStorage.setItem = realSet;
    }
    frame({ type: "ack", conversation_id: conv, client_msg_id: id, seq: 99 });
    await settle();
    assert.equal(returnedStored("c1"), "", "ack 를 받았는데 돌려받은 글을 지우지 않았다");
  });

  // 감사 29 blocked·SEED2: 저장이 막힌 탭의 돌려받은 글은 메모리에만 있다. 그 표시를 잃으면 다시 그릴 때 상자가 사라졌고(W9),
  // 고객마다 키 하나라 다른 탭이 그 키를 쓰면 이 탭의 메모리 글이 덮였다. 지금은 글마다 키이고 메모리 글은 저장소와 합친다.
  test("저장이 막힌 탭에서 돌려받은 글은 다시 그려도 상자에 남고 다른 탭이 돌려받은 글을 저장해도 덮이지 않으며 저장이 되면 저장소로 옮긴다", async () => {
    const LATE = "저장이 막힌 탭에서 돌려받은 글";
    const OTHER = "다른 탭이 돌려받아 저장한 글";
    const convId = await startChat();
    const realSet = localStorage.setItem;
    localStorage.setItem = () => { throw new Error("QuotaExceededError"); };
    try {
      submit(LATE);
      await settle();
      const id = sentOfType("send").filter((f) => f.body === LATE).at(-1).client_msg_id;
      frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "CLOSED", closed_by: "CUSTOMER" } });
      frame({ type: "error", code: "closed_by_customer", conversation_id: convId, client_msg_id: id, message: "종료한 상담에는 메시지를 보낼 수 없습니다" });
      await settle();
      assert.equal(el("returned").hidden, false, "저장하지 못한 돌려받은 글의 상자가 없다");
      assert.equal(el("returnedBody").textContent, LATE);
      assert.equal(returnedStored("c1"), "", "흉내가 틀렸다 — 저장이 막혀 있다");
      frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "CLOSED", closed_by: "CUSTOMER" } }); // 다시 그린다
      await settle();
      assert.equal(el("returned").hidden, false, "다시 그리자 저장하지 못한 돌려받은 글이 사라졌다");
      assert.equal(el("returnedBody").textContent, LATE);

      store.set(`gcs.returned.v2.c1.${convId}|other-1`, JSON.stringify({ id: "other-1", conv: convId, body: OTHER, file: "", at: Date.now() + 1 }));
      fireGlobal("storage", { key: `gcs.returned.v2.c1.${convId}|other-1` });
      await settle();
      assert.equal(el("returnedBody").textContent, `${LATE}\n\n${OTHER}`, "다른 탭의 쓰기가 이 탭의 메모리 글을 덮었다");
    } finally {
      localStorage.setItem = realSet;
    }
    frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "CLOSED", closed_by: "CUSTOMER" } });
    await settle();
    assert.match(returnedStored("c1"), /저장이 막힌 탭에서/, "저장이 되는데 메모리에 둔 돌려받은 글을 저장소로 옮기지 않았다 — 새로고침하면 사라진다");
    el("returnedClear").click();
    await settle();
    assert.equal(returnedStored("c1"), "");
  });

  // 옛 형식(고객마다 키 하나, gcs.returned.v1.<고객>)은 읽을 때 글마다 키로 옮긴다. 다 옮긴 뒤에만 옛 키를 지운다 — 옮기다 막히면 남겨 다음에 옮긴다.
  test("옛 형식의 돌려받은 글은 글마다 키로 옮겨 상자에 보이고 다 옮긴 뒤에만 옛 키를 지운다", async () => {
    const convId = await returnLate("옮기기 전에 받은 글");
    el("returnedClear").click();
    await settle();
    const old = JSON.stringify({ [convId]: [{ id: "old-1", body: "옛 형식으로 남은 글", file: "" }] });
    const realSet = localStorage.setItem;
    localStorage.setItem = () => { throw new Error("QuotaExceededError"); };
    try {
      store.set("gcs.returned.v1.c1", old); // 배포 전 코드의 탭이 남겼다
      fireGlobal("storage", { key: "gcs.returned.v1.c1" });
      await settle();
      assert.equal(el("returnedBody").textContent, "옛 형식으로 남은 글", "옛 형식의 글이 보이지 않는다");
      assert.equal(store.get("gcs.returned.v1.c1"), old, "옮기지 못했는데 옛 키를 지웠다 — 새로고침하면 사라진다");
    } finally {
      localStorage.setItem = realSet;
    }
    fireGlobal("storage", { key: "gcs.returned.v1.c1" });
    await settle();
    assert.equal(store.get("gcs.returned.v1.c1"), undefined, "다 옮겼는데 옛 키가 남았다");
    assert.equal(returnedStored("c1"), "옛 형식으로 남은 글", "글마다 키로 옮기지 않았다");
    assert.equal(el("returnedBody").textContent, "옛 형식으로 남은 글");
    el("returnedClear").click();
    await settle();
    assert.equal(returnedStored("c1"), "");
  });

  // 되돌리기 알림은 10초 뒤 저절로 닫힌다 — 문의 번호 알림(D47)과 같은 시간이다(감사 29 W6 생존).
  test("돌려받은 글을 지운 뒤의 되돌리기 알림은 10초 뒤 저절로 닫힌다", async () => {
    await returnLate("10초 알림을 볼 글");
    const realTimeout = globalThis.setTimeout;
    const timers = [];
    globalThis.setTimeout = (fn, ms) => { timers.push({ fn, ms }); return 0; };
    try {
      el("returnedClear").click();
    } finally {
      globalThis.setTimeout = realTimeout;
    }
    const t = toastEls().filter((e) => e.innerHTML.includes("보내지 못한 글을 지웠습니다")).at(-1);
    assert.ok(t?.parentNode?.children.includes(t), "되돌리기 알림이 없다");
    assert.deepEqual(timers.map((x) => x.ms), [10000], "되돌리기 알림이 10초 뒤 닫히지 않는다");
    timers[0].fn();
    assert.ok(!t.parentNode.children.includes(t), "10초가 지났는데 알림이 남았다");
  });

  test("닫는 동안에는 작성칸과 첨부·보내기·상담 종료가 잠기고 끝나면 풀린다", async () => {
    await startChat();
    let release;
    server.holdClose = new Promise((r) => (release = r));
    el("endChat").click();
    await settle();
    lastScrim().querySelector('[data-act="ok"]').click();
    await settle();
    for (const id of ["input", "send", "attachBtn", "endChat"]) assert.equal(el(id).disabled, true, `닫는 동안 ${id} 가 열려 있다`);
    release();
    server.holdClose = null;
    await settle();
    for (const id of ["input", "send", "attachBtn", "endChat"]) assert.equal(el(id).disabled, false, `닫은 뒤 ${id} 가 잠긴 채다`);
    assert.equal(el("closedFoot").hidden, false);
    el("newInquiry").click();
    await settle();
  });

  test("올리는 중이거나 작성칸에 올려 둔 파일이 있으면 확인 창이 보내지 않고 종료된다고 알린다", async () => {
    await startChat();
    pickFile();
    await settle();
    el("endChat").click();
    await settle();
    assert.match(lastScrim().innerHTML, /올리는 중인 파일은 보내지 않고 종료됩니다\./);
    lastScrim().querySelector('[data-act="cancel"]').click();
    await settle();
    xhrs.at(-1).finish(204);
    await settle();
    assert.match(el("attachHint").innerHTML, /보낼 준비/);
    el("endChat").click();
    await settle();
    assert.match(lastScrim().innerHTML, /작성칸에 올려 둔 파일은 보내지 않고 종료됩니다\./);
    lastScrim().querySelector('[data-act="ok"]').click();
    await settle();
    assert.doesNotMatch(el("attachHint").innerHTML, /보낼 준비/, "닫은 상담의 작성칸에 파일이 남았다");
    el("newInquiry").click(); // 파일이 없는 새 상담에서는 경고하지 않는다
    await settle();
    el("endChat").click();
    await settle();
    assert.doesNotMatch(lastScrim().innerHTML, /보내지 않고 종료됩니다/, "파일이 없는데 경고한다");
    lastScrim().querySelector('[data-act="cancel"]').click();
    await settle();
  });

  test("다른 탭에서 닫으면 대화 프레임으로 입력창 대신 새 문의하기가 보이고 상담원이 다시 열면 입력창이 돌아온다 — 상담원이 닫은 상담은 입력창 그대로다", async () => {
    const convId = await startChat();
    frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "CLOSED", closed_by: "CUSTOMER" } });
    await settle();
    assert.equal(el("composer").hidden, true);
    assert.equal(el("closedFoot").hidden, false);
    assert.equal(el("endChat").hidden, true);
    frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "OPEN" } });
    await settle();
    assert.equal(el("composer").hidden, false, "상담원이 다시 열었는데 입력창이 없다");
    assert.equal(el("closedFoot").hidden, true);
    assert.equal(el("endChat").hidden, false);
    frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "CLOSED", closed_by: "AGENT" } });
    await settle();
    assert.equal(el("composer").hidden, false, "상담원이 닫은 상담은 지금처럼 고객이 쓰면 다시 열린다");
    assert.equal(el("closedFoot").hidden, true);
    assert.equal(el("endChat").hidden, true, "종료된 상담에는 상담 종료가 없다");
  });
});

// MutationObserver 는 위젯을 띄우기 전에 심어야 widget.js 가 쓴다.
const observers = [];
globalThis.MutationObserver = class {
  constructor(cb) { this.cb = cb; }
  observe(target) { observers.push({ target, cb: this.cb }); }
};

// ── 임베드 위젯(고객 사이트의 iframe) ─────────────────────────────

describe("위젯 화면 — 임베드", () => {
  let doc, el, win;
  const listeners = {};
  const host = { postMessage() {} };
  const fromHost = (data) => (listeners.message || []).forEach((fn) => fn({ source: host, origin: "https://shop.test", data: { source: "go-chat-host", ...data } }));
  const resize = (frameW, screenW) => {
    win.innerWidth = frameW;
    globalThis.screen = { width: screenW };
    (listeners.resize || []).forEach((fn) => fn());
  };

  // 새로고침 전에 보냈지만 ack 를 못 받은 글이 저장소 큐에 남아 있다. 새 화면은 이 글을 기억하지 못한다.
  const RELOADED = "BAD 새로고침 전에 쓴 글입니다. 다시 보냈는데 서버가 영구 거절하는 글이고 마흔 자를 넘는다";
  before(() => {
    const key = "gcs.v1.customer:c1.conv-1";
    const saved = JSON.parse(store.get(key) || '{"lastSeq":0}');
    store.set(key, JSON.stringify({ lastSeq: saved.lastSeq, pending: [{ client_msg_id: "reload-1", body: RELOADED, at: Date.now(), tries: 0 }] }));
  });

  test("임베드 위젯은 관리자 색을 쓰고 호스트가 data-color 를 줬을 때만 그 색이 덮는다", async () => {
    ({ doc, el } = installDocument());
    globalThis.location = { search: "?embed=1", origin: "http://chat.test", protocol: "http:", host: "chat.test" };
    globalThis.screen = { width: 1440 };
    win = { parent: host, innerWidth: 400, addEventListener: (t, fn) => (listeners[t] ||= []).push(fn) };
    globalThis.window = win;
    server.customer = "c1";
    server.color = "#e8590c";
    await import("./widget.js?embed");
    await settle();
    const brand = () => doc.documentElement.style.getPropertyValue("--brand");
    assert.equal(brand(), "#e8590c", "임베드에도 관리자 색이 들어간다");
    assert.equal(doc.documentElement.style.getPropertyValue("--brand-ink"), "#111111", "밝은 관리자 색이면 글자를 어둡게");

    fromHost({ type: "boot", config: { color: "", position: "" } }); // 지금 설치 코드(data-color 없음)
    await settle();
    assert.equal(brand(), "#e8590c", "data-color 가 없으면 관리자 색 그대로다");

    fromHost({ type: "boot", config: { color: "#0c64e8" } }); // 옛 설치 코드의 data-color
    await settle();
    assert.equal(brand(), "#0c64e8", "호스트가 준 색이 덮는다");
    mock.timers.tick(5 * 60 * 1000); // 설정을 다시 받아도
    await settle();
    assert.equal(brand(), "#0c64e8", "설정을 다시 받아도 호스트 색이 남는다");
  });

  test("새로고침 뒤 다시 보낸 글이 영구 거절되면 그 글을 복사 단추와 함께 남기고 그 대화를 열면 입력칸에 돌려준다", async () => {
    const t = toastEls().find((e) => e.innerHTML.includes("BAD 새로고침 전에 쓴 글"));
    assert.ok(t, "다시 보낸 글이 거절되면 그 글이 어디에도 없다");
    assert.match(t.innerHTML, /첨부를 찾을 수 없거나 이미 보낸 첨부입니다/);
    assert.match(t.innerHTML, /class="toast-copy">복사</);
    t.querySelector(".toast-copy").click();
    await settle();
    assert.equal(clipboard.at(-1), RELOADED);
    assert.equal(el("input").value, "", "홈에 있는 동안 입력칸에 넣지 않는다");
    await settle();
    const row = doc.convItems?.find((r) => r.dataset.id === "conv-1");
    assert.ok(row, "목록에 그 대화가 없다");
    row.click();
    await settle();
    assert.equal(el("input").value, RELOADED, "그 대화를 열면 입력칸에 돌아온다");
  });

  test("같은 고객이면 저장된 상담원 묶음을 그대로 보인다", () => {
    assert.match(el("agentStack").innerHTML, />한</);
  });

  test("세션 중간에 다른 고객으로 바뀌면(회원 연동) 상담원 묶음을 그 고객 것으로 다시 읽는다", async () => {
    server.member = "c2";
    fromHost({ type: "identify", memberToken: "signed-c2" });
    await settle();
    assert.doesNotMatch(el("agentStack").innerHTML, />한</, "앞 고객이 받은 상담원 이름이 남는다");
    assert.match(el("agentStack").innerHTML, /w-avatar-icon/, "답을 받은 적이 없으면 헤드폰 아이콘이다");
  });

  // D46: 이어 주지 않은 세션은 이 브라우저에서 다시 닿을 길이 없다. 서버에서 끊어 둔다 — 회원 세션이면 공유 PC 의 다음 사람이 연 익명 위젯이 끊는다.
  test("들고 있던 세션을 서버가 이어 주지 않으면 그 세션을 서버에서 끊는다", () => {
    assert.ok(server.logouts.includes("Bearer tok-c1"), `익명→회원으로 바뀌며 버린 앞 세션을 끊지 않았다: ${server.logouts}`);
  });

  // D46 공유 PC: 호스트가 회원 로그아웃 때 ChatWidget.logout() 을 부른다. 같은 브라우저의 다음 사람에게 회원의 세션·보내지 못한 글이 남지 않는다.
  test("호스트가 위젯 로그아웃을 부르면 서버 세션을 끊고 위젯 저장소를 비운 뒤 저장된 토큰 없이 익명으로 다시 연다", async () => {
    assert.equal(store.get("gcs.customerToken"), "tok-c2", "회원 c2 로 들어와 있다");
    store.set("gcs.v1.customer:c2.conv-9", JSON.stringify({ lastSeq: 3, pending: [{ client_msg_id: "m-1", body: "회원의 보내지 못한 글", at: Date.now(), tries: 0 }] }));
    // 지금 형식의 전송 대기 글(글 이름 공간, DEF-165)과 돌려받은 글(D48)도 회원의 글이다(감사 28 A15)
    store.set("gcs.q1.customer:c2.conv-9|m-2", JSON.stringify({ client_msg_id: "m-2", body: "회원이 끊긴 채 쓴 글", at: Date.now(), n: 1, tries: 0 }));
    store.set("gcs.returned.v1.c2", JSON.stringify({ "conv-9": [{ id: "m-3", body: "회원이 돌려받은 글", file: "" }] }));
    store.set("gcs.returned.v2.c2.conv-9|m-4", JSON.stringify({ id: "m-4", conv: "conv-9", body: "회원이 돌려받은 글(글마다 키)", file: "", at: 1 }));
    const calls = server.sessionCalls.length;
    const revoked = server.logouts.length;
    fromHost({ type: "logout" });
    await settle();
    assert.deepEqual(server.logouts.slice(revoked), ["Bearer tok-c2"], "회원의 서버 세션을 끊는다");
    assert.equal(store.get("gcs.v1.customer:c2.conv-9"), undefined, "회원의 보내지 못한 글이 남았다");
    assert.equal(store.get("gcs.q1.customer:c2.conv-9|m-2"), undefined, "회원의 전송 대기 글(글 키)이 남았다 — 다음 사람의 브라우저에 남는다");
    assert.equal(store.get("gcs.returned.v1.c2"), undefined, "회원이 돌려받은 글이 남았다");
    assert.equal(store.get("gcs.returned.v2.c2.conv-9|m-4"), undefined, "회원이 돌려받은 글(글마다 키)이 남았다");
    assert.ok(![...store.keys()].some((k) => k.startsWith("gcs.") && k.includes("c2")), `회원 것이 남았다: ${[...store.keys()]}`);
    const next = server.sessionCalls.slice(calls);
    assert.deepEqual(next.map((c) => [c.kind, c.token]), [["anon", ""]], "저장된 토큰 없이 익명 세션을 새로 연다");
    assert.equal(store.get("gcs.customerToken"), "tok-c1");
  });

  // 감사 29 undo-logout(W5 생존): 지우기 → 로그아웃 → 되돌리기. 되돌리기가 고객을 확인하지 않으면 다음 고객의 키에 앞사람의 글을 썼다.
  // 로그아웃은 알림도 치운다 — 앞사람의 되돌리기·복사 단추가 다음 사람 화면에 남지 않는다.
  test("돌려받은 글을 지우고 로그아웃하면 되돌리기 알림이 사라지고 남은 단추를 눌러도 다음 고객의 저장소와 상자에 앞사람의 글을 되살리지 않는다", async () => {
    const SECRET = "앞사람이 돌려받은 비밀 글";
    server.member = "c2";
    fromHost({ type: "identify", memberToken: "signed-c2" });
    await settle();
    el("startChat").click();
    await settle();
    openSocket();
    const convId = server.convs[0].id;
    el("input").value = SECRET;
    el("composer").dispatch("submit");
    await settle();
    const id = sentOfType("send").filter((f) => f.body === SECRET).at(-1).client_msg_id;
    frame({ type: "conversation", conversation_id: convId, conversation: { id: convId, state: "CLOSED", closed_by: "CUSTOMER" } });
    frame({ type: "error", code: "closed_by_customer", conversation_id: convId, client_msg_id: id, message: "종료한 상담에는 메시지를 보낼 수 없습니다" });
    await settle();
    assert.equal(el("returnedBody").textContent, SECRET, "돌려받은 글 상자가 없다(흉내가 틀렸다)");
    el("returnedClear").click();
    await settle();
    const t = toastEls().filter((e) => e.innerHTML.includes("보내지 못한 글을 지웠습니다")).at(-1);
    assert.ok(t?.parentNode?.children.includes(t), "되돌리기 알림이 없다");
    const undo = t.querySelector(".toast-action");

    fromHost({ type: "logout" });
    await settle();
    assert.ok(!t.parentNode.children.includes(t), "로그아웃했는데 앞사람의 되돌리기 알림이 남았다");
    undo.click(); // 남아 있던 단추를 누른 셈이다
    await settle();
    assert.deepEqual([...store.keys()].filter((k) => k.startsWith("gcs.returned.")), [], "로그아웃 뒤 되돌리기가 앞사람의 글을 저장소에 썼다");
    assert.equal(el("returned").hidden, true);
    assert.doesNotMatch(el("returnedBody").textContent, /비밀 글/, "다음 고객 화면에 앞사람의 글이 보인다");
  });

  // 감사 30: 로그아웃 말고도 다른 고객이 되는 길(회원 연동으로 회원 A→B)이 있다. 그 순간 앞 고객의 알림을 치운다. 같은 고객의 새 토큰이면 남긴다.
  test("다른 고객으로 세션이 바뀌면 앞 고객의 알림을 치우고 같은 고객의 새 토큰이면 남긴다", async () => {
    const { toast } = await import("./ui.js");
    const attached = (t) => !!t?.parentNode?.children.includes(t);
    const toastOf = (text) => toastEls().filter((e) => e.innerHTML.includes(text)).at(-1);
    assert.equal(store.get("gcs.customerToken"), "tok-c1", "익명 c1 로 들어와 있다(흉내가 틀렸다)");
    toast("익명 c1 의 알림");
    const anon = toastOf("익명 c1 의 알림");
    fromHost({ type: "identify", memberToken: "signed-c3" });
    await settle();
    assert.equal(store.get("gcs.customerToken"), "tok-c3", "다른 고객으로 바뀌지 않았다(흉내가 틀렸다)");
    assert.ok(!attached(anon), "다른 고객이 됐는데 앞 고객의 알림이 남았다");

    toast("회원 c3 의 알림");
    const mine = toastOf("회원 c3 의 알림");
    fromHost({ type: "identify", memberToken: "signed-c3~2" });
    await settle();
    assert.ok(attached(mine), "같은 고객이 새 토큰을 받았는데 알림을 치웠다");
  });


  test("임베드 위젯은 iframe 폭이 화면 폭과 같고 480px 이하일 때만 휴대폰 화면으로 그린다", () => {
    assert.equal(doc.body.classList.contains("w-phone"), false, "데스크톱 400px 판");
    resize(390, 390);
    assert.equal(doc.body.classList.contains("w-phone"), true, "휴대폰에서 화면 전체로 커졌다");
    resize(400, 390);
    assert.equal(doc.body.classList.contains("w-phone"), false, "viewport meta 없는 호스트의 400×720 iframe");
    resize(400, 1440);
    assert.equal(doc.body.classList.contains("w-phone"), false);
    mock.timers.reset();
  });
});

// ── 세션 수립과 돌려받는 첨부(감사 9) ────────────────────────────────

describe("위젯 화면 — 세션 수립은 한 줄이다", () => {
  let doc, el, send, sendFrom, toHost, toHostOrigins;
  async function mount(tag) {
    ({ doc, el } = installDocument());
    globalThis.location = { search: "?embed=1", origin: "http://chat.test", protocol: "http:", host: "chat.test" };
    globalThis.screen = { width: 1440 };
    const listeners = {};
    toHost = [];
    toHostOrigins = [];
    const host = {
      postMessage: (msg, origin) => {
        toHost.push(msg);
        toHostOrigins.push(origin);
      },
    };
    globalThis.window = { parent: host, innerWidth: 400, addEventListener: (t, fn) => (listeners[t] ||= []).push(fn) };
    // sendFrom 은 아무 창(source)·출처(origin)에서 위젯에 보낸 메시지다. send 는 부모 창(호스트)이 보낸 것이다.
    sendFrom = (source, origin, data) => (listeners.message || []).forEach((fn) => fn({ source, origin, data: { source: "go-chat-host", ...data } }));
    send = (data) => sendFrom(host, "https://shop.test", data);
    store.delete("gcs.customerToken");
    server.sessionCalls = [];
    server.holdSessions = false;
    await import(`./widget.js?${tag}`);
    await settle();
  }
  const kinds = () => server.sessionCalls.map((c) => c.kind);

  before(() => mock.timers.enable({ apis: ["setInterval"] }));
  after(() => mock.timers.reset());

  // 감사 9: 회원 첫 방문 9번 중 1번이 익명으로 열렸다. boot 설정의 회원 토큰·대기열의 identify·익명 boot 가 동시에 나갔다.
  test("위젯 머리 이름의 기본값은 고객센터이고 인사 문장은 인사말이 따로 갖는다", async () => {
    server.title = "";
    try {
      await mount("untitled");
      assert.equal(el("headTitle").textContent, "고객센터", "이름이 없을 때 머리에 기본 이름이 아니다");
    } finally {
      server.title = "둠둠디라 상담";
    }
    // 이미 공백 이름을 저장한 설치도 빈 이름과 같다(정책 670)
    server.title = "   ";
    try {
      await mount("blank-title");
      assert.equal(el("headTitle").textContent, "고객센터", "공백뿐인 이름을 그대로 머리에 쓴다");
    } finally {
      server.title = "둠둠디라 상담";
    }
    // 설정을 받기 전 첫 화면(마크업)도 같은 이름이다
    const html = (await import("node:fs")).readFileSync(new URL("./widget.html", import.meta.url), "utf8");
    assert.match(html, /<h1 id="headTitle">고객센터<\/h1>/);
  });

  test("회원 토큰을 줄 호스트에서는 익명 세션을 열지 않는다 — embed.js 직후 identify 를 불러도 회원으로 열린다", async () => {
    await mount("serial");
    server.holdSessions = true;
    // embed.js 의 순서: identify 가 ready 전에 대기열에 들어가 boot 설정에 같은 토큰이 실린다. ready 뒤에 boot, 이어서 대기열의 identify 다.
    send({ type: "boot", config: { memberToken: "signed-m1" } });
    send({ type: "identify", memberToken: "signed-m1" });
    await settle();
    assert.deepEqual(kinds(), ["member:signed-m1"], "익명 세션을 따로 열면 떠도는 익명 고객이 생기고 늦게 오면 회원을 덮는다");
    server.holdSessions = false;
    server.sessionCalls.forEach((c) => c.release?.());
    await settle();
    assert.equal(store.get("gcs.customerToken"), "tok-m1");
    assert.equal(socketToken(socket()), "tok-m1");
  });

  test("세션 응답은 가장 마지막에 시작한 요청만 채택한다 — 늦게 온 익명 응답이 회원을 덮지 않는다", async () => {
    await mount("latest");
    server.holdSessions = true;
    send({ type: "boot", config: {} }); // 그때는 회원인지 몰랐다 → 익명
    await settle();
    send({ type: "identify", memberToken: "signed-m2" });
    await settle();
    assert.deepEqual(kinds(), ["anon", "member:signed-m2"]);
    server.holdSessions = false;
    server.sessionCalls[1].release(); // 회원 응답이 먼저
    await settle();
    server.sessionCalls[0].release(); // 익명 응답이 늦게
    await settle();
    assert.equal(store.get("gcs.customerToken"), "tok-m2", "늦게 온 익명 응답이 회원 세션을 덮는다");
    assert.equal(socketToken(socket()), "tok-m2");
    // 회원이 바뀌는 중에 또 바뀌었다(m3 → m4). 앞 회원의 응답이 늦게 와도 마지막 회원이 남는다
    server.holdSessions = true;
    send({ type: "identify", memberToken: "signed-m3" });
    send({ type: "identify", memberToken: "signed-m4" });
    await settle();
    server.holdSessions = false;
    server.sessionCalls.at(-1).release();
    await settle();
    server.sessionCalls.at(-2).release();
    await settle();
    assert.equal(store.get("gcs.customerToken"), "tok-m4", "늦게 온 앞 회원 응답이 마지막 회원 세션을 덮는다");
  });

  test("회원 세션이 실패하면 회원 정보를 확인하지 못했다고 문의 번호와 함께 알리고 익명으로 이어 간다", async () => {
    await mount("fallback");
    send({ type: "boot", config: { memberToken: "bad-token" } });
    await settle();
    assert.deepEqual(kinds(), ["member:bad-token", "anon"], "회원 세션이 실패한 뒤에만 익명을 연다");
    assert.equal(store.get("gcs.customerToken"), `tok-${server.customer}`);
    assert.match(toastEls().at(-1).innerHTML, /회원 정보를 확인하지 못했습니다\. 비회원으로 상담을 이어 갑니다/);
    // 서버 문구(회원 연동 설정·토큰 형식)는 고객이 할 수 있는 일이 아니라 보이지 않는다. 운영자는 문의 번호로 찾는다(D47).
    assert.doesNotMatch(toastEls().at(-1).innerHTML, /회원 토큰이 올바르지 않다/);
    assert.match(toastEls().at(-1).innerHTML, /문의 번호 feedc0de/);
  });

  function pickFile() {
    el("file").files = [new File(["\x00\x00\x00\x18ftypisom"], "clip.mp4", { type: "video/mp4" })];
    el("file").dispatch("change");
  }
  function submit(text) {
    el("input").value = text;
    el("composer").dispatch("submit");
  }

  // 감사 9, U282: 포기된 글을 돌려줄 때 본문만 돌려주고 사진을 조용히 잃었다.
  test("보낸 글이 재시도 상한으로 포기되면 아직 보내지 않은 첨부를 같은 상담의 작성칸에 다시 붙인다", async () => {
    openSocket();
    el("startChat").click();
    await settle();
    const convId = server.convs[0].id;
    pickFile();
    await settle();
    xhrs.at(-1).finish(204);
    await settle();
    assert.match(el("attachHint").innerHTML, /보낼 준비가 되었습니다/);
    submit("사진 보냅니다");
    await settle();
    const sent = sentOfType("send").at(-1);
    assert.ok(sent.attachment_id);
    assert.equal(el("attachHint").innerHTML, "", "보내면 작성칸 첨부가 빈다");
    for (let i = 0; i < 20; i++) frame({ type: "error", code: "conflict", conversation_id: convId, client_msg_id: sent.client_msg_id, message: "이미 있습니다" });
    assert.equal(el("input").value, "사진 보냅니다", "글이 돌아온다");
    assert.match(el("attachHint").innerHTML, /clip\.mp4.*보낼 준비가 되었습니다/s, "첨부를 잃는다");
    assert.match(toastEls().at(-1).innerHTML, /파일을 작성칸에 다시 붙여 두었습니다/);
    el("composer").dispatch("submit");
    await settle();
    assert.equal(sentOfType("send").at(-1).attachment_id, sent.attachment_id, "다시 보내면 같은 첨부가 실린다");
  });

  test("다시 붙일 수 없는 첨부는 파일 이름을 들어 다시 올려 달라고 알린다", async () => {
    const convId = server.convs[0].id;
    const again = sentOfType("send").at(-1);
    frame({ type: "error", code: "invalid", conversation_id: convId, client_msg_id: again.client_msg_id, message: "첨부를 찾을 수 없거나 이미 보낸 첨부입니다" });
    assert.equal(el("input").value, "사진 보냅니다");
    assert.equal(el("attachHint").innerHTML, "", "이미 쓸 수 없는 첨부를 다시 붙인다");
    assert.match(toastEls().at(-1).innerHTML, /파일 clip\.mp4 은 보내지 못했습니다\. 다시 올려 주세요/, "첨부를 잃은 것을 알리지 않는다");
    el("input").value = "";
  });

  test("보낸 글이 오래 ack 를 못 받으면 보내는 중 줄 아래에 다시 연결되면 보낸다고 알린다", async () => {
    const convId = server.convs[0].id;
    el("back").click();
    // 끊긴 동안 쓴 글이 저장소 큐에 오래 남아 있다
    store.set(`gcs.v1.customer:${server.customer}.${convId}`, JSON.stringify({ lastSeq: 0, pending: [{ client_msg_id: "old-1", body: "끊긴 동안 쓴 글", at: Date.now() - 60000, tries: 0 }] }));
    socket().close();
    send({ type: "open" }); // 목록을 다시 그린다
    await settle();
    doc.convItems.find((r) => r.dataset.id === convId).click();
    await settle();
    assert.match(el("pending").innerHTML, /끊긴 동안 쓴 글/);
    assert.match(el("pending").innerHTML, /연결이 끊겨 있습니다\. 다시 연결되면 보냅니다/, "오래 기다리는 글에 상태 안내가 없다");
    openSocket();
    frame({ type: "ack", conversation_id: convId, client_msg_id: "old-1", seq: 1 });
    assert.doesNotMatch(el("pending").innerHTML, /다시 연결되면/, "보낸 뒤에도 안내가 남는다");
  });

  // 감사 10 N25: 토큰 만료(401) 뒤 익명으로 다시 열면 회원의 상담이 익명 고객 것으로 바뀐다.
  test("토큰이 만료돼 다시 열 때도 회원이면 회원 세션을 연다", async () => {
    await mount("expired");
    send({ type: "boot", config: { memberToken: "signed-m5" } });
    await settle();
    assert.equal(store.get("gcs.customerToken"), "tok-m5");
    server.sessionCalls = [];
    server.listExpired = 1;
    send({ type: "open" }); // 목록을 다시 읽는다 — 401
    await settle();
    assert.deepEqual(kinds(), ["member:signed-m5"], "만료 뒤 익명 세션을 연다");
    assert.equal(store.get("gcs.customerToken"), "tok-m5");
  });

  // 감사 20 A22: 익명 입구가 들고 간 토큰을 이어 주지 않으면(회원 세션·지워진 세션) 이 브라우저는 그 세션에 다시 닿을 길이 없다. 서버에서 끊어 둔다.
  test("익명 세션을 열 때 들고 간 토큰을 서버가 이어 주지 않으면 그 세션을 서버에서 끊는다", async () => {
    await mount("boot-revoke");
    store.set("gcs.customerToken", "tok-someone-else");
    const before = server.logouts.length;
    send({ type: "boot", config: {} });
    await settle();
    assert.equal(store.get("gcs.customerToken"), `tok-${server.customer}`);
    assert.deepEqual(server.logouts.slice(before), ["Bearer tok-someone-else"], "이어 주지 않은 세션을 끊지 않았다");
  });

  // 감사 20(logcat 결정): 회원 세션이 회원 토큰 exp 에 끝나면 전에는 같은 만료 토큰으로 400·401 을 8초마다 끝없이 되풀이했다.
  const waitBackoff = () => new Promise((r) => setTimeout(r, 900)); // 소켓 재연결 백오프(0.5초 + 지터 0.25초)
  const seedPending = (customer, body) => store.set(`gcs.v1.customer:${customer}.conv-p`, JSON.stringify({ lastSeq: 0, pending: [{ client_msg_id: `p-${customer}`, body, at: Date.now(), tries: 0 }] }));
  const pendingOf = (customer) => queuedIn(`gcs.v1.customer:${customer}.`, "conv-p").map((p) => p.body);

  test("회원 세션이 끝나면 호스트의 getMemberToken 으로 새 회원 토큰을 받아 끊김 없이 이어 간다 — 보내지 못한 글은 큐에 남는다", async () => {
    await mount("member-refresh");
    send({ type: "boot", config: { memberToken: "signed-m7", memberRefresh: true } });
    await settle();
    openSocket();
    assert.equal(store.get("gcs.customerToken"), "tok-m7");
    seedPending("m7", "만료 직전에 쓴 글");
    // 회원 토큰 exp 가 지나 서버가 세션을 끝냈다. 소켓이 끊기고 다시 붙으려는 티켓이 401 이다.
    server.deadTokens.add("tok-m7");
    server.expiredMembers.add("signed-m7");
    server.sessionCalls = [];
    socket().close();
    await waitBackoff();
    await settle();
    const asks = toHost.filter((m) => m.type === "member-token-request");
    assert.equal(asks.length, 1, "호스트에 새 회원 토큰을 묻지 않았다");
    send({ type: "member-token", id: asks[0].id, memberToken: "signed-m7~2" });
    await settle();
    assert.deepEqual(kinds(), ["member:signed-m7", "member:signed-m7~2"], "만료 토큰으로 한 번, 새 토큰으로 한 번만 연다");
    assert.equal(store.get("gcs.customerToken"), "tok-m7~2");
    assert.equal(socketToken(socket()), "tok-m7~2", "소켓이 새 세션으로 다시 붙지 않았다");
    assert.equal(el("sessionNotice").hidden, true, "되살렸는데 만료 안내가 남았다");
    assert.deepEqual(pendingOf("m7"), ["만료 직전에 쓴 글"], "같은 회원의 보내지 못한 글이 사라졌다");
  });

  test("호스트에 getMemberToken 이 없으면 다시 시도를 멈추고 새로고침 안내를 띄운다 — 400·401 을 되풀이하지 않고 보내지 못한 글은 남는다", async () => {
    await mount("member-expired");
    send({ type: "boot", config: { memberToken: "signed-m8" } });
    await settle();
    openSocket();
    seedPending("m8", "보내지 못한 회원 글");
    server.deadTokens.add("tok-m8");
    server.expiredMembers.add("signed-m8");
    server.sessionCalls = [];
    socket().close();
    await waitBackoff();
    await settle();
    assert.deepEqual(toHost.filter((m) => m.type === "member-token-request"), [], "새 토큰을 줄 수 없는 호스트에 묻는다");
    assert.equal(el("sessionNotice").hidden, false, "안내가 없다");
    assert.equal(el("sessionNoticeText").textContent, "로그인이 만료되었습니다. 페이지를 새로고침해 주세요");
    assert.equal(el("sessionRetry").hidden, true, "다시 해도 같은 안내에 다시 연결 단추를 둔다");
    assert.match(toastEls().at(-1).innerHTML, /로그인이 만료되었습니다\. 페이지를 새로고침해 주세요/);
    const asked = server.ticketAsks.filter((t) => t === "tok-m8").length;
    const calls = server.sessionCalls.length;
    // 시간이 흘러도, 목록이 다시 401 을 받아도 더 묻지 않는다
    await waitBackoff();
    await waitBackoff();
    server.listExpired = 2;
    send({ type: "open" });
    await settle();
    assert.equal(server.listExpired, 2, "멈춘 뒤에도 끝난 세션으로 상담 목록을 묻는다(401 반복)");
    server.listExpired = 0;
    assert.equal(server.ticketAsks.filter((t) => t === "tok-m8").length, asked, "멈춘 뒤에도 티켓을 다시 받으러 간다(401 반복)");
    assert.equal(server.sessionCalls.length, calls, "멈춘 뒤에도 만료된 회원 토큰으로 다시 연다(400 반복)");
    assert.deepEqual(pendingOf("m8"), ["보내지 못한 회원 글"], "보내지 못한 글을 버렸다");
    // 호스트가 나중에 새 회원 토큰으로 identify 하면 그대로 이어 간다
    send({ type: "identify", memberToken: "signed-m8~2" });
    await settle();
    assert.equal(store.get("gcs.customerToken"), "tok-m8~2");
    assert.equal(socketToken(socket()), "tok-m8~2", "멈췄던 소켓이 다시 붙지 않았다");
    assert.equal(el("sessionNotice").hidden, true);
  });

  // 감사 22(감독 결정): 앞단의 JSON 아닌 401 한 번에 익명 위젯이 새 고객으로 다시 열려 지난 상담과 큐의 글을 잃었다.
  test("앞단의 401 HTML 은 세션 만료가 아니라 익명 위젯이 새 고객으로 바뀌지 않고 큐의 글이 남으며 우리 서버의 JSON 401 만 새 세션을 연다", async () => {
    await mount("front401");
    send({ type: "boot", config: {} });
    await settle();
    const tok = store.get("gcs.customerToken");
    seedPending(server.customer, "앞단에 막혔던 글");
    server.sessionCalls = [];
    server.listFront401 = 1;
    send({ type: "open" });
    await settle();
    assert.equal(server.listFront401, 0, "상담 목록을 묻지 않았다");
    assert.deepEqual(server.sessionCalls, [], "앞단의 401 HTML 로 새 세션을 열었다");
    assert.equal(store.get("gcs.customerToken"), tok, "고객 토큰이 바뀌었다");
    assert.deepEqual(pendingOf(server.customer), ["앞단에 막혔던 글"], "큐의 글을 잃었다");
    server.listExpired = 1;
    send({ type: "open" });
    await settle();
    assert.deepEqual(kinds(), ["anon"], "우리 서버의 JSON 401 unauthorized 는 세션 만료라 익명 세션을 다시 연다");
  });

  test("호스트의 getMemberToken 이 빈 값을 주면 같은 안내로 멈춘다", async () => {
    await mount("member-empty");
    send({ type: "boot", config: { memberToken: "signed-m9", memberRefresh: true } });
    await settle();
    openSocket();
    server.deadTokens.add("tok-m9");
    server.expiredMembers.add("signed-m9");
    server.listExpired = 1;
    send({ type: "open" }); // 목록 조회의 401 도 같은 길이다
    await settle();
    const ask = toHost.filter((m) => m.type === "member-token-request").at(-1);
    assert.ok(ask, "호스트에 묻지 않았다");
    send({ type: "member-token", id: ask.id, memberToken: "" });
    await settle();
    assert.equal(el("sessionNoticeText").textContent, "로그인이 만료되었습니다. 페이지를 새로고침해 주세요");
  });

  // ── 감사 21: 회원 회복과 호스트 메시지의 방어(생존 변이 J2·J3·J8·J9·J18·J20) ──────────────

  const expiredNotice = () => el("sessionNoticeText").textContent === "로그인이 만료되었습니다. 페이지를 새로고침해 주세요";
  const asks = () => toHost.filter((m) => m.type === "member-token-request");

  test("부모 창이 아닌 창이 보낸 메시지는 버린다 — 다른 창의 identify 로 회원이 바뀌지 않고 호스트 출처도 덮이지 않는다", async () => {
    await mount("foreign-source");
    send({ type: "boot", config: { memberToken: "signed-m20" } });
    await settle();
    assert.equal(store.get("gcs.customerToken"), "tok-m20");
    const attacker = { postMessage() {} }; // 위젯을 끼워 넣은 다른 창(같은 페이지의 다른 iframe·팝업)
    server.sessionCalls = [];
    sendFrom(attacker, "https://evil.test", { type: "identify", memberToken: "signed-evil" });
    sendFrom(attacker, "https://evil.test", { type: "logout" });
    await settle();
    assert.deepEqual(kinds(), [], "다른 창의 identify·logout 을 따랐다");
    assert.equal(store.get("gcs.customerToken"), "tok-m20", "다른 창이 위젯의 회원을 바꿨다");
    send({ type: "open" }); // 위젯이 호스트에 알린다(size)
    await settle();
    assert.equal(toHostOrigins.at(-1), "https://shop.test", "다른 창의 출처로 호스트 메시지를 보낸다");
  });

  test("묻지 않은 id 의 member-token 답은 받지 않는다 — 물은 id 의 답만 그 회원 토큰으로 다시 연다", async () => {
    await mount("member-ask-id");
    send({ type: "boot", config: { memberToken: "signed-m21", memberRefresh: true } });
    await settle();
    openSocket();
    server.expiredMembers.add("signed-m21");
    server.sessionCalls = [];
    server.listExpired = 1;
    send({ type: "open" });
    await settle();
    const ask = asks().at(-1);
    assert.ok(ask, "호스트에 묻지 않았다");
    send({ type: "member-token", id: ask.id + 1, memberToken: "signed-m21~guess" });
    await settle();
    assert.deepEqual(kinds(), ["member:signed-m21"], "묻지 않은 id 의 답으로 세션을 열었다");
    send({ type: "member-token", id: ask.id, memberToken: "signed-m21~2" });
    await settle();
    assert.deepEqual(kinds(), ["member:signed-m21", "member:signed-m21~2"]);
    assert.equal(store.get("gcs.customerToken"), "tok-m21~2");
  });

  test("호스트의 getMemberToken 은 10초까지 기다리고 넘으면 새로고침 안내로 멈춘다", async () => {
    await mount("member-wait");
    send({ type: "boot", config: { memberToken: "signed-m22", memberRefresh: true } });
    await settle();
    openSocket();
    mock.timers.reset();
    mock.timers.enable({ apis: ["setInterval", "setTimeout"] });
    try {
      server.expiredMembers.add("signed-m22");
      server.listExpired = 1;
      send({ type: "open" });
      await settle();
      assert.ok(asks().length > 0, "호스트에 묻지 않았다");
      mock.timers.tick(9999);
      await settle();
      assert.equal(expiredNotice(), false, "10초가 되기 전에 멈췄다");
      mock.timers.tick(1);
      await settle();
      assert.equal(expiredNotice(), true, "10초가 지나도 멈추지 않는다");
    } finally {
      mock.timers.reset();
      mock.timers.enable({ apis: ["setInterval"] });
    }
  });

  test("일시 실패(네트워크·5xx·429·JSON 이 아닌 앞단의 403)는 멈추지 않고 호스트에 묻지도 않는다 — 우리 서버의 JSON 403 은 거절이라 묻는다", async () => {
    await mount("member-transient");
    send({ type: "boot", config: { memberToken: "signed-m23", memberRefresh: true } });
    await settle();
    openSocket();
    const html403 = () => new Response("<html>Attention Required! | Cloudflare</html>", { status: 403, headers: { "Content-Type": "text/html" } });
    const replies = {
      "네트워크": () => { throw new TypeError("Failed to fetch"); },
      "503": () => json(503, { error: { code: "db_down", message: "잠시 뒤" } }),
      "429": () => new Response(JSON.stringify({ error: { code: "too_many", message: "잠시 뒤" } }), { status: 429, headers: { "Content-Type": "application/json", "Retry-After": "1" } }),
      "앞단 403 HTML": html403,
      "앞단 408 text": () => new Response("Request Timeout", { status: 408, headers: { "Content-Type": "text/plain" } }),
    };
    try {
      for (const [name, reply] of Object.entries(replies)) {
        server.memberReply = reply;
        server.listExpired = 1;
        send({ type: "open" });
        await settle();
        assert.equal(expiredNotice(), false, `${name}: 일시 실패로 멈췄다`);
        assert.deepEqual(asks(), [], `${name}: 일시 실패에 호스트의 새 토큰을 물었다`);
      }
      // 우리 서버가 JSON 으로 거절했다 — 호스트에 묻는다
      server.memberReply = () => json(403, { error: { code: "forbidden", message: "이 회원은 쓸 수 없다" } });
      server.listExpired = 1;
      send({ type: "open" });
      await settle();
      assert.equal(asks().length, 1, "우리 서버의 JSON 403 을 거절로 보지 않았다");
    } finally {
      server.memberReply = null;
      server.listExpired = 0;
      // 물은 것에 답해 둔다 — 답이 없으면 10초 시한 타이머가 테스트 프로세스를 붙잡는다
      for (const a of asks()) send({ type: "member-token", id: a.id, memberToken: "" });
      await settle();
    }
  });

  test("멈춘 뒤에 늦게 온 401 은 회원 회복을 다시 시작하지 않는다 — 만료 토큰으로 다시 열지 않고 안내도 한 번이다", async () => {
    await mount("member-late-401");
    send({ type: "boot", config: { memberToken: "signed-m24" } });
    await settle();
    openSocket();
    // 멈추기 전에 나간 목록 요청이 늦게 401 로 돌아온다
    let releaseList;
    server.holdList = new Promise((r) => (releaseList = r));
    server.listExpired = 1;
    send({ type: "open" });
    await settle();
    server.deadTokens.add("tok-m24");
    server.expiredMembers.add("signed-m24");
    socket().close();
    await waitBackoff();
    await settle();
    assert.equal(expiredNotice(), true, "멈추지 않았다");
    const calls = server.sessionCalls.length;
    const toasts = toastEls().filter((t) => /로그인이 만료되었습니다/.test(t.innerHTML)).length;
    server.holdList = null;
    releaseList();
    await settle();
    assert.equal(server.sessionCalls.length, calls, "멈춘 뒤 늦게 온 401 로 만료된 회원 토큰을 다시 보냈다");
    assert.equal(toastEls().filter((t) => /로그인이 만료되었습니다/.test(t.innerHTML)).length, toasts, "멈춘 뒤 같은 안내를 또 띄웠다");
  });
});
