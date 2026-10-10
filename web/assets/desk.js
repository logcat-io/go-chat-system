import { ChatSession, api, isSessionExpired } from "/chat.js";
import {
  $, escape, bindStatus, createLog, createTyping, autoGrow, createNotifier, dropzone, avatarHTML,
  relativeTime, STATE_LABEL, CLOSED_BY_CUSTOMER, ROLE_LABEL, withRo, toast, toastError, clearToasts, confirm, form, menu, emptyHTML, skeletonRows,
  rankSnippets, expiryLabel, formatDateTime, validateFaq, moveItem, FAQ_LIMITS, FAQ_HOME_LIMIT, faqEditorRowHTML,
  createUploadGuard, createSentDrafts, restoreDraft, clip, uploadingPillHTML, readyPillHTML, attachmentGiveBack,
  closeMenu, isMenuOpen, mergeConversation, ratingSummaryHTML, durationText, percentText, scoreText,
  METRIC_RANGES, metricsWindow, metricCards,
} from "/ui.js";

const TOKEN_KEY = "gcs.agentToken";
const FILTERS = {
  todo: { label: "답할 차례", query: "assignee=me&waiting=1", empty: ["✅", "답할 차례가 없습니다", "내 담당 중 고객이 기다리는 대화가 여기 보입니다"] },
  mine: { label: "내 담당", query: "assignee=me", empty: ["🙋", "맡은 대화가 없습니다", "미할당에서 가져오거나 배정을 받아 주세요"] },
  unassigned: { label: "미할당", query: "assignee=none", empty: ["🎉", "미할당 대화가 없습니다", "모든 문의에 담당자가 있습니다"] },
  following: { label: "팔로잉", query: "following=1", empty: ["👀", "팔로우한 대화가 없습니다", ""] },
  all: { label: "전체 대화", query: "", empty: ["💬", "아직 대화가 없습니다", "고객이 위젯에서 문의하면 여기 쌓입니다"] },
  open: { label: "진행중", query: "state=OPEN", empty: ["💬", "진행중인 대화가 없습니다", ""] },
  snoozed: { label: "보류", query: "state=SNOOZED", empty: ["⏸", "보류한 대화가 없습니다", ""] },
  closed: { label: "완료", query: "state=CLOSED", empty: ["📁", "완료한 대화가 없습니다", ""] },
  // 상담 삭제 권한이 있을 때만 보인다. 여기서 연 상담은 읽기 전용이고 되살리기만 할 수 있다.
  deleted: { label: "삭제됨", query: "deleted=1", empty: ["🗑", "삭제한 상담이 없습니다", "삭제한 상담은 여기서 보고 되살릴 수 있습니다"] },
};
const DAYS = [["mon", "월"], ["tue", "화"], ["wed", "수"], ["thu", "목"], ["fri", "금"], ["sat", "토"], ["sun", "일"]];

const ids = ["loginView", "loginForm", "email", "password", "inviteView", "inviteForm", "inviteDesc", "invitePassword", "inviteSubmit",
  "deskView", "mainNav", "settingsNavBtn", "search", "results", "status", "statusText", "meBtn", "inboxMain", "filters", "tagFilters",
  "listTitle", "convList", "listEmpty", "refresh", "thread", "backToList", "threadAvatar", "threadTitle", "threadSub", "toolbar",
  "assignBtn", "snoozeBtn", "followBtn", "closeBtn", "log", "msgs", "pending", "typing", "typingLabel", "composer", "snippetPop",
  "internalToggle", "file", "snippetBtn", "draftBtn", "composerHint", "input", "send", "profile", "profileEmpty", "profileBody", "pAvatar", "pName",
  "pMeta", "pEdit", "pEmail", "pPhone", "pExternal", "tagChips", "tagForm", "tagInput", "followerChips", "pNote", "pNoteSave",
  "brandName", "pRatingSec", "pRating", "pDangerZone", "pDelete", "deleteConvBtn", "restoreConvBtn", "deletedFilter", "searchWrap",
  "joinLink", "joinView", "joinForm", "joinDesc", "jName", "jEmail", "jPassword", "joinSubmit", "joinBackToLogin",
  "settingsMain", "settingsNav", "settingsBody", "composerTools", "navToggle", "profileToggle", "drawerScrim"];
const els = Object.fromEntries(ids.map((id) => [id, $(id)]));

const view = createLog({
  logEl: els.log, msgsEl: els.msgs, pendingEl: els.pending, mineKind: "AGENT", token: () => token,
  // 데스크는 회사 쪽(상담원·AI·자동)이 오른쪽, 고객이 왼쪽이다. 동료 상담원 줄은 이름을 보인다(정책 824·825).
  outKinds: ["AGENT", "AI", "BOT"], selfId: () => me?.agent_id,
  // ⋯ 는 열 항목이 하나라도 있을 때만 그린다. 삭제한 상담을 읽기 전용으로 볼 때는 그리지 않는다.
  canMenu: (m, mine, self) => !current?.readOnly && messageActions(m.id, { self, sender: m.sender_kind, text: "" }).length > 0,
  onMenu: (messageId, btn, opts) => {
    const items = messageActions(messageId, opts);
    if (items.length) menu(btn, items);
  },
});

const PRIOR_WINDOW = 30; // 대화를 열 때 먼저 보여 주는 이전 상담의 줄 수
const PRIOR_PAGE = 50;

// messageActions 는 이 메시지에서 열 수 있는 항목이다(서버 규칙과 같다). self 는 내가 보낸 글이고 동료 상담원 글은 아니다. 내 메시지는 수정·삭제, 메시지 관리 권한이 있으면 다른 상담원·AI·봇
// 줄도 삭제한다. 고객 메시지는 관리자도 지우지 못한다(서버 규칙) — 그래서 관리 권한의 삭제는 고객 줄에 붙지 않는다.
// 시스템 줄(배정·종료 기록)은 여기 오지 않는다 — 운영 기록이라 서버가 누구의 수정·삭제도 받지 않고(409 system_line, 감사 28) 화면은 ⋯ 를 그리지 않는다(ui.js rowHTML 의 SYSTEM 분기).
// 이력은 이력 조회 권한이 본다.
function messageActions(messageId, { self, sender, text }) {
  const convId = current?.id;
  if (!convId) return [];
  const base = `/api/conversations/${convId}/messages/${messageId}`;
  const items = [];
  if (self) {
    items.push({ label: "수정", onSelect: async () => {
      const out = await form({ title: "메시지 수정", fields: [{ name: "body", label: "본문", type: "textarea", value: text }], ok: "수정" });
      if (!out) return;
      try { await api(base, { token, method: "PATCH", body: { body: out.body } }); }
      catch (err) { toastError(err); }
    } });
  }
  if (self || (can("message.moderate") && sender !== "CUSTOMER")) {
    items.push({ label: "삭제", danger: true, onSelect: async () => {
      if (!(await confirm({ title: "이 메시지를 삭제할까요?", body: "본문과 첨부가 숨겨집니다. 원문은 이력에 남습니다.", ok: "삭제", danger: true }))) return;
      try { await api(base, { token, method: "DELETE" }); }
      catch (err) { toastError(err); }
    } });
  }
  if (can("message.revisions")) {
    items.push({ label: "수정 이력", onSelect: async () => {
      try {
        const out = await api(`/api/agent/conversations/${convId}/messages/${messageId}/revisions`, { token });
        const lines = (out.revisions || []).map((r) => `${r.reason === "DELETE" ? "삭제 전" : "수정 전"} (${relativeTime(r.created_at)}): ${r.body}`);
        await confirm({ title: "수정 이력", body: lines.join("\n\n") || "이력이 없습니다", ok: "닫기", cancel: "닫기" });
      } catch (err) { toastError(err); }
    } });
  }
  return items;
}

// loadPrior 는 이 고객의 이전 상담을 지금 대화 위에 이어 붙인다. 상담마다 대화가 따로라 서버가 고객 단위로 이어 준다.
// 대화를 열면 직전 30줄을 먼저 붙이고, 그 앞은 "이전 상담 더 보기" 로 불러온다.
async function loadPrior(limit) {
  const convId = current?.id;
  if (!convId || priorDone || priorLoading) return;
  // 대화를 바꿨다 돌아오면 같은 대화의 옛 응답이 늦게 온다. 세대 번호가 다르면 버린다.
  const gen = priorGen;
  const cursor = priorCursor ? `&before_conversation=${priorCursor.conversation}&before_seq=${priorCursor.seq}` : "";
  priorLoading = true;
  try {
    const out = await api(`/api/agent/conversations/${convId}/prior?limit=${limit}${cursor}`, { token });
    if (gen !== priorGen) return;
    const list = out.messages || [];
    if (list.length) priorCursor = { conversation: list[0].conversation_id, seq: list[0].seq };
    priorDone = list.length < limit;
    priorFailed = 0;
    view.prependPrior(list);
  } catch {
    // 이전 상담은 부가 정보다. 토스트로 알리면 상담원이 지금 대화가 실패한 것으로 읽는다(DEF-78, DB 가 끊긴 순간 대화를 열면 떴다).
    // 그 자리에 조용한 안내와 다시 시도를 둔다. 같은 크기로 다시 부른다.
    if (gen === priorGen) priorFailed = limit;
  } finally {
    if (gen === priorGen) { priorLoading = false; paintPriorButton(); }
  }
}

function paintPriorButton() {
  document.getElementById("loadPrior")?.remove();
  if (!current || priorDone) return;
  const btn = document.createElement("button");
  btn.id = "loadPrior";
  btn.className = priorFailed ? "btn ghost sm load-older prior-failed" : "btn ghost sm load-older";
  btn.textContent = priorFailed ? "이전 상담을 불러오지 못했습니다 · 다시 시도" : "이전 상담 더 보기";
  const limit = priorFailed || PRIOR_PAGE;
  btn.addEventListener("click", () => loadPrior(limit));
  els.msgs.before(btn);
}
const showTyping = createTyping(els.typing, els.typingLabel);
const setStatus = bindStatus(els.status, els.statusText);
const notifier = createNotifier();

let token = localStorage.getItem(TOKEN_KEY) || "";
let session = null;
// 로그인이 만료돼 버린 세션의 저장하지 못한 글(메모리 큐)이다(DEF-176). 저장이 막힌 브라우저에서 그 글은 메모리에만 있다 —
// 세션 객체와 같이 버리면 같은 상담원이 다시 로그인해도 나가지 않았다(감사 29 desk29 expire: 서버 0건). 상담원 id 로 묶어 들고 있다가
// 같은 상담원의 새 세션에 넘긴다. 다른 상담원이 로그인하거나 로그아웃을 누르면 버린다 — 다음 사람 이름으로 나가면 안 된다.
let heldUnsaved = null; // { owner, unsaved }
// 떠 있는 알림의 주인(마지막으로 세션을 연 상담원)이다. 로그인이 만료되면 알림을 남긴다 — 같은 상담원이 다시 들어오면 보내지 못한 답의
// 복사 단추가 아직 쓸모 있다. 다른 상담원이 들어오는 순간에는 치운다(감사 30 desktoast2: 만료 뒤 다른 상담원 화면에 앞 상담원의 답 본문 알림).
let toastOwner = "";
let me = null;
let current = null;
let priorCursor = null; // 이전 상담을 어디까지 불러왔나: { conversation, seq }. null 이면 아직 안 불렀다
let priorDone = false;
let priorLoading = false;
let priorGen = 0; // 대화를 열 때마다 오른다
let priorFailed = 0; // 직전 불러오기가 실패했으면 그 크기다. 다시 시도가 같은 크기로 부른다
let customer = null;
let conversations = [];
let agents = [];
let snippets = [];
let followers = [];
let filter = "all";
let tagFilter = "";
let internalMode = false;
let pendingAttachment = null;
// 작성칸 업로드는 시작한 대화에 묶인다(다른 대화로 옮기면 멈추고 버린다). 보낸 글은 거절되면 돌려받으려고 잠시 기억한다.
const uploads = createUploadGuard();
const drafts = createSentDrafts();
const held = new Map(); // 다른 대화에서 거절된 글. 그 대화를 다시 열면 입력칸에 돌려준다
// 쓰다 만 글은 그 대화의 초안이다. 대화를 떠나면 여기 두고 입력칸을 비운다. 그 대화를 다시 열면 돌려준다.
// 입력칸에 남겨 두면 열기가 실패한 뒤 "다시 시도" 로 연 다른 대화로 그 글이 나갈 수 있다(감사 10).
const composing = new Map();
// 열려 있는 담당 메뉴와 그 메뉴를 그릴 때 본 담당자. 그 사이 담당자가 바뀌면 메뉴를 닫는다(D39 — 보던 값으로만 바꾼다).
let assignMenu = null; // { el, convId, assignee }
let counts = {};
let knownTodo = null;

// can 은 서버가 준 권한 목록으로 버튼을 그릴지 정한다. 역할 이름으로 분기하지 않는다 — 막는 것은 서버다.
const can = (perm) => (me?.permissions || []).includes(perm);

// ── 받은함 ───────────────────────────────────────────────────────

function filterQuery() {
  const parts = [FILTERS[filter].query].filter(Boolean);
  if (tagFilter) parts.push(`tag_id=${encodeURIComponent(tagFilter)}`);
  parts.push("limit=100");
  return parts.join("&");
}

function paintCounts(next) {
  counts = next;
  for (const el of els.filters.querySelectorAll("[data-count]")) {
    const n = counts[el.dataset.count] ?? 0;
    el.textContent = n > 999 ? "999+" : String(n);
    if (el.classList.contains("badge")) el.hidden = n === 0;
  }
  document.title = counts.todo > 0 ? `(${counts.todo}) 상담원 데스크` : "상담원 데스크";
}

async function loadCounts() {
  try {
    const next = await api("/api/agent/inbox/counts", { token });
    if (knownTodo !== null && next.todo > knownTodo) {
      notifier.show("답할 차례가 늘었습니다", `내 담당 중 ${next.todo}건이 답을 기다립니다`);
    }
    knownTodo = next.todo;
    paintCounts(next);
  } catch {
    /* 다음 주기에 */
  }
}

let inboxTimer = null;
let inboxBusy = false;
// 받은함 갱신 신호는 묶는다. 신호마다 두 번씩 조회하면 서버 지연이 화면 지연으로 돌아온다.
function refreshInboxSoon() {
  if (inboxTimer) return;
  inboxTimer = setTimeout(async () => {
    inboxTimer = null;
    if (inboxBusy) return refreshInboxSoon();
    inboxBusy = true;
    try {
      await loadInbox();
    } finally {
      inboxBusy = false;
    }
  }, 700);
}

async function loadInbox() {
  try {
    const out = await api(`/api/agent/conversations?${filterQuery()}`, { token });
    conversations = out.conversations || [];
  } catch (err) {
    // 우리 서버의 판정(JSON unauthorized)일 때만 로그인을 버린다. 앞단의 401 HTML 은 일시 실패다(감사 22).
    if (isSessionExpired(err)) return logout(false);
    return;
  }
  els.listTitle.textContent = FILTERS[filter].label;
  loadCounts();
  const [glyph, title, text] = FILTERS[filter].empty;
  els.listEmpty.innerHTML = emptyHTML(glyph, title, text);
  els.listEmpty.hidden = conversations.length > 0;
  els.convList.innerHTML = conversations.map(convRowHTML).join("");
  const open = filter === "deleted" ? selectDeleted : selectConversation;
  for (const btn of els.convList.querySelectorAll(".conv-row")) {
    btn.addEventListener("click", () => open(btn.dataset.id));
  }
}

// closedByCustomerChip 은 고객이 직접 닫은 상담의 표시다(D48). 상태(완료) 옆에 붙는다. 상담원이 다시 열면 사라진다.
function closedByCustomerChip(c) {
  return c?.state === "CLOSED" && c?.closed_by === "CUSTOMER" ? ` <span class="chip" data-closed-by="customer">${CLOSED_BY_CUSTOMER}</span>` : "";
}

function convRowHTML(c) {
  const unread = c.id === current?.id ? 0 : c.unread || 0;
  const prefix = c.last_sender_kind === "AGENT" ? "나: " : c.last_sender_kind === "BOT" ? "자동: " : c.last_sender_kind === "AI" ? "AI: " : "";
  // 마지막 메시지가 글 없는 첨부면 본문이 비어 있다. 그때 "메시지 없음" 이라고 쓰면 오해한다.
  const tags = (c.tags || []).map((t) => `<span class="chip">${escape(t.name)}</span>`).join("");
  return `<button class="conv-row ${unread ? "unread" : ""}" data-id="${c.id}" aria-current="${c.id === current?.id}">
    ${avatarHTML(c.customer_name)}
    <div class="body">
      <div class="title"><span class="name">${escape(c.customer_name)}</span>
        <span class="chip ${c.state === "OPEN" ? "brand" : c.state === "SNOOZED" ? "warn" : ""}">${STATE_LABEL[c.state] || c.state}</span>${closedByCustomerChip(c)}</div>
      <div class="preview">${escape(prefix + (c.last_body || (c.last_sender_kind ? "파일을 보냈습니다" : "메시지 없음")))}</div>
      ${tags ? `<div class="tagline">${tags}</div>` : ""}
    </div>
    <div class="side">
      <span class="when">${relativeTime(c.last_message_at || c.created_at)}</span>
      ${unread ? `<span class="badge">${unread > 99 ? "99+" : unread}</span>` : ""}
      ${c.assignee_name ? `<span class="chip">${escape(c.assignee_name)}</span>` : ""}
    </div></button>`;
}

async function loadTagFilters() {
  try {
    const out = await api("/api/agent/tags", { token });
    els.tagFilters.innerHTML = (out.tags || [])
      .map((t) => `<button data-tag="${t.id}" aria-current="${tagFilter === t.id}"># ${escape(t.name)}</button>`)
      .join("") || `<div class="muted small filters-empty">태그 없음</div>`;
    for (const btn of els.tagFilters.querySelectorAll("button")) {
      btn.addEventListener("click", () => {
        tagFilter = tagFilter === btn.dataset.tag ? "" : btn.dataset.tag;
        closeDrawer();
        els.deskView.classList.add("show-list");
        loadTagFilters();
        loadInbox();
      });
    }
  } catch {
    /* 태그는 없어도 된다 */
  }
}

// ── 대화 ─────────────────────────────────────────────────────────

// 대화 열기는 성공한 응답이 와야 전환이 끝난다(감사 9, A2-0580 — 열기가 500 으로 실패하자 머리·작성칸·구독이 앞 대화에 남아
// 앞 고객에게 답이 나갈 수 있었다). 그래서
//   열기 시작하면 앞 대화에서 손을 뗀다 — 구독을 놓고 current 를 비우고 작성칸을 잠그고 머리를 "불러오는 중" 으로 둔다.
//   실패하면 앞 대화로 되돌리지 않는다. 빈 상태에 오류와 "다시 시도" 를 둔다.
//   늦게 온 응답은 버린다 — 그 사이 다른 대화를 눌렀으면 그 대화가 이긴다(openTicket).
// 어떤 경우에도 화면에 보이는 대화와 보내기 대상이 다를 수 없다. 보내기는 current 가 있을 때만 나간다.
let openTicket = 0;

function beginOpen(convId) {
  const ticket = ++openTicket;
  const moved = convId !== current?.id;
  const stopped = uploads.busy;
  closeThread();
  if (stopped) toast(moved ? "다른 대화로 옮겨 올리던 파일을 취소했습니다" : "대화를 다시 불러와 올리던 파일을 취소했습니다");
  els.threadTitle.textContent = "불러오는 중…";
  els.threadSub.textContent = "";
  els.msgs.innerHTML = skeletonRows(4);
  return ticket;
}

// failOpen 은 열기 실패를 빈 상태로 둔다. 앞 대화는 이미 놓았다. 다시 시도는 같은 대화를 처음부터 연다.
function failOpen(convId, err, retry) {
  els.msgs.innerHTML = "";
  // 목록이나 업무 기록이 가리킨 상담이 그 사이 지워졌다. 오류가 아니라 상태다.
  if (err.code === "not_found") {
    closeThread();
    els.deskView.classList.add("show-list");
    toast("이 상담은 더 이상 열 수 없습니다");
    return loadInbox();
  }
  els.threadTitle.textContent = "대화를 열지 못했습니다";
  els.threadSub.textContent = err.message;
  // 다시 시도 단추는 안내 묶음 안에 둔다(빈 상태는 가운데 정렬이라 밖에 두면 멀리 떨어진다).
  els.msgs.innerHTML = emptyHTML("⚠️", "대화를 열지 못했습니다", err.message)
    .replace(/<\/div>$/, `<div class="open-retry"><button type="button" class="btn sm" id="retryOpen">다시 시도</button></div></div>`);
  els.msgs.querySelector("#retryOpen")?.addEventListener("click", () => retry(convId));
  toastError(err);
}

async function selectConversation(convId) {
  const ticket = beginOpen(convId);
  let conv;
  try {
    conv = await api(`/api/conversations/${convId}`, { token });
  } catch (err) {
    if (ticket !== openTicket) return; // 그 사이 다른 대화를 열었다
    return failOpen(convId, err, selectConversation);
  }
  if (ticket !== openTicket) return;
  current = conv;
  view.clear();
  els.typing.hidden = true;
  els.input.disabled = false;
  els.send.disabled = false;
  els.toolbar.hidden = false;
  setReadOnlyToolbar(false);
  els.deskView.classList.remove("show-list");
  paintThreadHead();
  session.open(conv.id, { fromStart: true });
  // 이 대화의 초안만 채운다. 다른 대화에서 쓰던 글은 그 대화에 남아 있다.
  els.input.value = composing.get(conv.id) || "";
  composing.delete(conv.id);
  autoGrow(els.input);
  const back = held.get(conv.id);
  if (back !== undefined && restoreDraft(els.input, back)) {
    held.delete(conv.id);
    autoGrow(els.input);
  }
  document.getElementById("loadPrior")?.remove();
  priorCursor = null;
  priorDone = false;
  priorLoading = false;
  priorFailed = 0;
  priorGen++;
  loadPrior(PRIOR_WINDOW);
  els.pRatingSec.hidden = true;
  await Promise.all([loadCustomer(conv.customer_id), loadFollowers(), loadInbox(), loadRating()]);
  els.input.focus();
}

// setReadOnlyToolbar 는 삭제한 상담을 볼 때 되살리기 말고는 아무것도 못 누르게 한다. 막는 것은 서버다 — 가린 상담은 다른 모든 경로에서 없다.
// 작성칸의 도구(내부 메모 전환·첨부·빠른 답변·AI 초안)와 태그 추가 폼도 숨긴다. 눌러도 서버가 404 로 답할 뿐이다.
function setReadOnlyToolbar(readOnly) {
  for (const b of [els.assignBtn, els.snoozeBtn, els.followBtn, els.closeBtn]) b.hidden = readOnly;
  els.deleteConvBtn.hidden = readOnly || !can("conversation.delete");
  els.restoreConvBtn.hidden = !readOnly;
  els.composerTools.hidden = readOnly;
  els.tagForm.hidden = readOnly;
  if (readOnly) els.snippetPop.hidden = true;
}

// closeThread 는 보던 대화를 화면에서 내린다. 구독도 놓는다 — 놓지 않으면 그 대화의 "없다" 답이 오류로 뜬다.
function closeThread() {
  if (current && !current.readOnly && els.input.value.trim()) composing.set(current.id, els.input.value);
  els.input.value = "";
  closeMenu();
  assignMenu = null;
  session?.close();
  current = null;
  priorGen++;
  view.clear();
  clearAttachment();
  els.typing.hidden = true;
  els.toolbar.hidden = true;
  els.input.disabled = true;
  els.send.disabled = true;
  els.threadTitle.textContent = "대화를 선택해 주세요";
  els.threadSub.textContent = "왼쪽 목록에서 골라 주세요";
  els.threadAvatar.innerHTML = "";
  els.profileBody.hidden = true;
  els.profileEmpty.hidden = false;
  els.pRatingSec.hidden = true;
  document.getElementById("loadPrior")?.remove();
}

// selectDeleted 는 삭제한 상담을 읽기 전용으로 연다. 구독하지 않는다 — 가린 상담은 실시간 전달 대상이 아니다.
// 열기는 selectConversation 과 같은 규칙이다 — 시작하면 앞 대화를 놓고, 실패하면 빈 상태, 늦은 응답은 버린다.
async function selectDeleted(convId) {
  const ticket = beginOpen(convId);
  let out;
  try {
    out = await api(`/api/agent/conversations/${convId}/deleted?limit=500`, { token });
  } catch (err) {
    if (ticket !== openTicket) return;
    return failOpen(convId, err, selectDeleted);
  }
  if (ticket !== openTicket) return;
  // 앞서 보던 대화의 구독은 열기를 시작할 때 놓았다. 그 대화가 그 사이 지워지거나 바뀌어도 이 화면에 섞이지 않는다.
  current = { ...out.conversation, readOnly: true };
  view.clear();
  document.getElementById("loadPrior")?.remove();
  priorGen++; // 앞서 연 상담의 이전 상담 불러오기가 늦게 와도 붙지 않게 한다
  priorCursor = null;
  priorDone = true;
  priorLoading = false;
  els.typing.hidden = true;
  els.input.disabled = true;
  els.send.disabled = true;
  els.toolbar.hidden = false;
  setReadOnlyToolbar(true);
  els.deskView.classList.remove("show-list");
  els.threadAvatar.innerHTML = avatarHTML(current.customer_name);
  els.threadTitle.textContent = current.customer_name;
  const d = out.deletion || {};
  const when = d.deleted_at ? new Date(d.deleted_at).toLocaleString("ko-KR", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" }) : "";
  els.threadSub.textContent = ["삭제됨", d.deleted_by_name ? `${d.deleted_by_name} 님이 삭제` : "", when, d.reason ? `사유: ${d.reason}` : ""].filter(Boolean).join(" · ");
  for (const m of out.messages || []) view.append(m);
  paintTags();
  els.followerChips.innerHTML = `<span class="muted small">없음</span>`;
  els.pRatingSec.hidden = true; // 가린 상담의 평가는 지표에도 들지 않는다
  await Promise.all([loadCustomer(current.customer_id), loadInbox()]);
}

function paintThreadHead() {
  if (!current) return;
  els.threadAvatar.innerHTML = avatarHTML(current.customer_name);
  els.threadTitle.textContent = current.customer_name;
  const bits = [STATE_LABEL[current.state] || current.state];
  if (current.state === "CLOSED" && current.closed_by === "CUSTOMER") bits.push(CLOSED_BY_CUSTOMER);
  if (current.assignee_name) bits.push(`담당 ${current.assignee_name}`);
  if (current.snoozed_until) bits.push(`${new Date(current.snoozed_until).toLocaleString("ko-KR", { hour: "2-digit", minute: "2-digit", month: "numeric", day: "numeric" })}까지 보류`);
  els.threadSub.textContent = bits.join(" · ");
  els.closeBtn.textContent = current.state === "CLOSED" ? "다시 열기" : "완료";
  els.closeBtn.classList.toggle("primary", current.state !== "CLOSED");
  els.assignBtn.innerHTML = `${current.assignee_name ? escape(current.assignee_name) : "담당 지정"} <span class="caret">▾</span>`;
  paintTags();
}

function paintTags() {
  const removable = !current?.readOnly;
  els.tagChips.innerHTML = (current?.tags || [])
    .map((t) => `<span class="chip brand">${escape(t.name)}${removable ? `<button data-tag="${t.id}" aria-label="태그 제거">×</button>` : ""}</span>`)
    .join("") || `<span class="muted small">없음</span>`;
  for (const btn of els.tagChips.querySelectorAll("button")) {
    btn.addEventListener("click", async () => {
      const out = await api(`/api/agent/conversations/${current.id}/tags/${btn.dataset.tag}`, { token, method: "DELETE" });
      current.tags = out.tags;
      paintTags();
      loadInbox();
      loadTagFilters();
    });
  }
}

async function loadCustomer(customerID) {
  let loaded;
  try {
    loaded = await api(`/api/customers/${customerID}`, { token });
  } catch {
    return;
  }
  // 응답을 기다리는 사이 다른 대화를 열었으면 버린다. 남의 고객이 이 패널에 그려지면 안 된다.
  if (current?.customer_id !== customerID) return;
  customer = loaded;
  els.profileEmpty.hidden = true;
  els.profileBody.hidden = false;
  els.pAvatar.innerHTML = avatarHTML(customer.display_name, "lg");
  els.pName.textContent = customer.display_name;
  els.pMeta.textContent = `대화 ${customer.conversations}건 · 가입 ${formatDateTime(customer.created_at)}`;
  els.pEmail.textContent = customer.email || "";
  els.pPhone.textContent = customer.phone || "";
  els.pExternal.textContent = customer.external_id ? customer.external_id.replace(/^member:/, "회원 ") : "익명";
  els.pNote.value = customer.note || "";
  // 고객 정보는 누구나 본다. 고치기·지우기는 권한이 있을 때만 그린다.
  const editable = can("customer.edit");
  els.pEdit.hidden = !editable;
  els.pNote.readOnly = !editable;
  els.pNoteSave.hidden = !editable;
  els.pDangerZone.hidden = !can("customer.delete");
}

async function loadFollowers() {
  const convId = current?.id;
  if (!convId) return;
  try {
    const out = await api(`/api/agent/conversations/${convId}/followers`, { token });
    if (current?.id !== convId) return;
    followers = out.followers || [];
  } catch {
    return;
  }
  const following = followers.some((a) => a.id === me?.agent_id);
  els.followBtn.textContent = following ? "팔로우 해제" : "팔로우";
  els.followerChips.innerHTML =
    (current.assignee_name ? `<span class="chip brand">담당 ${escape(current.assignee_name)}</span>` : "") +
      followers.map((a) => `<span class="chip">${escape(a.name)}</span>`).join("") || `<span class="muted small">없음</span>`;
}

// loadRating 은 대화 옆 칸의 만족도를 다시 읽는다(D45). 평가가 없고 종료도 아니면 칸을 숨긴다.
// 고객이 평가를 내도 실시간 알림은 없다 — 대화를 열거나 상태가 바뀔 때 읽는다.
async function loadRating() {
  const convId = current?.id;
  if (!convId || current.readOnly) return;
  let out;
  try {
    out = await api(`/api/conversations/${convId}/rating`, { token });
  } catch {
    return;
  }
  if (current?.id !== convId) return;
  const html = ratingSummaryHTML(out);
  els.pRating.innerHTML = html;
  els.pRatingSec.hidden = !html;
}

// assign 은 담당자를 바꾼다. expected 는 사용자가 메뉴에서 본 담당자다(메뉴를 그릴 때 붙잡은 값, 빈 문자열 = 미배정).
// 누른 순간의 current 를 쓰면 메뉴가 열린 동안 실시간 프레임이 current 를 이긴 사람으로 바꿔, 늦게 누른 쪽이 이긴 사람 id 를
// 조건으로 보내 정상 이관이 된다(감사 10 — 동시 "나에게" 가 둘 다 성공). convId 는 그 메뉴의 대화다.
async function assign(agentID, expected, convId) {
  if (!current || current.id !== convId || current.readOnly) return;
  let updated;
  try {
    updated = await api(`/api/agent/conversations/${convId}/assign`, { token, method: "POST", body: { agent_id: agentID, expected_assignee: expected } });
  } catch (err) {
    // 409 는 그 사이 상태가 바뀌었다는 답이다(다른 상담원이 먼저 가져갔다 등). 까닭을 보이고 목록과 머리를 서버 상태로 다시 읽는다.
    // 403 도 같다 — 응대 권한만 있으면 남이 맡은 상담은 바꿀 수 없어서, 화면이 본 뒤 남이 가져갔으면 403 이 온다.
    if (err.status === 409 || err.status === 403) {
      toastError(err);
      loadInbox();
      return refreshCurrent(convId);
    }
    return toastError(err);
  }
  // 늦게 온 응답은 지금 보는 대화가 아니면 버린다. 안 그러면 헤더는 A, 작성칸은 B 가 된다.
  if (current?.id !== convId) return;
  current = updated;
  paintThreadHead();
  loadInbox();
  loadFollowers();
  toast(agentID ? "담당자를 지정했습니다" : "담당을 해제했습니다", "ok");
}

// refreshCurrent 는 보고 있는 대화의 머리를 서버 상태로 다시 그린다. 구독·화면은 그대로다. 그 사이 다른 대화를 열었으면 버린다.
async function refreshCurrent(convId) {
  let conv;
  try {
    conv = await api(`/api/conversations/${convId}`, { token });
  } catch {
    return;
  }
  if (current?.id !== convId || current?.readOnly) return;
  adoptConversation(conv);
  loadFollowers();
}

// adoptConversation 은 서버가 알려 준 대화 상태(응답·실시간 프레임)를 화면에 들인다. 빈 값을 싣지 않는 필드는 빈 값으로 본다
// (mergeConversation). 담당 메뉴가 열려 있는데 그 사이 담당자가 바뀌었으면 메뉴를 닫는다 — 낡은 메뉴를 누를 수 없게.
function adoptConversation(next) {
  const before = current?.state;
  current = mergeConversation(current, next);
  paintThreadHead();
  if (current.state !== before) loadRating(); // 종료·다시 열기 — 평가 칸을 지금 상태로(D45)
  if (assignMenu && isMenuOpen(assignMenu.el) && assignMenu.convId === current.id && (current.assignee_agent_id || "") !== assignMenu.assignee) {
    closeMenu();
    assignMenu = null;
    toast("그 사이 담당자가 바뀌어 메뉴를 닫았습니다");
  }
}

async function setState(state, snoozeMinutes) {
  if (!current) return;
  const convId = current.id;
  let updated;
  try {
    updated = await api(`/api/agent/conversations/${convId}/state`, {
      token, method: "POST", body: { state, snooze_minutes: snoozeMinutes || 0 },
    });
  } catch (err) {
    return toastError(err);
  }
  if (current?.id !== convId) return;
  current = updated;
  paintThreadHead();
  loadInbox();
}

// ── 작성 ─────────────────────────────────────────────────────────

function setInternal(on) {
  internalMode = on;
  els.composer.classList.toggle("internal", on);
  els.internalToggle.classList.toggle("on", on);
  els.composerHint.textContent = on ? "내부 메모 · 고객에게 보이지 않습니다" : "고객에게 보내는 메시지";
  els.input.placeholder = on ? "팀에게 남길 메모" : "메시지를 입력해 주세요. / 로 빠른 답변";
}

// clearAttachment 는 작성칸의 첨부를 비운다. 올리는 중이면 멈춘다. 멈춘 업로드가 있었으면 true.
function clearAttachment() {
  const stopped = uploads.cancel();
  resetAttachRow();
  return stopped;
}

function resetAttachRow() {
  pendingAttachment = null;
  els.file.value = "";
  els.composerHint.innerHTML = "";
  setInternal(internalMode);
}

// showReady 는 보낼 준비가 된 첨부를 작성칸에 붙인다. 방금 올린 것과 거절로 돌려받은 것(giveBack) 둘 다 여기로 온다.
function showReady(att) {
  pendingAttachment = att;
  els.composerHint.innerHTML = readyPillHTML(att);
  $("dropAttach").addEventListener("click", clearAttachment);
}

// 업로드는 시작한 대화(convId)에 묶인다. 끝났을 때 지금 대화가 다르거나 취소했으면 결과를 버린다.
// 남의 대화 첨부를 이 대화로 보내면 서버가 거절하고, 같이 쓴 답까지 사라진다.
async function attachFiles(files) {
  const file = files[0];
  if (!file || !current || current.readOnly) return;
  const convId = current.id;
  const ticket = uploads.begin(convId);
  pendingAttachment = null;
  els.composerHint.innerHTML = uploadingPillHTML(file.name);
  const progress = els.composerHint.querySelector(".upload-progress");
  $("cancelUpload").addEventListener("click", () => {
    if (uploads.owns(ticket, current?.id)) clearAttachment();
  });
  try {
    const att = await session.uploadAttachment(convId, file, {
      signal: ticket.signal,
      // 영상(최대 100MB)은 오래 걸린다. 얼마나 남았는지 보여 준다. 다 올린 뒤에는 서버가 확인하는 동안 "확인 중" 이다.
      onProgress: (loaded, total) => {
        if (!uploads.owns(ticket, current?.id)) return;
        progress.textContent = loaded >= total ? "확인 중" : `${Math.floor((loaded / total) * 100)}%`;
      },
    });
    if (!uploads.owns(ticket, current?.id)) return;
    uploads.finish(ticket);
    showReady(att);
  } catch (err) {
    // 취소했거나 다른 대화로 옮겨 멈춘 업로드다. 지금 작성칸과 상관없으니 알리지 않는다.
    if (!uploads.owns(ticket, current?.id)) return;
    uploads.finish(ticket);
    resetAttachRow();
    toastError(err);
  }
}

// giveBack 은 서버가 영구 거절한 메시지의 글을 돌려준다. 거절 까닭(서버 문구)은 언제나 보인다.
//   보던 대화의 것이고 입력칸이 비어 있으면 입력칸에 넣는다. 새 글이 있으면 덮지 않고 앞부분과 복사 단추를 보인다.
//   다른 대화를 보고 있으면 그 대화를 다시 열 때 넣는다 — 지금 작성칸에 넣으면 A 고객에게 쓴 답이 B 고객에게 나간다.
//   연 대화가 없으면(새로고침 직후) 그 대화를 열 때 넣고, 지금은 앞부분과 복사 단추를 남긴다.
//   첨부가 실려 있었으면(attachmentGiveBack) 아직 보내지지 않은 업로드는 같은 대화의 비어 있는 작성칸에 다시 붙이고,
//   그럴 수 없으면 파일 이름을 들어 다시 올려 달라고 알린다 — 글만 돌려주고 파일을 조용히 잃지 않는다.
function giveBack(back, reason, traceId) {
  const shown = current && !current.readOnly ? current.id : null;
  const file = attachmentGiveBack(back, { shown, slotFree: !pendingAttachment && !uploads.busy });
  if (file === "reattach") showReady(back.attachment);
  const why = file === "lost"
    ? `${reason} · 파일 ${back.attachment.filename || ""} 은 보내지 못했습니다. 다시 올려 주세요`
    : file === "reattach" ? `${reason} · 파일을 작성칸에 다시 붙였습니다` : reason;
  if (!back.body) return toast(why, "danger", { traceId });
  const unsent = () => toast(`${why} · 보내지 못한 글: ${clip(back.body)}`, "danger", { copy: back.body, traceId });
  if (back.convId === current?.id && !current?.readOnly) {
    if (restoreDraft(els.input, back.body)) {
      autoGrow(els.input);
      return toast(`${why} · 쓴 글을 입력칸에 되돌렸습니다`, "danger", { traceId });
    }
    return unsent();
  }
  if (held.has(back.convId)) return unsent();
  held.set(back.convId, back.body);
  if (!current) return unsent();
  toast(`${why} · 쓴 글은 그 대화를 다시 열면 입력칸에 돌아갑니다`, "danger", { copy: back.body, traceId });
}

function onSessionError(message, clientMsgId, dropped, traceId) {
  const isQueued = (convId, id) => !!session && session.stateOf(convId).pending.some((p) => p.client_msg_id === id);
  const out = drafts.settle(clientMsgId, dropped, isQueued, session?.convId);
  if (out.quiet) return;
  if (out.back) giveBack(out.back, message, traceId);
  else toast(message, "danger", { traceId });
}

function paintSnippets(query) {
  const list = rankSnippets(snippets, query);
  els.snippetPop.hidden = list.length === 0;
  els.snippetPop.innerHTML = list
    .map((s, i) => `<button type="button" data-id="${s.id}" class="${i === 0 ? "active" : ""}">
      <span class="sc">/${escape(s.shortcut)}</span> <span class="tt">${escape(s.title)}</span><div class="bd">${escape(s.body)}</div></button>`)
    .join("");
  for (const btn of els.snippetPop.querySelectorAll("button")) btn.addEventListener("click", () => applySnippet(btn.dataset.id));
}

function applySnippet(snippetID) {
  const sn = snippets.find((s) => s.id === snippetID);
  if (!sn) return;
  els.input.value = sn.body;
  els.snippetPop.hidden = true;
  autoGrow(els.input);
  els.input.focus();
}

// ── 세션 ─────────────────────────────────────────────────────────

function startSession() {
  const owner = `agent:${me?.agent_id || ""}`;
  const unsaved = heldUnsaved?.owner === owner ? heldUnsaved.unsaved : null;
  heldUnsaved = null; // 다른 상담원이면 여기서 버린다
  if (toastOwner && toastOwner !== owner) clearToasts(); // 앞 상담원의 알림을 다음 사람에게 남기지 않는다
  toastOwner = owner;
  session = new ChatSession({
    owner,
    unsaved,
    token,
    handlers: {
      onMessage: (m) => {
        // 삭제한 상담을 읽기 전용으로 보는 중이면 앞서 구독한 대화의 프레임을 그 화면에 섞지 않는다.
        if (current?.readOnly) return;
        view.append(m);
        // 수정·삭제 이벤트는 새 메시지가 아니다. 알림을 울리지 않는다.
        if (m.kind === "MESSAGE" && m.sender_kind === "CUSTOMER") notifier.show(`${m.sender_name} 님의 메시지`, m.body || "파일을 보냈습니다");
      },
      onPending: (list) => {
        drafts.rememberAll(session?.convId, list);
        view.setPending(list);
      },
      onStatus: setStatus,
      // 보낸 답이 오래 ack 를 못 받고 있다. 큐에 남아 연결되면 같은 답이 한 번만 나간다 — 포기하지 않는다(D5).
      onStalled: (s) => view.setPendingNotice(!s ? "" : s.offline
        ? "연결이 끊겨 있습니다. 다시 연결되면 보냅니다"
        : "메시지 전송이 지연되고 있습니다. 자동으로 다시 보냅니다"),
      onReady: (f) => {
        const mine = (f.reads || []).find((r) => r.reader_id === me?.agent_id);
        view.markUnreadFrom(mine?.last_read_seq || 0);
      },
      onConversation: (c) => {
        if (!c || c.id !== current?.id || current?.readOnly) return;
        adoptConversation(c);
        loadFollowers();
        refreshInboxSoon();
      },
      onReads: (reads) => !current?.readOnly && reads.forEach((r) => view.setReadFrom(r.reader_kind, r.last_read_seq)),
      onRead: (f) => !current?.readOnly && view.setReadFrom(f.reader_kind, f.seq),
      onTyping: (f) => !current?.readOnly && showTyping(f.sender_name || "고객"),
      onInboxChanged: refreshInboxSoon,
      // 메시지 하나가 영구 거절되면(client_msg_id 가 온다) chat.js 는 큐에서 지운다. 쓴 답은 화면에서 사라지면 안 되니 돌려준다.
      onError: onSessionError,
      // 소켓 티켓을 받으려다 401 이다(로그아웃·비활성·유휴 만료). REST 의 401 과 같이 로그인 화면으로 간다.
      onUnauthorized: () => logout(false),
      // 보던 대화가 사라졌다(다른 사람이 삭제했다). 실패가 아니라 상태라 중립 안내를 한 번 띄우고 목록으로 돌아간다.
      onGone: (convId) => {
        if (current?.id !== convId || current?.readOnly) return;
        closeThread();
        els.deskView.classList.add("show-list");
        toast("이 상담은 더 이상 열 수 없습니다");
        loadInbox();
      },
    },
  });
  session.start();
}

// logout 은 로그인 화면으로 간다. callServer 는 상담원이 로그아웃을 누른 것이고, 아니면 세션이 만료된 것이다(401).
// 만료면 저장하지 못한 글을 상담원 id 로 묶어 남긴다(같은 상담원이 다시 로그인하면 나간다). 로그아웃이면 버린다(DEF-176).
async function logout(callServer = true) {
  // 만료가 두 길(소켓 티켓·REST 401)로 겹쳐 와도 처음 남긴 것을 지키지 않는다 — 두 번째에는 세션이 없다.
  if (callServer) heldUnsaved = null;
  else if (session) heldUnsaved = { owner: `agent:${me?.agent_id || ""}`, unsaved: session.unsaved };
  if (callServer && token) {
    try {
      await api("/api/logout", { token, method: "POST" });
    } catch {
      /* 이미 끊긴 세션일 수 있다 */
    }
  }
  localStorage.removeItem(TOKEN_KEY);
  token = "";
  if (callServer) {
    session?.discard();
    clearToasts(); // 앞 상담원의 알림(보내지 못한 글의 복사 단추 등)을 다음 사람에게 남기지 않는다
  } else session?.stop();
  session = null;
  current = null;
  me = null;
  els.deskView.hidden = true;
  els.loginView.hidden = false;
}

async function enterDesk() {
  els.loginView.hidden = true;
  els.inviteView.hidden = true;
  els.deskView.hidden = false;
  // 좁은 화면(900px 이하)은 한 칸씩 보인다. 처음에는 받은함 목록이다 — 대화를 고르면 대화 칸으로 넘어가고 ‹ 로 돌아온다(정책 826).
  els.deskView.classList.add("show-list");
  me = await api("/api/me", { token });
  paintMyName();
  els.brandName.textContent = me.workspace?.name ? `· ${me.workspace.name}` : "";
  els.settingsNavBtn.hidden = false;
  // 팀·워크스페이스·빠른 답변 관리가 하나도 없으면 안에는 업무 기록과 내 계정뿐이다. 그때는 "설정" 이 아니라 "내 정보" 다.
  els.settingsNavBtn.textContent = can("team.manage") || can("workspace.manage") || can("library.manage") ? "설정" : "내 정보";
  els.draftBtn.hidden = !me.draft_model;
  els.searchWrap.hidden = !can("search.all");
  els.deleteConvBtn.hidden = !can("conversation.delete");
  els.deletedFilter.hidden = !can("conversation.delete");
  for (const b of els.settingsNav.querySelectorAll("button[data-perm]")) b.hidden = !can(b.dataset.perm);
  currentSettingsPage = [...els.settingsNav.querySelectorAll("button")].find((b) => !b.hidden)?.dataset.page || "account";
  notifier.ask();
  startSession();
  const [agentsOut] = await Promise.all([api("/api/agent/agents", { token }), reloadSnippets()]);
  agents = agentsOut.agents || [];
  els.profileEmpty.innerHTML = emptyHTML("👤", "대화를 선택하면 고객 정보가 보입니다", "");
  els.convList.innerHTML = skeletonRows(6);
  await Promise.all([loadInbox(), loadTagFilters()]);
  setInternal(false);
  setInterval(loadCounts, 8000);
}

// paintMyName 은 위쪽 내 이름 버튼을 지금 이름으로 다시 그린다. 이름을 바꾼 화면이 새로고침 없이 부른다.
function paintMyName() {
  els.meBtn.innerHTML = `<span class="me-name">${escape(me?.name || "상담원")}</span> <span class="caret">▾</span>`;
}

// reloadSnippets 는 작성칸의 "/" 목록이 쓰는 빠른 답변을 서버 순서로 다시 받는다. 관리 화면이 고칠 때마다 부른다.
async function reloadSnippets() {
  const out = await api("/api/agent/snippets", { token });
  snippets = out.snippets || [];
  return snippets;
}

function showMain(name) {
  closeDrawer({ restoreFocus: false });
  els.inboxMain.hidden = name !== "inbox";
  els.settingsMain.hidden = name !== "settings";
  for (const b of els.mainNav.querySelectorAll("button")) b.setAttribute("aria-current", String(b.dataset.main === name));
  if (name === "settings") renderSettings(currentSettingsPage);
}

// ── 설정 ─────────────────────────────────────────────────────────

let currentSettingsPage = "team";
let settingsCache = null;

async function renderSettings(page) {
  currentSettingsPage = page;
  for (const b of els.settingsNav.querySelectorAll("button")) b.setAttribute("aria-current", String(b.dataset.page === page));
  els.settingsBody.innerHTML = `<div class="skel-row"><div class="lines">${'<div class="skeleton half"></div>'.repeat(3)}</div></div>`;
  try {
    if (page === "account") return renderAccount();
    // 페이지마다 필요한 권한은 메뉴 버튼의 data-perm 에 있다. 숨긴 메뉴를 주소로 불러도 여기서 한 번 더 막는다.
    const perm = els.settingsNav.querySelector(`[data-page="${page}"]`)?.dataset.perm;
    if (perm && !can(perm)) {
      els.settingsBody.innerHTML = emptyHTML("🔒", "권한이 없습니다", "이 설정은 권한이 있는 사람이 관리합니다");
      return;
    }
    if (page === "metrics") return await renderMetrics();
    if (page === "work") return await renderWork(); // 전체 조회 권한이 없으면 서버가 자기 기록만 준다
    if (page === "team") return await renderTeam();
    // 빠른 답변 관리(library.manage)는 운영 설정 권한이 없는 직원도 연다. 운영 설정을 먼저 받으면 403 이 난다.
    if (page === "snippets") return await renderSnippets();
    if (page === "faq") return await renderFaq();
    settingsCache = await api("/api/admin/settings", { token });
    if (page === "hours") return renderHours(settingsCache);
    if (page === "messages") return renderMessages(settingsCache);
    if (page === "widget") return await renderWidget(settingsCache);
    if (page === "workspace") return await renderWorkspace();
    if (page === "audit") return await renderAudit();
  } catch (err) {
    els.settingsBody.innerHTML = emptyHTML("⚠️", "불러오지 못했습니다", err.message);
  }
}

async function saveSettings(patch) {
  try {
    settingsCache = await api("/api/admin/settings", { token, method: "PUT", body: { ...settingsCache, ...patch } });
    toast("저장했습니다", "ok");
  } catch (err) {
    toastError(err);
  }
}

const UNVERIFIED_TIP = "가입 입구는 이메일 소유를 확인하지 않습니다. 본인이 맞는지 확인하고 승인해 주세요";

// roleClass 는 역할 칩의 CSS 클래스다(role-admin·role-staff·role-part_time). 색은 CSS 가 정하고 화면 코드는 역할 이름으로 가르지 않는다.
const roleClass = (role) => `role-${String(role || "").toLowerCase().replace(/[^a-z_]/g, "")}`;

// ROLE_OPTIONS 는 역할 셀렉트 세 칸이다. 초대·승인·역할 변경이 같은 목록을 쓴다.
const ROLE_OPTIONS = [
  { value: "PART_TIME", label: "아르바이트 — 응대만" },
  { value: "STAFF", label: "직원 — 배정·검색·고객 수정·빠른 답변 관리" },
  { value: "ADMIN", label: "관리자 — 팀·설정·삭제까지" },
];

async function renderTeam() {
  const out = await api("/api/admin/team", { token });
  const pending = out.pending || [];
  const rows = out.members
    .map((m) => `<tr data-id="${m.id}">
      <td>${avatarHTML(m.name, "sm")}</td>
      <td><strong>${escape(m.name)}</strong><div class="muted small">${escape(m.email)}</div></td>
      <td><span class="chip ${roleClass(m.role)}">${ROLE_LABEL[m.role] || escape(m.role)}</span></td>
      <td>${m.active ? '<span class="chip ok">활성</span>' : '<span class="chip">비활성</span>'}</td>
      <td class="actions"><div class="menu-wrap"><button class="btn ghost sm" data-member="${m.id}">⋯</button></div></td></tr>`)
    .join("");
  const invites = out.invites
    .map((i) => `<tr><td></td><td><strong>${escape(i.name)}</strong><div class="muted small">${escape(i.email)}</div></td>
      <td><span class="chip">${ROLE_LABEL[i.role] || escape(i.role)}</span></td><td><span class="chip warn">초대 대기 · ${escape(expiryLabel(i.expires_at))}</span></td>
      <td class="actions"><button class="btn ghost sm danger" data-revoke="${escape(i.email)}">취소</button></td></tr>`)
    .join("");
  // 직접 가입은 이메일을 보내 확인하지 않는다. 남의 이메일로도 신청할 수 있으니 승인하는 사람이 본인인지 확인한다.
  const pendingRows = pending
    .map((p) => `<tr><td>${avatarHTML(p.name, "sm")}</td><td><strong>${escape(p.name)}</strong>
        <span class="chip warn" title="${UNVERIFIED_TIP}">이메일 미확인</span><div class="muted small">${escape(p.email)}</div></td>
      <td class="muted small">${escape(relativeTime(p.created_at))} 신청</td>
      <td class="actions"><button class="btn primary sm" data-approve="${p.id}">승인</button>
        <button class="btn ghost sm danger" data-reject="${p.id}">거절</button></td></tr>`)
    .join("");
  // 가입 입구를 여닫는 설정은 워크스페이스 설정이다. 그 권한이 없으면 상태만 보이고 바꾸는 칸은 그리지 않는다.
  const signupSettings = can("workspace.manage") ? await api("/api/admin/settings", { token }) : null;
  const signupOpen = signupSettings ? signupSettings.agent_signup !== false : null;
  els.settingsBody.innerHTML = `<section>
    <h2>팀</h2><p class="lead">상담원을 초대하거나 직접 가입한 사람을 승인하고, 역할을 정합니다. 초대 링크로 들어온 사람은 바로 활성 상태가 됩니다.</p>
    <div class="block"><div class="block-head"><h3>승인 대기 ${pending.length}명</h3></div>
      ${pendingRows ? `<table class="table"><tbody>${pendingRows}</tbody></table>
        <p class="muted small mt-3">직접 가입은 이메일 소유를 확인하지 않습니다. 본인이 맞는지 확인하고 승인해 주세요.</p>` : `<p class="muted small">가입을 신청한 사람이 없습니다.</p>`}
      <p class="muted small mt-3">데스크 로그인 화면의 "상담원 가입" 으로 신청하면 여기에 표시됩니다.</p>
      ${signupSettings ? `<div class="block-foot"><label for="signupOpen" class="muted small">직접 가입</label>
        <select id="signupOpen" class="input max-w-sm"><option value="true" ${signupOpen ? "selected" : ""}>받습니다</option><option value="false" ${signupOpen ? "" : "selected"}>받지 않습니다</option></select>
        <button class="btn sm" id="signupSave">저장</button></div>` : ""}</div>
    <div class="block"><div class="block-head"><h3>구성원 ${out.members.length}명</h3><span class="spacer"></span>
      <button class="btn primary sm" id="inviteBtn">+ 초대</button></div>
      <table class="table"><thead><tr><th></th><th>이름</th><th>역할</th><th>상태</th><th></th></tr></thead><tbody>${rows}${invites}</tbody></table></div></section>`;

  $("signupSave")?.addEventListener("click", async () => {
    try {
      const st = await api("/api/admin/settings", { token, method: "PUT", body: { ...signupSettings, agent_signup: $("signupOpen").value === "true" } });
      toast(st.agent_signup === false ? "직접 가입을 닫았습니다" : "직접 가입을 열었습니다", "ok");
    } catch (err) {
      toastError(err);
    }
  });
  for (const btn of els.settingsBody.querySelectorAll("[data-approve]")) {
    const who = pending.find((p) => p.id === btn.dataset.approve);
    btn.addEventListener("click", async () => {
      const data = await form({
        title: `${who.name} 님을 승인할까요?`, body: `${who.email} · 이메일 미확인. 본인이 맞는지 확인하고 승인해 주세요. 승인하면 바로 로그인할 수 있습니다.`,
        fields: [{ name: "role", label: "역할", type: "select", value: "PART_TIME", options: ROLE_OPTIONS }], ok: "승인",
      });
      if (!data) return;
      try {
        await api(`/api/admin/agents/${who.id}/approve`, { token, method: "POST", body: { role: data.role } });
        toast("승인했습니다", "ok");
        renderTeam();
      } catch (err) {
        toastError(err);
      }
    });
  }
  for (const btn of els.settingsBody.querySelectorAll("[data-reject]")) {
    const who = pending.find((p) => p.id === btn.dataset.reject);
    btn.addEventListener("click", async () => {
      if (!(await confirm({ title: `${who.name} 님의 가입을 거절할까요?`, body: `${who.email} · 신청이 지워집니다. 같은 이메일로 다시 신청할 수 있습니다.`, ok: "거절", danger: true }))) return;
      try {
        await api(`/api/admin/agents/${who.id}/reject`, { token, method: "POST" });
        toast("거절했습니다", "ok");
        renderTeam();
      } catch (err) {
        toastError(err);
      }
    });
  }

  $("inviteBtn").addEventListener("click", async () => {
    const data = await form({
      title: "상담원 초대", body: "초대 링크는 72시간 동안 유효합니다.",
      fields: [
        { name: "name", label: "이름", required: true },
        { name: "email", label: "이메일", type: "email", required: true },
        { name: "role", label: "역할", type: "select", value: "STAFF", options: ROLE_OPTIONS },
      ], ok: "초대 링크 만들기",
    });
    if (!data) return;
    try {
      const res = await api("/api/admin/invites", { token, method: "POST", body: data });
      await navigator.clipboard?.writeText(res.accept_url).catch(() => {});
      await confirm({ title: "초대 링크를 만들었습니다", body: `복사되었습니다. 상대에게 전달해 주세요:\n${res.accept_url}`, ok: "확인", cancel: "닫기" });
      renderTeam();
    } catch (err) {
      toastError(err);
    }
  });
  for (const btn of els.settingsBody.querySelectorAll("[data-revoke]")) {
    btn.addEventListener("click", async () => {
      if (!(await confirm({ title: "초대를 취소할까요?", body: btn.dataset.revoke, ok: "취소하기", danger: true }))) return;
      await api(`/api/admin/invites/${encodeURIComponent(btn.dataset.revoke)}`, { token, method: "DELETE" });
      renderTeam();
    });
  }
  for (const btn of els.settingsBody.querySelectorAll("[data-member]")) {
    const member = out.members.find((m) => m.id === btn.dataset.member);
    btn.addEventListener("click", (e) => {
      e.stopPropagation();
      menu(btn, [
        { header: member.name },
        ...ROLE_OPTIONS.map((o) => ({
          label: withRo(ROLE_LABEL[o.value]), checked: member.role === o.value, onSelect: () => setMember(member, { role: o.value }),
        })),
        { divider: true },
        { label: "이름 바꾸기", onSelect: () => renameMember(member) },
        member.active
          ? { label: "비활성화 (로그인 차단)", danger: true, onSelect: () => setMember(member, { active: false }) }
          : { label: "다시 활성화", onSelect: () => setMember(member, { active: true }) },
      ]);
    });
  }
}

// renameMember 는 관리자가 다른 상담원의 이름을 바꾼다(team.manage). 자기 줄이면 위쪽 내 이름도 바로 바꾼다.
// 다른 상담원의 화면에는 그 사람이 새로고침하거나 다시 로그인할 때 반영된다. 이름을 실시간으로 밀어 주지 않는다.
async function renameMember(member) {
  const data = await form({
    title: "이름 바꾸기", body: `${member.email} · 이미 보낸 메시지에도 새 이름이 보입니다.`,
    fields: [{ name: "name", label: "이름", value: member.name, required: true }], ok: "바꾸기",
  });
  if (!data) return;
  try {
    const out = await api(`/api/admin/agents/${member.id}/name`, { token, method: "POST", body: { name: data.name } });
    if (out.id === me?.agent_id) {
      me.name = out.name;
      paintMyName();
    }
    toast("이름을 바꿨습니다", "ok");
    renderTeam();
  } catch (err) {
    toastError(err);
  }
}

async function setMember(member, change) {
  try {
    if ("role" in change) await api(`/api/admin/agents/${member.id}/role`, { token, method: "POST", body: { role: change.role } });
    if ("active" in change) {
      if (!change.active && !(await confirm({ title: `${member.name} 님을 비활성화할까요?`, body: "로그인이 막히고 열린 세션이 끊깁니다. 대화 기록은 남습니다.", ok: "비활성화", danger: true }))) return;
      await api(`/api/admin/agents/${member.id}/active`, { token, method: "POST", body: { active: change.active } });
    }
    toast("반영했습니다", "ok");
    renderTeam();
  } catch (err) {
    toastError(err);
  }
}

function renderHours(st) {
  const h = st.business_hours;
  const dayRows = DAYS.map(([key, label]) => {
    const ranges = h.days[key] || [];
    return `<div class="hours-day" data-day="${key}">
      <strong>${label}</strong>
      <div class="ranges">${ranges.length === 0 ? '<span class="muted small">쉬는 날</span>' : ranges
        .map((r, i) => `<span class="range"><input type="time" value="${escape(r.from)}" data-i="${i}" data-k="from"> ~ <input type="time" value="${escape(r.to)}" data-i="${i}" data-k="to">
          <button type="button" class="icon-btn" data-remove="${i}" aria-label="구간 삭제">×</button></span>`).join("")}</div>
      <button type="button" class="btn ghost sm" data-add>+ 구간</button></div>`;
  }).join("");
  els.settingsBody.innerHTML = `<section>
    <h2>운영시간</h2><p class="lead">운영시간 밖에 고객이 문의하면 부재중 안내가 자동으로 나갑니다. 구간이 하나도 없으면 항상 운영 중으로 봅니다.</p>
    <div class="block"><div class="field"><label>시간대</label><input class="input max-w-md" id="tz" value="${escape(h.timezone)}"><div class="hint">예: Asia/Seoul</div></div>
      ${dayRows}
      <div class="block-foot"><span class="spacer"></span><button class="btn" id="hoursPreset">평일 9~18시로 채우기</button><button class="btn primary" id="hoursSave">저장</button></div></div></section>`;

  const read = () => {
    const days = {};
    for (const row of els.settingsBody.querySelectorAll(".hours-day")) {
      const list = [];
      for (const span of row.querySelectorAll(".range")) {
        list.push({ from: span.querySelector('[data-k="from"]').value, to: span.querySelector('[data-k="to"]').value });
      }
      days[row.dataset.day] = list;
    }
    return { timezone: $("tz").value.trim(), days };
  };
  const rerender = (hours) => renderHours({ ...st, business_hours: hours });
  for (const row of els.settingsBody.querySelectorAll(".hours-day")) {
    row.querySelector("[data-add]").addEventListener("click", () => {
      const hours = read();
      (hours.days[row.dataset.day] ||= []).push({ from: "09:00", to: "18:00" });
      rerender(hours);
    });
    for (const btn of row.querySelectorAll("[data-remove]")) {
      btn.addEventListener("click", () => {
        const hours = read();
        hours.days[row.dataset.day].splice(Number(btn.dataset.remove), 1);
        rerender(hours);
      });
    }
  }
  $("hoursPreset").addEventListener("click", () => {
    const hours = read();
    for (const [key] of DAYS.slice(0, 5)) hours.days[key] = [{ from: "09:00", to: "18:00" }];
    hours.days.sat = [];
    hours.days.sun = [];
    rerender(hours);
  });
  $("hoursSave").addEventListener("click", () => saveSettings({ business_hours: read() }));
}

function renderMessages(st) {
  const m = st.auto_messages;
  els.settingsBody.innerHTML = `<section>
    <h2>자동 메시지</h2><p class="lead">사람 대신 나가는 문장입니다. 비워 두면 그 메시지는 나가지 않습니다.</p>
    <div class="block">
      <div class="field"><label for="mWelcome">환영 메시지</label><textarea id="mWelcome">${escape(m.welcome)}</textarea><div class="hint">고객이 새 문의를 시작하면 첫 줄로 보입니다.</div></div>
      <div class="field"><label for="mFirst">접수 안내</label><textarea id="mFirst">${escape(m.first_reply)}</textarea><div class="hint">고객이 처음 말했고 아직 아무도 답하지 않았을 때 한 번 나갑니다.</div></div>
      <div class="field"><label for="mAway">부재중 안내</label><textarea id="mAway">${escape(m.away)}</textarea><div class="hint">운영시간 밖이면 접수 안내 대신 이 문장이 나갑니다.</div></div>
      <div class="field"><label for="mDraft">AI 초안 지시 (선택)</label><input id="mDraft" class="input" value="${escape(m.draft_style || "")}" placeholder="예: 존댓말, 세 문장 이내, 환불 약속 금지"><div class="hint">CHAT_OLLAMA_URL 이 설정되어 있을 때 초안을 만들 때 모델에 덧붙이는 한 줄입니다.</div></div>
      <div class="row-flex"><span class="spacer"></span><button class="btn primary" id="msgSave">저장</button></div></div></section>`;
  $("msgSave").addEventListener("click", () =>
    saveSettings({ auto_messages: { welcome: $("mWelcome").value, first_reply: $("mFirst").value, away: $("mAway").value, draft_style: $("mDraft").value } }));
}

async function renderWidget(st) {
  const w = st.widget;
  const ws = await api("/api/admin/workspace", { token });
  // 고객 사이트가 부를 주소는 서버가 아는 공개 주소(CHAT_PUBLIC_URL)다. 데스크를 연 주소(사내 주소·localhost)가 아니다.
  const origin = ws.public_url || location.origin;
  // 설치형 단일 워크스페이스라 위젯 키가 없다(D35). 옛 설치 코드의 data-key 는 그대로 둬도 서버가 읽지 않는다.
  const install = `<script src="${origin}/embed.js"></script>`;
  els.settingsBody.innerHTML = `<section>
    <h2>위젯·설치</h2><p class="lead">위젯 모양을 정하고, 고객사 사이트에 붙일 코드를 복사합니다.</p>
    <div class="block"><h3>모양</h3>
      <div class="field"><label for="wTitle">이름</label><input id="wTitle" class="input" value="${escape(w.title)}" aria-describedby="wTitleHint">
        <p class="muted small mt-1" id="wTitleHint">위젯 머리의 로고 옆에 보이는 이름입니다(예: 둠둠디라 상담). 인사 문장은 아래 인사말에 씁니다.</p></div>
      <div class="field"><label for="wGreeting">인사말</label><textarea id="wGreeting">${escape(w.greeting)}</textarea></div>
      <div class="field"><label>색</label><div class="color-field"><input type="color" id="wColor" value="${escape(w.color)}"><input class="input max-w-xs" id="wColorText" value="${escape(w.color)}"></div></div>
      <div class="field"><label>위치</label><div class="segment"><button type="button" data-pos="right" aria-pressed="${w.position === "right"}">오른쪽 아래</button><button type="button" data-pos="left" aria-pressed="${w.position === "left"}">왼쪽 아래</button></div></div>
      <div class="row-flex"><span class="spacer"></span><button class="btn primary" id="wSave">저장</button></div></div>
    <div class="block"><h3>설치 코드</h3><p class="muted small mb-2">사이트의 &lt;/body&gt; 바로 앞에 넣어 주세요.</p>
      <div class="code" id="installCode">${escape(install)}</div>
      <p class="muted small mt-3 mb-2">로그인 회원을 넘기려면 서버에서 서명한 토큰을 전달해 주세요. 서명 비밀은 <strong>워크스페이스</strong> 페이지에 있습니다.</p>
      <div class="code">&lt;script&gt;ChatWidget.identify("&lt;서버가 서명한 회원 토큰&gt;")&lt;/script&gt;</div>
      <div class="row-flex mt-2"><button class="btn sm" id="copyInstall">설치 코드 복사</button><a class="btn sm" href="/widget" target="_blank">미리보기 ↗</a></div></div></section>`;
  let position = w.position;
  for (const b of els.settingsBody.querySelectorAll("[data-pos]")) {
    b.addEventListener("click", () => {
      position = b.dataset.pos;
      for (const x of els.settingsBody.querySelectorAll("[data-pos]")) x.setAttribute("aria-pressed", String(x === b));
    });
  }
  $("wColor").addEventListener("input", () => ($("wColorText").value = $("wColor").value));
  $("wColorText").addEventListener("input", () => { if (/^#[0-9a-f]{6}$/i.test($("wColorText").value)) $("wColor").value = $("wColorText").value; });
  $("wSave").addEventListener("click", () =>
    saveSettings({ widget: { title: $("wTitle").value, greeting: $("wGreeting").value, color: $("wColorText").value, position } }));
  $("copyInstall").addEventListener("click", async () => {
    await navigator.clipboard?.writeText(install);
    toast("복사했습니다", "ok");
  });
}

// ── 빠른 답변 관리 (D36) ──────────────────────────────────────────

// snippetForm 은 빠른 답변 하나를 만들거나 고치는 창이다. 규칙은 서버와 같다(단축어 40자, 제목 80자, 본문 4000자, 비우면 안 된다).
function snippetForm(sn) {
  return form({
    title: sn ? "빠른 답변 고치기" : "빠른 답변 추가",
    body: "작성칸에서 / 와 단축어를 치면 본문이 들어갑니다.",
    fields: [
      { name: "shortcut", label: "단축어", value: sn?.shortcut || "", required: true, placeholder: "예: 배송" },
      { name: "title", label: "제목", value: sn?.title || "", required: true },
      { name: "body", label: "본문", type: "textarea", value: sn?.body || "", maxlength: 4000, counter: true },
    ],
    ok: sn ? "저장" : "추가",
    validate: (d) => (!d.body.trim() ? "본문이 비어 있습니다" : ""),
  });
}

// renderSnippets 는 빠른 답변 목록·추가·고치기·지우기다. 바꿀 때마다 목록을 서버에서 다시 받아
// 작성칸의 "/" 목록(snippets)도 같이 바뀐다. 다른 상담원의 데스크는 다시 로그인하거나 새로고침할 때 받는다.
async function renderSnippets() {
  const list = await reloadSnippets();
  const rows = list.map((sn) => `<tr data-id="${escape(sn.id)}">
      <td class="nowrap"><span class="code inline">/${escape(sn.shortcut)}</span></td>
      <td><strong>${escape(sn.title)}</strong><div class="muted small snippet-body">${escape(sn.body)}</div></td>
      <td class="actions"><button class="btn ghost sm" data-edit="${escape(sn.id)}">고치기</button>
        <button class="btn ghost sm danger" data-del="${escape(sn.id)}">지우기</button></td></tr>`).join("");
  els.settingsBody.innerHTML = `<section>
    <h2>빠른 답변</h2><p class="lead">작성칸에서 / 를 치면 고를 수 있는 답변입니다. 여기서 바꾸면 내 작성칸에는 바로 반영되고, 다른 상담원은 새로고침하면 받습니다.</p>
    <div class="block"><div class="block-head"><h3>${list.length}개</h3><span class="spacer"></span>
      <button class="btn primary sm" id="snippetAdd">+ 빠른 답변</button></div>
      ${rows ? `<table class="table"><thead><tr><th>단축어</th><th>제목·본문</th><th></th></tr></thead><tbody>${rows}</tbody></table>` : emptyHTML("⚡", "빠른 답변이 없습니다", "자주 쓰는 답을 추가해 주세요")}</div></section>`;
  const save = async (sn) => {
    const data = await snippetForm(sn);
    if (!data) return;
    try {
      if (sn) await api(`/api/agent/snippets/${sn.id}`, { token, method: "PUT", body: data });
      else await api("/api/agent/snippets", { token, method: "POST", body: data });
      toast(sn ? "고쳤습니다" : "추가했습니다", "ok");
    } catch (err) {
      if (err.status === 409) toast("같은 단축어의 빠른 답변이 이미 있습니다. 다른 단축어를 써 주세요", "danger");
      else toastError(err);
    }
    renderSnippets();
  };
  $("snippetAdd").addEventListener("click", () => save(null));
  for (const b of els.settingsBody.querySelectorAll("[data-edit]")) {
    b.addEventListener("click", () => save(list.find((x) => x.id === b.dataset.edit)));
  }
  for (const b of els.settingsBody.querySelectorAll("[data-del]")) {
    const sn = list.find((x) => x.id === b.dataset.del);
    b.addEventListener("click", async () => {
      if (!(await confirm({ title: `/${sn.shortcut} 를 지울까요?`, body: sn.title, ok: "지우기", danger: true }))) return;
      try {
        await api(`/api/agent/snippets/${sn.id}`, { token, method: "DELETE" });
        toast("지웠습니다", "ok");
      } catch (err) {
        toastError(err);
      }
      renderSnippets();
    });
  }
}

// ── 위젯 FAQ (D36) ────────────────────────────────────────────────

// renderFaq 는 위젯 홈의 "자주 묻는 질문" 편집 화면이다. 항목을 더하고 고치고 지우고 순서를 바꾼 뒤 한 번에 저장한다.
// state 가 있으면 서버에 묻지 않고 그 목록으로 다시 그린다(순서 바꾸기·추가·삭제는 저장 전까지 화면 안의 일이다).
async function renderFaq(state) {
  const faq = state || (await api("/api/admin/widget-faq", { token }));
  const items = faq.items || [];
  const rows = items.map((it, i) => faqEditorRowHTML(it, i, items.length)).join("");
  const full = items.length >= FAQ_LIMITS.items;
  els.settingsBody.innerHTML = `<section>
    <h2>자주 묻는 질문</h2><p class="lead">위젯의 자주 묻는 질문 화면에 이 순서대로 보이고, 앞의 ${FAQ_HOME_LIMIT}개는 위젯 홈에 알약으로도 보입니다. ${FAQ_LIMITS.items}개까지, 질문 ${FAQ_LIMITS.question}자·답 ${FAQ_LIMITS.answer}자까지.
      저장하면 새로 여는 위젯부터 보이고, 이미 열린 위젯은 설정을 다시 받을 때(최대 5분) 바뀝니다.</p>
    <div class="block">
      ${faq.is_default ? '<p class="muted small mb-3"><span class="chip">기본 질문</span> 아직 저장한 적이 없어 기본 질문을 보여 주는 중입니다. 저장하면 이 목록으로 바뀝니다.</p>' : ""}
      ${rows || emptyHTML("❓", "질문이 없습니다", "비워서 저장하면 위젯에 이 칸이 보이지 않습니다")}
      <div class="form-error" id="faqError" role="alert" hidden></div>
      <div class="block-foot"><button type="button" class="btn" id="faqAdd" ${full ? "disabled" : ""}>+ 항목 추가</button>
        <span class="muted small">${items.length}/${FAQ_LIMITS.items}</span><span class="spacer"></span><button class="btn primary" id="faqSave">저장</button></div></div></section>`;

  const read = () => [...els.settingsBody.querySelectorAll(".faq-item")].map((row) => ({
    question: row.querySelector('[data-k="question"]').value,
    answer: row.querySelector('[data-k="answer"]').value,
  }));
  const rerender = (next) => renderFaq({ ...faq, items: next });
  for (const row of els.settingsBody.querySelectorAll(".faq-item")) {
    const i = Number(row.dataset.i);
    for (const b of row.querySelectorAll("[data-move]")) b.addEventListener("click", () => rerender(moveItem(read(), i, Number(b.dataset.move))));
    row.querySelector("[data-remove]").addEventListener("click", () => rerender(read().filter((_, j) => j !== i)));
  }
  $("faqAdd").addEventListener("click", () => {
    const next = [...read(), { question: "", answer: "" }];
    rerender(next).then(() => $(`faqQ${next.length - 1}`)?.focus());
  });
  $("faqSave").addEventListener("click", async () => {
    const next = read();
    const problem = validateFaq(next);
    if (problem) {
      $("faqError").textContent = problem;
      $("faqError").hidden = false;
      return;
    }
    try {
      const saved = await api("/api/admin/widget-faq", { token, method: "PUT", body: { items: next } });
      toast("저장했습니다", "ok");
      renderFaq(saved);
    } catch (err) {
      $("faqError").textContent = err.message;
      $("faqError").hidden = false;
    }
  });
}

async function renderWorkspace() {
  const ws = await api("/api/admin/workspace", { token });
  els.settingsBody.innerHTML = `<section>
    <h2>워크스페이스</h2><p class="lead">우리 회사의 이름, 위젯을 띄울 사이트, 회원 서명 비밀을 정합니다. 비밀은 여기서만 볼 수 있습니다.</p>
    <div class="block"><h3>기본</h3>
      <div class="field"><label for="wsName">이름</label><input id="wsName" class="input" value="${escape(ws.name)}" maxlength="80"></div>
      <div class="field"><label for="wsOrigins">허용 사이트 (한 줄에 하나, 비우면 모든 사이트)</label>
        <textarea id="wsOrigins" rows="3" placeholder="https://shop.example.com">${escape((ws.allowed_origins || []).join("\n"))}</textarea>
        <p class="muted small mt-2">여기 적은 사이트에서만 위젯이 뜹니다. 다른 사이트에 설치 코드를 복사해 가도 소용없습니다.</p></div>
      <div class="row-flex"><span class="spacer"></span><button class="btn primary" id="wsSave">저장</button></div></div>
    <div class="block"><h3>회원 연동</h3>
      <div class="field"><label>회원 서명 비밀 (비공개)</label><div class="code" id="wsSecret">••••••••••••••••••••••••••••••••</div>
        <div class="row-flex mt-2"><button class="btn sm" id="wsReveal">보기</button><button class="btn sm" id="wsCopySecret">복사</button><span class="spacer"></span><button class="btn sm danger" id="wsRotate">새로 발급</button></div>
        <p class="muted small mt-2">호스트 서버가 회원 토큰을 서명할 때 씁니다. 새로 발급하면 옛 비밀로 서명한 토큰은 즉시 거절됩니다.</p></div></div>
    <div class="block"><h3>내보내기</h3><p class="muted small mb-2">모든 대화와 메시지(내부 메모 포함)를 파일로 받습니다. 감사 로그에 남습니다.</p>
      <div class="row-flex"><button class="btn sm" data-export="json">JSON</button><button class="btn sm" data-export="csv">CSV</button></div></div></section>`;
  $("wsSave").addEventListener("click", async () => {
    try {
      const saved = await api("/api/admin/workspace", { token, method: "PUT", body: { name: $("wsName").value, allowed_origins: $("wsOrigins").value.split("\n").map((s) => s.trim()).filter(Boolean) } });
      // 위쪽 이름도 저장한 값으로 바로 바꾼다. 새로고침해야 바뀌면 저장이 안 된 줄 안다.
      if (me?.workspace) me.workspace.name = saved.name;
      els.brandName.textContent = saved.name ? `· ${saved.name}` : "";
      toast("저장했습니다", "ok");
      renderWorkspace();
    } catch (err) { toastError(err); }
  });
  let shown = false;
  $("wsReveal").addEventListener("click", () => { shown = !shown; $("wsSecret").textContent = shown ? ws.member_secret : "••••••••••••••••••••••••••••••••"; $("wsReveal").textContent = shown ? "숨기기" : "보기"; });
  $("wsCopySecret").addEventListener("click", async () => { await navigator.clipboard?.writeText(ws.member_secret); toast("복사했습니다", "ok"); });
  $("wsRotate").addEventListener("click", async () => {
    if (!(await confirm({ title: "회원 서명 비밀을 새로 발급할까요?", body: "옛 비밀로 서명한 회원 토큰은 즉시 거절됩니다. 호스트 서버를 먼저 바꿀 준비가 되었을 때 눌러 주세요.", ok: "새로 발급", danger: true }))) return;
    try { await api("/api/admin/workspace/rotate-member-secret", { token, method: "POST" }); toast("새 비밀을 발급했습니다", "ok"); renderWorkspace(); }
    catch (err) { toastError(err); }
  });
  for (const b of els.settingsBody.querySelectorAll("[data-export]")) b.addEventListener("click", () => downloadExport(b.dataset.export));
}

// downloadExport 는 인증 헤더가 필요해 <a href> 로 못 받는다. blob 으로 받아 내려준다.
async function downloadExport(format) {
  try {
    const res = await fetch(`/api/admin/export?format=${format}`, { headers: { Authorization: `Bearer ${token}` } });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const url = URL.createObjectURL(await res.blob());
    const a = Object.assign(document.createElement("a"), { href: url, download: `conversations.${format}` });
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
  } catch (err) { toastError(err, "내보내지 못했습니다"); }
}

const ACTION_LABEL = {
  "workspace.signup": "워크스페이스 생성(옛 기록)", "workspace.update": "워크스페이스 설정 변경", "workspace.rotate_member_secret": "회원 비밀 재발급",
  "workspace.plan": "플랜 변경(옛 기록)", "workspace.export": "내보내기", "settings.update": "운영 설정 변경", "invite.create": "초대",
  "invite.revoke": "초대 취소", "invite.accept": "초대 수락", "agent.role": "역할 변경", "agent.active": "활성 변경",
  "agent.password": "비밀번호 변경", "customer.delete": "고객 삭제",
  "agent.approve": "가입 승인", "agent.reject": "가입 거절",
  "conversation.delete": "상담 삭제", "conversation.restore": "상담 되살리기",
  "agent.name": "이름 변경", "widget_faq.update": "자주 묻는 질문 변경",
};

const WORK_LABEL = {
  opened: "문의 시작", assigned: "담당 지정", transferred: "이관받음", unassigned: "담당 해제",
  answered: "응답", closed: "종료", snoozed: "보류", reopened: "다시 엶", tagged: "태그",
  deleted: "상담 삭제", restored: "상담 되살림",
};

// workLabel 은 업무 기록 한 줄의 무엇이다. 고객이 직접 닫은 종료(D48)는 상담원의 종료와 갈라 "고객이 종료" 다.
const workLabel = (e) => (e.kind === "closed" && e.detail?.by === "customer" ? "고객이 종료" : WORK_LABEL[e.kind] || e.kind);

// 업무 기록: 누가 어떤 고객의 어떤 대화에 무엇을 했나. 칸반·대시보드가 붙을 자리의 원천 데이터다.
async function renderWork() {
  const [summary, log] = await Promise.all([
    api("/api/agent/work/summary", { token }),
    api("/api/agent/work?limit=100", { token }),
  ]);
  const rows = summary.agents.map((a) => `<tr><td><strong>${escape(a.agent_name)}</strong></td>
    <td>${a.assigned}</td><td>${a.answered}</td><td>${a.closed}</td><td>${a.snoozed}</td><td>${a.customers}</td></tr>`).join("");
  const events = log.events.map((e) => `<tr><td class="muted small nowrap">${escape(relativeTime(e.created_at))}</td>
    <td>${escape(e.agent_name || "고객")}</td><td>${escape(workLabel(e))}</td>
    <td><a href="#" data-conv="${e.conversation_id}">${escape(e.customer_name || "고객")}</a></td></tr>`).join("");
  const scope = can("work.view_all") ? "누가 어떤 고객의 대화를 맡고, 답하고, 끝냈는지 보여 줍니다." : "내가 맡고, 답하고, 끝낸 대화를 보여 줍니다.";
  els.settingsBody.innerHTML = `<section><h2>업무 기록</h2><p class="lead">최근 7일. ${scope}</p>
    <div class="block"><h3>상담원별</h3>${rows ? `<table class="table"><thead><tr><th>상담원</th><th>배정</th><th>응답</th><th>종료</th><th>보류</th><th>고객 수</th></tr></thead><tbody>${rows}</tbody></table>` : emptyHTML("📊", "아직 기록이 없습니다", "")}</div>
    <div class="block"><h3>최근 처리</h3>${events ? `<table class="table"><tbody>${events}</tbody></table>` : emptyHTML("🗂️", "아직 기록이 없습니다", "")}</div></section>`;
  for (const a of els.settingsBody.querySelectorAll("[data-conv]")) {
    a.addEventListener("click", (e) => { e.preventDefault(); showMain("inbox"); selectConversation(a.dataset.conv); });
  }
}

// 지표(D45): AI 처리율, 첫 응답·평균 응답 시간, 만족도, 종료 수와 처리 주체, 답한 주체별 표. 권한은 work.view_all(서버가 막는다).
let metricsRange = { kind: "7d", fromDate: "", toDate: "" };

const dateText = (d) => d.toLocaleDateString("ko-KR", { year: "numeric", month: "2-digit", day: "2-digit" });
const isoDate = (d) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;

async function renderMetrics() {
  const win = metricsWindow(metricsRange.kind, metricsRange);
  if (!win) {
    metricsRange = { kind: "7d", fromDate: "", toDate: "" };
    toast("시작일과 종료일을 확인해 주세요", "danger");
    return renderMetrics();
  }
  const q = `from=${encodeURIComponent(win.from.toISOString())}&to=${encodeURIComponent(win.to.toISOString())}`;
  const m = await api(`/api/agent/metrics?${q}`, { token });
  const lastDay = new Date(win.to.getTime() - 1);
  const ranges = METRIC_RANGES.map(([k, label]) => `<button type="button" data-range="${k}" aria-pressed="${metricsRange.kind === k}">${label}</button>`).join("");
  const cards = metricCards(m).map((c) => `<div class="stat" data-stat="${c.key}">
      <span class="stat-label">${escape(c.label)}</span>
      <strong class="stat-value${c.value === "—" ? " is-empty" : ""}">${escape(c.value)}</strong>
      ${c.split
        ? `<span class="stat-split"><span class="is-agent"><i class="dot"></i>상담원 ${c.split.agent || 0}</span><span class="is-ai"><i class="dot"></i>AI ${c.split.ai || 0}</span><span class="is-self"><i class="dot"></i>고객 자체 해결 ${c.split.self || 0}</span><span><i class="dot"></i>미응답 ${c.split.unanswered || 0}</span></span>`
        : `<span class="stat-sub">${escape(c.sub)}</span>`}
    </div>`).join("");
  const rows = (m.responders || []).map((r) => `<tr>
      <td><strong>${escape(r.kind === "AI" ? "AI 응대" : r.agent_name || "상담원")}</strong></td>
      <td>${r.handled}</td><td>${r.response.count}</td>
      <td>${durationText(r.first_response.avg_ms)}</td><td>${durationText(r.response.avg_ms)}</td>
      <td>${durationText(r.response.p50_ms)}</td><td>${durationText(r.response.p90_ms)}</td>
      <td>${scoreText(r.satisfaction.average)}</td><td>${percentText(r.satisfaction.csat)}</td><td>${r.satisfaction.responses}</td></tr>`).join("");
  els.settingsBody.innerHTML = `<section class="metrics-page"><h2>지표</h2>
    <p class="lead">종료한 상담의 처리 주체, 응답 시간, 만족도를 기간별로 봅니다. 삭제한 상담은 세지 않습니다.</p>
    <div class="metrics-toolbar">
      <div class="segment" role="group" aria-label="기간">${ranges}</div>
      <form class="metrics-range" id="mCustom"${metricsRange.kind === "custom" ? "" : " hidden"}>
        <div class="field"><label for="mFrom">시작일</label><input class="input" type="date" id="mFrom" value="${escape(metricsRange.fromDate || isoDate(win.from))}"></div>
        <div class="field"><label for="mTo">종료일</label><input class="input" type="date" id="mTo" value="${escape(metricsRange.toDate || isoDate(lastDay))}"></div>
        <button class="btn" type="submit">적용</button>
      </form>
      <span class="muted small">${dateText(win.from)} ~ ${dateText(lastDay)}</span>
    </div>
    <div class="stat-grid">${cards}</div>
    <div class="block"><h3>답한 주체별</h3>${rows
      ? `<div class="table-scroll"><table class="table"><thead><tr><th>주체</th><th>처리 상담</th><th>응답</th><th>첫 응답</th><th>평균 응답</th><th>중앙값</th><th>p90</th><th>만족도</th><th>CSAT</th><th>평가</th></tr></thead><tbody>${rows}</tbody></table></div>`
      : emptyHTML("📈", "이 기간의 기록이 없습니다", "상담원·AI 가 답하거나 상담을 종료하면 여기에 쌓입니다")}</div>
    <p class="muted small">AI 처리율 = 상담원 없이 끝난 종료 상담(AI 응대만으로 끝났거나, 답 없이 고객이 직접 닫은 고객 자체 해결) / 상담원·AI·고객 자체 해결로 끝난 종료 상담. 미응답은 넣지 않습니다. 고객 자체 해결은 만족도 조사를 받지 않아 응답률에서 뺍니다. 응답 시간은 고객이 기다리기 시작한 글부터 상담원·AI 의 공개 답까지(접수 안내·내부 메모 제외). CSAT 는 4·5점 비율, 응답률은 조사를 받은 종료 상담(고객 자체 해결을 뺀 종료 상담) 대비 평가 수입니다.</p>
  </section>`;
  for (const b of els.settingsBody.querySelectorAll("[data-range]")) {
    b.addEventListener("click", () => {
      if (b.dataset.range === "custom") {
        metricsRange = { kind: "custom", fromDate: isoDate(win.from), toDate: isoDate(lastDay) };
      } else {
        metricsRange = { kind: b.dataset.range, fromDate: "", toDate: "" };
      }
      renderMetrics().catch((err) => toastError(err));
    });
  }
  $("mCustom")?.addEventListener("submit", (e) => {
    e.preventDefault();
    metricsRange = { kind: "custom", fromDate: $("mFrom").value, toDate: $("mTo").value };
    renderMetrics().catch((err) => toastError(err));
  });
}

async function renderAudit() {
  const out = await api("/api/admin/audit?limit=200", { token });
  const rows = out.entries.map((e) => `<tr><td class="muted small nowrap">${escape(relativeTime(e.created_at))}</td>
    <td><strong>${escape(e.actor_name || "시스템")}</strong></td><td>${escape(ACTION_LABEL[e.action] || e.action)}</td>
    <td class="muted small">${escape(e.target || "")} ${escape(Object.keys(e.detail || {}).length ? JSON.stringify(e.detail) : "")}</td></tr>`).join("");
  els.settingsBody.innerHTML = `<section><h2>감사 로그</h2><p class="lead">누가 언제 무엇을 바꿨는지 남깁니다. 지워지지 않습니다.</p>
    <div class="block">${rows ? `<table class="table"><tbody>${rows}</tbody></table>` : emptyHTML("📜", "아직 기록이 없습니다", "")}</div></section>`;
}

function renderAccount() {
  els.settingsBody.innerHTML = `<section>
    <h2>내 계정</h2><p class="lead">${escape(me.name)} · ${ROLE_LABEL[me.role] || ""}</p>
    <div class="block"><h3>이름</h3>
      <div class="field"><label for="myName">고객과 팀에게 보이는 이름 (40자까지)</label><input id="myName" class="input" value="${escape(me.name || "")}" maxlength="40" autocomplete="name"></div>
      <div class="row-flex"><span class="muted small">이미 보낸 메시지에도 새 이름이 보입니다.</span><span class="spacer"></span><button class="btn primary" id="nameSave">저장</button></div></div>
    <div class="block"><h3>비밀번호 변경</h3>
      <div class="field"><label for="pwCur">현재 비밀번호</label><input id="pwCur" type="password" autocomplete="current-password"></div>
      <div class="field"><label for="pwNext">새 비밀번호 (8자 이상)</label><input id="pwNext" type="password" autocomplete="new-password"></div>
      <div class="field"><label for="pwNext2">새 비밀번호 확인</label><input id="pwNext2" type="password" autocomplete="new-password"></div>
      <div class="row-flex"><span class="muted small">바꾸면 다른 기기의 세션이 전부 끊깁니다.</span><span class="spacer"></span><button class="btn primary" id="pwSave">변경</button></div></div></section>`;
  $("nameSave").addEventListener("click", async () => {
    try {
      const out = await api("/api/agent/name", { token, method: "POST", body: { name: $("myName").value } });
      me.name = out.name;
      paintMyName();
      toast("이름을 바꿨습니다", "ok");
      renderAccount();
    } catch (err) {
      toastError(err);
    }
  });
  $("pwSave").addEventListener("click", async () => {
    if ($("pwNext").value !== $("pwNext2").value) return toast("새 비밀번호가 서로 다릅니다", "danger");
    try {
      await api("/api/agent/password", { token, method: "POST", body: { current: $("pwCur").value, next: $("pwNext").value } });
      toast("비밀번호를 바꿨습니다", "ok");
      renderAccount();
    } catch (err) {
      toastError(err);
    }
  });
}

// ── 초대 수락 ────────────────────────────────────────────────────

async function showInvite(inviteToken) {
  els.loginView.hidden = true;
  els.inviteView.hidden = false;
  try {
    // 토큰은 본문으로 보낸다(D46). 주소(쿼리)에 실으면 접근 로그와 Referer 에 남는다.
    const inv = await api("/api/agent/invites/preview", { method: "POST", body: { token: inviteToken } });
    els.inviteDesc.textContent = `${inv.name} 님, ${withRo(ROLE_LABEL[inv.role])} 초대되었습니다 (${inv.email}). 비밀번호를 정하면 바로 시작합니다.`;
  } catch {
    els.inviteDesc.textContent = "초대가 만료되었거나 이미 사용되었습니다. 관리자에게 새 링크를 요청해 주세요.";
    els.inviteSubmit.disabled = true;
    return;
  }
  els.inviteForm.addEventListener("submit", async (e) => {
    e.preventDefault();
    try {
      const res = await api("/api/agent/invites/accept", { method: "POST", body: { token: inviteToken, password: els.invitePassword.value } });
      token = res.token;
      localStorage.setItem(TOKEN_KEY, token);
      history.replaceState(null, "", "/desk");
      await enterDesk();
    } catch (err) {
      toastError(err);
    }
  });
}

// inviteFromAddress 는 주소의 초대 토큰이다. 지금 링크는 조각(#invite=)에 싣는다 — 조각은 서버로 가지 않는다(D46).
// 옛 링크(?invite=)도 받는다. 받으면 곧바로 주소를 조각 꼴로 바꿔 쿼리를 지운다(replaceState) — 이 페이지에서 나가는 요청과 방문 기록에 남지 않게.
// 옛 링크를 연 그 한 번의 요청은 이미 쿼리를 실어 갔다. 그 기록은 막을 수 없다.
function inviteFromAddress() {
  const fromHash = new URLSearchParams(String(location.hash || "").replace(/^#/, "")).get("invite");
  if (fromHash) return fromHash;
  const legacy = new URLSearchParams(location.search).get("invite");
  if (!legacy) return "";
  history.replaceState(null, "", `/desk#invite=${encodeURIComponent(legacy)}`);
  return legacy;
}

// ── 이벤트 ───────────────────────────────────────────────────────

els.loginForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    const body = { email: els.email.value, password: els.password.value };
    const res = await api("/api/agent/login", { method: "POST", body });
    token = res.token;
    localStorage.setItem(TOKEN_KEY, token);
    await enterDesk();
  } catch (err) {
    toastError(err);
  }
});

els.joinLink.addEventListener("click", (e) => { e.preventDefault(); els.loginView.hidden = true; els.joinView.hidden = false; });
els.joinBackToLogin.addEventListener("click", (e) => { e.preventDefault(); els.joinView.hidden = true; els.loginView.hidden = false; });
// 상담원 가입은 늘 같은 답(202)이 온다. 계정이 이미 있는지 알려 주지 않는다 — 그래서 화면도 한 가지로 답한다.
els.joinForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await api("/api/agent/signup", {
      method: "POST",
      body: { name: els.jName.value, email: els.jEmail.value, password: els.jPassword.value },
    });
  } catch (err) {
    return toastError(err);
  }
  els.joinDesc.textContent = "신청을 보냈습니다. 관리자가 승인하면 이 이메일과 비밀번호로 로그인할 수 있습니다.";
  els.joinSubmit.disabled = true;
  els.email.value = els.jEmail.value;
  els.password.value = "";
});

els.pDelete.addEventListener("click", async () => {
  if (!customer) return;
  if (!(await confirm({ title: `${customer.display_name} 고객을 삭제할까요?`, body: "이름·연락처·메시지 본문·첨부가 지워지고 되돌릴 수 없습니다. 대화와 번호는 남습니다.", ok: "삭제", danger: true }))) return;
  try {
    await api(`/api/customers/${customer.id}`, { token, method: "DELETE" });
    toast("고객 정보를 지웠습니다", "ok");
    await loadCustomer(customer.id);
    if (current) selectConversation(current.id);
  } catch (err) { toastError(err); }
});

els.meBtn.addEventListener("click", (e) => {
  e.stopPropagation();
  menu(els.meBtn, [
    { header: me?.name || "" },
    { label: "내 계정", onSelect: () => { showMain("settings"); renderSettings("account"); } },
    { divider: true },
    { label: "로그아웃", danger: true, onSelect: () => logout() },
  ]);
});

for (const b of els.mainNav.querySelectorAll("button")) b.addEventListener("click", () => showMain(b.dataset.main));
for (const b of els.settingsNav.querySelectorAll("button")) {
  b.addEventListener("click", () => {
    renderSettings(b.dataset.page);
    closeDrawer();
  });
}

// ── 좁은 화면의 서랍(정책 827·828) ──────────────────────────────
// 900px 이하에서는 왼쪽 메뉴(받은함 필터, 설정 화면이면 설정 메뉴)가 ☰ 로 여는 왼쪽 서랍이고, 1280px 이하에서는 고객 정보가 오른쪽 서랍이다.
// 서랍인지 아닌지는 CSS 가 정한다. 여기서는 열림 상태와 접근성만 맡는다 — 여는 단추의 aria-expanded, 열면 서랍 첫 항목으로 초점,
// Esc·어두운 막·항목 선택으로 닫으면 여는 단추로 초점을 돌려준다. Esc 는 서랍이나 여는 단추에 초점이 있을 때만 받는다 —
// 서랍에서 연 확인 창(모달)의 Esc 는 창만 닫는다.
const drawers = {
  nav: { cls: "nav-open", toggle: els.navToggle, panel: () => (els.settingsMain.hidden ? els.filters : els.settingsNav) },
  profile: { cls: "profile-open", toggle: els.profileToggle, panel: () => els.profile },
};
let openDrawer = "";

function closeDrawer({ restoreFocus = true } = {}) {
  if (!openDrawer) return;
  const d = drawers[openDrawer];
  openDrawer = "";
  els.deskView.classList.remove(d.cls);
  d.toggle.setAttribute("aria-expanded", "false");
  els.drawerScrim.hidden = true;
  if (restoreFocus) d.toggle.focus();
}

function toggleDrawer(name) {
  if (openDrawer === name) return closeDrawer();
  closeDrawer({ restoreFocus: false });
  const d = drawers[name];
  openDrawer = name;
  els.deskView.classList.add(d.cls);
  d.toggle.setAttribute("aria-expanded", "true");
  els.drawerScrim.hidden = false;
  d.panel().querySelector("button:not([hidden]), input, textarea")?.focus();
}

els.navToggle.addEventListener("click", () => toggleDrawer("nav"));
els.profileToggle.addEventListener("click", () => toggleDrawer("profile"));
els.drawerScrim.addEventListener("click", () => closeDrawer());
for (const el of [els.navToggle, els.profileToggle, els.filters, els.settingsNav, els.profile]) {
  el.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && openDrawer) {
      e.stopPropagation();
      closeDrawer();
    }
  });
}
// 창을 넓혀 서랍이 칸으로 돌아가면 열림 표시도 걷는다. 다시 좁혔을 때 서랍이 저절로 열려 있지 않다.
for (const q of ["(max-width: 900px)", "(max-width: 1280px)"]) {
  window.matchMedia?.(q).addEventListener?.("change", () => closeDrawer({ restoreFocus: false }));
}

els.refresh.addEventListener("click", loadInbox);
els.backToList.addEventListener("click", () => {
  closeDrawer({ restoreFocus: false });
  els.deskView.classList.add("show-list");
});
for (const btn of els.filters.querySelectorAll("button[data-filter]")) {
  btn.addEventListener("click", () => {
    filter = btn.dataset.filter;
    for (const b of els.filters.querySelectorAll("button[data-filter]")) b.setAttribute("aria-current", String(b === btn));
    closeDrawer();
    els.deskView.classList.add("show-list"); // 좁은 화면에서 필터를 고르면 그 목록을 본다
    loadInbox();
  });
}

// 담당 메뉴는 그릴 때 본 담당자를 붙잡아 고른 항목과 함께 보낸다(D39). 서버는 그 값이 지금 담당자일 때만 바꾸고, 아니면 409 다.
// 메뉴가 열린 동안 담당자가 바뀌면 adoptConversation 이 메뉴를 닫는다.
els.assignBtn.addEventListener("click", (e) => {
  e.stopPropagation();
  if (!current || current.readOnly) return;
  const convId = current.id;
  const assignee = current.assignee_agent_id || "";
  const mine = assignee === me?.agent_id;
  const pick = (agentID) => () => assign(agentID, assignee, convId);
  let items;
  // 배정 권한이 없으면 "나에게" 와 "내 담당 해제" 만 그린다. 남이 맡은 대화는 가져올 수도 없다.
  if (!can("conversation.assign")) {
    items = [];
    if (!assignee) items.push({ label: "나에게", onSelect: pick(me.agent_id) });
    if (mine) items.push({ label: "나에게", checked: true, onSelect: () => {} }, { divider: true }, { label: "담당 해제", danger: true, onSelect: pick("") });
    if (items.length === 0) items.push({ header: `${current.assignee_name || "다른 상담원"} 님이 맡고 있습니다` });
  } else {
    items = [
      { label: "나에게", checked: mine, onSelect: pick(me.agent_id) },
      { divider: true },
      ...agents.filter((a) => a.id !== me?.agent_id).map((a) => ({ label: a.name, checked: assignee === a.id, onSelect: pick(a.id) })),
      { divider: true },
      { label: "담당 해제", danger: true, onSelect: pick("") },
    ];
  }
  assignMenu = { el: menu(els.assignBtn, items), convId, assignee };
});

// 되살리면 받은함·검색·고객 화면에 돌아온다. 되살린 상담은 보통 화면으로 다시 연다.
els.restoreConvBtn.addEventListener("click", async () => {
  if (!current?.readOnly) return;
  const convId = current.id;
  if (!(await confirm({ title: `${current.customer_name} 님의 상담을 되살릴까요?`, body: "받은함·검색·고객 화면에 다시 보입니다.", ok: "되살리기" }))) return;
  try {
    await api(`/api/agent/conversations/${convId}/restore`, { token, method: "POST" });
  } catch (err) {
    return toastError(err);
  }
  toast("상담을 되살렸습니다", "ok");
  await selectConversation(convId);
});

// 상담 삭제는 가리기다. 기록은 남고 받은함·검색·고객 화면에서 사라진다. 사유는 감사 로그에 남는다.
// 사유는 서버와 같은 규칙(비어 있으면 안 되고 200자까지)을 창 안에서 먼저 본다. 틀리면 창을 닫지 않는다.
els.deleteConvBtn.addEventListener("click", async () => {
  if (!current) return;
  const convId = current.id;
  const data = await form({
    title: `${current.customer_name} 님의 상담을 삭제할까요?`,
    body: "받은함·검색·고객 화면에서 사라집니다. 메시지 기록은 지워지지 않고 내보내기와 감사 로그에 남습니다.",
    fields: [{ name: "reason", label: "삭제 사유", type: "textarea", maxlength: 200, counter: true, placeholder: "예: 같은 문의가 두 번 열렸다" }],
    validate: (d) => (!d.reason.trim() ? "삭제 사유를 적어 주세요" : [...d.reason.trim()].length > 200 ? "삭제 사유는 200자까지입니다" : ""),
    ok: "삭제",
  });
  if (!data || current?.id !== convId) return;
  // 지우기 전에 구독을 놓고 화면을 비운다. 구독을 쥔 채로 지우면 서버가 그 대화에 "없다" 를 보내고 그게 오류로 뜬다.
  closeThread();
  try {
    await api(`/api/agent/conversations/${convId}`, { token, method: "DELETE", body: { reason: data.reason } });
  } catch (err) {
    toastError(err);
    return selectConversation(convId);
  }
  toast("상담을 삭제했습니다", "ok");
  els.deskView.classList.add("show-list");
  loadInbox();
});

els.snoozeBtn.addEventListener("click", (e) => {
  e.stopPropagation();
  const tomorrow9 = (() => {
    const t = new Date();
    t.setDate(t.getDate() + 1);
    t.setHours(9, 0, 0, 0);
    return Math.max(1, Math.round((t - Date.now()) / 60000));
  })();
  menu(els.snoozeBtn, [
    { header: "다시 열 시각" },
    { label: "30분 뒤", onSelect: () => setState("SNOOZED", 30) },
    { label: "1시간 뒤", onSelect: () => setState("SNOOZED", 60) },
    { label: "내일 오전 9시", onSelect: () => setState("SNOOZED", tomorrow9) },
    { label: "직접 입력…", onSelect: async () => {
        const d = await form({ title: "보류", fields: [{ name: "minutes", label: "몇 분 뒤에 다시 열까요?", type: "number", value: "120" }], ok: "보류" });
        if (d?.minutes) setState("SNOOZED", Number(d.minutes));
      } },
    ...(current?.state === "SNOOZED" ? [{ divider: true }, { label: "보류 해제", onSelect: () => setState("OPEN") }] : []),
  ]);
});

els.closeBtn.addEventListener("click", () => setState(current?.state === "CLOSED" ? "OPEN" : "CLOSED"));

els.followBtn.addEventListener("click", async () => {
  if (!current) return;
  const follow = !followers.some((a) => a.id === me?.agent_id);
  await api(`/api/agent/conversations/${current.id}/follow`, { token, method: "POST", body: { follow } });
  loadFollowers();
});

els.tagForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  const name = els.tagInput.value.trim();
  if (!name || !current) return;
  try {
    const out = await api(`/api/agent/conversations/${current.id}/tags`, { token, method: "POST", body: { name } });
    current.tags = out.tags;
    els.tagInput.value = "";
    paintTags();
    loadTagFilters();
    loadInbox();
  } catch (err) {
    toastError(err);
  }
});

els.pEdit.addEventListener("click", async () => {
  if (!customer) return;
  const data = await form({
    title: "고객 정보", fields: [
      { name: "display_name", label: "이름", value: customer.display_name, required: true },
      { name: "email", label: "이메일", type: "email", value: customer.email || "" },
      { name: "phone", label: "전화", value: customer.phone || "" },
    ],
  });
  if (!data) return;
  try {
    customer = await api(`/api/customers/${customer.id}`, { token, method: "POST", body: { ...data, note: customer.note, attributes: customer.attributes || {} } });
    loadCustomer(customer.id);
    loadInbox();
    toast("저장했습니다", "ok");
  } catch (err) {
    toastError(err);
  }
});

els.pNoteSave.addEventListener("click", async () => {
  if (!customer) return;
  try {
    customer = await api(`/api/customers/${customer.id}`, {
      token, method: "POST",
      body: { display_name: customer.display_name, email: customer.email || "", phone: customer.phone || "", note: els.pNote.value, attributes: customer.attributes || {} },
    });
    toast("메모를 저장했습니다", "ok");
  } catch (err) {
    toastError(err);
  }
});

els.internalToggle.addEventListener("click", () => setInternal(!internalMode));

// AI 초안은 작성칸을 채울 뿐이다. 보내기는 사람이 누른다. 초안이 틀려도 고객에겐 아직 아무것도 안 갔다.
els.draftBtn.addEventListener("click", async () => {
  if (!current) return;
  const convId = current.id;
  els.draftBtn.disabled = true;
  els.composerHint.innerHTML = `<span class="attach-pill">✨ 초안 만드는 중…</span>`;
  try {
    const res = await api(`/api/agent/conversations/${convId}/draft`, { token, method: "POST" });
    if (current?.id !== convId) return; // A 의 초안을 B 작성칸에 넣지 않는다
    els.input.value = res.draft;
    autoGrow(els.input);
    els.input.focus();
    els.composerHint.innerHTML = `<span class="attach-pill">✨ AI 초안 (${escape(res.model)}) · 검토하고 고친 뒤 보내 주세요</span>`;
  } catch (err) {
    setInternal(internalMode);
    toastError(err);
  } finally {
    els.draftBtn.disabled = false;
  }
});
els.snippetBtn.addEventListener("click", () => (els.snippetPop.hidden ? paintSnippets("") : (els.snippetPop.hidden = true)));

els.composer.addEventListener("submit", (e) => {
  e.preventDefault();
  if (!current) return;
  const body = els.input.value;
  if (!body.trim() && !pendingAttachment) return;
  // 큐에 못 들어갔으면(길이 초과 등) 작성칸을 비우지 않는다. 쓴 글이 사라지는 게 곧 유실이다.
  const sentAttachment = pendingAttachment;
  const item = session.send(body, { internal: internalMode, attachment: sentAttachment });
  if (!item) return;
  drafts.remember(current.id, item);
  els.input.value = "";
  // 보낸 첨부만 비운다. 올리는 중인 파일은 글만 먼저 보내도 계속 올라간다.
  if (sentAttachment) resetAttachRow();
  els.snippetPop.hidden = true;
  autoGrow(els.input);
  els.input.focus();
});

els.input.addEventListener("keydown", (e) => {
  if (e.key === "Escape") els.snippetPop.hidden = true;
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
    if (!els.snippetPop.hidden) {
      const first = els.snippetPop.querySelector("button");
      if (first) {
        e.preventDefault();
        return applySnippet(first.dataset.id);
      }
    }
    e.preventDefault();
    els.composer.requestSubmit();
  }
});
els.input.addEventListener("input", () => {
  autoGrow(els.input);
  const value = els.input.value;
  if (value.startsWith("/")) paintSnippets(value.slice(1));
  else els.snippetPop.hidden = true;
  if (value.trim() && !internalMode) session?.typing();
});

els.file.addEventListener("change", () => attachFiles([...els.file.files]));
dropzone(els.thread, attachFiles);

let searchTimer = null;
els.search.addEventListener("input", () => {
  clearTimeout(searchTimer);
  const q = els.search.value.trim();
  if (!q) return (els.results.hidden = true);
  searchTimer = setTimeout(async () => {
    try {
      const out = await api(`/api/agent/search?q=${encodeURIComponent(q)}`, { token });
      const hits = out.hits || [];
      els.results.innerHTML = hits.length === 0
        ? emptyHTML("🔍", "결과가 없습니다", "")
        : hits.map((h) => `<button data-id="${h.conversation_id}"><div class="who">${escape(h.customer_name)} · ${escape(h.sender_name)} · ${escape(formatDateTime(h.created_at))}</div><div class="snip">${highlight(h.body, q)}</div></button>`).join("");
      els.results.hidden = false;
      for (const btn of els.results.querySelectorAll("button[data-id]")) {
        btn.addEventListener("click", () => {
          els.results.hidden = true;
          els.search.value = "";
          showMain("inbox");
          selectConversation(btn.dataset.id);
        });
      }
    } catch (err) {
      els.results.innerHTML = emptyHTML("⚠️", err.message, "");
      els.results.hidden = false;
    }
  }, 220);
});

function highlight(text, q) {
  const safe = escape(text);
  const needle = escape(q);
  const at = safe.toLowerCase().indexOf(needle.toLowerCase());
  if (at < 0) return safe;
  return `${safe.slice(0, at)}<mark>${safe.slice(at, at + needle.length)}</mark>${safe.slice(at + needle.length)}`;
}

document.addEventListener("click", (e) => {
  if (!els.results.contains(e.target) && e.target !== els.search) els.results.hidden = true;
  if (!els.snippetPop.contains(e.target) && e.target !== els.snippetBtn) els.snippetPop.hidden = true;
});
document.addEventListener("keydown", (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k" && can("search.all")) {
    e.preventDefault();
    showMain("inbox");
    els.search.focus();
  }
});

// ── 시작 ─────────────────────────────────────────────────────────

const inviteToken = inviteFromAddress();
if (inviteToken) showInvite(inviteToken);
else if (token) api("/api/me", { token }).then(enterDesk).catch(() => logout(false));
