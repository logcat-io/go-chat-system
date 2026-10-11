/* go-chat-system 임베드 스크립트.
 *
 * 우리 사이트 페이지에 한 줄로 붙인다. 설정 > 위젯·설치 에서 복사한다.
 *   <script src="https://chat.example.com/embed.js"></script>
 *
 * 색은 관리자가 데스크에서 정한 색이다. data-color="#c2410c" 처럼 주면 그 색이 관리자 색을 덮는다(옛 설치 코드 호환).
 * data-position="left" 면 왼쪽 아래에 붙는다.
 *
 * 설치형 단일 워크스페이스라 위젯 키가 없다(D35). 옛 설치 코드에 data-key 가 남아 있어도 읽지 않고 그대로 뜬다.
 *
 * 로그인 회원을 넘기려면 서버가 서명한 토큰을 준다.
 *   <script>ChatWidget.identify("<member token>")</script>
 *
 * 회원 토큰의 exp 를 짧게 주면(예: 1시간) 위젯 세션도 그때 끝난다. 열어 둔 위젯이 끊기지 않게 새 회원 토큰을 받는 함수를 준다.
 * 위젯이 회원 세션이 끝났다고 알리면 이 함수를 부르고, 돌려준 토큰으로 다시 identify 한다. 함수가 없거나 실패하면(빈 값·예외·10초 넘김)
 * 위젯은 다시 시도하지 않고 "로그인이 만료되었습니다. 페이지를 새로고침해 주세요" 를 보인다. 보내지 못한 글은 위젯 저장소에 남는다.
 *   <script>ChatWidget.boot({ getMemberToken: async () => (await fetch("/my/chat-member-token")).text() })</script>
 *
 * 회원이 우리 사이트에서 로그아웃할 때 위젯도 로그아웃한다. 부르지 않으면 같은 브라우저를 쓰는 다음 사람에게 회원의 상담이 보일 수 있다
 * (서버는 익명 방문자에게 회원 세션을 이어 주지 않지만, 위젯 저장소의 보내지 못한 글은 이걸로만 지운다).
 *   <script>ChatWidget.logout()</script>
 *
 * 위젯은 iframe 안에서 돈다. 호스트 페이지의 CSS·스크립트와 섞이지 않고,
 * 호스트 페이지는 우리 토큰을 읽을 수 없다. 둘 사이는 postMessage 로만 말한다.
 */
(function () {
  "use strict";
  if (window.ChatWidget) return;

  var script = document.currentScript;
  var origin = new URL(script.src, location.href).origin;
  var config = {
    color: script.dataset.color || "",
    position: script.dataset.position || "",
    memberToken: "",
  };

  // memberTokenSource 는 호스트가 준 getMemberToken 이다. 함수는 postMessage 로 넘길 수 없어(구조화 복제) config 에 싣지 않고 여기 둔다.
  var memberTokenSource = null;
  var frame = null;
  var ready = false;
  var queued = [];
  var LAUNCHER = 76;
  var OPEN_W = 400;
  var OPEN_H = 720;

  function post(msg) {
    if (!frame) mount();
    if (!ready) {
      queued.push(msg);
      return;
    }
    frame.contentWindow.postMessage(Object.assign({ source: "go-chat-host" }, msg), origin);
  }

  function side() {
    return config.position === "left" ? "left" : "right";
  }

  function mount() {
    if (frame) return;
    frame = document.createElement("iframe");
    frame.src = origin + "/widget?embed=1";
    frame.title = "상담";
    frame.setAttribute("allow", "clipboard-write");
    var s = frame.style;
    s.position = "fixed";
    s.bottom = "0";
    s[side()] = "0";
    s.width = LAUNCHER + "px";
    s.height = LAUNCHER + "px";
    s.border = "0";
    s.background = "transparent";
    s.colorScheme = "normal";
    s.zIndex = "2147483000";
    s.transition = "width .18s ease, height .18s ease";
    (document.body || document.documentElement).appendChild(frame);
  }

  function resize(open) {
    if (!frame) return;
    var mobile = window.innerWidth < 480;
    if (!open) {
      frame.style.width = LAUNCHER + "px";
      frame.style.height = LAUNCHER + "px";
      return;
    }
    if (mobile) {
      frame.style.width = "100vw";
      frame.style.height = "100vh";
      return;
    }
    frame.style.width = Math.min(OPEN_W, window.innerWidth) + "px";
    frame.style.height = Math.min(OPEN_H, window.innerHeight) + "px";
  }

  // 우리 iframe 이 보낸 것만 받는다. 출처(origin)만 보면 같은 출처의 다른 창(공격자가 띄운 /widget 팝업·다른 iframe)이
  // member-token-request 로 호스트의 getMemberToken 을 불러 회원 토큰을 받아 갈 수 있다 — 답은 우리 iframe 으로만 가지만 호출 자체를 막는다.
  window.addEventListener("message", function (e) {
    if (e.origin !== origin || !frame || e.source !== frame.contentWindow || !e.data || e.data.source !== "go-chat") return;
    var msg = e.data;
    switch (msg.type) {
      case "ready":
        ready = true;
        post({ type: "boot", config: bootConfig() });
        queued.splice(0).forEach(post);
        break;
      case "size":
        resize(!!msg.open);
        break;
      case "member-token-request":
        answerMemberToken(msg.id);
        break;
      case "unread":
        if (typeof window.ChatWidget.onUnread === "function") window.ChatWidget.onUnread(msg.count);
        break;
    }
  });

  // answerMemberToken 은 위젯의 "회원 세션이 끝났다, 새 회원 토큰을 달라" 에 답한다. 함수가 없거나 실패하면 빈 값으로 답한다 —
  // 위젯은 빈 값을 받으면 다시 시도하지 않고 새로고침 안내를 보인다(무한 재시도를 막는다).
  function answerMemberToken(id) {
    var reply = function (token) {
      token = typeof token === "string" ? token.trim() : "";
      if (token) config.memberToken = token;
      post({ type: "member-token", id: id, memberToken: token });
    };
    if (typeof memberTokenSource !== "function") return reply("");
    var pending;
    try {
      pending = Promise.resolve(memberTokenSource());
    } catch (e) {
      return reply("");
    }
    pending.then(reply, function () {
      reply("");
    });
  }

  // bootConfig 는 위젯에 보내는 설정이다. 함수(getMemberToken)는 빼고 "새 토큰을 줄 수 있다" 만 싣는다.
  function bootConfig() {
    return Object.assign({}, config, { memberRefresh: typeof memberTokenSource === "function" });
  }

  window.addEventListener("resize", function () {
    if (frame && frame.style.width !== LAUNCHER + "px") resize(true);
  });

  window.ChatWidget = {
    /** 설정을 바꾸고 위젯을 올린다. 자동으로 한 번 불리므로 보통은 부를 일이 없다.
     *  getMemberToken: 새 회원 토큰(문자열)을 돌려주는 함수(async 가능). 회원 세션이 끝나면 위젯이 이걸로 다시 들어온다. */
    boot: function (opts) {
      opts = Object.assign({}, opts || {});
      if ("getMemberToken" in opts) {
        memberTokenSource = typeof opts.getMemberToken === "function" ? opts.getMemberToken : null;
        delete opts.getMemberToken;
      }
      Object.assign(config, opts);
      mount();
      if (ready) post({ type: "boot", config: bootConfig() });
    },
    /** 서명된 회원 토큰을 넘긴다. 위젯이 그 회원의 대화 이력으로 바뀐다. */
    identify: function (memberToken) {
      config.memberToken = memberToken || "";
      post({ type: "identify", memberToken: config.memberToken });
    },
    /** 회원 로그아웃. 위젯이 서버 세션을 끊고 위젯 저장소(토큰·보내지 못한 글)를 비운 뒤 익명으로 돌아간다. */
    logout: function () {
      config.memberToken = "";
      post({ type: "logout" });
    },
    open: function () {
      post({ type: "open" });
    },
    close: function () {
      post({ type: "close" });
    },
    onUnread: null,
  };

  if (script.dataset.auto !== "false") {
    if (document.body) mount();
    else document.addEventListener("DOMContentLoaded", mount);
  }
})();
