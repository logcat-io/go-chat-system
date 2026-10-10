// 위젯 FAQ(D36)의 화면 쪽 정책 테스트.
//
// ui.js 의 순수 함수만 부른다. 단언은 사람이 보는 결과다 — 그려진 HTML, 편집 화면이 먼저 띄우는 문장, 옮긴 뒤의 순서.
import { test, describe } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

// 브라우저는 "/chat.js" 를 서버 뿌리에서 받는다. node 에서는 같은 폴더의 파일로 잇는다.
register(
  "data:text/javascript," +
    encodeURIComponent(`export async function resolve(spec, ctx, next) {
      if (spec === "/chat.js") return next(new URL("./chat.js", ctx.parentURL).href, ctx);
      return next(spec, ctx);
    }`),
);
const { faqHTML, faqPillsHTML, FAQ_HOME_LIMIT, validateFaq, moveItem } = await import("./ui.js");

const item = (question, answer) => ({ question, answer });

describe("위젯 FAQ", () => {
  test("위젯 FAQ 는 질문과 답의 HTML 을 이스케이프해 그린다", () => {
    const html = faqHTML([item(`<img src=x onerror="alert(1)">`, `<script>alert('a')</script> & 끝`)]);
    assert.doesNotMatch(html, /<img|<script/);
    assert.match(html, /&lt;img src=x onerror=&quot;alert\(1\)&quot;&gt;/);
    assert.match(html, /&lt;script&gt;alert\(&#39;a&#39;\)&lt;\/script&gt; &amp; 끝/);
    // 항목 수만큼 펼침 칸이 생기고, 목록이 비면 아무것도 그리지 않는다.
    assert.equal(faqHTML([item("가", "나"), item("다", "라")]).match(/class="accordion-item"/g).length, 2);
    assert.equal(faqHTML([]), "");
    assert.equal(faqHTML(undefined), "");
    // 홈의 질문 알약도 이스케이프한다
    const pills = faqPillsHTML([item(`<img src=x onerror="alert(1)">`, "a")]);
    assert.doesNotMatch(pills, /<img/);
    assert.match(pills, /&lt;img src=x onerror=&quot;alert\(1\)&quot;&gt;/);
    assert.equal(faqPillsHTML([]), "");
  });

  // 홈에 보이는 것만 본다 — 단추 몇 개가 어떤 글자로 어떤 순서로 보이는가. 누르면 그 질문이 펼쳐지는 것은 정책 640(widget-screen.test.mjs)이 화면을 띄워 본다.
  test("홈의 자주 묻는 질문은 앞의 네 개만 알약 단추로 보이고 답은 펼치지 않는다", () => {
    const list = ["하나", "둘", "셋", "넷", "다섯", "여섯"].map((q) => item(q, `${q}의 답`));
    assert.equal(FAQ_HOME_LIMIT, 4);
    const html = faqPillsHTML(list);
    // 보이는 글자: 단추마다 안의 글자를 태그를 걷어 읽는다
    const buttons = [...html.matchAll(/<button\b[^>]*>([\s\S]*?)<\/button>/g)].map((m) => m[1].replace(/<[^>]+>/g, "").trim());
    assert.deepEqual(buttons, ["하나", "둘", "셋", "넷"], "관리자가 정한 순서의 앞 네 개가 아니다");
    assert.equal(html.replace(/<button\b[\s\S]*?<\/button>/g, "").trim(), "", "알약은 단추다(키보드로 누른다) — 단추 밖에 그린 것이 있다");
    assert.doesNotMatch(html, /의 답/, "홈의 알약에는 답을 펼치지 않는다");
    // 질문이 네 개보다 적으면 있는 만큼
    assert.equal([...faqPillsHTML(list.slice(0, 2)).matchAll(/<button\b/g)].length, 2);
  });

  test("홈의 질문 알약이 말줄임표로 잘려도 전체 질문은 알약의 title 로 보인다", () => {
    const long = "주문한 상품을 받기 전에 배송지를 바꾸고 싶은데 어디서 바꿀 수 있나요?";
    const html = faqPillsHTML([item(long, "a"), item(`"따옴표"<b>`, "b")]);
    const titles = [...html.matchAll(/<button\b[^>]*\btitle="([^"]*)"/g)].map((m) => m[1]);
    assert.deepEqual(titles, [long, "&quot;따옴표&quot;&lt;b&gt;"], "알약마다 이스케이프한 전체 질문이 title 에 있어야 한다");
  });

  test("자주 묻는 질문 화면은 관리자가 정한 순서로 질문을 한 줄씩 그리고 처음에는 모두 닫혀 있다 — 단추의 aria-expanded 와 aria-controls 가 답 칸을 가리킨다", () => {
    const list = ["배송", "교환", "환불"].map((q) => item(q, `${q}은 이렇게 합니다`));
    const html = faqHTML(list);
    assert.deepEqual([...html.matchAll(/class="accordion-title">([^<]+)</g)].map((m) => m[1]), ["배송", "교환", "환불"], "순서가 바뀌었다");
    const triggers = [...html.matchAll(/<button type="button" class="accordion-trigger" id="(wFaqQ\d+)" aria-expanded="(\w+)" aria-controls="(wFaqA\d+)"/g)];
    assert.equal(triggers.length, 3);
    for (const [, q, expanded, a] of triggers) {
      assert.equal(expanded, "false", `${q} 가 처음부터 열려 있다`);
      assert.match(html, new RegExp(`<div class="accordion-panel" id="${a}" role="region" aria-labelledby="${q}" hidden>`), `${q} 의 답 칸이 없거나 열려 있다`);
    }
    // 답의 주소는 눌러 열 수 있다(이스케이프한 뒤 링크로 바꾼다).
    assert.match(faqHTML([item("q", "여기서 https://example.com/a?b=1&c=2 확인")]), /<a href="https:\/\/example\.com\/a\?b=1&amp;c=2" target="_blank" rel="noopener noreferrer">/);
  });

  test("FAQ 편집 화면은 서버와 같은 한도로 입력 순서의 첫 오류를 먼저 알린다", () => {
    assert.equal(validateFaq([]), "");
    const ten = Array.from({ length: 10 }, (_, i) => item(`질문 ${i}`, `답 ${i}`));
    assert.equal(validateFaq(ten), "");
    assert.equal(validateFaq([...ten, item("", "")]), "자주 묻는 질문은 10개까지입니다");
    // 앞뒤 공백을 떼고 글자로 센다. 80자·500자는 받고 하나 넘으면 거절한다.
    assert.equal(validateFaq([item(` ${"질".repeat(80)} `, ` ${"답".repeat(500)} `)]), "");
    assert.equal(validateFaq([item("질".repeat(81), "a")]), "1번째 질문은 80자까지입니다");
    assert.equal(validateFaq([item("q", "답".repeat(501))]), "1번째 답은 500자까지입니다");
    assert.equal(validateFaq([item("   ", "a")]), "1번째 질문이 비어 있습니다");
    assert.equal(validateFaq([item("q", "\n\t ")]), "1번째 답이 비어 있습니다");
    // 여러 곳이 틀리면 입력 순서로 첫 오류 — 서버 정책과 같은 문장이다.
    assert.equal(validateFaq([item("멀쩡", "멀쩡"), item("둘째", ""), item("", "답".repeat(501))]), "2번째 답이 비어 있습니다");
    // 한 항목 안에서는 질문을 답보다 먼저 본다.
    assert.equal(validateFaq([item("", "")]), "1번째 질문이 비어 있습니다");
    assert.equal(validateFaq([item("질".repeat(81), "답".repeat(501))]), "1번째 질문은 80자까지입니다");
  });

  test("FAQ 항목을 위아래로 옮기면 그 항목만 이웃과 자리를 바꾸고 끝을 넘으면 그대로다", () => {
    const list = ["가", "나", "다", "라"].map((q) => item(q, q));
    const qs = (l) => l.map((x) => x.question).join("");
    assert.equal(qs(moveItem(list, 2, -1)), "가다나라");
    assert.equal(qs(moveItem(list, 0, 1)), "나가다라");
    assert.equal(qs(moveItem(list, 0, -1)), "가나다라");
    assert.equal(qs(moveItem(list, 3, 1)), "가나다라");
    assert.equal(qs(list), "가나다라", "받은 배열은 그대로다");
  });
});
