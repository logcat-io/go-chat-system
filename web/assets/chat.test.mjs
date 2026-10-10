// 클라이언트 유실 방지 정책 테스트.
//
// 단언 대상은 바깥에서 관찰할 수 있는 것만 쓴다 — localStorage 에 남은 것, 화면에 올라간 것,
// 소켓으로 실제 나간 프레임. 내부 필드를 단언하면 구현을 바꿀 때마다 깨지고,
// 정작 "탭을 닫아도 살아남는다" 같은 정책은 검증하지 않는다.
import { test, describe, beforeEach, after, mock } from "node:test";
import assert from "node:assert/strict";

// 살아 있는 주기 타이머를 센다. stop() 이 타이머를 놓치면(timers 에 안 넣으면) 그 타이머가 프로세스를 붙잡아 테스트가 끝나지 않는다(감사 14 A3).
// 테스트는 남은 타이머를 단언해 곧바로 실패하고, 끝에서 남은 것을 모두 지워 멈추지 않게 한다. 흉내 시계를 켠 테스트에는 걸리지 않는다.
const liveIntervals = new Set();
const realSetInterval = globalThis.setInterval;
const realClearInterval = globalThis.clearInterval;
globalThis.setInterval = (...a) => {
  const t = realSetInterval(...a);
  liveIntervals.add(t);
  return t;
};
globalThis.clearInterval = (t) => {
  liveIntervals.delete(t);
  return realClearInterval(t);
};
after(() => {
  for (const t of liveIntervals) realClearInterval(t);
});

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const STORE = "gcs.v1.";
// 전송 대기 글의 이름 공간(DEF-165). 커서(gcs.v1.<주인>.)와 다르다. qOf 는 커서 접두사에 맞는 글 접두사다.
const QUEUE = "gcs.q1.";
const qOf = (prefix) => QUEUE + prefix.slice(STORE.length);

// ── 브라우저 흉내 ─────────────────────────────────────────

const sentFrames = [];

function installBrowser() {
  const store = new Map();
  globalThis.localStorage = {
    get length() {
      return store.size;
    },
    key: (i) => [...store.keys()][i] ?? null,
    getItem: (k) => (store.has(k) ? store.get(k) : null),
    setItem: (k, v) => store.set(k, String(v)),
    removeItem: (k) => store.delete(k),
  };
  globalThis.document = { visibilityState: "visible" };
  globalThis.addEventListener = () => {};
  globalThis.location = { protocol: "http:", host: "localhost:8080" };

  // 보낸 프레임을 기록하는 소켓. 열려 있는 상태로 시작해 "무엇을 보냈나" 를 볼 수 있게 한다.
  globalThis.WebSocket = class {
    static CONNECTING = 0;
    static OPEN = 1;
    static CLOSED = 3;
    constructor() {
      this.readyState = WebSocket.OPEN;
      queueMicrotask(() => this.onopen?.());
    }
    send(raw) {
      sentFrames.push(JSON.parse(raw));
    }
    close() {
      this.readyState = WebSocket.CLOSED;
      this.onclose?.();
    }
  };
  return store;
}

const store = installBrowser();
const { ChatSession, MAX_BODY_RUNES, isPermanentReject, isTransientFailure, STALL_AFTER, COMPLETE_BUSY_RETRIES, COMPLETE_BUSY_INTERVAL, restBackoff, REST_BACKOFF_MAX, setTicketSource } = await import("./chat.js");
// 소켓을 곧바로 열어 "무엇을 보냈나" 를 보려고 티켓을 동기 값으로 준다. 티켓을 받는 길 자체(비동기·실패·401)는 "WebSocket 티켓" 묶음이 본다.
setTicketSource(() => "tkt");

// ── 관찰 도구 ─────────────────────────────────────────────

/** 저장소에 남은 전송 대기 큐. 탭을 닫았다 열어도 이게 그대로 있어야 한다.
 * 글마다 키가 따로다(`gcs.q1.<주인>.<대화>|<client_msg_id>`, DEF-158·165). 옛 형식(커서 키의 pending 배열)에 남은 글도 센다. 큐의 순서(번호 n)다. */
const storedPendingAt = (prefix, convId) => {
  const out = [];
  for (const [k, v] of store) if (k.startsWith(`${qOf(prefix)}${convId}|`)) out.push(JSON.parse(v));
  for (const p of JSON.parse(localStorage.getItem(prefix + convId) ?? "{}").pending || []) {
    if (!out.some((q) => q.client_msg_id === p.client_msg_id)) out.push(p);
  }
  return out.sort((a, b) => (a.n > 0) - (b.n > 0) || (a.n || 0) - (b.n || 0) || (a.at || 0) - (b.at || 0) || (a.ord || 0) - (b.ord || 0));
};
const storedPending = (convId) => storedPendingAt(STORE, convId);

/** 저장소에 남은 커서. 재접속할 때 어디부터 달라고 할지를 정한다. */
const storedCursor = (convId) => JSON.parse(localStorage.getItem(STORE + convId) ?? '{"lastSeq":0}').lastSeq;

const framesOfType = (type) => sentFrames.filter((f) => f.type === type);

function respond(status, body, headers = {}) {
  return { ok: status < 400, status, text: async () => JSON.stringify(body), headers: { get: (k) => headers[k] ?? null } };
}

function msg(convId, seq, body = "m") {
  return {
    conversation_id: convId,
    seq,
    id: `id-${seq}`,
    client_msg_id: `c-${seq}`,
    sender_kind: "AGENT",
    sender_name: "상담원",
    body,
    created_at: new Date().toISOString(),
  };
}

/** 세션과, 그 세션이 바깥으로 내보낸 것들을 같이 돌려준다. */
function newSession(handlers = {}) {
  const shown = { messages: [], errors: [], pending: [], conversations: [] };
  const s = new ChatSession({
    token: "tok",
    handlers: {
      onMessage: (m) => shown.messages.push(m),
      onError: (e) => shown.errors.push(e),
      onPending: (list) => shown.pending.push(list.map((p) => p.body)),
      onConversation: (c) => shown.conversations.push(c),
      ...handlers,
    },
  });
  // 소켓을 열어 둔다. 실제로 나간 프레임을 봐야 "무엇을 보냈나" 를 단언할 수 있다.
  s.connect();
  return { s, shown };
}

// seedPending 은 저장소의 그 대화 큐를 옛 형식(커서 키의 pending 배열)으로 바꿔 둔다 — 다른 탭이나 옛 코드가 남긴 큐다.
// 그 대화에 이미 있던 글 키는 지운다(같은 client_msg_id 를 다시 쓰는 테스트가 있다).
function seedPending(convId, bodies) {
  for (const k of [...store.keys()]) if (k.startsWith(`${QUEUE}${convId}|`)) store.delete(k);
  localStorage.setItem(
    STORE + convId,
    JSON.stringify({
      lastSeq: 0,
      pending: bodies.map((b, i) => ({ client_msg_id: `p${i}`, body: b, at: Date.now(), tries: 0 })),
    }),
  );
}

beforeEach(() => {
  for (const k of [...store.keys()]) store.delete(k);
  sentFrames.length = 0;
  globalThis.fetch = async () => respond(200, { messages: [] });
});

// ── 정책 ──────────────────────────────────────────────────

describe("전송 대기 큐", () => {
  test("보낸 메시지는 저장소에 남아 탭을 닫아도 살아남는다", () => {
    const { s, shown } = newSession();
    s.open("A");
    s.send("아직 못 보낸 문의");

    assert.deepEqual(storedPending("A").map((p) => p.body), ["아직 못 보낸 문의"]);
    assert.deepEqual(shown.pending.at(-1), ["아직 못 보낸 문의"], "보내는 중 표시가 화면에 올라가야 한다");
  });

  test("서버가 저장을 확인하면 큐에서 빠진다", () => {
    const { s, shown } = newSession();
    s.open("A");
    s.send("안녕하세요");
    const sent = framesOfType("send").at(-1);

    s.handleFrame({ type: "ack", conversation_id: "A", client_msg_id: sent.client_msg_id, seq: 1 });

    assert.deepEqual(storedPending("A"), []);
    assert.deepEqual(shown.pending.at(-1), []);
  });

  test("영구 거절만 큐에서 지우고 일시 오류는 남긴다", () => {
    const { s, shown } = newSession();
    s.open("A");
    s.send("거절될 것");
    s.handleFrame({
      type: "error", code: "invalid", message: "본문이 비어 있습니다",
      client_msg_id: framesOfType("send").at(-1).client_msg_id,
    });
    assert.deepEqual(storedPending("A"), [], "다시 보내도 결과가 같은 것은 지운다");

    s.send("다시 시도할 것");
    s.handleFrame({
      type: "error", code: "internal", message: "처리 실패",
      client_msg_id: framesOfType("send").at(-1).client_msg_id,
    });
    assert.equal(storedPending("A").length, 1, "일시 오류를 지우면 그대로 유실이다");
    // 알리는 것은 포기한 쪽뿐이다. 큐에 남아 다시 나갈 글에 실패를 알리면 사람이 다시 써서 두 번 저장된다(DEF-01, 정책 559).
    assert.deepEqual(shown.errors, ["본문이 비어 있습니다"], "영구 거절만 알린다");
  });

  test("다시 보내도 결과가 같은 거절만 영구 거절이다", () => {
    assert.equal(isPermanentReject("invalid"), true);
    assert.equal(isPermanentReject("not_found"), true);
    assert.equal(isPermanentReject("conflict"), false, "충돌은 상황이 바뀌면 통한다");
    assert.equal(isPermanentReject("internal"), false);
    assert.equal(isPermanentReject("forbidden"), false, "세션이 되살아나면 통할 수 있다");
    // 고객이 닫아 둔 상담(D48)은 상담원이 다시 열기 전까지 같은 답이다. 지운 글은 거절 알림이 화면에 돌려준다.
    assert.equal(isPermanentReject("closed_by_customer"), true);
  });

  test("길이를 넘는 본문은 큐에 넣지 않고 바로 알린다", () => {
    const { s, shown } = newSession();
    s.open("A");
    s.send("가".repeat(MAX_BODY_RUNES + 1));

    assert.deepEqual(storedPending("A"), []);
    assert.equal(framesOfType("send").length, 0, "소켓으로 내보내지도 않는다");
    assert.equal(shown.errors.length, 1);
  });

  test("대화를 바꿔도 옛 대화에 남은 큐를 계속 민다", async () => {
    seedPending("A", ["오프라인에 적어 둔 문의"]);
    const posted = [];
    globalThis.fetch = async (path, opts) => {
      if (opts?.method !== "POST") return respond(200, { messages: [] });
      posted.push(path);
      return respond(201, { message: msg("A", 1), created: true });
    };

    const { s } = newSession();
    s.open("B");
    await s.flushViaRest();
    s.stop();

    assert.deepEqual(posted, ["/api/conversations/A/messages"], "옛 대화의 큐가 고아가 됐다");
    assert.deepEqual(storedPending("A"), []);
  });

  test("여러 대화의 큐가 서로 섞이지 않는다", async () => {
    seedPending("A", ["A 의 메시지"]);
    seedPending("B", ["B 의 메시지"]);
    const sent = [];
    globalThis.fetch = async (path, opts) => {
      if (opts?.method !== "POST") return respond(200, { messages: [] });
      sent.push([path.split("/")[3], JSON.parse(opts.body).body]);
      return respond(201, { message: msg("A", 1), created: true });
    };

    const { s } = newSession();
    s.open("C");
    await s.flushViaRest();
    s.stop();

    assert.equal(sent.length, 2);
    for (const [target, body] of sent) {
      assert.ok(body.startsWith(target), `${body} 가 ${target} 대화로 갔다`);
    }
  });

  // 상한에 세는 것은 서버가 명시적으로 거절한 것뿐이다(영구 거절이 아닌 4xx). 네트워크·5xx 는 아래 정책이 다룬다.
  test("재시도 상한을 넘기면 버리고 못 보냈다고 알린다", async () => {
    seedPending("A", ["끝내 못 보낼 메시지"]);
    globalThis.fetch = async (path, opts) =>
      opts?.method === "POST"
        ? respond(409, { error: { code: "conflict", message: "이미 있습니다" } })
        : respond(200, { messages: [] });

    const { s, shown } = newSession();
    s.open("A");
    s.ws.close(); // REST 대체 경로는 소켓이 끊겼을 때 도는 길이다
    for (let i = 0; i < 25; i++) await s.flushViaRest();
    s.stop();

    assert.deepEqual(storedPending("A"), [], "영원히 재시도하면 사용자가 모른 채 묻힌다");
    assert.ok(shown.errors.some((e) => e.includes("보내지 못했습니다")), "포기했으면 알려야 한다");
  });

  // 새로고침하면 화면이 기억하던 보낸 글은 없다. 큐에서 지운 그 글을 알림이 들고 와야 화면이 되돌릴 수 있다.
  test("영구 거절로 큐에서 지운 메시지는 거절 알림의 세 번째 인자로 본문·대화·첨부와 함께 넘기고 네 번째 인자로 그 거절의 trace id 를 넘긴다", async () => {
    const calls = [];
    const onError = (...args) => calls.push(args);

    // 1) 소켓으로 보낸 글이 영구 거절됐다
    const { s } = newSession({ onError });
    s.open("A");
    s.send("소켓으로 보낸 글", { internal: true, attachment: { id: "att-1", filename: "a.png" } });
    const id1 = framesOfType("send").at(-1).client_msg_id;
    const frameTrace = "0123456789abcdef0123456789abcdef";
    s.handleFrame({ type: "error", code: "invalid", conversation_id: "A", client_msg_id: id1, message: "첨부를 찾을 수 없다", trace_id: frameTrace });
    assert.deepEqual(calls.at(-1), ["첨부를 찾을 수 없다", id1, {
      client_msg_id: id1, conversation_id: "A", body: "소켓으로 보낸 글", internal: true, attachment: { id: "att-1", filename: "a.png" }, code: "invalid",
    }, frameTrace]);

    // 2) 일시 오류는 큐에 남는다. 지운 것이 없고 포기하지도 않았으니 알리지 않는다(DEF-01)
    s.send("다시 나갈 글");
    const id2 = framesOfType("send").at(-1).client_msg_id;
    const before = calls.length;
    s.handleFrame({ type: "error", code: "internal", conversation_id: "A", client_msg_id: id2, message: "잠시 뒤" });
    assert.equal(calls.length, before);
    assert.equal(storedPending("A").length, 1);
    s.stop();

    // 3) 새로고침 뒤: 저장소에 남은 글을 REST 로 다시 밀었는데 영구 거절됐다
    for (const k of [...store.keys()]) store.delete(k);
    localStorage.setItem(STORE + "B", JSON.stringify({ lastSeq: 0, pending: [
      { client_msg_id: "r1", body: "새로고침 전에 쓴 글", at: Date.now(), tries: 0, attachment: { id: "att-9" } },
    ] }));
    const restTrace = "fedcba9876543210fedcba9876543210";
    globalThis.fetch = async (path, opts) =>
      opts?.method === "POST"
        ? respond(400, { error: { code: "invalid", message: "첨부를 찾을 수 없다" } }, { "X-Trace-Id": restTrace })
        : respond(200, { messages: [] });
    const fresh = newSession({ onError });
    await fresh.s.flushViaRest();
    fresh.s.stop();
    assert.deepEqual(calls.at(-1), ["첨부를 찾을 수 없다", "r1", {
      client_msg_id: "r1", conversation_id: "B", body: "새로고침 전에 쓴 글", internal: false, attachment: { id: "att-9" }, code: "invalid",
    }, restTrace]);
    assert.deepEqual(storedPending("B"), [], "큐 동작은 그대로다 — 영구 거절은 지운다");

    // 4) 재시도 상한으로 버린 글도 같이 넘긴다(상한은 명시적 거절만 센다)
    seedPending("C", ["끝내 못 보낸 글"]);
    globalThis.fetch = async (path, opts) =>
      opts?.method === "POST" ? respond(403, { error: { code: "forbidden", message: "이 대화에 접근할 수 없습니다" } }) : respond(200, { messages: [] });
    const capped = newSession({ onError });
    capped.s.ws.close();
    for (let i = 0; i < 25; i++) await capped.s.flushViaRest();
    capped.s.stop();
    const last = calls.at(-1);
    assert.match(last[0], /보내지 못했습니다/);
    assert.deepEqual(last[2], { client_msg_id: "p0", conversation_id: "C", body: "끝내 못 보낸 글", internal: false, attachment: null, code: "forbidden" });
  });
});

// 네트워크로는 포기하지 않는다 (D5, 2026-10-05 logcat 결정). 끊긴 동안 세면 1분 남짓에 글을 돌려주고, 그 사이 서버가 저장했으면
// 고객이 다시 보낸 글이 중복이 된다. 큐에 남겨 두고 연결이 돌아오면 같은 client_msg_id 로 다시 보낸다 — 서버가 멱등이라 한 번만 남는다.
describe("오래 끊겨도 포기하지 않는다", () => {
  test("네트워크 실패와 5xx 는 재시도 상한에 세지 않는다 — 큐에 남아 연결되면 같은 client_msg_id 로 다시 나가 한 번만 저장된다", async () => {
    seedPending("A", ["3분 끊긴 동안 쓴 글"]);
    let mode = "offline";
    const saved = new Map(); // 서버: client_msg_id → 저장한 본문(멱등)
    globalThis.fetch = async (path, opts) => {
      if (opts?.method !== "POST") return respond(200, { messages: [] });
      if (mode === "offline") throw new TypeError("Failed to fetch");
      if (mode === "5xx") return respond(503, { error: { code: "db_down", message: "DB 에 접속할 수 없습니다" } });
      if (mode === "html502") return { ok: false, status: 502, text: async () => "<html>Bad Gateway</html>" };
      if (mode === "empty504") return { ok: false, status: 504, text: async () => "" }; // 앞단 프록시가 본문 없이 답했다
      const b = JSON.parse(opts.body);
      if (!saved.has(b.client_msg_id)) saved.set(b.client_msg_id, b.body);
      return respond(200, { message: msg("A", 1, b.body), created: true });
    };
    const { s, shown } = newSession();
    s.open("B");
    s.ws.close();
    // 3초 주기로 3분 = 60번. 종류마다 상한(20)을 넘길 만큼 돈다
    for (const m of ["offline", "5xx", "html502", "empty504"]) {
      mode = m;
      for (let i = 0; i < 25; i++) await s.flushViaRest();
    }
    assert.deepEqual(storedPending("A").map((p) => p.body), ["3분 끊긴 동안 쓴 글"], "끊긴 동안 큐에서 빠지면 그 글은 사라지거나 중복이 된다");
    assert.equal(shown.errors.length, 0, "네트워크 실패로 '보내지 못했다' 를 띄우면 고객이 다시 보낸다");
    const id = storedPending("A")[0].client_msg_id;
    mode = "online";
    await s.flushViaRest();
    s.stop();
    assert.deepEqual(storedPending("A"), []);
    assert.deepEqual([...saved.entries()], [[id, "3분 끊긴 동안 쓴 글"]], "같은 client_msg_id 로 한 번만 저장된다");
  });

  test("일시 오류 프레임은 재시도 상한에 세지 않고 명시적 거절 프레임만 센다", () => {
    const { s, shown } = newSession();
    s.open("A");
    s.send("일시 오류가 계속되는 글");
    const id = framesOfType("send").at(-1).client_msg_id;
    for (let i = 0; i < 30; i++) s.handleFrame({ type: "error", code: "internal", conversation_id: "A", client_msg_id: id, message: "처리하지 못했습니다. 잠시 후 다시 시도해 주세요" });
    assert.equal(storedPending("A").length, 1, "일시 오류 프레임을 세면 30번째 전에 큐에서 빠진다");
    assert.ok(shown.errors.every((e) => !e.includes("보내지 못했습니다")));
    for (let i = 0; i < 20; i++) s.handleFrame({ type: "error", code: "forbidden", conversation_id: "A", client_msg_id: id, message: "이 대화에 접근할 수 없습니다" });
    assert.deepEqual(storedPending("A"), [], "명시적 거절은 상한을 넘기면 포기한다");
    assert.ok(shown.errors.some((e) => e.includes("보내지 못했습니다")));
    s.stop();
  });

  // DEF-01(실사용 v0.0.2 U295): Postgres 재시작 순간 저장이 internal 로 실패하자 "잠시 뒤 다시 시도해라" 토스트가 떴고,
  // 사람이 안내대로 다시 써서 같은 글이 두 줄 저장됐다. 큐에 남은 글은 포기하지 않았다 — 알릴 것은 끊김 안내뿐이다.
  test("일시 오류 프레임을 받은 글은 토스트 없이 보내는 중으로 남고 다시 나가 정확히 한 번 저장된다 — 영구 거절은 지금처럼 알리고 글을 돌려준다", async () => {
    const calls = [];
    const stalls = [];
    const { s, shown } = newSession({ onError: (...a) => calls.push(a), onStalled: (x) => stalls.push(x) });
    s.open("A");
    const item = s.send("DB 가 잠깐 끊긴 순간 쓴 글");
    const id = item.client_msg_id;
    for (const code of ["internal", "canceled", "too_many"]) {
      s.handleFrame({ type: "error", code, conversation_id: "A", client_msg_id: id, message: "처리하지 못했습니다. 잠시 후 다시 시도해 주세요" });
    }
    assert.deepEqual(calls, [], "포기하지 않은 글에 실패 알림을 띄우면 사람이 다시 써서 두 번 저장된다");
    assert.deepEqual(storedPending("A").map((p) => p.client_msg_id), [id], "큐에 남는다");
    assert.deepEqual(shown.pending.at(-1), ["DB 가 잠깐 끊긴 순간 쓴 글"], "보내는 중으로 보인다");
    // 오래 ack 가 없으면 끊김 안내(onStalled)로만 보인다
    item.at = Date.now() - STALL_AFTER - 1000;
    saveStateOf("A", s);
    s.checkStall();
    assert.deepEqual(stalls.at(-1), { offline: false, count: 1 });

    // 다시 나간다 — 소켓이 다시 준비되면 같은 client_msg_id 로, 오래되면 REST 로. 서버는 client_msg_id 로 멱등이다
    const saved = new Map();
    globalThis.fetch = async (path, opts) => {
      if (opts?.method !== "POST") return respond(200, { messages: [] });
      const b = JSON.parse(opts.body);
      if (!saved.has(b.client_msg_id)) saved.set(b.client_msg_id, b.body);
      return respond(201, { message: msg("A", 1, b.body), created: saved.size === 1 });
    };
    s.handleFrame({ type: "ready", conversation_id: "A", last_seq: 0 });
    const resent = framesOfType("send").filter((f) => f.client_msg_id === id);
    assert.ok(resent.length >= 2, "같은 client_msg_id 로 다시 나가야 한다");
    await s.flushViaRest();
    assert.deepEqual([...saved.entries()], [[id, "DB 가 잠깐 끊긴 순간 쓴 글"]], "정확히 한 번 저장된다");
    assert.deepEqual(storedPending("A"), []);
    s.checkStall();
    assert.equal(stalls.at(-1), null, "보낸 뒤 안내가 걷힌다");
    assert.deepEqual(calls, [], "끝까지 실패 알림이 없다");

    // 영구 거절은 지금처럼 알리고 글을 돌려준다
    const bad = s.send("영구 거절될 글");
    s.handleFrame({ type: "error", code: "invalid", conversation_id: "A", client_msg_id: bad.client_msg_id, message: "첨부를 찾을 수 없다" });
    assert.equal(calls.length, 1);
    assert.equal(calls[0][0], "첨부를 찾을 수 없다");
    assert.equal(calls[0][2]?.body, "영구 거절될 글", "버린 글을 돌려준다");
    assert.deepEqual(storedPending("A"), []);
    s.stop();
  });

  test("글과 무관한 일시 오류 프레임은 토스트를 띄우지 않는다 — 구독이 붙지 않았으면 다시 구독하고 읽음은 다시 보낸다", () => {
    const calls = [];
    const { s } = newSession({ onError: (...a) => calls.push(a) });
    s.open("A");
    // 구독·읽음·타이핑의 실패. client_msg_id 가 없다
    s.handleFrame({ type: "error", code: "internal", conversation_id: "A", message: "처리하지 못했습니다. 잠시 후 다시 시도해 주세요" });
    s.handleFrame({ type: "error", code: "too_many", message: "요청이 너무 많다" });
    s.handleFrame({ type: "error", code: "canceled", message: "연결이 닫히는 중이다" });
    assert.deepEqual(calls, []);
    // 판정된 거절은 지금처럼 알린다
    s.handleFrame({ type: "error", code: "forbidden", conversation_id: "A", message: "이 대화에 접근할 수 없습니다" });
    assert.deepEqual(calls.map((c) => c[0]), ["이 대화에 접근할 수 없습니다"]);
    s.stop();
  });

  test("무엇을 일시 실패로 보나 — 응답 없음·우리 서버의 JSON 판정이 없는 응답·5xx·429·일시 오류 프레임만이다", () => {
    assert.equal(isTransientFailure(new TypeError("Failed to fetch")), true, "오프라인");
    assert.equal(isTransientFailure({ status: 503, code: "db_down" }), true);
    assert.equal(isTransientFailure({ status: 500, code: "internal" }), true);
    assert.equal(isTransientFailure({ status: 429, code: "too_many" }), true);
    assert.equal(isTransientFailure({ code: "internal" }), true, "일시 오류 프레임");
    for (const status of [403, 404, 408, 413]) assert.equal(isTransientFailure({ status, code: "" }), true, `JSON 이 아닌 ${status}(앞단의 답)`);
    assert.equal(isTransientFailure({ status: 409, code: "conflict" }), false);
    assert.equal(isTransientFailure({ status: 403, code: "forbidden" }), false);
    assert.equal(isTransientFailure({ status: 401, code: "unauthorized" }), false);
    assert.equal(isTransientFailure({ code: "conflict" }), false, "명시적 거절 프레임");
  });

  // 감사 21: api() 가 JSON 이 아닌 오류 본문에도 상태를 남기면서(감사 20) 상태 코드를 code 로 채웠다. 그래서 앞단(WAF·프록시·CDN)의
  // 403·413 HTML, 정적 서버의 404 text 가 "서버가 거절했다" 로 세여 20번 만에 글을 포기했다 — 우리 서버는 그 글을 본 적도 없다(D5 위반).
  test("앞단이 JSON 없이 답한 403·404·408·413 은 일시 실패라 글이 큐에 남고 상한에 세지 않는다 — 우리 서버의 JSON 403 은 거절로 세어 20번에 포기한다", async () => {
    const html = (status, text) => ({ ok: false, status, headers: new Headers({ "Content-Type": "text/html" }), text: async () => text });
    const replies = [
      html(403, "<html>Attention Required! | Cloudflare</html>"),
      html(413, "<html>413 Request Entity Too Large</html>"),
      { ok: false, status: 404, headers: new Headers({ "Content-Type": "text/plain" }), text: async () => "404 page not found\n" },
      html(408, "<html>408 Request Timeout</html>"),
    ];
    for (const reply of replies) {
      seedPending("F", ["앞단에 막힌 글"]);
      globalThis.fetch = async (_p, o) => (o?.method === "POST" ? reply : respond(200, { messages: [] }));
      const errors = [];
      const s = new ChatSession({ token: "tok", handlers: { onError: (...e) => errors.push(e) } });
      for (let i = 0; i < 25; i++) await s.pushPending("F");
      s.stop();
      assert.deepEqual(storedPending("F").map((p) => p.body), ["앞단에 막힌 글"], `${reply.status}: 서버가 본 적 없는 글을 버렸다`);
      assert.deepEqual(errors, [], `${reply.status}: 포기 알림을 띄웠다`);
    }
    seedPending("F", ["우리 서버가 거절한 글"]);
    globalThis.fetch = async (_p, o) => (o?.method === "POST" ? respond(403, { error: { code: "forbidden", message: "이 대화에 쓸 수 없습니다" } }) : respond(200, { messages: [] }));
    const errors = [];
    const s = new ChatSession({ token: "tok", handlers: { onError: (...e) => errors.push(e) } });
    for (let i = 0; i < 19; i++) await s.pushPending("F");
    assert.equal(storedPending("F").length, 1, "상한(20) 전에는 남는다");
    await s.pushPending("F");
    s.stop();
    assert.deepEqual(storedPending("F"), [], "명시적 거절이 상한을 넘으면 큐에서 뺀다");
    assert.equal(errors.length, 1, "포기를 한 번 알린다");
    assert.equal(errors[0][2]?.body, "우리 서버가 거절한 글", "버린 글을 돌려준다");
  });

  test("포기하는 글이 이미 받은 메시지에 같은 client_msg_id 로 있으면 돌려주지 않는다 — 서버가 저장했다", () => {
    const calls = [];
    const { s } = newSession({ onError: (...a) => calls.push(a) });
    s.open("A");
    s.send("프레임이 나간 직후 끊긴 글");
    const id = framesOfType("send").at(-1).client_msg_id;
    // ack 는 못 받았지만 그 글이 메시지로 돌아왔다(재연결 뒤 after_seq 로 받은 것)
    s.ingest({ ...msg("A", 1, "프레임이 나간 직후 끊긴 글"), client_msg_id: id, sender_kind: "CUSTOMER" });
    for (let i = 0; i < 20; i++) s.handleFrame({ type: "error", code: "conflict", conversation_id: "A", client_msg_id: id, message: "이미 있습니다" });
    assert.deepEqual(storedPending("A"), [], "큐에서는 지운다");
    assert.ok(calls.every((c) => c[2] === undefined), "저장된 글을 돌려주면 고객이 다시 보내 두 번 남는다");
    s.stop();
  });

  test("보낸 글이 오래 ack 를 못 받으면 연결 상태와 함께 기다린다고 알리고 ack 를 받으면 거둔다", () => {
    const seen = [];
    const { s } = newSession({ onStalled: (v) => seen.push(v) });
    s.open("A");
    s.send("기다리는 글");
    s.checkLiveness();
    assert.equal(seen.filter(Boolean).length, 0, "막 보낸 글에 끊김 안내를 띄우지 않는다");
    for (const p of s.state.pending) p.at = Date.now() - STALL_AFTER - 1;
    s.ws.close();
    s.checkLiveness();
    assert.deepEqual(seen.at(-1), { offline: true, count: 1 }, "끊겨 있으면 다시 연결되면 보낸다고 알린다");
    assert.equal(storedPending("A").length, 1, "안내만 띄우고 큐는 그대로다");
    s.handleFrame({ type: "ack", conversation_id: "A", client_msg_id: s.state.pending[0].client_msg_id, seq: 1 });
    assert.equal(seen.at(-1), null, "보내지면 안내를 거둔다");
    s.stop();
  });
});

// 감사 22: 앞단이 글을 계속 막으면(JSON 아닌 403) 그 글은 D5 대로 큐 맨 앞에서 기다린다. 그동안 화면이 "연결이 끊겨 있습니다" 라고 하면
// 사실과 다르다 — 답은 오고 있다. 소켓이 닫혀 있어도 다시 보내기가 HTTP 답을 받으면 offline 이 아니다(화면: 전송이 지연되고 있다).
describe("끊김과 전송 지연을 가른다", () => {
  test("소켓이 닫혀 있어도 다시 보내기가 앞단의 403 HTML 같은 답을 받으면 offline 이 아니고 답이 없으면 offline 이다", async () => {
    const seen = [];
    const { s } = newSession({ onStalled: (v) => seen.push(v) });
    s.open("A");
    s.send("앞단에 막히는 글");
    for (const p of s.state.pending) p.at = Date.now() - STALL_AFTER - 1;
    s.ws.close();
    globalThis.fetch = async (_p, o) => (o?.method === "POST"
      ? { ok: false, status: 403, headers: new Headers({ "Content-Type": "text/html" }), text: async () => "<html>Attention Required! | Cloudflare</html>" }
      : respond(200, { messages: [] }));
    await s.pushPending("A");
    assert.deepEqual(seen.at(-1), { offline: false, count: 1 }, "답이 오는데 끊겼다고 알린다");
    assert.equal(storedPending("A").length, 1, "D5: 앞단의 거절은 일시 실패라 글이 큐에 남는다");
    globalThis.fetch = async () => { throw new TypeError("Failed to fetch"); };
    await s.pushPending("A");
    assert.deepEqual(seen.at(-1), { offline: true, count: 1 }, "답이 없으면 끊겼다고 알린다");
    s.stop();
  });

  // 감사 23 N18: 막혔던 다시 보내기가 성공하면 "답이 오고 있다" 는 기억(restReplyAt)을 지운다. 지우지 않으면 그 뒤 소켓이 닫힌 채
  // 새 글이 늦어질 때 끊긴 것을 "전송 지연" 으로 잘못 알린다 — 성공 뒤로 답을 받은 적이 없다.
  test("막혔던 다시 보내기가 성공한 뒤 소켓이 닫힌 채 새 글이 늦어지면 다시 offline 으로 알린다", async () => {
    const seen = [];
    const { s } = newSession({ onStalled: (v) => seen.push(v) });
    s.open("A");
    s.send("앞단에 한 번 막히는 글");
    for (const p of s.state.pending) p.at = Date.now() - STALL_AFTER - 1;
    s.ws.close();
    globalThis.fetch = async (_p, o) => (o?.method === "POST"
      ? { ok: false, status: 403, headers: new Headers({ "Content-Type": "text/html" }), text: async () => "<html>blocked</html>" }
      : respond(200, { messages: [] }));
    await s.pushPending("A");
    assert.deepEqual(seen.at(-1), { offline: false, count: 1 }, "답이 오는 동안은 전송 지연이다");
    globalThis.fetch = async (_p, o) => (o?.method === "POST"
      ? respond(200, { message: { ...msg("A", 1), client_msg_id: JSON.parse(o.body).client_msg_id } })
      : respond(200, { messages: [] }));
    await s.pushPending("A");
    assert.equal(storedPending("A").length, 0, "막혔던 글이 보내졌다");
    assert.equal(seen.at(-1), null, "기다리는 글이 없으면 안내를 거둔다");
    s.send("소켓이 닫힌 채 늦어지는 새 글");
    for (const p of s.state.pending) p.at = Date.now() - STALL_AFTER - 1;
    s.checkStall();
    assert.deepEqual(seen.at(-1), { offline: true, count: 1 }, "성공 뒤로는 답을 받은 적이 없다 — 끊겼다고 알려야 한다");
    s.stop();
  });
});

// 감사 22: 업로드 시작 요청이 앞단의 413 HTML 을 받으면 JSON.parse 원문("Unexpected token '<'")이 고객 알림에 떴다.
describe("첨부 요청의 앞단 실패", () => {
  test("업로드 시작이 앞단의 HTML 로 실패하면 원문 대신 상태별 문구로 알리고 상태와 문의 번호를 지키며 우리 서버의 JSON 거절은 서버 문구 그대로다", async () => {
    const file = new File(["hello"], "a.txt", { type: "text/plain" });
    const cases = [
      [413, "<html><head><title>413 Request Entity Too Large</title></head></html>", /파일이 너무 커서 올리지 못했습니다/],
      [403, "<html>Attention Required! | Cloudflare</html>", /파일을 올리지 못했습니다\. 잠시 후 다시 올려 주세요/],
      [429, "Too Many Requests", /요청이 많아 파일을 올리지 못했습니다/],
    ];
    for (const [status, body, want] of cases) {
      globalThis.fetch = async () => ({ ok: false, status, headers: { get: (k) => (k === "X-Trace-Id" ? "abcdef0123456789abcdef0123456789" : null) }, text: async () => body });
      const s = new ChatSession({ token: "tok", handlers: {} });
      const err = await s.uploadAttachment("A", file).then(() => null, (e) => e);
      assert.ok(err, `${status}: 실패가 아니다`);
      assert.match(err.message, want, `${status}: ${err.message}`);
      assert.doesNotMatch(err.message, /Unexpected|JSON|<html/, `${status}: 원문이 샌다`);
      assert.equal(err.status, status);
      assert.equal(err.traceId, "abcdef0123456789abcdef0123456789");
      s.stop();
    }
    globalThis.fetch = async () => respond(413, { error: { code: "too_large", message: "파일은 100MB 까지 올릴 수 있습니다" } });
    const s = new ChatSession({ token: "tok", handlers: {} });
    const err = await s.uploadAttachment("A", file).then(() => null, (e) => e);
    assert.equal(err?.code, "too_large");
    assert.equal(err?.message, "파일은 100MB 까지 올릴 수 있습니다");
    s.stop();
  });
});

describe("수신 커서", () => {
  test("받은 메시지만큼 커서가 올라가고 저장된다", () => {
    const { s, shown } = newSession();
    s.open("A");
    s.ingest(msg("A", 1));
    s.ingest(msg("A", 2));

    assert.deepEqual(shown.messages.map((m) => m.seq), [1, 2]);
    assert.equal(storedCursor("A"), 2, "커서가 저장돼야 재접속에서 이어진다");
  });

  test("재접속하면 가진 마지막 번호부터 달라고 한다", () => {
    const { s } = newSession();
    s.open("A");
    s.ingest(msg("A", 1));
    s.ingest(msg("A", 2));

    sentFrames.length = 0;
    s.subscribe();

    assert.deepEqual(framesOfType("subscribe").at(-1), {
      type: "subscribe", conversation_id: "A", after_seq: 2,
    });
  });

  test("이미 본 번호는 화면에 두 번 올리지 않는다", () => {
    const { s, shown } = newSession();
    s.open("A");
    s.ingest(msg("A", 1));
    s.ingest(msg("A", 1));
    s.ingest(msg("A", 2));

    assert.deepEqual(shown.messages.map((m) => m.seq), [1, 2]);
  });

  test("다른 대화의 메시지는 받지 않는다", () => {
    const { s, shown } = newSession();
    s.open("A");
    s.ingest(msg("B", 1));

    assert.equal(shown.messages.length, 0);
    assert.equal(storedCursor("A"), 0);
  });

  test("서버가 가진 것이 더 적으면 서버에 맞춘다", () => {
    const { s } = newSession();
    localStorage.setItem(STORE + "A", JSON.stringify({ lastSeq: 99, pending: [] }));
    s.open("A");
    s.handleFrame({ type: "ready", conversation_id: "A", last_seq: 3, reads: [] });

    assert.equal(storedCursor("A"), 3, "내 기록이 서버보다 앞설 수는 없다");
  });
});

describe("갭 복구", () => {
  test("빈 번호가 보이면 그 사이를 채워 올린다", async () => {
    globalThis.fetch = async () => respond(200, { messages: [msg("A", 1), msg("A", 2), msg("A", 3)] });
    const { s, shown } = newSession();
    s.open("A");
    s.ingest(msg("A", 3));
    await sleep(60);
    s.stop();

    assert.deepEqual(shown.messages.map((m) => m.seq), [1, 2, 3]);
    assert.equal(storedCursor("A"), 3);
  });

  test("복구가 실패해도 다른 타이머가 계속 돈다", async () => {
    let calls = 0;
    globalThis.fetch = async () => {
      calls += 1;
      return respond(500, { error: { code: "internal", message: "서버 오류" } });
    };
    const { s } = newSession();
    s.open("A");

    let timerFired = false;
    setTimeout(() => {
      timerFired = true;
    }, 30);

    s.ingest(msg("A", 5));
    await sleep(120);
    s.stop();

    assert.ok(calls <= 2, `재시도가 ${calls}회 — 백오프 없이 재귀하면 수천 번이 된다`);
    assert.ok(timerFired, "매크로태스크가 굶으면 재접속·재전송 타이머까지 같이 죽는다");
  });

  test("복구 도중 대화를 바꾸면 새 대화의 커서를 덮지 않는다", async () => {
    globalThis.fetch = async () => {
      await sleep(40);
      return respond(200, { messages: [msg("A", 1), msg("A", 12)] });
    };
    const { s, shown } = newSession();
    s.open("A");
    s.ingest(msg("A", 9));
    await sleep(5);
    s.open("B");
    await sleep(120);
    s.stop();

    assert.equal(storedCursor("B"), 0, "A 의 응답이 B 의 커서를 밀면 B 의 과거가 영영 가려진다");
    assert.equal(shown.messages.length, 0, "A 의 메시지가 B 화면에 그려졌다");
  });
});

describe("첨부와 내부 메모", () => {
  test("첨부만 있어도 보낼 수 있다", () => {
    const { s } = newSession();
    s.open("A");
    s.send("", { attachment: { id: "att-1", filename: "a.png", byte_size: 10 } });

    assert.equal(framesOfType("send").at(-1).attachment_id, "att-1");
    assert.equal(storedPending("A").length, 1);
  });

  test("본문도 첨부도 없으면 아무것도 나가지 않는다", () => {
    const { s } = newSession();
    s.open("A");
    s.send("   ");

    assert.equal(framesOfType("send").length, 0);
    assert.deepEqual(storedPending("A"), []);
  });

  test("재전송은 올려 둔 첨부를 다시 올리지 않는다", () => {
    const { s } = newSession();
    s.open("A");
    s.send("사진입니다", { attachment: { id: "att-9" } });

    sentFrames.length = 0;
    s.flush();

    assert.equal(framesOfType("send").at(-1).attachment_id, "att-9");
  });

  test("내부 메모 표시가 저장소에 남아 재전송까지 간다", () => {
    const { s } = newSession();
    s.open("A");
    s.send("팀 메모", { internal: true });

    assert.equal(storedPending("A")[0].internal, true, "탭을 닫았다 열어도 내부 메모여야 한다");
    assert.equal(framesOfType("send").at(-1).internal, true);
  });
});

describe("첨부 완료", () => {
  /** 주소 발급과 저장소 올리기는 성공하고, 완료는 busy 를 busyTimes 번 답한 뒤 201 이다. */
  function uploadServer(busyTimes) {
    const completes = [];
    globalThis.fetch = async (path, _opts) => {
      if (path === "/api/conversations/A/attachments") {
        return respond(201, { attachment: { id: "att-1" }, upload: { url: "/files/upload", fields: { token: "t" } } });
      }
      if (path === "/files/upload") return respond(204, null);
      if (path === "/api/attachments/att-1/complete") {
        completes.push(Date.now());
        if (completes.length <= busyTimes) {
          return respond(409, { error: { code: "busy", message: "이 첨부를 확인하는 중입니다. 잠시 후 다시 시도해 주세요" } });
        }
        return respond(201, { attachment: { id: "att-1", filename: "a.txt" } });
      }
      return respond(200, { messages: [] });
    };
    return completes;
  }
  const file = () => new File(["AAAAA"], "a.txt", { type: "text/plain" });

  test("완료가 busy 면 간격을 두고 다시 불러 확인된 첨부를 받는다", async () => {
    const completes = uploadServer(2);
    const { s } = newSession();
    const att = await s.uploadAttachment("A", file());
    s.stop();

    assert.equal(att.id, "att-1");
    assert.equal(completes.length, 3);
    for (let i = 1; i < completes.length; i++) {
      // 상수를 그대로 가져와 비교하면 상수를 바꿔도 테스트가 따라간다. 정한 값(400ms)을 숫자로 적는다.
      assert.ok(completes[i] - completes[i - 1] >= 400 - 20, "간격 없이 몰아 부르면 서버에 부담만 준다");
    }
  });

  test("완료가 5번 다시 불러도 busy 면 오류로 알린다", async () => {
    const completes = uploadServer(Infinity);
    const { s } = newSession();
    await assert.rejects(s.uploadAttachment("A", file()), (err) => err.code === "busy");
    s.stop();

    assert.equal(completes.length, 1 + 5);
  });

  test("busy 재시도는 400ms 간격으로 5번이다", () => {
    // 정한 값을 숫자로 적는다. 상수를 가져와 비교하면 상수를 바꿔도 테스트가 따라간다.
    assert.equal(COMPLETE_BUSY_RETRIES, 5);
    assert.equal(COMPLETE_BUSY_INTERVAL, 400);
  });

  test("busy 가 아닌 거절은 다시 부르지 않는다", async () => {
    const completes = [];
    uploadServer(0);
    const base = globalThis.fetch;
    globalThis.fetch = async (path, opts) => {
      if (path === "/api/attachments/att-1/complete") {
        completes.push(path);
        return respond(400, { error: { code: "invalid", message: "파일을 찾지 못했습니다. 다시 올려 주세요" } });
      }
      return base(path, opts);
    };
    const { s } = newSession();
    await assert.rejects(
      s.uploadAttachment("A", file()),
      (err) => err.code === "invalid" && err.message === "파일을 찾지 못했습니다. 다시 올려 주세요",
    );
    s.stop();

    assert.equal(completes.length, 1);
  });

  test("완료가 storage_timeout 이면 다시 부르지 않고 서버 문구를 그대로 알린다", async () => {
    // 옮긴 뒤 시한을 넘겼으면 같은 첨부의 완료를 다시 불러도 되지 않는다. 사용자가 파일을 다시 올려야 한다.
    const completes = [];
    uploadServer(0);
    const base = globalThis.fetch;
    globalThis.fetch = async (path, opts) => {
      if (path === "/api/attachments/att-1/complete") {
        completes.push(path);
        return respond(503, { error: { code: "storage_timeout", message: "저장소가 느리다. 파일을 다시 올려라" } });
      }
      return base(path, opts);
    };
    const { s } = newSession();
    await assert.rejects(
      s.uploadAttachment("A", file()),
      (err) => err.code === "storage_timeout" && err.message === "저장소가 느리다. 파일을 다시 올려라",
    );
    s.stop();

    assert.equal(completes.length, 1);
  });
});

describe("첨부 진행률", () => {
  /** 저장소 올리기를 받는 가짜 XMLHttpRequest. 보낸 바이트를 두 번 나눠 알린 뒤 status 로 끝난다. */
  function fakeXHR(status) {
    const sent = [];
    globalThis.XMLHttpRequest = class {
      constructor() {
        this.upload = {};
      }
      open(method, url) {
        this.url = url;
        this.method = method;
      }
      send(form) {
        sent.push({ url: this.url, method: this.method, file: form.get("file") });
        setTimeout(() => {
          this.upload.onprogress?.({ lengthComputable: true, loaded: 40, total: 100 });
          this.upload.onprogress?.({ lengthComputable: true, loaded: 100, total: 100 });
          this.status = status;
          if (status === 0) this.onerror?.();
          else this.onload?.();
        }, 0);
      }
    };
    return sent;
  }
  function server() {
    const calls = [];
    globalThis.fetch = async (path) => {
      calls.push(path);
      if (path === "/api/conversations/A/attachments") {
        return respond(201, { attachment: { id: "att-1" }, upload: { url: "/files/upload", fields: { token: "t" } } });
      }
      if (path === "/api/attachments/att-1/complete") return respond(201, { attachment: { id: "att-1", content_type: "video/mp4" } });
      return respond(200, { messages: [] });
    };
    return calls;
  }
  const video = () => new File(["\x00\x00\x00\x18ftypisom"], "clip.mp4", { type: "video/mp4" });

  test("진행률을 받으면 저장소로 올리는 동안 보낸 바이트를 알리고 완료된 첨부를 돌려준다", async () => {
    const calls = server();
    const sent = fakeXHR(204);
    const { s } = newSession();
    const seen = [];
    const att = await s.uploadAttachment("A", video(), { onProgress: (loaded, total) => seen.push([loaded, total]) });
    s.stop();
    delete globalThis.XMLHttpRequest;

    assert.deepEqual(seen, [[40, 100], [100, 100]]);
    assert.equal(sent.length, 1);
    assert.equal(sent[0].url, "/files/upload");
    assert.equal(sent[0].file.name, "clip.mp4");
    assert.ok(!calls.includes("/files/upload"), "진행률을 받으면 fetch 가 아니라 XMLHttpRequest 로 올린다");
    assert.equal(att.content_type, "video/mp4");
  });

  test("진행률로 올릴 때도 저장소가 413 이면 완료를 부르지 않고 크기 초과로 알린다", async () => {
    const calls = server();
    fakeXHR(413);
    const { s } = newSession();
    await assert.rejects(s.uploadAttachment("A", video(), { onProgress: () => {} }), (err) => err.code === "invalid");
    s.stop();
    delete globalThis.XMLHttpRequest;

    assert.ok(!calls.includes("/api/attachments/att-1/complete"));
  });

  test("진행률로 올리다 응답을 못 읽어도(다른 출처·끊김) 올라갔는지는 완료 확인이 판정한다", async () => {
    const calls = server();
    fakeXHR(0);
    const { s } = newSession();
    const att = await s.uploadAttachment("A", video(), { onProgress: () => {} });
    s.stop();
    delete globalThis.XMLHttpRequest;

    assert.ok(calls.includes("/api/attachments/att-1/complete"));
    assert.equal(att.id, "att-1");
  });
});

describe("첨부 크기 초과와 취소", () => {
  // S3(와 MinIO)는 양식의 content-length-range 를 넘으면 413 이 아니라 400 과 이 본문으로 답한다.
  const ENTITY_TOO_LARGE = `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>EntityTooLarge</Code><Message>Your proposed upload exceeds the maximum allowed size</Message></Error>`;
  function server(uploadStatus, uploadBody = "") {
    const calls = [];
    globalThis.fetch = async (path) => {
      calls.push(path);
      if (path === "/api/conversations/A/attachments") {
        return respond(201, { attachment: { id: "att-1" }, upload: { url: "/files/upload", fields: { token: "t" } } });
      }
      if (path === "/files/upload") return { ok: uploadStatus < 400, status: uploadStatus, text: async () => uploadBody };
      if (path === "/api/attachments/att-1/complete") return respond(201, { attachment: { id: "att-1" } });
      return respond(200, { messages: [] });
    };
    return calls;
  }
  /** 저장소 올리기를 받는 가짜 XMLHttpRequest. finish 를 부르기 전까지 끝나지 않는다. abort 를 부르면 끊긴다. */
  function holdXHR() {
    const xhrs = [];
    globalThis.XMLHttpRequest = class {
      constructor() {
        this.upload = {};
        this.aborted = false;
        xhrs.push(this);
      }
      open() {}
      send() {}
      abort() {
        this.aborted = true;
        this.onabort?.();
      }
      finish(status, text = "") {
        this.status = status;
        this.responseText = text;
        this.onload?.();
      }
    };
    return xhrs;
  }
  const file = () => new File(["AAAAA"], "big.mp4", { type: "video/mp4" });
  const settle = async () => { for (let i = 0; i < 10; i++) await new Promise((r) => setImmediate(r)); };

  test("저장소가 400 EntityTooLarge 로 답하면 413 과 같이 완료를 부르지 않고 크기 초과로 알린다", { timeout: 3000 }, async () => {
    // 진행률 없이(fetch) 올릴 때
    let calls = server(400, ENTITY_TOO_LARGE);
    const { s } = newSession();
    await assert.rejects(s.uploadAttachment("A", file()), (err) => err.code === "invalid" && /크기를 넘습니다/.test(err.message));
    assert.ok(!calls.includes("/api/attachments/att-1/complete"), "크기 초과면 완료를 부르지 않는다");

    // 진행률을 받으며(XMLHttpRequest) 올릴 때
    calls = server(0);
    const xhrs = holdXHR();
    const up = s.uploadAttachment("A", file(), { onProgress: () => {} });
    await settle();
    xhrs[0].finish(400, ENTITY_TOO_LARGE);
    await assert.rejects(up, (err) => err.code === "invalid" && /크기를 넘습니다/.test(err.message));
    assert.ok(!calls.includes("/api/attachments/att-1/complete"));

    // EntityTooLarge 가 아닌 400(못 읽은 본문 포함)은 크기 초과라고 단정하지 않고 완료 확인에 맡긴다
    calls = server(400, "");
    const att = await s.uploadAttachment("A", file());
    assert.equal(att.id, "att-1");
    assert.ok(calls.includes("/api/attachments/att-1/complete"));
    s.stop();
    delete globalThis.XMLHttpRequest;
  });

  test("업로드를 취소하면 저장소 올리기를 멈추고 완료를 부르지 않는다", { timeout: 3000 }, async () => {
    const calls = server(0);
    const xhrs = holdXHR();
    const { s } = newSession();
    const ctrl = new AbortController();
    const up = s.uploadAttachment("A", file(), { onProgress: () => {}, signal: ctrl.signal });
    await settle();
    assert.equal(xhrs.length, 1, "저장소로 올리는 중이다");
    ctrl.abort();
    await assert.rejects(up, (err) => err.code === "aborted");
    assert.equal(xhrs[0].aborted, true, "올리던 요청을 끊는다");
    assert.ok(!calls.includes("/api/attachments/att-1/complete"), "취소한 업로드는 완료를 부르지 않는다");

    // 시작하기 전에 이미 취소됐으면 아무것도 부르지 않는다
    const before = calls.length;
    await assert.rejects(s.uploadAttachment("A", file(), { signal: AbortSignal.abort() }), (err) => err.code === "aborted");
    assert.equal(calls.length, before);
    s.stop();
    delete globalThis.XMLHttpRequest;
  });
});

describe("PUT 으로 올리기(D42)", () => {
  // 양식 업로드가 없는 S3 호환 저장소는 서명된 PUT 주소를 준다. 파일이 본문이고 서버가 준 헤더(서명된 Content-Type)를 그대로 싣는다.
  const PUT_URL = "https://storage.example/bucket/tmp/att-1?X-Amz-Signature=abc";
  const UPLOAD = { method: "PUT", url: PUT_URL, headers: { "Content-Type": "video/mp4" } };
  function server(uploadStatus = 200) {
    const calls = [];
    globalThis.fetch = async (path, opts = {}) => {
      calls.push({ path, method: opts.method, headers: opts.headers, body: opts.body });
      if (path === "/api/conversations/A/attachments") return respond(201, { attachment: { id: "att-1" }, upload: UPLOAD });
      if (path === PUT_URL) return { ok: uploadStatus < 400, status: uploadStatus, text: async () => "" };
      if (path === "/api/attachments/att-1/complete") return respond(201, { attachment: { id: "att-1", content_type: "video/mp4" } });
      return respond(200, { messages: [] });
    };
    return calls;
  }
  /** 저장소 올리기를 받는 가짜 XMLHttpRequest. 실은 헤더와 본문을 남기고, finish 를 부르기 전까지 끝나지 않는다. */
  function holdXHR() {
    const xhrs = [];
    globalThis.XMLHttpRequest = class {
      constructor() {
        this.upload = {};
        this.headers = {};
        this.aborted = false;
        xhrs.push(this);
      }
      open(method, url) {
        this.method = method;
        this.url = url;
      }
      setRequestHeader(k, v) {
        this.headers[k] = v;
      }
      send(body) {
        this.body = body;
      }
      abort() {
        this.aborted = true;
        this.onabort?.();
      }
      finish(status) {
        this.upload.onprogress?.({ lengthComputable: true, loaded: 9, total: 9 });
        this.status = status;
        this.onload?.();
      }
    };
    return xhrs;
  }
  const video = () => new File(["\x00\x00\x00\x18ftypisom"], "clip.mp4", { type: "video/mp4" });
  const settle = async () => { for (let i = 0; i < 10; i++) await new Promise((r) => setImmediate(r)); };

  test("PUT 양식이면 파일을 본문으로 서명된 주소에 PUT 하고 받은 헤더를 그대로 실은 뒤 완료를 부른다", async () => {
    const calls = server();
    const { s } = newSession();
    const f = video();
    const att = await s.uploadAttachment("A", f);
    s.stop();

    const up = calls.find((c) => c.path === PUT_URL);
    assert.ok(up, "서명된 주소로 올린다");
    assert.equal(up.method, "PUT");
    assert.deepEqual(up.headers, { "Content-Type": "video/mp4" });
    assert.equal(up.body, f, "양식이 아니라 파일 그 자체가 본문이다");
    assert.ok(calls.some((c) => c.path === "/api/attachments/att-1/complete"));
    assert.equal(att.id, "att-1");
  });

  test("PUT 도 진행률을 받으면 XMLHttpRequest 로 같은 헤더와 파일을 보내며 보낸 바이트를 알린다", { timeout: 3000 }, async () => {
    const calls = server();
    const xhrs = holdXHR();
    const { s } = newSession();
    const f = video();
    const seen = [];
    const up = s.uploadAttachment("A", f, { onProgress: (loaded, total) => seen.push([loaded, total]) });
    await settle();
    assert.equal(xhrs.length, 1);
    assert.equal(xhrs[0].method, "PUT");
    assert.equal(xhrs[0].url, PUT_URL);
    assert.deepEqual(xhrs[0].headers, { "Content-Type": "video/mp4" });
    assert.equal(xhrs[0].body, f);
    xhrs[0].finish(200);
    const att = await up;
    s.stop();
    delete globalThis.XMLHttpRequest;

    assert.deepEqual(seen, [[9, 9]]);
    assert.ok(!calls.some((c) => c.path === PUT_URL), "진행률을 받으면 fetch 가 아니라 XMLHttpRequest 로 올린다");
    assert.equal(att.id, "att-1");
  });

  test("PUT 으로 올리는 중에 취소하면 요청을 끊고 완료를 부르지 않는다", { timeout: 3000 }, async () => {
    const calls = server();
    const xhrs = holdXHR();
    const { s } = newSession();
    const ctrl = new AbortController();
    const up = s.uploadAttachment("A", video(), { onProgress: () => {}, signal: ctrl.signal });
    await settle();
    ctrl.abort();
    await assert.rejects(up, (err) => err.code === "aborted");
    s.stop();
    delete globalThis.XMLHttpRequest;

    assert.equal(xhrs[0].aborted, true);
    assert.ok(!calls.some((c) => c.path === "/api/attachments/att-1/complete"));
  });

  test("PUT 이 서명 불일치(403)로 거절돼도 단정하지 않고 완료 확인이 판정한다", async () => {
    const calls = server(403);
    const { s } = newSession();
    const att = await s.uploadAttachment("A", video());
    s.stop();

    assert.ok(calls.some((c) => c.path === "/api/attachments/att-1/complete"));
    assert.equal(att.id, "att-1");
  });

  test("method 가 없는 양식(이전 서버)은 지금처럼 POST 양식으로 올린다", async () => {
    const calls = [];
    globalThis.fetch = async (path, opts = {}) => {
      calls.push({ path, method: opts.method, body: opts.body });
      if (path === "/api/conversations/A/attachments") {
        return respond(201, { attachment: { id: "att-1" }, upload: { url: "/files/upload", fields: { token: "t" } } });
      }
      if (path === "/files/upload") return respond(204, null);
      if (path === "/api/attachments/att-1/complete") return respond(201, { attachment: { id: "att-1" } });
      return respond(200, { messages: [] });
    };
    const { s } = newSession();
    await s.uploadAttachment("A", video());
    s.stop();

    const up = calls.find((c) => c.path === "/files/upload");
    assert.equal(up.method, "POST");
    assert.ok(up.body instanceof FormData);
    assert.equal(up.body.get("token"), "t");
    assert.equal(up.body.get("file").name, "clip.mp4");
  });
});

describe("대화 메타", () => {
  test("종료·재개를 화면에 알린다", () => {
    const { s, shown } = newSession();
    s.open("A");
    s.handleFrame({ type: "conversation", conversation_id: "A", conversation: { id: "A", state: "CLOSED" } });
    s.handleFrame({ type: "conversation", conversation_id: "A", conversation: { id: "A", state: "OPEN" } });

    assert.deepEqual(shown.conversations.map((c) => c.state), ["CLOSED", "OPEN"]);
  });

  test("다른 대화의 변화는 무시한다", () => {
    const { s, shown } = newSession();
    s.open("A");
    s.handleFrame({ type: "conversation", conversation_id: "B", conversation: { id: "B", state: "CLOSED" } });

    assert.equal(shown.conversations.length, 0);
  });

  test("구독 응답에 실려 온 대화 정보도 그대로 전한다", () => {
    const { s, shown } = newSession();
    s.open("A");
    s.handleFrame({
      type: "ready", conversation_id: "A", last_seq: 2, reads: [],
      conversation: { id: "A", state: "SNOOZED" },
    });

    assert.deepEqual(shown.conversations.map((c) => c.state), ["SNOOZED"]);
  });
});

describe("사라진 대화", () => {
  const goneSession = () => {
    const gone = [];
    const { s, shown } = newSession({ onGone: (id) => gone.push(id) });
    return { s, shown, gone };
  };

  test("보던 대화가 없다는 답은 오류가 아니라 닫힘으로 한 번 알린다", () => {
    const { s, shown, gone } = goneSession();
    s.open("A");
    s.handleFrame({ type: "error", conversation_id: "A", code: "not_found", message: "대화를 찾을 수 없습니다" });
    // 같은 대화의 뒤늦은 답(읽음·타이핑 거절)은 대화 id 없이 온다. 이미 닫았으니 다시 알리지 않는다.
    s.handleFrame({ type: "error", code: "not_found", message: "대화를 찾을 수 없습니다" });
    s.handleFrame({ type: "message", ...msg("A", 1) });

    assert.deepEqual(gone, ["A"]);
    assert.deepEqual(shown.errors, [], "오류 토스트로 띄우면 안 된다");
    assert.equal(shown.messages.length, 0, "닫은 대화의 프레임을 화면에 올렸다");
  });

  test("이미 떠난 대화의 not_found 는 버린다", () => {
    const { s, shown, gone } = goneSession();
    s.open("A");
    s.open("B");
    s.handleFrame({ type: "error", conversation_id: "A", code: "not_found", message: "대화를 찾을 수 없습니다" });

    assert.deepEqual(gone, []);
    assert.deepEqual(shown.errors, []);
  });

  test("닫은 뒤에 온 not_found 는 아무것도 띄우지 않는다", () => {
    const { s, shown, gone } = goneSession();
    s.open("A");
    s.close();
    s.handleFrame({ type: "error", conversation_id: "A", code: "not_found", message: "대화를 찾을 수 없습니다" });
    s.handleFrame({ type: "error", code: "not_found", message: "대화를 찾을 수 없습니다" });

    assert.deepEqual(gone, []);
    assert.deepEqual(shown.errors, []);
    s.typing();
    assert.equal(framesOfType("typing").length, 0, "닫은 뒤에는 타이핑을 보내지 않는다");
  });

  test("갭 복구가 없는 대화라는 답을 받으면 닫힘으로 알린다", async () => {
    globalThis.fetch = async () => respond(404, { error: { code: "not_found", message: "대화를 찾을 수 없습니다" } });
    const { s, shown, gone } = goneSession();
    s.open("A");
    s.ingest(msg("A", 5));
    await sleep(60);
    s.stop();

    assert.deepEqual(gone, ["A"]);
    assert.deepEqual(shown.errors, []);
  });

  test("메시지 하나의 거절은 여전히 오류로 알린다", () => {
    const { s, shown, gone } = goneSession();
    s.open("A");
    s.send("지워진 대화에 보낸 글");
    const sent = framesOfType("send").at(-1);
    s.handleFrame({ type: "error", client_msg_id: sent.client_msg_id, code: "not_found", message: "대화를 찾을 수 없습니다" });

    assert.deepEqual(gone, []);
    assert.equal(shown.errors.length, 1, "보내지 못한 글은 사용자에게 알려야 한다");
    assert.deepEqual(storedPending("A"), []);
  });
});

describe("저장소 한도", () => {
  test("용량이 차면 옛 커서를 버리고 전송 대기 큐는 지킨다", () => {
    const { s } = newSession();
    for (let i = 0; i < 5; i++) {
      localStorage.setItem(`${STORE}old${i}`, JSON.stringify({ lastSeq: 7, pending: [] }));
    }
    seedPending("keepme", ["아직 못 보낸 문의"]);

    const real = localStorage.setItem.bind(localStorage);
    let failed = false;
    localStorage.setItem = (k, v) => {
      if (!failed && k.startsWith(`${QUEUE}A|`)) {
        failed = true; // 첫 시도는 용량 초과
        throw new Error("QuotaExceededError");
      }
      real(k, v);
    };

    s.open("A");
    s.send("새 문의");
    localStorage.setItem = real;

    assert.equal(storedPending("A").length, 1, "정리 뒤 재시도로 저장돼야 한다");
    assert.equal(storedPending("keepme").length, 1, "전송 대기가 남은 대화를 지웠다");
    assert.equal(localStorage.getItem(`${STORE}old0`), null, "전송 대기 없는 커서는 정리 대상이다");
  });
});

describe("큐의 주인", () => {
  test("다른 주체의 전송 대기 큐는 보내지도 지우지도 않는다", async () => {
    // A 상담원이 남긴 큐. 같은 브라우저에 B 가 들어온다.
    const a = new ChatSession({ token: "tok-a", owner: "agent:A" });
    a.open("C1");
    a.send("A 가 못 보낸 것");
    a.ws?.close();
    const aKey = [...store.keys()].find((k) => k.startsWith("gcs.q1.agent:A.C1|"));
    assert.ok(aKey, "소유자 접두사가 붙은 키에 저장된다");

    const posted = [];
    globalThis.fetch = async (url, _init) => {
      posted.push(url);
      return respond(404, { error: { code: "not_found", message: "없다" } });
    };
    const b = new ChatSession({ token: "tok-b", owner: "agent:B" });
    b.start();
    await sleep(20);
    b.stop();
    assert.equal(posted.length, 0, "B 는 A 의 큐를 보내지 않는다");
    assert.equal(storedPendingAt("gcs.v1.agent:A.", "C1").length, 1, "A 의 큐는 그대로 남는다");
  });
});

describe("소켓이 열려 있는데 서버가 거절한 메시지", () => {
  test("오래 ack 가 없으면 REST 로 다시 민다", async () => {
    const { s } = newSession();
    s.open("A");
    const item = s.send("거절당한 메시지");
    // 서버가 일시 오류 프레임을 보냈다. 영구 거절이 아니라 큐에 남는다.
    s.handleFrame({ type: "error", code: "internal", client_msg_id: item.client_msg_id, message: "잠깐 실패" });
    assert.equal(storedPending("A").length, 1);
    // 소켓은 멀쩡하다. 그래도 6초 넘게 ack 가 없으면 REST 가 민다.
    item.at = Date.now() - 7000;
    saveStateOf("A", s);
    const posted = [];
    globalThis.fetch = async (url, _init) => {
      posted.push(url);
      return respond(201, { message: msg("A", 1, "거절당한 메시지"), created: true });
    };
    await s.flushViaRest();
    assert.equal(posted.length, 1, "REST 로 한 번 밀었다");
    assert.equal(storedPending("A").length, 0, "ack 를 받아 큐에서 빠졌다");
  });
});

function saveStateOf(convId, s) {
  localStorage.setItem(STORE + convId, JSON.stringify({ lastSeq: s.state.lastSeq }));
  for (const p of s.state.pending) localStorage.setItem(`${QUEUE}${convId}|${p.client_msg_id}`, JSON.stringify(p));
}

describe("서버가 내려갈 때", () => {
  // closeWith 는 서버가 그 코드로 연결을 닫은 것처럼 만든다. 소켓이 바뀌기를 기다려 걸린 시간을 돌려준다.
  async function closeWith(s, code, limit) {
    const first = s.ws;
    const closedAt = Date.now();
    first.readyState = WebSocket.CLOSED;
    first.onclose?.({ code });
    while (s.ws === first && Date.now() - closedAt < limit) await sleep(5);
    await sleep(0); // 새 소켓의 onopen
    return { first, waited: Date.now() - closedAt };
  }

  test("클라이언트는 1001 을 받으면 백오프 없이(지터 250ms 까지) 다시 연결해 빠진 메시지를 이어 받는다", async () => {
    const { s, shown } = newSession();
    s.open("A");
    s.ingest(msg("A", 1));
    s.ingest(msg("A", 2));
    const item = s.send("내려가는 중에 보낸 문의");
    sentFrames.length = 0;

    const { first, waited } = await closeWith(s, 1001, 1000);

    assert.notEqual(s.ws, first, "다시 연결해야 한다");
    assert.ok(waited <= 250 + 50, `지터는 250ms 까지다. ${waited}ms 기다렸다`);
    assert.deepEqual(framesOfType("subscribe").at(-1), { type: "subscribe", conversation_id: "A", after_seq: 2 });
    // 새 서버가 구독을 받고 그 사이 메시지를 내려보낸다. ack 못 받은 메시지는 같은 id 로 다시 나간다.
    s.handleFrame({ type: "ready", conversation_id: "A", last_seq: 3 });
    s.handleFrame({ type: "message", ...msg("A", 3) });
    assert.deepEqual(shown.messages.map((m) => m.seq), [1, 2, 3]);
    assert.deepEqual(framesOfType("send").map((f) => f.client_msg_id), [item.client_msg_id]);
    s.stop();
  });

  test("클라이언트는 1001 이 아닌 코드로 끊기면 백오프를 둔다", async () => {
    const { s } = newSession();
    s.open("A");

    const { first } = await closeWith(s, 1006, 250 + 50);

    assert.equal(s.ws, first, "첫 백오프 500ms 가 지나기 전에 다시 붙었다");
    s.stop();
  });
});

// 갭 복구(REST)로 받은 메시지도 "서버가 저장했다" 의 증거다. 재접속 뒤 빠진 글은 대개 이 길로 돌아온다.
describe("갭 복구로 돌아온 글", () => {
  test("갭 복구로 받은 메시지에 같은 client_msg_id 가 있으면 포기하는 글을 돌려주지 않는다", async () => {
    const calls = [];
    const { s } = newSession({ onError: (...a) => calls.push(a) });
    s.open("A");
    s.send("끊기기 직전에 보낸 글");
    const id = framesOfType("send").at(-1).client_msg_id;
    globalThis.fetch = async (path) =>
      path.includes("/messages?after_seq=")
        ? respond(200, { messages: [{ ...msg("A", 1, "끊기기 직전에 보낸 글"), client_msg_id: id, sender_kind: "CUSTOMER" }] })
        : respond(200, { messages: [] });
    await s.backfill();
    for (let i = 0; i < 20; i++) s.handleFrame({ type: "error", code: "conflict", conversation_id: "A", client_msg_id: id, message: "이미 있습니다" });
    assert.deepEqual(storedPending("A"), []);
    assert.ok(calls.every((c) => c[2] === undefined), "갭 복구로 받은 글을 돌려주면 고객이 다시 보내 두 번 남는다");
    s.stop();
  });
});

// REST 로 다시 미는 간격(D5, 감사 10). 3초 고정이면 3분 끊김에 60번 넘게 두드리고, 429 의 Retry-After 를 무시하고,
// 한 대화의 요청이 끝나지 않으면 다른 대화의 큐까지 멈췄다.
describe("다시 미는 간격", () => {
  // 연결되지 않는 소켓(오프라인). 열리지 않으니 재연결 타이머도 돌지 않는다.
  class DeadSocket {
    static CONNECTING = 0;
    static OPEN = 1;
    static CLOSED = 3;
    constructor() { this.readyState = 0; }
    send(raw) { sentFrames.push(JSON.parse(raw)); }
    close() { this.readyState = 3; this.onclose?.({ code: 1006 }); }
  }
  const settle = async () => { for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r)); };
  let restore;
  // offline 은 시계·주기·소켓을 갈아 끼운다. online 이벤트 처리기를 돌려준다.
  function offline() {
    const LiveSocket = globalThis.WebSocket, addListener = globalThis.addEventListener;
    const listeners = {};
    globalThis.WebSocket = DeadSocket;
    globalThis.addEventListener = (t, fn) => (listeners[t] = fn);
    mock.timers.enable({ apis: ["setInterval", "setTimeout", "Date"], now: 1_000_000 });
    restore = () => {
      mock.timers.reset();
      globalThis.WebSocket = LiveSocket;
      globalThis.addEventListener = addListener;
    };
    return listeners;
  }
  async function run(seconds) {
    for (let t = 0; t < seconds; t++) {
      mock.timers.tick(1000);
      await settle();
    }
  }

  test("일시 실패가 이어지면 다시 미는 간격을 3초에서 두 배씩 30초까지 늘리고 연결이 돌아오면 곧바로 다시 민다", async () => {
    const listeners = offline();
    try {
      seedPending("A", ["3분 끊긴 동안 쓴 글"]);
      const at = [];
      globalThis.fetch = async (path, opts) => {
        if (opts?.method !== "POST") return respond(200, { messages: [] });
        at.push(Date.now());
        throw new TypeError("Failed to fetch");
      };
      const errors = [];
      const s = new ChatSession({ token: "tok", handlers: { onError: (e) => errors.push(e) } });
      s.open("B");
      s.start();
      await settle();
      await run(180);
      const gaps = at.slice(1).map((t, i) => t - at[i]);
      assert.ok(gaps[0] >= 2400 && gaps[0] < 4000, `첫 간격은 3초 안팎이다: ${gaps}`);
      assert.ok(gaps[1] >= 4800 && gaps[1] < 7000, `두 번째는 6초 안팎이다: ${gaps}`);
      assert.ok(gaps[2] >= 9600 && gaps[2] < 13000, `세 번째는 12초 안팎이다: ${gaps}`);
      assert.ok(gaps[3] >= 19200 && gaps[3] < 25000, `네 번째는 24초 안팎이다: ${gaps}`);
      for (const g of gaps.slice(4)) assert.ok(g >= 24000 && g <= 31000, `그 뒤는 30초를 넘지 않는다: ${gaps}`);
      assert.ok(at.length <= 12, `3분 끊김에 ${at.length}번 두드렸다 — 3초 고정이면 60번이다`);
      assert.deepEqual(storedPending("A").map((p) => p.body), ["3분 끊긴 동안 쓴 글"], "백오프 중에도 글은 큐에 남는다");
      assert.equal(errors.length, 0);

      // 브라우저가 온라인이 됐다 — 기다리던 간격을 버리고 다음 확인(1초 안)에 민다
      const before = at.length;
      listeners.online();
      await run(1);
      assert.equal(at.length, before + 1, "온라인이 됐는데 백오프가 끝날 때까지 기다린다");
      // 소켓이 다시 연결됐다 — 다른 대화의 큐도 곧바로 민다
      await run(5);
      const before2 = at.length;
      s.ws.readyState = DeadSocket.OPEN;
      s.ws.onopen();
      await run(1);
      assert.equal(at.length, before2 + 1, "소켓이 다시 붙었는데 다른 대화의 큐는 백오프가 끝날 때까지 기다린다");
      s.stop();
    } finally {
      restore();
    }
  });

  test("429 면 Retry-After 만큼 기다렸다가 다시 민다 — 연결 신호가 와도 서버가 정한 시간은 지킨다", async () => {
    const listeners = offline();
    try {
      seedPending("A", ["한도에 걸린 글"]);
      const at = [];
      globalThis.fetch = async (path, opts) => {
        if (opts?.method !== "POST") return respond(200, { messages: [] });
        at.push(Date.now());
        return { ok: false, status: 429, headers: { get: (k) => (k === "Retry-After" ? "20" : null) }, text: async () => "" };
      };
      const s = new ChatSession({ token: "tok", handlers: {} });
      s.open("B");
      s.start();
      await settle();
      await run(5);
      listeners.online();
      await run(10);
      assert.equal(at.length, 1, "Retry-After 전에 다시 두드린다");
      await run(6);
      assert.equal(at.length, 2);
      assert.ok(at[1] - at[0] >= 20000 && at[1] - at[0] <= 21000, `Retry-After 20초를 따른다: ${at[1] - at[0]}ms`);
      assert.equal(storedPending("A").length, 1);
      s.stop();
    } finally {
      restore();
    }
  });

  test("코드 없는 429 도 재시도 상한에 세지 않는다", async () => {
    seedPending("A", ["앞단 프록시가 본문 없이 429 로 막은 글", "그 뒤에 쓴 글"]);
    const posted = [];
    globalThis.fetch = async (path, opts) => {
      if (opts?.method !== "POST") return respond(200, { messages: [] });
      posted.push(JSON.parse(opts.body).body);
      return { ok: false, status: 429, text: async () => "" };
    };
    const { s, shown } = newSession();
    s.open("B");
    for (let i = 0; i < 25; i++) await s.flushViaRest();
    s.stop();
    assert.equal(storedPending("A").length, 2, "본문 없는 429 를 세면 20번째에 글을 포기한다");
    assert.equal(shown.errors.length, 0);
    assert.ok(posted.every((b) => b === "앞단 프록시가 본문 없이 429 로 막은 글"), "앞 글이 막혔는데 뒤 글이 먼저 나간다 — 한 대화 안의 순서가 깨진다");
  });

  test("한 대화의 막힌 글이 다른 대화 큐를 막지 않는다 — 대화마다 따로 밀고 한 대화 안의 순서는 지킨다", async () => {
    seedPending("A", ["A 의 앞 글", "A 의 뒤 글"]);
    seedPending("B", ["B 의 글"]);
    const posted = [];
    globalThis.fetch = async (path, opts) => {
      if (opts?.method !== "POST") return respond(200, { messages: [] });
      const conv = path.split("/")[3];
      posted.push([conv, JSON.parse(opts.body).body]);
      if (conv === "A") return new Promise(() => {}); // 끝나지 않는 요청(반쯤 끊긴 연결)
      return respond(201, { message: msg(conv, 1), created: true });
    };
    const { s } = newSession();
    s.open("C");
    s.ws.close();
    s.flushViaRest(); // A 가 끝나지 않으니 기다리지 않는다
    await settle();
    assert.deepEqual(storedPending("B"), [], "A 의 요청이 끝나지 않는다고 B 의 글이 묶인다");
    seedPending("B", ["B 의 다음 글"]);
    await s.flushViaRest({ due: true });
    assert.deepEqual(storedPending("B"), [], "다음 주기에도 B 는 제 차례에 나간다");
    assert.deepEqual(posted.filter(([c]) => c === "A").map(([, b]) => b), ["A 의 앞 글"], "앞 글이 끝나기 전에 뒤 글이 나가거나 같은 글이 겹쳐 나간다");
    assert.equal(storedPending("A").length, 2, "끝나지 않은 글은 큐에 남는다");
    s.stop();
  });

  test("백오프 간격은 3초에서 시작해 두 배씩 늘고 30초를 넘지 않는다", () => {
    assert.equal(restBackoff(1, 1), 3000);
    assert.equal(restBackoff(2, 1), 6000);
    assert.equal(restBackoff(5, 1), 30000);
    assert.equal(restBackoff(50, 1), 30000);
    assert.equal(REST_BACKOFF_MAX, 30000);
    assert.equal(restBackoff(1, 0), 2400, "지터는 아래로만 흔든다");
  });
});

// DEF-72(감사 13): 데스크가 대화를 여는 순간 DB 가 300ms 끊기자 구독이 일시 오류로 실패했고, 서버는 연결을 유지한 채 구독을 붙이지 않았다.
// 클라이언트는 구독을 열 때·소켓이 열릴 때만 보내서, 복구 뒤 고객이 보낸 글이 화면에 영영 오지 않았다. 읽음도 다음 글이 올 때까지 다시 보내지 않았다.
describe("구독과 읽음의 다시 시도", () => {
  const settle = async () => { for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r)); };
  let s, served, answer, stalls;
  // 서버 흉내. 1초마다 새로 나간 프레임에 답한다 — 구독에는 answer(f) 가 돌려준 프레임으로(없으면 답하지 않는다), ping 에는 pong 으로.
  // 프레임은 소켓으로 받는다(onmessage). 그래야 조용한 연결 감지가 소켓을 끊지 않는다.
  const subs = [];
  function serve() {
    for (; served < sentFrames.length; served++) {
      const f = sentFrames[served];
      if (f.type === "subscribe") {
        subs.push({ at: Date.now(), ...f });
        const reply = answer(f);
        if (reply) s.ws.onmessage({ data: JSON.stringify(reply) });
      }
      if (f.type === "ping") s.ws.onmessage({ data: JSON.stringify({ type: "pong" }) });
    }
  }
  async function run(seconds) {
    for (let t = 0; t < seconds; t++) {
      mock.timers.tick(1000);
      await settle();
      serve();
    }
  }
  const transient = { type: "error", code: "internal", message: "처리하지 못했습니다. 잠시 후 다시 시도해 주세요" };
  async function begin(conv = "A", more = {}) {
    mock.timers.enable({ apis: ["setInterval", "setTimeout", "Date"], now: 1_000_000 });
    served = 0;
    subs.length = 0;
    stalls = [];
    globalThis.fetch = async () => respond(200, { messages: [] });
    const errors = [];
    s = new ChatSession({ token: "tok", handlers: { onError: (e) => errors.push(e), onStalled: (v) => stalls.push(v), ...more } });
    s.start();
    await settle(); // 소켓이 열린다
    s.open(conv);
    serve();
    return errors;
  }
  function end() {
    s.stop();
    mock.timers.reset();
  }

  test("구독이 일시 오류로 실패하면 3초에서 두 배씩 30초까지 기다려 다시 구독하고 ready 를 받으면 멈춘다", async () => {
    let failing = true;
    answer = (f) => (failing ? transient : { type: "ready", conversation_id: f.conversation_id, last_seq: 0 });
    const errors = await begin();
    try {
      await run(120);
      const gaps = subs.slice(1).map((x, i) => x.at - subs[i].at);
      assert.ok(gaps[0] >= 2400 && gaps[0] <= 4000, `첫 간격은 3초 안팎이다: ${gaps}`);
      assert.ok(gaps[1] >= 4800 && gaps[1] <= 7000, `두 번째는 6초 안팎이다: ${gaps}`);
      assert.ok(gaps[2] >= 9600 && gaps[2] <= 13000, `세 번째는 12초 안팎이다: ${gaps}`);
      for (const g of gaps.slice(4)) assert.ok(g >= 24000 && g <= 31000, `그 뒤는 30초를 넘지 않는다: ${gaps}`);
      assert.ok(subs.every((x) => x.conversation_id === "A" && x.after_seq === 0), "보는 대화를 가진 번호부터 다시 구독한다");
      assert.deepEqual(errors, [], "다시 시도하는 실패는 알리지 않는다");
      failing = false;
      await run(31);
      const count = subs.length;
      await run(120);
      assert.equal(subs.length, count, "ready 를 받은 뒤에도 다시 구독한다");
    } finally {
      end();
    }
  });

  test("구독을 보내고 10초 안에 ready 가 없으면 다시 구독한다", async () => {
    answer = () => null; // 답이 없다(서버가 묶였다)
    await begin();
    try {
      await run(9);
      assert.equal(subs.length, 1, "10초가 되기 전에 다시 구독한다");
      await run(40);
      assert.ok(subs.length >= 3, "ready 가 없는데 다시 구독하지 않는다");
      const gap = subs[1].at - subs[0].at;
      assert.ok(gap >= 10000 + 2400 && gap <= 10000 + 4000, `10초를 기다리고 백오프(3초 안팎) 뒤에 다시 구독한다: ${gap}`);
      // 다시 보낸 구독도 10초를 따로 센다. 첫 구독 시각으로 세면 답을 기다리지 않고 백오프만으로 다시 보낸다(감사 14 A16)
      const second = subs[2].at - subs[1].at;
      assert.ok(second >= 10000 + 4800 && second <= 10000 + 8000, `두 번째 구독도 10초를 기다리고 백오프(6초 안팎) 뒤에 다시 구독한다: ${second}`);
    } finally {
      end();
    }
  });

  test("읽음 보고가 일시 오류로 실패하면 보고 지점을 되돌려 다시 보낸다", async () => {
    answer = (f) => ({ type: "ready", conversation_id: f.conversation_id, last_seq: 7 });
    await begin();
    try {
      const reads = () => framesOfType("read").filter((f) => f.conversation_id === "A");
      assert.deepEqual(reads().map((f) => f.seq), [7], "ready 를 받으면 그 지점을 읽음으로 보낸다");
      s.ws.onmessage({ data: JSON.stringify(transient) }); // 그 읽음이 DB 오류로 실패했다
      await run(5);
      assert.deepEqual(reads().map((f) => f.seq), [7, 7], "실패한 읽음을 다시 보내지 않는다");
      await run(60);
      assert.equal(reads().length, 2, "실패가 없는데 읽음을 또 보낸다");
      assert.equal(subs.length, 1, "구독은 붙어 있으니 다시 구독하지 않는다");
    } finally {
      end();
    }
  });

  test("소켓이 다시 붙은 뒤 구독이 일시 오류로 실패해도 다시 구독한다", async () => {
    let failing = false;
    answer = (f) => (failing ? transient : { type: "ready", conversation_id: f.conversation_id, last_seq: 0 });
    await begin();
    try {
      assert.equal(subs.length, 1);
      // 서버가 내려갔다(1001). 새 소켓이 붙는 순간 DB 가 잠깐 안 된다
      failing = true;
      s.ws.readyState = WebSocket.CLOSED;
      s.ws.onclose({ code: 1001 });
      await run(2);
      assert.equal(subs.length, 2, "다시 붙은 소켓이 구독하지 않는다");
      failing = false;
      await run(5);
      assert.equal(subs.length, 3, "앞 소켓의 ready 를 믿고 다시 구독하지 않는다");
    } finally {
      end();
    }
  });

  test("소켓은 열려 있는데 구독이 10초 넘게 성공하지 못하면 끊김 안내를 띄우고 ready 를 받으면 거둔다", async () => {
    let failing = true;
    answer = (f) => (failing ? transient : { type: "ready", conversation_id: f.conversation_id, last_seq: 0 });
    await begin();
    try {
      await run(9);
      assert.equal(stalls.filter(Boolean).length, 0, "막 연 대화에 끊김 안내를 띄운다");
      await run(3);
      assert.deepEqual(stalls.at(-1), { offline: true, count: 0 }, "구독이 오래 붙지 않는데 안내가 없다 — 새 메시지가 오지 않는다");
      failing = false;
      await run(20);
      assert.equal(stalls.at(-1), null, "구독이 붙은 뒤에도 안내가 남는다");
      // 소켓이 다시 붙은 뒤에도 같다. 새 소켓의 구독을 기다리기 시작한 때부터 센다(감사 14 A8)
      failing = true;
      s.ws.readyState = WebSocket.CLOSED;
      s.ws.onclose({ code: 1001 });
      await run(9);
      assert.equal(stalls.at(-1), null, "다시 붙자마자 끊김 안내를 띄운다");
      await run(4);
      assert.deepEqual(stalls.at(-1), { offline: true, count: 0 }, "다시 붙은 소켓의 구독이 오래 붙지 않는데 안내가 없다");
    } finally {
      end();
    }
  });

  // 감사 14 A1: ready 의 대화 id 확인을 빼도 테스트가 통과했다. 빼면 앞 대화의 늦은 ready 를 지금 대화의 구독으로 보고(다시 구독하지 않는다),
  // 앞 대화의 정보를 화면에 그리고, 앞 대화의 번호로 지금 대화에 읽음을 보낸다.
  test("A 를 열고 ready 전에 B 로 옮기면 늦게 온 A 의 ready 는 버린다 — B 를 구독된 것으로 보지 않고 A 의 대화와 번호를 B 에 쓰지 않는다", async () => {
    answer = () => null; // 서버가 늦다
    const seen = { conv: [], ready: 0, reads: 0 };
    await begin("A", { onConversation: (c) => seen.conv.push(c.id), onReady: () => seen.ready++, onReads: () => seen.reads++ });
    try {
      s.open("B");
      serve();
      s.ws.onmessage({ data: JSON.stringify({ type: "ready", conversation_id: "A", last_seq: 42, conversation: { id: "A", customer_name: "가고객" }, reads: [{ reader_kind: "AGENT", last_read_seq: 40 }] }) });
      assert.deepEqual(seen, { conv: [], ready: 0, reads: 0 }, "떠난 대화의 ready 를 화면에 넘겼다");
      assert.deepEqual(framesOfType("read"), [], "떠난 대화의 번호로 읽음을 보냈다");
      await run(15);
      assert.deepEqual(subs.map((f) => f.conversation_id), ["A", "B", "B"], "B 의 ready 가 없는데 다시 구독하지 않는다 — 떠난 대화의 ready 를 B 의 것으로 봤다");
      assert.ok(framesOfType("read").every((f) => f.seq !== 42), "떠난 대화의 번호로 읽음을 보냈다");
    } finally {
      end();
    }
  });

  // 감사 14 C9: 대화를 열 때 구독 상태를 비우지 않아도 테스트가 통과했다. 비우지 않으면 앞 대화에서 쌓인 백오프 횟수가 이어져 새 대화가 30초 가까이 기다린다.
  test("다른 대화를 열면 다시 구독의 간격은 처음(3초 안팎)부터다 — 앞 대화의 실패 횟수를 잇지 않는다", async () => {
    answer = () => transient;
    await begin("A");
    try {
      await run(60); // A 가 여러 번 실패해 간격이 30초 가까이 늘었다
      assert.ok(subs.filter((f) => f.conversation_id === "A").length >= 4);
      s.open("B");
      serve();
      await run(5);
      const b = subs.filter((f) => f.conversation_id === "B");
      assert.ok(b.length >= 2, `B 가 실패한 뒤 3초 안팎에 다시 구독하지 않는다: ${b.map((f) => f.at - b[0].at)}`);
      assert.ok(b[1].at - b[0].at <= 4000, `B 의 첫 다시 구독 간격: ${b[1].at - b[0].at}`);
    } finally {
      end();
    }
  });

  // 감사 14 A10: 다시 구독 간격의 지터를 빼도 테스트가 통과했다. 지터가 없으면 같은 순간 실패한 연결(장애 조치 뒤 전원)이 같은 순간에 다시 구독한다.
  test("다시 구독 간격도 아래로 20% 까지 흔든다 — 같은 순간 실패한 연결이 한꺼번에 다시 구독하지 않는다", async () => {
    answer = () => transient;
    const realRandom = Math.random;
    Math.random = () => 0; // 가장 많이 당긴다
    try {
      await begin("A");
      await run(120);
      const gaps = subs.slice(1).map((x, i) => x.at - subs[i].at);
      // 백오프 3·6·12·24·30초의 80% 는 2.4·4.8·9.6·19.2·24초다. 1초 주기로 보므로 그 다음 초에 나간다
      assert.ok(gaps[3] <= 20000, `네 번째 간격이 24초 그대로다 — 흔들지 않는다: ${gaps}`);
      for (const g of gaps.slice(4)) assert.ok(g <= 25000, `30초 상한도 흔들어야 한다: ${gaps}`);
    } finally {
      Math.random = realRandom;
      end();
    }
  });

  // 감사 14 A14: 화면이 안 보일 때도 실패한 읽음을 다시 보내게 해도 테스트가 통과했다. 보이지 않는 동안 읽었다고 하면 상대에게 거짓 읽음이 간다.
  test("화면이 보이지 않으면 실패한 읽음을 다시 보내지 않는다 — 보이지 않는 동안 읽었다고 하지 않는다", async () => {
    answer = (f) => ({ type: "ready", conversation_id: f.conversation_id, last_seq: 7 });
    await begin();
    try {
      const reads = () => framesOfType("read").filter((f) => f.conversation_id === "A");
      assert.equal(reads().length, 1);
      document.visibilityState = "hidden";
      s.ws.onmessage({ data: JSON.stringify(transient) }); // 그 읽음이 DB 오류로 실패했다
      await run(10);
      assert.equal(reads().length, 1, "화면이 안 보이는데 읽음을 다시 보냈다");
    } finally {
      document.visibilityState = "visible";
      end();
    }
  });
});

// 감사 14 A3: 1초 주기(구독 확인) 타이머를 timers 에 넣지 않으면 stop() 뒤에도 남아 테스트 프로세스가 끝나지 않았다(640초 멈춤).
test("멈추면 주기 타이머를 하나도 남기지 않는다", async () => {
  const before = new Set(liveIntervals);
  const s = new ChatSession({ token: "tok", handlers: {} });
  s.start();
  await sleep(0);
  s.open("A");
  s.stop();
  const left = [...liveIntervals].filter((t) => !before.has(t));
  assert.equal(left.length, 0, `stop() 뒤에 주기 타이머 ${left.length}개가 남았다`);
});

// ── 토큰 보안(D46) ─────────────────────────────────────────

describe("WebSocket 티켓(DEF-91)", () => {
  const settle = async () => { for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r)); };
  let urls, restoreSocket;
  beforeEach(() => {
    urls = [];
    const Live = globalThis.WebSocket;
    globalThis.WebSocket = class extends Live {
      constructor(url) {
        super(url);
        urls.push(url);
      }
    };
    restoreSocket = () => { globalThis.WebSocket = Live; };
  });
  const cleanup = (s) => {
    s?.stop();
    restoreSocket();
    mock.timers.reset();
  };

  test("소켓 주소에는 세션 토큰이 실리지 않고 티켓만 실리며 다시 붙을 때마다 새 티켓을 받는다", async () => {
    mock.timers.enable({ apis: ["setTimeout"] });
    let n = 0;
    const asked = [];
    const s = new ChatSession({ token: "session-secret", ticket: async (tok) => { asked.push(tok); return `T${++n}`; } });
    try {
      s.connect();
      await settle();
      s.ws.close(); // 끊겼다 — 백오프 뒤 다시 붙는다
      mock.timers.tick(1000);
      await settle();
      assert.deepEqual(urls.map((u) => new URL(u).search), ["?ticket=T1", "?ticket=T2"]);
      assert.ok(urls.every((u) => !u.includes("session-secret")), `주소에 세션 토큰이 실렸다: ${urls}`);
      assert.deepEqual(asked, ["session-secret", "session-secret"], "티켓은 지금 세션 토큰으로 받는다");
    } finally {
      cleanup(s);
    }
  });

  test("티켓을 못 받으면(네트워크·5xx) 끊긴 소켓과 같은 백오프로 다시 받으러 가고 그동안 쓴 글은 큐에 남는다", async () => {
    mock.timers.enable({ apis: ["setTimeout", "Date"], now: 1_000_000 });
    const realRandom = Math.random;
    Math.random = () => 0;
    const at = [];
    const fails = [new TypeError("Failed to fetch"), Object.assign(new Error("HTTP 503"), { status: 503 })];
    const statuses = [];
    const s = new ChatSession({
      token: "tok",
      handlers: { onStatus: (v) => statuses.push(v) },
      ticket: async () => {
        at.push(Date.now());
        const err = fails.shift();
        if (err) throw err;
        return "OK";
      },
    });
    try {
      s.open("A");
      s.send("티켓이 안 나오는 동안 쓴 글");
      s.connect();
      await settle();
      assert.equal(urls.length, 0, "티켓 없이 소켓을 열지 않는다");
      assert.equal(statuses.at(-1), "offline");
      assert.equal(storedPending("A").length, 1, "글은 큐에 남는다");
      mock.timers.tick(500);
      await settle();
      mock.timers.tick(1000);
      await settle();
      assert.deepEqual(at.map((t) => t - at[0]), [0, 500, 1500], "0.5초에서 두 배씩 — 소켓 재연결과 같은 백오프다");
      assert.deepEqual(urls.map((u) => new URL(u).search), ["?ticket=OK"]);
      assert.equal(storedPending("A").length, 1, "ack 를 받기 전까지 큐에 남는다");
    } finally {
      Math.random = realRandom;
      cleanup(s);
    }
  });

  test("티켓이 401 이면 세션 만료로 알리고 평소 백오프로 다시 시도한다", async () => {
    mock.timers.enable({ apis: ["setTimeout"] });
    let unauthorized = 0;
    let calls = 0;
    const s = new ChatSession({
      token: "expired",
      handlers: { onUnauthorized: () => unauthorized++ },
      ticket: async () => {
        calls++;
        throw Object.assign(new Error("세션이 만료되었습니다"), { status: 401, code: "unauthorized" });
      },
    });
    try {
      s.connect();
      await settle();
      assert.equal(unauthorized, 1);
      assert.equal(urls.length, 0);
      mock.timers.tick(1000);
      await settle();
      assert.equal(calls, 2, "다시 시도한다 — 화면이 새 토큰을 주면 reconnect 가 이어 간다");
    } finally {
      cleanup(s);
    }
  });

  test("티켓을 받는 사이 토큰이 바뀌면 옛 토큰의 티켓으로 열지 않는다", async () => {
    const pending = {};
    const s = new ChatSession({ token: "old", ticket: (tok) => new Promise((resolve) => { pending[tok] = resolve; }) });
    try {
      s.connect();
      await settle();
      s.reconnect("new");
      await settle();
      pending.new("T-NEW");
      await settle();
      pending.old("T-OLD"); // 늦게 온 옛 티켓
      await settle();
      assert.deepEqual(urls.map((u) => new URL(u).search), ["?ticket=T-NEW"]);
    } finally {
      cleanup(s);
    }
  });

  // 옛 서버(v0.0.4)는 POST /api/ws-ticket 을 정적 파일 서버("GET /")가 받아 405 text/plain "Method Not Allowed" 로 답한다.
  // 앞단 프록시나 다른 판의 서버는 404 text/plain 이다. JSON 이 아니어도 상태를 잃지 않아야 옛 길로 붙는다(감사 20 — 전에는 본문 파싱이 상태를 지웠다).
  test("기본 티켓은 POST /api/ws-ticket 에 Authorization 헤더로 받고, 티켓 길이 없는 옛 서버(405·404 text/plain)면 옛 길로 붙는다", async () => {
    setTicketSource(null);
    const seen = [];
    let reply = null;
    const plain = (status, text) => ({ ok: false, status, headers: new Headers({ "Content-Type": "text/plain; charset=utf-8" }), text: async () => text });
    globalThis.fetch = async (path, opts = {}) => {
      seen.push({ path, method: opts.method, auth: opts.headers?.Authorization });
      return reply || respond(200, { ticket: "FROM-SERVER", expires_at: "x" });
    };
    let s = new ChatSession({ token: "tok-1" });
    try {
      s.connect();
      await settle();
      assert.deepEqual(seen[0], { path: "/api/ws-ticket", method: "POST", auth: "Bearer tok-1" });
      assert.equal(new URL(urls[0]).search, "?ticket=FROM-SERVER");
      s.stop();
      reply = plain(405, "Method Not Allowed\n"); // v0.0.4 의 실제 답
      s = new ChatSession({ token: "tok-2" });
      s.connect();
      await settle();
      assert.equal(new URL(urls[1]).search, "?token=tok-2", "옛 서버(405)에는 옛 길로 붙는다 — 배포 중에만 생긴다");
      s.stop();
      reply = plain(404, "404 page not found\n");
      s = new ChatSession({ token: "tok-3" });
      s.connect();
      await settle();
      assert.equal(new URL(urls[2]).search, "?token=tok-3", "티켓 길이 없는 서버(404)에도 옛 길로 붙는다");
    } finally {
      setTicketSource(() => "tkt");
      cleanup(s);
    }
  });

  test("JSON 이 아닌 오류 본문도 HTTP 상태를 남긴다 — 프록시의 502·401 HTML 은 일시 실패이고 우리 서버의 JSON 401 unauthorized 만 세션 만료다", async () => {
    setTicketSource(null);
    let unauthorized = 0;
    let reply = { ok: false, status: 502, text: async () => "<html>Bad Gateway</html>" };
    globalThis.fetch = async () => reply;
    const s = new ChatSession({ token: "tok", handlers: { onUnauthorized: () => unauthorized++ } });
    mock.timers.enable({ apis: ["setTimeout"] });
    try {
      s.connect();
      await settle();
      assert.equal(urls.length, 0, "502 를 옛 서버로 오해해 옛 길로 붙었다");
      assert.equal(s.status, "offline");
      // 앞단(인증 게이트웨이·WAF)의 401 HTML 은 우리 서버의 판정이 아니다. 세션 만료로 알리면 익명 위젯이 새 고객이 된다(감사 22).
      reply = { ok: false, status: 401, text: async () => "<html>401 Authorization Required</html>" };
      mock.timers.tick(1000);
      await settle();
      assert.equal(unauthorized, 0, "앞단의 401 HTML 을 세션 만료로 알렸다");
      assert.equal(s.status, "offline", "일시 실패라 백오프로 다시 받으러 간다");
      reply = { ok: false, status: 401, text: async () => JSON.stringify({ error: { code: "unauthorized", message: "로그인 정보가 만료되었거나 올바르지 않습니다" } }) };
      mock.timers.tick(10000);
      await settle();
      assert.equal(unauthorized, 1, "우리 서버의 JSON 401 unauthorized 는 세션 만료로 알린다");
    } finally {
      setTicketSource(() => "tkt");
      cleanup(s);
    }
  });

  test("티켓 요청이 10초 안에 오지 않으면 그 요청을 끊고 끊긴 소켓과 같은 백오프로 다시 받는다 — 늦게 온 티켓으로 열지 않는다", async () => {
    mock.timers.enable({ apis: ["setTimeout"] });
    const asks = [];
    const s = new ChatSession({
      token: "tok",
      ticket: (_tok, { signal } = {}) => new Promise((resolve) => asks.push({ resolve, signal })),
    });
    try {
      s.connect();
      await settle();
      assert.equal(asks.length, 1);
      mock.timers.tick(9999);
      await settle();
      assert.equal(asks[0].signal?.aborted, false, "시한 전에는 기다린다");
      mock.timers.tick(1);
      await settle();
      assert.equal(asks[0].signal?.aborted, true, "시한이 지난 요청을 끊지 않았다");
      assert.equal(s.status, "offline");
      mock.timers.tick(1000);
      await settle();
      assert.equal(asks.length, 2, "시한 뒤 백오프가 지나도 다시 받으러 가지 않는다 — connecting 이 풀리지 않았다");
      asks[0].resolve("LATE");
      asks[1].resolve("FRESH");
      await settle();
      assert.deepEqual(urls.map((u) => new URL(u).search), ["?ticket=FRESH"], "시한이 지난 티켓으로 열었다");
    } finally {
      cleanup(s);
    }
  });

  // 감사 21 J16: 위 테스트는 늦은 티켓이 다음 요청이 나간 뒤에 와서, 시한이 시도를 버리지 않아도(connectSeq) 통과했다.
  // 시한과 다음 시도 사이(백오프 동안)에 늦은 티켓이 와도 그 티켓으로 열지 않는다.
  test("시한이 지난 티켓 요청의 답이 백오프 동안 와도 그 티켓으로 열지 않고 다음 시도의 티켓으로 연다", async () => {
    mock.timers.enable({ apis: ["setTimeout"] });
    const asks = [];
    const s = new ChatSession({ token: "tok", ticket: () => new Promise((resolve) => asks.push(resolve)) });
    try {
      s.connect();
      await settle();
      mock.timers.tick(10000);
      await settle();
      assert.equal(s.status, "offline");
      assert.equal(asks.length, 1, "백오프 전에 다시 받으러 갔다");
      asks[0]("LATE"); // 시한은 지났고 다음 시도는 아직이다
      await settle();
      assert.deepEqual(urls, [], "시한이 지난 티켓으로 열었다");
      mock.timers.tick(1000);
      await settle();
      assert.equal(asks.length, 2);
      asks[1]("FRESH");
      await settle();
      assert.deepEqual(urls.map((u) => new URL(u).search), ["?ticket=FRESH"]);
    } finally {
      cleanup(s);
    }
  });

  test("티켓을 못 받으면 여는 중 표시가 풀려 다음 connect 가 곧바로 다시 받으러 간다", async () => {
    let calls = 0;
    const s = new ChatSession({
      token: "tok",
      ticket: async () => {
        calls++;
        throw Object.assign(new Error("HTTP 503"), { status: 503 });
      },
    });
    mock.timers.enable({ apis: ["setTimeout"] });
    try {
      s.connect();
      await settle();
      s.connect(); // online 이벤트·탭 보이기가 부르는 connect
      await settle();
      assert.equal(calls, 2, "실패한 뒤에도 여는 중으로 남아 connect 를 막는다");
    } finally {
      cleanup(s);
    }
  });
});

describe("숨긴 탭의 생존 확인(DEF-102)", () => {
  const settle = async () => { for (let i = 0; i < 5; i++) await new Promise((r) => setImmediate(r)); };
  const pong = (s) => s.ws.onmessage({ data: JSON.stringify({ type: "pong" }) });
  async function begin() {
    mock.timers.enable({ apis: ["Date"], now: 5_000_000 });
    const sockets = [];
    const Live = globalThis.WebSocket;
    globalThis.WebSocket = class extends Live {
      constructor(url) {
        super(url);
        sockets.push(this);
      }
    };
    const statuses = [];
    const { s } = newSession({ onStatus: (v) => statuses.push(v) });
    await settle(); // onopen
    s.open("A");
    const end = () => {
      s.stop();
      globalThis.WebSocket = Live;
      document.visibilityState = "visible";
      mock.timers.reset();
    };
    return { s, sockets, statuses, end };
  }
  const pings = () => framesOfType("ping").length;

  test("숨긴 탭에서 확인이 1분 간격으로 늦게 돌아도 ping 에 답이 오면 연결을 끊지 않는다", async () => {
    const { s, sockets, end } = await begin();
    try {
      document.visibilityState = "hidden";
      for (let i = 0; i < 5; i++) {
        mock.timers.tick(60_000); // 브라우저가 숨긴 탭 타이머를 1분으로 묶었다
        s.checkLiveness();
        pong(s); // 서버는 곧바로 답한다(메시지 이벤트는 숨긴 탭에서도 바로 온다)
      }
      assert.equal(s.ws.readyState, WebSocket.OPEN, "멀쩡한 연결을 끊었다 — 분마다 다시 붙는다");
      assert.ok(sockets.length <= 1, `새 소켓을 ${sockets.length - 1}번 열었다`);
      assert.equal(pings(), 5, "늦게 돈 확인마다 ping 하나로 살아 있는지 묻는다");
    } finally {
      end();
    }
  });

  test("숨긴 탭에서 보낸 ping 에 답이 없으면 다음 확인에서 끊는다 — 죽은 연결은 숨긴 탭에서도 찾는다", async () => {
    const { s, statuses, end } = await begin();
    try {
      document.visibilityState = "hidden";
      mock.timers.tick(60_000);
      s.checkLiveness();
      assert.equal(pings(), 1);
      assert.equal(s.ws.readyState, WebSocket.OPEN, "ping 을 보낸 그 확인에서는 끊지 않는다");
      mock.timers.tick(60_000);
      s.checkLiveness();
      assert.equal(statuses.at(-1), "offline", "답 없는 ping 뒤 1분이 지났는데 끊지 않았다");
    } finally {
      end();
    }
  });

  // 보이는 탭이어도 기기가 잠들었다 깨면(노트북 덮개, 휴대폰 화면 끔) 확인이 늦게 돈다. 그 사이 조용했던 시간은 연결이 죽은 증거가 아니다.
  test("보이는 탭에서도 확인이 늦게 돌면 조용한 시간으로 끊지 않고 ping 으로 묻는다 — 답이 오면 그대로다", async () => {
    const { s, sockets, end } = await begin();
    try {
      mock.timers.tick(60_000); // 기기가 잠들어 확인이 1분 늦었다
      s.checkLiveness();
      assert.equal(s.ws.readyState, WebSocket.OPEN, "늦게 돈 확인이 조용한 1분을 보고 끊었다");
      assert.equal(pings(), 1, "늦은 확인은 ping 으로 묻는다");
      pong(s);
      mock.timers.tick(2000);
      s.checkLiveness();
      assert.equal(s.ws.readyState, WebSocket.OPEN);
      assert.ok(sockets.length <= 1, `새 소켓을 ${sockets.length - 1}번 열었다`);
    } finally {
      end();
    }
  });

  test("보이는 탭은 전처럼 6초 조용하면 ping 을 보내고 14초 조용하면 끊는다", async () => {
    const { s, statuses, end } = await begin();
    try {
      let dropped = 0;
      for (let t = 2; t <= 20; t += 2) {
        mock.timers.tick(2000);
        s.checkLiveness();
        if (!dropped && statuses.at(-1) === "offline") dropped = t;
      }
      assert.ok(pings() >= 1, "6초 뒤 ping 을 보낸다");
      assert.equal(dropped, 16, `14초를 넘긴 첫 확인(16초)에 끊는다: ${dropped}`);
    } finally {
      end();
    }
  });
});

// ── 고객 종료(D48) ─────────────────────────────────────────────
// 유실 방지가 먼저다 — 보내지 못한 글이 있으면 먼저 보내고 저장 확인(REST 응답)을 받은 뒤 닫는다. 보낼 수 없으면 닫지 않는다.
// 닫은 상담에 고객 글은 closed_by_customer 로 거절되므로, 큐를 남긴 채 닫으면 그 글이 거절로 큐에서 빠진다(그 길이 없어야 한다).
describe("고객 종료(D48)", () => {
  // closeServer 는 글 저장과 닫기를 받는 서버다. 닫힌 뒤에 온 글은 실제 서버처럼 409 closed_by_customer 로 거절한다.
  function closeServer({ failSend = null } = {}) {
    const calls = [];
    let closed = false;
    globalThis.fetch = async (path, opts = {}) => {
      const method = opts.method || "GET";
      if (method === "POST" && /\/messages$/.test(path)) {
        const body = JSON.parse(opts.body);
        calls.push({ kind: "send", body: body.body, id: body.client_msg_id });
        if (failSend) return failSend();
        if (closed) return respond(409, { error: { code: "closed_by_customer", message: "종료한 상담에는 메시지를 보낼 수 없습니다" } });
        return respond(201, { message: { conversation_id: "A", seq: calls.length, client_msg_id: body.client_msg_id, body: body.body } });
      }
      if (method === "POST" && path === "/api/customer/conversations/A/close") {
        calls.push({ kind: "close" });
        closed = true;
        return respond(200, { conversation: { id: "A", state: "CLOSED", closed_by: "CUSTOMER" } });
      }
      return respond(200, { messages: [] });
    };
    return calls;
  }

  test("보내지 못한 글이 있으면 먼저 보내고 저장 확인을 받은 뒤 닫는다 — 큐는 비고 거절된 글이 없다", async () => {
    const calls = closeServer();
    const { s, shown } = newSession();
    s.open("A");
    s.send("닫기 직전에 쓴 글"); // 소켓으로 나갔지만 ack 를 아직 못 받았다
    assert.equal(storedPending("A").length, 1);

    const conv = await s.closeConversation("A");
    assert.deepEqual(calls.map((c) => c.kind), ["send", "close"], "글을 먼저 보내고 그 저장 확인 뒤에 닫는다");
    assert.equal(calls[0].body, "닫기 직전에 쓴 글");
    assert.equal(conv.state, "CLOSED");
    assert.equal(conv.closed_by, "CUSTOMER");
    assert.deepEqual(storedPending("A"), [], "보낸 글은 저장 확인으로 큐에서 빠진다");
    assert.deepEqual(shown.errors, [], "거절로 사라진 글이 없다");
    s.stop();
  });

  test("같은 브라우저의 다른 탭이 남긴 같은 상담의 글도 먼저 보낸다", async () => {
    const calls = closeServer();
    const { s, shown } = newSession();
    s.open("A");
    seedPending("A", ["다른 탭에서 쓴 글"]); // 이 탭이 연 뒤에 다른 탭이 저장소에 넣었다
    await s.closeConversation("A");
    assert.deepEqual(calls.map((c) => c.body ?? c.kind), ["다른 탭에서 쓴 글", "close"]);
    assert.deepEqual(storedPending("A"), []);
    assert.deepEqual(shown.errors, []);
    s.stop();
  });

  test("글을 보낼 수 없으면(끊김) 닫지 않고 pending_unsent 로 알리며 글은 큐에 남는다", async () => {
    const calls = closeServer({ failSend: () => { throw new TypeError("Failed to fetch"); } });
    const { s, shown } = newSession();
    s.open("A");
    s.send("끊긴 채 쓴 글");
    await assert.rejects(s.closeConversation("A"), (err) => err.code === "pending_unsent" && err.count === 1);
    assert.ok(!calls.some((c) => c.kind === "close"), "보내지 못한 글이 있는데 닫았다");
    assert.equal(storedPending("A").length, 1, "글은 큐에 남아 연결되면 나간다");
    assert.deepEqual(shown.errors, [], "포기하지 않은 글에 실패를 알리지 않는다(DEF-01)");
    s.stop();
  });

  test("서버가 일시 실패(5xx)로 답해도 닫지 않는다", async () => {
    const calls = closeServer({ failSend: () => respond(503, { error: { code: "internal", message: "잠시 후" } }) });
    const { s } = newSession();
    s.open("A");
    s.send("503 을 받은 글");
    await assert.rejects(s.closeConversation("A"), (err) => err.code === "pending_unsent");
    assert.ok(!calls.some((c) => c.kind === "close"));
    assert.equal(storedPending("A").length, 1);
    s.stop();
  });

  test("보낼 글이 없으면 곧바로 닫는다", async () => {
    const calls = closeServer();
    const { s } = newSession();
    s.open("A");
    await s.closeConversation("A");
    assert.deepEqual(calls.map((c) => c.kind), ["close"]);
    s.stop();
  });

  test("닫힌 뒤 다른 탭이 늦게 보낸 글은 closed_by_customer 로 큐에서 빠지고 글을 화면에 돌려준다 — 조용히 사라지지 않는다", async () => {
    const calls = closeServer();
    const dropped = [];
    const { s } = newSession({ onError: (msg, id, item) => dropped.push({ msg, item }) });
    s.open("A");
    await s.closeConversation("A");
    seedPending("A", ["늦은 글"]); // 다른 탭이 닫힌 줄 모르고 넣었다
    s.close(); // 지금 대화가 아니면 저장소의 큐를 민다
    await s.flushViaRest();
    assert.ok(calls.some((c) => c.body === "늦은 글"));
    assert.deepEqual(storedPending("A"), []);
    assert.equal(dropped.length, 1, "거절 알림이 한 번 온다");
    assert.equal(dropped[0].item.body, "늦은 글", "알림이 그 글을 들고 온다");
    assert.equal(dropped[0].item.code, "closed_by_customer");
    s.stop();
  });
});

describe("같은 브라우저의 두 탭(DEF-158)", () => {
  // tabs 는 같은 저장소를 쓰는 세션(탭) 여럿을 만든다. 저장소 이벤트는 브라우저처럼 쓴 탭을 뺀 나머지 탭에만 간다.
  function tabs(n) {
    const list = [];
    for (let i = 0; i < n; i++) {
      const listeners = [];
      const real = globalThis.addEventListener;
      globalThis.addEventListener = (t, fn) => t === "storage" && listeners.push(fn);
      const tab = newSession();
      globalThis.addEventListener = real;
      tab.storage = (key) => listeners.forEach((fn) => fn({ key }));
      list.push(tab);
    }
    // tell 은 from 탭이 key 를 바꿨다고 다른 탭들에 알린다.
    const tell = (from, key) => list.filter((t) => t !== from).forEach((t) => t.storage(key));
    return { list, tell };
  }
  const itemKeyOf = (convId, item) => `${QUEUE}${convId}|${item.client_msg_id}`;
  const lastShown = (tab) => tab.shown.pending.at(-1) || [];

  test("두 탭이 같은 상담에 번갈아 쓴 글은 저장소에서 서로 덮어쓰지 않고 한 탭을 ack 전에 닫아도 남은 탭이 모두 보낸다", () => {
    const { list: [one, two], tell } = tabs(2);
    one.s.open("A");
    two.s.open("A"); // 둘 다 빈 큐를 읽고 열었다
    // 저장소 이벤트는 나중에 온다. 서로의 글을 모르는 채 번갈아 쓴다.
    const written = [];
    for (const [tab, body] of [[one, "a1"], [two, "b1"], [one, "a2"], [two, "b2"]]) written.push([tab, tab.s.send(body)]);
    assert.deepEqual(storedPending("A").map((p) => p.body), ["a1", "b1", "a2", "b2"], "한 탭의 저장이 다른 탭의 글을 지웠다");
    for (const [tab, item] of written) tell(tab, itemKeyOf("A", item));

    one.s.stop(); // 탭 하나를 ack 를 받기 전에 닫았다
    sentFrames.length = 0;
    two.s.handleFrame({ type: "ready", conversation_id: "A", last_seq: 0, reads: [] }); // 남은 탭이 다시 붙었다
    assert.deepEqual(framesOfType("send").map((f) => f.body).sort(), ["a1", "a2", "b1", "b2"], "닫힌 탭의 글을 남은 탭이 보내지 않았다");

    const three = newSession();
    three.s.open("A"); // 새로 연 탭도 저장소에서 모두 읽는다
    assert.deepEqual(lastShown(three), ["a1", "b1", "a2", "b2"]);
    two.s.stop();
    three.s.stop();
  });

  test("다른 탭이 ack 를 받아 지운 글은 이 탭의 큐에서도 빠지고 이 탭이 다시 써도 되살아나지 않는다", () => {
    const { list: [one, two], tell } = tabs(2);
    one.s.open("A");
    two.s.open("A");
    const a = one.s.send("탭 1 의 글");
    tell(one, itemKeyOf("A", a));
    assert.deepEqual(lastShown(two), ["탭 1 의 글"], "다른 탭이 넣은 글이 이 탭의 큐에 들어오지 않았다");

    one.s.handleFrame({ type: "ack", conversation_id: "A", client_msg_id: a.client_msg_id });
    // 이벤트가 오기 전에 이 탭도 같은 글을 보냈다가 명시적 거절을 받아 시도 횟수를 센다. 지워진 글을 되살리면 안 된다.
    two.s.handleFrame({ type: "error", code: "forbidden", conversation_id: "A", client_msg_id: a.client_msg_id, message: "잠깐" });
    assert.deepEqual(storedPending("A"), [], "ack 된 글이 되살아났다");
    tell(one, itemKeyOf("A", a));
    assert.deepEqual(lastShown(two), [], "다른 탭이 ack 로 지운 글이 이 탭에 남았다");

    const b = two.s.send("탭 2 의 글");
    two.s.handleFrame({ type: "error", code: "forbidden", conversation_id: "A", client_msg_id: b.client_msg_id, message: "잠깐" });
    assert.deepEqual(storedPending("A").map((p) => p.body), ["탭 2 의 글"]);
    assert.equal(storedPending("A")[0].tries, 1, "명시적 거절은 저장소의 그 글에 센다");
    one.s.stop();
    two.s.stop();
  });

  test("다른 대화의 저장소 변경은 보고 있는 대화의 큐를 바꾸지 않는다", () => {
    const { list: [one, two] } = tabs(2);
    two.s.open("A");
    const before = two.shown.pending.length;
    one.s.open("B");
    const other = one.s.send("다른 대화의 글");
    two.storage(itemKeyOf("B", other));
    assert.equal(two.shown.pending.length, before, "다른 대화의 변경으로 화면의 큐를 다시 그렸다");
    one.s.stop();
    two.s.stop();
  });

  test("옛 형식 큐가 남은 채 열면 그 글을 보내고 글마다 키로 옮기며 커서는 그대로다", () => {
    localStorage.setItem(STORE + "A", JSON.stringify({ lastSeq: 5, pending: [
      { client_msg_id: "old-1", body: "옛 큐 1", at: 1000, tries: 2 },
      { client_msg_id: "old-2", body: "옛 큐 2", at: 1000, tries: 0 },
    ] }));
    const { s, shown } = newSession();
    s.open("A");
    assert.deepEqual(shown.pending.at(-1), ["옛 큐 1", "옛 큐 2"], "옛 큐의 순서를 지킨다");
    s.handleFrame({ type: "ready", conversation_id: "A", last_seq: 5, reads: [] });
    assert.deepEqual(framesOfType("send").map((f) => f.client_msg_id), ["old-1", "old-2"]);
    assert.ok(store.has(`${QUEUE}A|old-1`) && store.has(`${QUEUE}A|old-2`), "글마다 키로 옮기지 않았다");
    assert.deepEqual(JSON.parse(store.get(STORE + "A")), { lastSeq: 5 }, "옮긴 뒤 옛 배열을 지우고 커서는 남긴다");
    assert.equal(JSON.parse(store.get(`${QUEUE}A|old-1`)).tries, 2, "옮기며 시도 횟수를 잃었다");
    s.handleFrame({ type: "ack", conversation_id: "A", client_msg_id: "old-1" });
    assert.deepEqual(storedPending("A").map((p) => p.client_msg_id), ["old-2"]);
    s.stop();
  });

  test("옛 형식 큐를 옮기다 저장이 막히면 옛 큐를 지우지 않는다 — 커서를 써도 남는다", () => {
    localStorage.setItem(STORE + "A", JSON.stringify({ lastSeq: 0, pending: [{ client_msg_id: "old-1", body: "옮기지 못한 글", at: 1000, tries: 0 }] }));
    const real = localStorage.setItem;
    localStorage.setItem = (k, v) => {
      if (k.includes("|")) throw new Error("QuotaExceededError");
      return real(k, v);
    };
    try {
      const { s } = newSession();
      s.open("A");
      s.ingest(msg("A", 1)); // 커서를 쓴다
      s.stop();
    } finally {
      localStorage.setItem = real;
    }
    assert.deepEqual(storedPending("A").map((p) => p.body), ["옮기지 못한 글"], "옮기지 못한 채 옛 큐를 지웠다");
    assert.equal(JSON.parse(store.get(STORE + "A")).lastSeq, 1, "커서는 쓴다");

    const { s, shown } = newSession(); // 저장이 다시 되면 옮긴다
    s.open("A");
    assert.deepEqual(shown.pending.at(-1), ["옮기지 못한 글"]);
    assert.ok(store.has(`${QUEUE}A|old-1`));
    s.stop();
  });

  test("옛 코드의 탭이 같은 상담에 배열로 쓴 글도 새 탭이 저장소 이벤트로 받아 보낸다", () => {
    const { list: [tab] } = tabs(1);
    tab.s.open("A");
    tab.s.send("새 탭의 글");
    // 배포 중 아직 열려 있는 옛 코드의 탭이 자기 큐를 커서 키에 통째로 썼다.
    localStorage.setItem(STORE + "A", JSON.stringify({ lastSeq: 0, pending: [{ client_msg_id: "old-tab-1", body: "옛 탭의 글", at: Date.now(), tries: 0 }] }));
    tab.storage(STORE + "A");
    assert.deepEqual(lastShown(tab).sort(), ["새 탭의 글", "옛 탭의 글"].sort());
    assert.deepEqual(storedPending("A").map((p) => p.body).sort(), ["새 탭의 글", "옛 탭의 글"].sort(), "옛 탭의 쓰기가 새 탭의 글을 지웠다");
    sentFrames.length = 0;
    tab.s.handleFrame({ type: "ready", conversation_id: "A", last_seq: 0, reads: [] });
    assert.ok(framesOfType("send").some((f) => f.client_msg_id === "old-tab-1"), "옛 탭의 글을 보내지 않았다");
    tab.s.stop();
  });
});

describe("큐의 순서와 이름 공간(감사 27)", () => {
  // 같은 저장소를 쓰는 탭 둘. 저장소 이벤트는 쓴 탭을 뺀 나머지에만 간다(위 DEF-158 묶음과 같은 흉내).
  function twoTabs() {
    const list = [];
    for (let i = 0; i < 2; i++) {
      const listeners = [];
      const real = globalThis.addEventListener;
      globalThis.addEventListener = (t, fn) => t === "storage" && listeners.push(fn);
      const tab = newSession();
      globalThis.addEventListener = real;
      tab.storage = (key) => listeners.forEach((fn) => fn({ key }));
      list.push(tab);
    }
    return list;
  }
  const keyOf = (convId, item) => `${QUEUE}${convId}|${item.client_msg_id}`;
  // withKeyOrder 는 저장소가 키를 내놓는 순서를 바꾼다. 브라우저는 키 나열 순서를 약속하지 않는다.
  function withKeyOrder(order, fn) {
    const real = localStorage.key;
    localStorage.key = (i) => order([...store.keys()])[i] ?? null;
    try {
      return fn();
    } finally {
      localStorage.key = real;
    }
  }
  const reversed = (keys) => [...keys].reverse();
  const interleaved = (keys) => [...keys.filter((_, i) => i % 2), ...keys.filter((_, i) => i % 2 === 0).reverse()];

  test("저장하지 못한 이 탭의 글은 다른 탭이 ack 로 지운 것을 맞출 때 지우지 않고 다시 붙으면 보낸다", () => {
    const [one, two] = twoTabs();
    one.s.open("A");
    two.s.open("A");
    const real = localStorage.setItem;
    localStorage.setItem = () => { throw new Error("SecurityError"); }; // 이 탭은 저장이 막혔다
    try {
      two.s.send("저장 못 한 글");
    } finally {
      localStorage.setItem = real;
    }
    assert.deepEqual(storedPending("A"), [], "저장이 막혔는데 저장소에 있다");
    const a = one.s.send("다른 탭의 글");
    two.storage(keyOf("A", a));
    one.s.handleFrame({ type: "ack", conversation_id: "A", client_msg_id: a.client_msg_id }); // 다른 탭이 ack 를 받아 지웠다
    two.storage(keyOf("A", a));
    assert.deepEqual(two.shown.pending.at(-1), ["저장 못 한 글"], "저장소에 없다고 이 탭의 글을 버렸다");
    sentFrames.length = 0;
    two.s.handleFrame({ type: "ready", conversation_id: "A", last_seq: 0, reads: [] });
    assert.deepEqual(framesOfType("send").map((f) => f.body), ["저장 못 한 글"], "다시 붙었는데 보내지 않았다");
    one.s.stop();
    two.s.stop();
  });

  test("끊긴 채 연속으로 쓴 글은 저장소가 키를 어떤 순서로 내놓아도 쓴 순서로 나간다", () => {
    const now = mock.method(Date, "now", () => 1_000_000); // 같은 밀리초에 썼다
    try {
      const { s } = newSession();
      s.open("A");
      const want = [];
      for (let i = 1; i <= 8; i++) {
        want.push(`s${i}`);
        s.send(`s${i}`);
      }
      s.stop(); // 탭을 닫았다
      for (const order of [reversed, interleaved]) {
        withKeyOrder(order, () => {
          const next = newSession();
          next.s.open("A");
          assert.deepEqual(next.shown.pending.at(-1), want, `${order.name}: 큐의 순서가 저장소 키 순서를 따랐다`);
          sentFrames.length = 0;
          next.s.handleFrame({ type: "ready", conversation_id: "A", last_seq: 0, reads: [] });
          assert.deepEqual(framesOfType("send").map((f) => f.body), want, `${order.name}: 쓴 순서로 나가지 않았다`);
          next.s.stop();
        });
      }
    } finally {
      now.mock.restore();
    }
  });

  test("시계가 뒤로 가도 대화 안의 순서는 쓴 순서다 — 두 탭이 같은 번호를 받으면 어느 탭이 읽어도 같은 순서다", async () => {
    let clock = 5_000_000;
    const now = mock.method(Date, "now", () => clock);
    try {
      const { s } = newSession();
      s.open("A");
      s.send("먼저");
      clock -= 3_600_000; // 시계를 한 시간 되감았다(NTP·사용자 변경)
      s.send("나중");
      clock -= 60_000;
      s.send("맨 나중");
      s.stop();
      for (const order of [reversed, interleaved]) {
        withKeyOrder(order, () => {
          const next = newSession();
          next.s.open("A");
          assert.deepEqual(next.shown.pending.at(-1), ["먼저", "나중", "맨 나중"], "시계를 따라 순서가 뒤집혔다");
          next.s.stop();
        });
      }
      // REST 로 밀 때도 같은 순서다
      const posted = [];
      globalThis.fetch = async (_p, init) => {
        const b = JSON.parse(init.body);
        posted.push(b.body);
        return respond(201, { message: { conversation_id: "A", seq: posted.length, client_msg_id: b.client_msg_id, body: b.body } });
      };
      const rest = newSession();
      await rest.s.pushConversation("A");
      assert.deepEqual(posted, ["먼저", "나중", "맨 나중"]);
      rest.s.stop();

      // 다른 탭의 글이 저장소에 있으면(이 탭의 메모리에는 없어도) 그 다음 번호를 받는다. client_msg_id 는 뒤로 갈수록 작게 만들어
      // 번호가 같아지면 순서가 뒤집히게 한다.
      let k = 0;
      const ids = mock.method(crypto, "randomUUID", () => `id-${String(99 - k++).padStart(2, "0")}`);
      try {
        const [p, q] = twoTabs();
        p.s.open("C");
        q.s.open("C");
        for (const [tab, body] of [[p, "c1"], [q, "c2"], [p, "c3"], [q, "c4"]]) tab.s.send(body); // 저장소 이벤트 없이 번갈아 썼다
        const next = newSession();
        next.s.open("C");
        assert.deepEqual(next.shown.pending.at(-1), ["c1", "c2", "c3", "c4"], "저장소의 다른 탭 글보다 앞 번호를 받았다");
        for (const t of [p, q, next]) t.s.stop();
      } finally {
        ids.mock.restore();
      }

      // 두 탭이 서로의 글을 보기 전에 같은 번호를 받았다
      const [one, two] = twoTabs();
      one.s.open("B");
      two.s.open("B");
      const x = one.s.send("탭 1");
      // 탭 2 의 저장소 사본에는 아직 탭 1 의 글이 없다(브라우저는 다른 탭의 쓰기를 나중에 맞춘다)
      const seen = store.get(keyOf("B", x));
      store.delete(keyOf("B", x));
      const y = two.s.send("탭 2");
      store.set(keyOf("B", x), seen);
      assert.equal(JSON.parse(store.get(keyOf("B", x))).n, JSON.parse(store.get(keyOf("B", y))).n, "같은 번호를 만들지 못했다(흉내가 틀렸다)");
      const want = [x, y].sort((p, q) => (p.client_msg_id < q.client_msg_id ? -1 : 1)).map((p) => p.body);
      for (const order of [(keys) => keys, reversed, interleaved]) {
        withKeyOrder(order, () => {
          const next = newSession();
          next.s.open("B");
          assert.deepEqual(next.shown.pending.at(-1), want, "같은 번호의 순서가 키 순서에 따라 달라졌다");
          next.s.stop();
        });
      }
      one.s.stop();
      two.s.stop();
    } finally {
      now.mock.restore();
    }
  });

  test("용량이 차서 커서를 정리해도 다른 대화의 글마다 키로 쓴 전송 대기 글은 지우지 않는다", () => {
    const b = newSession();
    b.s.open("B");
    b.s.send("B 에 쓴 글");
    b.s.stop();
    for (let i = 0; i < 5; i++) localStorage.setItem(`${STORE}old${i}`, JSON.stringify({ lastSeq: 7 }));
    // 아직 열지 않은 대화 M 에는 중간 형식(커서 이름 공간의 글 키)으로 남은 글이 있다. 열 때 옮기기 전까지 그 키가 글이다
    store.set(`${STORE}M|mid-9`, JSON.stringify({ client_msg_id: "mid-9", body: "M 에 남은 글", at: 1, tries: 0 }));
    const real = localStorage.setItem;
    let failed = false;
    localStorage.setItem = (k, v) => {
      if (!failed && k.startsWith(`${QUEUE}A|`)) {
        failed = true;
        throw new Error("QuotaExceededError");
      }
      return real(k, v);
    };
    try {
      const { s } = newSession();
      s.open("A");
      s.send("A 에 쓴 글");
      s.stop();
    } finally {
      localStorage.setItem = real;
    }
    assert.equal(localStorage.getItem(`${STORE}old0`), null, "정리가 돌지 않았다(흉내가 틀렸다)");
    assert.deepEqual(storedPending("B").map((p) => p.body), ["B 에 쓴 글"], "커서를 정리하며 다른 대화의 글을 지웠다");
    assert.deepEqual(storedPending("A").map((p) => p.body), ["A 에 쓴 글"]);
    assert.ok(store.has(`${STORE}M|mid-9`), "커서를 정리하며 옮기기 전의 글 키를 지웠다");
  });

  // 되돌려 배포한 옛 코드(DEF-158 이전)는 접두사 아래 키를 모두 커서로 보고, 용량이 차거나 키가 200개를 넘으면
  // 전송 대기 배열(pending)이 없는 키를 지운다. 그 정리를 그대로 흉내 낸다.
  function oldCodePrune(prefix, keepConvId) {
    for (const key of [...store.keys()]) {
      if (!key.startsWith(prefix)) continue;
      const rest = key.slice(prefix.length);
      if (prefix === STORE && rest.includes(".")) continue;
      if (rest === keepConvId) continue;
      const pending = JSON.parse(store.get(key) || "{}").pending;
      if (Array.isArray(pending) && pending.length > 0) continue;
      store.delete(key);
    }
  }

  test("되돌려 배포한 옛 코드가 커서를 정리해도 새 코드가 쓴 전송 대기 글은 남아 새 코드가 다시 열면 보낸다", () => {
    const owner = "customer:c1";
    const prefix = `${STORE}${owner}.`;
    const tab = new ChatSession({ token: "tok", owner });
    tab.connect();
    tab.open("A");
    tab.ws.close(); // 끊긴 채 썼다
    tab.send("되돌려 배포 전에 쓴 글");
    tab.stop();
    oldCodePrune(prefix, "other"); // 옛 코드의 탭이 다른 상담을 보다가 용량이 차서 정리했다
    oldCodePrune(STORE, "other");
    assert.ok(![...store.keys()].some((k) => k.startsWith(prefix) && k.includes("|")), "글 키가 옛 코드가 정리하는 이름 공간에 있다");

    const next = new ChatSession({ token: "tok", owner });
    next.connect();
    next.open("A");
    sentFrames.length = 0;
    next.handleFrame({ type: "ready", conversation_id: "A", last_seq: 0, reads: [] });
    assert.deepEqual(framesOfType("send").map((f) => f.body), ["되돌려 배포 전에 쓴 글"], "옛 코드의 정리로 글을 잃었다");
    next.stop();
  });

  test("커서 이름 공간에 글마다 키로 남은 글(배포되지 않은 중간 형식)은 열 때 글 이름 공간으로 옮겨 보낸다", () => {
    store.set(`${STORE}A|mid-1`, JSON.stringify({ client_msg_id: "mid-1", body: "중간 형식의 글", at: 1000, ord: 1, tries: 3 }));
    const { s, shown } = newSession();
    s.open("A");
    assert.deepEqual(shown.pending.at(-1), ["중간 형식의 글"]);
    assert.ok(!store.has(`${STORE}A|mid-1`), "옛 키가 남아 옛 코드의 정리에 걸린다");
    assert.equal(JSON.parse(store.get(`${QUEUE}A|mid-1`)).tries, 3, "옮기며 시도 횟수를 잃었다");
    s.handleFrame({ type: "ready", conversation_id: "A", last_seq: 0, reads: [] });
    assert.ok(framesOfType("send").some((f) => f.client_msg_id === "mid-1"));
    s.handleFrame({ type: "ack", conversation_id: "A", client_msg_id: "mid-1" });
    assert.deepEqual(storedPending("A"), []);
    s.stop();
  });

  // 감사 28 감독 결정: 배포 창 동안 옛 코드의 탭(번호 없는 배열)과 새 코드의 탭(번호 글)이 같은 상담에 쓰면, 번호 없는 글이 먼저 나간다 —
  // 나중에 썼어도 그렇다. 옛 글과 새 글의 시각(at)은 서로 다른 탭의 시계라 견줄 수 없다. 배포 창 동안만이고 글은 하나도 잃지 않는다(D5 대가).
  test("배포 중 옛 코드의 탭이 쓴 번호 없는 글은 새 코드가 쓴 번호 글보다 먼저 나간다 — 나중에 썼어도 그렇다", () => {
    store.set(`${QUEUE}A|new-1`, JSON.stringify({ client_msg_id: "new-1", body: "새 탭이 먼저 쓴 글", at: 1000, n: 1, tries: 0 }));
    store.set(`${QUEUE}A|new-2`, JSON.stringify({ client_msg_id: "new-2", body: "새 탭이 다음에 쓴 글", at: 1500, n: 2, tries: 0 }));
    store.set(`${STORE}A`, JSON.stringify({ lastSeq: 0, pending: [{ client_msg_id: "old-1", body: "옛 탭이 나중에 쓴 글", at: 2000, tries: 0 }] }));
    const want = ["옛 탭이 나중에 쓴 글", "새 탭이 먼저 쓴 글", "새 탭이 다음에 쓴 글"];
    for (const order of [(keys) => keys, reversed]) {
      withKeyOrder(order, () => {
        const { s, shown } = newSession();
        s.open("A");
        assert.deepEqual(shown.pending.at(-1), want, "번호 없는 옛 글이 번호 글 뒤로 갔다");
        sentFrames.length = 0;
        s.handleFrame({ type: "ready", conversation_id: "A", last_seq: 0, reads: [] });
        assert.deepEqual(framesOfType("send").map((f) => f.body), want);
        s.stop();
      });
    }
  });

  // 감사 28 A5: 커서 정리가 "전송 대기 글이 있는 대화" 를 글 이름 공간(gcs.q1.)에서도 찾는다. 안 보면 글이 있는 대화의 커서를 지워
  // 그 대화를 다시 열 때 받은 데까지가 아니라 처음부터 다시 받는다.
  test("용량이 차서 커서를 정리해도 전송 대기 글이 있는 대화의 커서는 지우지 않는다 — 다시 열면 받은 데까지 이어 받는다", async () => {
    store.set(`${STORE}B`, JSON.stringify({ lastSeq: 7 }));
    store.set(`${QUEUE}B|b-1`, JSON.stringify({ client_msg_id: "b-1", body: "B 에 쓴 글", at: 1, n: 1, tries: 0 }));
    for (let i = 0; i < 5; i++) localStorage.setItem(`${STORE}old${i}`, JSON.stringify({ lastSeq: 3 }));
    const real = localStorage.setItem;
    let failed = false;
    localStorage.setItem = (k, v) => {
      if (!failed && k.startsWith(`${QUEUE}A|`)) {
        failed = true;
        throw new Error("QuotaExceededError");
      }
      return real(k, v);
    };
    try {
      const { s } = newSession();
      s.open("A");
      s.send("A 에 쓴 글");
      s.stop();
    } finally {
      localStorage.setItem = real;
    }
    assert.equal(localStorage.getItem(`${STORE}old0`), null, "정리가 돌지 않았다(흉내가 틀렸다)");
    assert.equal(storedCursor("B"), 7, "전송 대기 글이 있는 대화의 커서를 지웠다");
    const { s } = newSession();
    await new Promise((r) => queueMicrotask(r));
    sentFrames.length = 0;
    s.open("B");
    assert.equal(framesOfType("subscribe").at(-1)?.after_seq, 7, "받은 데까지가 아니라 처음부터 다시 받는다");
    s.stop();
  });

  test("주인이 없던 시절의 큐를 옮기다 저장이 막히면 옛 큐를 지우지 않는다 — 저장이 되면 다음에 열 때 옮겨 보낸다", () => {
    localStorage.setItem(STORE + "A", JSON.stringify({ lastSeq: 0, pending: [{ client_msg_id: "noowner-1", body: "주인 없는 글", at: 1000, tries: 0 }] }));
    const owner = "agent:A";
    const real = localStorage.setItem;
    localStorage.setItem = (k, v) => {
      if (k.includes("|")) throw new Error("QuotaExceededError");
      return real(k, v);
    };
    try {
      const s = new ChatSession({ token: "tok", owner });
      s.open("A");
      assert.deepEqual(s.state.pending.map((p) => p.body), ["주인 없는 글"], "옮기지 못해도 이 탭은 보낸다");
      s.stop();
    } finally {
      localStorage.setItem = real;
    }
    assert.deepEqual(storedPending("A").map((p) => p.body), ["주인 없는 글"], "옮기지 못했는데 옛 큐를 지웠다");

    const s = new ChatSession({ token: "tok", owner });
    s.connect();
    s.open("A");
    sentFrames.length = 0;
    s.handleFrame({ type: "ready", conversation_id: "A", last_seq: 0, reads: [] });
    assert.deepEqual(framesOfType("send").map((f) => f.client_msg_id), ["noowner-1"]);
    assert.deepEqual(storedPendingAt(`${STORE}${owner}.`, "A").map((p) => p.body), ["주인 없는 글"], "주인 접두사 아래로 옮기지 않았다");
    assert.deepEqual(storedPending("A"), [], "옮긴 뒤 옛 큐를 지우지 않았다");
    s.stop();
  });

  // ── 저장이 막힌 탭의 메모리 큐(DEF-175) ─────────────────────────
  // 쓰기가 막힌 브라우저(QuotaExceeded, Safari 사생활 보호)에서 저장하지 못한 글은 이 탭의 메모리에만 있다.
  // 전에는 open·close 가 큐를 저장소에서 다시 읽어 다른 대화에 다녀오면 그 글이 사라졌다(f28run/blocked.mjs: 1개 → 0개).

  // blockWrites 는 저장소 쓰기를 막는다(읽기·지우기는 된다). 돌려준 함수로 푼다.
  function blockWrites() {
    const real = localStorage.setItem;
    localStorage.setItem = () => { throw new Error("QuotaExceededError"); };
    return () => { localStorage.setItem = real; };
  }
  // memoryServer 는 멱등 키로 한 번만 저장하는 서버다. 켜기(online) 전에는 응답이 없다(끊김).
  function memoryServer() {
    const saved = new Map();
    const posts = [];
    let up = false;
    globalThis.fetch = async (path, opts) => {
      if (opts?.method !== "POST" || !path.endsWith("/messages")) return respond(200, { messages: [] }); // 읽음 보고 등
      if (!up) throw new TypeError("Failed to fetch");
      const conv = path.split("/")[3];
      const b = JSON.parse(opts.body);
      posts.push([conv, b.body]);
      if (!saved.has(b.client_msg_id)) saved.set(b.client_msg_id, [conv, b.body]);
      return respond(201, { message: { ...msg(conv, saved.size, b.body), client_msg_id: b.client_msg_id }, created: !saved.has(b.client_msg_id) });
    };
    return { saved, posts, online: () => { up = true; } };
  }
  // offlineSession 은 소켓 없이(끊긴 채) 시작한 세션이다. 글은 REST 다시 밀기(flushViaRest)로만 나간다.
  function offlineSession(owner = "") {
    const shown = { errors: [], pending: [] };
    const s = new ChatSession({
      token: "tok",
      owner,
      handlers: { onError: (e) => shown.errors.push(e), onPending: (list) => shown.pending.push(list.map((p) => p.body)) },
    });
    return { s, shown };
  }

  test("저장하지 못한 글은 같은 탭에서 다른 대화를 여러 번 오가도 남고 연결되면 서버에 한 번 저장된다 — 알림은 이 탭을 닫거나 새로고침하거나 로그아웃하면 사라진다고 말한다", async () => {
    const server = memoryServer();
    const unblock = blockWrites();
    try {
      const { s, shown } = offlineSession();
      s.open("A");
      s.send("A 에 쓴 글");
      assert.deepEqual(shown.errors, ["이 브라우저는 전송 대기를 저장하지 못합니다. 이 탭을 닫거나 새로고침하거나 로그아웃하면 보내지 못한 메시지가 사라집니다"]);
      for (let i = 0; i < 3; i++) {
        s.open("B");
        assert.deepEqual(shown.pending.at(-1), [], "다른 대화에 A 의 글이 섞였다");
        s.open("A");
        assert.deepEqual(shown.pending.at(-1), ["A 에 쓴 글"], `${i + 1}번째로 돌아왔는데 저장하지 못한 글이 사라졌다`);
      }
      s.close();
      s.open("A");
      assert.deepEqual(shown.pending.at(-1), ["A 에 쓴 글"], "대화를 닫았다 열었는데 저장하지 못한 글이 사라졌다");
      await s.flushViaRest(); // 아직 끊겨 있다
      assert.deepEqual(s.pendingOf("A").map((p) => p.body), ["A 에 쓴 글"], "끊긴 동안 글을 버렸다");

      server.online();
      await s.flushViaRest();
      assert.deepEqual([...server.saved.values()], [["A", "A 에 쓴 글"]], "서버에 한 번 저장되지 않았다");
      assert.deepEqual(shown.pending.at(-1), [], "저장됐는데 큐에 남았다");
      await s.flushViaRest();
      s.open("B");
      s.open("A");
      await s.flushViaRest();
      assert.deepEqual(server.posts, [["A", "A 에 쓴 글"]], "저장된 글을 다시 보냈다");
      s.stop();
    } finally {
      unblock();
    }
  });

  test("두 대화 모두 저장하지 못한 글이 있으면 보지 않는 대화의 글도 연결되면 보내고 다른 탭의 저장소 변경을 맞출 때 어느 대화의 글도 지우지 않는다", async () => {
    const server = memoryServer();
    const [one, two] = twoTabs();
    await sleep(0); // 소켓이 열렸다
    two.s.ws.readyState = WebSocket.CLOSED; // 이 탭은 끊겼다. 글은 REST 로만 나간다
    let unblock = blockWrites(); // 이 탭은 저장이 막혔다
    two.s.open("A");
    two.s.send("A 에 쓴 글");
    two.s.open("B");
    two.s.send("B 에 쓴 글");
    unblock();

    // 다른 탭이 두 대화에 쓰고 ack 를 받아 지운다. 저장소 이벤트가 이 탭에 온다 — 보고 있는 대화(B)와 다른 대화(A), 전부 지우기(key 없음)
    for (const conv of ["A", "B"]) {
      one.s.open(conv);
      const a = one.s.send(`다른 탭이 ${conv} 에 쓴 글`);
      two.storage(keyOf(conv, a));
      one.s.handleFrame({ type: "ack", conversation_id: conv, client_msg_id: a.client_msg_id });
      two.storage(keyOf(conv, a));
    }
    two.storage(null);
    assert.deepEqual(two.shown.pending.at(-1), ["B 에 쓴 글"], "보고 있는 대화의 저장하지 못한 글을 맞추며 지웠다");
    unblock = blockWrites();
    try {
      two.s.open("A");
      assert.deepEqual(two.shown.pending.at(-1), ["A 에 쓴 글"], "다른 대화의 저장소 변경을 맞추며 A 의 저장하지 못한 글을 지웠다");
      two.s.open("B");
      two.s.open("A");

      server.online();
      await two.s.flushViaRest();
      assert.deepEqual([...server.saved.values()].sort(), [["A", "A 에 쓴 글"], ["B", "B 에 쓴 글"]], "보지 않는 대화(B)의 글이 나가지 않았다");
      assert.deepEqual(server.posts.slice().sort(), [["A", "A 에 쓴 글"], ["B", "B 에 쓴 글"]], "한 번씩만 나가야 한다");
      assert.deepEqual(two.shown.pending.at(-1), []);
      two.s.open("B");
      assert.deepEqual(two.shown.pending.at(-1), [], "저장된 B 의 글이 큐에 남았다");
    } finally {
      unblock();
      one.s.stop();
      two.s.stop();
    }
  });

  test("로그아웃은 이 탭이 저장하지 못한 글도 버려 다시 시작해도 어느 대화로도 보내지 않는다 — 멈추기만 하면 남아 다시 시작할 때 보낸다", async () => {
    const unblock = blockWrites();
    try {
      for (const [how, want] of [["stop", [["A", "A 에 쓴 글"], ["B", "B 에 쓴 글"]]], ["discard", []]]) {
        const server = memoryServer();
        const { s } = offlineSession("customer:c1");
        s.open("A");
        s.send("A 에 쓴 글");
        s.open("B");
        s.send("B 에 쓴 글");
        s.close(); // 두 대화 모두 보지 않는다 — REST 로만 나간다
        s[how](); // 로그인 만료(stop)와 로그아웃(discard)
        server.online();
        s.start(); // 처음 시작은 기다림 없이 모든 대화를 민다
        await sleep(20);
        s.stop();
        assert.deepEqual(server.posts.slice().sort(), want, `${how}: 보낸 글이 다르다`);
      }
    } finally {
      unblock();
    }
  });

  test("주인이 없던 시절의 큐를 옮기지 못해 메모리에도 둔 글은 대화를 오가도 한 번만 보이고 한 번만 나간다", async () => {
    localStorage.setItem(STORE + "A", JSON.stringify({ lastSeq: 0, pending: [{ client_msg_id: "noowner-1", body: "주인 없는 글", at: 1000, tries: 0 }] }));
    const server = memoryServer();
    const unblock = blockWrites();
    try {
      const { s, shown } = offlineSession("agent:A");
      s.open("A");
      s.open("B");
      s.open("A");
      assert.deepEqual(shown.pending.at(-1), ["주인 없는 글"], "저장소의 옛 큐와 메모리의 같은 글이 두 번 보인다");
      assert.equal(s.pendingOf("A").length, 1);
      s.open("B");
      server.online();
      await s.flushViaRest();
      s.stop();
      assert.deepEqual(server.posts, [["A", "주인 없는 글"]], "같은 글을 두 번 보냈다");
    } finally {
      unblock();
    }
  });
  // 감사 29 a29 mix: 쓰기가 일부만 막히면 한 대화에 저장한 글과 저장하지 못한 글이 섞인다 — 용량이 거의 찬 저장소는 긴 글만 거절한다.
  // 저장하지 못한 긴 글은 저장소로 옮기려 해도(DEF-177) 계속 못 옮긴다. 대화를 열 때 둘을 합쳐 번호(n)로 다시 세우지 않으면
  // 저장한 글이 먼저 나가 쓴 순서가 뒤집힌다(감사 29 J6 생존).
  test("저장한 글과 저장하지 못한 글이 한 대화에 섞이면 다른 대화에 다녀와도 쓴 순서로 보이고 쓴 순서로 서버에 저장된다", async () => {
    const server = memoryServer();
    const real = localStorage.setItem;
    localStorage.setItem = (k, v) => {
      if (k.startsWith(QUEUE) && String(v).length > 500) throw new Error("QuotaExceededError"); // 긴 글만 들어가지 않는다
      return real(k, v);
    };
    const long = (name) => `${name} ${"긴 글".repeat(200)}`;
    const head = (list) => list.map((b) => b.split(" ")[0]);
    const { s, shown } = offlineSession();
    try {
      s.open("A");
      for (const body of ["m1", long("m2"), "m3", long("m4"), long("m5"), "m6"]) s.send(body);
      assert.deepEqual(head(storedPending("A").map((p) => p.body)), ["m1", "m3", "m6"], "섞이지 않았다(흉내가 틀렸다)");
      s.open("B");
      s.open("A");
      assert.deepEqual(head(shown.pending.at(-1)), ["m1", "m2", "m3", "m4", "m5", "m6"], "다른 대화에 다녀오니 쓴 순서가 아니다");
      server.online();
      await s.flushViaRest();
      assert.deepEqual(head(server.posts.map(([, b]) => b)), ["m1", "m2", "m3", "m4", "m5", "m6"], "쓴 순서로 나가지 않았다");
    } finally {
      localStorage.setItem = real;
      s.stop();
    }
  });

  // 감사 29 a29 recover: 저장이 다시 돼도 메모리 글은 저장소로 옮기지 않았다. 저장이 돌아온 뒤 새로고침하면 그 글만 사라졌다(서버에도 없다).
  // 저장에 성공하는 쓰기가 생기면(send) 곧바로, 아니면 주기 확인(flushViaRest)에서 옮긴다. 옮긴 글만 메모리에서 뺀다.
  test("저장이 다시 되면 저장하지 못한 글을 저장소로 옮겨 새로고침해도 남는다 — 쓴 순서이고 서버에는 한 번만 저장된다", async () => {
    const server = memoryServer();
    let unblock = blockWrites();
    const { s } = offlineSession();
    s.open("A");
    s.send("r1");
    s.open("B");
    s.send("b1");
    unblock();
    s.open("A");
    s.send("r2"); // 저장이 된다. 앞서 저장하지 못한 글도 지금 옮긴다
    assert.deepEqual(storedPending("A").map((p) => p.body), ["r1", "r2"], "저장이 됐는데 저장하지 못한 글을 저장소로 옮기지 않았다");
    assert.deepEqual(storedPending("B").map((p) => p.body), ["b1"], "보지 않는 대화의 저장하지 못한 글을 옮기지 않았다");

    // 쓰기 없이 저장만 다시 된 경우는 주기 확인이 옮긴다(아직 끊겨 있다)
    unblock = blockWrites();
    s.send("r3");
    unblock();
    await s.flushViaRest();
    assert.deepEqual(storedPending("A").map((p) => p.body), ["r1", "r2", "r3"], "주기 확인이 저장하지 못한 글을 옮기지 않았다");
    s.stop(); // 새로고침 — 이 탭의 메모리는 사라진다

    const again = offlineSession();
    again.s.open("A");
    assert.deepEqual(again.shown.pending.at(-1), ["r1", "r2", "r3"], "새로고침 뒤 저장하지 못했던 글이 사라졌다");
    server.online();
    await again.s.flushViaRest();
    await again.s.flushViaRest();
    assert.deepEqual(server.posts.slice().sort(), [["A", "r1"], ["A", "r2"], ["A", "r3"], ["B", "b1"]], "한 번씩 나가야 한다");
    assert.deepEqual(server.posts.filter(([c]) => c === "A").map(([, b]) => b), ["r1", "r2", "r3"], "쓴 순서로 나가야 한다");
    again.s.stop();
  });

  // 데스크는 로그인이 만료되면 세션을 버리고 다시 로그인할 때 새로 만든다. 저장하지 못한 글은 화면이 들고 있다가 같은 주인의 새 세션에 넘긴다(DEF-176).
  test("멈춘 세션의 저장하지 못한 글을 같은 주인의 새 세션에 넘기면 연결될 때 한 번 보낸다", async () => {
    const server = memoryServer();
    const unblock = blockWrites();
    try {
      const old = offlineSession("agent:a1");
      old.s.open("A");
      old.s.send("만료 전에 쓴 답");
      old.s.stop();
      server.online();
      const next = new ChatSession({ token: "tok2", owner: "agent:a1", unsaved: old.s.unsaved });
      next.start();
      await sleep(20);
      next.stop();
      assert.deepEqual(server.posts, [["A", "만료 전에 쓴 답"]]);
    } finally {
      unblock();
    }
  });
});
