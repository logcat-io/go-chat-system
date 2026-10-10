// 임베드 스크립트 테스트.
//
// embed.js 는 고객 사이트에 <script> 로 붙는 일반 스크립트다. 모듈이 아니라서 vm 으로 그대로 돌린다.
// 문서·창은 쓰는 만큼만 흉내 내고, 단언은 바깥에서 보이는 결과다 — iframe 주소와 위젯에 보내는 boot 설정.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const SOURCE = readFileSync(new URL("./embed.js", import.meta.url), "utf8");

// mountEmbed 는 dataset 을 단 <script> 아래에서 embed.js 를 돌리고, 붙은 iframe 과 위젯에 보낸 메시지를 돌려준다.
// plain 은 vm 안에서 만든 객체를 이 realm 의 평범한 객체로 옮긴다(프로토타입이 달라 deepEqual 이 엄격 비교에서 다르다고 본다).
const plain = (v) => JSON.parse(JSON.stringify(v));

function mountEmbed(dataset) {
  const listeners = {};
  const posted = [];
  const frames = [];
  const document = {
    currentScript: { src: "https://chat.example.com/embed.js", dataset },
    body: { appendChild: (el) => frames.push(el) },
    documentElement: { appendChild: (el) => frames.push(el) },
    addEventListener() {},
    createElement: () => ({
      style: {},
      setAttribute() {},
      contentWindow: { postMessage: (msg, origin) => posted.push({ msg, origin }) },
    }),
  };
  const window = {
    innerWidth: 1280,
    innerHeight: 800,
    addEventListener: (type, fn) => (listeners[type] = fn),
  };
  window.window = window;
  vm.runInNewContext(SOURCE, { window, document, location: { href: "https://shop.example.com/page" }, URL, Promise });
  return {
    window,
    frames,
    posted,
    // ready 는 위젯이 떴다고 알리는 메시지다. 이걸 받아야 boot 설정을 보낸다. 위젯의 메시지는 우리 iframe 의 창(contentWindow)에서 온다.
    ready: () => listeners.message({ source: frames[0].contentWindow, origin: "https://chat.example.com", data: { source: "go-chat", type: "ready" } }),
    fromWidget: (data) => listeners.message({ source: frames[0].contentWindow, origin: "https://chat.example.com", data: { source: "go-chat", ...data } }),
    // from 은 아무 창·출처에서 온 메시지다(공격자의 다른 창).
    from: (source, origin, data) => listeners.message({ source, origin, data: { source: "go-chat", ...data } }),
  };
}

test("옛 설치 코드의 data-key 는 iframe 주소에도 위젯 boot 설정에도 싣지 않는다", () => {
  // 설치형 단일 워크스페이스라 위젯 키가 없다(D35). 옛 설치 코드에 남은 data-key 는 읽지 않고 그대로 뜬다.
  const embed = mountEmbed({ key: "wk_옛설치코드의키", color: "#0c64e8", position: "left" });
  assert.equal(embed.frames.length, 1, "iframe 하나를 붙인다");
  assert.equal(embed.frames[0].src, "https://chat.example.com/widget?embed=1");
  embed.ready();
  const boot = embed.posted.find((p) => p.msg.type === "boot");
  assert.ok(boot, "ready 를 받으면 boot 설정을 보낸다");
  assert.equal(boot.origin, "https://chat.example.com");
  assert.equal(boot.msg.config.color, "#0c64e8", "다른 설정은 그대로 넘긴다");
  assert.equal(boot.msg.config.position, "left");
  assert.ok(!("key" in boot.msg.config), "boot 설정에 키가 없다");
  assert.doesNotMatch(JSON.stringify(boot.msg), /wk_/);
});

test("data-key 가 없는 설치 코드도 같은 주소로 뜬다", () => {
  const embed = mountEmbed({});
  assert.equal(embed.frames[0].src, "https://chat.example.com/widget?embed=1");
});

test("ChatWidget.logout() 은 회원 토큰을 잊고 위젯에 로그아웃을 보낸다(D46 공유 PC)", () => {
  const embed = mountEmbed({});
  const window = embed.window;
  embed.ready();
  window.ChatWidget.identify("signed-member");
  window.ChatWidget.logout();
  const types = embed.posted.map((p) => p.msg.type);
  assert.deepEqual(types.slice(-2), ["identify", "logout"]);
  assert.ok(embed.posted.every((p) => p.origin === "https://chat.example.com"), "위젯 출처에만 보낸다");
  // 그 뒤 boot 를 다시 불러도 회원 토큰을 싣지 않는다 — 로그아웃한 회원으로 다시 들어가지 않는다.
  window.ChatWidget.boot({});
  assert.equal(embed.posted.at(-1).msg.config.memberToken, "");
});

// 감사 20(logcat 결정): 회원 토큰 exp 를 짧게 주는 호스트는 getMemberToken 으로 새 회원 토큰을 준다. 위젯이 묻고 embed.js 가 답한다.
test("ChatWidget.boot 의 getMemberToken 은 위젯이 물을 때 불리고 그 토큰을 위젯에 돌려준다 — 함수는 위젯에 넘기지 않는다", async () => {
  const embed = mountEmbed({});
  const window = embed.window;
  let calls = 0;
  window.ChatWidget.boot({ getMemberToken: async () => `signed-new-${++calls}` });
  embed.ready();
  const boot = embed.posted.filter((p) => p.msg.type === "boot").at(-1);
  assert.equal(boot.msg.config.memberRefresh, true, "위젯이 새 토큰을 물어도 되는지 모른다");
  assert.ok(!Object.values(boot.msg.config).some((v) => typeof v === "function"), "함수는 postMessage 로 넘길 수 없다(DataCloneError)");
  assert.doesNotThrow(() => structuredClone(boot.msg), "boot 메시지를 복제할 수 없다");
  embed.fromWidget({ type: "member-token-request", id: 7 });
  await new Promise((r) => setImmediate(r));
  assert.equal(calls, 1);
  assert.deepEqual(plain(embed.posted.at(-1).msg), { source: "go-chat-host", type: "member-token", id: 7, memberToken: "signed-new-1" });
  // 다음 boot 에는 새 회원 토큰이 실린다(위젯을 다시 띄워도 새 토큰으로 들어온다)
  window.ChatWidget.boot({});
  assert.equal(embed.posted.at(-1).msg.config.memberToken, "signed-new-1");
});

test("getMemberToken 이 없거나 실패하면 빈 토큰으로 답한다 — 위젯은 다시 시도하지 않고 새로고침 안내를 띄운다", async () => {
  const embed = mountEmbed({});
  embed.ready();
  assert.equal(embed.posted.find((p) => p.msg.type === "boot").msg.config.memberRefresh, false);
  embed.fromWidget({ type: "member-token-request", id: 1 });
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(plain(embed.posted.at(-1).msg), { source: "go-chat-host", type: "member-token", id: 1, memberToken: "" });
  embed.window.ChatWidget.boot({ getMemberToken: async () => { throw new Error("로그인 안 됨"); } });
  embed.fromWidget({ type: "member-token-request", id: 2 });
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(plain(embed.posted.at(-1).msg), { source: "go-chat-host", type: "member-token", id: 2, memberToken: "" });
});

// 감사 21(J1·J11): 출처·창 검사와 동기 예외 처리를 지우는 변이가 살아남았다.
test("우리 iframe 이 아닌 창이나 다른 출처의 메시지는 버린다 — getMemberToken 을 부르지 않는다", async () => {
  const embed = mountEmbed({});
  let calls = 0;
  embed.window.ChatWidget.boot({ getMemberToken: async () => `signed-${++calls}` });
  embed.ready();
  const before = embed.posted.length;
  const evilWindow = { postMessage() {} };
  embed.from(evilWindow, "https://evil.example.com", { type: "member-token-request", id: 1 }); // 다른 사이트
  embed.from(evilWindow, "https://chat.example.com", { type: "member-token-request", id: 2 }); // 같은 출처의 다른 창(공격자가 띄운 /widget)
  embed.from(embed.frames[0].contentWindow, "https://evil.example.com", { type: "member-token-request", id: 3 }); // 출처가 다르다
  embed.from(evilWindow, "https://chat.example.com", { type: "ready" });
  await new Promise((r) => setImmediate(r));
  assert.equal(calls, 0, "다른 창·출처의 요청으로 호스트의 getMemberToken 을 불렀다");
  assert.equal(embed.posted.length, before, "다른 창·출처의 메시지에 답했다");
  embed.fromWidget({ type: "member-token-request", id: 4 });
  await new Promise((r) => setImmediate(r));
  assert.equal(calls, 1, "우리 iframe 의 요청에는 답한다");
});

test("getMemberToken 이 동기로 던져도 빈 토큰으로 답하고 호스트 페이지에 예외가 새지 않는다", async () => {
  const embed = mountEmbed({});
  embed.window.ChatWidget.boot({ getMemberToken: () => { throw new Error("로그인 안 됨"); } });
  embed.ready();
  assert.doesNotThrow(() => embed.fromWidget({ type: "member-token-request", id: 9 }));
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(plain(embed.posted.at(-1).msg), { source: "go-chat-host", type: "member-token", id: 9, memberToken: "" });
});
