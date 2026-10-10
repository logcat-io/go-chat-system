// 위젯 부트의 재시도 배선 테스트.
//
// widget.js 를 그대로 불러온다. 화면은 무엇이든 받아 주는 가짜 요소로 채우고, 서버(fetch)와 시계(setTimeout)만 가짜로 둔다.
// 단언은 바깥에서 보이는 결과다 — 세션 요청이 언제 다시 나가는가.
import { test, mock } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

// 브라우저는 "/chat.js"·"/ui.js"·"/widget-view.js" 를 서버 뿌리에서 받는다. node 에서는 같은 폴더의 파일로 잇는다.
register(
  "data:text/javascript," +
    encodeURIComponent(`export async function resolve(spec, ctx, next) {
      if (spec === "/chat.js" || spec === "/ui.js" || spec === "/widget-view.js") return next(new URL("." + spec, ctx.parentURL).href, ctx);
      return next(spec, ctx);
    }`),
);

// anything 은 어떤 속성을 읽어도, 불러도 또 가짜 요소를 돌려준다. 값을 쓰면 기억한다.
// then 은 없다 — 있으면 await 가 영영 풀리지 않는다. 순회하면 비어 있다.
function anything() {
  const store = {};
  const target = function () {};
  return new Proxy(target, {
    get(_, key) {
      if (key in store) return store[key];
      if (key === "then") return undefined;
      if (key === Symbol.iterator) return function* () {};
      if (key === Symbol.toPrimitive) return () => "";
      if (typeof key === "symbol") return undefined;
      return (store[key] = anything());
    },
    set(_, key, value) {
      store[key] = value;
      return true;
    },
    apply() {
      return anything();
    },
  });
}

test("요청이 잦아 세션을 못 열면 Retry-After 초 뒤에 세션을 다시 요청한다", async () => {
  mock.timers.enable({ apis: ["setTimeout", "setInterval"] });

  const sessionCalls = [];
  globalThis.fetch = async (path, init) => {
    if (path === "/api/customer/session") {
      sessionCalls.push(init?.method);
      return new Response(JSON.stringify({ error: { code: "too_many", message: "요청이 너무 잦다" } }), {
        status: 429,
        headers: { "Content-Type": "application/json", "Retry-After": "2" },
      });
    }
    return new Response(JSON.stringify({}), { status: 200, headers: { "Content-Type": "application/json" } });
  };
  globalThis.location = { search: "", origin: "http://test.local", protocol: "http:", host: "test.local" };
  globalThis.document = anything();
  document.referrer = "";
  globalThis.window = anything();
  const saved = new Map();
  globalThis.localStorage = {
    getItem: (k) => saved.get(k) ?? null,
    setItem: (k, v) => saved.set(k, String(v)),
    removeItem: (k) => saved.delete(k),
  };

  await import("./widget.js");
  const settle = async () => {
    for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r));
  };
  await settle();
  assert.equal(sessionCalls.length, 1, "부트가 세션을 한 번 요청한다");

  mock.timers.tick(1999);
  await settle();
  assert.equal(sessionCalls.length, 1, "Retry-After(2초)가 지나기 전에는 다시 요청하지 않는다");

  mock.timers.tick(1);
  await settle();
  assert.equal(sessionCalls.length, 2, "Retry-After(2초)가 지나면 세션을 다시 요청한다");
  assert.equal(sessionCalls[1], "POST");

  mock.timers.reset();
});
