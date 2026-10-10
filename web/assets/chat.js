// 클라이언트 쪽 유실 방지. 서버만 안전해도 화면에서는 메시지가 사라진다 (docs/design.md D5).
//
// 두 가지를 localStorage 에 둔다.
//   pending  보냈지만 ack 를 못 받은 메시지. 재접속마다 같은 client_msg_id 로 다시 보낸다
//   lastSeq  받은 마지막 seq. 재접속 때 after_seq 로 보내 그 이후만 받는다
// 탭을 닫아도 둘 다 살아남는다. pending 은 글마다 키가 따로라 같은 브라우저의 두 탭이 서로의 글을 덮어쓰지 않는다(DEF-158, 아래 저장 형식).
// 글 키는 커서와 이름 공간이 다르다(gcs.q1., DEF-165) — 되돌려 배포한 옛 코드의 커서 정리가 글을 지우지 않게.
//
// 읽음과 타이핑은 보장 수준이 다르다.
//   읽음   서버 DB 에 남고 단조 증가한다. 끊기면 재접속 때 ready 프레임으로 맞춰진다
//   타이핑 아무 데도 안 남는다. 못 받으면 그냥 안 보일 뿐이다
//
// 비동기 경로는 모두 시작 시점의 대화 id 를 붙잡고, 끝날 때 대화가 바뀌었으면 결과를 버린다.
// 안 그러면 늦게 온 응답이 지금 보고 있는 다른 대화의 커서를 덮어써서 과거 메시지를 영영 가린다.

const STORE_PREFIX = "gcs.v1.";
// 전송 대기 큐는 주체(상담원·고객)마다 따로 둔다. 같은 브라우저에서 다른 계정으로 들어오면
// 남의 큐를 내 이름으로 보내거나(같은 워크스페이스) 404 로 지워 버린다(다른 워크스페이스).
// owner 가 없으면 옛 방식(대화 id 만)으로 동작한다. 해시를 쓰지 않는다 — 주체 둘이 같은 키를 나눠 쓰는 일이 아예 없어야 한다.
function storePrefix(owner) {
  return owner ? `${STORE_PREFIX}${String(owner).replace(/[^a-zA-Z0-9:_-]/g, "_")}.` : STORE_PREFIX;
}
// WebSocket 이 열려 있어도 이만큼 ack 가 없으면 REST 로도 민다. 소켓은 멀쩡한데 서버가 거절한 메시지를 위해서다.
const REST_RETRY_AFTER = 6000;
const TYPING_SEND_INTERVAL = 1200; // 입력 중 신호를 보내는 최소 간격
export const TYPING_EXPIRE = 4000; // 신호가 끊기면 이만큼 뒤에 표시를 지운다
export const MAX_BODY_RUNES = 4000; // 서버와 같은 한도. 넘기면 보내기 전에 막는다

// 반쯤 죽은 연결 감지. 네트워크가 끊기면 소켓은 OPEN 으로 남고 onclose 가 오지 않는다.
// 그동안 화면은 "연결됨"인데 아무것도 못 받는다. 그래서 조용한 시간을 직접 잰다.
const LIVENESS_TICK = 2000;
const QUIET_BEFORE_PING = 6000;
const QUIET_BEFORE_DROP = 14000;
// 숨긴 탭(DEF-102). 브라우저는 숨긴 탭의 타이머를 묶어(Chrome 은 1분 간격) 생존 확인이 늦게 돈다. 그러면 "조용한 시간" 이 우리가
// 재지 못한 시간까지 쌓여 멀쩡한 연결을 1분마다 끊었다. 숨긴 동안이나 확인이 이만큼 늦게 돌았으면 조용한 시간으로 판정하지 않고
// ping 을 보내 답을 본다 — 보낸 ping 뒤로 PING_REPLY_WAIT 넘게 아무것도 못 받았을 때만 끊는다. 메시지 이벤트는 숨긴 탭에서도 곧바로 온다.
export const LIVENESS_LATE = LIVENESS_TICK * 3;
export const PING_REPLY_WAIT = QUIET_BEFORE_DROP - QUIET_BEFORE_PING;

// 서버가 배포·축소로 내려가며 보내는 close 코드(going away). 다른 서버가 받아 주므로 백오프 없이 다시 붙는다.
// 한 서버의 연결이 같은 순간에 몰리지 않게 지터만 둔다 (docs/design.md D32).
export const GOING_AWAY = 1001;
export const GOING_AWAY_JITTER = 250;

// REST 로 다시 미는 간격은 대화마다 따로 센다(D5, 감사 10). 한 대화가 막혀도 다른 대화는 제 차례에 나간다.
//   서버가 명시적으로 거절했다(영구 거절이 아닌 4xx) → REST_TICK 뒤에 다시. 상한(20번)에 센다
//   일시 실패(응답 없음·5xx·429)                   → 3초에서 두 배씩 30초까지(지터). 429 에 Retry-After 가 있으면 그만큼
//   연결이 돌아왔다(online·소켓 연결)               → 기다리던 간격을 버리고 다음 확인(REST_CHECK)에 곧바로
// 예전에는 3초 고정이라 3분 끊기면 연결 시도가 60번 넘게 나갔고 429 의 Retry-After 를 무시했다.
const REST_TICK = 3000;
const REST_CHECK = 1000; // 다시 밀 차례가 된 대화가 있나 보는 주기. 요청은 차례가 된 대화만 나간다
export const REST_BACKOFF_BASE = 3000;
export const REST_BACKOFF_MAX = 30000;
const BACKFILL_RETRY_BASE = 1000; // 갭 복구 실패 시 첫 대기
const BACKFILL_RETRY_MAX = 15000;
// 서버가 명시적으로 거절했지만 영구 거절은 아닌 것(JSON 오류 본문의 401·403·409 등)이 이만큼 쌓이면 포기하고 사용자에게 알린다.
// 네트워크 실패·5xx·JSON 이 아닌 오류 응답·일시 오류 프레임은 세지 않는다 — 연결이 돌아오면 같은 client_msg_id 로 다시 나간다(isTransientFailure).
const MAX_SEND_TRIES = 20;
// 보낸 글이 이만큼 ack 를 못 받고 있으면 화면에 "다시 연결되면 보낸다" 를 띄운다(onStalled).
// 소켓은 열려 있는데 보는 대화의 구독이 이만큼 성공(ready)하지 못해도 같은 안내를 띄운다 — 그동안 새 메시지가 오지 않는다(DEF-72).
export const STALL_AFTER = 10000;
// 구독을 보내고 이만큼 ready 가 없으면 실패로 보고 다시 구독한다(DEF-72). 서버는 권한 확인과 읽음 지점 조회에 각각 5초 시한을 둔다.
export const SUBSCRIBE_READY_TIMEOUT = 10000;
const MAX_SAVED_IDS = 1000; // 받은 메시지의 client_msg_id 를 이만큼 기억한다. 포기하기 전에 이미 저장됐는지 본다
const PAGE = 200;
const MAX_CURSORS = 200; // 이보다 많아지면 전송 대기가 없는 옛 커서를 정리한다
// 첨부 완료를 다른 요청이 확인하는 중이면 서버는 기다리지 않고 409 busy 로 답한다. 이 간격으로 이만큼 다시 부른다.
export const COMPLETE_BUSY_RETRIES = 5;
export const COMPLETE_BUSY_INTERVAL = 400;

export function newClientMsgId() {
  if (crypto.randomUUID) return crypto.randomUUID();
  const b = crypto.getRandomValues(new Uint8Array(16));
  return [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
}

// api 는 JSON API 를 부른다. 실패 응답(!ok)은 본문이 JSON 이 아니어도(프록시·옛 서버의 text/plain 404·405) HTTP 상태를 err.status 로 남긴다 —
// 본문을 못 읽었다고 상태를 잃으면 부르는 쪽이 "응답이 없다" 로 오해한다(옛 서버 폴백·401 판정, 감사 20).
// err.code 는 우리 서버의 JSON 오류 본문(error.code)에서만 온다. 없으면 비워 둔다 — 상태 코드를 code 로 채우면 앞단(WAF·프록시·CDN)의
// 403·413 HTML 이 "서버가 그 글을 거절했다" 로 읽혀 재시도 상한에 세이고 글을 잃는다(감사 21). 그런 응답은 일시 실패다(isTransientFailure).
// 성공 응답의 본문을 못 읽으면 그대로 던진다(상태 없음 = 일시 실패로 본다).
// signal 을 주면 그 신호로 요청을 끊는다(티켓 시한).
export async function api(path, { token, method = "GET", body, signal } = {}) {
  const res = await fetch(path, {
    method,
    headers: {
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(body ? { "Content-Type": "application/json" } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
    ...(signal ? { signal } : {}),
  });
  const text = await res.text();
  let data = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch (err) {
    if (res.ok) throw err;
  }
  if (!res.ok) {
    const err = new Error(data?.error?.message || `HTTP ${res.status}`);
    err.code = typeof data?.error?.code === "string" ? data.error.code : "";
    err.status = res.status;
    err.data = data;
    // 문의 번호의 원본이다(D47). 서버가 모든 응답에 싣는 X-Trace-Id(32자리 hex)를 남긴다. 앞단이 답한 응답이면 없을 수 있다.
    err.traceId = res.headers?.get?.("X-Trace-Id") || "";
    // 429 의 Retry-After(초). 화면이 그만큼 기다렸다가 다시 시도한다.
    err.retryAfter = Number(res.headers?.get?.("Retry-After")) || 0;
    throw err;
  }
  return data;
}

// fetchTicket 은 WebSocket 티켓을 받는다(D46, DEF-91). 세션 토큰은 Authorization 헤더로만 가고, 주소에는 30초 일회용 티켓이 실린다.
// 앞단(ALB·CDN)의 접근 로그에 주소가 남아도 그 티켓은 이미 쓰였거나 곧 만료된다.
async function fetchTicket(token, { signal } = {}) {
  const res = await api("/api/ws-ticket", { token, method: "POST", signal });
  return res?.ticket || "";
}

// TICKET_TIMEOUT 은 티켓 요청 하나를 기다리는 시한이다. 넘으면 그 요청을 끊고 끊긴 소켓과 같은 백오프로 다시 받는다.
// 시한이 없으면 응답 없이 매달린 요청(죽은 TCP·멈춘 프록시) 하나가 connecting 을 쥔 채 다시 붙기를 영원히 막는다.
// 10초: 서버의 발급은 INSERT 하나이고 DB 읽기 시한이 5초다. 구독 응답 시한(SUBSCRIBE_READY_TIMEOUT)과 같은 값이다.
export const TICKET_TIMEOUT = 10000;

// ticketSource 는 티켓을 받는 기본 함수다. 동기 값을 돌려줘도 된다 — 테스트가 소켓을 바로 열려고 갈아 끼운다(setTicketSource).
let ticketSource = fetchTicket;
export function setTicketSource(fn) {
  ticketSource = fn || fetchTicket;
}

// 영구 거절만 큐에서 지운다. 다시 보내도 결과가 같은 것들이다.
// 나머지(네트워크 끊김, 5xx, 세션 일시 문제)는 큐에 남겨 두고 다시 시도한다.
export function isPermanentReject(code) {
  // ended: 종료된 지난 상담이다. 다시 보내도 같은 답이 온다.
  // closed_by_customer: 고객이 직접 닫은 상담이다(D48). 상담원이 다시 열기 전까지 같은 답이 온다. 지운 글은 거절 알림이 화면에 돌려준다.
  // 이 탭이 닫을 때는 큐를 먼저 비운 뒤 닫으므로(closeConversation) 이 거절은 다른 탭이 늦게 쓴 글에만 온다.
  return code === "invalid" || code === "not_found" || code === "ended" || code === "closed_by_customer";
}

// UNSAVED_NOTICE 는 저장이 막힌 브라우저에서 글을 저장하지 못했을 때의 알림이다. 그 글은 이 탭의 메모리에만 있어
// 다른 대화로 옮겨 가도 남고 연결되면 나가지만, 저장소에 옮기기 전에 이 탭을 닫거나 새로고침하면 사라진다(DEF-175).
// 로그아웃(다음 사람에게 가면 안 된다)도 버린다. 로그인 만료로 멈췄다가 같은 사람이 다시 들어오면 나간다(DEF-176).
const UNSAVED_NOTICE = "이 브라우저는 전송 대기를 저장하지 못합니다. 이 탭을 닫거나 새로고침하거나 로그아웃하면 보내지 못한 메시지가 사라집니다";

// CLOSE_FLUSH_TIMEOUT 은 고객 종료 전에 보내지 못한 글을 보내는 데 기다리는 시한이다(D48). 넘으면 닫지 않는다.
export const CLOSE_FLUSH_TIMEOUT = 15000;

// isTransientFailure 는 서버가 그 메시지를 판정하지 못한 실패다. 재시도 상한에 세지 않는다 (D5, 2026-10-05 logcat 결정).
//   REST   응답이 없다(오프라인·끊김·읽을 수 없는 본문), 우리 서버의 JSON 오류(code)가 없는 응답(앞단 WAF·프록시·CDN 의 403·404·408·413 HTML,
//          옛 서버의 text/plain — 감사 21), 5xx, 429(잠시 뒤), 499(요청 취소)
//   프레임 internal(처리 실패), canceled(연결이 닫히는 중)
// 세면 1분 넘게 끊긴 고객이 "보내지 못했다" 를 보고 글을 돌려받는다. 그 사이 서버가 저장했으면 다시 보낸 글이 중복이 된다.
// 그 밖의 거절(우리 서버가 JSON 으로 답한 영구 거절이 아닌 4xx, forbidden·conflict·unauthorized 프레임)은 서버가 명시적으로 거절한 것이라 센다.
const TRANSIENT_CODES = new Set(["internal", "canceled", "too_many", "draining", "db_down"]);
// isSessionExpired 는 세션이 끝났다는 우리 서버의 판정이다 — 401 이고 JSON 오류 본문의 code 가 unauthorized 일 때만이다(감사 22).
// 앞단(WAF·프록시·인증 게이트웨이)의 JSON 이 아닌 401 HTML 은 판정이 아니라 일시 실패다. 그걸 만료로 보면 익명 위젯이 새 고객으로
// 다시 열려 지난 상담과 큐의 글이 다른 고객 것이 되고, 데스크는 로그인을 잃는다. D5 의 "명시적 거절만 센다" 와 같은 경계다.
export function isSessionExpired(err) {
  return err?.status === 401 && err?.code === "unauthorized";
}

// uploadFailure 는 첨부 요청이 우리 서버의 판정 없이 실패했을 때(앞단의 HTML 답, 끊김, 읽을 수 없는 본문) 사람이 읽을 문구로 바꾼다.
// 우리 서버의 JSON 오류(code 가 있다)는 서버 문구를 그대로 둔다. 상태(status)와 문의 번호(traceId)는 지킨다.
export function uploadFailure(err) {
  if (err?.code) return err;
  const status = err?.status || 0;
  let message = "파일을 올리지 못했습니다. 연결을 확인한 뒤 다시 올려 주세요";
  if (status === 413) message = "파일이 너무 커서 올리지 못했습니다. 더 작은 파일을 올려 주세요";
  else if (status === 429) message = "요청이 많아 파일을 올리지 못했습니다. 잠시 후 다시 올려 주세요";
  else if (status) message = "파일을 올리지 못했습니다. 잠시 후 다시 올려 주세요";
  return Object.assign(new Error(message), { code: "", status, traceId: err?.traceId || "", retryAfter: err?.retryAfter || 0 });
}

export function isTransientFailure({ status, code } = {}) {
  if (TRANSIENT_CODES.has(code)) return true;
  if (!code) return true; // 응답이 없거나 우리 서버의 판정(JSON 오류 본문)이 아니다
  return status >= 500 || status === 429 || status === 499;
}

// restBackoff 는 일시 실패가 fails 번 이어진 뒤 기다릴 시간이다. 3초에서 두 배씩 30초까지, 같은 순간에 몰리지 않게
// 아래로 20% 까지 흔든다(지터). 상한은 넘지 않는다.
export function restBackoff(fails, rand = Math.random()) {
  const base = Math.min(REST_BACKOFF_BASE * 2 ** Math.max(0, fails - 1), REST_BACKOFF_MAX);
  return Math.round(base * (0.8 + 0.2 * rand));
}

// ── 저장 형식(D5, DEF-158, DEF-165) ─────────────────────────────
//   gcs.v1.<주인>.<대화 id>                  커서 { lastSeq }
//   gcs.q1.<주인>.<대화 id>|<client_msg_id>  전송 대기 글 하나 { …, n }
// 글마다 키가 따로다. 같은 브라우저의 두 탭이 같은 대화에 글을 쌓아도 서로의 글을 덮어쓰지 않는다 — 넣기는 자기 키 하나를 쓰고,
// 지우기(ack·영구 거절)는 그 글의 키 하나만 지운다. 예전에는 큐 전체를 한 키에 통째로 써서, 열 때 읽은 큐를 들고 있던 탭이
// 다른 탭이 넣은 글을 지운 채 저장했다. 그 탭을 ack 전에 닫으면 글이 사라졌다(DEF-158).
// 한 키에 읽고-고치고-쓰기를 하면 안 되는 까닭: 브라우저는 탭마다 저장소의 사본을 들고 바뀐 것을 나중에 알린다. 쓰기 직전에
// 다시 읽어도 다른 탭이 방금 쓴 것을 못 볼 수 있다. 키가 다르면 그 틈이 없다.
// 글 키는 커서와 다른 이름 공간(gcs.q1.)에 둔다(DEF-165). 옛 코드(되돌려 배포)는 gcs.v1.<주인>. 아래 키를 모두 커서로 보고,
// 용량이 차거나 키가 200개를 넘으면 전송 대기 배열이 없는 키를 지운다. 글 키가 그 아래 있으면 옛 코드가 글을 지웠다(감사 27 a3).
// 로그아웃·공유 PC 가드는 gcs.* 를 지우므로 글 키도 같이 지운다.
// 옛 형식 둘은 읽을 때 옮긴다. 커서 키 안의 pending 배열(배포된 형식)은 글마다 키로 옮기고 다 써야 배열을 지운다(migrateItems).
// gcs.v1.<주인>.<대화>|<cmid>(배포된 적 없는 중간 형식)는 새 이름 공간에 쓴 뒤 지운다(moveStray).
// 순서는 대화마다 늘어나는 번호(n)다. 시계(Date.now)는 뒤로 갈 수 있어 순서로 쓰지 않는다. 쓰기 직전 저장소에 있는 그 대화의 가장 큰 번호 + 1 이다.
// 두 탭이 같은 순간에 써서 번호가 같으면 client_msg_id 로 정렬한다 — 어느 탭이 읽어도 같은 순서다.
const QUEUE_PREFIX = "gcs.q1.";
const ITEM_SEP = "|";

// queuePrefix 는 커서 접두사(gcs.v1.<주인>.)에 맞는 글 접두사(gcs.q1.<주인>.)다.
function queuePrefix(prefix) {
  return QUEUE_PREFIX + prefix.slice(STORE_PREFIX.length);
}

function itemKey(prefix, convId, clientMsgId) {
  return `${queuePrefix(prefix)}${convId}${ITEM_SEP}${clientMsgId}`;
}

// parseKey 는 저장소 키를 { convId, cmid } 로 푼다. 커서 키면 cmid 가 비어 있다. 이 접두사의 키가 아니면 null 이다.
function parseKey(prefix, key) {
  if (!key?.startsWith(prefix)) return null;
  const rest = key.slice(prefix.length);
  // 옛 방식 키(접두사 바로 뒤가 대화 id)와 소유자 키(주인.대화)가 섞이지 않게 한다.
  if ((prefix === STORE_PREFIX || prefix === QUEUE_PREFIX) && rest.includes(".")) return null;
  const sep = rest.lastIndexOf(ITEM_SEP);
  if (sep < 0) return { convId: rest, cmid: "" };
  return { convId: rest.slice(0, sep), cmid: rest.slice(sep + 1) };
}

// parseItem 은 글 키만 푼다(이름 공간 gcs.q1.). 아니면 null 이다.
function parseItem(prefix, key) {
  const at = parseKey(queuePrefix(prefix), key);
  return at?.cmid ? at : null;
}

function storageKeys() {
  const out = [];
  try {
    for (let i = 0; i < localStorage.length; i++) out.push(localStorage.key(i));
  } catch {
    // 접근이 막히면 현재 대화만 다룬다
  }
  return out;
}

function readJSON(key) {
  try {
    const raw = localStorage.getItem(key);
    return raw ? JSON.parse(raw) : null;
  } catch {
    return null;
  }
}

function legacyPending(cursor) {
  return Array.isArray(cursor?.pending) ? cursor.pending.filter((p) => p?.client_msg_id) : [];
}

// byQueueOrder 는 큐의 순서다. 번호(n)가 있는 글은 번호 순, 같으면 client_msg_id 순이다.
// 번호가 없는 글(옛 형식에서 옮긴 것)은 그보다 앞이고 넣은 시각(at)과 배열 순서(ord)를 따른다.
const byQueueOrder = (a, b) => (a.n > 0) - (b.n > 0) || (a.n || 0) - (b.n || 0)
  || (a.at || 0) - (b.at || 0) || (a.ord || 0) - (b.ord || 0)
  || (a.client_msg_id < b.client_msg_id ? -1 : a.client_msg_id > b.client_msg_id ? 1 : 0);

// loadState 는 그 대화의 커서와 전송 대기 글을 저장소에서 읽는다. 글은 큐의 순서(byQueueOrder)다.
// 옛 형식이 남아 있으면 옮긴다.
function loadState(convId, prefix = STORE_PREFIX) {
  const cursor = readJSON(prefix + convId) || {};
  const seen = new Set();
  const pending = [];
  const add = (p) => {
    if (!p?.client_msg_id || seen.has(p.client_msg_id)) return;
    seen.add(p.client_msg_id);
    pending.push(p);
  };
  const strays = [];
  for (const key of storageKeys()) {
    const item = parseItem(prefix, key);
    if (item && item.convId === convId) add(readJSON(key));
    const at = parseKey(prefix, key);
    if (at?.cmid && at.convId === convId) strays.push(key);
  }
  for (const key of strays) add(moveStray(key, convId, prefix));
  const legacy = legacyPending(cursor);
  legacy.forEach((p, i) => add({ ...p, ord: p.ord ?? i }));
  if (legacy.length > 0) migrateCursor(convId, prefix, cursor);
  pending.sort(byQueueOrder);
  return { lastSeq: Number(cursor.lastSeq) || 0, pending };
}

// moveStray 는 커서 이름 공간에 글마다 키로 남은 글(배포된 적 없는 중간 형식)을 글 이름 공간으로 옮기고 그 글을 돌려준다.
// 새 키에 썼거나 이미 있을 때만 옛 키를 지운다.
function moveStray(key, convId, prefix) {
  const item = readJSON(key);
  if (!item?.client_msg_id) return null;
  const to = itemKey(prefix, convId, item.client_msg_id);
  let ok = false;
  try { ok = localStorage.getItem(to) !== null; } catch { /* 읽기가 막혔다 */ }
  if (!ok) ok = writeKey(to, JSON.stringify(item), convId, prefix);
  if (ok) {
    try { localStorage.removeItem(key); } catch { /* 다음에 다시 옮긴다(이미 있으면 건너뛴다) */ }
  }
  return item;
}

// maxOrder 는 저장소에 있는 그 대화 글의 가장 큰 번호다. 없으면 0 이다.
function maxOrder(convId, prefix) {
  let max = 0;
  for (const key of storageKeys()) {
    const item = parseItem(prefix, key);
    if (item?.convId === convId) max = Math.max(max, Number(readJSON(key)?.n) || 0);
  }
  return max;
}

// trySet 은 한 키를 쓴다. 용량 정리 없이 한 번만 — 저장하지 못한 글을 옮기려고 주기마다 부르는 길이라 커서를 지우지 않는다.
function trySet(key, payload) {
  try {
    localStorage.setItem(key, payload);
    return true;
  } catch {
    return false;
  }
}

// writeKey 는 한 키를 쓴다. 용량이 차면 전송 대기가 없는 옛 커서를 버리고 한 번 더 시도한다.
// 저장이 막히면 전송 대기 글이 탭을 닫는 순간 사라지므로 커서보다 글이 먼저다.
function writeKey(key, payload, keepConvId, prefix) {
  try {
    localStorage.setItem(key, payload);
    return true;
  } catch {
    pruneCursors(keepConvId, prefix);
    try {
      localStorage.setItem(key, payload);
      return true;
    } catch {
      // 시크릿 창처럼 쓰기 자체가 막힌 환경이다. 동작은 유지하되 탭을 닫으면 큐가 사라진다.
      return false;
    }
  }
}

// migrateItems 는 옛 형식 배열의 글을 글마다 키로 쓴다. 이미 키가 있는 글은 그대로 둔다(그쪽이 새것이다). 다 썼으면 true 다.
function migrateItems(convId, prefix, items) {
  let ok = true;
  items.forEach((p, i) => {
    const key = itemKey(prefix, convId, p.client_msg_id);
    let has = false;
    try { has = localStorage.getItem(key) !== null; } catch { /* 읽기가 막혔다. 쓰기도 막혔을 것이다 */ }
    if (has) return;
    if (!writeKey(key, JSON.stringify({ ...p, ord: p.ord ?? i }), convId, prefix)) ok = false;
  });
  return ok;
}

// migrateCursor 는 커서 키의 옛 배열을 글마다 키로 옮기고, 다 옮겼을 때만 배열을 지운다.
function migrateCursor(convId, prefix, cursor) {
  if (!migrateItems(convId, prefix, legacyPending(cursor))) return;
  try {
    localStorage.setItem(prefix + convId, JSON.stringify({ lastSeq: Number(cursor.lastSeq) || 0 }));
  } catch { /* 배열이 남는다. 다음에 읽을 때 다시 옮긴다(이미 옮긴 글은 건너뛴다) */ }
}

// saveCursor 는 커서만 쓴다. 전송 대기 글은 건드리지 않는다. 옛 배열이 남아 있으면 먼저 옮기고, 못 옮기면 배열을 그대로 둔다.
function saveCursor(convId, lastSeq, prefix = STORE_PREFIX) {
  if (!convId) return false;
  const cursor = readJSON(prefix + convId) || {};
  let left = legacyPending(cursor);
  if (left.length > 0 && migrateItems(convId, prefix, left)) left = [];
  const payload = JSON.stringify(left.length > 0 ? { lastSeq, pending: left } : { lastSeq });
  return writeKey(prefix + convId, payload, convId, prefix);
}

// saveItem 은 전송 대기 글 하나를 그 글의 키에 쓴다.
function saveItem(convId, item, prefix = STORE_PREFIX) {
  if (!convId) return false;
  return writeKey(itemKey(prefix, convId, item.client_msg_id), JSON.stringify(item), convId, prefix);
}

// updateItem 은 저장소에 아직 있는 글만 고쳐 쓴다(시도 횟수). 다른 탭이 ack 로 지운 글을 되살리지 않는다.
function updateItem(convId, item, prefix = STORE_PREFIX) {
  const key = itemKey(prefix, convId, item.client_msg_id);
  try {
    if (localStorage.getItem(key) !== null) localStorage.setItem(key, JSON.stringify(item));
  } catch { /* 고치지 못해도 글은 남아 있다. 시도 횟수만 덜 센다 */ }
}

// removeItem 은 그 글의 키를 지운다. 옛 형식(배열, 중간 형식 키)에 남아 있으면 거기서도 뺀다.
function removeItem(convId, clientMsgId, prefix = STORE_PREFIX) {
  try {
    localStorage.removeItem(itemKey(prefix, convId, clientMsgId));
    localStorage.removeItem(`${prefix}${convId}${ITEM_SEP}${clientMsgId}`);
    const cursor = readJSON(prefix + convId);
    const legacy = legacyPending(cursor);
    if (legacy.some((p) => p.client_msg_id === clientMsgId)) {
      cursor.pending = legacy.filter((p) => p.client_msg_id !== clientMsgId);
      localStorage.setItem(prefix + convId, JSON.stringify(cursor));
    }
  } catch { /* 못 지우면 다시 나가고 서버가 같은 글로 답한다(멱등) */ }
}

// pruneCursors 는 전송 대기 메시지가 없는 옛 대화의 커서를 지운다. 커서 키만 본다 — 글의 키는 지우지 않는다.
// 커서는 잃어도 서버가 처음부터 다시 보내 주므로 복구되지만, 전송 대기 큐는 복구 수단이 없다.
function pruneCursors(keepConvId, prefix = STORE_PREFIX) {
  const keys = storageKeys();
  const queued = new Set();
  for (const key of keys) {
    const at = parseItem(prefix, key) || parseKey(prefix, key);
    if (at?.cmid) queued.add(at.convId);
  }
  for (const key of keys) {
    const at = parseKey(prefix, key);
    if (!at || at.cmid || at.convId === keepConvId || queued.has(at.convId)) continue;
    if (legacyPending(readJSON(key)).length > 0) continue;
    try {
      localStorage.removeItem(key);
    } catch {
      return;
    }
  }
}

// storedConversations 는 이 브라우저가 기억하는 대화 id 를 모두 준다(커서나 전송 대기 글이 있는 대화).
// 대화가 바뀌어도 옛 대화의 전송 대기 메시지를 버리지 않으려면 이게 필요하다.
// 소유자 접두사 아래의 키만 본다. 다른 주체의 큐는 건드리지도, 지우지도 않는다.
function storedConversations(prefix = STORE_PREFIX) {
  const out = new Set();
  for (const key of storageKeys()) {
    const at = parseKey(prefix, key) || parseItem(prefix, key);
    if (at) out.add(at.convId);
  }
  return [...out];
}

export class ChatSession {
  // handlers: { onMessage, onPending, onStatus, onReady, onConversation, onReads, onRead,
  //             onTyping, onInboxChanged, onError, onGone }
  // onGone(convId) 은 보던 대화를 서버가 "없다" 고 답했을 때 한 번 온다. 삭제됐거나 회원에게 지난 상담이 된 경우다.
  // onError(문구, client_msg_id, 버린 항목, trace id) 의 세 번째 인자는 이 알림 때문에 큐에서 지운 메시지다
  // ({ client_msg_id, conversation_id, body, internal, attachment }). 지운 것이 없으면 없다. 새로고침 뒤 재전송이 거절돼도
  // 화면이 본문을 되돌릴 수 있게 넘긴다. 큐 동작(언제 지우고 언제 남기나)은 이 인자와 상관없다.
  // 네 번째 인자는 그 거절을 낸 서버 처리의 trace id 다(응답의 X-Trace-Id, 오류 프레임의 trace_id). 화면이 앞 8자리를 문의 번호로 보인다(D47).
  // owner 는 "agent:<id>" / "customer:<id>" 처럼 큐의 주인이다. 주체가 바뀌면 새 세션을 만든다.
  // unsaved 는 저장하지 못한 글의 메모리 큐(대화 id → (client_msg_id → 글))다. 화면이 세션 밖에 들고 있다가 같은 주인의 새 세션에 넘긴다 —
  // 데스크는 로그인이 만료되면 세션을 버리고 다시 로그인할 때 새로 만든다. 그 사이 메모리 글을 잃지 않게(DEF-176). 없으면 새로 만든다.
  // onUnauthorized() 는 티켓을 받으려다 우리 서버의 401 unauthorized(세션 만료·로그아웃)를 받았을 때 온다(isSessionExpired). 앞단의 JSON 아닌 401 은 일시 실패다. 화면이 다시 로그인하거나 세션을 다시 연다.
  // 그동안 소켓은 평소 백오프로 다시 붙으러 간다 — 새 토큰을 받으면 reconnect 가 기다림을 끊는다.
  // ticket(token) 은 티켓을 받는 함수다. 없으면 POST /api/ws-ticket 이다.
  constructor({ token, owner = "", handlers = {}, ticket = null, unsaved = null }) {
    this.token = token;
    this.ticket = ticket;
    // connectSeq 는 연결 시도의 번호다. 티켓을 받는 사이 토큰이 바뀌거나(reconnect) 멈추면(stop) 늦게 온 티켓을 버린다.
    this.connectSeq = 0;
    this.connecting = false;
    this.ticketTimer = null; // 티켓 요청의 시한(TICKET_TIMEOUT)
    this.lastCheck = 0;
    this.pingAt = 0;
    this.prefix = storePrefix(owner);
    this.h = handlers;
    this.ws = null;
    this.convId = null;
    this.state = { lastSeq: 0, pending: [] };
    this.backoff = 500;
    this.stopped = false;
    this.timers = [];
    this.backfilling = false;
    this.backfillFails = 0;
    this.backfillTimer = null;
    this.buffer = [];
    this.status = "connecting";
    this.reportedRead = 0;
    this.lastTypingSent = 0;
    this.lastSeen = 0;
    this.stalled = "";
    this.savedIds = new Set();
    this.restWait = new Map(); // 대화 id → { fails, at, hard } 다음에 REST 로 밀 시각. hard 는 서버가 정한 Retry-After 다
    // 구독과 읽음 보고의 다시 시도(DEF-72). subscribed 는 ready 를 받은 대화, subSentAt 은 마지막 구독을 보낸 시각,
    // subSince 는 지금 대화의 구독을 기다리기 시작한 시각이다. retryAt 이 차면 다시 구독하거나(ready 전) 읽음을 다시 보낸다(ready 뒤).
    this.subscribed = null;
    this.subSentAt = 0;
    this.subSince = 0;
    this.retryFails = 0;
    this.retryAt = 0;
    this.readAgain = 0;
    this.pushing = new Set(); // REST 로 미는 중인 대화. 끝나지 않은 대화는 건너뛰고 다른 대화는 기다리지 않는다
    // 저장소에 쓰지 못한 글(쓰기가 막힌 브라우저). 대화 id → (client_msg_id → 글). 이 탭의 메모리에만 있으니 저장소와 맞출 때 버리지 않고,
    // 다른 대화로 옮겨 가도 남는다 — 보지 않는 대화의 글은 저장소 큐와 같이 flushViaRest 가 REST 로 민다(DEF-175). 탭을 닫거나 새로고침하면 사라진다.
    // 저장이 다시 되면 저장소로 옮긴다(persistUnsaved, DEF-177).
    this.unsaved = unsaved instanceof Map ? unsaved : new Map();

    // 같은 브라우저의 다른 탭이 큐를 바꿨다(DEF-158). 보고 있는 대화면 화면의 큐를 저장소에 맞춘다 —
    // 다른 탭이 넣은 글은 들이고, 다른 탭이 ack 를 받아 지운 글은 뺀다. 저장소 이벤트는 쓴 탭 자신에게는 오지 않는다.
    addEventListener("storage", (e) => this.onStorage(e));
    addEventListener("online", () => {
      this.resetRestWait();
      this.connect();
    });
    addEventListener("visibilitychange", () => {
      if (document.visibilityState !== "visible") return;
      this.connect();
      this.reportRead(this.state.lastSeq);
    });
  }

  setStatus(s) {
    if (this.status === s) return;
    this.status = s;
    this.h.onStatus?.(s);
    this.checkStall();
  }

  // checkStall 은 보고 있는 대화의 글이 오래 ack 를 못 받고 있는지 화면에 알린다. 바뀔 때만 onStalled 를 부른다.
  //   onStalled({ offline, count }) — offline 이면 연결이 끊겨 있다. 다시 연결되면 같은 client_msg_id 로 나간다.
  //                                   소켓이 닫혀 있어도 REST 다시 보내기가 HTTP 답(앞단의 403 HTML·5xx 등)을 받고 있으면 끊긴 것이 아니다 —
  //                                   offline 이 아니고 화면은 "전송이 지연되고 있다" 고 알린다(감사 22, 사실대로 알린다)
  //   onStalled(null)              — 기다리는 글이 없다
  // 큐 동작과는 상관없다. 알림만이다.
  // 소켓은 열려 있는데 보는 대화의 구독이 오래 성공하지 못했으면 끊긴 것과 같이 알린다 — 새 메시지가 오지 않는다(DEF-72).
  checkStall() {
    const now = Date.now();
    const pending = this.convId ? this.state.pending : [];
    const unsubscribed = !!this.convId && this.ws?.readyState === WebSocket.OPEN && this.subscribed !== this.convId
      && this.subSince > 0 && now - this.subSince > STALL_AFTER;
    const late = unsubscribed || pending.some((p) => now - (p.at || 0) > STALL_AFTER);
    // 다시 보내기가 최근(백오프 상한 + 안내 기준 안)에 실패 답(HTTP 상태가 있는 오류)을 받았으면 끊긴 것이 아니라 전송이 막힌 것이다.
    // 답 없는 실패와 성공은 restReplyAt 을 지운다.
    const replied = this.restReplyAt > 0 && now - this.restReplyAt < REST_BACKOFF_MAX + STALL_AFTER;
    const offline = unsubscribed || (this.ws?.readyState !== WebSocket.OPEN && !replied);
    const next = late ? `${offline ? "offline" : "waiting"}:${pending.length}` : "";
    if (next === this.stalled) return;
    this.stalled = next;
    this.h.onStalled?.(late ? { offline, count: pending.length } : null);
  }

  // noteSaved 는 서버가 저장한 메시지의 client_msg_id 를 기억한다. 포기하기 전에 이걸 보고, 이미 저장된 글은 돌려주지 않는다.
  noteSaved(msg) {
    if (!msg?.client_msg_id) return;
    this.savedIds.add(msg.client_msg_id);
    if (this.savedIds.size > MAX_SAVED_IDS) this.savedIds.delete(this.savedIds.values().next().value);
  }

  start() {
    this.stopped = false;
    this.connect();
    // WebSocket 이 죽어 있는 동안에도, 그리고 다른 대화에 남은 것까지 밀어 넣는다.
    this.timers.push(setInterval(() => this.flushViaRest({ due: true }), REST_CHECK));
    this.timers.push(setInterval(() => this.checkLiveness(), LIVENESS_TICK));
    this.timers.push(setInterval(() => this.checkSubscription(), REST_CHECK));
    if (storedConversations(this.prefix).length > MAX_CURSORS) pruneCursors(this.convId, this.prefix);
    this.flushViaRest();
  }

  stop() {
    this.stopped = true;
    this.connectSeq += 1;
    this.connecting = false;
    clearTimeout(this.ticketTimer);
    for (const t of this.timers) clearInterval(t);
    this.timers = [];
    clearTimeout(this.backfillTimer);
    this.ws?.close();
  }

  // discard 는 로그아웃(공유 PC 가드, D46)에서 세션을 버린다. 멈추고 이 탭의 메모리에만 있는 글(저장하지 못한 글)도 버린다 —
  // 다음 사람에게 가면 안 된다. 저장소의 큐는 부르는 쪽이 gcs.* 를 지워 버린다. 멈추기만 할 때(stop)는 메모리 글을 남긴다(다시 start 하면 나간다).
  discard() {
    this.stop();
    this.unsaved.clear();
    this.convId = null;
    this.state = { lastSeq: 0, pending: [] };
  }

  // ── 저장하지 못한 글(메모리 큐, DEF-175) ─────────────────────────

  keepUnsaved(convId, item) {
    let mem = this.unsaved.get(convId);
    if (!mem) this.unsaved.set(convId, (mem = new Map()));
    mem.set(item.client_msg_id, item);
  }

  isUnsaved(convId, clientMsgId) {
    return !!this.unsaved.get(convId)?.has(clientMsgId);
  }

  forgetUnsaved(convId, clientMsgId) {
    const mem = this.unsaved.get(convId);
    if (!mem) return;
    mem.delete(clientMsgId);
    if (mem.size === 0) this.unsaved.delete(convId);
  }

  // persistUnsaved 는 저장하지 못한 글을 저장소로 옮긴다(DEF-177). 저장이 다시 되면(용량이 비었다, 막힘이 풀렸다) 그 글은 탭을 닫아도 남아야 한다.
  // 전에는 메모리에 둔 채 보냈고, 저장이 돌아온 뒤에도 새로고침하면 그 글만 사라졌다(감사 29 a29 recover). 글 키에 쓴 것만 메모리에서 뺀다.
  // 번호(n)는 메모리에 둘 때 받은 그대로다 — 이 탭의 쓴 순서다. 같은 client_msg_id 의 키가 이미 있으면 쓰지 않는다(다른 탭이 옮겼다).
  // 하나라도 못 쓰면 이번에는 멈춘다(아직 막혀 있다). 주기 확인(flushViaRest)과 글을 저장한 직후(send)에 부른다.
  persistUnsaved() {
    for (const [convId, mem] of [...this.unsaved]) {
      for (const item of [...mem.values()]) {
        const key = itemKey(this.prefix, convId, item.client_msg_id);
        let has = false;
        try { has = localStorage.getItem(key) !== null; } catch { /* 읽기가 막혔다 */ }
        if (!has && !trySet(key, JSON.stringify(item))) return;
        this.forgetUnsaved(convId, item.client_msg_id);
      }
    }
  }

  // withUnsaved 는 저장소에서 읽은 그 대화의 상태에 이 탭이 저장하지 못한 글을 합친다. 같은 client_msg_id 는 하나만(저장소 쪽)이고
  // 큐의 순서(byQueueOrder, 번호 n)로 다시 세운다 — 저장하지 못한 글도 쓰기 직전의 번호를 받았다.
  withUnsaved(convId, state) {
    const mem = this.unsaved.get(convId);
    if (!mem) return state;
    const known = new Set(state.pending.map((p) => p.client_msg_id));
    let added = false;
    for (const p of mem.values()) {
      if (known.has(p.client_msg_id)) continue;
      state.pending.push(p);
      added = true;
    }
    if (added) state.pending.sort(byQueueOrder);
    return state;
  }

  // open 은 볼 대화를 바꾼다. 데스크에서 다른 대화를 누를 때마다 호출한다.
  // fromStart 는 화면을 비우고 다시 그릴 때 쓴다. 커서만 이어가면 화면이 빈 채로 남는다.
  // 큐는 저장소에서 다시 읽고 이 탭이 저장하지 못한 그 대화의 글을 합친다(DEF-175) — 다른 대화에 다녀와도 그 글이 남는다.
  open(convId, { fromStart = false } = {}) {
    clearTimeout(this.backfillTimer);
    this.convId = convId;
    this.state = this.withUnsaved(convId, loadState(convId, this.prefix));
    this.adoptLegacy(convId);
    if (fromStart) this.state.lastSeq = 0;
    this.buffer = [];
    this.reportedRead = 0;
    this.lastTypingSent = 0;
    this.backfilling = false;
    this.backfillFails = 0;
    this.resetSubscription();
    this.subSince = Date.now();
    this.h.onPending?.(this.state.pending);
    this.checkStall();
    this.subscribe();
  }

  // close 는 보던 대화에서 손을 뗀다. 서버의 구독은 다음 subscribe 가 바꾸고, 그 사이 옛 대화의 프레임은 대화 id 가 달라 버린다.
  // 옛 대화의 전송 대기 큐는 저장소에 그대로 남아 flushViaRest 가 계속 민다. 저장하지 못한 글은 메모리 큐(unsaved)에 남아 같이 민다.
  close() {
    clearTimeout(this.backfillTimer);
    this.convId = null;
    this.state = { lastSeq: 0, pending: [] };
    this.buffer = [];
    this.backfilling = false;
    this.backfillFails = 0;
    this.reportedRead = 0;
    this.resetSubscription();
    this.h.onPending?.([]);
    this.checkStall();
  }

  // gone 은 보던 대화가 더 이상 없다는 답을 받았을 때다. 실패가 아니라 상태라서 오류로 알리지 않는다.
  // 대화에서 손을 떼므로 같은 대화의 not_found 가 뒤따라 와도 다시 알리지 않는다.
  gone() {
    const convId = this.convId;
    if (!convId) return;
    this.close();
    if (this.h.onGone) this.h.onGone(convId);
    else this.h.onError?.("이 상담은 더 이상 열 수 없습니다");
  }

  // adoptLegacy 는 소유자 접두사가 없던 시절의 큐를 지금 여는 대화에 한해 이어받는다.
  // 그 대화를 열 수 있는 사람이라면 서버가 소속을 확인해 주므로 남의 큐를 보내는 일은 없다.
  // 이렇게 안 하면 배포 시점에 ack 못 받은 메시지가 조용히 묻힌다. 내 접두사 아래에 다 쓴 뒤에만 옛 키를 지운다.
  adoptLegacy(convId) {
    if (this.prefix === STORE_PREFIX) return;
    const legacy = loadState(convId, STORE_PREFIX);
    if (legacy.pending.length === 0) return;
    const known = new Set(this.state.pending.map((p) => p.client_msg_id));
    const moved = migrateItems(convId, this.prefix, legacy.pending);
    for (const p of legacy.pending) {
      if (known.has(p.client_msg_id)) continue;
      this.state.pending.push(p);
      if (!moved) this.keepUnsaved(convId, p);
    }
    this.state.pending.sort(byQueueOrder);
    if (!moved) return; // 옛 키를 남긴다. 다음에 열 때 다시 옮긴다
    for (const p of legacy.pending) removeItem(convId, p.client_msg_id, STORE_PREFIX);
    try { localStorage.removeItem(STORE_PREFIX + convId); } catch { /* 못 지워도 다음에 다시 합친다 */ }
  }

  // reconnect 는 토큰이 바뀌었을 때 쓴다. 열린 소켓은 옛 주체로 인증돼 있어 그대로 두면 REST 와 WS 가 다른 사람이 된다.
  // 옛 토큰으로 받던 티켓이 늦게 와도 버린다(connectSeq).
  reconnect(token) {
    this.token = token;
    this.connectSeq += 1;
    this.connecting = false;
    clearTimeout(this.ticketTimer);
    const ws = this.ws;
    this.ws = null;
    try { ws?.close(); } catch { /* 이미 닫혔다 */ }
    this.connect();
  }

  // connect 는 티켓을 받고 소켓을 연다. 이미 열려 있거나 여는 중이면 아무것도 하지 않는다.
  // 티켓을 못 받으면(네트워크·5xx·429) 소켓이 끊긴 것과 같은 백오프로 다시 시도한다. 401 은 onUnauthorized 로 알리고 같은 백오프다.
  // 유실 방지(전송 대기·ack·after_seq·1001·구독 다시 시도)는 소켓이 열린 뒤의 일이라 티켓과 상관없이 그대로다.
  connect() {
    if (this.stopped || this.connecting) return;
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING)) return;

    this.setStatus("connecting");
    const seq = ++this.connectSeq;
    const abort = typeof AbortController === "function" ? new AbortController() : null;
    let got;
    try {
      got = (this.ticket || ticketSource)(this.token, { signal: abort?.signal });
    } catch (err) {
      got = Promise.reject(err);
    }
    if (!got || typeof got.then !== "function") {
      this.openSocket(got);
      return;
    }
    this.connecting = true;
    // 시한을 넘기면 그 시도를 버린다(connectSeq). 늦게 온 티켓·실패는 무시되고, 끊긴 소켓과 같은 백오프로 다시 받는다.
    clearTimeout(this.ticketTimer);
    const timer = setTimeout(() => {
      if (seq !== this.connectSeq || this.stopped) return;
      this.connectSeq += 1;
      this.connecting = false;
      try { abort?.abort(); } catch { /* 이미 끝났다 */ }
      this.ticketFailed({ code: "timeout" });
    }, TICKET_TIMEOUT);
    this.ticketTimer = timer;
    got.then(
      (ticket) => {
        clearTimeout(timer);
        if (seq !== this.connectSeq || this.stopped) return; // 그 사이 토큰이 바뀌었거나 멈췄거나 시한이 지났다
        this.connecting = false;
        this.openSocket(ticket);
      },
      (err) => {
        clearTimeout(timer);
        if (seq !== this.connectSeq || this.stopped) return;
        this.connecting = false;
        this.ticketFailed(err);
      },
    );
  }

  // ticketFailed 는 티켓을 못 받았을 때다. 티켓 길이 없는 옛 서버(404·405)면 옛 길(?token=)로 붙는다 — 배포 중 새 chat.js 가
  // 옛 서버에 닿는 동안만 생긴다. 옛 서버(v0.0.4)는 POST /api/ws-ticket 에 정적 서버의 405 "Method Not Allowed"(text/plain)로 답한다 —
  // api() 가 JSON 이 아닌 본문에도 상태를 남겨서 이 판정이 선다. 그 밖은 소켓이 끊긴 것과 같은 백오프로 다시 붙으러 간다.
  ticketFailed(err) {
    if (err?.status === 404 || err?.status === 405) {
      this.openSocket("");
      return;
    }
    if (isSessionExpired(err)) this.h.onUnauthorized?.();
    this.setStatus("offline");
    this.retryConnect();
  }

  // retryConnect 는 백오프 뒤에 다시 붙으러 간다. 0.5초에서 두 배씩 8초까지, 지터를 둔다.
  retryConnect() {
    const wait = this.backoff + Math.floor(Math.random() * 250);
    this.backoff = Math.min(this.backoff * 2, 8000);
    setTimeout(() => this.connect(), wait);
  }

  openSocket(ticket) {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const auth = ticket ? `ticket=${encodeURIComponent(ticket)}` : `token=${encodeURIComponent(this.token)}`;
    const ws = new WebSocket(`${proto}//${location.host}/ws?${auth}`);
    this.ws = ws;

    ws.onopen = () => {
      this.backoff = 500;
      this.resetRestWait(); // 연결이 돌아왔다. 다른 대화의 큐도 다음 확인에 곧바로 민다
      this.lastSeen = Date.now();
      this.lastCheck = Date.now();
      this.pingAt = 0;
      this.resetSubscription(); // 새 연결이다. 구독은 처음부터다
      if (this.convId) this.subSince = Date.now();
      this.setStatus("online");
      this.subscribe();
    };
    ws.onmessage = (ev) => {
      this.lastSeen = Date.now(); // 어떤 프레임이든 받았으면 연결은 살아 있다
      let f;
      try {
        f = JSON.parse(ev.data);
      } catch {
        return;
      }
      this.handleFrame(f);
    };
    ws.onclose = (ev) => {
      this.setStatus("offline");
      if (this.stopped || this.ws !== ws) return; // 갈아 끼운 옛 소켓이다(reconnect)
      // 1001 은 서버가 정상으로 내려간다는 뜻이다. 백오프를 올리지 않고 곧바로 다시 붙는다. 붙으면 subscribe 가
      // after_seq 로 빠진 것을 받고, ack 못 받은 메시지는 큐에 남아 있다가 다시 나간다.
      if (ev?.code === GOING_AWAY) {
        setTimeout(() => this.connect(), Math.floor(Math.random() * GOING_AWAY_JITTER));
        return;
      }
      this.retryConnect();
    };
    ws.onerror = () => ws.close();
  }

  // checkLiveness 는 조용한 연결을 떨어뜨린다. 떨어지면 재접속이 돌고 캐치업이 메운다.
  // 숨긴 탭이거나 확인이 늦게 돌았으면(타이머 묶임) 조용한 시간 대신 ping 의 답으로 판정한다(DEF-102).
  checkLiveness() {
    this.checkStall();
    const now = Date.now();
    const late = this.lastCheck > 0 && now - this.lastCheck > LIVENESS_LATE;
    this.lastCheck = now;
    if (this.ws?.readyState !== WebSocket.OPEN) return;
    const quiet = now - this.lastSeen;
    const waiting = this.pingAt > 0 && this.lastSeen < this.pingAt; // 보낸 ping 의 답을 아직 못 받았다
    if (late || document.visibilityState === "hidden") {
      if (waiting) {
        if (now - this.pingAt > PING_REPLY_WAIT) this.drop();
        return;
      }
      if (late || quiet > QUIET_BEFORE_PING) this.ping(now);
      return;
    }
    if (quiet > QUIET_BEFORE_DROP) {
      this.drop();
      return;
    }
    if (quiet > QUIET_BEFORE_PING) this.ping(now);
  }

  ping(now) {
    if (this.sendFrame({ type: "ping" }) && !(this.pingAt > 0 && this.lastSeen < this.pingAt)) this.pingAt = now;
  }

  drop() {
    this.setStatus("offline");
    this.ws.close();
  }

  subscribe() {
    if (!this.convId) return;
    if (this.sendFrame({ type: "subscribe", conversation_id: this.convId, after_seq: this.state.lastSeq })) this.subSentAt = Date.now();
  }

  // resetSubscription 은 구독 상태를 비운다. 대화를 바꾸거나 닫을 때, 새 소켓이 열릴 때 부른다.
  resetSubscription() {
    this.subscribed = null;
    this.subSentAt = 0;
    this.subSince = 0;
    this.retryFails = 0;
    this.retryAt = 0;
    this.readAgain = 0; // 되돌린 읽음 보고 지점. 다시 보낼 때 쓴다
  }

  // retryLater 는 글과 무관한 일시 오류(구독·읽음·입력 중의 실패, client_msg_id 없음)를 받았거나 구독의 ready 가 늦을 때 부른다.
  // 오류 프레임만으로는 어느 요청의 답인지 모른다. 다시 해도 결과가 같은 것들이라 함께 다시 한다 — ready 전이면 다시 구독하고,
  // ready 뒤면 읽음을 다시 보낸다. 읽음은 보고 지점을 되돌린다. 서버는 읽음 지점을 되돌리지 않으므로 한 번 더 보내도 같다.
  // 입력 중 신호는 잃어도 된다. 간격은 REST 백오프와 같이 3초에서 두 배씩 30초까지다(restBackoff).
  retryLater() {
    if (!this.convId) return;
    this.readAgain = Math.max(this.readAgain, this.reportedRead);
    this.reportedRead = 0;
    this.retryFails += 1;
    this.retryAt = Date.now() + restBackoff(this.retryFails);
  }

  // checkSubscription 은 1초마다 구독이 걸려 있는지 본다(DEF-72). 구독이 일시 오류로 실패해도 서버는 연결을 유지할 뿐 구독을 붙이지 않는다.
  // 다시 구독하지 않으면 그 대화의 새 메시지가 영영 오지 않는다. 소켓이 닫혀 있으면 다시 붙을 때(onopen) 구독한다.
  checkSubscription() {
    if (!this.convId || this.ws?.readyState !== WebSocket.OPEN) return;
    const now = Date.now();
    if (this.subscribed !== this.convId) {
      if (!this.retryAt && this.subSentAt && now - this.subSentAt > SUBSCRIBE_READY_TIMEOUT) this.retryLater();
      if (this.retryAt && now >= this.retryAt) {
        this.retryAt = 0;
        this.subscribe();
      }
      return;
    }
    if (this.retryAt && now >= this.retryAt) {
      this.retryAt = 0;
      if (document.visibilityState === "visible") this.reportRead(Math.max(this.state.lastSeq, this.readAgain));
      this.readAgain = 0;
    }
  }

  sendFrame(obj) {
    if (this.ws?.readyState !== WebSocket.OPEN) return false;
    this.ws.send(JSON.stringify(obj));
    return true;
  }

  // send 는 큐에 먼저 넣고 보낸다. 큐에서 지우는 건 ack 를 받은 뒤다.
  // opts: { internal, attachment } — attachment 는 먼저 업로드해 둔 메타데이터다.
  send(body, opts = {}) {
    const text = (body || "").trim();
    if (!this.convId) return null;
    if (!text && !opts.attachment) return null;
    if ([...text].length > MAX_BODY_RUNES) {
      // 서버도 거절하지만, 그 전에 프레임 크기 한도에 걸려 소켓이 끊긴다. 여기서 막는다.
      this.h.onError?.(`본문은 ${MAX_BODY_RUNES}자까지입니다`);
      return null;
    }
    const item = {
      client_msg_id: newClientMsgId(),
      body: text,
      at: Date.now(), // 기다린 시간을 잰다(checkStall). 순서는 n 이다
      n: Math.max(maxOrder(this.convId, this.prefix), ...this.state.pending.map((p) => Number(p.n) || 0)) + 1,
      tries: 0,
      internal: !!opts.internal,
      attachment: opts.attachment || null,
    };
    this.state.pending.push(item);
    if (!saveItem(this.convId, item, this.prefix)) {
      this.keepUnsaved(this.convId, item);
      // 저장이 막힌 환경이다. 보내긴 하지만(다른 대화로 옮겨 가도 이 탭에 남는다) 탭을 닫거나 새로고침하면 사라진다는 걸 알린다.
      this.h.onError?.(UNSAVED_NOTICE);
    } else {
      this.persistUnsaved(); // 저장이 된다. 앞서 저장하지 못한 글도 지금 옮긴다
    }
    this.h.onPending?.(this.state.pending);
    this.flush();
    return item;
  }

  // uploadAttachment 는 파일을 먼저 올린다. 전송과 나눠야 재전송이 파일을 다시 올리지 않는다.
  // 세 걸음이다: 주소를 받고, 저장소로 직접 올리고, 완료를 알린다. 서버는 파일 바이트를 받지 않는다.
  // opts.onProgress(loaded, total) 를 주면 저장소로 올리는 동안 보낸 바이트를 알린다. 영상(최대 100MB)은 오래 걸린다(D37).
  // 진행률은 fetch 로 알 수 없어 그때만 XMLHttpRequest 로 올린다. 판정(크기 초과면 거절, 그 밖은 완료 확인에 맡김)은 같다.
  // 크기 초과는 로컬 저장소의 413 과 S3 의 400 EntityTooLarge(양식의 content-length-range 위반) 둘이다.
  // 올리기 방식은 서버가 준 upload.method 를 따른다(D42). POST 는 양식 필드 뒤에 file 을 붙이고, PUT 은 파일을 본문으로 보내며
  // upload.headers 를 그대로 싣는다(서명된 Content-Type). method 가 없으면 POST 다(D42 이전 서버).
  // opts.signal(AbortSignal)을 주면 화면이 업로드를 멈출 수 있다. 멈추면 저장소 올리기를 끊고 완료를 부르지 않으며 code "aborted" 로 끝난다.
  // 이 함수는 첨부 업로드만 맡는다. 전송 대기 큐(pending)·재전송과는 관계가 없다.
  async uploadAttachment(convId, file, opts = {}) {
    const { signal } = opts;
    const stopIfAborted = () => {
      if (signal?.aborted) throw Object.assign(new Error("업로드를 취소했습니다"), { code: "aborted" });
    };
    stopIfAborted();
    const size = await imageSize(file);
    const started = await this.api(`/api/conversations/${convId}/attachments`, {
      filename: file.name, content_type: file.type || "application/octet-stream", byte_size: file.size, ...size,
    });
    stopIfAborted();
    const req = storeRequest(started.upload, file);
    try {
      const res = opts.onProgress && typeof XMLHttpRequest !== "undefined"
        ? await sendWithProgress(req, opts.onProgress, signal)
        : await sendWithFetch(req, signal);
      if (isTooLarge(res)) throw Object.assign(new Error("파일이 허용된 크기를 넘습니다"), { code: "invalid" });
    } catch (err) {
      // 저장소가 다른 출처면 응답을 못 읽을 수 있다. 올라갔는지는 아래 완료 확인이 판정한다.
      if (err.code === "invalid") throw err;
    }
    stopIfAborted();
    return this.completeUpload(started.attachment.id);
  }

  // completeUpload 는 완료 확인을 부른다. busy 는 같은 첨부를 다른 요청(앞서 끊긴 시도 등)이 확인하는 중이라는 뜻이라
  // 잠깐 뒤 다시 부르면 그 결과를 받는다. 다시 불러도 busy 면 오류로 알린다. 다른 거절은 바로 알린다.
  // 503 storage_timeout 과 400 "파일을 찾지 못했다" 는 같은 첨부를 다시 불러도 되지 않는다. 서버 문구(다시 올려라)를 그대로 보인다.
  async completeUpload(attachmentId) {
    for (let retry = 0; ; retry++) {
      try {
        const done = await this.api(`/api/attachments/${attachmentId}/complete`, {});
        return done.attachment;
      } catch (err) {
        if (err.code !== "busy" || retry >= COMPLETE_BUSY_RETRIES) throw err;
        await new Promise((r) => setTimeout(r, COMPLETE_BUSY_INTERVAL));
      }
    }
  }

  // api 는 첨부의 POST 다. 모듈의 api() 와 같은 규칙이다 — JSON 이 아닌 본문(앞단의 413 HTML 등)도 상태를 남기고 code 는 우리 서버의
  // JSON 본문에서만 온다. 우리 서버의 판정이 아닌 실패는 uploadFailure 가 상태별 문구로 바꾼다 — 전에는 JSON.parse 의 원문
  // ("Unexpected token '<'…")이 고객 알림에 그대로 떴다(감사 22).
  async api(path, body) {
    try {
      return await api(path, { token: this.token, method: "POST", body });
    } catch (err) {
      throw uploadFailure(err);
    }
  }

  frameFor(item) {
    return {
      type: "send",
      conversation_id: this.convId,
      client_msg_id: item.client_msg_id,
      body: item.body,
      internal: item.internal || undefined,
      attachment_id: item.attachment?.id || undefined,
    };
  }

  flush() {
    if (!this.convId) return;
    for (const p of this.state.pending) this.sendFrame(this.frameFor(p));
  }

  // reportRead 는 "여기까지 봤다"를 올린다. 뒤로 가는 보고는 보내지 않고, 서버도 무시한다.
  reportRead(seq) {
    if (!this.convId || !seq || seq <= this.reportedRead) return;
    const convId = this.convId;
    this.reportedRead = seq;
    if (this.sendFrame({ type: "read", conversation_id: convId, seq })) return;
    api(`/api/conversations/${convId}/read`, { token: this.token, method: "POST", body: { seq } }).catch(() => {
      if (convId === this.convId) this.reportedRead = Math.min(this.reportedRead, seq - 1);
    });
  }

  // typing 은 입력할 때마다 부르면 된다. 간격은 여기서 끊는다.
  typing() {
    if (!this.convId) return;
    const now = Date.now();
    if (now - this.lastTypingSent < TYPING_SEND_INTERVAL) return;
    this.lastTypingSent = now;
    this.sendFrame({ type: "typing", conversation_id: this.convId });
  }

  // ── 고객 종료(D48) ─────────────────────────────────────────────

  // pendingOf 는 그 대화의 전송 대기 글이다. 같은 브라우저의 다른 탭이 저장소에 넣은 글까지 합친다(같은 client_msg_id 는 하나).
  pendingOf(convId) {
    const out = [...this.stateOf(convId).pending];
    const known = new Set(out.map((p) => p.client_msg_id));
    for (const p of loadState(convId, this.prefix).pending) if (!known.has(p.client_msg_id)) out.push(p);
    return out;
  }

  // closeConversation 은 고객이 상담을 닫는다. 유실 방지가 먼저다 — 보내지 못한 글이 있으면 먼저 보내고 저장 확인(REST 의 응답 = ack)을
  // 받은 뒤에 닫는다. 닫은 상담에는 고객 글이 거절되므로(closed_by_customer) 큐를 남긴 채 닫으면 그 글이 돌아온다.
  // 다른 탭이 저장소에 남긴 같은 상담의 글도 이 탭의 큐로 받아 같이 보낸다(서버가 멱등이라 겹쳐 나가도 한 번 저장된다).
  // 보낼 수 없으면(끊김·일시 실패·시한 초과) 닫지 않고 code "pending_unsent" 로 던진다 — 큐의 글은 그대로 남아 연결되면 나간다.
  // 닫기 요청의 실패는 api() 의 오류 그대로다(err.code·status·traceId). 성공하면 서버가 준 대화(고객 표현)를 돌려준다.
  async closeConversation(convId, { timeout = CLOSE_FLUSH_TIMEOUT } = {}) {
    const deadline = Date.now() + timeout;
    const unsent = (count) => Object.assign(new Error("보내지 못한 메시지가 있어 상담을 종료하지 않았습니다. 연결된 뒤 다시 시도해 주세요"),
      { code: "pending_unsent", count });
    if (convId === this.convId) this.syncPending();
    for (;;) {
      const before = this.pendingOf(convId).length;
      if (before === 0) break;
      if (Date.now() > deadline) throw unsent(before);
      if (this.pushing.has(convId)) {
        // 주기 확인이 이 대화를 미는 중이다. 끝나기를 기다렸다가 다시 본다 — 같은 글을 동시에 두 길로 밀지 않는다.
        await new Promise((r) => setTimeout(r, 50));
        continue;
      }
      await this.pushConversation(convId);
      // 하나도 줄지 않았으면 지금은 보낼 수 없다(서버가 답하지 않거나 일시 실패). 닫지 않는다.
      const after = this.pendingOf(convId).length;
      if (after >= before) throw unsent(after);
    }
    const out = await api(`/api/customer/conversations/${convId}/close`, { token: this.token, method: "POST" });
    return out?.conversation;
  }

  // ── 전송 대기 큐 ───────────────────────────────────────────────

  // stateOf 는 그 대화의 큐다. 보고 있는 대화면 이 탭의 큐, 아니면 저장소 큐에 이 탭이 저장하지 못한 글을 합친 것이다.
  stateOf(convId) {
    return convId === this.convId ? this.state : this.withUnsaved(convId, loadState(convId, this.prefix));
  }

  // findPending 은 그 글을 이 탭의 큐(보고 있는 대화면)나 메모리 큐에서, 없으면 저장소에서 찾는다. 다른 탭이 넣은 글도 찾는다.
  findPending(clientMsgId, convId) {
    const mine = convId === this.convId ? this.state.pending.find((p) => p.client_msg_id === clientMsgId) : undefined;
    return mine || this.unsaved.get(convId)?.get(clientMsgId)
      || loadState(convId, this.prefix).pending.find((p) => p.client_msg_id === clientMsgId);
  }

  // showPending 은 보고 있는 대화의 큐가 바뀌었을 때 화면에 알린다.
  showPending(convId) {
    if (convId !== this.convId) return;
    this.h.onPending?.(this.state.pending);
    this.checkStall();
  }

  // syncPending 은 보고 있는 대화의 큐를 저장소에 맞춘다(DEF-158). 지우는 것은 저장소에서 사라진 글뿐이다 — 저장소에서 사라졌다는 건
  // 어느 탭이 ack 나 영구 거절로 지웠다는 뜻이다. 이 탭이 저장하지 못한 글(unsaved)은 저장소에 없어도 남긴다.
  // 보고 있는 대화의 화면 큐만 맞춘다 — 메모리 큐(unsaved)는 고치지 않는다. 다른 대화의 저장하지 못한 글도 그대로다(DEF-175).
  // 이 탭의 순서는 그대로 두고 다른 탭이 넣은 글을 뒤에 붙인다.
  syncPending() {
    const convId = this.convId;
    if (!convId) return;
    const stored = loadState(convId, this.prefix).pending;
    const inStore = new Map(stored.map((p) => [p.client_msg_id, p]));
    const kept = this.state.pending.filter((p) => inStore.has(p.client_msg_id) || this.isUnsaved(convId, p.client_msg_id));
    const known = new Set(kept.map((p) => p.client_msg_id));
    const next = [...kept, ...stored.filter((p) => !known.has(p.client_msg_id))];
    const same = next.length === this.state.pending.length && next.every((p, i) => p === this.state.pending[i]);
    if (same) return;
    this.state.pending = next;
    this.showPending(convId);
  }

  onStorage(e) {
    if (!this.convId) return;
    if (e?.key != null) {
      const at = parseKey(this.prefix, e.key) || parseItem(this.prefix, e.key);
      if (!at || at.convId !== this.convId) return;
    }
    this.syncPending();
  }

  // dropPending 은 그 글을 큐(저장소의 그 글 키와 이 탭의 메모리)에서 지우고 지운 항목을 돌려준다(없으면 undefined).
  // 거절 알림이 그 본문을 화면에 넘긴다. 다른 탭의 큐는 저장소 이벤트로 이 삭제를 본다.
  dropPending(clientMsgId, convId = this.convId) {
    if (!convId) return;
    const item = this.findPending(clientMsgId, convId);
    if (!item) return;
    removeItem(convId, clientMsgId, this.prefix);
    this.forgetUnsaved(convId, clientMsgId);
    if (convId === this.convId) {
      this.state.pending = this.state.pending.filter((p) => p.client_msg_id !== clientMsgId);
      this.showPending(convId);
    }
    return droppedItem(item, convId);
  }

  // giveUp 은 거절로 큐에서 지우고 화면에 알린다. 그 글을 이미 받은 메시지로 봤으면(서버가 저장했고 ack 만 못 받았다)
  // 돌려주지 않고 조용히 지운다 — 돌려주면 고객이 다시 보내 같은 글이 두 번 남는다. 볼 수 있는 것은 보고 있는 대화뿐이다.
  giveUp(clientMsgId, convId, message, code, traceId) {
    const dropped = this.dropPending(clientMsgId, convId);
    if (this.savedIds.has(clientMsgId)) return;
    this.h.onError?.(message, clientMsgId, dropped && { ...dropped, code }, traceId);
  }

  // flushViaRest 는 WebSocket 이 못 하는 몫을 맡는다.
  //   1. 연결이 끊긴 동안의 현재 대화
  //   2. 더 이상 보고 있지 않은 옛 대화에 남은 것 (대화를 바꿔도 버리지 않는다)
  //   3. 소켓은 열려 있는데 오래 ack 가 없는 것 — 서버가 일시 거절한 메시지는 다음 ready 까지 기다리면 안 된다
  // 대화마다 따로 민다. 한 대화의 요청이 끝나지 않거나 그 대화가 백오프 중이어도 다른 대화는 제 차례에 나간다.
  // 한 대화 안에서는 순서를 지킨다 — 앞 글이 막히면 뒤 글은 그 대화의 다음 차례를 기다린다.
  // due 면(주기 확인) 다시 밀 차례가 된 대화만 민다. 없으면(처음 시작할 때) 기다리던 간격과 상관없이 모두 민다.
  async flushViaRest({ due = false } = {}) {
    if (this.unsaved.size > 0) this.persistUnsaved();
    // 이 탭이 저장하지 못한 글이 있는 대화도 민다(메모리 큐, DEF-175). 저장소 큐와 같은 규칙이다.
    const targets = new Set([...storedConversations(this.prefix), ...this.unsaved.keys()]);
    if (this.ws?.readyState === WebSocket.OPEN) {
      const stale = this.state.pending.some((p) => Date.now() - (p.at || 0) > REST_RETRY_AFTER);
      if (!stale) targets.delete(this.convId);
    }
    const now = Date.now();
    const runs = [];
    for (const convId of targets) {
      if (this.stopped) break;
      if (this.pushing.has(convId)) continue; // 앞 시도가 아직 안 끝났다
      if (due && (this.restWait.get(convId)?.at || 0) > now) continue;
      runs.push(this.pushConversation(convId));
    }
    await Promise.all(runs);
  }

  async pushConversation(convId) {
    this.pushing.add(convId);
    try {
      await this.pushPending(convId);
    } finally {
      this.pushing.delete(convId);
    }
  }

  // resetRestWait 는 연결이 돌아왔을 때 기다리던 간격을 버린다. 서버가 정한 Retry-After 는 지킨다.
  resetRestWait() {
    for (const [convId, w] of this.restWait) if (!w.hard) this.restWait.delete(convId);
  }

  // waitRest 는 그 대화를 다음에 언제 밀지 정한다. 명시적 거절은 REST_TICK 뒤, 일시 실패는 백오프, 429 는 Retry-After 를 따른다.
  waitRest(convId, err) {
    if (!isTransientFailure(err)) {
      this.restWait.set(convId, { fails: 0, at: Date.now() + REST_TICK });
      return;
    }
    const fails = (this.restWait.get(convId)?.fails || 0) + 1;
    const hard = err.status === 429 && err.retryAfter > 0;
    this.restWait.set(convId, { fails, hard, at: Date.now() + (hard ? err.retryAfter * 1000 : restBackoff(fails)) });
  }

  async pushPending(convId) {
    const queued = [...this.stateOf(convId).pending];
    for (const item of queued) {
      if (this.stopped) return;
      try {
        const res = await api(`/api/conversations/${convId}/messages`, {
          token: this.token,
          method: "POST",
          body: {
            client_msg_id: item.client_msg_id,
            body: item.body,
            internal: !!item.internal,
            attachment_id: item.attachment?.id || "",
          },
        });
        this.restReplyAt = 0; // 보내졌다. 막혀 있던 것이 풀렸다
        this.dropPending(item.client_msg_id, convId);
        // 보고 있는 대화일 때만 화면에 올린다. 다른 대화 메시지를 섞으면 커서가 망가진다.
        if (convId === this.convId) this.ingest(res.message);
      } catch (err) {
        // 답이 왔는지(상태가 있는지)를 기억한다. 답이 오면 연결은 살아 있고 전송만 막힌 것이다(checkStall 의 offline 판정).
        // 답은 왔는데 일시 실패(앞단의 HTML·5xx·429)면 전송이 막힌 것이다. 답이 없거나 우리 서버가 판정한 거절이면 지운다.
        this.restReplyAt = err?.status && isTransientFailure(err) ? Date.now() : 0;
        this.checkStall();
        if (isPermanentReject(err.code)) {
          this.giveUp(item.client_msg_id, convId, err.message, err.code, err.traceId);
          continue;
        }
        // 네트워크·5xx 는 세지 않는다. 큐에 남아 다음 주기에 같은 client_msg_id 로 다시 나간다.
        if (!isTransientFailure(err)) this.countFailure(convId, item.client_msg_id, err.message, err.code, err.traceId);
        // 이 대화는 여기서 멈춘다(뒤 글이 앞 글을 앞지르지 않게). 다음 차례는 waitRest 가 정한다. 다른 대화와는 상관없다.
        this.waitRest(convId, err);
        return;
      }
    }
    this.restWait.delete(convId);
  }

  // countFailure 는 서버가 명시적으로 거절한 실패를 센다. 같은 거절이 계속되면 사용자가 모르는 채 묻히는 걸 막는다.
  // 네트워크 실패·5xx·일시 오류 프레임은 여기 오지 않는다(isTransientFailure). 포기했으면 true 다(그 알림을 이미 했다).
  countFailure(convId, clientMsgId, reason, code, traceId) {
    const item = this.findPending(clientMsgId, convId);
    if (!item) return false;
    item.tries = (item.tries || 0) + 1;
    if (item.tries >= MAX_SEND_TRIES) {
      this.giveUp(clientMsgId, convId, `보내지 못했습니다: ${reason}`, code, traceId);
      return true;
    }
    updateItem(convId, item, this.prefix);
    if (convId === this.convId) {
      const mine = this.state.pending.find((p) => p.client_msg_id === clientMsgId);
      if (mine && mine !== item) mine.tries = item.tries;
    }
    return false;
  }

  // ── 수신 ──────────────────────────────────────────────────────

  handleFrame(f) {
    switch (f.type) {
      case "ready":
        if (f.conversation_id !== this.convId) return;
        // 구독이 붙었다. 다시 시도하던 것을 거둔다 — 아래에서 읽음도 지금 지점으로 보낸다.
        this.subscribed = this.convId;
        this.subSentAt = 0;
        this.subSince = 0;
        this.retryFails = 0;
        this.retryAt = 0;
        this.readAgain = 0;
        // 서버 last_seq 가 내가 가진 것보다 작으면 내 기록이 틀린 거다. 서버에 맞춘다.
        if (this.state.lastSeq > f.last_seq) {
          this.state.lastSeq = f.last_seq;
          saveCursor(this.convId, this.state.lastSeq, this.prefix);
        }
        this.backfillFails = 0;
        this.h.onReady?.(f);
        if (f.conversation) this.h.onConversation?.(f.conversation);
        this.h.onReads?.(f.reads || []);
        this.flush();
        if (document.visibilityState === "visible") this.reportRead(f.last_seq);
        break;

      case "message":
        if (f.conversation_id !== this.convId) return;
        this.ingest(f);
        break;

      case "ack":
        // 서버가 커밋을 끝냈다는 뜻이다. 이제 큐에서 지워도 안전하다.
        if (f.conversation_id && f.conversation_id !== this.convId) {
          this.dropPending(f.client_msg_id, f.conversation_id);
          break;
        }
        this.dropPending(f.client_msg_id);
        break;

      case "conversation":
        // 상태·담당자·태그는 메시지가 아니라서 번호로는 안 보인다. 화면을 여기서 맞춘다.
        if (f.conversation_id !== this.convId) return;
        this.h.onConversation?.(f.conversation || f);
        break;

      case "typing":
        if (f.conversation_id !== this.convId) return;
        this.h.onTyping?.(f);
        break;

      case "read":
        if (f.conversation_id !== this.convId) return;
        this.h.onRead?.(f);
        break;

      case "inbox_changed":
        this.h.onInboxChanged?.();
        break;

      case "error":
        // 대화가 없다는 답이다. 메시지 하나의 거절(client_msg_id)이 아니면 구독·읽음·타이핑에 대한 답이고,
        // 보던 대화의 것이면 대화를 닫는다. 이미 떠난 대화의 것이면 버린다 — 오류 토스트로 띄울 일이 아니다.
        if (f.code === "not_found" && !f.client_msg_id) {
          const about = f.conversation_id || this.convId;
          if (about && about === this.convId) this.gone();
          break;
        }
        if (f.client_msg_id && isPermanentReject(f.code)) {
          this.giveUp(f.client_msg_id, this.convId, f.message, f.code, f.trace_id);
          break;
        }
        // 영구 거절이 아니면 큐에 남긴다. REST 주기가 다시 민다. 일시 오류(internal 등)는 세지 않고 알리지도 않는다 —
        // 그 글은 포기하지 않았고 같은 client_msg_id 로 다시 나간다. "다시 시도해라" 를 띄우면 사람이 다시 써서 두 번 저장된다(DEF-01).
        // 늦어지면 끊김 안내(onStalled)가 보인다. 글과 무관한 일시 오류(구독·읽음)도 알리지 않는다 — 구독은 백오프로 다시 하고
        // 읽음은 보고 지점을 되돌려 다시 보낸다(retryLater, DEF-72). 오래 구독하지 못하면 끊김 안내가 보인다.
        // onError 는 서버가 거절했거나 글을 포기했을 때만 부른다. 명시적인 거절(forbidden·conflict 등)은 세어 상한을 넘기면 포기하고 알린다.
        if (isTransientFailure({ code: f.code })) {
          if (!f.client_msg_id) this.retryLater();
          break;
        }
        if (f.client_msg_id && this.countFailure(this.convId, f.client_msg_id, f.message, f.code, f.trace_id)) break;
        this.h.onError?.(f.message, f.client_msg_id, undefined, f.trace_id);
        break;
    }
  }

  // ingest 는 seq 연속성을 지킨다. 구멍이 보이면 REST 로 메우고 나서 이어 붙인다.
  ingest(msg) {
    if (!msg || msg.conversation_id !== this.convId) return;
    this.noteSaved(msg);
    this.buffer.push(msg);
    this.pump();
  }

  pump() {
    if (this.backfilling) return;
    this.buffer.sort((a, b) => a.seq - b.seq);
    while (this.buffer.length) {
      const next = this.buffer[0];
      if (next.seq <= this.state.lastSeq) {
        this.buffer.shift(); // 이미 본 메시지다. 재전송 echo 거나 중복 전달이다
        continue;
      }
      if (next.seq > this.state.lastSeq + 1) {
        this.scheduleBackfill(0);
        return;
      }
      this.buffer.shift();
      this.state.lastSeq = next.seq;
      saveCursor(this.convId, this.state.lastSeq, this.prefix);
      this.h.onMessage?.(next);
    }
    // 화면이 보이는 동안 들어온 메시지는 읽은 것으로 올린다.
    if (document.visibilityState === "visible") this.reportRead(this.state.lastSeq);
  }

  // scheduleBackfill 은 갭 복구를 타이머로만 돌린다.
  // 실패했다고 곧바로 다시 부르면 await 체인이 매크로태스크 큐를 굶겨 탭 전체가 멈춘다.
  scheduleBackfill(delay) {
    if (this.backfilling || this.stopped || !this.convId) return;
    clearTimeout(this.backfillTimer);
    this.backfillTimer = setTimeout(() => this.backfill(), delay);
  }

  async backfill() {
    if (this.backfilling || !this.convId) return;
    const convId = this.convId; // 끝날 때 대화가 바뀌었으면 결과를 버린다
    this.backfilling = true;
    try {
      for (;;) {
        const res = await api(
          `/api/conversations/${convId}/messages?after_seq=${this.stateOf(convId).lastSeq}&limit=${PAGE}`,
          { token: this.token },
        );
        if (convId !== this.convId) return; // 다른 대화로 옮겨 갔다
        const msgs = res.messages || [];
        for (const m of msgs) {
          this.noteSaved(m);
          if (m.seq <= this.state.lastSeq) continue;
          this.state.lastSeq = m.seq;
          this.h.onMessage?.(m);
        }
        saveCursor(convId, this.state.lastSeq, this.prefix);
        if (msgs.length < PAGE) break;
      }
      this.backfillFails = 0;
    } catch (err) {
      if (convId !== this.convId) return;
      if (err.code === "not_found") {
        this.backfilling = false;
        this.gone();
        return;
      }
      this.backfillFails += 1;
      const wait = Math.min(BACKFILL_RETRY_BASE * 2 ** (this.backfillFails - 1), BACKFILL_RETRY_MAX);
      this.h.onError?.(`지난 메시지를 불러오지 못했습니다 (${this.backfillFails}번째): ${err.message}`, undefined, undefined, err.traceId);
      this.backfilling = false;
      this.scheduleBackfill(wait);
      return;
    } finally {
      if (convId === this.convId) this.backfilling = false;
    }
    this.pump();
  }
}

// droppedItem 은 큐에서 지운 메시지를 화면에 넘길 모양으로 만든다. 큐 안의 시도 횟수·시각은 넘기지 않는다.
function droppedItem(item, convId) {
  return {
    client_msg_id: item.client_msg_id,
    conversation_id: convId,
    body: item.body || "",
    internal: !!item.internal,
    attachment: item.attachment || null,
  };
}

export function formatTime(iso) {
  const d = new Date(iso);
  return d.toLocaleTimeString("ko-KR", { hour: "2-digit", minute: "2-digit" });
}

export function escapeText(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

// imageSize 는 이미지의 가로·세로를 잰다. 말풍선이 그림이 오기 전에 자리를 잡는 데 쓴다.
// 서버가 내용에서 직접 읽으면 그 값이 이긴다. 못 재면 비워 보낸다.
async function imageSize(file) {
  if (!file.type?.startsWith("image/") || typeof createImageBitmap !== "function") return {};
  try {
    const bmp = await createImageBitmap(file);
    const size = { width: bmp.width, height: bmp.height };
    bmp.close?.();
    return size;
  } catch {
    return {};
  }
}

// isTooLarge 는 저장소 응답이 크기 초과인지 본다. 로컬은 413, S3(와 MinIO)는 400 에 본문 <Code>EntityTooLarge</Code> 다.
// 400 의 본문을 못 읽으면(다른 출처) 크기 초과라고 하지 않는다. 그때는 완료 확인이 판정한다.
function isTooLarge({ status, text }) {
  return status === 413 || (status === 400 && /<Code>\s*EntityTooLarge\s*<\/Code>/.test(text || ""));
}

// storeRequest 는 서버가 준 올리기 양식으로 저장소에 보낼 요청 { method, url, headers, body } 를 만든다(D42).
// PUT 이면 파일이 본문이고 headers 를 그대로 싣는다. Content-Length 는 브라우저가 본문으로 정한다(스크립트가 실을 수 없다).
// 그 밖(POST, method 없음)은 양식 필드를 싣고 마지막에 file 을 붙인다.
function storeRequest(upload, file) {
  if (String(upload.method || "").toUpperCase() === "PUT") {
    return { method: "PUT", url: upload.url, headers: { ...(upload.headers || {}) }, body: file };
  }
  const form = new FormData();
  for (const [k, v] of Object.entries(upload.fields || {})) form.append(k, v);
  form.append("file", file, file.name); // 파일은 마지막 필드여야 한다
  return { method: "POST", url: upload.url, headers: {}, body: form };
}

// sendWithFetch 는 진행률 없이 올리고 { status, text } 를 돌려준다. 본문은 400 일 때만 읽는다(크기 초과 판정용).
async function sendWithFetch(req, signal) {
  const res = await fetch(req.url, { method: req.method, headers: req.headers, body: req.body, signal });
  const text = res.status === 400 ? await res.text().catch(() => "") : "";
  return { status: res.status, text };
}

// sendWithProgress 는 올리며 보낸 바이트를 onProgress(loaded, total) 로 알리고 { status, text } 를 돌려준다.
// 다른 출처의 응답을 못 읽거나 네트워크가 끊기면 상태 0 이다. 올라갔는지는 완료 확인이 판정한다.
// signal 이 멈추라고 하면 XHR 을 끊는다(abort). 그때도 상태 0 으로 끝나고, 부른 쪽이 signal 을 보고 완료를 부르지 않는다.
function sendWithProgress(req, onProgress, signal) {
  return new Promise((resolve) => {
    const xhr = new XMLHttpRequest();
    const stop = () => xhr.abort();
    xhr.open(req.method, req.url);
    for (const [k, v] of Object.entries(req.headers)) xhr.setRequestHeader(k, v);
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) onProgress(e.loaded, e.total);
    };
    const done = (status) => {
      signal?.removeEventListener?.("abort", stop);
      let text = "";
      try { text = status === 400 ? xhr.responseText || "" : ""; } catch { /* 못 읽는 응답이다 */ }
      resolve({ status, text });
    };
    xhr.onload = () => done(xhr.status);
    xhr.onerror = () => done(0);
    xhr.onabort = () => done(0);
    signal?.addEventListener?.("abort", stop, { once: true });
    xhr.send(req.body);
    if (signal?.aborted) stop();
  });
}
