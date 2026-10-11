// 화면 문구와 고르기 정책 테스트.
//
// ui.js 의 순수 함수만 부른다. 단언은 사람이 보는 결과다 — 첫 줄에 오는 빠른 답변, 화면에 쓰이는 문장.
import { test, describe, mock } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";
import { readFileSync } from "node:fs";

// 브라우저는 "/chat.js" 를 서버 뿌리에서 받는다. node 에서는 같은 폴더의 파일로 잇는다.
register(
  "data:text/javascript," +
    encodeURIComponent(`export async function resolve(spec, ctx, next) {
      if (spec === "/chat.js") return next(new URL("./chat.js", ctx.parentURL).href, ctx);
      return next(spec, ctx);
    }`),
);
// 시각 표시는 시간대에 따라 다르다. 테스트는 서울 시간으로 고정한다.
process.env.TZ = "Asia/Seoul";
const {
  rankSnippets, expiryLabel, sessionErrorNotice, inquiryNumber, formatDateTime, attachmentHTML, brandTokens, contrastRatio, createLog, AVATAR_COLORS,
  createUploadGuard, createSentDrafts, restoreDraft, uploadingPillHTML, readyPillHTML, faqEditorRowHTML, attachmentGiveBack,
  ratingCardHTML, ratingSummaryHTML, starsHTML, durationText, percentText, metricsWindow, metricCards, HANDLER_LABEL, RATING_COMMENT_MAX,
  confirm, form, menu, closeMenu, toast, clearToasts, TOAST_MS, TOAST_TRACE_MS,
} = await import("./ui.js");

// 서버의 빠른 답변 목록은 단축어 가나다순이다. 시드 네 개와 같은 모양.
const SEED = [
  { id: "1", shortcut: "마무리", title: "상담 종료 인사", body: "감사합니다." },
  { id: "2", shortcut: "배송", title: "배송 안내", body: "배송은 2~3일." },
  { id: "3", shortcut: "인사", title: "첫 인사", body: "안녕하세요." },
  { id: "4", shortcut: "확인중", title: "확인 중 안내", body: "확인하고 있습니다." },
];
const first = (q, list = SEED) => rankSnippets(list, q)[0]?.shortcut;

describe("빠른 답변 고르기", () => {
  test("단축어가 정확히 맞는 빠른 답변이 제목에만 맞는 것보다 먼저 온다", () => {
    assert.equal(first("인사"), "인사");
    assert.deepEqual(rankSnippets(SEED, "인사").map((s) => s.shortcut), ["인사", "마무리"]);
  });

  test("단축어가 정확히 맞는 빠른 답변은 앞부분만 맞는 것보다도 먼저 온다", () => {
    const list = [
      { id: "a", shortcut: "인사말", title: "긴 인사", body: "" },
      { id: "b", shortcut: "인사", title: "첫 인사", body: "" },
    ];
    assert.equal(first("인사", list), "인사");
  });

  test("단축어 앞부분이 맞는 빠른 답변이 제목이나 단축어 중간에만 맞는 것보다 먼저 온다", () => {
    const list = [
      { id: "a", shortcut: "교환", title: "배송 후 교환", body: "" },
      { id: "b", shortcut: "재배송", title: "다시 보냄", body: "" },
      { id: "c", shortcut: "배송지", title: "주소 변경", body: "" },
    ];
    assert.deepEqual(rankSnippets(list, "배송").map((s) => s.shortcut), ["배송지", "교환", "재배송"]);
  });

  test("같은 등급 안에서는 서버가 준 순서를 지킨다", () => {
    assert.deepEqual(rankSnippets(SEED, "").map((s) => s.shortcut), ["마무리", "배송", "인사", "확인중"]);
  });

  test("대소문자를 가리지 않고 아무것도 맞지 않으면 비어 있다", () => {
    const list = [{ id: "x", shortcut: "Hello", title: "영문 인사", body: "" }];
    assert.equal(first("hello", list), "Hello");
    assert.deepEqual(rankSnippets(SEED, "없는말"), []);
  });
});

describe("초대 만료 표시", () => {
  const NOW = Date.parse("2026-10-04T00:00:00Z");
  const at = (ms) => new Date(NOW + ms).toISOString();
  const HOUR = 3600 * 1000;

  test("72시간 남은 초대는 남은 시간 뒤 만료로 보인다", () => {
    assert.equal(expiryLabel(at(72 * HOUR - 1000), NOW), "71시간 뒤 만료");
    assert.equal(expiryLabel(at(5 * HOUR + 1000), NOW), "5시간 뒤 만료");
    assert.equal(expiryLabel(at(30 * 60 * 1000 + 1000), NOW), "30분 뒤 만료");
    assert.equal(expiryLabel(at(20 * 1000), NOW), "곧 만료");
  });

  test("만료 시각이 지난 초대는 만료됨으로 보인다", () => {
    assert.equal(expiryLabel(at(-1000), NOW), "만료됨");
    assert.equal(expiryLabel(at(-5 * 24 * HOUR), NOW), "만료됨");
  });
});

describe("위젯 세션 실패 안내", () => {
  const err = (status, code, message, retryAfter = 0) => Object.assign(new Error(message), { status, code, retryAfter });

  test("요청이 잦아 거절되면 잠시 뒤 다시 연결한다고 알리고 Retry-After 뒤에 다시 시도한다", () => {
    const n = sessionErrorNotice(err(429, "too_many", "요청이 너무 잦습니다. 잠시 후 다시 시도해 주세요", 60));
    assert.match(n.text, /잠시 후/);
    assert.equal(n.retryInMs, 60_000);
  });

  test("Retry-After 가 없는 429 도 자동으로 다시 시도한다", () => {
    assert.ok(sessionErrorNotice(err(429, "too_many", "x")).retryInMs > 0);
  });

  // 위젯 키는 걷어냈다(D35). 없는 장치를 탓하는 안내를 내지 않는다.
  test("허용되지 않은 사이트는 원인을 말하고 자동으로 다시 시도하지 않으며, 위젯 키를 탓하지 않는다", () => {
    const origin = sessionErrorNotice(err(403, "origin_not_allowed", "이 사이트에서는 위젯을 쓸 수 없습니다"));
    assert.match(origin.text, /이 사이트/);
    assert.equal(origin.retryInMs, 0);
    for (const e of [err(404, "not_found", "대상을 찾을 수 없습니다"), err(403, "origin_not_allowed", "x"), err(500, "internal", "x")]) {
      assert.doesNotMatch(sessionErrorNotice(e).text, /위젯 키/);
      assert.equal(sessionErrorNotice(e).retryInMs, 0);
    }
  });

  test("서버가 답했는데 위젯 세션을 열지 못하면 응답의 trace id 앞 8자리를 문의 번호로 붙인다", () => {
    const e = Object.assign(err(500, "internal", "서버 오류로 요청을 처리하지 못했습니다"), { traceId: "ab12cd34ef567890ab12cd34ef567890" });
    assert.match(sessionErrorNotice(e).text, /\(문의 번호 ab12cd34\)$/);
    assert.doesNotMatch(sessionErrorNotice(err(500, "internal", "x")).text, /문의 번호/, "번호가 없으면 붙이지 않는다");
  });

  test("서버에 닿지 못하면 연결하지 못했다고 알린다", () => {
    const n = sessionErrorNotice(new TypeError("Failed to fetch"));
    assert.match(n.text, /연결하지 못했/);
  });

  test("안내 문장은 토큰이 없다는 서버 내부 문구가 아니다", () => {
    for (const e of [err(429, "too_many", "x"), err(403, "origin_not_allowed", "x"), err(404, "not_found", "x"), err(500, "internal", "처리하지 못했습니다")]) {
      assert.doesNotMatch(sessionErrorNotice(e).text, /토큰이 없습니다/);
    }
  });
});

// D47: 문의 번호는 trace id(32자리 소문자 hex)의 앞 8자리다. 모양이 아니면(앞단의 응답, 빈 값) 번호를 만들지 않는다.
describe("문의 번호", () => {
  test("문의 번호는 32자리 hex trace id 의 앞 8자리이고 모양이 아니면 빈 문자열이다", () => {
    assert.equal(inquiryNumber("ab12cd34ef567890ab12cd34ef567890"), "ab12cd34");
    for (const bad of ["", undefined, null, "ab12cd34", "AB12CD34EF567890AB12CD34EF567890", "zz12cd34ef567890ab12cd34ef567890", 42]) {
      assert.equal(inquiryNumber(bad), "", String(bad));
    }
  });
});

describe("시각 표시", () => {
  test("서버가 준 RFC 3339 시각과 옛 문자열 시각이 모두 사람이 읽는 형식으로 나온다", () => {
    assert.equal(formatDateTime("2026-10-04T21:31:53.377362+09:00"), "2026. 10. 4. 오후 09:31");
    assert.equal(formatDateTime("2026-10-04T12:31:53Z"), "2026. 10. 4. 오후 09:31");
    // 합치기 전 옛 형식은 이미 사람이 읽는 모양이라 그대로 둔다. 해석할 수 없는 값도 원문 그대로다.
    assert.equal(formatDateTime("2026-10-04 12:31"), "2026-10-04 12:31");
    assert.equal(formatDateTime("엉뚱한 값T"), "엉뚱한 값T");
    assert.equal(formatDateTime(""), "");
  });
});

describe("영상 첨부", () => {
  const att = (contentType, filename = "상담.mov") => ({ id: "att-1", filename, content_type: contentType, byte_size: 5 << 20 });

  test("재생하는 영상(mp4·mov·webm)은 controls·preload=metadata·playsinline 인 video 로 그리고 주소는 늦게 붙인다", () => {
    for (const ct of ["video/mp4", "video/quicktime", "video/webm"]) {
      const html = attachmentHTML(att(ct));
      assert.match(html, /class="attach att-video"/, ct);
      assert.match(html, /data-kind="video"/, ct);
      const video = html.match(/<video[^>]*>/)?.[0] || "";
      assert.match(video, /\bcontrols\b/, ct);
      assert.match(video, /preload="metadata"/, ct);
      assert.match(video, /\bplaysinline\b/, ct);
      // 주소는 권한 확인 뒤 발급하는 서명된 주소다. 그리는 순간에는 없다.
      assert.doesNotMatch(video, /\ssrc=/, ct);
      assert.match(html, /class="att-video-save"/, ct);
    }
  });

  test("재생하지 않는 영상(mkv)과 영상이 아닌 파일은 video 로 그리지 않고 파일 카드다", () => {
    for (const ct of ["video/x-matroska", "video/avi", "application/octet-stream", "text/plain; charset=utf-8"]) {
      const html = attachmentHTML(att(ct, "녹화.mkv"));
      assert.doesNotMatch(html, /<video/, ct);
      assert.match(html, /class="attach file"/, ct);
    }
  });

  test("영상 파일 이름은 이스케이프해 그린다", () => {
    const html = attachmentHTML(att("video/mp4", `<img src=x onerror=alert(1)>.mp4`));
    assert.doesNotMatch(html, /<img src=x/);
  });
});

describe("관리자 색의 글자 대비", () => {
  test("관리자 색이 밝으면 머리·버튼의 글자와 아이콘을 어둡게 바꿔 대비 4.5:1 이상을 지킨다", () => {
    const yellow = brandTokens("#ffd43b");
    assert.equal(yellow.ink, "#111111", "노란색에는 흰 글자를 얹지 않는다");
    assert.equal(contrastRatio("#ffffff", "#ffd43b").toFixed(2), "1.43", "흰 글자였다면 1.43:1 이다");
    assert.equal(contrastRatio(yellow.ink, yellow.brand).toFixed(2), "13.25");
    assert.equal(contrastRatio(yellow.ink, yellow.strong).toFixed(2), "11.14", "그라데이션의 어두운 끝에서도 읽힌다");

    const orange = brandTokens("#e8590c");
    assert.equal(orange.ink, "#111111");
    assert.equal(contrastRatio("#ffffff", "#e8590c").toFixed(2), "3.58");
    assert.equal(contrastRatio(orange.ink, orange.brand).toFixed(2), "5.27");
    assert.equal(contrastRatio(orange.ink, orange.strong).toFixed(2), "4.53");

    const blue = brandTokens("#0c64e8");
    assert.equal(blue.ink, "#ffffff", "흰 글자가 충분히 읽히는 색은 그대로 흰 글자다");
    assert.equal(contrastRatio(blue.ink, blue.brand).toFixed(2), "5.25");
    assert.equal(contrastRatio(blue.ink, blue.strong).toFixed(2), "7.09");

    for (const t of [yellow, orange, blue]) {
      assert.ok(contrastRatio(t.ink, t.brand) >= 4.5 && contrastRatio(t.ink, t.strong) >= 4.5, t.brand);
    }
    assert.equal(brandTokens("노랑"), null, "색이 아니면 토큰을 만들지 않는다");
  });

  // 글자는 그라데이션의 두 끝(관리자 색, 어두운 끝) 모두에 얹힌다. 한 끝만 보고 고르면 다른 끝에서 읽기 어려워진다.
  test("관리자 색 위 글자는 흰 글자와 어두운 글자 가운데 관리자 색과 그라데이션 어두운 끝 위 대비의 작은 값이 큰 쪽이다", () => {
    const worst = (t) => Math.min(contrastRatio(t.ink, t.brand), contrastRatio(t.ink, t.strong)).toFixed(2);
    const cases = [
      // [색, 고른 글자, 고른 쪽의 최소 대비, 다른 쪽이었다면의 최소 대비]
      ["#2b8a3e", "#ffffff", "4.37", "3.76"], // 흰 글자가 4.5 아래여도 어두운 글자는 어두운 끝에서 더 나쁘다
      ["#7a7a7a", "#ffffff", "4.29", "3.81"], // 관리자 색 위만 보면 어두운 글자(4.40)가 낫지만 어두운 끝에서 3.81 이다
      ["#ffd43b", "#111111", "11.14", "1.43"],
      ["#e8590c", "#111111", "4.53", "3.58"],
      ["#0c64e8", "#ffffff", "5.25", "3.15"],
      ["#808080", "#111111", "4.16", "3.95"],
      ["#c2410c", "#ffffff", "5.18", "3.19"], // 기본 색(D41)
    ];
    for (const [hex, ink, picked, other] of cases) {
      const t = brandTokens(hex);
      assert.equal(t.ink, ink, hex);
      assert.equal(worst(t), picked, `${hex} 고른 쪽`);
      const flip = ink === "#ffffff"
        ? { ink: "#111111", brand: hex, strong: darken(hex, 0.92) }
        : { ink: "#ffffff", brand: hex, strong: darken(hex, 0.82) };
      assert.equal(worst(flip), other, `${hex} 다른 쪽`);
      assert.ok(Number(picked) >= Number(other), hex);
    }
    assert.equal(brandTokens("#2b8a3e").strong, "#237133", "흰 글자면 어두운 끝은 18% 낮춘다");
    assert.equal(brandTokens("#808080").strong, "#767676", "어두운 글자면 8% 만 낮춘다");
  });
});

describe("흰 바탕 위 관리자 색 글자", () => {
  test("관리자 색을 흰 바탕의 글자·아이콘으로 쓸 때는 흰 바탕 대비 4.5:1 이 되게 낮추고 충분히 진한 색은 그대로 쓴다", () => {
    assert.equal(brandTokens("#c2410c").text, "#c2410c", "진한 색은 그대로다");
    const cases = [["#ffd43b", "#7f6a1d", "5.27"], ["#e8590c", "#c54c0a", "4.76"], ["#808080", "#737373", "4.74"]];
    for (const [hex, text, ratio] of cases) {
      const t = brandTokens(hex);
      assert.equal(t.text, text, hex);
      assert.equal(contrastRatio(t.text, "#ffffff").toFixed(2), ratio, hex);
    }
  });
});

// 화면의 기본 색(app.css :root). 관리자 색을 칠하기 전, 그리고 데스크 전체가 이 값이다(D41).
describe("기본 색의 글자 대비", () => {
  const css = readFileSync(new URL("./app.css", import.meta.url), "utf8").replace(/\/\*[\s\S]*?\*\//g, "");
  const root = css.slice(css.indexOf(":root {"), css.indexOf("}", css.indexOf(":root {")));
  const token = (name) => {
    const m = root.match(new RegExp(`--${name}:\\s*(#[0-9a-f]{6})\\s*;`, "i"));
    assert.ok(m, `--${name} 가 :root 에 없다`);
    return m[1].toLowerCase();
  };

  test("기본 관리자 색은 주황 #c2410c 이고 그 어두운 끝과 글자색은 applyBrand 가 만드는 값과 같다", () => {
    assert.equal(token("brand"), "#c2410c");
    const t = brandTokens(token("brand"));
    assert.equal(token("brand-strong"), t.strong);
    assert.equal(token("brand-ink"), t.ink);
    assert.equal(token("brand-text"), t.text);
  });

  test("본문·보조 글은 흰 바탕과 회색 바탕 모두에서 대비 4.5:1 이상이다", () => {
    for (const fg of ["ink", "ink-2", "ink-3"]) {
      for (const bg of ["panel", "bg", "line-2"]) {
        const r = contrastRatio(token(fg), token(bg));
        assert.ok(r >= 4.5, `--${fg} 가 --${bg} 위에서 ${r.toFixed(2)}:1 이다`);
      }
    }
    assert.equal(contrastRatio(token("ink-3"), token("panel")).toFixed(2), "5.24", "보조 글은 #696c76 이다(#808080 은 3.95:1)");
  });

  test("상태 글자는 자기 연한 바탕과 흰 바탕에서 대비 4.5:1 이상이고 관리자 색 글자는 연한 관리자 색 위에서도 읽힌다", () => {
    for (const [fg, bg] of [["ok-ink", "ok-soft"], ["warn-ink", "warn-soft"], ["danger", "danger-soft"], ["brand", "brand-soft"],
      ["ok-ink", "panel"], ["warn-ink", "panel"], ["danger", "panel"], ["brand", "panel"]]) {
      const r = contrastRatio(token(fg), token(bg));
      assert.ok(r >= 4.5, `--${fg} 가 --${bg} 위에서 ${r.toFixed(2)}:1 이다`);
    }
    // 알림 수 배지와 위험 토스트는 --danger 바탕에 흰 글자다
    assert.ok(contrastRatio("#ffffff", token("danger")) >= 4.5);
  });

  test("아바타 바탕색은 흰 이니셜이 읽히도록 여덟 색 모두 대비 4.5:1 이상이다", () => {
    assert.equal(AVATAR_COLORS.length, 8);
    for (const c of AVATAR_COLORS) assert.ok(contrastRatio("#ffffff", c) >= 4.5, `${c} 위 흰 글자 ${contrastRatio("#ffffff", c).toFixed(2)}:1`);
  });
});

// darken 은 테스트가 "다른 쪽을 골랐다면" 의 어두운 끝을 직접 만든다(ui.js 와 같은 계산).
function darken(hex, factor) {
  const n = parseInt(hex.slice(1), 16);
  return `#${[(n >> 16) & 255, (n >> 8) & 255, n & 255].map((v) => Math.round(v * factor).toString(16).padStart(2, "0")).join("")}`;
}

// createLog 를 띄울 만큼의 가짜 칸. 그린 HTML 을 모으고, 스크롤 값과 잡은 이벤트를 기억한다.
function fakeLog() {
  const listeners = {};
  const logEl = {
    scrollTop: 0, scrollHeight: 0, clientHeight: 400,
    addEventListener: (type, fn) => ((listeners[type] ||= []).push(fn)),
    fire: (type) => (listeners[type] || []).forEach((fn) => fn({ type })),
  };
  const pane = () => ({
    html: "",
    set innerHTML(v) { this.html = v; },
    get innerHTML() { return this.html; },
    insertAdjacentHTML(pos, v) { this.html = pos === "afterbegin" ? v + this.html : this.html + v; },
    querySelectorAll: () => [],
    querySelector: () => null,
  });
  return { logEl, msgsEl: pane(), pendingEl: pane() };
}

describe("대화 줄", () => {
  const at = "2026-10-05T01:02:03Z";
  const mine = (seq, body) => ({ seq, id: `m${seq}`, kind: "MESSAGE", sender_kind: "CUSTOMER", sender_id: "c1", sender_name: "나", body, created_at: at });

  test("같은 분에 이어 보낸 내 줄은 보내는 중 표시를 거쳐도 한 묶음으로 그린다", () => {
    const f = fakeLog();
    const log = createLog({ ...f, mineKind: "CUSTOMER", token: "t" });
    log.append(mine(1, "하나"));
    // 둘째 줄을 보내면 ack 전까지 보내는 중 칸에 그려지고, ack 뒤에 확정된 줄이 붙는다
    log.setPending([{ client_msg_id: "x", body: "둘" }]);
    log.append(mine(2, "둘"));
    log.setPending([]);
    const rows = [...f.msgsEl.html.matchAll(/<div class="row ([^"]*)"/g)].map((m) => m[1].split(" "));
    assert.equal(rows.length, 2);
    assert.ok(!rows[0].includes("same"));
    assert.ok(rows[1].includes("same"), "둘째 줄이 앞 줄과 같은 묶음이어야 마지막 줄에만 시간이 남는다");
  });

  test("맨 아래에 있던 사람만 그림이 늦게 커져도 맨 아래를 따라간다", () => {
    const f = fakeLog();
    const log = createLog({ ...f, mineKind: "CUSTOMER", token: "t" });
    f.logEl.scrollHeight = 1000;
    f.logEl.scrollTop = 600; // 맨 아래(1000 - 400)
    log.append(mine(1, "그림"));
    assert.equal(f.logEl.scrollTop, 1000);
    // 그림이 내려와 높이가 커졌다
    f.logEl.scrollHeight = 1300;
    f.logEl.fire("load");
    assert.equal(f.logEl.scrollTop, 1300, "맨 아래에 있었으면 따라 내려간다");
    f.logEl.scrollHeight = 1500;
    f.logEl.fire("loadedmetadata");
    assert.equal(f.logEl.scrollTop, 1500, "영상 크기가 정해질 때도 따라간다");

    // 위로 올려 지난 글을 읽는 중이면 끌어내리지 않는다
    f.logEl.scrollTop = 200;
    f.logEl.fire("scroll");
    f.logEl.scrollHeight = 1800;
    f.logEl.fire("load");
    assert.equal(f.logEl.scrollTop, 200);
  });

});

describe("업로드와 거절된 글", () => {
  test("업로드는 시작한 대화에 묶인다 — 대화를 옮기거나 취소한 업로드의 결과는 지금 작성칸의 것이 아니다", () => {
    const g = createUploadGuard();
    const a = g.begin("A");
    assert.equal(g.owns(a, "A"), true);
    assert.equal(g.owns(a, "B"), false, "다른 대화로 옮겼으면 붙이지 않는다");
    assert.equal(g.cancel(), true, "올리던 것을 멈췄다");
    assert.equal(a.signal.aborted, true, "저장소 올리기를 끊는다");
    assert.equal(g.owns(a, "A"), false, "취소한 업로드는 돌아와도 붙지 않는다");
    assert.equal(g.cancel(), false, "멈출 것이 없다");
    const b1 = g.begin("A");
    const b2 = g.begin("A");
    assert.equal(b1.signal.aborted, true, "새 업로드가 앞선 업로드를 멈춘다");
    assert.equal(g.owns(b1, "A"), false);
    assert.equal(g.owns(b2, "A"), true);
    g.finish(b2);
    assert.equal(g.busy, false);
  });

  test("보낸 글은 영구 거절될 때만 돌려받고 큐에 남아 다시 나갈 글은 돌려받지 않는다", () => {
    const d = createSentDrafts(3);
    d.remember("A", { client_msg_id: "1", body: "중요한 글" });
    d.rememberAll("A", [{ client_msg_id: "2", body: "일시 오류" }]);
    assert.equal(d.take("2", () => true), null, "큐에 남아 있으면 일시 오류다");
    assert.deepEqual(d.take("1"), { convId: "A", body: "중요한 글" });
    assert.equal(d.take("1"), null, "한 번만 돌려준다");
    assert.equal(d.take("모름"), null);
    for (const id of ["a", "b", "c", "d"]) d.remember("A", { client_msg_id: id, body: id });
    assert.equal(d.take("a"), null, "오래된 것부터 잊는다(최근 limit 개)");

    const input = { value: "" };
    assert.equal(restoreDraft(input, "중요한 글"), true);
    assert.equal(input.value, "중요한 글");
    const busy = { value: "새로 쓰던 글" };
    assert.equal(restoreDraft(busy, "중요한 글"), false, "새로 쓰던 글을 덮지 않는다");
    assert.equal(busy.value, "새로 쓰던 글");
  });

  test("데스크 업로드 표시의 파일 이름은 이스케이프해 그린다", () => {
    const evil = `<img src=x onerror=alert(1)>.png`;
    for (const html of [uploadingPillHTML(evil), readyPillHTML({ filename: evil, byte_size: 10 })]) {
      assert.doesNotMatch(html, /<img/);
      assert.match(html, /&lt;img src=x onerror=alert\(1\)&gt;\.png/);
    }
    assert.match(uploadingPillHTML("a.png"), /id="cancelUpload"/, "올리는 중에는 취소 단추가 있다");
  });

  test("FAQ 편집 화면은 질문과 답을 입력칸 안에서도 이스케이프해 그린다", () => {
    const html = faqEditorRowHTML({ question: `"><script>alert(1)</script>`, answer: `</textarea><img src=x onerror=alert(2)>` }, 0, 1);
    assert.doesNotMatch(html, /<script|<img/);
    assert.match(html, /value="&quot;&gt;&lt;script&gt;alert\(1\)&lt;\/script&gt;"/);
    assert.match(html, /<textarea[^>]*>&lt;\/textarea&gt;&lt;img src=x onerror=alert\(2\)&gt;<\/textarea>/);
  });

  test("포기된 글의 첨부는 아직 보내지 않은 업로드일 때만 같은 대화의 빈 작성칸에 다시 붙이고 그 밖에는 잃었다고 알린다", () => {
    const att = { id: "att-1", filename: "사진.png" };
    const capped = { convId: "A", body: "글", attachment: att, code: "conflict" }; // 재시도 상한 — 첨부는 아직 쓰이지 않았다
    assert.equal(attachmentGiveBack(capped, { shown: "A", slotFree: true }), "reattach");
    assert.equal(attachmentGiveBack(capped, { shown: "B", slotFree: true }), "lost", "다른 대화 작성칸에 붙이면 남의 대화로 나간다");
    assert.equal(attachmentGiveBack(capped, { shown: null, slotFree: true }), "lost", "열린 대화가 없다");
    assert.equal(attachmentGiveBack(capped, { shown: "A", slotFree: false }), "lost", "작성칸의 다른 첨부를 덮지 않는다");
    for (const code of ["invalid", "not_found", "ended"]) {
      assert.equal(attachmentGiveBack({ ...capped, code }, { shown: "A", slotFree: true }), "lost", `${code} 는 다시 보내도 거절된다`);
    }
    assert.equal(attachmentGiveBack({ convId: "A", body: "글" }, { shown: "A", slotFree: true }), "", "첨부가 없다");
  });

  test("거절로 돌려받은 글에는 실려 있던 첨부와 거절 까닭 코드가 함께 온다", () => {
    const drafts = createSentDrafts();
    const out = drafts.settle("c1", { client_msg_id: "c1", conversation_id: "A", body: "글", attachment: { id: "att-1" }, code: "conflict" });
    assert.deepEqual(out, { back: { convId: "A", body: "글", attachment: { id: "att-1" }, code: "conflict" } });
  });
});

// ── 상담 지표와 만족도(D45) ─────────────────────────────────────

describe("만족도 카드", () => {
  const closed = { closed: true, can_rate: true, current: false, rating: null };

  test("진행 중이거나 다시 열린 상담에는 카드가 없고 시간이 지났는데 이번 평가가 없어도 없다", () => {
    assert.equal(ratingCardHTML(null), "");
    assert.equal(ratingCardHTML({ ...closed, closed: false }), "");
    assert.equal(ratingCardHTML({ ...closed, can_rate: false }), "");
  });

  test("낼 수 있으면 별 다섯 개가 라디오 묶음이고 고르기 전에는 보내기가 잠겨 있다 — 의견 칸은 200자까지다", () => {
    const html = ratingCardHTML(closed);
    assert.equal((html.match(/role="radio"/g) || []).length, 5);
    assert.match(html, /role="radiogroup"/);
    assert.match(html, /id="csatSubmit" disabled/);
    assert.match(html, new RegExp(`maxlength="${RATING_COMMENT_MAX}"`));
    assert.equal(RATING_COMMENT_MAX, 200);
    const picked = ratingCardHTML(closed, { score: 3, comment: "<b>좋음</b>" });
    assert.match(picked, /id="csatStar3" data-score="3" aria-checked="true"/);
    assert.match(picked, /id="csatStar4" data-score="4" aria-checked="false"/);
    assert.doesNotMatch(picked, /id="csatSubmit" disabled/);
    assert.match(picked, /value="&lt;b&gt;좋음&lt;\/b&gt;"/, "고객이 쓴 의견은 이스케이프한다");
  });

  test("이번 종료에 낸 평가가 있으면 고맙다는 말과 별점이고 고칠 수 있는 동안만 수정하기가 붙는다", () => {
    const given = { closed: true, can_rate: true, current: true, rating: { score: 5, comment: "친절" } };
    assert.match(ratingCardHTML(given), /평가해 주셔서 감사합니다/);
    assert.match(ratingCardHTML(given), /5점 만점에 5점/);
    assert.match(ratingCardHTML(given), /data-action="edit"/);
    assert.doesNotMatch(ratingCardHTML({ ...given, can_rate: false }), /data-action="edit"/, "24시간이 지나면 고칠 수 없다");
    assert.match(ratingCardHTML(given, { editing: true, score: 5 }), /id="csatForm"/, "수정하기를 누르면 다시 고른다");
    // 다시 열었다 닫은 뒤의 앞 평가(current=false)는 이번 종료의 평가가 아니다 — 다시 받는다
    assert.match(ratingCardHTML({ ...given, current: false }), /상담은 어떠셨나요/);
  });

  test("데스크의 평가 칸은 별점·의견·처리 주체를 보이고 의견을 이스케이프한다 — 지난 종료의 평가는 그렇게 적는다", () => {
    const view = { closed: true, current: true, rating: { score: 2, comment: "<script>x</script>", handler: "AI", agent_name: "", updated_at: "2026-10-08T05:15:00Z" } };
    const html = ratingSummaryHTML(view);
    assert.match(html, /5점 만점에 2점/);
    assert.match(html, /AI 응대/);
    assert.doesNotMatch(html, /<script>/);
    assert.match(ratingSummaryHTML({ ...view, current: false }), /지난 종료의 평가/);
    assert.equal(ratingSummaryHTML({ closed: false, rating: null }), "", "평가가 없고 종료도 아니면 칸을 숨긴다");
    assert.match(ratingSummaryHTML({ closed: true, rating: null }), /아직 평가가 없습니다/);
    assert.match(starsHTML(4), /★★★★<\/span><span class="stars-off" aria-hidden="true">☆</);
  });
});

describe("지표 화면 글자", () => {
  test("응답 시간은 초·분·시간·일로 쓰고 값이 없으면 —, 비율은 소수 첫째 자리 백분율이다", () => {
    assert.equal(durationText(null), "—");
    assert.equal(durationText(400), "1초 미만");
    assert.equal(durationText(42_000), "42초");
    assert.equal(durationText(200_000), "3분 20초");
    assert.equal(durationText(3_900_000), "1시간 5분");
    assert.equal(durationText(97_200_000), "1일 3시간");
    assert.equal(percentText(null), "—");
    assert.equal(percentText(1 / 3), "33.3%");
    assert.equal(percentText(0.5), "50%");
    assert.equal(percentText(0), "0%");
  });

  test("AI 응대가 한 번도 없으면 AI 처리율은 0% 가 아니라 — 이고 아직 없다고 쓴다", () => {
    const empty = { ai_active: false, ai_rate: null, closed: { total: 0, agent: 0, ai: 0, unanswered: 0 }, first_response: { count: 0 }, response: { count: 0 }, satisfaction: { responses: 0 }, response_rate: null };
    const ai = metricCards(empty).find((c) => c.key === "ai");
    assert.equal(ai.value, "—");
    assert.match(ai.sub, /AI 응대가 아직 없습니다/);
    // 상담원만 처리한 기간 — AI 가 있었던 워크스페이스라면 0% 다
    const zero = metricCards({ ...empty, ai_active: true, ai_rate: 0, closed: { total: 3, agent: 3, ai: 0, unanswered: 0 } }).find((c) => c.key === "ai");
    assert.equal(zero.value, "0%");
    const cards = metricCards(empty);
    assert.equal(cards.find((c) => c.key === "first").value, "—", "응답이 없으면 0초가 아니다");
    assert.match(cards.find((c) => c.key === "csat").sub, /아직 평가가 없습니다/);
  });

  test("AI 처리율의 분자에 고객 자체 해결이 들고 AI 응대가 없어도 고객 자체 해결이 있으면 비율을 보인다 — 고객이 직접 종료한 수는 카드가 따로다", () => {
    const base = { first_response: { count: 0 }, response: { count: 0 }, satisfaction: { responses: 0 }, response_rate: null };
    const m = { ...base, ai_active: false, ai_rate: 0.5, closed: { total: 5, agent: 2, ai: 0, unanswered: 1, self: 2, by_customer: 3 } };
    const cards = metricCards(m);
    const ai = cards.find((c) => c.key === "ai");
    assert.equal(ai.value, "50%", "AI 응대가 없어도 상담원 없이 끝난 비율이 있다");
    assert.match(ai.sub, /고객 자체 해결 2건/);
    const by = cards.find((c) => c.key === "by_customer");
    assert.equal(by.label, "고객이 직접 종료");
    assert.equal(by.value, "3건");
    assert.match(by.sub, /고객 자체 해결 2건/);
    assert.equal(cards.find((c) => c.key === "closed").split.self, 2, "종료 분포에 고객 자체 해결이 있다");
    assert.equal(HANDLER_LABEL.SELF, "고객 자체 해결");
  });

  test("기간 단추: 오늘은 오늘 0시부터, 7일은 오늘 포함 7일, 직접은 종료일 하루를 다 넣는다", () => {
    const now = new Date(2026, 9, 8, 14, 30);
    const today = metricsWindow("today", { now });
    assert.equal(today.from.getTime(), new Date(2026, 9, 8).getTime());
    assert.equal(today.to.getTime(), now.getTime());
    assert.equal(metricsWindow("7d", { now }).from.getTime(), new Date(2026, 9, 2).getTime());
    assert.equal(metricsWindow("30d", { now }).from.getTime(), new Date(2026, 8, 9).getTime());
    const custom = metricsWindow("custom", { fromDate: "2026-10-01", toDate: "2026-10-05" });
    assert.equal(custom.from.getTime(), new Date(2026, 9, 1).getTime());
    assert.equal(custom.to.getTime(), new Date(2026, 9, 6).getTime());
    assert.equal(metricsWindow("custom", { fromDate: "2026-10-05", toDate: "2026-10-01" }), null, "거꾸로 고른 기간");
    assert.equal(metricsWindow("custom", { fromDate: "", toDate: "2026-10-01" }), null);
  });
});

// ── 확인 창·입력 창(DEF-157) ─────────────────────────────────────
// 작은 가짜 문서. 창(막)이 붙고 빠지는 것, 문서의 keydown 듣기, 초점(activeElement)만 흉내 낸다.
// 창 안의 단추는 그린 HTML 에 있는 선택자마다 하나씩 준다.
function installModalDocument() {
  class El {
    constructor(tag) {
      this.tagName = tag.toUpperCase();
      this.listeners = {};
      this.children = [];
      this.parentNode = null;
      this.className = "";
      this._html = "";
      this._q = new Map();
    }
    set innerHTML(v) { this._html = String(v); this._q = new Map(); }
    get innerHTML() { return this._html; }
    get isConnected() { return this === doc.body || (!!this.parentNode && this.parentNode.isConnected); }
    get firstElementChild() { return this; }
    addEventListener(t, fn) { (this.listeners[t] ||= []).push(fn); }
    dispatch(t, ev = {}) { for (const fn of this.listeners[t] || []) fn({ type: t, target: this, preventDefault() {}, ...ev }); }
    setAttribute(k, v) { (this.attrs ||= {})[k] = String(v); }
    getAttribute(k) { return this.attrs?.[k] ?? null; }
    appendChild(c) { this.children.push(c); c.parentNode = this; return c; }
    remove() {
      const list = this.parentNode?.children;
      if (list) list.splice(list.indexOf(this), 1);
      this.parentNode = null;
    }
    focus() { doc.activeElement = this; }
    querySelector(sel) {
      const token = sel.startsWith("[") ? sel.slice(1, -1) : sel.split(",")[0].trim().replace(/^\./, "");
      if (!this._html.includes(token) && !sel.includes("button")) return null;
      if (!this._q.has(sel)) {
        const c = new El("button");
        c.parentNode = this;
        this._q.set(sel, c);
      }
      return this._q.get(sel);
    }
    // 메뉴(menu)가 쓰는 것. 메뉴의 단추는 그린 HTML 의 <button> 마다 하나다. 위치는 테스트가 rect·offsetWidth 로 정한다.
    querySelectorAll(sel) {
      if (sel !== "button") return [];
      return (this._buttons ||= [...this._html.matchAll(/<button[^>]*>([^<]*)<\/button>/g)].map(([, label]) => {
        const b = new El("button");
        b.textContent = label;
        b.parentNode = this;
        return b;
      }));
    }
    get classList() {
      const self = this;
      const set = () => new Set(self.className.split(" ").filter(Boolean));
      return {
        add: (...c) => { self.className = [...new Set([...set(), ...c])].join(" "); },
        contains: (c) => set().has(c),
        toggle: (c, on) => { const x = set(); if (on ?? !x.has(c)) x.add(c); else x.delete(c); self.className = [...x].join(" "); },
      };
    }
    get parentElement() { return this.parentNode; }
    closest() { return null; }
    contains(x) { return x === this; }
    getBoundingClientRect() { return this.rect || { left: 0, right: 0, width: 0 }; }
  }
  const keydown = new Set();
  const doc = {
    body: null,
    activeElement: null,
    createElement: (tag) => new El(tag),
    documentElement: { clientWidth: 390 },
    addEventListener: (t, fn) => t === "keydown" && keydown.add(fn),
    removeEventListener: (t, fn) => t === "keydown" && keydown.delete(fn),
    press: (key) => { for (const fn of [...keydown]) fn({ key }); },
    keydownCount: () => keydown.size,
    scrims: () => doc.body.children.filter((c) => c.className === "scrim"),
  };
  doc.body = new El("body");
  doc.activeElement = doc.body;
  globalThis.document = doc;
  // 창을 연 단추. 문서에 붙어 있고 초점이 여기 있다.
  const opener = doc.body.appendChild(new El("button"));
  opener.focus();
  return { doc, opener };
}

const settled = (p) => Promise.race([p.then((v) => ({ v })), new Promise((r) => setTimeout(() => r("pending"), 20))]);

describe("확인 창과 입력 창은 어느 길로 닫아도 풀린다", () => {
  test("확인 창을 Esc 로 닫으면 confirm 이 false 로 풀리고 초점이 창을 연 단추로 돌아간다", async () => {
    const { doc, opener } = installModalDocument();
    const answer = confirm({ title: "상담을 종료할까요?" });
    assert.equal(doc.scrims().length, 1);
    assert.notEqual(doc.activeElement, opener, "창이 열리면 초점이 창 안으로 간다");
    doc.press("Escape");
    assert.deepEqual(await settled(answer), { v: false }, "Esc 로 닫았는데 약속이 풀리지 않았다");
    assert.equal(doc.scrims().length, 0);
    assert.equal(doc.activeElement, opener, "초점이 창을 연 단추로 돌아가지 않았다");
    assert.equal(doc.keydownCount(), 0, "닫은 창의 Esc 듣기가 남았다");
  });

  test("확인 창을 바깥(막) 클릭으로 닫으면 confirm 이 false 로 풀리고 초점이 돌아간다", async () => {
    const { doc, opener } = installModalDocument();
    const answer = confirm({ title: "이 메시지를 삭제할까요?", ok: "삭제", danger: true });
    const scrim = doc.scrims()[0];
    scrim.dispatch("click", { target: scrim });
    assert.deepEqual(await settled(answer), { v: false });
    assert.equal(doc.activeElement, opener);
    assert.equal(doc.keydownCount(), 0);
  });

  test("확인 단추는 true 취소 단추는 false 로 한 번만 풀리고 초점이 돌아간다", async () => {
    let { doc, opener } = installModalDocument();
    const yes = confirm({ title: "되살릴까요?", ok: "되살리기" });
    doc.scrims()[0].querySelector('[data-act="ok"]').dispatch("click");
    doc.press("Escape"); // 이미 닫힌 창이다. 답을 바꾸지 않는다
    assert.deepEqual(await settled(yes), { v: true });
    assert.equal(doc.activeElement, opener);
    ({ doc, opener } = installModalDocument());
    const no = confirm({ title: "되살릴까요?", ok: "되살리기" });
    doc.scrims()[0].querySelector('[data-act="cancel"]').dispatch("click");
    assert.deepEqual(await settled(no), { v: false });
    assert.equal(doc.activeElement, opener);
  });

  test("입력 창을 Esc 로 닫으면 form 이 null 로 풀리고 초점이 돌아간다", async () => {
    const { doc, opener } = installModalDocument();
    const out = form({ title: "보류", fields: [{ name: "minutes", label: "몇 분 뒤", type: "number", value: "120" }] });
    doc.press("Escape");
    assert.deepEqual(await settled(out), { v: null });
    assert.equal(doc.activeElement, opener);
  });

  test("창을 연 단추가 그사이 문서에서 빠졌으면 초점을 그리로 옮기지 않는다", async () => {
    const { doc, opener } = installModalDocument();
    const answer = confirm({ title: "삭제할까요?" });
    const inside = doc.activeElement;
    opener.remove();
    doc.press("Escape");
    assert.deepEqual(await settled(answer), { v: false });
    assert.equal(doc.activeElement, inside, "문서에 없는 요소로 초점을 옮겼다");
  });
});

// ── 알림(toast) ──────────────────────────────────────────────────
// 시간은 정책 숫자라 리터럴로 고정한다(감사 29 U3·W6). 되돌리기 단추를 누르면 알림이 닫힌다(U2).
describe("알림", () => {
  const toastsOf = (doc) => doc.body.children.find((c) => c.className === "toasts");
  const shown = (doc) => toastsOf(doc)?.children || [];

  test("알림 칸은 role=status aria-live=polite 이고 기본 4초 문의 번호가 있으면 10초 duration 을 주면 그 시간 뒤에 닫힌다", () => {
    mock.timers.enable({ apis: ["setTimeout"] });
    try {
      const { doc } = installModalDocument();
      toast("기본 알림");
      toast("오류 알림", "danger", { traceId: "0123456789abcdef0123456789abcdef" });
      toast("되돌릴 수 있는 알림", "", { duration: 7000, action: { label: "되돌리기", onSelect() {} } });
      const root = toastsOf(doc);
      assert.equal(root.getAttribute("role"), "status");
      assert.equal(root.getAttribute("aria-live"), "polite");
      assert.equal(TOAST_MS, 4000);
      assert.equal(TOAST_TRACE_MS, 10000);
      assert.equal(shown(doc).length, 3);
      mock.timers.tick(3999);
      assert.equal(shown(doc).length, 3, "4초 전에 닫혔다");
      mock.timers.tick(1);
      assert.deepEqual(shown(doc).map((t) => t.className), ["toast danger", "toast "], "기본 알림이 4초에 닫히지 않았다");
      mock.timers.tick(2999);
      assert.equal(shown(doc).length, 2, "duration(7초) 전에 닫혔다");
      mock.timers.tick(1);
      assert.deepEqual(shown(doc).map((t) => t.className), ["toast danger"], "duration(7초)을 따르지 않았다");
      mock.timers.tick(3000);
      assert.equal(shown(doc).length, 0, "문의 번호 알림이 10초에 닫히지 않았다");
    } finally {
      mock.timers.reset();
    }
  });

  test("되돌리기 단추를 누르면 알림이 닫히고 되돌리기를 부른다 — clearToasts 는 떠 있는 알림을 모두 치운다", () => {
    const { doc } = installModalDocument();
    let undone = 0;
    toast("보내지 못한 글을 지웠습니다", "", { duration: 10000, action: { label: "되돌리기", onSelect: () => undone++ } });
    toast("보내지 못했습니다", "danger", { copy: "글" });
    const [undoToast] = shown(doc);
    assert.match(undoToast.innerHTML, /class="toast-action">되돌리기</);
    undoToast.querySelector(".toast-action").dispatch("click");
    assert.equal(undone, 1);
    assert.ok(!shown(doc).includes(undoToast), "되돌리기를 눌렀는데 알림이 남았다 — 다시 누르면 두 번 되살린다");
    assert.equal(shown(doc).length, 1);
    clearToasts();
    assert.equal(shown(doc).length, 0, "로그아웃에서 치울 알림이 남았다");
  });
});

describe("메뉴", () => {
  // anchorAt 은 감싼 칸이 화면 가로 [left, right] 에 있는 메뉴 단추다. 메뉴 폭은 width 다.
  function anchorAt(left, right, width, vw = 390) {
    const { doc, opener } = installModalDocument();
    doc.documentElement.clientWidth = vw;
    const wrap = doc.body.appendChild(doc.createElement("div"));
    wrap.rect = { left, right, width: right - left };
    const anchor = wrap.appendChild(doc.createElement("button"));
    const realCreate = doc.createElement;
    doc.createElement = (tag) => Object.assign(realCreate(tag), { offsetWidth: width });
    return { doc, opener, wrap, anchor };
  }
  const items = [{ label: "30분 뒤" }, { label: "직접 입력…" }];

  test("메뉴가 화면 가로 밖으로 나가면 펼치는 쪽을 바꿔 화면 안에 든다 — 들면 펼치는 쪽을 그대로 둔다", () => {
    // 390px 휴대폰의 왼쪽 단추(담당·보류): 오른쪽 끝에 맞춰 왼쪽으로 펼치면 화면 왼쪽 밖이다
    let { anchor } = anchorAt(12, 56, 220);
    assert.ok(menu(anchor, items).classList.contains("left"), "왼쪽 밖으로 나가는 메뉴를 오른쪽으로 펼치지 않았다");
    closeMenu();
    // 오른쪽 단추: 왼쪽으로 펼쳐도 화면 안이다
    ({ anchor } = anchorAt(330, 378, 220));
    assert.ok(!menu(anchor, items).classList.contains("left"), "화면 안에 드는 메뉴의 쪽을 바꿨다");
    closeMenu();
    // 왼쪽 맞춤을 고른 메뉴가 오른쪽 밖으로 나가면 왼쪽으로 펼친다
    ({ anchor } = anchorAt(330, 378, 220));
    assert.ok(!menu(anchor, items, { align: "left" }).classList.contains("left"), "오른쪽 밖으로 나가는 메뉴를 왼쪽으로 펼치지 않았다");
    closeMenu();
    // 넓은 화면은 그대로다
    ({ anchor } = anchorAt(400, 444, 220, 1280));
    assert.ok(!menu(anchor, items).classList.contains("left"));
    closeMenu();
    // 둘 다 넘치면 덜 넘치는 쪽이다(가운데 좁은 칸에 넓은 메뉴)
    ({ anchor } = anchorAt(150, 190, 320));
    assert.ok(menu(anchor, items).classList.contains("left"), "더 넘치는 쪽으로 펼쳤다");
    closeMenu();
  });

  test("메뉴 항목이 연 확인 창·입력 창을 닫으면 초점이 사라진 항목이 아니라 메뉴를 연 단추로 돌아간다", async () => {
    for (const open of [() => confirm({ title: "이 메시지를 삭제할까요?", ok: "삭제", danger: true }),
      () => form({ title: "보류", fields: [{ name: "minutes", label: "몇 분 뒤", type: "number", value: "120" }] })]) {
      const { doc, anchor } = anchorAt(12, 56, 220);
      anchor.focus();
      let answer;
      const el = menu(anchor, [{ label: "직접 입력…", onSelect: () => { answer = open(); } }]);
      const item = el.querySelectorAll("button")[0];
      item.focus(); // 항목을 눌렀다(초점이 항목에 있다)
      item.dispatch("click");
      assert.ok(answer, "항목이 창을 열지 않았다");
      assert.equal(el.parentNode, null, "메뉴가 닫히지 않았다");
      doc.press("Escape");
      await settled(answer);
      assert.equal(doc.activeElement, anchor, "창을 닫은 뒤 초점이 메뉴를 연 단추로 돌아가지 않았다");
    }
  });
});
