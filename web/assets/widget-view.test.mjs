// 위젯 디자인(D38)의 화면 쪽 정책 테스트.
//
// widget-view.js 의 순수 함수와 위젯 스타일 파일(widget.css)을 본다. 단언은 사람이 보는 결과다 —
// 홈에 무엇이 보이는가, 시간이 어느 줄에 남는가, 어떤 색과 움직임 규칙이 적용되는가.
// 실제 브라우저에서 계산된 스타일은 리포 밖 uitest 와 워커 확인 스크립트가 본다.
import { test, describe } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import { homeSections, rememberAgent, agentsFor, phoneFrame, timeGroupHides, attachIcon, uploadProgress, ICON, RECENT_LIMIT, AGENT_LIMIT, stampTime, dayStamp, clockTime, convCard } from "./widget-view.js";

// 시각은 이 기기의 시간대로 쓴다. 테스트는 서울(UTC+9)로 고정한다 — UTC 에서 돌려도 "UTC 로 읽는" 실수가 같은 글자를 내지 않게(정책 653).
process.env.TZ = "Asia/Seoul";

const here = (name) => new URL(name, import.meta.url);
const read = (name) => readFileSync(here(name), "utf8");

// cssRules 는 CSS 를 [{ media, selectors, decls }] 로 편다. @media 한 겹까지만 안다(위젯 스타일이 그 이상 쓰지 않는다).
function cssRules(text) {
  const src = text.replace(/\/\*[\s\S]*?\*\//g, "");
  const out = [];
  function walk(body, media) {
    let i = 0;
    while (i < body.length) {
      const open = body.indexOf("{", i);
      if (open < 0) break;
      const head = body.slice(i, open).trim();
      let depth = 1, j = open + 1;
      while (j < body.length && depth > 0) {
        if (body[j] === "{") depth++;
        else if (body[j] === "}") depth--;
        j++;
      }
      const inner = body.slice(open + 1, j - 1);
      if (head.startsWith("@media")) walk(inner, head);
      else if (!head.startsWith("@")) {
        const decls = {};
        for (const part of inner.split(";")) {
          const k = part.indexOf(":");
          if (k > 0) decls[part.slice(0, k).trim()] = part.slice(k + 1).trim();
        }
        out.push({ media, selectors: head.split(",").map((s) => s.trim().replace(/\s+/g, " ")), decls });
      }
      i = j;
    }
  }
  walk(src, "");
  return out;
}

describe("위젯 홈", () => {
  test("FAQ 와 최근 대화가 홈에 보인다 — FAQ 는 한 줄이라도 있으면, 최근 대화는 하나라도 있으면 세 개까지", () => {
    const convs = ["a", "b", "c", "d", "e"].map((id) => ({ id }));
    const both = homeSections({ faq: [{ question: "q", answer: "a" }], conversations: convs });
    assert.equal(both.faqVisible, true);
    assert.equal(both.recentVisible, true);
    assert.equal(RECENT_LIMIT, 3);
    assert.deepEqual(both.recent.map((c) => c.id), ["a", "b", "c"]);

    const none = homeSections({ faq: [], conversations: [] });
    assert.equal(none.faqVisible, false);
    assert.equal(none.recentVisible, false);
    assert.deepEqual(none.recent, []);
    assert.deepEqual(homeSections({}), { faqVisible: false, recentVisible: false, recent: [] });
  });

  test("홈의 상담원 묶음은 답한 상담원을 최근 순으로 겹치지 않게 세 명까지 보인다", () => {
    assert.equal(AGENT_LIMIT, 3);
    let list = [];
    for (const name of ["김", "이", "김", "박", "최"]) list = rememberAgent(list, name);
    assert.deepEqual(list, ["최", "박", "김"]);
    const before = ["가", "나"];
    assert.deepEqual(rememberAgent(before, "  "), ["가", "나"], "빈 이름은 무시한다");
    assert.deepEqual(before, ["가", "나"], "받은 배열은 그대로다");
    assert.deepEqual(rememberAgent("깨진 값", "다"), ["다"], "저장된 값이 배열이 아니면 새로 시작한다");
    // 이미 앞에 있는 사람이 또 답해도 한 번만 남는다
    assert.deepEqual(rememberAgent(["김"], "김"), ["김"]);
    assert.deepEqual(rememberAgent(["김", "이"], " 김 "), ["김", "이"]);
    // 저장된 목록이 이미 겹쳐 있어도(옛 저장분·손으로 고친 값) 한 번만 보인다
    assert.deepEqual(rememberAgent(["김", "김", "이"], ""), ["김", "이"]);
    assert.deepEqual(rememberAgent(["이", "김", "이", "박"], "최"), ["최", "이", "김"]);
  });

  test("상담원 묶음은 그 고객의 것만 보이고 다른 고객으로 바뀌면 비운다", () => {
    const saved = { owner: "c1", names: ["김", "이"] };
    assert.deepEqual(agentsFor(saved, "c1"), ["김", "이"]);
    assert.deepEqual(agentsFor(saved, "c2"), [], "다른 고객에게 남이 받은 상담원 이름을 보이지 않는다");
    assert.deepEqual(agentsFor(["김"], "c1"), [], "주인을 모르는 옛 형식은 버린다");
    assert.deepEqual(agentsFor(saved, ""), [], "세션을 열기 전에는 보이지 않는다");
    assert.deepEqual(agentsFor(null, "c1"), []);
  });
});

describe("위젯 대화", () => {
  test("같은 사람이 같은 분에 보낸 말풍선은 마지막 줄에만 시간을 남긴다", () => {
    const rows = [
      { same: false, time: "오전 10:01" }, // 상담원 첫 줄
      { same: true, time: "오전 10:01" }, //  같은 상담원, 같은 분
      { same: true, time: "오전 10:02" }, //  같은 상담원, 다음 분
      { same: false, time: "오전 10:02" }, // 고객 — 사람이 바뀌었다
      { same: false, time: "" }, //          시스템 줄은 묶음을 끊는다
      { same: true, time: "오전 10:02" },
    ];
    assert.deepEqual(timeGroupHides(rows), [true, false, false, false, false, false]);
    assert.deepEqual(timeGroupHides([]), []);
  });

  test("임베드 위젯은 iframe 폭이 화면 폭과 같고 480px 이하일 때만 휴대폰 화면으로 그린다", () => {
    assert.equal(phoneFrame(390, 390), true, "휴대폰에서 iframe 이 화면 전체로 커졌다");
    assert.equal(phoneFrame(389, 390), true, "2px 까지는 같은 폭이다");
    assert.equal(phoneFrame(400, 1440), false, "데스크톱의 400px 판");
    assert.equal(phoneFrame(400, 390), false, "viewport meta 가 없는 호스트의 휴대폰 — iframe 은 400×720 그대로다");
    assert.equal(phoneFrame(800, 800), false, "480px 보다 넓으면 휴대폰이 아니다");
    assert.equal(phoneFrame(400, 0), false, "화면 폭을 모르면 휴대폰이라고 하지 않는다");
  });

  test("업로드 진행률은 올린 바이트의 백분율로 보이고 다 올라가면 확인 중으로 바뀐다", () => {
    assert.deepEqual(uploadProgress(0, 0), { known: false, percent: 0, phase: "uploading" });
    assert.deepEqual(uploadProgress(25, 100), { known: true, percent: 25, phase: "uploading" });
    assert.deepEqual(uploadProgress(999, 1000), { known: true, percent: 99, phase: "uploading" }, "다 올라가기 전에는 100% 가 아니다");
    assert.deepEqual(uploadProgress(100 * 1024 * 1024, 100 * 1024 * 1024), { known: true, percent: 100, phase: "verifying" });
    assert.equal(uploadProgress(150, 100).percent, 100);
  });

  test("업로드 중 표시는 이미지·영상·파일마다 다른 3D 아이콘을 쓴다", () => {
    assert.equal(attachIcon("image/png"), ICON.picture);
    assert.equal(attachIcon("video/mp4"), ICON.video);
    assert.equal(attachIcon("application/pdf"), ICON.page);
    assert.equal(attachIcon(""), ICON.page);
  });
});

// 관리자 색으로 칠하는 표면. 바탕은 --brand 에서, 그 위의 글자·아이콘은 --brand-ink 에서 나온다.
// 머리는 흰 바탕이고 관리자 색은 머리의 로고 자리(.w-head-avatar)에만 들어간다(D44).
// 인사 배너는 관리자 색을 채우지 않는다 — 연한 관리자 색(--brand-soft) 바탕에 진한 제목이다(정책 650).
// 주 버튼·보내기 단추는 공용 컴포넌트(app.css)가 관리자 색을 칠하고 위젯은 크기만 덮는다(ADR 0005). 그래서 두 파일을 같이 본다.
const BRAND_SURFACES = [".w-head-avatar", ".launcher", ".btn.primary", ".composer .send"];

// 바깥 주소에서 자산을 받는 모양. 프로토콜을 뺀 주소(//cdn…)도 바깥 주소다. @import 도 본다.
const OUTSIDE_REF = /(src=|url\(|@import)\s*["']?(https?:)?\/\//i;

describe("위젯 스타일", () => {
  const rules = cssRules(read("widget.css"));
  const ruleFor = (selector, media = "") => rules.filter((r) => r.media === media && r.selectors.includes(selector));

  // surfaceRules 는 표면 하나를 칠하는 규칙이다 — 공용 컴포넌트(app.css 의 그 선택자)와 위젯(widget.css 의 body.widget 아래 그 선택자).
  const appRules = cssRules(read("app.css"));
  const surfaceRules = (sel) => [...appRules.filter((r) => r.selectors.includes(sel)), ...rules.filter((r) => r.selectors.includes(`body.widget ${sel}`))];

  test("관리자 색이 로고와 버튼에 반영된다 — 머리 로고·런처·주 버튼·보내기 단추의 바탕은 --brand 에서 나온다", () => {
    for (const sel of BRAND_SURFACES) {
      const found = surfaceRules(sel).filter((r) => r.decls.background);
      assert.ok(found.length > 0, `${sel} 에 바탕 규칙이 없다`);
      // 바탕을 칠하는 모든 규칙(마우스 올림 등 @media 안 포함)이 관리자 색을 쓴다. background-color·background-image 로 덮어도 잡는다.
      for (const r of surfaceRules(sel)) {
        for (const prop of ["background", "background-color", "background-image"]) {
          const v = r.decls[prop];
          if (v === undefined) continue;
          assert.match(v, /var\(--brand/, `${sel} 의 ${prop} 가 관리자 색을 쓰지 않는다: ${v}`);
          assert.doesNotMatch(v, /#[0-9a-f]{3,8}\b/i, `${sel} 의 ${prop} 에 고정 색이 있다`);
        }
      }
    }
    // 위젯 스타일은 --brand 자체를 덮지 않는다. 덮으면 applyBrand(ui.js)가 html 에 쓴 관리자 색이 가려진다.
    for (const r of rules) assert.equal(r.decls["--brand"], undefined, `${r.selectors.join(",")} 가 --brand 를 덮는다`);
  });

  test("관리자 색 위의 글자와 아이콘은 --brand-ink 를 따른다 — 머리 로고·런처·주 버튼·보내기 단추", () => {
    for (const sel of BRAND_SURFACES) {
      const colored = surfaceRules(sel).filter((r) => r.decls.color !== undefined);
      assert.ok(colored.length > 0, `${sel} 에 글자색 규칙이 없다`);
      for (const r of colored) assert.match(r.decls.color, /var\(--brand-ink\)/, `${sel} 의 글자색이 --brand-ink 가 아니다: ${r.decls.color}`);
    }
    // 머리 안에서 흰색을 고정으로 쓰는 글자·테두리가 없다. 노란 관리자 색에서 안 보인다.
    for (const r of rules.filter((x) => x.selectors.some((sel) => sel.startsWith("body.widget .m-head")))) {
      for (const prop of ["color", "outline-color"]) {
        const v = r.decls[prop];
        if (v !== undefined) assert.doesNotMatch(v, /#fff\b|#ffffff|rgba?\(255,\s*255,\s*255/i, `${r.selectors.join(",")} 의 ${prop} 가 흰색 고정이다`);
      }
    }
  });

  test("인사 배너는 크림 바탕에 진한 제목과 본문색 글이다 — 관리자 색을 깔지 않는다", () => {
    const hero = rules.filter((r) => r.selectors.includes("body.widget .w-hero"));
    assert.ok(hero.some((r) => r.decls.background === "var(--cream)"), "배너 바탕이 크림이 아니다");
    for (const r of rules.filter((x) => x.selectors.some((sel) => /^body\.widget \.w-hero( |$)/.test(sel)))) {
      for (const prop of ["background", "background-color"]) {
        if (r.decls[prop] !== undefined) assert.doesNotMatch(r.decls[prop], /var\(--brand(-strong|-soft)?\)/, `${r.selectors.join(",")} 가 관리자 색을 채운다`);
      }
      if (r.decls.color !== undefined) assert.doesNotMatch(r.decls.color, /--brand-ink/, `${r.selectors.join(",")} 가 관리자 색 위 글자색을 쓴다(배너 바탕이 연하다)`);
    }
    assert.equal(rules.find((r) => r.selectors.includes("body.widget .w-hero h2"))?.decls.color, "var(--ink)", "배너 제목이 진한 글자가 아니다");
  });

  test("머리는 흰 바탕에 진한 이름과 닫기 단추이고 관리자 색은 로고 자리에만 쓴다", () => {
    const head = ruleFor("body.widget .m-head");
    assert.ok(head.some((r) => r.decls.background === "var(--panel)"), "머리 바탕이 흰 판(--panel)이 아니다");
    assert.ok(head.some((r) => r.decls.color === "var(--ink)"), "머리 글자가 본문색(--ink)이 아니다");
    assert.ok(ruleFor("body.widget .m-head .icon-btn").some((r) => r.decls.color === "var(--ink)"), "닫기·뒤로 단추가 본문색이 아니다");
    // 어느 화면(data-view)이든, 어느 @media 든 머리 바탕을 관리자 색으로 칠하는 규칙이 없다.
    for (const r of rules.filter((x) => x.selectors.some((sel) => /^body\.widget .*\.m-head$/.test(sel)))) {
      for (const prop of ["background", "background-color"]) {
        if (r.decls[prop] !== undefined) assert.doesNotMatch(r.decls[prop], /--brand/, `${r.selectors.join(",")} 가 머리를 관리자 색으로 칠한다`);
      }
    }
  });

  test("위젯 스타일의 선택자는 전부 body.widget 아래라 데스크에 닿지 않는다", () => {
    // 임베드 iframe 을 투명하게 하는 html 규칙 하나만 예외다. 그것도 body.widget.embed 가 있을 때만 맞는다.
    const ALLOWED = new Set(["html:has(> body.widget.embed)", "html.w-embed"]);
    for (const r of rules) {
      for (const sel of r.selectors) {
        if (ALLOWED.has(sel)) continue;
        assert.match(sel, /^body\.widget(?=[\s.:[#>~+]|$)/, `${sel} 가 body.widget 밖으로 샌다`);
      }
    }
  });

  test("움직임 최소화 설정에서는 전환 애니메이션이 없다", () => {
    const REDUCE = "@media (prefers-reduced-motion: reduce)";
    const reduce = rules.filter((r) => r.media === REDUCE);
    const all = reduce.find((r) => r.selectors.includes("body.widget *"));
    assert.ok(all, "움직임 최소화 블록에 위젯 전체 규칙이 없다");
    assert.match(all.decls.animation, /^none !important$/);
    assert.match(all.decls.transition, /^none !important$/);
    // 누르거나 올렸을 때 움직이는(transform) 규칙은 움직임 최소화에서 전부 transform: none 으로 되돌린다.
    const still = new Set(reduce.filter((r) => r.decls.transform === "none").flatMap((r) => r.selectors));
    const moving = rules
      .filter((r) => r.media !== REDUCE && r.decls.transform && r.decls.transform !== "none")
      .flatMap((r) => r.selectors)
      .filter((s) => /:hover|:active|\.closed/.test(s));
    assert.ok(moving.length > 0);
    for (const s of moving) assert.ok(still.has(s), `${s} 가 움직임 최소화에서도 움직인다`);
  });

  test("위젯에서 손가락으로 누르는 단추는 가로세로 44px 이상이다", () => {
    const tap = rules.find((r) => r.selectors.includes("body.widget") && r.decls["--w-tap"]);
    assert.equal(tap?.decls["--w-tap"], "44px");
    const has = (sel, prop) => ruleFor(sel).some((r) => r.decls[prop] === "var(--w-tap)");
    for (const sel of ["body.widget .m-head .icon-btn", "body.widget .composer .send", "body.widget .w-attach-btn"]) {
      assert.ok(has(sel, "width") && has(sel, "height"), `${sel} 가 44px 칸이 아니다`);
    }
    const pill = rules.find((r) => r.selectors.includes("body.widget .attach-pill #dropAttach"));
    assert.equal(pill?.decls.width, "var(--w-tap)", "첨부 취소 단추가 44px 칸이 아니다");
    assert.ok(has("body.widget .btn", "min-height"), "버튼이 44px 보다 낮다");
    assert.ok(has("body.widget .pill", "min-height"), "자주 묻는 질문 알약이 44px 보다 낮다");
    // 자주 묻는 질문 화면의 질문 줄은 공용 아코디언(app.css)이 56px 이다(design-system.test.mjs)
    assert.ok(has("body.widget .link-btn", "min-height"), "전체보기가 44px 보다 낮다");
  });

  // 3D 아이콘은 widget-view.js 의 ICON 가운데 포인트 자리의 것만 화면에 나온다. 위젯 마크업·스크립트·스타일 어디에서 쓰든 그 밖의 아이콘이 있으면 실패한다.
  test("3D 아이콘은 인사 배너·빈 상태·첨부 종류에만 보이고 나머지 자리는 평면 글리프다", () => {
    const POINT = new Set(Object.values(ICON).map((p) => p.replace("/icons/", "")));
    assert.deepEqual([...POINT].sort(), ["framed_picture.png", "headphone.png", "page.png", "speech_balloon.png", "video_camera.png", "waving_hand.png"]);
    for (const name of ["widget.html", "widget.js", "widget.css"]) {
      for (const [, icon] of read(name).matchAll(/\/icons\/([\w.-]+)/g)) assert.ok(POINT.has(icon), `${name} 가 포인트가 아닌 자리에 3D 아이콘 ${icon} 을 쓴다`);
    }
    // 홈 인사 배너만 마크업에 3D 아이콘을 둔다. 런처·머리 로고·첨부 단추는 마크업에 그림이 없다(평면 글리프).
    const html = read("widget.html");
    assert.deepEqual([...html.matchAll(/<img[^>]*src="\/icons\/([\w.-]+)"/g)].map((m) => m[1]), ["waving_hand.png"]);
    for (const id of ["launcher", "attachBtn"]) assert.doesNotMatch(html.match(new RegExp(`id="${id}"[^]*?</button>`))[0], /<img/, `${id} 에 그림이 있다`);
    // 평면 글리프 자리: 런처·머리 로고·첨부 단추는 mask 로 그린다.
    for (const sel of ["body.widget .launcher::before", "body.widget .w-head-avatar::before", "body.widget .w-attach-btn::before"]) {
      assert.ok(ruleFor(sel).some((r) => /var\(--glyph-/.test(r.decls.mask || "")), `${sel} 가 평면 글리프가 아니다`);
    }
    for (const sel of ["body.widget .w-hero-icon", "body.widget .empty .glyph img"]) {
      assert.ok(!ruleFor(sel).some((r) => r.decls.display === "none"), `${sel} 가 숨는다`);
    }
  });

  test("상대 줄의 작은 아바타와 이름은 묶음의 끝 줄에만 보인다", () => {
    // 다음 줄이 같은 사람(.same)이면 이름과 아바타를 숨기고, 묶음 끝 줄(같은 사람 줄 포함)에서는 다시 보인다.
    assert.equal(ruleFor("body.widget .row.theirs:has(+ .row.same) .who")[0]?.decls.display, "none");
    assert.equal(ruleFor("body.widget .row.theirs:has(+ .row.same) .avatar")[0]?.decls.visibility, "hidden");
    assert.equal(ruleFor("body.widget .row.theirs.same:not(:has(+ .row.same)) .who")[0]?.decls.display, "block");
    assert.equal(ruleFor("body.widget .row.theirs.same:not(:has(+ .row.same)) .avatar")[0]?.decls.visibility, "visible");
    // 이름(.who)은 말풍선 아래로 내려간다
    assert.equal(ruleFor("body.widget .row.theirs .stack > .who")[0]?.decls.order, "10");
  });

  // 검사 자체가 빈틈이면 위 테스트는 늘 통과한다. 잡아야 하는 모양과 잡으면 안 되는 모양을 둘 다 본다(pages_test.go 와 같은 목록).
  test("바깥 주소 검사는 프로토콜을 뺀 주소도 잡는다", () => {
    for (const bad of [`src="https://cdn.example.com/a.png"`, `url(http://x/a.png)`, `url("//cdn.example.com/x.png")`, `src="//cdn.example.com/x.png"`,
      `url( '//cdn.example.com/a.png')`, `src='//cdn.example.com/a.js'`, `SRC=//cdn.example.com/a.js`, `@import "//fonts.example.com/a.css"`]) {
      assert.match(bad, OUTSIDE_REF, `${bad} 를 바깥 주소로 잡지 못한다`);
    }
    for (const ok of [`src="/icons/x.png"`, `url("/icons/x.png")`, `src="\${ICON.balloon}"`, `// 주석`]) {
      assert.doesNotMatch(ok, OUTSIDE_REF, `${ok} 를 바깥 주소로 잘못 잡는다`);
    }
  });

  test("아이콘은 같은 서버의 /icons/ 에서만 받고 바깥 주소를 쓰지 않는다", () => {
    const files = new Set(readdirSync(here("./icons/")));
    assert.ok(files.has("LICENSE"), "라이선스 고지가 없다");
    for (const name of ["widget.html", "widget.css", "widget.js", "widget-view.js"]) {
      const text = read(name);
      assert.doesNotMatch(text, OUTSIDE_REF, `${name} 이 바깥 주소에서 자산을 받는다`);
      for (const [, icon] of text.matchAll(/\/icons\/([\w.-]+)/g)) assert.ok(files.has(icon), `${name} 의 /icons/${icon} 가 없다`);
    }
    for (const path of Object.values(ICON)) assert.ok(files.has(path.replace("/icons/", "")), `${path} 가 없다`);
  });
});

// 위젯의 시각·날짜 글자 모양(D44 보완). 시각은 이 기기의 시간대로 쓴다 — 테스트는 같은 기기에서 만든 시각(new Date(y, m, d, …))을 넣는다.
describe("위젯 시각과 상담 카드", () => {
  const local = (y, mo, d, h = 0, mi = 0) => new Date(y, mo - 1, d, h, mi).toISOString();

  test("상담 카드의 시각은 절대 시각 2026.10.06 14:15 꼴이다 — 몇 분 전처럼 흐르는 말로 쓰지 않는다", () => {
    // 서버가 주는 UTC 시각을 이 기기(서울) 시각으로 읽는다. 05:15Z 는 14:15, 날짜가 넘어가는 16:30Z 는 다음 날 01:30 이다.
    assert.equal(stampTime("2026-10-06T05:15:00Z"), "2026.10.06 14:15");
    assert.equal(stampTime("2026-10-05T16:30:00Z"), "2026.10.06 01:30");
    assert.equal(stampTime(local(2026, 10, 6, 14, 15)), "2026.10.06 14:15");
    assert.equal(stampTime(local(2026, 1, 2, 3, 4)), "2026.01.02 03:04");
    assert.equal(stampTime(new Date().toISOString()).length, 16, "방금 보낸 것도 시각이다");
    assert.equal(stampTime(""), "");
    assert.equal(stampTime("아님"), "");
  });

  test("대화의 날짜 구분선은 2026년 10월 06일 꼴이고 오늘도 날짜로 쓴다", () => {
    assert.equal(dayStamp(local(2026, 10, 6, 23, 59)), "2026년 10월 06일");
    assert.equal(dayStamp("2026-10-05T16:30:00Z"), "2026년 10월 06일", "서울 날짜로 읽는다");
    assert.equal(dayStamp(local(2026, 3, 9)), "2026년 03월 09일");
    const now = new Date();
    assert.equal(dayStamp(now.toISOString()), `${now.getFullYear()}년 ${String(now.getMonth() + 1).padStart(2, "0")}월 ${String(now.getDate()).padStart(2, "0")}일`);
    assert.equal(dayStamp(undefined), "");
  });

  test("상담 카드의 제목은 마지막 글이고 상담 제목이 있으면 그것이 제목, 마지막 글은 회색 보조 줄이다 — 채팅·시각과 진행중·종료 배지가 붙고 보류는 고객에게 진행중이다. 글 없이 파일만 보냈으면 파일을 보냈습니다, 고객·상담원 글이 없으면 아직 보낸 메시지가 없습니다다", () => {
    const at = local(2026, 10, 6, 14, 15);
    // 상담 제목이 없다(지금 고객 상담은 모두 이렇다) — 카드끼리 마지막 글로 가른다. 보조 줄은 없다.
    assert.deepEqual(convCard({ state: "OPEN", subject: "", last_body: "주문내역 확인", last_message_at: at }),
      { kind: "채팅", when: "2026.10.06 14:15", title: "주문내역 확인", sub: "", state: "진행중", open: true });
    assert.notEqual(convCard({ last_body: "환불 문의" }).title, convCard({ last_body: "배송 문의" }).title, "카드끼리 제목이 같다");
    assert.equal(convCard({ subject: "  ", last_body: "배송 문의" }).title, "배송 문의", "빈칸뿐인 제목은 없는 것이다");
    // 상담 제목이 있다(문의 유형) — 그것이 제목이고 마지막 글은 보조 줄이다.
    assert.deepEqual(convCard({ state: "OPEN", subject: "주문 확인", last_body: "주문내역 확인", last_message_at: at }),
      { kind: "채팅", when: "2026.10.06 14:15", title: "주문 확인", sub: "주문내역 확인", state: "진행중", open: true });
    assert.equal(convCard({ subject: "배송", last_sender_kind: "CUSTOMER", last_message_at: at }).sub, "파일을 보냈습니다");
    // 세 경우를 보낸 사람으로 가른다 — 글이 있으면 그 글, 보낸 사람만 있으면(파일만) 파일, 보낸 사람이 없으면 아직 없다.
    assert.equal(convCard({ last_body: "환불 문의", last_sender_kind: "CUSTOMER", last_message_at: at }).title, "환불 문의");
    assert.equal(convCard({ last_sender_kind: "CUSTOMER", last_message_at: at }).title, "파일을 보냈습니다");
    assert.equal(convCard({ last_sender_kind: "AGENT", last_message_at: at }).title, "파일을 보냈습니다");
    // 환영 글만 있는 상담은 시각(last_message_at)이 있어도 고객·상담원 글이 없다 — 파일이라고 쓰지 않는다.
    assert.equal(convCard({ last_message_at: at, created_at: at }).title, "아직 보낸 메시지가 없습니다");
    assert.equal(convCard({ created_at: at }).title, "아직 보낸 메시지가 없습니다");
    assert.equal(convCard({ created_at: at }).when, "2026.10.06 14:15", "메시지가 없으면 연 시각이다");
    assert.equal(convCard({ state: "SNOOZED" }).state, "진행중");
    assert.deepEqual([convCard({ state: "CLOSED" }).state, convCard({ state: "CLOSED" }).open], ["종료", false]);
  });

  test("위젯 말풍선의 시각은 24시간 14:15 꼴이다 — 데스크는 오전·오후 그대로다", () => {
    assert.equal(clockTime("2026-10-06T05:15:00Z"), "14:15");
    assert.equal(clockTime("2026-10-06T00:05:00Z"), "09:05");
    assert.equal(clockTime("2026-10-05T15:00:00Z"), "00:00");
    assert.equal(clockTime(""), "");
    assert.equal(clockTime("아님"), "");
  });
});

describe("위젯 머리·작성칸·알약 모양", () => {
  const rules = cssRules(read("widget.css"));
  const app = cssRules(read("app.css"));
  const ruleFor = (list, selector) => list.filter((r) => r.selectors.includes(selector));

  test("머리의 이름은 크고 굵고 로고 칸은 34px 이다 — 연결 상태는 연결돼 있으면 점 하나만 보인다", () => {
    const h1 = ruleFor(rules, "body.widget .m-head h1")[0];
    assert.equal(h1?.decls["font-size"], "20px");
    assert.equal(h1?.decls["font-weight"], "800");
    const logo = ruleFor(rules, "body.widget .w-head-avatar")[0];
    assert.deepEqual([logo?.decls.width, logo?.decls.height], ["34px", "34px"]);
    // 글자는 화면에서 빠지지만 화면 읽기에는 남는다(display: none 이 아니다)
    const hidden = ruleFor(rules, "body.widget .m-head .status[data-state=online] #statusText")[0];
    assert.ok(hidden, "연결됨 글자를 감추는 규칙이 없다");
    assert.equal(hidden.decls.position, "absolute");
    assert.equal(hidden.decls["clip-path"], "inset(50%)");
    assert.equal(hidden.decls.display, undefined);
  });

  // WCAG 1.4.11(비텍스트 대비): 초점 표시는 둘레 색과 3:1 이상이어야 한다. 연한 회색 테두리(--line)와 옅은 그림자는 1.1~1.3:1 이라 모자랐다(감사 18).
  test("입력 상자는 초점이 오면 테두리가 진한 회색(--ink-3) 2px 로 바뀌고 둘레와 3:1 이상이다", () => {
    const box = ruleFor(app, ".composer.composer-boxed .input-row")[0];
    assert.equal(box?.decls.border, "1px solid var(--line)");
    const focus = ruleFor(app, ".composer.composer-boxed .input-row:focus-within")[0];
    assert.equal(focus?.decls["border-color"], "var(--ink-3)", "초점 때 테두리가 진한 회색이 아니다");
    // 테두리 1px 에 같은 색 바깥 테 1px 를 더해 2px 로 보인다(칸 크기는 그대로)
    assert.equal(focus?.decls["box-shadow"], "0 0 0 1px var(--ink-3)");
    // 위젯이 이 색을 다시 칠하지 않는다(관리자 색으로 바꾸지 않는다)
    for (const r of rules) {
      if (!r.selectors.some((sel) => /input-row:focus-within$/.test(sel))) continue;
      assert.fail(`${r.selectors.join(",")} 가 초점 표시를 다시 정한다`);
    }
    // 실제 색으로 잰다: 초점 테두리(--ink-3)와 상자 안·밖 바탕(--panel·--bg)의 대비. 테마는 :root 하나다
    const lum = (hex) => {
      const c = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255).map((v) => (v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4));
      return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2];
    };
    const ratio = (a, b) => { const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p); return (x + 0.05) / (y + 0.05); };
    const root = app.find((r) => r.selectors.includes(":root")).decls;
    for (const around of ["--panel", "--bg"]) {
      const v = ratio(root["--ink-3"], root[around]);
      assert.ok(v >= 3, `초점 테두리 ${root["--ink-3"]} 와 ${around} ${root[around]} 대비 ${v.toFixed(2)}:1`);
    }
  });

  test("위젯은 운영체제가 어두운 모드여도 흰 바탕이다", () => {
    for (const css of [rules, app]) {
      const dark = css.filter((r) => r.media.includes("prefers-color-scheme: dark"));
      assert.deepEqual(dark.map((r) => r.selectors.join(",")), [], "어두운 모드에서 색을 다시 정하는 규칙이 있다");
    }
    assert.equal(app.find((r) => r.selectors.includes(":root")).decls["--bg"], "#ffffff");
  });

  test("홈의 질문 알약은 한 줄에 두 개씩 놓이고 폭이 모자라면 둘이 나눠 가진 뒤 말줄임표로 잘린다", () => {
    const faq = ruleFor(rules, "body.widget .w-faq")[0];
    assert.equal(faq?.decls.display, "grid");
    assert.equal(faq?.decls["grid-template-columns"], "repeat(2, minmax(0, max-content))");
    assert.equal(ruleFor(rules, "body.widget .w-faq .pill")[0]?.decls["min-width"], "0");
  });
});
