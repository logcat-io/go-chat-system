// 디자인 시스템(D44)의 정책 테스트.
//
// 컴파일된 app.css·widget.css(web/styles/ 의 SCSS 에서 나온다)와 화면 마크업을 글자로 읽는다. 단언은 화면에 보이는 규칙이다 —
// 모서리, 알약, 아코디언, 아바타 색, 인라인 style 이 없는 마크업. SCSS 와 컴파일 결과가 같은지는 npm run css:check 가 본다.
import { test, describe } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import { register } from "node:module";

register(
  "data:text/javascript," +
    encodeURIComponent(`export async function resolve(spec, ctx, next) {
      if (spec === "/chat.js") return next(new URL("./chat.js", ctx.parentURL).href, ctx);
      return next(spec, ctx);
    }`),
);
const { AVATAR_COLORS, avatarHTML, avatarTone } = await import("./ui.js");

const read = (name) => readFileSync(new URL(name, import.meta.url), "utf8");

// cssRules 는 CSS 를 [{ media, selectors, decls }] 로 편다(widget-view.test.mjs 와 같은 방식, @media 한 겹).
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
      if (head.startsWith("@media") || head.startsWith("@supports")) walk(inner, head);
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

const app = cssRules(read("app.css"));
const widget = cssRules(read("widget.css"));
const all = [...app, ...widget];
const ruleFor = (rules, selector) => rules.filter((r) => r.selectors.includes(selector));
const root = app.find((r) => r.selectors.includes(":root"));

describe("디자인 시스템", () => {
  test("모서리는 4px 이 기준이고 작은 것은 2px 이며 알약과 원만 예외다", () => {
    assert.equal(root.decls["--r"], "4px");
    assert.equal(root.decls["--r-xs"], "2px");
    assert.equal(root.decls["--r-pill"], "999px");
    assert.equal(root.decls["--r-circle"], "50%");
    // 화면의 모든 모서리는 이 넷(또는 없음·물려받기) 가운데 하나다. 10px·16px·20px 같은 값을 직접 쓰지 않는다.
    const ALLOWED = new Set(["var(--r)", "var(--r-xs)", "var(--r-pill)", "var(--r-circle)", "0", "inherit"]);
    for (const r of all) {
      for (const [prop, value] of Object.entries(r.decls)) {
        if (!/radius$/.test(prop)) continue;
        for (const part of value.split(/\s+/)) {
          assert.ok(ALLOWED.has(part), `${r.selectors.join(",")} 의 ${prop}: ${value} 가 모서리 토큰이 아니다`);
        }
      }
    }
  });

  test("알약 버튼은 흰 바탕·회색 테두리·진한 글자이고 긴 질문은 한 줄로 잘린다", () => {
    const pill = ruleFor(app, ".pill").find((r) => r.decls["border-radius"]);
    assert.equal(pill.decls["border-radius"], "var(--r-pill)");
    assert.equal(pill.decls.background, "var(--panel)");
    assert.equal(pill.decls.border, "1px solid var(--line)");
    assert.equal(pill.decls.color, "var(--ink)");
    assert.equal(pill.decls["text-align"], "left");
    const text = ruleFor(app, ".pill > span")[0];
    assert.equal(text?.decls["white-space"], "nowrap");
    assert.equal(text?.decls["text-overflow"], "ellipsis");
    assert.equal(ruleFor(app, ".pills")[0]?.decls["flex-wrap"], "wrap");
  });

  test("자주 묻는 질문 화면의 질문 줄은 왼쪽 정렬·같은 최소 높이·구분선이고 펼치면 꺾쇠가 뒤집힌다", () => {
    const trigger = ruleFor(app, ".accordion-trigger").find((r) => r.decls.display);
    assert.equal(trigger.decls["text-align"], "left");
    assert.equal(trigger.decls.width, "100%");
    assert.equal(trigger.decls["min-height"], "56px");
    assert.equal(ruleFor(app, ".accordion-item")[0]?.decls["border-bottom"], "1px solid var(--line)");
    assert.equal(ruleFor(app, ".accordion-trigger[aria-expanded=true]::after")[0]?.decls.transform, "rotate(180deg)");
    // 답은 줄바꿈을 살리고 긴 주소도 칸 안에서 끊는다
    const panel = ruleFor(app, ".accordion-panel")[0];
    assert.equal(panel.decls["white-space"], "pre-line");
    assert.equal(panel.decls["overflow-wrap"], "anywhere");
  });

  test("아바타 색은 디자인 시스템의 여덟 톤이고 화면 코드는 인라인 색 없이 톤 클래스만 붙인다", () => {
    AVATAR_COLORS.forEach((color, i) => {
      assert.equal(ruleFor(app, `.avatar.tone-${i}`)[0]?.decls.background, color, `.avatar.tone-${i} 가 AVATAR_COLORS[${i}] 와 다르다`);
    });
    const html = avatarHTML("김상담", "sm");
    assert.equal(html, `<div class="avatar tone-${avatarTone("김상담")} sm">김</div>`);
    assert.doesNotMatch(html, /style=/);
  });

  // 인라인 스타일은 디자인 시스템을 비켜 간다(색·간격·모서리·배치가 토큰 밖으로 샌다). 정책 645.
  // 화면 마크업과 화면 코드에서 실수로 들어오는 꼴을 찾는다 — style 속성(마크업·템플릿·innerHTML 글자, 대소문자 무시), <style> 블록·글자,
  // 요소의 .style 접근(el.style.x =, style.setProperty, style.cssText, Object.assign(el.style, …), 별칭), ["style"], 구조 분해,
  // setAttribute("style")·createAttribute("style")(대소문자 무시, 템플릿 인자 포함), cssText, attributeStyleMap, createElement("style")(대소문자 무시).
  // 찾은 줄은 아래 예외 목록에 있어야 한다. 예외는 데이터에 따라 값이 정해지는 것뿐이다.
  // ESLint(no-restricted-properties·no-restricted-syntax)가 같은 규칙을 구문 트리로 한 번 더 막는다(구조 분해 const { style } = el 도).
  // 막지 않는 것: 일부러 비켜 가는 꼴(insertRule, adoptedStyleSheets, Reflect.set(el, "style"), "st" + "yle" 처럼 조립한 이름, 변수에 담은 속성 이름,
  // animate 의 fill) — 글자나 구문으로 가려낼 수 없고 실수로 쓰는 꼴이 아니라 리뷰가 막는다(D44 보완).
  const STYLE_EXCEPTIONS = [
    { file: "ui.js", line: /^const ratio = att\.width && att\.height \? ` style="aspect-ratio:\$\{att\.width\}\/\$\{att\.height\}"` : "";$/, why: "사진 비율(사진마다 다른 데이터)" },
    { file: "ui.js", line: /^textarea\.style\.height = "auto";$/, why: "입력칸 자동 높이(쓴 글에 따라 정해진다)" },
    { file: "ui.js", line: /^textarea\.style\.height = `\$\{Math\.min\(textarea\.scrollHeight, max\)\}px`;$/, why: "입력칸 자동 높이" },
    { file: "ui.js", line: /^document\.documentElement\.style\.setProperty\(name, value\);$/, why: "관리자 색 CSS 변수(--brand 다섯, 실행 중에 바뀐다)" },
    { file: "widget.js", line: /^bar\.querySelector\("i"\)\.style\.width = `\$\{p\.percent\}%`;$/, why: "업로드 진행률 막대 폭" },
  ];
  // embed.js 는 고객 사이트 페이지에서 돈다. 그곳에는 이 리포의 스타일시트가 없어 iframe 의 자리·크기를 인라인으로만 정한다.
  const STYLE_EXEMPT_FILES = new Set(["embed.js"]);
  const STYLE_PATTERNS = [/\bstyle\s*=/i, /<style\b/i, /\.style\b/, /\[\s*["'`]style["'`]\s*\]/i, /\{[^}]*\bstyle\b[^}]*\}\s*=\s*\w/,
    /\b(setAttribute|setAttributeNS|toggleAttribute|createAttribute|createAttributeNS)\([^)]*["'`]style["'`]/i,
    /\bcssText\b/, /\battributeStyleMap\b/, /createElement(NS)?\([^)]*["'`]style["'`]/i];
  // styleHits 는 파일에서 스타일에 닿는 줄을 [{ file, line }] 로 돌려준다. 주석으로 시작하는 줄(//, *, <!--)은 보지 않는다.
  // 줄 끝 주석은 떼지 않는다 — 글자 안의 "//" 를 주석으로 알고 뒤의 코드를 버리면 그 줄의 스타일을 놓친다(감사 18 A18).
  function styleHits(name, text) {
    const out = [];
    for (const raw of text.split("\n")) {
      const line = raw.trim();
      if (/^(\/\/|\*|\/\*|<!--)/.test(line)) continue;
      if (STYLE_PATTERNS.some((re) => re.test(line))) out.push({ file: name, line });
    }
    return out;
  }
  const screenFiles = () => readdirSync(new URL(".", import.meta.url)).filter((n) => /\.(html|js)$/.test(n) && !STYLE_EXEMPT_FILES.has(n));

  test("화면 마크업과 화면 코드는 인라인 스타일을 쓰지 않는다 — 사진 비율·입력칸 높이·진행률 폭·관리자 색 변수만 예외다", () => {
    const files = screenFiles();
    for (const must of ["index.html", "desk.html", "widget.html", "ui.js", "desk.js", "widget.js", "chat.js", "widget-view.js"]) {
      assert.ok(files.includes(must), `${must} 을 보지 않는다`);
    }
    const used = new Set();
    for (const name of files) {
      for (const hit of styleHits(name, read(name))) {
        const i = STYLE_EXCEPTIONS.findIndex((e) => e.file === hit.file && e.line.test(hit.line));
        assert.ok(i >= 0, `${hit.file} 의 인라인 스타일: ${hit.line}`);
        used.add(i);
      }
    }
    // 예외 목록이 낡지 않았다 — 지운 코드의 예외가 남아 새 코드를 덮지 않는다.
    assert.deepEqual([...STYLE_EXCEPTIONS.keys()].filter((i) => !used.has(i)), [], "쓰이지 않는 예외가 있다");
    // 찾는 길이 실제로 잡는지 본다(변이 M9·M14, 감사 18 A7·A8·A10·A11·A15·A16·A18·A20·H1~H3 과 같은 꼴).
    for (const bad of [`wrap.style.color = "#ff0000";`, `el.setAttribute("style", "color:red")`, `el.style.setProperty("color", "red")`,
      `el.style.cssText = "color:red"`, `Object.assign(el.style, { color: "red" })`, `const { style } = el;`, `el["style"].color = "red"`,
      `return \`<b style="color:red">\`;`, `document.createElement("style")`,
      `el.setAttribute("STYLE", "color:red");`, "el.setAttribute(`style`, \"color:red\");", `el.setAttributeNS(null, "style", "x");`,
      `el.innerHTML = '<b STYLE="color:red">x</b>';`, `document.head.insertAdjacentHTML("beforeend", "<style>body{color:red}</style>");`,
      `el.innerHTML = "<STYLE>b{}</STYLE>";`, `const a = document.createAttribute("style");`, `const u = "a // b"; el.style.color = "red";`,
      `document.head.append(document.createElement("STYLE"));`, `document.createElementNS(svg, "style")`]) {
      assert.equal(styleHits("x.js", bad).length, 1, `${bad} 를 못 잡는다`);
    }
    for (const bad of [`<div style="color:red"></div>`, `<style>body{color:red}</style>`, `<div STYLE="color:red"></div>`, `<STYLE media="all">`]) {
      assert.equal(styleHits("x.html", bad).length, 1, `${bad} 를 못 잡는다`);
    }
    // 스타일이 아닌 글자는 잡지 않는다
    for (const ok of [`<link rel="stylesheet" href="/app.css">`, `const lifestyle = 1;`, `el.dataset.styled = "1";`]) {
      assert.equal(styleHits("x.js", ok).length, 0, `${ok} 를 잘못 잡는다`);
    }
  });

  test("데스크의 대화 칸은 자기 안에서만 스크롤한다 — 대화가 길어도 위 막대와 목록이 밀려 올라가지 않는다", () => {
    assert.equal(ruleFor(app, ".desk")[0]?.decls["grid-template-rows"], "52px minmax(0, 1fr)");
    assert.equal(ruleFor(app, ".desk")[0]?.decls.overflow, "hidden");
    const thread = ruleFor(app, ".thread")[0];
    assert.equal(thread.decls["grid-template-rows"], "auto minmax(0, 1fr) auto auto");
    assert.equal(thread.decls.overflow, "hidden");
    assert.equal(ruleFor(app, ".desk-body > *")[0]?.decls["min-height"], "0");
  });

  test("긴 파일 이름과 긴 상담원 이름은 한 줄로 잘리고 칸 밖으로 나가지 않는다", () => {
    for (const [rules, sel] of [[app, ".attach.file .name"], [app, ".attach.file .body"], [app, ".row .who"], [widget, "body.widget .w-pill-name"]]) {
      const r = ruleFor(rules, sel)[0];
      assert.ok(r, `${sel} 규칙이 없다`);
      assert.equal(r.decls["min-width"], "0", `${sel} 가 줄어들지 못한다`);
    }
    for (const sel of [".attach.file .name", ".row .who"]) {
      assert.equal(ruleFor(app, sel)[0].decls["text-overflow"], "ellipsis", `${sel} 가 잘리지 않는다`);
    }
    // 위젯 상대 줄의 이름은 시각 자리를 남기고 줄어들며(말줄임표) 시각은 줄어들지도 꺾이지도 않는다
    const who = ruleFor(widget, "body.widget .row.theirs .stack > .who")[0];
    assert.equal(who.decls.flex, "0 1 auto");
    assert.match(who.decls["max-width"], /^calc\(100% - \d+px\)$/);
    assert.equal(ruleFor(widget, "body.widget .row.theirs .stack > .meta")[0]?.decls.flex, "none");
    assert.equal(ruleFor(app, ".meta")[0]?.decls["white-space"], "nowrap");
  });

  // specificity 는 선택자 하나의 구체성 [id, 클래스·속성·가상 클래스, 요소] 다. 이 테스트가 쓰는 꼴(:has·:not 없음)만 센다.
  const specificity = (sel) => [
    (sel.match(/#[\w-]+/g) || []).length,
    (sel.match(/\.[\w-]+|\[[^\]]+\]|:(?!:)[\w-]+/g) || []).length,
    (sel.replace(/[#.][\w-]+|\[[^\]]+\]|::?[\w-]+/g, " ").match(/[a-z][\w-]*/gi) || []).length,
  ];
  const beats = (a, b) => { const x = specificity(a), y = specificity(b); for (let i = 0; i < 3; i++) if (x[i] !== y[i]) return x[i] > y[i]; return false; };

  test("고객 말풍선 안의 링크는 말풍선 글자색이다 — 관리자 색 위에 관리자 색 글자를 얹지 않는다", () => {
    // 공용 컴포넌트가 정하고(위젯·데스크 같이), 위젯의 링크 색 규칙들보다 구체적이라 위젯에서도 이긴다.
    const own = ".row.mine .bubble a";
    assert.equal(ruleFor(app, own)[0]?.decls.color, "inherit");
    for (const r of widget) {
      if (r.decls.color === undefined || r.decls.color === "inherit") continue;
      for (const sel of r.selectors) {
        if (!/(^| )a(:[\w-]+)?$/.test(sel)) continue; // 링크를 칠하는 규칙만
        assert.ok(beats(own, sel), `${sel} 의 ${r.decls.color} 가 고객 말풍선 링크 색을 이긴다`);
      }
    }
  });

  // 데스크 대화의 쪽(정책 824·825). 쪽은 .out 이 정하고 "내 것"(.mine)은 쪽을 정하지 않는다 — 동료 상담원 글도 오른쪽이다.
  test("말풍선의 쪽은 .out 이 정하고 .mine 은 쪽을 정하지 않는다 — 동료 상담원 말풍선은 채우지 않은 외곽선이고 내부 메모 모양이 이긴다", () => {
    assert.equal(ruleFor(app, ".row.out")[0]?.decls["flex-direction"], "row-reverse");
    assert.equal(ruleFor(app, ".row.out .stack")[0]?.decls["align-items"], "flex-end");
    assert.equal(ruleFor(app, ".row.out .meta")[0]?.decls["flex-direction"], "row-reverse");
    for (const sel of [".row.mine", ".row.mine .stack", ".row.mine .meta"]) {
      assert.ok(!ruleFor(app, sel).some((r) => r.decls["flex-direction"] || r.decls["align-items"]), `${sel} 가 쪽을 정한다`);
    }
    const peer = ruleFor(app, ".row.peer:not(.internal) .bubble")[0];
    assert.equal(peer?.decls.background, "var(--panel)");
    assert.equal(peer?.decls["border-color"], "var(--brand)");
  });

  // 좁은 데스크(정책 826~828). 폭 기준점은 tokens 의 tablet(900px)·desk-3col(1280px) 이다.
  const inMedia = (q, sel) => app.filter((r) => r.media === `@media (max-width: ${q})` && r.selectors.includes(sel));
  const outMedia = (sel) => app.filter((r) => r.media === "" && r.selectors.includes(sel));
  const declOf = (rules, prop) => rules.map((r) => r.decls[prop]).filter(Boolean).at(-1);

  test("900px 이하에서는 받은함 목록과 대화가 한 칸씩 보이고 대화 머리에 목록으로 단추가 보인다", () => {
    assert.equal(declOf(outMedia(".back-btn"), "display"), "none", "넓은 화면에 목록으로 단추가 보인다");
    assert.equal(declOf(inMedia("900px", ".back-btn"), "display"), "inline-grid");
    assert.equal(declOf(inMedia("900px", ".list"), "display"), "none");
    assert.equal(declOf(inMedia("900px", ".desk.show-list .list"), "display"), "flex");
    assert.equal(declOf(inMedia("900px", ".desk.show-list .thread"), "display"), "none");
  });

  test("900px 이하에서는 메뉴 열기 단추가 보이고 받은함 필터와 설정 메뉴는 그 단추로 여는 왼쪽 서랍이다 — 닫힌 서랍은 감춰 Tab 이 닿지 않는다", () => {
    assert.equal(declOf(outMedia(".nav-toggle"), "display"), "none", "넓은 화면에 메뉴 열기 단추가 보인다");
    assert.equal(declOf(inMedia("900px", ".nav-toggle"), "display"), "inline-grid", "좁은 화면에 메뉴 열기 단추가 없다");
    for (const panel of [".filters", ".settings-nav"]) {
      const closed = inMedia("900px", panel);
      assert.equal(declOf(closed, "position"), "fixed", `${panel} 가 서랍이 아니다`);
      assert.equal(declOf(closed, "visibility"), "hidden", `닫힌 ${panel} 에 Tab 이 닿는다`);
      assert.equal(declOf(closed, "transform"), "translateX(-100%)");
      const open = inMedia("900px", `.desk.nav-open ${panel}`);
      assert.equal(declOf(open, "visibility"), "visible", `열린 ${panel} 가 보이지 않는다`);
      assert.equal(declOf(open, "transform"), "none");
    }
    // 서랍은 위 막대 아래에서 열리고 그 뒤를 막이 덮는다. 위 막대의 메뉴와 모달은 서랍 위에 뜬다
    assert.equal(declOf(outMedia(".drawer-scrim"), "position"), "fixed");
    const z = (sel, media) => Number(declOf(media ? inMedia(media, sel) : outMedia(sel), "z-index"));
    assert.ok(z(".drawer-scrim") < z(".filters", "900px") && z(".filters", "900px") < z(".menu") && z(".menu") < z(".scrim"));
  });

  test("1280px 이하에서는 고객 정보가 대화 머리의 고객 정보 단추로 여는 오른쪽 서랍이고 대화를 열지 않았으면 단추가 없다", () => {
    assert.equal(declOf(outMedia(".profile-toggle"), "display"), "none");
    assert.equal(declOf(inMedia("1280px", ".profile-toggle"), "display"), "inline-flex");
    assert.equal(declOf(inMedia("1280px", ".toolbar[hidden] ~ .profile-toggle"), "display"), "none");
    const closed = inMedia("1280px", ".profile");
    assert.equal(declOf(closed, "position"), "fixed");
    assert.equal(declOf(closed, "visibility"), "hidden");
    assert.equal(declOf(closed, "transform"), "translateX(100%)");
    assert.equal(declOf(inMedia("1280px", ".desk.profile-open .profile"), "visibility"), "visible");
    const html = read("desk.html");
    assert.match(html, /<button class="icon-btn nav-toggle" id="navToggle" type="button" aria-label="메뉴" aria-expanded="false" aria-controls="filters settingsNav">/);
    assert.match(html, /<button class="btn sm profile-toggle" id="profileToggle" type="button" aria-expanded="false" aria-controls="profile">/);
  });

  // ADR 0005: 위젯은 공용 컴포넌트(작성칸·말풍선·첨부)의 색을 다시 칠하지 않는다. 색이 다른 모양은 컴포넌트의 변형 클래스
  // (.log-plain, .composer-boxed)로 고르고 위젯 파일은 크기·여백·배치와 위젯에만 있는 장식만 덮는다.
  test("위젯은 공용 작성칸·말풍선·첨부의 색을 다시 칠하지 않는다 — 색은 컴포넌트의 변형이 정하고 위젯은 크기만 덮는다", () => {
    const SHARED = /\.(composer|input-row|send|attach-pill|bubble|attach|att-video-save)(?=$|[.:[\s])|(^|\s)textarea(?=$|[.:[])/;
    const COLOR_PROPS = ["color", "background-color", "border-color", "border-top-color", "box-shadow", "outline", "outline-color", "fill", "stroke"];
    const colorish = (prop, v) => COLOR_PROPS.includes(prop) || (prop === "background" && !/^url\(/.test(v)) || (/^border(-top|-bottom|-left|-right)?$/.test(prop) && !/^(0|none)$/.test(v));
    for (const r of widget) {
      for (const sel of r.selectors) {
        const last = sel.split(/\s*[ >+~]\s*/).at(-1); // 규칙이 칠하는 요소(마지막 칸)
        if (!SHARED.test(last) || /::(before|after)$/.test(last)) continue;
        for (const [prop, v] of Object.entries(r.decls)) {
          assert.ok(!colorish(prop, v), `${sel} 가 공용 컴포넌트의 ${prop}: ${v} 를 다시 칠한다`);
        }
      }
    }
    // 위젯이 고른 변형이 실제로 컴포넌트에 있다
    assert.ok(ruleFor(app, ".log-plain .row.theirs .bubble")[0], "본문형 대화 변형이 없다");
    assert.ok(ruleFor(app, ".composer.composer-boxed .input-row")[0], "상자형 작성칸 변형이 없다");
    assert.match(read("widget.html"), /class="log log-plain"/);
    assert.match(read("widget.html"), /class="composer composer-boxed"/);
  });
});
