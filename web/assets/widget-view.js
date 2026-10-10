// 위젯 화면이 쓰는 순수 함수. DOM 을 만지지 않아 node 에서 그대로 시험한다(widget-view.test.mjs).

// 3D 아이콘(Fluent Emoji, MIT — icons/LICENSE). 같은 서버의 /icons/ 에서만 받는다. 바깥 주소를 쓰지 않는다(CSP·사내망).
// 3D 는 포인트에만 쓴다(D41·D44) — 인사 배너(손 흔들기·상담원 자리의 헤드폰), 빈 상태(말풍선), 첨부 종류(그림·영상·문서).
// 나머지 자리(런처·머리 로고·첨부 단추·운영 표시·읽음)는 widget.css 의 평면 글리프다. 여기에 없는 아이콘을 화면에 쓰면 테스트가 실패한다.
export const ICON = {
  wave: "/icons/waving_hand.png",
  headset: "/icons/headphone.png",
  balloon: "/icons/speech_balloon.png",
  picture: "/icons/framed_picture.png",
  video: "/icons/video_camera.png",
  page: "/icons/page.png",
};

// 홈의 최근 대화는 세 개까지다. 목록은 서버가 최근 순으로 준다.
export const RECENT_LIMIT = 3;
// 홈의 상담원 묶음은 세 명까지다.
export const AGENT_LIMIT = 3;

// homeSections 는 홈에 무엇을 보일지 정한다. FAQ 는 한 줄이라도 있으면, 최근 대화는 하나라도 있으면 보인다.
export function homeSections({ faq, conversations } = {}) {
  const items = Array.isArray(faq) ? faq : [];
  const convs = Array.isArray(conversations) ? conversations : [];
  return {
    faqVisible: items.length > 0,
    recentVisible: convs.length > 0,
    recent: convs.slice(0, RECENT_LIMIT),
  };
}

// rememberAgent 는 답한 상담원 이름을 최근 순으로 앞에 놓는다. 같은 이름은 한 번만, AGENT_LIMIT 명까지다.
// 저장된 목록에 이미 겹친 이름이 있어도 한 번만 남긴다. 받은 배열은 고치지 않는다. 빈 이름은 무시한다.
export function rememberAgent(list, name) {
  const prev = Array.isArray(list) ? list.filter((n) => typeof n === "string") : [];
  const out = [];
  for (const n of [String(name || ""), ...prev].map((x) => x.trim())) {
    if (n && !out.includes(n)) out.push(n);
  }
  return out.slice(0, AGENT_LIMIT);
}

// agentsFor 는 저장된 상담원 묶음({ owner, names })을 지금 고객(owner)의 것일 때만 돌려준다.
// 같은 브라우저에 다른 고객이 들어오면(익명→회원, 회원 A→B) 남이 받은 상담원 이름을 보이지 않는다. 주인이 없는 옛 형식(배열)도 버린다.
export function agentsFor(saved, owner) {
  if (!owner || !saved || Array.isArray(saved) || saved.owner !== owner) return [];
  return rememberAgent(saved.names, "");
}

// phoneFrame 은 임베드 iframe 이 휴대폰 화면 전체로 커졌는지 본다. iframe 안에서는 폭 조건으로 데스크톱(400px 판)과 가를 수 없다.
// 화면 폭(screen.width)과 iframe 폭이 같고(2px 오차) 그 폭이 480px 이하일 때만 휴대폰이다.
// viewport meta 가 없는 호스트는 휴대폰에서도 iframe 을 400×720 으로 두는데(embed.js 가 휴대폰으로 못 알아본다), 그때 화면 폭은 400 이 아니다.
export const PHONE_MAX_WIDTH = 480;
export function phoneFrame(frameWidth, screenWidth) {
  const w = Number(frameWidth) || 0;
  const s = Number(screenWidth) || 0;
  return s > 0 && w > 0 && w <= PHONE_MAX_WIDTH && Math.abs(w - s) <= 2;
}

// timeGroupHides 는 말풍선 줄마다 시간을 숨길지 정한다.
// rows 는 화면 순서의 [{ same, time }] 이다. same 은 "바로 앞 줄과 같은 사람" 이다(ui.js 의 .row.same).
// 다음 줄이 같은 사람이고 같은 시각(분)이면 이 줄의 시간을 숨긴다 — 묶음의 마지막 줄에만 시간이 남는다.
export function timeGroupHides(rows) {
  const list = Array.isArray(rows) ? rows : [];
  return list.map((row, i) => {
    const next = list[i + 1];
    return !!(next && next.same && row.time && next.time === row.time);
  });
}

// uploadProgress 는 업로드 진행 표시의 값이다. total 이 없거나 0 이면 아직 모른다(막대는 흐르는 모양).
// 다 올라가면(loaded >= total) 서버가 파일을 확인하는 단계라 "확인 중" 이다. 백분율은 0~100 정수로 자른다.
export function uploadProgress(loaded, total) {
  const t = Number(total) || 0;
  if (t <= 0) return { known: false, percent: 0, phase: "uploading" };
  const percent = Math.max(0, Math.min(100, Math.floor(((Number(loaded) || 0) / t) * 100)));
  return { known: true, percent, phase: (Number(loaded) || 0) >= t ? "verifying" : "uploading" };
}

// attachIcon 은 첨부 종류(content_type)에 맞는 아이콘이다. 업로드 중 표시와 파일 카드가 쓴다.
export function attachIcon(contentType) {
  const t = String(contentType || "");
  if (t.startsWith("image/")) return ICON.picture;
  if (t.startsWith("video/")) return ICON.video;
  return ICON.page;
}

// ── 문의내역 카드·날짜(D44) ─────────────────────────────────

const pad2 = (n) => String(n).padStart(2, "0");
const validDate = (iso) => {
  if (!iso) return null;
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? null : d;
};

// stampTime 은 상담 카드의 시각이다. 절대 시각 "2026.10.06 14:15"(이 기기의 시간대, 24시간)로 쓴다.
// "3분 전" 같은 상대 시각은 화면을 열어 둔 채 시간이 흐르면 틀린다. 읽을 수 없는 값은 빈 글자다.
export function stampTime(iso) {
  const d = validDate(iso);
  if (!d) return "";
  return `${d.getFullYear()}.${pad2(d.getMonth() + 1)}.${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}

// clockTime 은 위젯 말풍선 아래의 시각이다. 24시간 "14:15"(이 기기의 시간대)로 쓴다.
// 데스크는 ui.js 기본값(formatTime, "오후 02:15")을 그대로 쓴다 — 상담원 화면의 다른 시각(보류 기한·삭제 시각)과 같은 꼴이다.
export function clockTime(iso) {
  const d = validDate(iso);
  if (!d) return "";
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}

// dayStamp 는 위젯 대화의 날짜 구분선이다. 오늘·어제도 말로 바꾸지 않고 "2026년 10월 06일" 로 쓴다.
export function dayStamp(iso) {
  const d = validDate(iso);
  if (!d) return "";
  return `${d.getFullYear()}년 ${pad2(d.getMonth() + 1)}월 ${pad2(d.getDate())}일`;
}

// 고객에게 보이는 상담 상태는 둘이다 — 끝나지 않았으면 진행중, 끝났으면 종료. 보류(SNOOZED)는 상담원의 일정이라 고객에게는 진행중이다.
export const CUSTOMER_STATE = { OPEN: "진행중", SNOOZED: "진행중", CLOSED: "종료" };
export const customerState = (state) => CUSTOMER_STATE[state] || "진행중";

// convCard 는 상담 카드(홈의 최근 문의·대화 목록) 한 장의 글자다. 마크업은 widget.js 가 이스케이프해 그린다.
//   kind  "채팅"            when  절대 시각(마지막 메시지, 없으면 연 시각)
//   title 마지막 글(고객·상담원이 쓴 글, 서버가 고른다). 글 없이 파일만 보냈으면(보낸 사람은 있고 본문이 없으면) "파일을 보냈습니다",
//         고객·상담원 글이 아직 없으면(환영 글만 있어도) "아직 보낸 메시지가 없습니다". 판정은 시각이 아니라 보낸 사람으로 한다 — 환영 글도 시각을 남긴다.
//         상담 제목(subject)이 있으면 그것이 제목이고 마지막 글은 sub(회색 한 줄)로 내려간다. 제목이 없으면 sub 는 빈 글자다(줄을 그리지 않는다).
//   state 진행중·종료, open 은 진행중인지(배지 색)
// 지금은 고객 상담에 제목이 없다 — 카드끼리 가를 수 있게 마지막 글을 제목에 둔다. 상담 흐름을 고칠 때 고객이 고른 문의 유형을 subject 로 넣으면
// 그대로 제목이 되고 마지막 글은 보조 줄이 된다(D44 보완).
export function convCard(c = {}) {
  const last = c.last_body || (c.last_sender_kind ? "파일을 보냈습니다" : "아직 보낸 메시지가 없습니다");
  const subject = (c.subject || "").trim();
  return {
    kind: "채팅",
    when: stampTime(c.last_message_at || c.created_at),
    title: subject || last,
    sub: subject ? last : "",
    state: customerState(c.state),
    open: c.state !== "CLOSED",
  };
}
