// 위젯과 데스크가 같이 쓰는 컴포넌트.
// DOM 을 만드는 코드는 전부 여기 있고, 화면 코드는 조립만 한다.
import { formatTime, escapeText, TYPING_EXPIRE, isPermanentReject } from "/chat.js";

export const $ = (id) => document.getElementById(id);
export const escape = escapeText;

const STATUS_LABEL = { online: "연결됨", connecting: "연결 중", offline: "연결 끊김" };

export function bindStatus(dotEl, textEl) {
  return (state) => {
    dotEl.dataset.state = state;
    if (textEl) textEl.textContent = STATUS_LABEL[state] || state;
  };
}

// 받침이 있으면 "이", 없으면 "가". 이름이 서버에서 오니 하드코딩할 수 없다.
export function subjectParticle(name) {
  const code = name.charCodeAt(name.length - 1);
  const hangul = code >= 0xac00 && code <= 0xd7a3;
  return hangul && (code - 0xac00) % 28 !== 0 ? "이" : "가";
}

export const initials = (name) => (name || "?").trim().slice(0, 1).toUpperCase();

// 이름마다 같은 색이 나오게 해시한다. 글자 하나짜리 아바타가 전부 회색이면 구분이 안 된다.
// 흰 이니셜이 얹히므로 여덟 색 모두 흰 글자 대비 5:1 이상이다(D41. 전에는 #f59f00 이 2.13:1 이었다).
// 색은 디자인 시스템의 .avatar.tone-0~7 이 칠한다(web/styles/tokens/_color.scss 의 $avatar-tones, 순서까지 같다). 인라인 색을 쓰지 않는다.
export const AVATAR_COLORS = ["#3b5bdb", "#c2410c", "#087f5b", "#9c36b5", "#c2255c", "#0b7285", "#8a5a00", "#6741d9"];
export function avatarTone(name) {
  let h = 0;
  for (const c of name || "") h = (h * 31 + c.charCodeAt(0)) >>> 0;
  return h % AVATAR_COLORS.length;
}
export const avatarColor = (name) => AVATAR_COLORS[avatarTone(name)];
export function avatarHTML(name, cls = "") {
  const extra = cls ? ` ${cls}` : "";
  return `<div class="avatar tone-${avatarTone(name)}${extra}">${escape(initials(name))}</div>`;
}

export function humanSize(bytes) {
  if (bytes >= 1 << 20) return `${(bytes / (1 << 20)).toFixed(1)}MB`;
  if (bytes >= 1 << 10) return `${Math.round(bytes / (1 << 10))}KB`;
  return `${bytes}B`;
}

export function relativeTime(iso) {
  if (!iso) return "";
  const diff = Date.now() - new Date(iso).getTime();
  const min = Math.floor(diff / 60000);
  if (min < 1) return "방금";
  if (min < 60) return `${min}분 전`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}시간 전`;
  const day = Math.floor(hr / 24);
  if (day < 7) return `${day}일 전`;
  return new Date(iso).toLocaleDateString("ko-KR", { month: "numeric", day: "numeric" });
}

// formatDateTime 은 서버가 준 RFC 3339 시각을 "2026. 10. 4. 오후 09:31" 로 쓴다.
// RFC 3339 가 아닌 값(옛 "2026-10-04 12:31" 형식 등)과 해석할 수 없는 값은 원문 그대로 둔다.
// 브라우저마다 다르게 해석하는 모양을 억지로 읽지 않는다.
export function formatDateTime(value) {
  if (!value || !String(value).includes("T")) return value || "";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return value;
  return d.toLocaleString("ko-KR", { year: "numeric", month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

// expiryLabel 은 만료 시각까지 남은 시간을 말로 쓴다. 초대 대기 줄이 쓴다.
// relativeTime 은 지난 시각용이라 미래 시각을 넣으면 "방금" 이 나온다. 그래서 따로 센다.
export function expiryLabel(iso, now = Date.now()) {
  const left = new Date(iso).getTime() - now;
  if (!(left > 0)) return "만료됨";
  const min = Math.floor(left / 60000);
  if (min < 1) return "곧 만료";
  if (min < 60) return `${min}분 뒤 만료`;
  const hr = Math.floor(min / 60);
  if (hr < 24 * 7) return `${hr}시간 뒤 만료`;
  return `${Math.floor(hr / 24)}일 뒤 만료`;
}

// rankSnippets 는 작성칸의 "/질의" 에 맞는 빠른 답변을 고른다. Enter 는 첫 항목을 넣는다.
// 순서: 단축어 완전 일치 → 단축어 앞부분 일치 → 그 밖(단축어 중간·제목). 같은 등급 안에서는 서버 순서(단축어 가나다)다.
// 제목까지 찾는 건 기억이 안 날 때를 위해서다. 그것 때문에 정확히 친 단축어가 밀리면 엉뚱한 답이 나간다.
export function rankSnippets(snippets, query) {
  const q = (query || "").toLowerCase();
  const grade = (s) => {
    const sc = s.shortcut.toLowerCase();
    if (!q) return 2;
    if (sc === q) return 0;
    if (sc.startsWith(q)) return 1;
    if (sc.includes(q) || s.title.toLowerCase().includes(q)) return 2;
    return -1;
  };
  return snippets
    .map((s, i) => ({ s, i, g: grade(s) }))
    .filter((x) => x.g >= 0)
    .sort((a, b) => a.g - b.g || a.i - b.i)
    .map((x) => x.s);
}

// sessionErrorNotice 는 위젯이 고객 세션을 못 열었을 때 고객에게 보일 문장과 자동 재시도 간격을 정한다.
// 서버 code 는 바꾸지 않고 HTTP 상태로 가른다. retryInMs 가 0 이면 저절로 다시 시도하지 않는다(다시 해도 같다).
export function sessionErrorNotice(err) {
  const status = err?.status || 0;
  if (status === 429 || err?.code === "too_many") {
    const sec = err?.retryAfter > 0 ? err.retryAfter : 30;
    return { text: "요청이 많아 잠시 후 다시 연결합니다", retryInMs: sec * 1000 };
  }
  if (err?.code === "origin_not_allowed") return { text: "이 사이트에서는 상담을 열 수 없습니다. 사이트 관리자에게 알려 주세요", retryInMs: 0 };
  if (!status) return { text: "상담 서버에 연결하지 못했습니다. 잠시 후 다시 시도해 주세요", retryInMs: 0 };
  // 서버가 답은 했는데 열지 못했다. 문의 번호가 있으면 붙인다 — 고객이 알려 주면 그 요청의 로그를 찾는다(D47).
  const no = inquiryNumber(err?.traceId);
  return { text: `상담을 열지 못했습니다. 잠시 후 다시 시도해 주세요${no ? ` (문의 번호 ${no})` : ""}`, retryInMs: 0 };
}

// dayLabel 은 날짜 구분선 글자다. 오늘·어제는 말로, 나머지는 날짜로.
export function dayLabel(iso) {
  const d = new Date(iso);
  const today = new Date();
  const dayKey = (x) => `${x.getFullYear()}-${x.getMonth()}-${x.getDate()}`;
  if (dayKey(d) === dayKey(today)) return "오늘";
  const yesterday = new Date(today);
  yesterday.setDate(today.getDate() - 1);
  if (dayKey(d) === dayKey(yesterday)) return "어제";
  return d.toLocaleDateString("ko-KR", { year: "numeric", month: "long", day: "numeric", weekday: "short" });
}

export const STATE_LABEL = { OPEN: "진행중", SNOOZED: "보류", CLOSED: "완료" };
// CLOSED_BY_CUSTOMER 는 고객이 직접 닫은 상담(D48)의 표시다. 데스크의 목록 줄과 대화 머리에서 상태(완료) 옆에 붙는다.
export const CLOSED_BY_CUSTOMER = "고객 종료";
export const ROLE_LABEL = { ADMIN: "관리자", STAFF: "직원", PART_TIME: "아르바이트" };

// withRo 는 낱말 뒤에 받침에 맞는 조사 "으로/로" 를 붙인다. 받침이 없거나 ㄹ 받침이면 "로" 다. 한글이 아니면 "로" 로 둔다.
export function withRo(word) {
  const s = String(word || "");
  const code = s.charCodeAt(s.length - 1) - 0xac00;
  if (code < 0 || code > 11171) return `${s}로`;
  const jong = code % 28;
  return jong === 0 || jong === 8 ? `${s}로` : `${s}으로`;
}

// ── 알림·모달·메뉴 ────────────────────────────────────────────────

let toastRoot = null;
// toast 는 알림 한 줄을 띄운다. copy 를 주면 "복사" 단추를 달고 닫을 때까지 남긴다 —
// 보내지 못한 글이 입력칸에도 대화에도 없을 때 그 글이 사라지지 않게 하는 마지막 자리다.
// inquiryNumber 는 문의 번호다 — 오류 응답의 trace id(32자리 hex) 앞 8자리(D47). 고객·상담원이 이 번호를 알려 주면 운영자가
// 로그의 trace_id 로 그 요청을 찾는다. 요청 하나를 가리키는 값이라 세션 토큰이 아니고 보여도 무엇을 열 수 없다. 모양이 아니면 빈 문자열이다.
export function inquiryNumber(traceId) {
  return typeof traceId === "string" && /^[0-9a-f]{32}$/.test(traceId) ? traceId.slice(0, 8) : "";
}

// copyButton 은 단추를 누르면 text 를 복사하고 결과를 단추 글자로 알린다.
function copyButton(btn, text) {
  btn.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(text);
      btn.textContent = "복사되었습니다";
    } catch {
      btn.textContent = "복사하지 못했습니다";
    }
  });
}

// toast 는 잠깐 뜨는 알림이다. copy 를 주면 그 글을 복사하는 단추를 달고 저절로 닫지 않는다(보내지 못한 글).
// traceId 를 주면 "문의 번호 ab12cd34" 와 번호 복사 단추를 단다. 번호를 옮겨 적을 시간을 주려고 10초 동안 띄운다. 복사는 32자리 전체다.
// action({ label, onSelect })을 주면 그 단추를 단다(지운 것 되돌리기). 누르면 알림을 닫고 onSelect 를 부른다. duration 은 저절로 닫히기까지의 밀리초다.
// 알림 칸은 화면 읽기 프로그램이 읽는 자리다(role="status", aria-live="polite"). 오류 알림도 polite 다 — 알림은 대개 사람이 하던 일과 따로 오는
// 소식(뒤에서 다시 보내다 거절됨, 저장이 막힘)이라 쓰던 글을 끊고 읽지 않는다. 그 자리에서 막는 오류(확인 창·입력 창의 오류)는 role="alert" 가 따로 있다.
// 칸 하나에 둘을 섞으면(안의 알림에 role="alert") 화면 읽기 프로그램이 같은 말을 두 번 읽는다.
// 기본 시간(TOAST_MS)과 문의 번호가 있을 때의 시간(TOAST_TRACE_MS)이다. duration 을 주면 그 시간이다.
export const TOAST_MS = 4000;
export const TOAST_TRACE_MS = 10000;
export function toast(message, kind = "", { copy, traceId, action, duration } = {}) {
  if (!toastRoot || toastRoot.parentNode !== document.body) {
    toastRoot = document.createElement("div");
    toastRoot.className = "toasts";
    toastRoot.setAttribute("role", "status");
    toastRoot.setAttribute("aria-live", "polite");
    document.body.appendChild(toastRoot);
  }
  const no = inquiryNumber(traceId);
  const el = document.createElement("div");
  el.className = `toast ${kind}`;
  el.innerHTML = `<span>${escape(message)}${no ? ` <small class="toast-trace">문의 번호 ${no}</small>` : ""}</span>`
    + `${no ? `<button type="button" class="toast-trace-copy" aria-label="문의 번호 복사">번호 복사</button>` : ""}`
    + `${action ? `<button type="button" class="toast-action">${escape(action.label)}</button>` : ""}`
    + `${copy ? `<button type="button" class="toast-copy">복사</button>` : ""}<button type="button" class="toast-close" aria-label="닫기">×</button>`;
  el.querySelector(".toast-close").addEventListener("click", () => el.remove());
  if (copy) copyButton(el.querySelector(".toast-copy"), copy);
  if (no) copyButton(el.querySelector(".toast-trace-copy"), traceId);
  if (action) {
    el.querySelector(".toast-action").addEventListener("click", () => {
      el.remove();
      action.onSelect?.();
    });
  }
  toastRoot.appendChild(el);
  if (!copy) setTimeout(() => el.remove(), duration || (no ? TOAST_TRACE_MS : TOAST_MS));
}

// clearToasts 는 떠 있는 알림을 모두 치운다. 로그아웃에서 부른다 — 앞 사람의 알림(되돌리기·보내지 못한 글의 복사)을 다음 사람이 누르면 안 된다.
export function clearToasts() {
  if (!toastRoot) return;
  for (const el of [...(toastRoot.children || [])]) el.remove();
}

// toastError 는 API 오류(api() 가 던진 것)를 알린다. 서버 문구에 문의 번호를 붙인다. prefix 가 있으면 "prefix: 문구" 다.
export function toastError(err, prefix = "") {
  const message = err?.message || "요청을 처리하지 못했습니다";
  toast(prefix ? `${prefix}: ${message}` : message, "danger", { traceId: err?.traceId });
}

// modal 은 열고 닫는 것만 맡는다. 내용은 호출자가 HTML 로 준다.
// 어느 길로 닫든(호출자의 단추·Esc·막 클릭) 한 번만 닫히고, 초점은 창을 열기 전의 요소로 돌아가며, onClose 가 한 번 불린다(DEF-157).
// 예전에는 Esc·막 클릭이 창만 지워 confirm·form 의 약속이 영원히 풀리지 않았고 초점이 문서 처음으로 떨어졌다.
// 열기 전의 요소가 그사이 문서에서 빠졌으면(지운 메시지의 메뉴 등) 초점을 옮기지 않는다.
export function modal(html, { onMount, onClose } = {}) {
  const opener = document.activeElement;
  const scrim = document.createElement("div");
  scrim.className = "scrim";
  scrim.innerHTML = `<div class="modal" role="dialog" aria-modal="true">${html}</div>`;
  let closed = false;
  const close = () => {
    if (closed) return;
    closed = true;
    document.removeEventListener("keydown", onKey);
    scrim.remove();
    if (opener && opener !== document.body && opener.isConnected !== false) opener.focus?.();
    onClose?.();
  };
  scrim.addEventListener("click", (e) => {
    if (e.target === scrim) close();
  });
  const onKey = (e) => {
    if (e.key === "Escape") close();
  };
  document.addEventListener("keydown", onKey);
  document.body.appendChild(scrim);
  onMount?.(scrim.firstElementChild, close);
  const first = scrim.querySelector("input, textarea, select, button");
  first?.focus();
  return close;
}

// confirm 은 alert/confirm 을 대신한다. 되돌리기 어려운 동작 앞에 선다.
// blocked 를 주면 지금은 할 수 없는 까닭을 창 안에 알리고(role=alert) 확인 단추를 잠근다 — 단추를 숨기지 않고 왜 못 하는지 보인다.
// 확인 단추만 true 다. 취소·Esc·막 클릭은 모두 false 로 풀린다(DEF-157).
export function confirm({ title, body = "", ok = "확인", cancel = "취소", danger = false, blocked = "" }) {
  return new Promise((resolve) => {
    let answer = false;
    modal(
      `<h2>${escape(title)}</h2>${body ? `<p>${escape(body)}</p>` : ""}
       ${blocked ? `<p class="confirm-blocked" role="alert">${escape(blocked)}</p>` : ""}
       <div class="actions">
         <button class="btn" data-act="cancel">${escape(cancel)}</button>
         <button class="btn ${danger ? "danger" : "primary"}" data-act="ok"${blocked ? " disabled" : ""}>${escape(ok)}</button>
       </div>`,
      {
        onMount(el, close) {
          el.querySelector('[data-act="ok"]').addEventListener("click", () => ((answer = true), close()));
          el.querySelector('[data-act="cancel"]').addEventListener("click", () => close());
        },
        onClose: () => resolve(answer),
      },
    );
  });
}

// form 은 입력 몇 개를 받는 모달이다. prompt 를 대신한다.
// fields: [{name, label, type, value, placeholder, hint, maxlength, counter, options:[{value,label}]}]
// validate(data) 가 문구를 돌려주면 창을 닫지 않고 그 문구를 창 안에 보여 준다. 빈 문자열이면 통과다.
// 취소·Esc·막 클릭은 null 로 풀린다(DEF-157).
export function form({ title, body = "", fields, ok = "저장", validate }) {
  return new Promise((resolve) => {
    let out = null;
    const inputs = fields
      .map((f) => {
        const id = `f-${f.name}`;
        let control;
        if (f.type === "select") {
          control = `<select id="${id}" name="${f.name}">${(f.options || [])
            .map((o) => `<option value="${escape(o.value)}" ${o.value === f.value ? "selected" : ""}>${escape(o.label)}</option>`)
            .join("")}</select>`;
        } else if (f.type === "textarea") {
          const max = f.maxlength ? ` maxlength="${Number(f.maxlength)}"` : "";
          control = `<textarea id="${id}" name="${f.name}"${max} placeholder="${escape(f.placeholder || "")}">${escape(f.value || "")}</textarea>`;
        } else {
          control = `<input id="${id}" name="${f.name}" type="${f.type || "text"}" value="${escape(f.value ?? "")}"
            placeholder="${escape(f.placeholder || "")}" ${f.required ? "required" : ""} autocomplete="off">`;
        }
        // 글자 수는 서버처럼 문자 단위로 센다.
        const counter = f.counter && f.maxlength
          ? `<div class="hint counter" data-counter="${f.name}">${[...(f.value || "")].length}/${Number(f.maxlength)}</div>` : "";
        return `<div class="field"><label for="${id}">${escape(f.label)}</label>${control}
          ${f.hint ? `<div class="hint">${escape(f.hint)}</div>` : ""}${counter}</div>`;
      })
      .join("");
    modal(
      `<h2>${escape(title)}</h2>${body ? `<p>${escape(body)}</p>` : ""}
       <form data-form>${inputs}
         <div class="form-error" role="alert" hidden></div>
         <div class="actions">
           <button type="button" class="btn" data-act="cancel">취소</button>
           <button type="submit" class="btn primary">${escape(ok)}</button>
         </div></form>`,
      {
        onMount(el, close) {
          const f = el.querySelector("[data-form]");
          const errorEl = f.querySelector(".form-error");
          for (const c of f.querySelectorAll("[data-counter]")) {
            const input = f.querySelector(`[name="${c.dataset.counter}"]`);
            const max = input.getAttribute("maxlength");
            input.addEventListener("input", () => (c.textContent = `${[...input.value].length}/${max}`));
          }
          f.addEventListener("submit", (e) => {
            e.preventDefault();
            const data = Object.fromEntries(new FormData(f).entries());
            const problem = validate?.(data);
            if (problem) {
              errorEl.textContent = problem;
              errorEl.hidden = false;
              return;
            }
            out = data;
            close();
          });
          el.querySelector('[data-act="cancel"]').addEventListener("click", () => close());
        },
        onClose: () => resolve(out),
      },
    );
  });
}

// menu 는 버튼 아래 드롭다운이다. items: [{label, onSelect, checked, danger, divider, header}]
let openMenu = null;
export function menu(anchor, items, { align = "right" } = {}) {
  closeMenu();
  const wrap = anchor.closest(".menu-wrap") || anchor.parentElement;
  const el = document.createElement("div");
  el.className = `menu ${align === "left" ? "left" : ""}`;
  el.innerHTML = items
    .map((it) => {
      if (it.divider) return "<hr>";
      if (it.header) return `<div class="label">${escape(it.header)}</div>`;
      return `<button type="button" ${it.checked ? 'aria-checked="true"' : ""} class="${it.danger ? "danger" : ""}">${escape(it.label)}</button>`;
    })
    .join("");
  const actionable = items.filter((it) => !it.divider && !it.header);
  [...el.querySelectorAll("button")].forEach((b, i) => {
    b.addEventListener("click", () => {
      closeMenu();
      // 누른 항목은 메뉴와 함께 문서에서 빠진다. 초점을 메뉴를 연 단추로 돌린다 — 항목이 띄운 창(보류 직접 입력, 메시지 수정·삭제)이
      // 닫힐 때 초점이 그 단추로 돌아온다. 안 그러면 창이 기억한 "열기 전 요소" 가 빠진 항목이라 초점이 문서 처음으로 떨어졌다(감사 27).
      if (anchor.isConnected !== false) anchor.focus?.();
      actionable[i].onSelect?.();
    });
  });
  // 메뉴는 감싼 칸 기준으로 뜬다. .menu-wrap 이 없는 자리(부모 요소)도 같은 클래스로 기준을 만든다 — 인라인 style 을 쓰지 않는다(정책 645).
  wrap.classList.add("menu-wrap");
  wrap.appendChild(el);
  fitMenu(el, wrap);
  openMenu = el;
  setTimeout(() => document.addEventListener("click", closeMenuOnOutside), 0);
  return el;
}
// fitMenu 는 메뉴가 화면 가로 밖으로 나가지 않게 펼치는 쪽을 고른다(감사 27 — 390px 에서 담당·보류 메뉴가 화면 왼쪽 밖으로 잘렸다).
// 펼치는 쪽은 둘이다: 감싼 칸의 오른쪽 끝에 맞춰 왼쪽으로(.menu 기본), 왼쪽 끝에 맞춰 오른쪽으로(.left). 고른 쪽이 화면 안에 들면 그대로 두고,
// 넘치면 덜 넘치는 쪽으로 바꾼다. 메뉴 폭은 화면 폭 - 24px 을 넘지 않으므로(.menu max-width) 감싼 칸이 화면 안에 있으면 둘 중 하나는 든다.
// 위치는 클래스로만 바꾼다(정책 645). 폭은 offsetWidth 다 — 여는 움직임(pop-in)의 확대·축소에 흔들리지 않는다.
export const MENU_EDGE = 8; // 화면 가장자리와 메뉴 사이에 남길 틈(px)
function fitMenu(el, wrap) {
  const vw = document.documentElement?.clientWidth || globalThis.innerWidth || 0;
  const width = el.offsetWidth || 0;
  if (!vw || !width || typeof wrap.getBoundingClientRect !== "function") return;
  const box = wrap.getBoundingClientRect();
  const spill = {
    right: Math.max(0, MENU_EDGE - (box.right - width)), // 오른쪽 끝에 맞추면 왼쪽으로 넘치는 양
    left: Math.max(0, box.left + width - (vw - MENU_EDGE)), // 왼쪽 끝에 맞추면 오른쪽으로 넘치는 양
  };
  const side = el.classList.contains("left") ? "left" : "right";
  const other = side === "left" ? "right" : "left";
  if (spill[side] > 0 && spill[other] < spill[side]) el.classList.toggle("left", other === "left");
}

// isMenuOpen 은 menu() 가 돌려준 그 메뉴가 아직 열려 있는지 본다. 열린 메뉴가 낡았으면 화면이 닫는다.
export const isMenuOpen = (el) => !!el && openMenu === el;
function closeMenuOnOutside(e) {
  if (openMenu && !openMenu.contains(e.target)) closeMenu();
}
export function closeMenu() {
  openMenu?.remove();
  openMenu = null;
  document.removeEventListener("click", closeMenuOnOutside);
}

export function lightbox(src, alt = "") {
  const el = document.createElement("div");
  el.className = "lightbox";
  el.innerHTML = `<img src="${src}" alt="${escape(alt)}">`;
  el.addEventListener("click", () => el.remove());
  document.body.appendChild(el);
}

export function emptyHTML(glyph, title, text = "") {
  return `<div class="empty"><div class="glyph">${glyph}</div><strong>${escape(title)}</strong>${text ? `<span>${escape(text)}</span>` : ""}</div>`;
}

export function skeletonRows(n = 5) {
  return Array.from({ length: n }, () =>
    `<div class="skel-row"><div class="skeleton avatar"></div><div class="lines">
      <div class="skeleton short"></div><div class="skeleton long"></div></div></div>`,
  ).join("");
}

// linkify 는 escape 뒤에만 부른다. 주소를 눌러 열 수 있게 한다.
export function linkify(escaped) {
  return escaped.replace(/(https?:\/\/[^\s<]+)/g, (m) => `<a href="${m}" target="_blank" rel="noopener noreferrer">${m}</a>`);
}

// ── 첨부 ─────────────────────────────────────────────────────────

// 화면에서 바로 재생하는 영상 종류다. 서버가 내용으로 판정한 값이고, 이 셋만 내려받기 주소가 inline 이다(D37).
const PLAYABLE_VIDEO = new Set(["video/mp4", "video/quicktime", "video/webm"]);

// 첨부는 인증이 필요한데 <img src>·<video src>·<a href> 는 Authorization 헤더를 못 붙인다.
// URL 에 토큰을 박으면 이미지 주소 하나로 세션이 통째로 새므로, JS 가 토큰을 실어 몇 분짜리 서명된 주소를 받아 src·href 에 꽂는다.

// attachmentHTML 은 말풍선 안의 첨부다. 위젯과 데스크가 같이 쓴다. 주소는 hydrateAttachments 가 늦게 붙인다.
// 영상은 div.attach.att-video 안의 video(controls, preload=metadata, playsinline)와 a.att-video-save(내려받기)다.
// 재생하지 못하면(코덱을 모르는 mov 등) 파일 카드(a.attach.file)로 바뀐다.
export function attachmentHTML(att) {
  if (!att) return "";
  if (PLAYABLE_VIDEO.has(att.content_type)) {
    return `<div class="attach att-video" data-attach="${att.id}" data-kind="video" data-name="${escape(att.filename)}">
      <video controls preload="metadata" playsinline aria-label="${escape(att.filename)}"></video>
      <a class="att-video-save" href="#">${escape(att.filename)} · ${humanSize(att.byte_size || 0)} 내려받기</a></div>`;
  }
  const isImage = (att.content_type || "").startsWith("image/");
  if (isImage) {
    // 사진 비율은 올린 사진마다 다른 데이터라 인라인 aspect-ratio 로 자리를 잡는다(내려받기 전에 줄이 밀리지 않게).
    // eslint-disable-next-line no-restricted-syntax -- 사진 비율(데이터). 정책 645 예외
    const ratio = att.width && att.height ? ` style="aspect-ratio:${att.width}/${att.height}"` : "";
    return `<a class="attach" data-attach="${att.id}" data-kind="image" data-name="${escape(att.filename)}" href="#">
      <img alt="${escape(att.filename)}"${ratio}></a>`;
  }
  return `<a class="attach file" data-attach="${att.id}" data-kind="file" data-name="${escape(att.filename)}" href="#">
    <span aria-hidden="true">📎</span>
    <span class="body"><span class="name">${escape(att.filename)}</span>
    <span class="size">${humanSize(att.byte_size || 0)}</span></span></a>`;
}

// 내려받기 주소는 권한 확인 뒤에 발급되고 몇 분만 살아 있다. 만료 전까지만 다시 쓴다.
const URL_TTL_MS = 4 * 60 * 1000;
const urlCache = new Map();
// fresh 면 남은 주소를 버리고 새로 받는다. 영상은 재생하는 동안 구간마다 주소를 다시 쓰므로, 만료된 뒤에는 새 주소가 필요하다.
async function attachmentURL(token, attachmentID, fresh = false) {
  const hit = urlCache.get(attachmentID);
  if (!fresh && hit && hit.until > Date.now()) return hit.url;
  const res = await fetch(`/api/attachments/${attachmentID}/url`, { headers: { Authorization: `Bearer ${token}` } });
  if (!res.ok) throw new Error(`첨부를 불러오지 못했습니다 (HTTP ${res.status})`);
  const { url } = await res.json();
  urlCache.set(attachmentID, { url, until: Date.now() + URL_TTL_MS });
  return url;
}

// saveFile 은 내려받기다. 같은 출처(로컬 저장소)면 download 속성이 이름을 붙인다. 다른 출처(S3)면 브라우저가 속성을 무시한다.
// 그때 inline 주소(영상)를 이 탭에서 열면 데스크·위젯 화면이 그 파일로 바뀌므로 inline 이면 새 탭으로 연다.
function saveFile(url, name, inline = false) {
  const a = document.createElement("a");
  a.href = url;
  a.download = name || "file";
  if (inline) {
    a.target = "_blank";
    a.rel = "noopener";
  }
  a.click();
}

// hydrateVideo 는 영상에 주소를 붙인다. 주소는 몇 분만 살아 있는데 영상은 재생·탐색 때마다 그 주소로 구간을 다시 받는다.
// 그래서 재생을 시작할 때 주소가 오래됐으면 새로 받고, 받다가 네트워크 오류가 나면 한 번 새 주소로 이어 간다.
function hydrateVideo(el, token, attachmentID) {
  const video = el.querySelector("video");
  let loadedAt = 0;
  const load = async (fresh) => {
    const resume = video.currentTime || 0;
    const wasPlaying = fresh && !video.paused;
    video.src = await attachmentURL(token, attachmentID, fresh);
    loadedAt = Date.now();
    if (resume) video.currentTime = resume;
    if (wasPlaying) video.play().catch(() => {});
  };
  // fallback 은 재생하지 못하는 영상을 파일 카드로 바꾼다. 카드를 누르면 내려받는다.
  const fallback = (text) => {
    const parent = el.parentNode;
    if (!parent) return;
    el.outerHTML = `<a class="attach file" data-attach="${attachmentID}" data-kind="file" data-inline="1" data-name="${escape(el.dataset.name || "")}" href="#">
      <span aria-hidden="true">🎞</span><span class="body"><span class="name">${escape(el.dataset.name || "영상")}</span>
      <span class="size">${escape(text)}</span></span></a>`;
    hydrateAttachments(parent, token);
  };
  let retried = false;
  video.addEventListener("play", () => {
    if (Date.now() - loadedAt > URL_TTL_MS) load(true).catch(() => {});
  });
  video.addEventListener("error", () => {
    const code = video.error?.code;
    // 2 = 네트워크 오류. 주소가 만료됐을 수 있다. 한 번만 새 주소로 다시 붙인다.
    if (code === 2 && !retried) {
      retried = true;
      load(true).catch(() => fallback("영상을 불러오지 못했습니다 · 눌러서 내려받기"));
      return;
    }
    fallback(code === 4 || code === 3 ? "이 브라우저에서 재생할 수 없습니다 · 눌러서 내려받기" : "영상을 불러오지 못했습니다 · 눌러서 내려받기");
  });
  el.querySelector(".att-video-save").addEventListener("click", async (e) => {
    e.preventDefault();
    try {
      saveFile(await attachmentURL(token, attachmentID), el.dataset.name, true);
    } catch (err) {
      toast(err.message, "danger");
    }
  });
  load(false).catch(() => fallback("영상을 불러오지 못했습니다 · 눌러서 내려받기"));
}

export function hydrateAttachments(root, token) {
  for (const el of root.querySelectorAll("[data-attach]:not([data-ready])")) {
    el.dataset.ready = "1";
    const attachmentID = el.dataset.attach;
    if (el.dataset.kind === "video") {
      hydrateVideo(el, token, attachmentID);
      continue;
    }
    if (el.dataset.kind === "image") {
      attachmentURL(token, attachmentID)
        .then((url) => {
          const img = el.querySelector("img");
          img.src = url;
          el.addEventListener("click", (e) => {
            e.preventDefault();
            lightbox(url, el.dataset.name);
          });
        })
        .catch(() => {
          el.className = "attach file";
          el.textContent = "이미지를 불러오지 못했습니다";
        });
      continue;
    }
    el.addEventListener("click", async (e) => {
      e.preventDefault();
      try {
        saveFile(await attachmentURL(token, attachmentID), el.dataset.name, el.dataset.inline === "1");
      } catch (err) {
        toast(err.message, "danger");
      }
    });
  }
}

// ── 메시지 영역 ──────────────────────────────────────────────────

// createLog 는 한 대화의 메시지 영역을 맡는다.
// mineKind 는 "CUSTOMER" 또는 "AGENT" 로, 어느 쪽 말풍선을 내 것으로 그릴지 정한다.
// onMenu(messageId, buttonEl, {mine, self}) 가 있으면 말풍선에 ⋯ 버튼이 붙는다. 수정·삭제는 쓰는 쪽이 정한다.
// dayText 는 날짜 구분선의 글자를 정한다. 데스크는 dayLabel(오늘·어제를 말로), 위젯은 "2026년 10월 06일"(widget-view.js dayStamp)이다.
// timeText 는 말풍선 아래 시각의 글자다. 데스크는 formatTime("오후 02:15"), 위젯은 24시간 "14:15"(widget-view.js clockTime)이다.
// 줄의 쪽(오른쪽 .out)과 "내 것"(.mine — 읽음 표시·⋯ 메뉴의 mine)은 따로 정한다(정책 824).
// outKinds 는 오른쪽에 붙일 보낸 쪽 목록이다. 기본은 mineKind 하나(위젯: 고객만 오른쪽). 데스크는 회사 쪽 발화(상담원·AI·자동)를 모두 오른쪽에 둔다.
// selfId 가 있으면 mineKind 줄 가운데 보낸 사람이 그 id 가 아닌 줄(동료 상담원)은 .peer 이고 이름과 아바타를 보인다. .mine 은 그대로라 메뉴와 읽음은 바뀌지 않는다.
export function createLog({ logEl, msgsEl, pendingEl, mineKind, token, onMenu, canMenu, dayText = dayLabel, timeText = formatTime, outKinds = [mineKind], selfId }) {
  let otherRead = 0;
  let lastKey = "";
  let lastDay = "";
  let unreadFrom = 0; // 처음 열 때 "여기부터 안 읽음" 을 그릴 번호
  let unreadDrawn = false;
  // 보내는 중 줄 아래의 안내(오래 끊겨 ack 를 못 받을 때). setPending 이 줄을 다시 그려도 남는다.
  let pendingNotice = "";
  let pendingList = [];
  const currentToken = () => (typeof token === "function" ? token() : token);

  const atBottom = () => logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 120;
  // pinned 는 "사람이 맨 아래를 보고 있었다" 다. 스크롤할 때만 다시 잰다.
  // 그림·영상은 줄을 그린 뒤에 내용이 와서 높이가 커진다. 그 순간에는 이미 맨 아래가 아니어서 atBottom 으로는 가를 수 없다.
  let pinned = true;
  const toEnd = () => {
    logEl.scrollTop = logEl.scrollHeight;
    pinned = true;
  };
  logEl.addEventListener?.("scroll", () => {
    pinned = atBottom();
  });
  // load·loadedmetadata 는 거품처럼 올라오지 않아서 잡기 단계(capture)로 받는다. 맨 아래에 있던 사람만 따라 내려간다.
  const follow = () => {
    if (pinned) logEl.scrollTop = logEl.scrollHeight;
  };
  logEl.addEventListener?.("load", follow, true);
  logEl.addEventListener?.("loadedmetadata", follow, true);

  // 내가 보낸 말풍선 중 상대가 읽은 마지막 하나에만 표시를 남긴다. 줄마다 붙이면 읽기 어렵다.
  function paintRead() {
    const mine = [...msgsEl.querySelectorAll(".row.mine[data-seq]")];
    let target = null;
    for (const el of mine) if (Number(el.dataset.seq) <= otherRead) target = el;
    for (const el of mine) {
      const mark = el.querySelector(".read");
      if (mark) mark.hidden = el !== target;
    }
  }

  function rowHTML(m, { pending = false, prior = false } = {}) {
    if (m.sender_kind === "SYSTEM") {
      const seqAttr = pending || prior || !m.seq ? "" : ` data-seq="${m.seq}"`;
      return `<div class="row system${prior ? " prior" : ""}"${seqAttr}><div class="bubble">${escape(m.body)}</div></div>`;
    }
    const mine = m.sender_kind === mineKind;
    const out = outKinds.includes(m.sender_kind);
    // 같은 쪽의 다른 사람(동료 상담원). 보내는 중 줄은 언제나 내 것이다. id 를 모르면(옛 줄) 내 것으로 본다.
    const me = typeof selfId === "function" ? selfId() : selfId;
    const peer = mine && !pending && !!me && !!m.sender_id && m.sender_id !== me;
    const self = mine && !peer;
    // 자동 응답(BOT)과 AI 응대(AI, D45)는 사람이 아니라는 표시를 같이 받는다. 아바타 글자만 다르다.
    const bot = m.sender_kind === "BOT" || m.sender_kind === "AI";
    const internal = m.visibility === "PRIVATE";
    const key = `${m.sender_kind}:${m.sender_id || ""}:${internal}`;
    const same = key === lastKey && !pending;
    // 보내는 중 줄은 따로 그리는 칸(pendingEl)이라 묶음 기억을 건드리지 않는다. 지우면 ack 뒤에 붙는 내 줄이 매번 새 묶음이 돼
    // 같은 분에 이어 보낸 내 줄마다 시간이 남는다.
    if (!pending) lastKey = key;

    const who = self ? "나" : m.sender_name;
    const avatar = self ? "" : avatarHTML(bot ? (m.sender_kind === "AI" ? "AI" : "자동") : m.sender_name, bot ? "bot" : "");
    const lock = internal ? `<div class="lock">🔒 내부 메모 · 고객에게 보이지 않습니다</div>` : "";
    const time = pending ? "보내는 중" : timeText(m.created_at);
    const seqMark = pending ? "" : `<span>#${m.seq}</span>`;
    const read = mine && !pending && !prior ? `<span class="read" hidden>읽음</span>` : "";
    const deleted = !!m.deleted_at;
    const edited = m.edited_at && !deleted ? `<span class="edited">수정됨</span>` : "";
    const body = deleted
      ? `<div class="bubble deleted">삭제된 메시지</div>`
      : m.body ? `<div class="bubble">${linkify(escape(m.body))}</div>` : "";
    const showMenu = onMenu && !pending && !prior && !deleted && (canMenu ? canMenu(m, mine, self) : mine);
    const menuBtn = showMenu ? `<button class="msg-menu" data-msg="${m.id}" aria-label="메시지 메뉴">⋯</button>` : "";
    const cls = [mine ? "mine" : "theirs", out ? "out" : "", peer ? "peer" : "", bot ? "bot" : "", internal ? "internal" : "", same ? "same" : "", pending ? "pending" : "", prior ? "prior" : ""]
      .filter(Boolean)
      .join(" ");

    return `<div class="row ${cls}" data-sender="${escape(m.sender_kind || "")}" ${pending || prior ? "" : `data-seq="${m.seq}" data-id="${m.id}"`}>
      ${avatar}
      <div class="stack">
        ${self ? "" : `<div class="who">${escape(who)}</div>`}
        ${lock}${body}${deleted ? "" : attachmentHTML(m.attachment)}
        <div class="meta"><span>${time}</span>${seqMark}${edited}${read}${menuBtn}</div>
      </div></div>`;
  }

  function dividersFor(m) {
    let html = "";
    const day = dayText(m.created_at);
    if (day !== lastDay) {
      lastDay = day;
      lastKey = "";
      html += `<div class="divider">${escape(day)}</div>`;
    }
    if (!unreadDrawn && unreadFrom > 0 && m.seq > unreadFrom && m.sender_kind !== mineKind) {
      unreadDrawn = true;
      lastKey = "";
      html += `<div class="divider unread">여기부터 안 읽음</div>`;
    }
    return html;
  }

  // applyRevision 은 EDIT/DELETE 이벤트를 화면의 원래 줄에 반영한다. 그 줄이 화면에 없으면(더 이전 것) 할 일이 없다.
  function applyRevision(ev) {
    const row = msgsEl.querySelector(`.row[data-id="${ev.ref_message_id}"]`);
    if (!row) return;
    const stack = row.querySelector(".stack");
    const meta = row.querySelector(".meta");
    if (ev.kind === "DELETE") {
      for (const el of stack.querySelectorAll(".bubble, .attach, .lock")) el.remove();
      meta.insertAdjacentHTML("beforebegin", `<div class="bubble deleted">삭제된 메시지</div>`);
      meta.querySelector(".msg-menu")?.remove();
      meta.querySelector(".edited")?.remove();
      return;
    }
    const bubble = stack.querySelector(".bubble");
    if (bubble) bubble.innerHTML = linkify(escape(ev.body));
    else meta.insertAdjacentHTML("beforebegin", `<div class="bubble">${linkify(escape(ev.body))}</div>`);
    if (!meta.querySelector(".edited")) meta.querySelector("span")?.insertAdjacentHTML("afterend", `<span class="edited">수정됨</span>`);
  }

  function bindMenus() {
    if (!onMenu) return;
    for (const btn of msgsEl.querySelectorAll(".msg-menu:not([data-bound])")) {
      btn.dataset.bound = "1";
      btn.addEventListener("click", (e) => {
        e.stopPropagation();
        const row = btn.closest(".row");
        onMenu(btn.dataset.msg, btn, {
          mine: row.classList.contains("mine"), self: row.classList.contains("mine") && !row.classList.contains("peer"), sender: row.dataset.sender, text: row.querySelector(".bubble")?.textContent || "",
        });
      });
    }
  }

  return {
    clear() {
      msgsEl.innerHTML = "";
      pendingEl.innerHTML = "";
      pendingList = [];
      otherRead = 0;
      lastKey = "";
      lastDay = "";
      unreadFrom = 0;
      unreadDrawn = false;
    },

    // markUnreadFrom 은 구독 직후 내 읽음 지점을 알려 준다. 그 다음 줄에 경계선이 그려진다.
    markUnreadFrom(seq) {
      unreadFrom = seq;
      unreadDrawn = false;
    },

    append(m) {
      // 수정·삭제 이벤트는 줄로 그리지 않고, 가리키는 메시지를 고친다.
      if (m.kind === "EDIT" || m.kind === "DELETE") {
        applyRevision(m);
        return;
      }
      const keep = atBottom();
      msgsEl.insertAdjacentHTML("beforeend", dividersFor(m) + rowHTML(m));
      hydrateAttachments(msgsEl, currentToken());
      bindMenus();
      paintRead();
      if (keep) toEnd();
    },

    // prependPrior 는 이 고객의 이전 상담을 지금 대화 위에 붙인다. 읽기 전용이다 —
    // 번호도 메뉴도 없는 줄이라 지금 대화의 커서·읽음·수정과 섞이지 않는다.
    // 상담이 바뀌는 자리마다 구분선을 긋고, 스크롤 위치를 지켜서 읽던 줄이 튀지 않게 한다.
    prependPrior(list) {
      if (list.length === 0) return;
      const before = logEl.scrollHeight;
      const savedKey = lastKey, savedDay = lastDay;
      const first = msgsEl.firstElementChild;
      // 이미 붙어 있는 맨 위 상담과 같은 상담이 이어지면 그 구분선은 새 묶음의 머리로 옮긴다.
      const joins = first?.classList.contains("prior-head") && first.dataset.conv === list[list.length - 1].conversation_id;
      if (joins) first.remove();
      let html = "", conv = "";
      lastKey = "";
      for (const m of list) {
        if (m.conversation_id !== conv) {
          conv = m.conversation_id;
          lastKey = "";
          html += `<div class="divider prior-head" data-conv="${conv}">이전 상담 · ${escape(dayText(m.created_at))}</div>`;
        }
        html += rowHTML(m, { prior: true });
      }
      if (!msgsEl.querySelector(".prior-end")) html += `<div class="divider prior-end">이번 상담</div>`;
      lastKey = savedKey; lastDay = savedDay;
      msgsEl.insertAdjacentHTML("afterbegin", html);
      hydrateAttachments(msgsEl, currentToken());
      logEl.scrollTop += logEl.scrollHeight - before;
    },

    setPending(list) {
      pendingList = list;
      pendingEl.innerHTML = list
        .map((p) =>
          rowHTML(
            { sender_kind: mineKind, sender_name: "나", body: p.body, visibility: p.internal ? "PRIVATE" : "PUBLIC", attachment: p.attachment },
            { pending: true },
          ),
        )
        .join("") + (pendingNotice && list.length ? `<div class="pending-notice" role="status">${escape(pendingNotice)}</div>` : "");
      hydrateAttachments(pendingEl, currentToken());
      toEnd();
    },

    // setPendingNotice 는 보내는 중 줄 아래에 상태 안내를 단다(빈 글이면 걷는다). 대기 글의 순서와 내용은 그대로다.
    setPendingNotice(text) {
      if ((text || "") === pendingNotice) return;
      pendingNotice = text || "";
      this.setPending(pendingList);
    },

    setReadFrom(readerKind, seq) {
      if (readerKind === mineKind) return;
      if (seq <= otherRead) return;
      otherRead = seq;
      paintRead();
    },

    system(text) {
      msgsEl.insertAdjacentHTML("beforeend", rowHTML({ sender_kind: "SYSTEM", body: text }));
      toEnd();
    },

    toEnd,
  };
}

export function createTyping(el, labelEl) {
  let timer = null;
  return (name) => {
    const who = name || "상대";
    labelEl.textContent = `${who}${subjectParticle(who)} 입력 중`;
    el.hidden = false;
    clearTimeout(timer);
    timer = setTimeout(() => (el.hidden = true), TYPING_EXPIRE);
  };
}

// 입력칸 높이는 쓴 글의 줄 수에 따라 정해지는 값이라 인라인 높이를 쓴다(정책 645 의 예외).
export function autoGrow(textarea, max = 160) {
  // eslint-disable-next-line no-restricted-properties -- 입력칸 자동 높이(쓴 글에 따라 정해진다). 정책 645 예외
  textarea.style.height = "auto";
  // eslint-disable-next-line no-restricted-properties -- 위와 같다
  textarea.style.height = `${Math.min(textarea.scrollHeight, max)}px`;
}

export function createNotifier() {
  let audio = null;
  const ensureAudio = () => {
    if (!audio && window.AudioContext) audio = new AudioContext();
    return audio;
  };
  return {
    async ask() {
      if (!("Notification" in window) || Notification.permission !== "default") return;
      try {
        await Notification.requestPermission();
      } catch {
        /* 사용자가 막았다 */
      }
    },
    beep() {
      const ctx = ensureAudio();
      if (!ctx) return;
      try {
        if (ctx.state === "suspended") ctx.resume();
        const osc = ctx.createOscillator();
        const gain = ctx.createGain();
        osc.connect(gain).connect(ctx.destination);
        osc.frequency.value = 880;
        gain.gain.setValueAtTime(0.0001, ctx.currentTime);
        gain.gain.exponentialRampToValueAtTime(0.08, ctx.currentTime + 0.02);
        gain.gain.exponentialRampToValueAtTime(0.0001, ctx.currentTime + 0.22);
        osc.start();
        osc.stop(ctx.currentTime + 0.24);
      } catch {
        /* 소리는 없어도 된다 */
      }
    },
    show(title, body) {
      this.beep();
      if (!("Notification" in window) || Notification.permission !== "granted") return;
      if (document.visibilityState === "visible") return;
      try {
        new Notification(title, { body, tag: "go-chat" });
      } catch {
        /* 알림은 없어도 된다 */
      }
    },
  };
}

export function dropzone(el, onFiles) {
  let depth = 0;
  el.addEventListener("dragenter", (e) => {
    e.preventDefault();
    depth += 1;
    el.classList.add("over");
  });
  el.addEventListener("dragover", (e) => e.preventDefault());
  el.addEventListener("dragleave", () => {
    depth = Math.max(0, depth - 1);
    if (depth === 0) el.classList.remove("over");
  });
  el.addEventListener("drop", (e) => {
    e.preventDefault();
    depth = 0;
    el.classList.remove("over");
    const files = [...(e.dataTransfer?.files || [])];
    if (files.length) onFiles(files);
  });
}

// ── 관리자 색 ────────────────────────────────────────────────────

export const isBrandColor = (hex) => /^#[0-9a-f]{6}$/i.test(hex || "");
const rgbOf = (hex) => {
  const n = parseInt(hex.slice(1), 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
};
const hexOf = (rgb) => `#${rgb.map((v) => v.toString(16).padStart(2, "0")).join("")}`;

// luminance 는 WCAG 2 의 상대 휘도다(0 검정 ~ 1 흰색).
function luminance(hex) {
  const [r, g, b] = rgbOf(hex).map((v) => {
    const c = v / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

// contrastRatio 는 두 색의 WCAG 대비(1~21)다. 본문 크기 글자는 4.5 이상이어야 읽힌다.
export function contrastRatio(a, b) {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

// 관리자 색 위의 글자·아이콘 색. 흰 글자와 어두운 글자 가운데 바탕(관리자 색과 그라데이션의 어두운 끝) 위에서 더 잘 읽히는 쪽이다.
export const BRAND_INK_LIGHT = "#ffffff";
export const BRAND_INK_DARK = "#111111";

// brandTokens 는 관리자 색 하나에서 화면이 쓰는 토큰을 만든다. 색이 아니면 null 이다.
//   strong  그라데이션의 어두운 끝. 흰 글자면 색을 18% 낮추고, 어두운 글자면 8% 만 낮춘다 —
//           어두운 글자는 바탕이 어두워질수록 안 보이므로 그라데이션 끝에서도 대비가 남게 한다.
//   ink     머리·히어로·주 버튼·보내기 단추·고객 말풍선 글자와 아이콘(currentColor) 색.
//           글자는 그라데이션의 두 끝 모두에 얹힌다. 그래서 흰 글자와 어두운 글자 각각 두 끝 위 대비의 작은 값을 재고 그 값이 큰 쪽을 고른다.
//           관리자 색 위 대비만 보면 #2b8a3e 처럼 흰 글자가 4.37:1 인 색에서 어두운 글자를 골라 어두운 끝에서 3.76:1 로 떨어진다.
export function brandTokens(hex) {
  if (!isBrandColor(hex)) return null;
  const rgb = rgbOf(hex);
  const option = (ink, factor) => {
    const strong = hexOf(rgb.map((v) => Math.max(0, Math.round(v * factor))));
    return { ink, strong, worst: Math.min(contrastRatio(ink, hex), contrastRatio(ink, strong)) };
  };
  const light = option(BRAND_INK_LIGHT, 0.82);
  const dark = option(BRAND_INK_DARK, 0.92);
  const pick = light.worst >= dark.worst ? light : dark;
  // text 는 흰 바탕 위에 관리자 색으로 쓰는 글자·평면 아이콘 색이다(D41). 흰 바탕 대비 4.5:1 이 될 때까지 5%씩 낮춘다.
  // 노란색처럼 밝은 관리자 색을 흰 바탕의 글자·아이콘에 그대로 쓰면 읽히지 않는다. 충분히 진한 색은 그대로다.
  let text = hex.toLowerCase();
  for (let f = 0.95; contrastRatio(text, "#ffffff") < 4.5 && f > 0; f -= 0.05) text = hexOf(rgb.map((v) => Math.round(v * f)));
  return { brand: hex.toLowerCase(), strong: pick.strong, soft: `rgba(${rgb[0]} ${rgb[1]} ${rgb[2]} / .12)`, ink: pick.ink, text };
}

// applyBrand 는 설정의 색을 토큰에 반영한다. 글자색(--brand-ink)도 같이 정한다 — 노란색 같은 밝은 색에 흰 글자를 얹지 않는다.
export function applyBrand(hex) {
  const t = brandTokens(hex);
  if (!t) return;
  // 관리자 색은 실행 중에 정해지는 데이터라 html 의 CSS 변수로만 쓴다(정책 645 의 예외). 변수 이름은 --brand 로 시작하는 다섯뿐이다.
  for (const [name, value] of [["--brand", t.brand], ["--brand-strong", t.strong], ["--brand-soft", t.soft], ["--brand-ink", t.ink], ["--brand-text", t.text]]) {
    // eslint-disable-next-line no-restricted-properties -- 관리자 색 CSS 변수(데이터). 정책 645 예외 목록에 있다
    document.documentElement.style.setProperty(name, value);
  }
}

// ── 업로드와 거절된 글 ───────────────────────────────────────────

// createUploadGuard 는 작성칸의 업로드 하나를 그 업로드를 시작한 대화에 묶는다.
// 다른 대화로 옮기거나 취소하면 업로드를 멈추고(abort), 늦게 끝난 결과는 지금 작성칸에 붙이지 않는다.
// 남의 대화 첨부를 지금 대화로 보내면 서버가 거절하고, 그 메시지에 쓴 글까지 같이 사라진다.
export function createUploadGuard() {
  let active = null;
  return {
    // begin 은 새 업로드의 표를 준다. 앞선 업로드는 멈춘다.
    begin(convId) {
      this.cancel();
      active = { convId, controller: new AbortController() };
      active.signal = active.controller.signal;
      return active;
    },
    // owns 는 그 표가 아직 살아 있고 지금 대화(convId)의 것인지 본다. 아니면 결과를 버린다.
    owns(ticket, convId) {
      return !!ticket && ticket === active && !ticket.signal.aborted && ticket.convId === convId;
    },
    finish(ticket) {
      if (ticket === active) active = null;
    },
    // cancel 은 올리는 중인 업로드를 멈춘다. 멈춘 것이 있으면 true.
    cancel() {
      if (!active) return false;
      active.controller.abort();
      active = null;
      return true;
    },
    get busy() {
      return !!active;
    },
  };
}

// createSentDrafts 는 보낸 글을 client_msg_id 로 기억한다(최근 limit 개).
// 서버가 영구 거절하면 chat.js 는 큐에서 지우고 onError(문구, client_msg_id) 로 알린다. 그때 글을 꺼내 작성칸에 돌려준다.
// 쓴 글이 화면에서 사라지면 그게 유실이다.
export function createSentDrafts(limit = 200) {
  const sent = new Map();
  const put = (convId, p) => {
    if (!convId || !p?.client_msg_id || sent.has(p.client_msg_id)) return;
    sent.set(p.client_msg_id, { convId, body: p.body || "" });
    while (sent.size > limit) sent.delete(sent.keys().next().value);
  };
  return {
    remember: (convId, item) => put(convId, item),
    rememberAll: (convId, list) => (list || []).forEach((p) => put(convId, p)),
    // take 는 거절된 글을 꺼낸다. isQueued(convId, id) 가 참이면(일시 오류 알림이라 큐에 남아 다시 나간다) 꺼내지 않는다.
    take(clientMsgId, isQueued = () => false) {
      const hit = sent.get(clientMsgId);
      if (!hit || isQueued(hit.convId, clientMsgId)) return null;
      sent.delete(clientMsgId);
      return hit;
    },
    // settle 은 chat.js 의 onError(문구, client_msg_id, 버린 항목) 하나를 보고 쓴 글을 어떻게 할지 정한다.
    //   { back: { convId, body } }  큐에서 지운 글이다. 화면이 돌려준다. 버린 항목이 오면 그것이 글이다 —
    //                               새로고침 뒤 재전송처럼 화면이 기억하지 못한 글도 사라지지 않는다.
    //   { quiet: true }            지금 열린 대화(openConvId)가 아닌 대화의 큐에 아직 남은 글의 알림이다. 그 글은 다시 나가고,
    //                               거절되면 그때 글과 함께 다시 온다. 지금 까닭만 띄우면 같은 거절이 두 번 뜬다.
    //   {}                         글과 상관없는 알림이거나 지금 대화의 큐에 남은 글(상한 전의 명시적 거절)이다. 까닭만 보인다.
    //                               일시 오류는 chat.js 가 알리지 않아 여기 오지 않는다(DEF-01).
    //                               첨부가 실려 있었으면 back.attachment 와 거절 code 도 온다(attachmentGiveBack 이 쓴다).
    settle(clientMsgId, dropped, isQueued = () => false, openConvId = null) {
      if (dropped?.conversation_id) {
        sent.delete(dropped.client_msg_id || clientMsgId);
        return { back: { convId: dropped.conversation_id, body: dropped.body || "", attachment: dropped.attachment || null, code: dropped.code || "" } };
      }
      if (!clientMsgId) return {};
      const hit = sent.get(clientMsgId);
      if (hit && hit.convId !== openConvId && isQueued(hit.convId, clientMsgId)) return { quiet: true };
      const back = this.take(clientMsgId, isQueued);
      return back ? { back } : {};
    },
  };
}

// attachmentGiveBack 은 돌려받은 글에 실려 있던 첨부를 어떻게 할지 정한다(감사 9, U282 — 글만 돌려주고 사진을 조용히 잃었다).
//   "reattach"  아직 보내지지 않은 업로드다 — 영구 거절이 아닌 거절(재시도 상한)로 포기했고, 그 대화가 열려 있고, 작성칸 첨부 자리가 비었다.
//               작성칸에 다시 붙인다.
//   "lost"      다시 붙일 수 없다 — 영구 거절(첨부를 찾을 수 없거나 이미 보낸 첨부, 종료·없는 대화)이거나 다른 대화거나 자리가 찼다.
//               화면이 파일 이름을 들어 "다시 올려 달라" 고 알린다.
//   ""          첨부가 없다.
export function attachmentGiveBack(back, { shown = null, slotFree = false } = {}) {
  if (!back?.attachment) return "";
  if (isPermanentReject(back.code)) return "lost";
  return back.convId && back.convId === shown && slotFree ? "reattach" : "lost";
}

// restoreDraft 는 입력칸이 비어 있을 때만 글을 되돌려 넣는다. 그 사이 새로 쓰던 글을 덮지 않는다. 되돌렸으면 true.
// mergeConversation 은 실시간으로 온 대화(프레임)를 화면이 쥔 대화에 합친다. 서버는 빈 값을 싣지 않는 필드(omitempty)가 있어
// 그냥 덮으면 해제된 담당자·풀린 보류가 앞 값으로 남는다. 프레임은 대화 전체라 그 필드가 없으면 빈 값이다.
// 화면만 쓰는 필드(readOnly 등)는 그대로 둔다.
export const CONVERSATION_OMITTED = Object.freeze({
  assignee_agent_id: "", assignee_name: "", snoozed_until: null, deleted_at: null, last_body: "", last_sender_kind: "", closed_by: "",
});
// tags 는 예외다(DEF-83). 서버가 태그를 읽지 못하면 tags 를 싣지 않는다 — "태그 없음"(빈 목록)이 아니라 "모름" 이라 가진 태그를 둔다.
export function mergeConversation(prev, next) {
  const out = { ...prev, ...CONVERSATION_OMITTED, ...next };
  if (next?.tags == null && prev?.tags) out.tags = prev.tags;
  return out;
}

export function restoreDraft(input, body) {
  if (!body || String(input.value || "").trim()) return false;
  input.value = body;
  return true;
}

// clip 은 긴 글을 안내 문구에 넣을 만큼 자른다.
export const clip = (text, n = 40) => {
  const chars = [...String(text || "")];
  return chars.length > n ? `${chars.slice(0, n).join("")}…` : chars.join("");
};

// 데스크 작성칸의 업로드 표시. 파일 이름은 사용자가 정한 글자라 이스케이프한다.
export function uploadingPillHTML(name) {
  return `<span class="attach-pill"><span class="attach-pill-name">${escape(name)}</span> 올리는 중… <span class="upload-progress">0%</span>
    <button type="button" id="cancelUpload" aria-label="업로드 취소">취소</button></span>`;
}
export function readyPillHTML(att) {
  return `<span class="attach-pill">📎 <span class="attach-pill-name">${escape(att.filename)}</span> · ${humanSize(att.byte_size || 0)}
    <button type="button" id="dropAttach" aria-label="첨부 취소">×</button></span>`;
}

// ── 위젯 FAQ(D36) ────────────────────────────────────────────────

// FAQ_LIMITS 는 서버(app/manage.go)와 같은 한도다. 화면은 먼저 알려 줄 뿐이고 막는 것은 서버다.
export const FAQ_LIMITS = { items: 10, question: 80, answer: 500 };

// 홈에는 앞의 몇 개만 알약으로 보인다. 전체는 "전체보기" 의 자주 묻는 질문 화면에 있다(D44).
export const FAQ_HOME_LIMIT = 4;

// faqHTML 은 위젯의 자주 묻는 질문 화면(#faqList)이다. 관리자가 정한 순서 그대로 아코디언 한 줄씩 그린다.
// 단추는 aria-expanded 로 열림을, aria-controls 로 답 칸을 알린다. 답 칸은 처음에 닫혀 있다(hidden).
// 질문·답은 관리자가 적은 글이라 HTML 로 해석하지 않고 글자 그대로 그린다(이스케이프). 답의 주소만 escape 뒤에 링크로 바꾼다.
export function faqHTML(items) {
  return (items || [])
    .map((f, i) => `<div class="accordion-item">
  <h3><button type="button" class="accordion-trigger" id="wFaqQ${i}" aria-expanded="false" aria-controls="wFaqA${i}" data-i="${i}">
    <span class="accordion-mark" aria-hidden="true">Q</span><span class="accordion-title">${escape(f.question)}</span></button></h3>
  <div class="accordion-panel" id="wFaqA${i}" role="region" aria-labelledby="wFaqQ${i}" hidden>${linkify(escape(f.answer))}</div></div>`)
    .join("");
}

// faqPillsHTML 은 홈의 자주 묻는 질문 알약이다. 앞의 limit 개만, 누르면 그 질문을 펼친 자주 묻는 질문 화면이 열린다(data-i).
// id(wFaqPill{i})는 그 화면에서 뒤로 돌아올 때 초점을 돌려줄 자리다. 좁은 화면에서 말줄임표로 잘려도 title 로 전체 질문을 볼 수 있다.
export function faqPillsHTML(items, limit = FAQ_HOME_LIMIT) {
  return (items || [])
    .slice(0, limit)
    .map((f, i) => `<button type="button" class="pill faq-pill" id="wFaqPill${i}" data-i="${i}" title="${escape(f.question)}"><span>${escape(f.question)}</span></button>`)
    .join("");
}

// faqEditorRowHTML 은 데스크 FAQ 편집 화면의 항목 하나다. 질문·답은 관리자가 적은 글이라 value 와 textarea 안에서도 이스케이프한다.
export function faqEditorRowHTML(it, i, count) {
  return `<div class="faq-item" data-i="${i}">
      <div class="faq-item-head"><strong class="muted small">${i + 1}</strong><span class="spacer"></span>
        <button type="button" class="icon-btn" data-move="-1" aria-label="위로" ${i === 0 ? "disabled" : ""}>↑</button>
        <button type="button" class="icon-btn" data-move="1" aria-label="아래로" ${i === count - 1 ? "disabled" : ""}>↓</button>
        <button type="button" class="icon-btn" data-remove aria-label="삭제">×</button></div>
      <div class="field"><label for="faqQ${i}">질문</label><input id="faqQ${i}" class="input" data-k="question" value="${escape(it.question ?? "")}" maxlength="${FAQ_LIMITS.question}"></div>
      <div class="field"><label for="faqA${i}">답</label><textarea id="faqA${i}" data-k="answer" maxlength="${FAQ_LIMITS.answer}">${escape(it.answer ?? "")}</textarea></div></div>`;
}

// validateFaq 는 서버와 같은 규칙과 순서로 첫 문제를 말한다. 문제가 없으면 빈 문자열이다.
// 개수를 먼저 보고, 그다음 항목을 입력 순서대로 질문·답 순으로 본다. 앞뒤 공백을 떼고 글자(코드 포인트)로 센다.
export function validateFaq(items) {
  const list = items || [];
  if (list.length > FAQ_LIMITS.items) return `자주 묻는 질문은 ${FAQ_LIMITS.items}개까지입니다`;
  for (const [i, it] of list.entries()) {
    const n = i + 1;
    const q = String(it.question ?? "").trim();
    const a = String(it.answer ?? "").trim();
    if (!q) return `${n}번째 질문이 비어 있습니다`;
    if ([...q].length > FAQ_LIMITS.question) return `${n}번째 질문은 ${FAQ_LIMITS.question}자까지입니다`;
    if (!a) return `${n}번째 답이 비어 있습니다`;
    if ([...a].length > FAQ_LIMITS.answer) return `${n}번째 답은 ${FAQ_LIMITS.answer}자까지입니다`;
  }
  return "";
}

// moveItem 은 index 의 항목을 delta(-1 위, +1 아래)만큼 옮긴 새 배열이다. 이웃과 자리만 바꾼다.
// 끝을 넘는 이동은 아무것도 바꾸지 않는다. 받은 배열은 고치지 않는다.
export function moveItem(list, index, delta) {
  const out = (list || []).slice();
  const to = index + delta;
  if (index < 0 || index >= out.length || to < 0 || to >= out.length) return out;
  [out[index], out[to]] = [out[to], out[index]];
  return out;
}

// ── 상담 지표와 만족도(D45) ─────────────────────────────────────

// RATING_COMMENT_MAX 는 한 줄 의견의 글자 수 상한이다. 서버(app.MaxRatingComment)와 같다. 막는 것은 서버다.
export const RATING_COMMENT_MAX = 200;
// RATING_LABELS 는 별점마다 붙는 말이다(1점부터).
export const RATING_LABELS = ["매우 불만족", "불만족", "보통", "만족", "매우 만족"];
// HANDLER_LABEL 은 처리 주체의 이름이다.
export const HANDLER_LABEL = { AGENT: "상담원", AI: "AI 응대", UNANSWERED: "미응답", SELF: "고객 자체 해결" };

// starsHTML 은 읽기 전용 별점이다. 채운 별(★)과 빈 별(☆)을 따로 칠하고, 화면 읽기에는 "5점 만점에 4점" 한 줄로 읽힌다.
export function starsHTML(score) {
  const n = Math.max(0, Math.min(5, Math.round(Number(score) || 0)));
  return `<span class="stars" role="img" aria-label="5점 만점에 ${n}점"><span class="stars-on" aria-hidden="true">${"★".repeat(n)}</span><span class="stars-off" aria-hidden="true">${"☆".repeat(5 - n)}</span></span>`;
}

// durationText 는 응답 시간(밀리초)을 사람이 읽는 글로 쓴다. 값이 없으면(null) "—" 다 — 0초와 "없음" 을 가른다.
export function durationText(ms) {
  if (ms === null || ms === undefined || !Number.isFinite(Number(ms))) return "—";
  const s = Math.round(Number(ms) / 1000);
  if (Number(ms) < 1000) return "1초 미만";
  if (s < 60) return `${s}초`;
  const m = Math.floor(s / 60);
  if (m < 60) return s % 60 ? `${m}분 ${s % 60}초` : `${m}분`;
  const h = Math.floor(m / 60);
  if (h < 24) return m % 60 ? `${h}시간 ${m % 60}분` : `${h}시간`;
  const d = Math.floor(h / 24);
  return h % 24 ? `${d}일 ${h % 24}시간` : `${d}일`;
}

// percentText 는 비율(0~1)을 백분율로 쓴다. 소수 첫째 자리까지, 끝의 .0 은 뗀다. 값이 없으면 "—".
export function percentText(ratio) {
  if (ratio === null || ratio === undefined || !Number.isFinite(Number(ratio))) return "—";
  return `${(Math.round(Number(ratio) * 1000) / 10).toString()}%`;
}

// scoreText 는 평균 점수다. 소수 첫째 자리까지. 값이 없으면 "—".
export function scoreText(avg) {
  if (avg === null || avg === undefined || !Number.isFinite(Number(avg))) return "—";
  return `${(Math.round(Number(avg) * 10) / 10).toFixed(1)}점`;
}

// ratingSummaryHTML 은 데스크 대화 옆 칸의 만족도다. 평가가 없고 지금 종료도 아니면 빈 글자(칸을 숨긴다)다.
// 다시 열었다 닫은 뒤의 앞 평가는 "지난 종료의 평가" 로 표시한다. 의견은 고객이 쓴 글이라 이스케이프한다.
export function ratingSummaryHTML(view) {
  if (!view) return "";
  const r = view.rating;
  if (!r) {
    if (!view.closed) return "";
    return `<p class="muted small">아직 평가가 없습니다. 종료 뒤 24시간까지 받습니다.</p>`;
  }
  const who = [HANDLER_LABEL[r.handler] || "", r.agent_name ? `담당 ${escape(r.agent_name)}` : ""].filter(Boolean).join(" · ");
  return `<div class="rating-summary">
    <div class="rating-summary-score">${starsHTML(r.score)}<strong>${r.score}점</strong><span class="muted small">${escape(RATING_LABELS[r.score - 1] || "")}</span></div>
    ${r.comment ? `<p class="rating-summary-comment">${escape(r.comment)}</p>` : ""}
    <p class="muted small">${[who, formatDateTime(r.updated_at), view.current ? "" : "지난 종료의 평가"].filter(Boolean).join(" · ")}</p></div>`;
}

// ratingCardHTML 은 위젯 대화 안의 만족도 카드다(D45). 종료된 상담에서만 보이고, 무엇을 그릴지는 서버의 상태(view)와 고르는 중인 값(draft)이 정한다.
//   - 지금 종료가 아니면(진행 중이거나 다시 열렸다) 빈 글자 — 카드를 숨긴다.
//   - 이번 종료에 낸 평가가 있고 고치는 중이 아니면 고맙다는 말과 낸 별점·의견. 고칠 수 있는 동안은 "수정하기" 가 붙는다.
//   - 낼 수 있으면(종료 뒤 24시간 안) 별 다섯(라디오 묶음)·한 줄 의견·보내기. 별을 고르기 전에는 보내기가 잠겨 있다.
//   - 시간이 지났고 이번 종료의 평가가 없으면 빈 글자다.
// 별 단추는 data-score, 수정하기는 data-action="edit" 이다. 위젯이 카드 하나에 이벤트를 걸고 이 표식으로 가른다.
export function ratingCardHTML(view, draft = {}) {
  if (!view || !view.closed) return "";
  const mine = view.current ? view.rating : null;
  if (mine && !draft.editing) {
    return `<section class="rating-card" aria-labelledby="csatTitle">
      <h3 id="csatTitle" class="rating-title">평가해 주셔서 감사합니다</h3>
      <div class="rating-given">${starsHTML(mine.score)}<span class="rating-word">${escape(RATING_LABELS[mine.score - 1] || "")}</span></div>
      ${mine.comment ? `<p class="rating-comment">${escape(mine.comment)}</p>` : ""}
      ${view.can_rate ? `<button type="button" class="btn sm" data-action="edit">수정하기</button>` : ""}
    </section>`;
  }
  if (!view.can_rate) return "";
  const score = Number(draft.score) || 0;
  const stars = [1, 2, 3, 4, 5]
    .map((n) => `<button type="button" class="rating-star" role="radio" id="csatStar${n}" data-score="${n}" aria-checked="${n === score}" aria-label="${n}점 ${RATING_LABELS[n - 1]}" tabindex="${n === (score || 1) ? 0 : -1}"><span aria-hidden="true">${n <= score ? "★" : "☆"}</span></button>`)
    .join("");
  return `<section class="rating-card" aria-labelledby="csatTitle">
    <h3 id="csatTitle" class="rating-title">상담은 어떠셨나요?</h3>
    <p class="rating-lead">상담 만족도를 남겨 주시면 더 나은 상담에 쓰겠습니다.</p>
    <form id="csatForm" class="rating-form">
      <div class="rating-stars" role="radiogroup" aria-label="별점">${stars}</div>
      <p class="rating-word" aria-live="polite">${score ? escape(RATING_LABELS[score - 1]) : "별을 눌러 주세요"}</p>
      <label class="visually-hidden" for="csatComment">한 줄 의견 (선택)</label>
      <input class="input" id="csatComment" maxlength="${RATING_COMMENT_MAX}" placeholder="한 줄 의견 (선택, ${RATING_COMMENT_MAX}자까지)" value="${escape(draft.comment ?? "")}" autocomplete="off">
      <button type="submit" class="btn primary block" id="csatSubmit"${score ? "" : " disabled"}>평가 보내기</button>
      <p class="rating-note">상담 종료 뒤 24시간 안에는 고칠 수 있습니다.</p>
    </form>
  </section>`;
}

// METRIC_RANGES 는 지표 화면의 기간 단추다.
export const METRIC_RANGES = [["today", "오늘"], ["7d", "7일"], ["30d", "30일"], ["custom", "직접"]];

// metricsWindow 는 기간 단추를 [from, to) 로 바꾼다. 날짜는 이 기기의 시간대다.
//   오늘 = 오늘 0시 ~ 지금, 7일 = 6일 전 0시 ~ 지금(오늘 포함 7일), 30일 = 29일 전 0시 ~ 지금,
//   직접 = 시작일 0시 ~ 종료일 다음 날 0시(종료일 하루를 다 넣는다). 직접 날짜가 비었거나 거꾸로면 null.
export function metricsWindow(kind, { fromDate = "", toDate = "", now = new Date() } = {}) {
  const midnight = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate());
  const daysBack = { today: 0, "7d": 6, "30d": 29 }[kind];
  if (daysBack !== undefined) {
    const from = midnight(now);
    from.setDate(from.getDate() - daysBack);
    return { from, to: new Date(now) };
  }
  const parse = (v) => {
    const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(v || "");
    return m ? new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3])) : null;
  };
  const from = parse(fromDate);
  const last = parse(toDate);
  if (!from || !last || last < from) return null;
  const to = new Date(last);
  to.setDate(to.getDate() + 1);
  return { from, to };
}

// metricCards 는 지표 화면 위쪽 카드의 글자다. 값이 없으면 "—" 와 그 까닭을 쓴다 — 0 으로 보이게 하지 않는다.
// AI 처리율은 상담원 없이 끝난 비율이다 — 분자에 AI 응대와 고객 자체 해결(답 없이 고객이 닫았다, D48)이 든다.
// AI 응대가 한 번도 없었고 고객 자체 해결도 없으면 "—" 이고 "AI 응대가 아직 없습니다" 다(0% 로 읽히지 않게).
// 고객이 직접 닫은 상담 수(누가 닫았나)는 처리 주체와 다른 축이라 카드를 따로 둔다.
export function metricCards(m) {
  const closed = m?.closed || { total: 0, agent: 0, ai: 0, unanswered: 0, self: 0, by_customer: 0 };
  const sat = m?.satisfaction || {};
  const fr = m?.first_response || {};
  const rs = m?.response || {};
  const self = closed.self || 0;
  const handled = (closed.ai || 0) + self + (closed.agent || 0);
  const shown = !!m?.ai_active || self > 0;
  let aiSub;
  if (!shown) aiSub = "AI 응대가 아직 없습니다. 도입하면 상담원 개입 없이 끝난 비율이 보입니다";
  else if (m.ai_rate === null || m.ai_rate === undefined) aiSub = "이 기간에 상담원·AI 가 처리한 종료 상담이 없습니다";
  else aiSub = `AI ${closed.ai || 0}건 · 고객 자체 해결 ${self}건 / 처리 ${handled}건`;
  const timeSub = (d) => (d.count ? `중앙값 ${durationText(d.p50_ms)} · p90 ${durationText(d.p90_ms)} · ${d.count}건` : "이 기간에 응답이 없습니다");
  return [
    { key: "ai", label: "AI 처리율", value: shown ? percentText(m.ai_rate) : "—", sub: aiSub },
    { key: "first", label: "첫 응답 시간(평균)", value: durationText(fr.avg_ms), sub: timeSub(fr) },
    { key: "response", label: "평균 응답 시간", value: durationText(rs.avg_ms), sub: timeSub(rs) },
    {
      key: "csat", label: "만족도(CSAT)", value: percentText(sat.csat),
      sub: sat.responses
        ? `평균 ${scoreText(sat.average)} · 응답 ${sat.responses}건 · 응답률 ${percentText(m.response_rate)}`
        : `아직 평가가 없습니다${m?.response_rate === null || m?.response_rate === undefined ? "" : ` · 응답률 ${percentText(m.response_rate)}`}`,
    },
    { key: "closed", label: "종료한 상담", value: `${closed.total || 0}건`, sub: "", split: closed },
    {
      key: "by_customer", label: "고객이 직접 종료", value: `${closed.by_customer || 0}건`,
      sub: closed.by_customer ? `그중 답 없이 닫은 고객 자체 해결 ${self}건` : "이 기간에 고객이 닫은 상담이 없습니다",
    },
  ];
}
