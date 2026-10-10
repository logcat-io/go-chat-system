// Stylelint 설정. SCSS 소스(web/styles/)만 본다 — web/assets/*.css 는 컴파일 결과라 보지 않는다.
// npm run lint:css — 경고도 실패로 본다(--max-warnings 0).
export default {
  extends: ["stylelint-config-standard-scss"],
  rules: {
    // 화면 코드와 테스트가 쓰는 요소 id 는 camelCase 다(#dropAttach, #cancelUpload, uitest 선택자). 바꾸지 않는다.
    "selector-id-pattern": null,
    // 컴포넌트 클래스는 kebab-case 다(.accordion-trigger, .w-head-avatar). 숫자 붙은 톤(.tone-0)도 허용한다.
    "selector-class-pattern": ["^[a-z][a-z0-9]*(-[a-z0-9]+)*$", { message: "클래스는 kebab-case 로 쓴다" }],
    // :not(.a):not(.b) 를 쓴다. 위젯 스타일 테스트가 선택자를 쉼표로 나눠 body.widget 아래인지 보기 때문이다(정책 505).
    "selector-not-notation": "simple",
    // iOS 15.4 이전은 mask 에 접두어가 필요하다. 여러 줄 자르기(-webkit-line-clamp)는 접두어 말고 방법이 없다.
    "property-no-vendor-prefix": [true, { ignoreProperties: ["mask", "-webkit-mask", "box-orient", "line-clamp"] }],
    // 글꼴 이름은 원래 표기대로 쓴다(--sans·--mono).
    "value-keyword-case": ["lower", { ignoreProperties: ["/^--(sans|mono)$/"] }],
    "value-no-vendor-prefix": [true, { ignoreValues: ["box"] }],
    // 토큰 이름에 숫자 단계를 쓴다(--ink-2, --line-2).
    "custom-property-pattern": "^[a-z][a-z0-9]*(-[a-z0-9]+)*$",
    // Sass 변수는 토큰 파일에서만 정한다(아래 overrides). 화면 파일에서 `$zz: 7px; padding: $zz;` 로 값 규칙을 비켜 가지 못하게
    // 토큰 밖의 변수 선언은 어떤 이름도 통과하지 못하는 패턴으로 막는다(감사 19 C13). 믹스인·함수의 인자는 선언이 아니라 걸리지 않는다.
    "scss/dollar-variable-pattern": ["^(?!)", { message: "Sass 변수는 토큰 파일(web/styles/tokens/·pages/widget/_tokens.scss)에서만 정한다 — 화면 파일은 토큰을 쓴다" }],
    // 같은 까닭으로 값을 돌려주는 함수·믹스인은 tools/·tokens/ 에서만 정한다(@function zz() { @return 7px; } 우회).
    "at-rule-disallowed-list": [["function", "mixin"], { message: "@function·@mixin 은 web/styles/tools/·tokens/ 에서만 정한다" }],
    // 색은 #rrggbb 로 쓴다 — ui.test.mjs 가 :root 의 여섯 자리 값을 읽어 대비를 잰다.
    "color-hex-length": "long",
    // 한국어 주석이 긴 줄이 많다. 규칙의 순서는 컴포넌트 안에서 읽기 좋은 순서(배치 → 상자 → 글자)를 따른다.
    "no-descending-specificity": null,
    // 토큰 밖으로 새는 값을 막는다(D44). 색은 var(--…), 글자 크기는 font-size(), 간격은 space() 한 단계, 모서리는 var(--r…) 만 쓴다.
    // 색 글자(#hex·rgb()·이름)와 글자 크기 숫자는 토큰 파일(아래 overrides)에서만 쓴다. 토큰에 셈을 붙여 비켜 가지 않는다 —
    // space(n) ± Npx, space(n) * k, calc(#{space(n)} + Npx), font-size("xs") + 1px, 13 * 1px 처럼. 칸이 모자라면 $space·$font-size 에 단계를 더한다.
    "color-no-hex": [true, { message: "색은 토큰(var(--…))으로 쓴다. 새 색은 web/styles/tokens/_color.scss 에 더한다" }],
    "color-named": ["never", { message: "색은 토큰(var(--…))으로 쓴다" }],
    "declaration-property-value-disallowed-list": [
      {
        "/.*/": [
          "/\\b(rgba?|hsla?|hwb|lab|lch|oklab|oklch)\\(/",
          // 토큰 함수 뒤에 셈: space(2) + 1px, space(2) * 1.5, #{space(2)} + 1px, font-size("xs") - 1px
          "/\\b(space|font-size)\\([^)]*\\)\\}?\\s*[-+*/%]\\s*-?[0-9.]/",
          // 숫자 뒤에 토큰 함수: 1px + space(2), 2 * #{space(1)}
          "/[0-9.](px|rem|em)?\\s*[-+*/]\\s*(#\\{)?(space|font-size)\\(/",
          // 숫자끼리 곱하기: 13 * 1px, 2px * 3
          "/[0-9.](px|rem|em|%)?\\s*\\*\\s*-?[0-9.]/",
          // calc 안의 생 px 셈: calc(100% + 6px). 간격이면 #{space(n)}, 크기에서 나온 값이면 그 토큰(var(--w-tap) 등)으로 쓴다
          "/calc\\([^;]*[-+]\\s*[0-9.]+px/",
        ],
        // 간격 속성은 space() 만 쓴다(생 px 금지 — 3·5·7px 같은 단계 밖 값이 여기로 샜다)
        "/^(padding|margin|gap|row-gap|column-gap|inset|scroll-padding|scroll-margin)(-.+)?$/": ["/[0-9](px|rem|em)\\b/"],
        font: ["/[0-9.]+(px|rem|em|pt|%)/"],
      },
      { message: "토큰을 비켜 가는 값이다 — 색은 var(--…), 글자 크기는 font-size(), 간격은 space() 한 단계, 셈을 붙이지 않는다" },
    ],
    "declaration-property-value-allowed-list": [
      {
        // 글자 크기는 토큰 하나뿐이다(0 은 글자를 감추는 글리프 단추). calc·%·Sass 셈·숫자를 쓰지 않는다.
        "font-size": ["/^font-size\\(\"[0-9a-z]+\"\\)$/", "0", "inherit"],
        // 모서리는 넷(정책 641) — 4px·2px·알약·원, 그리고 없음·물려받기
        "/^border(-[a-z]+)*-radius$/": ["/^(var\\(--r(-xs|-pill|-circle)?\\)|0)( (var\\(--r(-xs|-pill|-circle)?\\)|0))*$/", "inherit"],
      },
      { message: "토큰 밖의 값이다 — 글자 크기는 font-size(\"…\"), 모서리는 var(--r)·var(--r-xs)·var(--r-pill)·var(--r-circle)" },
    ],
  },
  overrides: [
    {
      // 토큰 파일: 색·그림자 값의 출처다. 위젯 어두운 테마도 여기서 색을 다시 정한다.
      files: ["web/styles/tokens/**/*.scss", "web/styles/pages/widget/_tokens.scss"],
      rules: {
        "color-no-hex": null, "color-named": null, "declaration-property-value-disallowed-list": null, "declaration-property-value-allowed-list": null,
        "scss/dollar-variable-pattern": "^[a-z][a-z0-9]*(-[a-z0-9]+)*$",
        "at-rule-disallowed-list": null,
      },
    },
    {
      // 도구 층: 믹스인을 정한다. 값(색·크기)은 정하지 않으므로 Sass 변수 선언은 여기서도 막힌 채다.
      files: ["web/styles/tools/**/*.scss"],
      rules: { "at-rule-disallowed-list": null },
    },
  ],
};
