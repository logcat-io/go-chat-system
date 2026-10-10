// ESLint 설정(flat config). 화면 코드(web/assets/*.js)는 브라우저 모듈, 클라이언트 테스트(*.test.mjs)와 scripts/ 는 node 다.
// npm run lint:js — 경고도 실패로 본다(--max-warnings 0).
import js from "@eslint/js";
import globals from "globals";

export default [
  { ignores: ["node_modules/", "bin/", "data/", "docs/", "deploy/"] },
  js.configs.recommended,
  {
    linterOptions: { reportUnusedDisableDirectives: "error" },
    rules: {
      "no-unused-vars": ["error", { argsIgnorePattern: "^_", caughtErrors: "none" }],
      eqeqeq: ["error", "always", { null: "ignore" }],
      "no-var": "error",
      "prefer-const": "error",
      "no-implicit-globals": "error",
    },
  },
  {
    files: ["web/assets/**/*.js"],
    languageOptions: { sourceType: "module", globals: { ...globals.browser } },
    // 정책 645: 화면 코드는 인라인 스타일을 쓰지 않는다 — 색·간격·배치는 디자인 시스템(web/styles/)의 클래스로 고른다.
    // 요소의 style 에 닿는 길(el.style.x =, style.setProperty, style.cssText, Object.assign(el.style, …), const { style } = el)과
    // style 속성·<style> 을 만드는 길(setAttribute·createAttribute("style"), createElement("style"), 마크업 글자의 style=·<style)을 막는다.
    // 속성 이름은 대소문자를 가리지 않고(HTML 이 그렇다) 템플릿 글자 인자(`style`)도 본다.
    // 일부러 비켜 가는 꼴(insertRule, adoptedStyleSheets, Reflect.set, "st" + "yle")은 막지 않는다 — 리뷰가 막는다(D44 보완).
    // 예외는 데이터에 따라 값이 정해지는 넷뿐이고 줄마다 eslint-disable-next-line 과 이유를 단다(design-system.test.mjs 의 목록과 같다).
    rules: {
      "no-restricted-properties": [
        "error",
        { property: "style", message: "인라인 스타일 대신 디자인 시스템 클래스를 쓴다(정책 645)" },
        { property: "cssText", message: "인라인 스타일 대신 디자인 시스템 클래스를 쓴다(정책 645)" },
        { property: "attributeStyleMap", message: "인라인 스타일 대신 디자인 시스템 클래스를 쓴다(정책 645)" },
      ],
      "no-restricted-syntax": [
        "error",
        // 첫 인자가 "style"(글자·인자 없는 템플릿, 대소문자 무시)인 부름. NS 판은 둘째 인자다.
        ...[
          ["setAttribute|toggleAttribute|createAttribute", 0, "style 속성을 만들지 않는다(정책 645)"],
          ["setAttributeNS|createAttributeNS", 1, "style 속성을 만들지 않는다(정책 645)"],
          ["createElement", 0, "<style> 을 만들지 않는다 — 스타일은 web/styles/ 에 둔다(정책 645)"],
          ["createElementNS", 1, "<style> 을 만들지 않는다 — 스타일은 web/styles/ 에 둔다(정책 645)"],
        ].flatMap(([names, i, message]) => [
          { selector: `CallExpression[callee.property.name=/^(${names})$/][arguments.${i}.value=/^style$/i]`, message },
          { selector: `CallExpression[callee.property.name=/^(${names})$/][arguments.${i}.quasis.0.value.cooked=/^style$/i]`, message },
        ]),
        { selector: "TemplateElement[value.raw=/\\bstyle\\s*=|<style\\b/i]", message: "마크업에 style 속성이나 <style> 을 쓰지 않는다(정책 645)" },
        { selector: "Literal[value=/\\bstyle\\s*=|<style\\b/i]", message: "마크업에 style 속성이나 <style> 을 쓰지 않는다(정책 645)" },
      ],
    },
  },
  {
    // embed.js 는 고객 사이트 페이지에서 도는 고전 스크립트(모듈 아님)다. 오래된 호스트 페이지와 섞여도 되게 var 로 쓴다.
    files: ["web/assets/embed.js"],
    // 고객 사이트에는 이 리포의 스타일시트가 없다. iframe 의 자리와 크기는 인라인 style 로만 정한다(정책 645 의 예외 — 파일 전체).
    languageOptions: { sourceType: "script" },
    rules: { "no-var": "off", "no-restricted-properties": "off", "no-restricted-syntax": "off" },
  },
  {
    // 클라이언트 테스트는 node 에서 가짜 document·window 를 globalThis 에 깔고 화면 코드를 그대로 띄운다.
    files: ["web/assets/**/*.test.mjs"],
    languageOptions: { sourceType: "module", globals: { ...globals.node, ...globals.browser } },
  },
  {
    files: ["scripts/**/*.mjs", "*.config.js", "*.config.mjs"],
    languageOptions: { sourceType: "module", globals: { ...globals.node } },
  },
];
