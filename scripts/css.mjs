#!/usr/bin/env node
// SCSS 디자인 시스템(web/styles/)을 web/assets/*.css 로 컴파일한다(D44).
//   node scripts/css.mjs           쓴다(npm run css)
//   node scripts/css.mjs --check   쓰지 않고, 컴파일 결과가 커밋된 CSS 와 다르면 실패한다(npm run css:check)
// 컴파일된 CSS 는 리포에 커밋한다. Go 빌드와 Docker 이미지는 Node 없이 그 파일을 embed 한다.
import { compile } from "sass";
import { readFileSync, writeFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join, relative } from "node:path";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
// 진입점 → 출력. 새 진입점을 더하면 web/embed.go 의 패턴(assets/*.css)에 그대로 들어간다.
const ENTRIES = [
  ["web/styles/app.scss", "web/assets/app.css"],
  ["web/styles/widget.scss", "web/assets/widget.css"],
];

const check = process.argv.includes("--check");
let failed = 0;

for (const [src, out] of ENTRIES) {
  // charset: false — 한국어 주석이 있어도 @charset 을 붙이지 않는다. 테스트가 CSS 를 글자로 읽는다(widget-view.test.mjs).
  const { css } = compile(join(root, src), { style: "expanded", charset: false, sourceMap: false });
  const text = `${css}\n`;
  const target = join(root, out);
  if (!check) {
    writeFileSync(target, text);
    console.log(`${src} → ${out} (${Buffer.byteLength(text)} B)`);
    continue;
  }
  const current = existsSync(target) ? readFileSync(target, "utf8") : "";
  if (current !== text) {
    failed++;
    console.error(`${relative(root, target)} 가 ${src} 의 컴파일 결과와 다르다. 'npm run css' 를 돌리고 함께 커밋해라.`);
  }
}

if (check) {
  if (failed) process.exit(1);
  console.log("컴파일된 CSS 가 SCSS 와 같다.");
}
