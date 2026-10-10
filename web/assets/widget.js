import { ChatSession, api, isTransientFailure, isSessionExpired } from "/chat.js";
import {
  $, escape, bindStatus, createLog, createTyping, autoGrow, createNotifier, dropzone,
  humanSize, emptyHTML, toast, toastError, clearToasts, applyBrand, isBrandColor, menu, form, confirm, sessionErrorNotice, faqHTML, faqPillsHTML, avatarHTML,
  createUploadGuard, createSentDrafts, restoreDraft, clip, attachmentGiveBack, mergeConversation, ratingCardHTML,
} from "/ui.js";
import { ICON, homeSections, rememberAgent, agentsFor, phoneFrame, timeGroupHides, attachIcon, uploadProgress, convCard, dayStamp, clockTime, customerState } from "/widget-view.js";

const TOKEN_KEY = "gcs.customerToken";
// 홈의 상담원 묶음에 쓸 이름. 고객에게 보인 적 있는 상담원 이름뿐이라 비밀이 아니다. 저장이 막힌 브라우저에서는 이번 화면에서만 기억한다.
// { owner: 고객 id, names: [...] } 로 둔다. 다른 고객이 들어오면 비운다.
const AGENTS_KEY = "gcs.recentAgents";
const params = new URLSearchParams(location.search);
const EMBED = params.get("embed") === "1";
// iframe 을 띄운 페이지의 오리진. 호스트의 boot 메시지가 오기 전에도 세션을 열어야 해서 referrer 로 먼저 잡는다.
const REFERRER_ORIGIN = (() => { try { return document.referrer ? new URL(document.referrer).origin : ""; } catch { return ""; } })();

// 세션 입구에 실어 보내는 설치처 정보. 설치형 단일 워크스페이스라 위젯 키는 없다(D35). 옛 설치 코드가 주소에 ?key= 를 붙여도 읽지 않는다.
function tenantFields() {
  return { host_origin: EMBED ? hostOrigin || REFERRER_ORIGIN : location.origin };
}

const els = Object.fromEntries(
  ["demoHost", "launcher", "launcherBadge", "messenger", "back", "close", "headTitle", "headSub", "status", "statusText",
   "viewHome", "viewList", "viewChat", "greetText", "hoursPill", "startChat", "recentCard", "recentList",
   "faq", "faqCard", "convList", "listEmpty", "log", "msgs", "pending", "csat", "typing", "typingLabel", "composer", "input", "send",
   "file", "attachHint", "nav", "navBadge", "sessionNotice", "sessionNoticeText", "sessionRetry",
   "agentStack", "replyHint", "attachBtn", "attachRow", "viewFaq", "faqList", "faqAll", "faqStart", "recentAll",
   "endChat", "closedFoot", "newInquiry", "returned", "returnedBody", "returnedHint", "returnedCopy", "returnedClear"].map((id) => [id, $(id)]),
);

const view = createLog({
  logEl: els.log, msgsEl: els.msgs, pendingEl: els.pending, mineKind: "CUSTOMER", token: () => token, dayText: dayStamp, timeText: clockTime,
  // 고객은 자기 메시지만 고치고 지운다.
  onMenu: (messageId, btn, { text }) => {
    const base = `/api/conversations/${current.id}/messages/${messageId}`;
    menu(btn, [
      { label: "수정", onSelect: async () => {
        const out = await form({ title: "메시지 수정", fields: [{ name: "body", label: "본문", type: "textarea", value: text }], ok: "수정" });
        if (!out) return;
        try { await api(base, { token, method: "PATCH", body: { body: out.body } }); } catch (err) { toastError(err); }
      } },
      { label: "삭제", danger: true, onSelect: async () => {
        if (!(await confirm({ title: "이 메시지를 삭제할까요?", ok: "삭제", danger: true }))) return;
        try { await api(base, { token, method: "DELETE" }); } catch (err) { toastError(err); }
      } },
    ]);
  },
});
const showTyping = createTyping(els.typing, els.typingLabel);
const paintStatus = bindStatus(els.status, els.statusText);
// connStatus 는 소켓의 연결 상태다(online·connecting·offline). 상담 종료 확인 창이 "보내지 못한 글이 있고 끊겨 있다" 를 판정한다(D48).
let connStatus = "connecting";
const setStatus = (s) => {
  connStatus = s;
  paintStatus(s);
};
const notifier = createNotifier();

let session = null;
let token = "";
let customerID = "";
let current = null;
let conversations = [];
let pendingAttachment = null;
let open = false;
let settings = null;
let hostOrigin = null;
let memberToken = "";
// hostMemberRefresh 는 호스트가 새 회원 토큰을 줄 수 있다는 표시다(embed.js boot 의 getMemberToken). 없으면 회원 세션이 끝났을 때 묻지 않는다.
let hostMemberRefresh = false;
// sessionError 는 고객 세션을 못 연 까닭이다. 있으면 홈 머리와 안내 칸이 이걸 보인다. 설정을 다시 불러와도 지우지 않는다.
let sessionError = null;
let retryTimer = null;
// hostColor 는 호스트 설치 코드의 data-color 다. 있으면 관리자 색을 덮는다(옛 설치 코드 호환). 없으면 관리자 색이 기준이다.
let hostColor = "";
// 작성칸 업로드는 시작한 상담에 묶인다. 보낸 글은 거절되면 돌려받으려고 잠시 기억한다.
const uploads = createUploadGuard();
const drafts = createSentDrafts();
// held 는 다른 상담에서 거절된 글이다. 그 상담을 다시 열면 입력칸에 돌려준다.
const held = new Map();

// ── 호스트 페이지와의 대화 (임베드 모드) ─────────────────────────

function tellHost(msg) {
  if (!EMBED || !window.parent || window.parent === window) return;
  window.parent.postMessage({ source: "go-chat", ...msg }, hostOrigin || "*");
}

if (EMBED) {
  document.body.classList.add("embed");
  document.documentElement.classList.add("w-embed");
  els.demoHost.hidden = true;
  // 휴대폰이면 호스트가 iframe 을 화면 전체로 키운다(embed.js). iframe 안의 폭 조건으로는 데스크톱(400px)과 가를 수 없어 화면 폭과 비교한다.
  const fitPhone = () => {
    const screenW = typeof screen === "object" && screen ? Number(screen.width) || 0 : 0;
    document.body.classList.toggle("w-phone", phoneFrame(window.innerWidth, screenW));
  };
  fitPhone();
  window.addEventListener("resize", fitPhone);
  window.addEventListener("message", (e) => {
    const m = e.data;
    // 부모 창이 보낸 것만 받는다. 다른 창이 source 문자열을 흉내 내도 hostOrigin 을 못 덮는다.
    if (e.source !== window.parent) return;
    if (!m || m.source !== "go-chat-host") return;
    hostOrigin = e.origin;
    switch (m.type) {
      case "boot":
        applyHostConfig(m.config || {});
        // 호스트 오리진을 알고 나서 세션을 연다. referrer 가 막힌 페이지에서도 e.origin 은 온다.
        // 설정에 회원 토큰이 있으면 applyHostConfig 가 이미 회원 세션을 열기 시작했다. 그때 boot 는 그것을 기다린다(익명을 열지 않는다).
        if (!token) boot().then(paintBadges);
        break;
      case "identify":
        identify(m.memberToken);
        break;
      case "member-token":
        answerMemberToken(m.id, m.memberToken);
        break;
      case "logout":
        logout();
        break;
      case "open":
        setOpen(true);
        break;
      case "close":
        setOpen(false);
        break;
    }
  });
}

function applyHostConfig(cfg) {
  // 설치 코드에 data-color 가 있을 때만 관리자 색을 덮는다. 없으면 loadSettings 가 칠한 관리자 색 그대로다.
  if (isBrandColor(cfg.color)) {
    hostColor = cfg.color;
    applyBrand(hostColor);
  }
  document.body.classList.toggle("pos-left", cfg.position === "left");
  hostMemberRefresh = !!cfg.memberRefresh;
  if (cfg.memberToken && cfg.memberToken !== memberToken) identify(cfg.memberToken);
}

// ── 화면 전환 ────────────────────────────────────────────────────

function setOpen(next) {
  open = next;
  els.messenger.classList.toggle("closed", !open);
  els.launcher.classList.toggle("hide", open);
  tellHost({ type: "size", open });
  if (open) {
    notifier.ask();
    setView(session?.convId ? "chat" : "home");
    loadConversations();
  }
  paintBadges();
}

// 화면은 넷이다: 홈, 대화 목록, 대화, 자주 묻는 질문. 자주 묻는 질문은 홈에서 들어가는 화면이라 아래 탭은 "홈" 이 켜지고 뒤로가기는 홈이다.
const VIEWS = ["home", "list", "chat", "faq"];
const backTarget = { chat: "list", faq: "home" };

function setView(name) {
  const panes = { home: els.viewHome, list: els.viewList, chat: els.viewChat, faq: els.viewFaq };
  for (const key of VIEWS) panes[key].hidden = key !== name;
  els.messenger.dataset.view = name;
  els.back.hidden = !backTarget[name];
  const tab = name === "faq" ? "home" : name;
  for (const btn of els.nav.querySelectorAll("button")) btn.setAttribute("aria-current", String(btn.dataset.view === tab));
  if (name === "chat") {
    // 대화 머리도 홈과 같은 모양이다 — 뒤로 · 로고 · 이름(관리자가 정한 제목) · 닫기. 상태(진행중·종료)는 이름 아래 작은 회색 글자다.
    els.headTitle.textContent = brandName();
    describeCurrent();
    view.toEnd();
    if (!els.composer.hidden) els.input.focus();
  } else if (name === "list") {
    els.headTitle.textContent = "대화";
    els.headSub.textContent = "이전 문의를 이어서 할 수 있습니다";
  } else if (name === "faq") {
    els.headTitle.textContent = "자주 묻는 질문";
    els.headSub.textContent = "";
  } else {
    // 홈의 머리는 흰 바탕에 로고와 이름(관리자가 정한 제목)이다. 보조 줄은 세션을 못 열었을 때만 그 까닭을 보인다.
    els.headTitle.textContent = brandName();
    els.headSub.textContent = sessionError ? sessionError.text : "";
    els.replyHint.textContent = settings?.open_now === false ? "운영시간에 순서대로 답해 드립니다" : "보통 몇 분 안에 답변합니다";
  }
  paintClosing();
}

// brandName 은 머리의 로고 옆 이름이다. 관리자가 데스크에서 정한 위젯 이름이고, 설정을 못 받았거나 이름이 비었으면 기본 이름(서버 기본값과 같다)이다.
// 공백만 있는 이름은 빈 이름이다(정책 670) — 서버는 앞뒤 공백을 떼고 저장하지만 그 전에 저장한 설치가 있다.
// 이름 자리라 문장("무엇을 도와드릴까요?")을 기본값으로 두지 않는다 — 대화 머리에 질문이 이름처럼 떴다. 인사 문장은 인사 배너(greeting)가 맡는다.
const brandName = () => String(settings?.title ?? "").trim() || "고객센터";

// ── 자주 묻는 질문 ───────────────────────────────────────────────

// setFaqOpen 은 아코디언 한 줄을 열거나 닫는다. 단추의 aria-expanded 와 답 칸의 hidden 이 같이 바뀐다.
function setFaqOpen(trigger, open) {
  trigger.setAttribute("aria-expanded", String(open));
  const panel = $(trigger.getAttribute("aria-controls"));
  if (panel) panel.hidden = !open;
}

// faqCount 는 지금 그린 자주 묻는 질문의 수다(loadSettings). 화면에 들어올 때 앞서 펼친 줄을 닫는 데 쓴다.
let faqCount = 0;
// faqOrigin 은 자주 묻는 질문 화면에 들어온 단추의 id 다(알약 wFaqPill{i} 또는 전체보기 faqAll). 뒤로가기가 초점을 거기로 돌려준다.
let faqOrigin = "";

// openFaq 는 자주 묻는 질문 화면을 연다. 들어올 때마다 모든 줄을 닫고 시작한다 — 앞서 펼친 질문이 남지 않는다.
// index 를 주면(홈의 질문 알약) 그 질문 하나만 펼치고 초점을 옮긴다. 전체보기(index 없음)는 모두 닫힌 채다.
function openFaq(index, origin) {
  faqOrigin = origin || "";
  for (let i = 0; i < faqCount; i++) {
    const t = $(`wFaqQ${i}`);
    if (t) setFaqOpen(t, false);
  }
  setView("faq");
  if (index === undefined) return;
  const trigger = $(`wFaqQ${index}`);
  if (!trigger) return;
  setFaqOpen(trigger, true);
  trigger.focus?.();
  trigger.scrollIntoView?.({ block: "nearest" });
}

// goBack 은 머리의 뒤로가기다. 자주 묻는 질문에서 홈으로 돌아가면 들어온 단추(알약·전체보기)로 초점을 돌려준다 —
// 키보드·화면 읽기 사용자가 홈의 맨 위부터 다시 찾지 않는다. 그 단추가 없어졌으면(목록이 바뀌었다) 전체보기로 간다.
function goBack() {
  const from = els.messenger.dataset.view;
  setView(backTarget[from] || "home");
  if (from !== "faq" || !faqOrigin) return;
  const target = $(faqOrigin) || els.faqAll;
  faqOrigin = "";
  target?.focus?.();
}

function describeCurrent() {
  if (!current) return;
  els.headSub.textContent = customerState(current.state);
  paintClosing();
}

// ── 고객 종료(D48) ───────────────────────────────────────────────

// customerClosed 는 고객이 닫아 둔 상담인지다. 고객 쪽에서는 읽기 전용이다 — 입력창 대신 "새 문의하기" 를 보인다.
// 상담원이 닫은 상담은 아니다(지금처럼 고객이 쓰면 다시 열린다). 상담원이 다시 열면 상태가 바뀌어 입력창이 돌아온다.
const customerClosed = (c) => c?.state === "CLOSED" && c?.closed_by === "CUSTOMER";

// paintClosing 은 지금 상담으로 머리의 "상담 종료" 와 아래 칸(입력창 또는 새 문의하기)을 정한다.
// "상담 종료" 는 대화 화면에서 진행 중인 상담에만 보인다 — 이미 종료된 상담, 상태를 아직 모르는 상담에서는 숨긴다.
function paintClosing() {
  const inChat = els.messenger.dataset.view === "chat";
  els.endChat.hidden = !inChat || !current?.id || !current.state || current.state === "CLOSED";
  const readOnly = customerClosed(current);
  els.composer.hidden = readOnly;
  els.closedFoot.hidden = !readOnly;
  if (readOnly) els.typing.hidden = true;
  paintReturned(inChat, readOnly);
}

// ── 종료된 상담에서 돌려받은 글(D48, 감사 26) ─────────────────────
// 고객이 닫은 뒤 다른 탭이 늦게 보낸 글은 closed_by_customer 로 거절된다. 그 상담에는 입력칸이 없어 거기 돌려 넣으면 보이지 않고
// 새로고침하면 사라졌다. 그래서 고객별 저장소 키에 남기고 그 상담의 아래 칸(새 문의하기 위)에 본문과 복사 단추를 보인다.
// 새 문의하기를 누르면 새 상담의 입력칸으로 옮기고, 상담원이 다시 열어 입력칸이 돌아오면 그 입력칸에 넣는다. 그 입력칸에서 보내면 지운다.
// 전송 대기 큐(gcs.q1.)와 다른 키라 다시 나가지 않는다. 로그아웃은 gcs.* 를 지우므로 공유 PC 에 남지 않는다.
//
// 글마다 키가 따로다(DEF-179): gcs.returned.v2.<고객>.<상담>|<글 id> = { id, conv, body, file, at }. 전에는 고객마다 키 하나(gcs.returned.v1.<고객>)에
// 상담별 목록을 통째로 써서, 다른 탭이 그 키를 쓰면 이 탭이 메모리에만 들고 있던 글(저장하지 못한 글)이 화면에서 사라졌다(감사 29 SEED2).
// 옛 키는 읽을 때 글마다 키로 옮기고 다 옮긴 뒤에만 지운다.
//
// 저장소가 진실이다. 메모리(returnedMem.unsaved)는 이 탭이 저장하지 못한 글(쓰기가 막힌 브라우저)만 들고, 읽을 때마다 저장소에 다시 써 본다 —
// 쓰이면 메모리에서 뺀다. 다른 탭의 쓰기는 다른 키라 메모리 글을 덮지 않는다.
//
// 입력칸으로 옮겨도 저장소에서 지우지 않는다(DEF-178). 옮긴 글은 이 탭의 표시(returnedMem.moved)로만 상자에서 빼고, 그 입력칸에서 글을 보내
// 전송 큐에 들어갈 때(sendMovedReturned) 또는 고객이 지우기를 누를 때만 지운다. 전에는 옮기는 순간 지워서, 두 탭이 모두 보일 때 옮긴 탭을
// 보내지 않고 닫으면 글이 사라졌다(감사 29 ret2 twotabs). 지금은 두 탭이 같은 글을 입력칸에 가져가도 저장소에 남는다. 한 탭이 보내면 지운 키가
// 저장소 이벤트로 다른 탭에 가고, 그 탭은 표시와 상자를 맞춘다 — 입력칸의 글은 건드리지 않는다(같은 글을 두 번 보내는 것은 고객이 정한다).
const RETURNED_PREFIX = "gcs.returned.v2.";
const RETURNED_OLD = "gcs.returned.v1.";
const returnedPrefix = () => `${RETURNED_PREFIX}${customerID}.`;
const returnedKey = (convId, id) => `${returnedPrefix()}${convId}|${id}`;
// 고객이 바뀌면 버린다 — 남의 글을 보이지 않는다.
// link 는 입력칸과 마지막으로 옮긴 글의 연결이다({ text, live, keys }). waiting 은 보냈지만 그 글이 아직 이 탭의 메모리 큐에만 있어 지우기를 미룬 것이다
// ({ session, conv, id, keys }[]). 둘 다 sendMovedReturned 가 쓴다(감사 30).
const freshReturned = () => ({ owner: customerID, unsaved: new Map(), moved: new Set(), link: null, waiting: [] });
let returnedMem = { ...freshReturned(), owner: "" };
function returnedState() {
  if (returnedMem.owner !== customerID) returnedMem = freshReturned();
  return returnedMem;
}
// writeReturned 는 글 하나를 그 키에 쓴다. 못 쓰면(쓰기가 막힌 브라우저) 이 탭의 메모리에 둔다 — 이번 화면에서는 보이고 저장이 되면 옮긴다.
function writeReturned(key, item) {
  const st = returnedState();
  try {
    localStorage.setItem(key, JSON.stringify(item));
    st.unsaved.delete(key);
    return true;
  } catch {
    st.unsaved.set(key, item);
    return false;
  }
}
// migrateReturned 는 옛 키(고객마다 하나)의 글을 글마다 키로 옮긴다. 다 옮겼을 때만 옛 키를 지운다. 이미 키가 있는 글은 그대로 둔다(그쪽이 새것이다).
function migrateReturned() {
  const old = RETURNED_OLD + customerID;
  let all;
  try { all = JSON.parse(localStorage.getItem(old) || "null"); } catch { return; }
  if (!all || typeof all !== "object") return;
  let ok = true;
  for (const [convId, list] of Object.entries(all)) {
    (Array.isArray(list) ? list : []).forEach((r, i) => {
      if (!r?.id) return;
      const key = returnedKey(convId, r.id);
      let has = false;
      try { has = localStorage.getItem(key) !== null; } catch { /* 읽기가 막혔다 */ }
      if (!has && !writeReturned(key, { id: r.id, conv: convId, body: r.body || "", file: r.file || "", at: i })) ok = false;
    });
  }
  if (ok) try { localStorage.removeItem(old); } catch { /* 다음에 다시 옮긴다(이미 옮긴 글은 건너뛴다) */ }
}
// loadReturned 는 이 고객의 돌려받은 글 전부(키 → 글)다. 저장소의 글에 이 탭이 저장하지 못한 글을 합친다. 옮긴 표시는 저장소에 남은 글만 남긴다 —
// 다른 탭이 보냈거나 지운 글의 표시는 버린다.
function loadReturned() {
  const st = returnedState();
  const out = new Map();
  if (!customerID) return out;
  migrateReturned();
  const prefix = returnedPrefix();
  try {
    const keys = [];
    for (let i = 0; i < localStorage.length; i++) keys.push(localStorage.key(i));
    for (const k of keys) {
      if (!k?.startsWith(prefix)) continue;
      const item = JSON.parse(localStorage.getItem(k) || "null");
      if (item?.id) out.set(k, item);
    }
  } catch { /* 저장소를 못 읽는다. 메모리의 것만 보인다 */ }
  for (const [k, item] of [...st.unsaved]) {
    if (out.has(k) || writeReturned(k, item)) st.unsaved.delete(k);
    out.set(k, out.get(k) || item);
  }
  for (const k of [...st.moved]) if (!out.has(k)) st.moved.delete(k);
  return out;
}
function keepReturned(back, clientMsgId) {
  if (!customerID) return;
  const id = clientMsgId || `${Date.now()}`;
  const key = returnedKey(back.convId, id);
  if (loadReturned().has(key)) return;
  writeReturned(key, { id, conv: back.convId, body: back.body, file: back.attachment?.filename || "", at: Date.now() });
}
// returnedOf 는 그 상담에서 돌려받은 글이다(받은 순서). 이 탭의 입력칸으로 옮긴 글은 뺀다 — 입력칸에 있다.
function returnedOf(convId) {
  if (!convId) return [];
  const moved = returnedState().moved;
  return [...loadReturned()].filter(([k, r]) => r.conv === convId && !moved.has(k))
    .map(([key, r]) => ({ ...r, key }))
    .sort((a, b) => (a.at || 0) - (b.at || 0) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
}
const returnedText = (list) => list.map((r) => r.body).join("\n\n");
// dropReturned 는 그 글들을 저장소와 이 탭의 메모리·표시에서 지운다. 옛 키에 남은 것(옮기지 못했을 때)도 뺀다.
function dropReturned(keys) {
  const st = returnedState();
  const gone = new Set(keys);
  for (const k of gone) {
    try { localStorage.removeItem(k); } catch { /* 못 지웠다. 다음에 다시 보인다 */ }
    st.unsaved.delete(k);
    st.moved.delete(k);
  }
  const old = RETURNED_OLD + customerID;
  try {
    const all = JSON.parse(localStorage.getItem(old) || "null");
    if (!all || typeof all !== "object") return;
    for (const [convId, list] of Object.entries(all)) {
      all[convId] = (Array.isArray(list) ? list : []).filter((r) => !gone.has(returnedKey(convId, r?.id)));
      if (!all[convId].length) delete all[convId];
    }
    if (Object.keys(all).length) localStorage.setItem(old, JSON.stringify(all));
    else localStorage.removeItem(old);
  } catch { /* 옛 키를 고치지 못했다 */ }
}
// moveReturned 는 그 상담에서 돌려받은 글을 지금 입력칸(비어 있을 때)으로 옮긴다. 옮겼으면 true. 저장소에서는 지우지 않는다(DEF-178) —
// 이 탭의 표시로 상자에서만 뺀다. 보내면(sendMovedReturned) 지운다.
function moveReturned(fromConvId) {
  const list = returnedOf(fromConvId);
  if (!list.length || els.composer.hidden || !restoreDraft(els.input, returnedText(list))) return false;
  const st = returnedState();
  for (const r of list) st.moved.add(r.key);
  st.link = { text: returnedText(list).trim(), live: true, keys: list.map((r) => r.key) }; // 앞서 끊긴 연결의 글은 이 연결에 들지 않는다
  autoGrow(els.input);
  return true;
}
// keepsMoved 는 입력칸의 글이 아직 옮긴 글에서 비롯된 것인가다 — 옮긴 글의 앞이나 뒤를 MOVED_KEEP 자 이상(옮긴 글이 더 짧으면 그 길이) 그대로 지닌다.
// 고객이 끝에 덧붙이거나 가운데를 고치면 앞이 남고, 앞에 덧붙이면 뒤가 남는다. 다 지우거나 전부 골라 덮어쓰면 첫 입력에서 끊긴다.
const MOVED_KEEP = 4;
function keepsMoved(value, text) {
  const v = String(value || "").trim();
  if (!v || !text) return false;
  const need = Math.min(MOVED_KEEP, text.length);
  let head = 0;
  while (head < v.length && head < text.length && v[head] === text[head]) head++;
  let tail = 0;
  while (tail < v.length && tail < text.length && v[v.length - 1 - tail] === text[text.length - 1 - tail]) tail++;
  return head >= need || tail >= need;
}
// 입력칸이 바뀔 때마다 옮긴 글과의 연결을 본다. 한 번 끊기면 다시 잇지 않는다 — 비운 뒤 새로 쓴 글이 우연히 같은 말로 시작해도 그 글은 아니다.
function trackMovedLink() {
  const link = returnedState().link;
  if (link?.live && !keepsMoved(els.input.value, link.text)) link.live = false;
}
// sendMovedReturned 는 입력칸의 글이 전송 큐에 들어갔을 때(send 가 항목을 돌려준 뒤) 부른다. 이 탭이 입력칸으로 옮긴 돌려받은 글을 저장소에서 지운다 —
// 이제 그 글은 전송 큐가 지킨다. 다른 탭은 저장소 이벤트로 이 삭제를 보고 상자를 맞춘다. 두 조건이 다 맞아야 지운다(감사 30, D48).
//   1. 보낸 글이 옮긴 글에서 비롯됐다 — 옮긴 뒤 입력칸이 그 글과 이어져 있었거나(link.live, keepsMoved), 보낸 본문이 옮긴 글과 같다(앞뒤 공백 무시).
//      전에는 입력칸을 비우고 전혀 다른 글을 보내도 지워, 돌려받은 글이 저장소에도 서버에도 없었다(ret30 erase-other).
//   2. 보낸 글이 저장소의 전송 큐에 있다. 용량이 꽉 차 이 탭의 메모리 큐에만 있으면(isUnsaved) 새로고침에 사라지므로 지우기를 미룬다 —
//      그 글이 저장소로 옮겨지거나 ack 로 큐에서 빠지면(settleMovedReturned) 그때 지운다.
function sendMovedReturned(body, item) {
  const st = returnedState();
  if (!st.link) return;
  if (!st.link.live && String(body || "").trim() !== st.link.text) return; // 다른 글이다. 옮긴 글은 저장소에 남는다
  const keys = st.link.keys.filter((k) => st.moved.has(k)); // 다른 탭이 보내거나 지운 글은 이미 표시가 빠졌다
  st.link = null;
  const conv = session?.convId;
  if (item && session?.isUnsaved(conv, item.client_msg_id)) {
    st.waiting.push({ session, conv, id: item.client_msg_id, keys });
    return;
  }
  dropReturned(keys);
}
// settleMovedReturned 는 미뤄 둔 지우기를 마저 한다. 그 글이 메모리 큐를 벗어났으면(저장소로 옮겨졌거나 ack·거절로 큐에서 빠졌다) 지운다.
// 거절이면 거절 알림이 그 본문을 돌려준다. 세션이 바뀌었으면(다른 주체) 지우지 않고 버린다 — 고객이 바뀌면 returnedMem 도 새것이다.
function settleMovedReturned() {
  const st = returnedState();
  if (!st.waiting.length) return;
  const done = [];
  st.waiting = st.waiting.filter((w) => {
    if (w.session !== session) return false;
    if (session.isUnsaved(w.conv, w.id)) return true;
    done.push(...w.keys);
    return false;
  });
  if (done.length) dropReturned(done);
}
// paintReturned 는 지금 상담에서 돌려받은 글 상자를 그린다. 닫힌 상담이면 새 문의하기 위에 보인다.
// 상담원이 다시 열어 입력칸이 돌아왔으면 그 입력칸에 옮긴다. 입력칸에 쓰던 글이 있어 옮기지 못하면 상자를 그대로 보인다(감사 27) —
// 전에는 옮기지 못해도 상자를 숨겨 글이 화면 어디에도 없었다. 저장소의 글은 입력칸에서 보냈을 때와 고객이 지울 때만 지운다(고객이 쓴 글이다).
// 저절로 옮기는 것은 보이는 탭뿐이다(감사 28). 숨은 탭은 상자만 그리고 보이게 되면(visibilitychange) 그때 다시 판단한다.
const tabVisible = () => document.visibilityState === "visible";
function paintReturned(inChat, readOnly) {
  settleMovedReturned();
  if (inChat && !readOnly && current?.id && tabVisible()) moveReturned(current.id);
  const list = inChat ? returnedOf(current?.id) : [];
  els.returned.hidden = list.length === 0;
  els.returnedBody.textContent = returnedText(list);
  const files = list.map((r) => r.file).filter(Boolean);
  els.returnedHint.textContent = (readOnly
    ? "종료된 상담이라 보내지 못했습니다. 새 문의하기를 누르면 입력칸에 옮겨 드립니다."
    : "입력칸에 쓰던 글이 있어 옮기지 못했습니다. 글을 복사해 붙여 넣거나, 필요 없으면 지워 주세요.")
    + (files.length ? ` 파일 ${files.join(", ")} 은 ${readOnly ? "새 문의에서 " : ""}다시 올려 주세요.` : "");
}
// 지우기는 고객이 스스로 치우는 것이다. 확인 창 없이 바로 지우고, 사라진 상자 대신 입력칸(없으면 새 문의하기)으로 초점을 옮긴다.
// 잘못 눌렀을 때를 위해 "되돌리기" 가 달린 알림을 띄운다(감사 28 감독 결정) — 확인 창은 매번 한 번 더 누르게 하고, 되돌리기는 실수한 사람만 누른다.
// 되돌리면 지운 글을 저장소와 상자에 되살린다. 그사이 다른 고객이 됐으면(로그아웃) 되살리지 않는다 — 앞 사람의 글이다(로그아웃은 알림도 치운다).
// 알림은 RETURNED_UNDO_MS 뒤 저절로 닫힌다 — 문의 번호 알림(D47)과 같은 10초다.
const RETURNED_UNDO_MS = 10000;
els.returnedClear.addEventListener("click", () => {
  const convId = current?.id;
  if (!convId) return;
  const owner = customerID;
  const removed = returnedOf(convId);
  dropReturned(removed.map((r) => r.key));
  paintClosing();
  (els.composer.hidden ? els.newInquiry : els.input).focus?.();
  if (!removed.length) return;
  toast("보내지 못한 글을 지웠습니다", "", { duration: RETURNED_UNDO_MS, action: { label: "되돌리기", onSelect: () => {
    if (!owner || customerID !== owner) return;
    const have = loadReturned();
    for (const { key, ...r } of removed) if (!have.has(key)) writeReturned(key, r);
    paintClosing();
    if (current?.id === convId && !els.returned.hidden) els.returnedCopy.focus?.();
  } } });
});
// 다른 탭이 돌려받은 글을 옮기거나 지웠거나 새로 받았다. 이 탭의 상자를 저장소에 맞춘다(저장소 이벤트는 쓴 탭 자신에게는 오지 않는다).
globalThis.addEventListener?.("storage", (e) => {
  if (e?.key == null || (customerID && (e.key.startsWith(returnedPrefix()) || e.key === RETURNED_OLD + customerID))) paintClosing();
});
// 숨어 있던 탭이 보이게 됐다. 미뤄 둔 옮기기를 지금 판단한다.
globalThis.addEventListener?.("visibilitychange", () => {
  if (tabVisible()) paintClosing();
});
els.returnedCopy.addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText(returnedText(returnedOf(current?.id)));
    els.returnedCopy.textContent = "복사되었습니다";
  } catch {
    els.returnedCopy.textContent = "복사하지 못했습니다";
  }
});

const CLOSE_TITLE = "상담을 종료할까요?";
const CLOSE_BODY = "종료하면 이 상담에는 더 이상 메시지를 보낼 수 없습니다. 새로 문의하려면 새 문의하기를 눌러 주세요.";
const CLOSE_UPLOADING = "올리는 중인 파일은 보내지 않고 종료됩니다.";
const CLOSE_ATTACHED = "작성칸에 올려 둔 파일은 보내지 않고 종료됩니다.";

// askClose 는 확인 창을 거쳐 상담을 닫는다. 유실 방지가 먼저다(D5) — 보내지 못한 글이 있으면 chat.js 가 먼저 보내고 저장 확인을 받은 뒤 닫는다.
// 보내지 못한 글이 있는데 연결이 끊겨 있으면 확인 단추를 잠그고 까닭을 보인다. 닫는 동안은 작성칸을 잠근다 — 닫히는 상담에 새 글이 들어가지 않게.
let closing = false;
async function askClose() {
  if (!current?.id || !session || closing) return;
  const convId = current.id;
  const unsent = session.pendingOf(convId).length;
  const blocked = unsent > 0 && connStatus !== "online"
    ? `보내지 못한 메시지가 ${unsent}개 있어 지금은 종료할 수 없습니다. 연결되어 메시지가 전송되면 다시 시도해 주세요.`
    : "";
  // 작성칸의 파일은 메시지가 아니라 전송 대기 큐에 없다. 닫으면 보내지 않고 버린다 — 확인 창에서 미리 알린다(감사 26).
  const fileWarn = uploads.busy ? ` ${CLOSE_UPLOADING}` : pendingAttachment ? ` ${CLOSE_ATTACHED}` : "";
  const ok = await confirm({ title: CLOSE_TITLE, body: CLOSE_BODY + fileWarn, ok: "종료하기", cancel: "취소", blocked });
  if (!ok || current?.id !== convId) {
    els.endChat.focus?.();
    return;
  }
  closing = true;
  for (const el of [els.endChat, els.input, els.send, els.attachBtn]) el.disabled = true;
  try {
    const conv = await session.closeConversation(convId);
    if (current?.id !== convId || !conv) return;
    clearAttachment(); // 확인 창이 알린 대로 작성칸의 파일은 보내지 않는다
    adoptCurrent(conv);
    // 알림을 띄우지 않는다 — 아래 칸의 "상담이 종료되었습니다" 가 같은 말이고, 알림이 새 문의하기 단추를 가린다. 초점은 새 문의하기로 간다.
    els.newInquiry.focus?.();
  } catch (err) {
    toastError(err);
  } finally {
    closing = false;
    for (const el of [els.endChat, els.input, els.send, els.attachBtn]) el.disabled = false;
    paintClosing();
  }
}

// adoptCurrent 는 서버가 알려 준 지금 상담(응답·실시간 프레임)을 화면에 들인다. 종료되거나 다시 열리면 만족도 카드를 다시 정한다(D45).
function adoptCurrent(c) {
  const before = current.state;
  current = mergeConversation(current, c); // 빈 값을 싣지 않는 필드는 빈 값으로 본다
  describeCurrent();
  loadConversations();
  if (current.state !== before) {
    resetRating();
    loadRating();
  }
}

function totalUnread() {
  return conversations.reduce((sum, c) => sum + (c.id === current?.id && open ? 0 : c.unread || 0), 0);
}

function paintBadges() {
  const n = totalUnread();
  for (const el of [els.launcherBadge, els.navBadge]) {
    el.textContent = n > 99 ? "99+" : String(n);
    el.hidden = n === 0;
  }
  tellHost({ type: "unread", count: n });
}

// 상담 카드(문의내역): "채팅 · 2026.10.06 14:15", 제목(마지막 글), 상태 배지(진행중·종료). 글자는 convCard 가 정한다.
// 상담 제목이 있으면 제목 아래에 회색 보조 줄(마지막 글)이 붙는다.
function convItemHTML(c) {
  const unread = c.id === current?.id && open ? 0 : c.unread || 0;
  const card = convCard(c);
  return `<button class="conv-item" data-id="${c.id}">
    <span class="w-conv-top"><span class="w-conv-kind">${card.kind}</span><span class="when">${escape(card.when)}</span>${unread ? `<span class="badge">${unread > 99 ? "99+" : unread}</span>` : ""}</span>
    <span class="title">${escape(card.title)}</span>
    ${card.sub ? `<span class="w-conv-sub">${escape(card.sub)}</span>` : ""}
    <span class="w-conv-state"><span class="chip ${card.open ? "brand" : ""}">${escape(card.state)}</span></span>
  </button>`;
}

function renderConversations() {
  els.convList.innerHTML = conversations.map(convItemHTML).join("");
  els.listEmpty.innerHTML = emptyHTML(`<img src="${ICON.balloon}" alt="" width="56" height="56">`, "아직 대화가 없습니다", "홈에서 새 문의를 시작해 주세요");
  els.listEmpty.hidden = conversations.length > 0;
  const home = homeSections({ conversations });
  els.recentList.innerHTML = home.recent.map(convItemHTML).join("");
  els.recentCard.hidden = !home.recentVisible;
  for (const btn of document.querySelectorAll(".conv-item")) {
    btn.addEventListener("click", () => openConversation(btn.dataset.id));
  }
  paintBadges();
}

// 상담 목록 다시 받기는 하나로 묶는다(정책 668·669). 같은 틱에 몇 번을 불러도 요청은 하나다 — 지난 글을 다시 받을 때 chat.js 가
// 한 쪽(최대 200줄)을 한 번에 onMessage 로 넘기므로 그 쪽이 요청 하나가 된다. 받는 중에 또 부르면 끝난 뒤 한 번만 더 받는다.
// 기다림은 마이크로태스크 하나와 앞 요청이 끝날 때까지뿐이라 실시간 이벤트의 카드도 곧바로 바뀐다(정책 666). 요청이 겹치지 않아
// 늦게 온 앞 응답이 새 목록을 덮지 않는다.
let listing = false; // 목록을 받는 중이다
let listAgain = false; // 받는 중에 또 불렀다 — 끝나면 한 번 더 받는다
let listSoon = false; // 이번 틱 끝에 받기로 했다
function loadConversations() {
  if (listing) listAgain = true;
  if (listing || listSoon) return;
  listSoon = true;
  queueMicrotask(async () => {
    listSoon = false;
    listing = true;
    try {
      await fetchConversations();
    } finally {
      listing = false;
      if (listAgain) {
        listAgain = false;
        loadConversations();
      }
    }
  });
}

async function fetchConversations() {
  // 회원 로그인이 만료돼 멈췄으면 목록도 묻지 않는다 — 끝난 세션으로 15초마다 401 을 되풀이하지 않는다(감사 20 실측).
  if (!token || memberExpired) return;
  try {
    const out = await api("/api/customer/conversations", { token });
    conversations = out.conversations || [];
    renderConversations();
  } catch (err) {
    // 토큰이 만료됐다. 회원이면 회원 세션을 다시 연다 — 익명으로 열면 회원의 상담이 익명 고객 것으로 바뀐다.
    // 우리 서버의 판정(JSON unauthorized)일 때만이다. 앞단의 401 HTML 로 새 고객이 되면 지난 상담과 큐의 글을 잃는다(감사 22).
    if (isSessionExpired(err)) await sessionEnded();
  }
}

async function openConversation(convId) {
  // 올리던 파일은 그 상담의 것이다. 다른 상담으로 옮기면 멈추고 버린다 — 옮긴 상담의 작성칸에 붙이면 서버가 그 메시지를 거절한다.
  const moved = convId !== current?.id;
  // 목록에 없더라도 앞 상담을 current 로 남기지 않는다. 머리·업로드가 앞 상담을, 보내기가 이 상담을 가리키게 된다.
  current = conversations.find((c) => c.id === convId) || { id: convId };
  view.clear();
  els.typing.hidden = true;
  if (moved && clearAttachment()) toast("다른 상담으로 옮겨 올리던 파일을 취소했습니다");
  session.open(convId, { fromStart: true });
  setView("chat");
  resetRating();
  loadRating();
  const back = held.get(convId);
  if (back !== undefined && restoreDraft(els.input, back)) {
    held.delete(convId);
    autoGrow(els.input);
  }
  loadConversations();
}

async function startConversation() {
  // 홈의 "새 문의 시작하기" 와 고객이 닫은 상담의 "새 문의하기" 가 같은 길이다. 두 번 눌러도 한 번만 연다.
  if (els.startChat.disabled) return;
  els.startChat.disabled = true;
  els.newInquiry.disabled = true;
  try {
    // 세션이 없으면 먼저 연다. 못 열면 그 까닭이 안내 칸에 남는다("토큰이 없다" 를 띄우지 않는다).
    if (!token && !(await establish())) return;
    const out = await api("/api/customer/conversations", { token, method: "POST" });
    conversations.unshift({ ...out.conversation, unread: 0 });
    await openConversation(out.conversation.id);
  } catch (err) {
    toastError(err);
  } finally {
    els.startChat.disabled = false;
    els.newInquiry.disabled = false;
  }
}

// clearAttachment 는 작성칸의 첨부를 비운다. 올리는 중이면 멈춘다. 멈춘 업로드가 있었으면 true.
function clearAttachment() {
  const stopped = uploads.cancel();
  resetAttachRow();
  return stopped;
}

function resetAttachRow() {
  pendingAttachment = null;
  els.attachHint.innerHTML = "";
  els.attachRow.hidden = true;
  els.file.value = "";
}

// 업로드 진행률: chat.js 의 uploadAttachment 에 onProgress 를 주면 XHR 로 올리며 (loaded, total) 을 알려 준다.
// 첫 값이 오기 전에는 막대가 흐르는 모양이고, 다 올라가면 서버 확인을 기다리는 "확인 중" 이 된다(D38).
function paintProgress(loaded, total) {
  const bar = els.attachHint.querySelector(".w-progress");
  const sub = els.attachHint.querySelector(".w-pill-sub");
  if (!bar || !sub) return;
  const p = uploadProgress(loaded, total);
  if (!p.known) return;
  bar.classList.add("is-known");
  bar.setAttribute("aria-valuenow", String(p.percent));
  // eslint-disable-next-line no-restricted-properties -- 진행률 막대 폭(올린 바이트에 따라 정해진다). 정책 645 예외
  bar.querySelector("i").style.width = `${p.percent}%`;
  sub.textContent = p.phase === "verifying" ? `확인 중… ${humanSize(total)}` : `올리는 중… ${p.percent}% · ${humanSize(loaded)} / ${humanSize(total)}`;
}

// showReady 는 보낼 준비가 된 첨부를 작성칸에 붙인다. 방금 올린 것과 거절로 돌려받은 것(giveBack) 둘 다 여기로 온다.
function showReady(att, fallbackType = "") {
  pendingAttachment = att;
  els.attachRow.hidden = false;
  els.attachHint.innerHTML = `<span class="attach-pill w-ready">
      <img src="${attachIcon(att.content_type || fallbackType)}" alt="" width="28" height="28">
      <span class="w-pill-text"><span class="w-pill-name">${escape(att.filename || "")}</span>
        <span class="w-pill-sub">${humanSize(att.byte_size || 0)} · 보낼 준비가 되었습니다</span></span>
      <button type="button" id="dropAttach" aria-label="첨부 취소">×</button></span>`;
  $("dropAttach").addEventListener("click", clearAttachment);
}

// 업로드는 시작한 상담(convId)에 묶인다. 끝났을 때 지금 상담이 다르거나 취소했으면 결과를 버린다 — 지금 작성칸에 붙이지 않는다.
async function attachFiles(files) {
  const file = files[0];
  if (!file || !current) return;
  const convId = current.id;
  const ticket = uploads.begin(convId);
  pendingAttachment = null;
  const icon = attachIcon(file.type);
  els.attachRow.hidden = false;
  els.attachHint.innerHTML = `<span class="attach-pill w-uploading" role="status">
    <img src="${icon}" alt="" width="28" height="28">
    <span class="w-pill-text"><span class="w-pill-name">${escape(file.name)}</span>
      <span class="w-pill-sub">올리는 중… ${humanSize(file.size)}</span>
      <span class="w-progress" role="progressbar" aria-label="업로드 진행률" aria-valuemin="0" aria-valuemax="100"><i></i></span></span>
    <button type="button" id="cancelUpload" aria-label="업로드 취소">×</button></span>`;
  $("cancelUpload").addEventListener("click", () => {
    if (uploads.owns(ticket, current?.id)) clearAttachment();
  });
  try {
    const att = await session.uploadAttachment(convId, file, {
      signal: ticket.signal,
      onProgress: (loaded, total) => {
        if (uploads.owns(ticket, current?.id)) paintProgress(loaded, total);
      },
    });
    if (!uploads.owns(ticket, current?.id)) return;
    uploads.finish(ticket);
    showReady(att, file.type);
  } catch (err) {
    // 취소했거나 다른 상담으로 옮겨 멈춘 업로드다. 지금 작성칸과 상관없으니 알리지 않는다.
    if (!uploads.owns(ticket, current?.id)) return;
    uploads.finish(ticket);
    resetAttachRow();
    toastError(err);
  }
}

// ── 거절된 글 ────────────────────────────────────────────────────

// giveBack 은 서버가 영구 거절한 메시지의 글을 고객에게 돌려준다. 거절 까닭(서버 문구)은 언제나 보인다.
//   화면에 열린 상담의 글이고 입력칸이 비어 있으면 입력칸에 넣는다. 새 글이 있으면 덮지 않고 앞부분과 복사 단추를 보인다.
//   다른 상담이 열려 있으면 그 상담을 다시 열 때 넣는다.
//   열린 상담이 없으면(새로고침 직후 홈·목록) 그 상담을 열 때 넣고, 지금은 앞부분과 복사 단추를 남긴다 — 어디에도 안 보이면 그게 유실이다.
//   첨부가 실려 있었으면(attachmentGiveBack) 아직 보내지지 않은 업로드는 같은 상담의 비어 있는 작성칸에 다시 붙이고,
//   그럴 수 없으면 파일 이름을 들어 다시 올려 달라고 알린다 — 글만 돌려주고 사진을 조용히 잃지 않는다.
function giveBack(back, reason, traceId, clientMsgId) {
  const shown = els.viewChat.hidden ? null : current?.id;
  if (back.code === "closed_by_customer" && back.body) return giveBackClosed(back, reason, traceId, clientMsgId, shown);
  const file = attachmentGiveBack(back, { shown, slotFree: !pendingAttachment && !uploads.busy });
  if (file === "reattach") showReady(back.attachment);
  const why = file === "lost"
    ? `${reason} · 파일 ${back.attachment.filename || ""} 은 보내지 못했습니다. 다시 올려 주세요`
    : file === "reattach" ? `${reason} · 파일을 작성칸에 다시 붙여 두었습니다` : reason;
  if (!back.body) return toast(why, "danger", { traceId });
  const unsent = () => toast(`${why} · 보내지 못한 글: ${clip(back.body)}`, "danger", { copy: back.body, traceId });
  if (back.convId === shown) {
    if (restoreDraft(els.input, back.body)) {
      autoGrow(els.input);
      return toast(`${why} · 쓰신 글을 입력칸에 되돌려 두었습니다`, "danger", { traceId });
    }
    return unsent();
  }
  if (held.has(back.convId)) return unsent();
  held.set(back.convId, back.body);
  if (!shown) return unsent();
  toast(`${why} · 쓰신 글은 그 상담을 다시 열면 입력칸에 돌아갑니다`, "danger", { copy: back.body, traceId });
}

// giveBackClosed 는 고객이 닫은 상담에 늦게 나간 글을 돌려준다. 그 상담에는 입력칸이 없다 — 숨은 입력칸에 넣으면 안 보이고 사라진다.
// 저장소에 남겨 그 상담의 아래 칸에 보인다(paintReturned). 그 상담을 보고 있으면 알림을 띄우지 않는다 — 아래 칸이 같은 말이고
// 알림이 새 문의하기를 가린다. 다른 화면에 있으면 잠깐 뜨는 알림(복사 단추 없이 저절로 닫힘)으로 어디서 찾는지 알린다.
// 서버가 그 상담이 닫혔다고 답했으니 보고 있는 상담이면 화면도 닫힌 상담으로 바꾼다(대화 프레임이 늦을 수 있다).
function giveBackClosed(back, reason, traceId, clientMsgId, shown) {
  keepReturned(back, clientMsgId);
  if (back.convId === current?.id && !customerClosed(current)) {
    current = { ...current, state: "CLOSED", closed_by: "CUSTOMER" };
    describeCurrent();
    loadConversations();
  } else if (back.convId === current?.id) {
    paintClosing();
  }
  if (back.convId === shown) return;
  toast(`${reason} · 쓰신 글은 그 상담을 열면 아래에 남아 있습니다`, "danger", { traceId });
}

function onSessionError(message, clientMsgId, dropped, traceId) {
  const isQueued = (convId, id) => !!session && session.stateOf(convId).pending.some((p) => p.client_msg_id === id);
  const out = drafts.settle(clientMsgId, dropped, isQueued, session?.convId);
  if (out.quiet) return;
  if (out.back) giveBack(out.back, message, traceId, clientMsgId);
  else toast(message, "danger", { traceId });
}

// ── 홈의 상담원 묶음 ─────────────────────────────────────────────

// 묶음은 고객마다 따로다. 세션을 열어 고객을 알기 전에는 아무 이름도 보이지 않는다.
let agents = [];
let agentsOwner = "";

// loadAgents 는 고객이 바뀌었을 때 그 고객의 묶음을 불러온다. 남의 것이 저장돼 있으면 지운다.
function loadAgents(owner) {
  let saved;
  try { saved = JSON.parse(localStorage.getItem(AGENTS_KEY) || "null"); } catch { saved = null; }
  agentsOwner = owner;
  agents = agentsFor(saved, owner);
  if (saved && agents.length === 0) {
    try { localStorage.removeItem(AGENTS_KEY); } catch { /* 못 지워도 다음에 주인을 보고 버린다 */ }
  }
  paintAgents();
}

function noteAgent(name) {
  const next = rememberAgent(agents, name);
  if (next.join("\n") === agents.join("\n")) return;
  agents = next;
  try { localStorage.setItem(AGENTS_KEY, JSON.stringify({ owner: agentsOwner, names: agents })); } catch { /* 저장이 막혀도 이번 화면에서는 보인다 */ }
  paintAgents();
}

function paintAgents() {
  els.agentStack.innerHTML = agents.length
    ? agents.map((n) => avatarHTML(n)).join("")
    : `<span class="avatar w-avatar-icon"><img src="${ICON.headset}" alt="" width="22" height="22"></span>`;
}

// ── 시간 묶음 ────────────────────────────────────────────────────

// 같은 사람이 같은 분에 보낸 말풍선은 마지막 줄에만 시간을 남긴다. 줄은 ui.js 가 그리고, 여기는 표시만 고른다.
function regroupTimes(root) {
  const rows = [...root.children].filter((el) => el.classList?.contains("row"));
  const hides = timeGroupHides(rows.map((el) => ({
    same: el.classList.contains("same"),
    time: el.classList.contains("system") ? "" : el.querySelector(".meta > span")?.textContent || "",
  })));
  rows.forEach((el, i) => el.classList.toggle("t-grouped", hides[i]));
}

// ── 만족도(D45) ──────────────────────────────────────────────────

// 종료된 상담의 대화 끝에 별점 카드를 둔다. 무엇을 그릴지는 서버의 상태(rating)와 고르는 중인 값(ratingDraft)이 정하고(ratingCardHTML),
// 카드 하나에 이벤트를 걸어 별(data-score)·수정하기(data-action)·보내기(submit)를 가른다. 막는 것은 서버다(상담당 한 번, 24시간, 그 상담의 고객만).
let rating = null;
let ratingDraft = { score: 0, comment: "", editing: false };
let ratingTicket = 0;

function resetRating() {
  rating = null;
  ratingDraft = { score: 0, comment: "", editing: false };
  ratingTicket++;
  renderRating();
}

async function loadRating() {
  const convId = current?.id;
  if (!convId || current.state !== "CLOSED" || !token) return;
  const ticket = ++ratingTicket;
  try {
    const out = await api(`/api/conversations/${convId}/rating`, { token });
    if (ticket !== ratingTicket || current?.id !== convId) return; // 그 사이 다른 상담을 열었다
    rating = out;
    renderRating();
    if (!els.csat.hidden) view.toEnd(); // 카드는 대화 끝에 붙는다 — 닫힌 상담을 열면 바로 보이게
  } catch {
    // 평가 칸은 덤이다. 못 받으면 카드를 그리지 않는다.
  }
}

function renderRating() {
  const html = current?.state === "CLOSED" ? ratingCardHTML(rating, ratingDraft) : "";
  els.csat.innerHTML = html;
  els.csat.hidden = !html;
}

async function submitRating() {
  const convId = current?.id;
  if (!convId || !ratingDraft.score) return;
  const comment = $("csatComment")?.value ?? ratingDraft.comment;
  ratingDraft.comment = comment;
  try {
    const out = await api(`/api/conversations/${convId}/rating`, { token, method: "PUT", body: { score: ratingDraft.score, comment } });
    if (current?.id !== convId) return;
    rating = out;
    ratingDraft = { score: 0, comment: "", editing: false };
    renderRating();
    toast("평가를 보냈습니다. 고맙습니다", "ok");
  } catch (err) {
    toastError(err);
    if (err.status === 409) loadRating(); // 다시 열렸거나 시간이 지났다 — 지금 상태로 다시 그린다
  }
}

els.csat.addEventListener("click", (e) => {
  const star = e.target?.closest?.("[data-score]");
  if (star) {
    ratingDraft.comment = $("csatComment")?.value ?? ratingDraft.comment;
    ratingDraft.score = Number(star.dataset.score);
    renderRating();
    $(`csatStar${ratingDraft.score}`)?.focus?.();
    return;
  }
  if (e.target?.closest?.("[data-action=edit]") && rating?.rating) {
    ratingDraft = { score: rating.rating.score, comment: rating.rating.comment || "", editing: true };
    renderRating();
  }
});
// 별 묶음은 라디오다 — 왼쪽·오른쪽 화살표로 고른다.
els.csat.addEventListener("keydown", (e) => {
  if (!e.target?.closest?.("[data-score]") || !["ArrowLeft", "ArrowRight"].includes(e.key)) return;
  e.preventDefault();
  const next = Math.min(5, Math.max(1, (ratingDraft.score || 1) + (e.key === "ArrowRight" ? 1 : -1)));
  ratingDraft.comment = $("csatComment")?.value ?? ratingDraft.comment;
  ratingDraft.score = next;
  renderRating();
  $(`csatStar${next}`)?.focus?.();
});
els.csat.addEventListener("submit", (e) => {
  e.preventDefault();
  submitRating();
});

// ── 세션 ─────────────────────────────────────────────────────────

function startSession() {
  session = new ChatSession({
    owner: `customer:${customerID}`,
    token,
    handlers: {
      onMessage: (m) => {
        view.append(m);
        // 상담 카드의 제목은 마지막 글이다. 지우거나 고친 이벤트도 그 글을 바꾸므로 새 글과 같이 다시 받는다(정책 666). 요청은 loadConversations 가 묶는다(668).
        loadConversations();
        if (m.kind !== "MESSAGE") return; // 수정·삭제 이벤트는 새 메시지가 아니다
        // 상담원이 답했다 = 그 앞의 내 메시지를 읽었다. 읽기만 한 건 서버가 알려 주지 않는다.
        if (m.sender_kind === "AGENT") {
          view.setReadFrom("AGENT", m.seq);
          noteAgent(m.sender_name);
        }
        if (m.sender_kind === "AGENT" || m.sender_kind === "BOT" || m.sender_kind === "AI") notifier.show("상담 답변", m.body || "파일을 보냈습니다");
      },
      onPending: (list) => {
        drafts.rememberAll(session?.convId, list);
        view.setPending(list);
        settleMovedReturned(); // ack 로 큐에서 빠진 글이면 미뤄 둔 돌려받은 글 지우기를 마저 한다
      },
      onStatus: setStatus,
      // 보낸 글이 오래 ack 를 못 받고 있다. 큐에 남아 연결되면 같은 글이 한 번만 나간다 — 포기하지 않는다(D5).
      onStalled: (s) => view.setPendingNotice(!s ? "" : s.offline
        ? "연결이 끊겨 있습니다. 다시 연결되면 보냅니다"
        : "메시지 전송이 지연되고 있습니다. 자동으로 다시 보냅니다"),
      onReady: (f) => {
        const mine = (f.reads || []).find((r) => r.reader_kind === "CUSTOMER");
        view.markUnreadFrom(mine?.last_read_seq || 0);
      },
      // 상태가 바뀌었다(상담원의 종료·다시 열기, 이 고객의 다른 탭에서 누른 상담 종료). 머리·아래 칸·만족도 카드를 지금 상태로.
      onConversation: (c) => {
        if (!c || c.id !== current?.id) return;
        adoptCurrent(c);
      },
      onReads: (reads) => reads.forEach((r) => view.setReadFrom(r.reader_kind, r.last_read_seq)),
      onRead: (f) => view.setReadFrom(f.reader_kind, f.seq),
      onTyping: (f) => showTyping(f.sender_name || "상담원"),
      // 메시지 하나가 영구 거절되면(client_msg_id 가 온다) chat.js 는 큐에서 지운다. 쓴 글은 화면에서 사라지면 안 되니 돌려준다.
      onError: onSessionError,
      // 소켓 티켓을 받으려다 401 이다(세션 만료·유휴 만료·비밀 재발급). 목록 조회의 401 과 같다 — 회원이면 회원 세션을 되살리고(recoverMember),
      // 아니면 새 익명 세션을 연다.
      onUnauthorized: () => sessionEnded(),
      // 보던 상담이 사라졌다(상담원이 삭제했거나 새 문의로 지난 상담이 됐다). 오류가 아니라 상태라 중립 안내를 한 번 띄우고 목록으로 간다.
      onGone: () => {
        current = null;
        resetRating();
        view.clear();
        clearAttachment();
        els.typing.hidden = true;
        setView("list");
        loadConversations();
        toast("이 상담은 더 이상 열 수 없습니다");
      },
    },
  });
  session.start();
}

async function loadSettings() {
  try {
    settings = await api("/api/widget/settings");
  } catch {
    settings = null;
    return;
  }
  // 관리자가 정한 색이 기준이다. 임베드도 같다. 호스트 설치 코드가 data-color 를 줬을 때만 그 색이 덮는다(옛 설치 코드 호환).
  // 런처는 iframe 안에 있어서 같은 색을 따른다.
  applyBrand(hostColor || settings.color);
  if (!EMBED) document.body.classList.toggle("pos-left", settings.position === "left");
  // 관리자가 정한 제목은 머리의 로고 옆 이름이다(setView). 인사 배너의 제목은 "안녕하세요" 그대로다(머리는 이름, 배너는 인사).
  els.greetText.textContent = settings.greeting || els.greetText.textContent;
  // 자주 묻는 질문은 관리자가 데스크에서 고친다(D36). 저장한 적이 없으면 서버가 기본 세 줄을 준다. 비어 있으면 칸을 숨긴다.
  // 저장한 목록은 이 설정을 다시 받을 때(위젯을 새로 열거나 5분마다) 바뀐다. 실시간으로 밀어 주지 않는다.
  // 홈에는 앞의 몇 개만 알약으로, 전체는 자주 묻는 질문 화면에 아코디언으로 그린다(D44).
  const faq = Array.isArray(settings.faq) ? settings.faq : [];
  els.faq.innerHTML = faqPillsHTML(faq);
  els.faqList.innerHTML = faqHTML(faq);
  faqCount = faq.length;
  els.faqCard.hidden = !homeSections({ faq }).faqVisible;
  // 자주 묻는 질문을 보던 중에 목록이 비면 홈으로 돌아간다.
  if (!els.viewFaq.hidden && faq.length === 0) setView("home");
  els.hoursPill.innerHTML = settings.open_now
    ? `<span class="chip ok w-hours">지금 운영 중</span>`
    : `<span class="chip warn w-hours">운영시간 외 · 남겨 주시면 순서대로 답합니다</span>`;
  if (!open) setView("home");
}

// 세션 수립은 한 줄이다(감사 9 — 회원 첫 방문 9번에 1번이 익명으로 열렸다. boot 설정의 회원 토큰·대기열의 identify·익명 boot 가
// 동시에 나가 가장 늦게 온 익명 응답이 회원 세션을 덮었다).
//   호스트가 회원 토큰을 줄 예정이면(boot 설정에 memberToken, 진행 중인 identify) 익명 세션을 열지 않는다 — 떠도는 익명 고객이 생기지 않는다.
//   응답은 가장 마지막에 시작한 요청의 것만 채택한다. 늦게 온 옛 응답은 버린다(sessionTicket).
//   회원 세션이 실패하면 까닭을 알림으로 보이고, 열린 세션이 없으면 익명으로 이어 간다.
let sessionTicket = 0;
let memberAttempt = null; // { token, promise } — 진행 중인 회원 세션 요청

// boot 는 익명 세션을 연다. fresh 면 저장된 토큰을 버린다. 회원 세션을 여는 중이면 그 결과를 기다린다.
// 못 열면 까닭을 남기고, 429 처럼 기다리면 풀리는 거절이면 Retry-After 뒤에 저절로 다시 시도한다.
async function boot({ fresh = false } = {}) {
  if (memberAttempt) return memberAttempt.promise;
  clearTimeout(retryTimer);
  retryTimer = null;
  const ticket = ++sessionTicket;
  const held = fresh ? "" : storedToken();
  try {
    const res = await api("/api/customer/session", {
      method: "POST",
      body: { token: held, ...tenantFields() },
    });
    if (ticket !== sessionTicket) return !!token; // 그 뒤에 시작한 요청이 있다. 이 응답은 채택하지 않는다
    if (held && res.token !== held) revoke(held); // 이어 주지 않은 세션(회원 세션·만료)은 이 브라우저에서 다시 닿을 길이 없다
    adoptSession(res);
    setSessionError(null);
    return true;
  } catch (err) {
    if (ticket !== sessionTicket) return !!token;
    setSessionError(sessionErrorNotice(err));
    if (sessionError.retryInMs > 0) retryTimer = setTimeout(() => boot({ fresh }).then(paintBadges), sessionError.retryInMs);
    return false;
  }
}

// establish 는 지금 알맞은 세션을 연다. 회원 토큰을 받았으면 회원, 아니면 익명이다.
function establish(opts) {
  return memberToken ? identify(memberToken) : boot(opts);
}

// sessionEnded 는 들고 있던 세션이 서버에서 끝났을 때다(우리 서버의 401 unauthorized, isSessionExpired). 회원이면 회원 세션을 되살리고, 익명이면 새 익명 세션을 연다.
// 익명으로 열면 회원의 상담이 익명 고객 것으로 바뀐다(감사 10 N25).
function sessionEnded() {
  return memberToken ? recoverMember() : boot({ fresh: true });
}

// ── 회원 세션이 끝났을 때(감사 20, logcat 결정) ──────────────────────
// 회원 세션은 회원 토큰 exp 에 끝난다(D46). 전에는 끝난 뒤 같은(만료된) 회원 토큰으로 member-session 400 과 티켓 401 을 8초마다 끝없이 되풀이했다.
// 이제 한 번에 셋까지만 한다.
//   1) 들고 있는 회원 토큰으로 다시 연다 — 세션만 밀려났으면(상한 20개 정리) 토큰은 아직 산다.
//   2) 우리 서버가 거절하면(JSON 오류 본문) 호스트에 새 회원 토큰을 묻는다(embed.js 의 getMemberToken). 받으면 그것으로 다시 연다. 같은 회원이면 소켓만 새 토큰으로 다시 붙는다.
//   3) 그래도 안 되면(호스트에 함수가 없음·빈 값·10초 넘김·새 토큰도 거절) 재시도를 멈추고 새로고침 안내를 보인다.
// 일시 실패(네트워크·5xx·429·JSON 이 아닌 앞단의 4xx)는 멈추지 않는다 — 소켓의 백오프가 다음 401 에서 다시 부른다. 보내지 못한 글은 어느 경우에도 저장소 큐에 남는다.
const MEMBER_TOKEN_WAIT = 10000; // 호스트의 getMemberToken 을 기다리는 시한. 호스트 서버가 토큰을 서명해 주는 왕복을 느린 망에서도 덮는다
let memberRecovery = null; // 진행 중인 되살리기
let memberExpired = false; // 되살리지 못해 멈췄다. 호스트가 새 토큰으로 identify 하거나 새로고침할 때까지 다시 시도하지 않는다
let memberAsk = 0;
const memberAsks = new Map(); // 요청 id → resolve

function recoverMember() {
  if (memberRecovery) return memberRecovery;
  if (memberExpired) return Promise.resolve(false);
  memberRecovery = (async () => {
    let r = await openMember(memberToken);
    if (r.state !== "rejected") return r.state !== "transient";
    const fresh = await askHostForMemberToken();
    if (fresh && fresh !== r.token) {
      r = await openMember(fresh);
      if (r.state !== "rejected") return r.state !== "transient";
    }
    memberLoginExpired();
    return false;
  })().finally(() => {
    memberRecovery = null;
  });
  return memberRecovery;
}

// askHostForMemberToken 은 호스트에 새 회원 토큰을 묻는다. 호스트가 새 토큰을 줄 수 없거나(옛 embed.js·getMemberToken 없음) 답이 없으면 빈 값이다.
function askHostForMemberToken() {
  if (!EMBED || !hostMemberRefresh) return Promise.resolve("");
  const id = ++memberAsk;
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      memberAsks.delete(id);
      resolve("");
    }, MEMBER_TOKEN_WAIT);
    memberAsks.set(id, (tok) => {
      clearTimeout(timer);
      resolve(tok);
    });
    tellHost({ type: "member-token-request", id });
  });
}

function answerMemberToken(id, tok) {
  const done = memberAsks.get(id);
  if (!done) return; // 시한이 지났거나 묻지 않은 답이다
  memberAsks.delete(id);
  done(typeof tok === "string" ? tok : "");
}

// openMember 는 회원 토큰으로 세션을 연다. state 는 ok(열었다)·stale(그 사이 다른 세션 요청이 시작됐다)·rejected(우리 서버가 JSON 오류로 거절했다)·
// transient(판정 못 함 — 네트워크·5xx·429·JSON 이 아닌 앞단의 4xx) 중 하나다. 알림은 띄우지 않는다 — 부르는 쪽이 정한다.
// 거절 판정은 큐의 그것과 같다(isTransientFailure, D5) — 앞단(WAF·프록시)의 403 HTML 로 회원 회복을 멈추면 새로고침 전까지 글이 나가지 않는다(감사 21).
async function openMember(nextMemberToken) {
  if (!nextMemberToken) return { state: "rejected", token: "" };
  memberToken = nextMemberToken;
  const ticket = ++sessionTicket;
  try {
    // 들고 있는 세션이 바로 그 회원의 것이면 서버가 이어 쓴다 — 페이지마다 identify 해도 세션이 쌓이지 않는다(D46).
    const held = storedToken();
    const res = await api("/api/customer/member-session", { method: "POST", body: { member_token: nextMemberToken, token: held, ...tenantFields() } });
    if (ticket !== sessionTicket) return { state: "stale", token: nextMemberToken, ticket };
    if (held && res.token !== held) revoke(held);
    memberExpired = false;
    adoptSession(res);
    setSessionError(null);
    return { state: "ok", token: nextMemberToken, ticket };
  } catch (err) {
    if (ticket !== sessionTicket) return { state: "stale", token: nextMemberToken, ticket };
    return { state: isTransientFailure(err) ? "transient" : "rejected", token: nextMemberToken, ticket, err };
  }
}

// memberLoginExpired 는 회원 세션을 되살리지 못했을 때다. 소켓·REST 재시도를 멈춘다(무한 400·401 반복을 막는다).
// 세션 객체는 남긴다 — 보내지 못한 글은 저장소 큐(소유자 customer:<id>)에 그대로 있고, 같은 회원으로 다시 들어오면(새로고침·호스트의 identify) 나간다.
function memberLoginExpired() {
  memberExpired = true;
  session?.stop();
  setStatus("offline");
  setSessionError({ text: MEMBER_EXPIRED_TEXT, retryInMs: 0, final: true });
  toast(MEMBER_EXPIRED_TEXT, "danger");
}
const MEMBER_EXPIRED_TEXT = "로그인이 만료되었습니다. 페이지를 새로고침해 주세요";

function setSessionError(notice) {
  sessionError = notice;
  els.sessionNotice.hidden = !notice;
  els.sessionNoticeText.textContent = notice?.text || "";
  // 다시 해도 같은 안내(회원 로그인 만료)에는 "다시 연결" 단추를 두지 않는다.
  els.sessionRetry.hidden = !!notice?.final;
  if (!els.viewHome.hidden) setView("home");
}

// identify 는 호스트가 서명한 회원 토큰으로 세션을 바꾼다. 같은 회원이면 대화 이력이 따라온다.
// 같은 토큰으로 이미 여는 중이면 그 요청을 같이 기다린다(boot 설정과 대기열의 identify 가 같은 토큰을 두 번 준다).
function identify(nextMemberToken) {
  if (!nextMemberToken) return Promise.resolve(!!token);
  if (memberAttempt?.token === nextMemberToken) return memberAttempt.promise;
  clearTimeout(retryTimer); // 익명 boot 의 재시도가 회원 세션을 덮지 않게 한다
  retryTimer = null;
  const attempt = { token: nextMemberToken };
  let ticket = 0;
  attempt.promise = (async () => {
    try {
      const r = await openMember(nextMemberToken);
      ticket = r.ticket;
      if (r.state === "ok") return true;
      if (r.state === "stale") return !!token;
      // 열린 세션이 있으면 그대로 쓴다. 없으면 익명으로 이어 간다 — 고객이 상담을 못 여는 것보다 낫다.
      // 서버 문구(회원 연동 설정·토큰 형식)는 고객이 할 수 있는 일이 아니라 보이지 않는다. 문의 번호로 운영자가 까닭을 찾는다(D47).
      toast(`회원 정보를 확인하지 못했습니다${token ? "" : ". 비회원으로 상담을 이어 갑니다"}`, "danger", { traceId: r.err?.traceId });
      return token ? true : null;
    } finally {
      if (memberAttempt === attempt) memberAttempt = null;
    }
  })().then((ok) => (ok === null && ticket === sessionTicket ? boot() : ok));
  memberAttempt = attempt;
  return attempt.promise;
}

// storedToken 은 저장해 둔 고객 세션 토큰이다. 저장소가 막힌 브라우저면 빈 값이다(DEF-95).
function storedToken() {
  try { return localStorage.getItem(TOKEN_KEY) || ""; } catch { return ""; }
}

// revoke 는 이 브라우저가 더는 쓰지 않을 세션을 서버에서 끊는다(D46). 실패해도(오프라인·이미 만료) 그만이다 —
// 남은 세션은 유휴 한도·회원 토큰 만료에 죽고, 익명 입구는 회원 세션을 이어 주지 않는다.
// 회원이 호스트에서 로그아웃만 하고(위젯 logout 없이) 다음 사람이 위젯을 열면, 익명 입구가 그 회원 세션을 이어 주지 않고 여기서 끊긴다.
function revoke(oldToken) {
  api("/api/logout", { token: oldToken, method: "POST" }).catch(() => {});
}

// logout 은 호스트의 ChatWidget.logout() 이다(D46, 공유 PC). 서버 세션을 끊고, 위젯 저장소의 이 위젯 것(토큰·보내지 못한 글·커서·
// 최근 상담원)을 지운 뒤 익명으로 다시 연다. 보내지 못한 글은 회원의 것이라 다음 사람이 보면 안 된다 — 지우는 대신 잃는다.
// 서버 세션 끊기가 실패해도(오프라인) 저장소는 비운다. 남은 서버 세션은 회원 토큰 만료·유휴 한도에 죽고, 익명 입구는 그 토큰을 이어 주지 않는다.
async function logout() {
  // 끊을 세션: 지금 쓰는 것과 저장소에 있는 것. 로그아웃 페이지가 뜨자마자 부르면 그 사이 익명 세션이 먼저 열려 둘이 다를 수 있다.
  const was = [...new Set([token, storedToken()].filter(Boolean))];
  memberToken = "";
  memberAttempt = null;
  memberExpired = false;
  clearTimeout(retryTimer);
  retryTimer = null;
  sessionTicket += 1; // 진행 중인 세션 요청의 응답은 채택하지 않는다
  session?.discard(); // 저장하지 못해 이 탭의 메모리에만 있는 글도 버린다(DEF-175)
  session = null;
  clearToasts(); // 앞 사람의 알림(돌려받은 글 되돌리기·보내지 못한 글 복사)을 다음 사람이 누르지 않게 치운다
  token = "";
  customerID = "";
  current = null;
  conversations = [];
  view.clear();
  clearAttachment();
  held.clear();
  try {
    const keys = [];
    for (let i = 0; i < localStorage.length; i++) keys.push(localStorage.key(i));
    for (const k of keys) if (k?.startsWith("gcs.")) localStorage.removeItem(k);
  } catch { /* 저장소가 막혔다. 지울 것도 없다 */ }
  await Promise.all(was.map((t) => api("/api/logout", { token: t, method: "POST" }).catch(() => {})));
  setView("home");
  renderConversations();
  await boot({ fresh: true });
  paintBadges();
}

function adoptSession(res) {
  try { localStorage.setItem(TOKEN_KEY, res.token); } catch { /* 저장이 막힌 브라우저다. 이번 화면에서만 쓴다(DEF-95) */ }
  const sameCustomer = customerID && customerID === (res.customer?.id || "");
  token = res.token;
  customerID = res.customer?.id || "";
  current = res.conversation || null;
  if (customerID !== agentsOwner) loadAgents(customerID);
  if (!session) startSession();
  else if (sameCustomer) {
    // 같은 사람, 새 토큰. 열린 소켓은 옛 토큰으로 인증돼 있다. 로그인 만료로 멈춰 있었으면 다시 돌린다 — 저장소 큐의 글이 그대로 나간다.
    session.reconnect(token);
    if (session.stopped) session.start();
  }
  else {
    // 다른 사람이 됐다(익명→회원, 회원 A→B). 큐·커서·구독을 전부 새 주체 것으로 바꾼다. 앞 사람의 저장하지 못한 글(메모리)도 버린다(DEF-176).
    session.discard();
    session = null;
    clearToasts(); // 앞 사람의 알림(보내지 못한 글 복사·돌려받은 글 되돌리기)을 새 주체가 누르지 않게 치운다(감사 30)
    view.clear();
    clearAttachment(); // 앞 사람이 올리던 파일은 그 사람의 상담 것이다
    held.clear();
    startSession();
  }
  loadConversations();
}

// ── 이벤트 ───────────────────────────────────────────────────────

els.launcher.addEventListener("click", () => setOpen(true));
els.close.addEventListener("click", () => setOpen(false));
els.back.addEventListener("click", goBack);
els.faqAll.addEventListener("click", () => openFaq(undefined, "faqAll"));
els.faqStart.addEventListener("click", startConversation);
els.recentAll.addEventListener("click", () => {
  loadConversations();
  setView("list");
});
// 질문 알약과 아코디언은 다시 그려지므로 칸에 한 번만 건다(이벤트 위임).
els.faq.addEventListener("click", (e) => {
  const pill = e.target?.closest?.(".faq-pill");
  if (pill) openFaq(Number(pill.dataset.i), `wFaqPill${pill.dataset.i}`);
});
els.faqList.addEventListener("click", (e) => {
  const trigger = e.target?.closest?.(".accordion-trigger");
  if (trigger) setFaqOpen(trigger, trigger.getAttribute("aria-expanded") !== "true");
});
els.startChat.addEventListener("click", startConversation);
els.endChat.addEventListener("click", askClose);
// 새 문의하기는 닫힌 상담에서 돌려받은 글을 새 상담의 입력칸으로 옮긴다.
els.newInquiry.addEventListener("click", async () => {
  const from = current?.id;
  await startConversation();
  if (from && current?.id && current.id !== from) moveReturned(from);
});
els.sessionRetry.addEventListener("click", () => establish().then(paintBadges));
for (const btn of els.nav.querySelectorAll("button")) {
  btn.addEventListener("click", () => {
    if (btn.dataset.view === "list") loadConversations();
    setView(btn.dataset.view);
  });
}

els.composer.addEventListener("submit", (e) => {
  e.preventDefault();
  // 고객이 닫아 둔 상담이거나 닫는 중이면 보내지 않는다(서버도 closed_by_customer 로 거절한다).
  if (!current || closing || customerClosed(current)) return;
  const body = els.input.value;
  if (!body.trim() && !pendingAttachment) return;
  const sentAttachment = pendingAttachment;
  const item = session.send(body, { attachment: sentAttachment });
  if (!item) return; // 큐에 못 들어갔으면 작성칸을 지킨다
  sendMovedReturned(body, item); // 입력칸으로 옮긴 돌려받은 글은 이제 전송 큐가 지킨다(DEF-178, 감사 30)
  drafts.remember(current.id, item);
  els.input.value = "";
  // 보낸 첨부만 비운다. 올리는 중인 파일은 글만 먼저 보내도 계속 올라간다.
  if (sentAttachment) resetAttachRow();
  autoGrow(els.input);
  els.input.focus();
});
els.input.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    els.composer.requestSubmit();
  }
});
els.input.addEventListener("input", () => {
  autoGrow(els.input);
  trackMovedLink();
  if (els.input.value.trim()) session?.typing();
});
els.file.addEventListener("change", () => attachFiles([...els.file.files]));
els.attachBtn.addEventListener("click", () => els.file.click());
if (typeof MutationObserver === "function") {
  for (const root of [els.msgs, els.pending]) new MutationObserver(() => regroupTimes(root)).observe(root, { childList: true });
}
els.attachRow.hidden = true;
paintAgents();
dropzone(els.viewChat, attachFiles);

// 닫혀 있어도 안 읽은 수는 알아야 한다. 세션을 미리 연다.
// 임베드면 호스트의 boot 메시지가 오리진을 들고 오므로 ready 만 알리고 기다린다.
loadSettings();
if (EMBED) tellHost({ type: "ready" });
else boot().then(paintBadges);
setInterval(loadConversations, 15000);
setInterval(loadSettings, 5 * 60 * 1000);
