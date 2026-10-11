# go-chat-system

상담 채팅 플랫폼. 고객 위젯과 상담원 데스크가 같은 대화를 양쪽에서 연다.
**메시지를 잃지 않는 것을 1순위 품질 속성으로 두고 설계했다.** logcat 의 개인 포트폴리오이고, 다음 단계로 AI 상담사를 붙인다(데모 무대는 1:1 베이스 레슨 스튜디오 둠둠디라).

Go 1.27 + Postgres 17, 외부 의존은 pgx·websocket·x/crypto 와 OpenTelemetry 공식 모듈, 실행 파일 하나.

## 띄우기

```bash
make up        # Postgres 를 Docker 로 띄운다 (호스트 포트 56432)
make run       # migrate(데모 계정 agent@example.com / chat1234 포함) → 서버 시작 → http://localhost:8090
```

실행 파일 하나가 명령 셋을 한다(design.md D49). 서버는 DB 에 쓰지 않고, 스키마는 배포의 앞 단계가 바꾼다.

| 명령 | 하는 일 |
|---|---|
| `server` | 서빙. 스키마를 읽기만 한다 — 모자라면 `/readyz` 만 503 `schema_behind`. DB 가 없어도 뜬다 |
| `server migrate` | 마이그레이션 + 보정(시드는 `CHAT_SEED_DEMO=true` 일 때만, 관리자 0명 보정, 빈 회원 서명 비밀 채우기). 배포의 앞 단계(ECS 1회성 태스크)가 돌린다. 끝나면 종료 코드 0 |
| `server bootstrap` | 보정만(마이그레이션 없이). 스키마가 모자라면 아무것도 쓰지 않고 실패한다 |

배포 모양으로 보려면 앱까지 컨테이너로 띄운다. compose 가 같은 이미지로 `migrate` 를 먼저 돌리고 끝나면 앱을 띄운다.

```bash
docker compose --profile app up --build   # Dockerfile 로 빌드, migrate → app, 헬스체크 포함
```

## 사이트에 붙이기

우리 사이트 페이지의 `</body>` 앞에 한 줄 넣는다. 위젯은 iframe 안에서 돌아 호스트 CSS 와 섞이지 않고,
호스트 페이지는 우리 토큰을 읽을 수 없다. 데스크의 설정 > 위젯·설치 에서 `CHAT_PUBLIC_URL` 주소로 만든 설치 코드를 그대로 복사한다.
설치 하나에 워크스페이스 하나라 위젯 키가 없다. 옛 설치 코드에 `data-key` 가 남아 있어도 읽지 않고 그대로 뜬다(위젯 키는 제거됨, design.md D35).

```html
<script src="https://chat.example.com/embed.js"></script>
```

색은 관리자가 데스크에서 정한 색이다. `data-color` 를 주면 그 색이 관리자 색을 덮는다(옛 설치 코드 호환). `data-position="left"` 로 왼쪽에 붙인다.

로그인 회원을 넘기려면 호스트 **서버**가 `CHAT_MEMBER_SECRET` 으로 서명한 토큰을 준다.
브라우저는 비밀을 모르니 "이 방문자가 회원 1234다" 를 지어낼 수 없다.

```html
<script>ChatWidget.identify("<서명된 회원 토큰>")</script>
```

회원이 우리 사이트에서 로그아웃할 때 위젯도 로그아웃한다. 같은 브라우저를 쓰는 다음 사람에게 회원의 상담이 남지 않는다(D46).

```html
<script>ChatWidget.logout()</script>
```

회원 토큰의 `exp` 를 짧게 주면 위젯 세션도 그때 끝난다. 열어 둔 위젯이 끊기지 않게 새 회원 토큰을 받는 함수를 준다(감사 20).
없으면 위젯은 다시 시도하지 않고 "로그인이 만료되었습니다. 페이지를 새로고침해 주세요" 를 보인다. 예시는 [docs/api.md](docs/api.md) 11절.

```html
<script>ChatWidget.boot({ getMemberToken: async () => (await fetch("/account/chat-member-token")).text() })</script>
```

토큰 모양은 `base64url(json) + "." + base64url(hmac-sha256)` 이고, json 은
`{"member_id","name","email","phone","exp"}` 다. 서명 함수는 `internal/app/member.go` 의 `SignMember` 와 같다.

| 주소 | 화면 |
|---|---|
| `/widget` | 고객 위젯. 오른쪽 아래 런처를 누르면 메신저가 열린다 |
| `/desk` | 상담원 데스크. 시드 계정 `agent@example.com` / `chat1234`. 로그인 칸은 비어 있고 첫 화면 `/` 에도 이 계정이 적혀 있지 않으니 직접 넣는다 |

두 탭을 나란히 열고 양쪽에서 입력해 보면 동작이 보인다. 데스크의 연결 표시 점을 보면서 서버를
`Ctrl+C` 로 죽이고 위젯에서 메시지를 몇 개 더 보내 보면, 서버를 다시 띄웠을 때 그 메시지들이
그대로 올라간다. 이게 이 프로젝트의 핵심이다.

검증은 한 번에 돈다.

```bash
./scripts/verify.sh   # gofmt → go mod tidy → vet → golangci-lint → build → npm(css:check · lint · test) → Postgres → go test -race → 가이드 대조
```

클라이언트 테스트는 `node --test` 로 돈다. 브라우저 쪽이 유실 방지의 당사자라서 거기도 그물이
필요하다. Node 24 이상이 있어야 한다(CI 와 같은 판, package.json 의 engines). golangci-lint 는 verify.sh 가 고정한 판(2.14.0)이어야 한다.

## 워크스페이스 하나 (설치형)

한 팀이 직접 설치해 쓰는 플랫폼이다(design.md D21). 설치 하나에 **워크스페이스 하나**(마이그레이션이 만든 `default`)다. 위젯·회원 세션·로그인·상담원 가입처럼
토큰 없이 들어오는 입구는 전부 이 워크스페이스로 간다. 첫 관리자는 시드 상담원이고, 팀은 초대와 상담원 가입 승인으로 들인다.
회사 셀프 가입("워크스페이스 만들기", `POST /api/signup`)·워크스페이스 고르기·위젯 키는 제거됨(design.md D35). 나중에 필요하면 다시 만든다 —
내부 `workspace_id` 컬럼과 모든 쿼리의 테넌트 범위 조건(I11)은 그대로 남아 있다.

- **허용 사이트** — 설정 > 워크스페이스 에 `https://shop.example.com` 처럼 적으면 그 사이트에서만 위젯이 뜬다(세션 생성의 오리진 검사와 `/widget` 의 CSP `frame-ancestors`).
- **플랜은 없다** — 설치형이라 상담원 수도, 한 달의 새 문의 수도 막지 않는다. 막는 것은 남용 방어(입구마다 IP 당 한도, 입구끼리 따로 센다)와
  자원 보호용 상한(업로드 크기·페이지 크기·승인 대기 수)뿐이다(design.md D21).
- **감사 로그·내보내기·고객 삭제** — 설정 > 감사 로그 / 워크스페이스. 고객 삭제는 익명화라 대화 번호가 안 바뀐다.
- `CHAT_DEFAULT_WORKSPACE`·`CHAT_SIGNUP_OPEN` 은 제거됨(D35). 남아 있어도 읽지 않는다.

## 구조

```
adapter (pgstore · httpapi · realtime · filestore · ollama)  →  app (유스케이스·포트)  →  core (타입·규칙)
```

의존은 한 방향이고 어댑터끼리는 서로를 모른다. 포트는 쓰는 쪽(`internal/app/ports.go`)이 정의한다. 명령(`Commands`)과
조회(`Queries`)는 타입이 갈라서, 조회 경로에 쓰기를 넣으면 컴파일이 안 된다. 이 규칙은 `internal/archtest` 가 import 를 읽어
검사한다. 근거는 [docs/design.md](docs/design.md) D23, 과정은 [가이드 13](docs/guide/13-구조-core-app-adapter.md).

## 화면 개발

화면은 빌드 없는 바닐라 JS(`web/assets/*.js`)이고, 스타일은 `web/styles/` 의 SCSS 디자인 시스템이다(design.md D44, ADR 0005).
SCSS 를 컴파일한 `web/assets/app.css`·`widget.css` 는 리포에 들어 있어서 **서버 빌드·Docker·배포에는 Node 가 필요 없다.**
스타일이나 화면 코드를 고칠 때만 Node 24 이상과 아래 명령을 쓴다.

```bash
npm ci              # 개발 도구(sass, eslint, stylelint). 한 번만
npm run css         # web/styles/*.scss → web/assets/app.css·widget.css. 컴파일 결과도 같이 커밋한다
npm run css:check   # 컴파일 결과가 커밋된 CSS 와 다르면 실패
npm run lint        # ESLint(화면 JS·테스트) + Stylelint(SCSS). 경고도 실패다
npm test            # 클라이언트 테스트(node --test web/assets/*.test.mjs)
```

| 층 | 위치 | 담는 것 |
|---|---|---|
| 토큰 | `web/styles/tokens/` | 색(브랜드 팔레트·어두운 테마·아바타 톤·덮개), 간격 4px 단위와 반 단계(2px), 글자 크기 10~28px, 모서리(4px 기준·2px·알약·원), 그림자, 쌓임, 움직임, 평면 글리프 |
| 도구 | `web/styles/tools/` | 믹스인(`truncate`, `glyph`, `hover`, `below`, `tap-target` …). CSS 를 내보내지 않는다 |
| 기본 | `web/styles/base/` | `:root` 변수, 리셋, 글자, 유틸리티(`.mt-2`, `.text-end`, `.nowrap` … 맨 끝에 컴파일) |
| 컴포넌트 | `web/styles/components/` | 버튼·입력·칩·배지·아바타·카드·섹션 머리·토스트·모달·메뉴·탭·알약·아코디언·표·메시지·첨부·작성칸 (위젯·데스크 공용) |
| 배치 | `web/styles/layouts/` | 데스크(위 막대·받은함 네 단·대화)·설정·로그인 |
| 위젯 | `web/styles/pages/widget/` | 위젯 전용. 선택자는 전부 `body.widget` 아래 → `widget.css` |

색·모서리·그림자는 CSS 변수(`var(--brand)`, `var(--r)`)로, 간격·글자 크기는 Sass 함수(`space(2.5)`, `font-size("md")`)로 쓴다.
토큰 파일 밖의 색 글자(`#hex`·`rgb()`·색 이름), 글자 크기 숫자(`13px`), 토큰에 셈을 붙인 값(`space(n) ± Npx`, `calc(#{space(n)} + Npx)`, `space(n) * k`), 간격 속성(padding·margin·gap)의 생 px, 토큰 밖의 모서리는 Stylelint 가 막는다. 칸이 모자라면 토큰에 단계를 더한다.
관리자 색은 실행 중에 `ui.js` 의 `applyBrand` 가 바꾼다.
화면 마크업과 화면 코드는 인라인 스타일을 쓰지 않는다(정책 645 — `design-system.test.mjs` 와 ESLint 가 막는다). 필요한 모양은 컴포넌트·변형·유틸리티 클래스로 만든다.
예외는 데이터에 따라 값이 정해지는 넷(사진 비율, 입력칸 자동 높이, 업로드 진행률 폭, 관리자 색 CSS 변수)과 고객 사이트에서 도는 `embed.js` 다.
위젯은 공용 컴포넌트의 색을 다시 칠하지 않는다 — 다른 모양은 컴포넌트의 변형(`.log-plain`, `.composer-boxed`)으로 고른다.

## 기록을 지우지 않는다

- **수정·삭제** — 자기 메시지를 고치고 지운다. 원문은 이력에 남고, 변화는 같은 로그의 이벤트 행으로 전달된다(번호에 빈칸이 없다).
- **시스템 줄·읽음** — 배정·종료 같은 시스템 줄은 상담원만 본다. 고객 화면의 "읽음" 은 상담원이 **답한 지점**까지만 찍힌다.
- **상담마다 대화 하나** — 종료 뒤 다시 문의하면 새 대화가 열린다. 상담원은 그 고객의 직전 상담 30줄을 이어 보고 더 불러온다.
- **업무 기록** — 누가 어떤 고객의 대화를 맡고·답하고·끝냈는지 남는다. 설정 > 업무 기록, `GET /api/agent/work`.
- **고객의 상담 종료** — 위젯 대화 머리의 "상담 종료" 로 고객이 직접 닫는다. 보내지 못한 글이 있으면 먼저 보내고 닫는다. 닫은 상담은 고객 쪽에서 읽기 전용(입력창 대신 새 문의하기)이고 상담원이 다시 열면 풀린다. 답 없이 닫으면 처리 주체는 고객 자체 해결이고 AI 처리율의 분자에 든다(D48).
- **상담 지표·만족도** — 상담을 끝내면 처리 주체(상담원·AI·고객 자체 해결·미응답)가, 답이 고객의 기다림을 닫으면 응답 기록이 같은 트랜잭션에 남는다. 종료된 상담에는 위젯 대화 안에 별점 카드가 붙는다(24시간 안 고치기). 설정 > 지표, `GET /api/agent/metrics`(D45).

## 메시지가 안 사라지는 이유

진실은 `messages` 테이블의 `(conversation_id, seq)` 로그다. WebSocket 은 본문을 옮기지 않고
"DB 를 다시 읽어라"만 옮긴다. 그래서 전달 경로에 잃을 페이로드가 없다.

```
브라우저 ──send(client_msg_id)──▶ 서버 ──UPDATE conversations ... RETURNING last_seq──▶ Postgres
   │                                 │                                                     │
   │                                 └── INSERT messages(seq) + pg_notify ─────────────────┘
   │                                                   커밋
   ◀─────── ack(seq) ────────────────┘  (커밋 전에는 ack 를 보내지 않는다)

Postgres ──NOTIFY──▶ 리스너 ──깨우기(용량 1)──▶ 구독자 ──SELECT seq > sentSeq──▶ 브라우저
                                     ▲
                                     └── 안전망이 5초마다 바뀐 대화를 모아 깨운다 (NOTIFY 유실 대비)
```

끊기는 지점마다 복구 수단이 하나씩 붙어 있다.

| 끊기는 곳 | 어떻게 되나 |
|---|---|
| 커밋 전 서버가 죽는다 | ack 가 없다. 브라우저가 `localStorage` pending 에서 같은 `client_msg_id` 로 다시 보낸다 |
| 커밋 후 ack 전에 죽는다 | 재전송이 저장된 그 행의 seq 를 되돌려준다. 중복 행이 생기지 않는다 |
| WebSocket 이 끊긴다 | 재접속하면서 `after_seq = 가진 마지막 seq` 로 구독해 그 뒤를 받는다 |
| NOTIFY 가 사라진다 | 안전망의 주기 재조회(서버당 한 번)가 바뀐 대화의 구독자를 깨운다. 유실이 아니라 최대 5초 지연이 된다 |
| 느린 소비자의 버퍼가 찬다 | 신호가 합쳐질 뿐이다. 다음 루프가 DB 에서 남은 걸 전부 읽는다 |
| 브라우저 탭을 닫는다 | pending 과 lastSeq 가 `localStorage` 에 남아 다음 진입에서 이어진다 |
| WebSocket 이 계속 안 붙는다 | 3초마다 REST `POST /messages` 로 밀어 넣는다. 멱등이라 겹쳐도 안전하다 |
| 연결이 반쯤 죽는다 (onclose 가 안 온다) | 조용한 시간이 14초를 넘으면 클라이언트가 스스로 끊고 다시 붙는다 |
| 배포·축소로 서버가 내려간다 | `/readyz` 가 먼저 503 이 되고, 처리 중인 저장을 끝낸 뒤 WebSocket 을 1001 로 닫는다. 브라우저는 백오프 없이 다른 서버로 다시 붙어 `after_seq` 로 이어 받는다 |
| 상담원이 보던 대화를 바꾼다 | 비동기 응답마다 대화 id 를 대조해서 남의 대화 커서를 덮지 않는다 |
| 상담원이 대화를 종료한다 | 고객이 다시 쓰면 같은 대화가 다시 열린다. 새 대화를 만들지 않는다 |
| 고객이 대화를 직접 종료한다 | 고객이 그 대화에 쓰면 409 `closed_by_customer` 로 거절한다(다시 열지 않는다). 상담원이 다시 열면 고객도 다시 쓴다(D48) |

전송 대기 큐에서 메시지를 지우는 조건은 둘뿐이다. 서버가 저장을 확인했거나, 다시 보내도 결과가
같은 영구 거절을 받았거나. 네트워크 오류와 5xx 는 전부 남긴다. 상태 코드 구간으로 추측하면
서버가 저장하지 않은 메시지를 지우게 되고 그게 곧 유실이다.

seq 를 `SEQUENCE` 나 `IDENTITY` 로 뽑지 않은 게 핵심이다. `nextval` 은 롤백해도 되돌아오지
않아서 번호에 빈칸이 생기고, 빈칸은 수신측에서 유실로 읽힌다. 대화 행을 `UPDATE` 해서 락을 쥔 채
발급하면 실패한 트랜잭션의 번호가 함께 되돌아간다. 자세한 근거는 [docs/design.md](docs/design.md) 의 D2.

## 운영에 필요한 것

- **팀** — 관리자가 초대 링크를 만들고, 상대가 비밀번호를 정해 들어온다. 상담원이 워크스페이스 주소로 직접 가입하면
  아르바이트로 승인 대기에 오르고 관리자가 승인한다. 역할은 관리자·직원·아르바이트 셋이고 코드는 역할이 아니라 권한을 묻는다
  (권한표는 [docs/design.md](docs/design.md) D29). 비활성화하면 열린 세션까지 끊긴다. 마지막 관리자는 강등·비활성화할 수 없다.
- **운영시간·자동 메시지** — 요일별 구간을 두면 밖에서 온 문의에 부재중 안내가 나간다.
  고객이 처음 말했고 아직 아무도 답하지 않았으면 접수 안내가 한 번 나간다. 둘 다 문장을 비우면 안 나간다.
- **AI 초안** — `CHAT_OLLAMA_URL` 을 주면 작성칸에 ✨ 버튼이 생긴다. 모델은 작성칸을 채울 뿐이고
  보내기는 사람이 누른다. 초안은 DB 에 남지 않는다. 판정은 코드, 모델은 표현.
- **입구 한도** — 토큰 없는 입구(세션 생성·로그인·초대·가입 일곱 곳)는 입구마다 따로 IP 당 분당 30회(`CHAT_ENTRY_RATE_PER_MINUTE`).
  같은 IP 가 고객 세션 생성 한도를 다 써도 상담원 로그인은 된다. 역프록시 뒤에 두면
  `CHAT_TRUSTED_PROXIES` 에 프록시 수를 적어야 실제 클라이언트 주소로 센다. 0 이면 `X-Forwarded-For` 를 믿지 않는다.
- **로그아웃** — 서버의 세션 행을 지운다. 브라우저에서 토큰만 지우면 상담원 세션은 서버에서 유휴 12시간(최대 720시간) 산다. 익명 고객 세션은 만료가 없어 로그아웃·고객 삭제로만 끝난다.

## 무엇이 들어 있나

**고객 위젯** — 플로팅 런처와 안 읽은 수 배지, 홈(인사·FAQ·최근 대화), 대화 목록, 여러 건의
상담을 따로 진행, 파일·이미지 첨부와 드래그앤드롭, 읽음 표시, 입력 중 표시, 연결 상태,
브라우저 알림과 소리.

**상담원 데스크** — 받은함 필터(답할 차례·내 담당·미할당·팔로잉·전체 대화·진행중·보류·완료·태그, 관리자는 삭제됨), 대화 목록과
안 읽은 수, 대화 뷰, 고객 프로필 패널(이름·연락처·속성·운영 메모), 담당자 배정과 팔로워,
태그, **내부 메모**, 빠른 답변(`/`), 전체 검색(`⌘K`), 상태 전환(완료·보류·재개), 파일 첨부.

### 보장 수준을 셋으로 나눴다

| 대상 | 저장 | 잃으면 |
|---|---|---|
| 메시지 | `messages` 로그, 번호 연속 | 안 된다. 재전송·캐치업으로 끝까지 복구한다 |
| 읽음 지점 | `conversation_reads`, 단조 증가 | 재접속하면 서버 값으로 맞춰진다 |
| 입력 중 | 저장하지 않는다 | 괜찮다. 1.5초 뒤 거짓이 되는 사실이다 |

읽음이 되돌아가면 안 읽은 수가 되살아나 상담원이 처리한 문의를 다시 연다. 그래서 `GREATEST` 로만
올린다. 타이핑은 받는 쪽 버퍼가 차면 버린다.

### 내부 메모는 번호를 따로 센다

메모와 공개 메시지가 번호를 같이 쓰면 고객이 받는 번호에 빈칸이 생긴다. 이 시스템의 클라이언트는
빈칸을 유실로 읽고 복구를 돌기 때문에, 메모 하나가 고객 브라우저를 영원히 헛돌게 만든다.
그래서 대화마다 번호를 두 벌 돌린다 — 전체용 `seq` 와 공개용 `public_seq`. 둘 다 같은 UPDATE
에서 발급해서 "연속·커밋 순서 일치" 성질이 양쪽에 그대로 산다. 자세한 건 [docs/design.md](docs/design.md) D6.

**안 들어 있다.** 서포트봇·FAQ 자동화, 상담 만족도 평가, 멘션 알림, 통계 화면. 여러 워크스페이스를 받는 입구(셀프 가입·위젯 키)는 걷어냈고 내부 테넌트 격리만 남았다(D19·D35).
플랜과 결제는 여러 팀을 받게 되면 그때 설계한다(D21).
지금 구조에 붙일 수 있고 유실 방지 설계를 바꾸지 않는다. 남은 목록은 [docs/product-gaps.md](docs/product-gaps.md).

## 얼마나 받아내나

동시 3000명 · 상담원 3명으로 쟀다. 자세한 건 [docs/loadtest.md](docs/loadtest.md).

상담원 데스크가 실제로 하는 조회와 내부 메모까지 포함한 수치다.

| 메시지/초 | ack p99 | 전달 p99 | 받은함 조회 p50 | 유실 | 번호 빈칸 |
|---|---|---|---|---|---|
| 100 (현실 수준) | 7ms | 8ms | 9ms | 0 | 0 |
| 1,000 | 48ms | 57ms | 10ms | 0 | 0 |
| 1,500 | 70ms | 84ms | 13ms | 0 | 0 |
| 2,000 | 3.5s | 4.2s | 1.1s | 0 | 0 |

유휴 상태로 3000명이 붙어 있을 때 DB 부하는 초당 64 트랜잭션, 메모리 239MB.
한계는 Postgres 트랜잭션 수(초당 9,000)였고, 커넥션 풀을 키워도 천장은 그대로였다.
**어느 구간에서도 번호에 빈칸은 생기지 않았다** — 밀릴 뿐 순서와 연속성은 깨지지 않는다.

측정하면서 고친 것: 받은함 갱신 신호를 묶기(데스크 3개만 열려도 지연이 초 단위로 밀렸다),
메시지당 DB 질의 5회 → 2~3회(알림이 번호를 싣는다), 유휴 안전망을 연결마다 돌리지 않고
한 번에 모아 돌리기(유휴 DB 부하 2,531 → 64/초), ping 한 번 실패로 연결을 끊지 않기,
WebSocket 경로의 내부 오류 로그 남기기.

```bash
make up && ./bin/server &
go run ./cmd/loadtest -customers 3000 -agents 3 -duration 75s -rate 2
```

## 확인한 것과 확인 안 한 것

로컬에서 확인했다. `go test -race` 와 클라이언트 테스트 전체 통과 — 한 대화에 동시 200건을 넣어
seq 가 `1..200` 연속인 것, 같은 `client_msg_id` 12개를 동시에 보내도 행이 하나이고 그다음 seq 가
2 인 것, WebSocket 으로 80건을 연속 발사해도 받은 seq 가 하나도 안 빠지는 것, NOTIFY 리스너를
아예 띄우지 않아도 주기 재조회만으로 도착하는 것, 끊긴 사이의 5건을 재접속 후 전부 받는 것,
내부 메모가 고객 쪽 WebSocket 으로 가지 않고 고객 번호에 빈칸을 남기지 않는 것,
남의 대화 첨부를 올리지도 내려받지도 못하는 것.

브라우저 두 개를 실제로 띄워 18단계 시나리오도 돌렸다 — 런처·첨부 이미지·내부 메모 격리·
입력 중·읽음·빠른 답변·배정·태그·프로필·검색·필터·오프라인 복구·보류/완료/재개·다중 대화.

운영 조건은 확인하지 않았다. 동시 접속 수백 이상에서의 DB 재조회 비용, 여러 인스턴스 배포,
Postgres 장애 복구 시간, 브라우저별 `localStorage` 한도는 측정한 적이 없다. 숫자가 필요하면
부하 테스트부터 해야 한다.

## 설정

| 환경변수 | 기본값 | 설명 |
|---|---|---|
| `CHAT_DATABASE_URL` | `postgres://chat:chat@127.0.0.1:56432/chat?sslmode=disable` | 로컬 compose 는 TLS 가 없어 `disable`. RDS 는 `sslmode=verify-full&sslrootcert=/etc/ssl/certs/rds-global-bundle.pem`(이미지에 CA 번들이 들어 있다. docs/deploy.md 3.2절) |
| `CHAT_TEST_DATABASE_URL` | `postgres://chat:chat@127.0.0.1:56432/chat_test?sslmode=disable` | `go test` 만 읽는다. 개발 서버의 DB(`chat`)와 따로 두고, 없으면 테스트가 만든다 |
| `CHAT_ADDR` | `:8090` | |
| `CHAT_CATCHUP_INTERVAL` | `5s` | 안전망이 바뀐 대화를 DB 에서 모아 구독자를 깨우는 주기. NOTIFY 를 놓쳤을 때의 지연 상한이다. 0 은 거부한다 |
| `CHAT_SESSION_TTL` | `720h` | 상담원 세션의 절대 수명이자 회원 세션 수명의 상한(회원 토큰 `exp` 와 이른 쪽). 익명 고객 세션에는 걸리지 않는다 — 만료가 없다(D46) |
| `CHAT_SESSION_IDLE_CUSTOMER` | `0` | 고객 세션(익명·회원)의 유휴 한도(D46). 0 이면 없다 — 몇 달 뒤 돌아와도 상담이 이어진다. 값을 주면 이만큼 안 쓴 고객 세션이 끝난다. 세션 행이 아니라 판정 때의 이 값으로 보므로 켜면 이미 있는 고객 세션 모두에 걸린다. 음수는 거부한다 |
| `CHAT_SESSION_IDLE_AGENT` | `12h` | 상담원 세션의 유휴 한도. 데스크를 열어 두면 WebSocket 재확인이 사용 시각을 올린다. 0 이하는 거부한다 |
| `CHAT_DB_MAX_CONNS` | `40` | 커넥션 풀 크기 |
| `CHAT_PUBLIC_URL` | `http://localhost:8090` | 초대 링크처럼 바깥에서 여는 주소. 트레이스·HTTP 지표의 서버 이름(`server.address`·`server.port`)도 여기서 온다 — 요청의 Host 헤더를 쓰지 않는다(D33) |
| `CHAT_ENTRY_RATE_PER_MINUTE` | `30` | 토큰 없는 입구 하나당 IP 당 분당 한도(입구마다 따로 센다) |
| `CHAT_TRUSTED_PROXIES` | `0` | 앞에 선 역프록시 수. 그만큼 `X-Forwarded-For` 오른쪽을 건너뛴 값을 클라이언트로 본다 |
| `CHAT_DEFAULT_WORKSPACE` | — | 제거됨(D35). 입구는 늘 기본 워크스페이스다. 남아 있어도 읽지 않는다 |
| `CHAT_SIGNUP_OPEN` | — | 제거됨(D35). 회사 셀프 가입이 없다. 상담원 가입은 설정 `agent_signup` 으로 여닫는다 |
| `CHAT_MEMBER_SECRET` | 없음 | 기본 워크스페이스의 회원 서명 비밀 초기값. 비어 있을 때 한 번만 쓴다. 주면 32바이트 이상이어야 한다(`openssl rand -hex 32`, DEF-93) |
| `CHAT_SEED_DEMO` | `false` | `true` 면 `server migrate`·`server bootstrap` 이 시드 상담원(`CHAT_SEED_AGENT_*`)·빠른 답변을 만든다. 서버 기동은 시드하지 않는다(D49). 켰는데 비밀번호가 없으면 migrate·bootstrap 이 실패한다 |
| `CHAT_MIGRATE_LOCK_TIMEOUT` | `3s` | `server migrate` 의 트랜잭션 파일 DDL 이 잠금을 기다리는 한도. 그동안 그 표의 질의가 DDL 뒤에 줄을 선다. 넘으면 물러났다가 다시 한다. 0 이하는 거부한다. `-- migrate: no-transaction` 파일(CONCURRENTLY)에는 걸지 않는다 — 그 대기는 아무것도 줄 세우지 않는다(D49) |
| `CHAT_MIGRATE_STATEMENT_TIMEOUT` | `60s` | 트랜잭션 파일의 문장 하나의 한도. `-- migrate: no-transaction` 파일(CONCURRENTLY)에는 걸지 않는다 |
| `CHAT_MIGRATE_RETRIES` | `5` | 잠금을 못 잡았을 때 다시 하는 횟수(1초부터 두 배, 최대 10초). `0` 이면 다시 하지 않는다. 음수·정수가 아닌 값은 거부한다 |
| `CHAT_MIGRATE_ADVISORY_WAIT` | `60s` | 다른 migrate 가 자문 잠금을 쥐고 있을 때 기다리는 한도 |
| `CHAT_OLLAMA_URL` | (없음) | 예 `http://localhost:11434`. 비우면 AI 초안이 꺼진다 |
| `CHAT_OLLAMA_MODEL` | `qwen2.5:7b` | 초안에 쓸 모델 |
| `CHAT_MAX_PENDING_UPLOADS` | `10` | 대화당 보내지 않은 첨부 상한 |
| `CHAT_SEED_AGENT_COUNT` | `3` | 기본 상담원 수 (`agent@`, `agent2@`, `agent3@`) |
| `CHAT_STORAGE` | `local` | 첨부 저장소. `local`(서버 디스크) 또는 `s3`(S3 호환). 브라우저가 저장소로 직접 올리고 받는다 |
| `CHAT_UPLOAD_DIR` | `./data/uploads` | `local` 일 때의 저장 경로 |
| `CHAT_S3_ENDPOINT` | (없음) | 서버가 저장소를 부르는 주소. compose 의 MinIO 는 `http://127.0.0.1:59100` |
| `CHAT_S3_PUBLIC_ENDPOINT` | (없음) | 브라우저가 쓰는 주소가 다를 때만 |
| `CHAT_S3_BUCKET` · `CHAT_S3_REGION` | (없음) · `us-east-1` | 버킷과 리전 |
| `CHAT_S3_ACCESS_KEY` · `CHAT_S3_SECRET_KEY` | (없음) | 정적 키. 로컬 MinIO 용. 둘 다 비우면 ECS 태스크 역할의 임시 자격 증명을 쓴다(아래 줄). 하나만 주면 뜨지 않는다 |
| `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` · `AWS_CONTAINER_CREDENTIALS_FULL_URI` · `AWS_CONTAINER_AUTHORIZATION_TOKEN(_FILE)` | (ECS 가 넣는다) | 정적 키가 없을 때 자격 증명을 받는 곳. ECS 는 태스크 역할이 있으면 `RELATIVE_URI` 를 알아서 넣는다. 만료 15분 전에 다시 받고(올리기 양식 10분보다 길다), 기동 때 못 받으면 뜨지 않는다 |
| `CHAT_S3_PATH_STYLE` | `true` | `false` 면 `bucket.host` 방식 (AWS S3) |
| `CHAT_S3_UPLOAD` | `post` | 브라우저가 올리는 방식. `post` 는 양식 업로드(POST policy. AWS S3·MinIO), `put` 은 서명된 PUT 주소(양식 업로드가 없는 S3 호환 저장소는 이것만 된다). 다른 값이면 시작을 거부한다. 어느 쪽이든 완료가 크기를 다시 확인한다(design.md D42) |
| `CHAT_S3_CREATE_BUCKET` | `false` | 뜰 때 버킷을 만든다. 로컬 개발용. 운영 버킷에는 아래 `tmp/` 수명 주기 규칙을 미리 건다 |
| `CHAT_MAX_UPLOAD_MB` | `100` | 첨부 한 건의 최대 크기. 휴대폰 영상 1~2분이 들어간다(D37). 파일은 서버를 거치지 않고 저장소로 바로 올라간다 |
| `CHAT_SEED_AGENT_EMAIL` | `agent@example.com` | 없으면 만들고, 있으면 비밀번호를 덮어쓰지 않는다 |
| `CHAT_SEED_AGENT_NAME` | `상담원` | `CHAT_SEED_DEMO` 가 켜져 있을 때 시드 관리자(`CHAT_SEED_AGENT_EMAIL`)를 처음 만들며 쓰는 표시 이름. 이미 있으면 바꾸지 않는다. 화면에서 바꾸는 칸은 없다 |
| `CHAT_SEED_AGENT_PASSWORD` | (없음) | 시드 계정의 비밀번호. 기본값이 없다. 주면 8글자 이상·72바이트 이하여야 한다(아니면 시작을 거부한다). `make run` 은 로컬에서 `chat1234` 를 준다 |
| `CHAT_LOG_FORMAT` | `text` | `json` 이면 한 줄 JSON. 운영은 json. 다른 값이면 시작을 거부한다 |
| `CHAT_LOG_LEVEL` | `info` | `debug`·`info`·`warn`·`error`. `debug` 면 프레임 단위 전달까지 남긴다 |
| `CHAT_SHUTDOWN_DRAIN` | `0` | 종료 신호 뒤 `/readyz` 를 503 으로 바꾸고 WebSocket 을 닫기 전까지 기다리는 시간. ECS 는 SIGTERM 전에 ALB 에서 타깃을 빼므로, 이 값은 그 변경이 ALB 노드마다 반영되는 지연을 버는 시간이다(운영은 `5s`, docs/deploy.md 2절). 그동안에도 붙어 있는 WebSocket 에 실시간 전달은 계속된다. 음수는 거부한다. 컨테이너의 정지 유예(ECS `stopTimeout`)는 이 값 + 25초보다 길게 둔다(WebSocket 10초 + HTTP 10초 + 관측 내보내기 5초) |

관측(트레이스·지표, design.md D33)은 OpenTelemetry 표준 환경 변수를 그대로 읽는다. 앱은 OTLP/HTTP 로만 내보낸다.

| 환경변수 | 기본값 | 설명 |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (없음) | 컬렉터 주소. 예 `http://localhost:4318`. **비우면 아무것도 내보내지 않는다**(로컬 개발·테스트 기본). 신호별로 `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`·`OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` 도 된다 |
| `OTEL_SERVICE_NAME` | `go-chat-system` | 트레이스·지표의 `service.name` |
| `OTEL_RESOURCE_ATTRIBUTES` | (없음) | 예 `deployment.environment.name=prod` |
| `OTEL_TRACES_SAMPLER` · `OTEL_TRACES_SAMPLER_ARG` | `parentbased_always_on` | 예 `parentbased_traceidratio` · `0.1`. 샘플링에서 빠진 요청도 로그의 `trace_id` 는 남는다 |
| `OTEL_METRIC_EXPORT_INTERVAL` | `60000` | 지표를 모아 보내는 주기(ms) |
| `OTEL_SDK_DISABLED` | `false` | `true` 면 엔드포인트가 있어도 끈다 |

로컬에서 보려면 compose 의 컬렉터를 profile 로 띄운다. 기본 `docker compose up` 에는 뜨지 않는다.

```bash
docker compose --profile otel up -d otel-collector          # 4317·4318 → 호스트 55317·55318 (OTEL_COLLECTOR_GRPC_PORT·_HTTP_PORT 로 바꾼다)
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:55318 make run
docker logs -f go-chat-otel                                  # 받은 스팬·지표 수. 하나하나 보려면 OTEL_DEBUG_VERBOSITY=detailed 로 띄운다
```

첨부는 임시 키 `tmp/<첨부 id>` 로 올라오고, 완료 확인 때 확정 키 `ws/<워크스페이스>/<대화>/<첨부 id>/<임대 token>` 으로 옮겨진다.
token 은 완료 요청마다 다르다. 완료가 실패하고 자기 확정 파일까지 지우지 못하면 행이 모르는 파일이 `ws/` 아래 남는다(design.md D28 남는 한계).
확인한 뒤 같은 양식으로 다시 올린 파일은 행 없이 `tmp/` 에 남는다. `local` 저장소는 서버가 1시간 넘은 `tmp/` 파일을 지운다.
`s3` 는 서버가 지우지 않는다. 버킷에 수명 주기 규칙을 건다: 접두사 `tmp/`, 만든 지 1일 뒤 만료. MinIO 는
`mc ilm rule add --prefix tmp/ --expire-days 1 <별칭>/<버킷>`, AWS S3 는 같은 내용의 Lifecycle 규칙이다.

## API

라우트 하나가 한 줄이다. 인증 칸의 `토큰` 은 고객·상담원 누구의 세션 토큰이든 되고 대화 단위 권한은 그 뒤에 따로 본다.
권한 이름(`conversation.work` 등)은 상담원이 그 권한을 가져야 한다는 뜻이다(권한표는 [docs/design.md](docs/design.md) D29).
코드의 출처는 [internal/adapter/httpapi/httpapi.go](internal/adapter/httpapi/httpapi.go) 의 `Handler` 다.

| 메서드 | 경로 | 인증 | 용도 |
|---|---|---|---|
| GET | `/healthz` | 없음 | 프로세스가 살아 있는지. DB 를 묻지 않는다 — 배포·컨테이너 헬스체크가 본다(D49) |
| GET | `/readyz` | 없음 | DB 에 붙고 스키마가 이 코드를 따라왔는지. 못 붙으면 503 `db_down`, 마이그레이션이 모자라면 503 `schema_behind`, 종료 신호를 받았으면 503 `draining` |
| POST | `/api/customer/session` | 없음 | 익명 고객 세션(기본 워크스페이스). 익명 고객의 토큰을 같이 보내면 같은 고객으로 이어 붙인다. 회원 세션 토큰은 이어 주지 않는다(D46) |
| POST | `/api/customer/member-session` | 없음 | 서명된 회원 토큰으로 세션. 수명은 회원 토큰 `exp` 를 넘지 않고, 그 회원의 세션을 들고 오면 이어 쓴다 |
| POST | `/api/agent/login` | 없음 | 상담원 토큰 발급 |
| POST | `/api/agent/invites/preview` | 없음 | 초대 링크의 이메일·이름·역할·만료 미리 보기. 토큰은 본문으로(D46) |
| POST | `/api/agent/invites/accept` | 없음(초대 토큰) | 초대 수락. 같은 워크스페이스·이메일의 승인 대기 행을 대체한다 |
| POST | `/api/agent/signup` | 없음 | 상담원 직접 가입(기본 워크스페이스). 결과와 상관없이 202, 승인 대기로 남는다 |
| GET | `/api/widget/settings` | 없음 | 위젯 색·제목·운영 여부·자주 묻는 질문(`faq`) |
| GET | `/api/me` | 토큰 | 내 주체 정보와 권한 목록 |
| GET | `/api/workspace` | 토큰 | 소속 워크스페이스 요약(키·비밀 없음) |
| POST | `/api/logout` | 토큰 | 세션 폐기. 그 세션으로 열린 WebSocket 도 곧바로 닫힌다 |
| POST | `/api/agent/password` | 상담원 | 비밀번호 변경 (다른 세션 끊김) |
| POST | `/api/agent/name` | 상담원 | 내 이름 변경(권한 없이 본인만) |
| GET | `/api/admin/team` | `team.manage` | 팀·초대·승인 대기 목록 |
| POST | `/api/admin/invites` | `team.manage` | 초대 |
| DELETE | `/api/admin/invites/{email}` | `team.manage` | 초대 취소 |
| POST | `/api/admin/agents/{id}/role` | `team.manage` | 역할 변경 |
| POST | `/api/admin/agents/{id}/active` | `team.manage` | 비활성화·재활성화 |
| POST | `/api/admin/agents/{id}/approve` | `team.manage` | 가입 승인(역할 지정) |
| POST | `/api/admin/agents/{id}/reject` | `team.manage` | 가입 거절 |
| POST | `/api/admin/agents/{id}/name` | `team.manage` | 다른 상담원 이름 변경 |
| GET | `/api/admin/settings` | `workspace.manage` | 운영시간·자동 메시지·위젯 설정 |
| PUT | `/api/admin/settings` | `workspace.manage` | 같은 설정 저장 |
| GET | `/api/admin/widget-faq` | `workspace.manage` | 위젯 자주 묻는 질문. 저장한 적이 없으면 기본 세 항목과 `is_default: true` |
| PUT | `/api/admin/widget-faq` | `workspace.manage` | 위젯 자주 묻는 질문 통째로 저장(10개, 질문 80자, 답 500자까지). `items` 는 필수이고 FAQ 를 두지 않으려면 `[]` |
| GET | `/api/admin/workspace` | `workspace.manage` | 이름·허용 출처·회원 서명 비밀·공개 주소 |
| PUT | `/api/admin/workspace` | `workspace.manage` | 이름·허용 출처 수정 |
| POST | `/api/admin/workspace/rotate-member-secret` | `workspace.manage` | 회원 서명 비밀 새로 발급. 옛 비밀로 서명한 회원 토큰은 거절되고 회원 고객의 세션이 모두 끝난다 |
| GET | `/api/admin/audit` | `workspace.manage` | 감사 로그 |
| GET | `/api/admin/export?format=json\|csv` | `workspace.manage` | 대화 내보내기(내부 메모·삭제한 상담 포함) |
| DELETE | `/api/customers/{id}` | `customer.delete` | 고객 삭제(익명화). 본문·첨부를 지운다 |
| GET | `/api/customer/conversations` | 고객 | 내 대화 목록 |
| POST | `/api/customer/conversations` | 고객 | 새 문의 시작 |
| POST | `/api/customer/conversations/{id}/close` | 고객(자기 상담) | 상담 직접 종료(D48). 이미 닫혔으면 같은 결과로 200 |
| GET | `/api/conversations/{id}` | 토큰 | 대화 메타 |
| GET | `/api/conversations/{id}/messages?after_seq=&limit=` | 토큰 | 캐치업·백필 |
| POST | `/api/conversations/{id}/messages` | 토큰 | WebSocket 대체 전송. 멱등 |
| PATCH | `/api/conversations/{id}/messages/{msgId}` | 토큰 | 자기 메시지 수정 |
| DELETE | `/api/conversations/{id}/messages/{msgId}` | 토큰 | 자기 메시지 삭제. 남의 상담원·AI·봇 메시지는 `message.moderate`. 시스템 줄은 누구도 못 지운다(409 `system_line`) |
| GET | `/api/agent/conversations/{id}/messages/{msgId}/revisions` | `message.revisions` | 수정·삭제 전 원문 이력 |
| GET | `/api/agent/conversations/{id}/prior` | `conversation.work` | 같은 고객의 이전 상담 이어 보기 |
| GET | `/api/conversations/{id}/reads` | 토큰 | 참여자별 읽음 지점 |
| POST | `/api/conversations/{id}/read` | 토큰 | 읽음 보고. 단조 증가 |
| GET | `/api/conversations/{id}/rating` | 토큰 | 만족도 조사 상태(종료 여부·고칠 수 있는 기한·낸 평가). 그 상담의 고객과 상담원(D45) |
| PUT | `/api/conversations/{id}/rating` | 고객 | 별점 1~5·한 줄 의견 200자. 종료 뒤 24시간 안, 상담당 한 번(마지막 값). 그 상담의 고객만 |
| POST | `/api/conversations/{id}/attachments` | 토큰 | 업로드 신청(이름·종류·크기) → 저장소로 올릴 양식. 크기 한도를 넘으면 413 `too_large` |
| POST | `/api/attachments/{id}/complete` | 토큰(올린 본인) | 올라온 파일 확인. 지나야 메시지에 붙는다. 다른 요청이 확인 중이면 409 `busy`, 저장소가 20초 안에 끝내지 못하면 503 `storage_timeout` |
| GET | `/api/attachments/{id}` | 토큰 | 내려받기 주소로 302 |
| GET | `/api/attachments/{id}/url` | 토큰 | 5분짜리 내려받기 주소 |
| POST | `/files/upload` | 서명된 양식 | `local` 저장소일 때만. 브라우저가 파일을 직접 올린다 |
| GET | `/files/get?token=` | 서명된 허가 | `local` 저장소일 때만. 발급받은 주소로 내려받는다 |
| GET | `/api/customers/{id}` | 토큰 | 고객 프로필. 고객은 자기 것만 |
| POST | `/api/customers/{id}` | `customer.edit`, 고객은 본인 | 고객 프로필·메모 수정. 고객은 자기 프로필만, 메모·속성은 못 바꾼다 |
| GET | `/api/agent/conversations?state=&assignee=&following=&tag_id=&q=&waiting=&deleted=` | `conversation.work` | 받은함. `waiting=1` 은 답할 차례(배지 `todo` 와 같은 조건), `deleted=1` 은 `conversation.delete`, 본문 검색어는 `search.all` 이 없으면 고객 이름만 찾는다 |
| GET | `/api/agent/inbox/counts` | `conversation.work` | 받은함 칸별 건수 |
| POST | `/api/agent/conversations/{id}/state` | `conversation.work` | `OPEN` / `SNOOZED` / `CLOSED` |
| DELETE | `/api/agent/conversations/{id}` | `conversation.delete` | 상담 삭제(가리기). 본문 `{"reason": "1~200자"}` |
| POST | `/api/agent/conversations/{id}/restore` | `conversation.delete` | 삭제한 상담 되살리기 |
| GET | `/api/agent/conversations/{id}/deleted` | `conversation.delete` | 삭제한 상담 읽기 전용 보기(사유·사람·시각) |
| POST | `/api/agent/conversations/{id}/assign` | `conversation.work` | 미배정 상담을 자기에게, 자기 담당 해제. 남에게 배정·이관과 남의 담당 해제는 `conversation.assign`. 본문 `{"agent_id", "expected_assignee"}` — 화면이 보던 담당자(빈 문자열 = 미배정)가 그 사이 바뀌었으면 409 `conflict` |
| POST | `/api/agent/conversations/{id}/follow` | `conversation.work` | 팔로우·언팔로우 |
| GET | `/api/agent/conversations/{id}/followers` | `conversation.work` | 팔로워 목록 |
| POST | `/api/agent/conversations/{id}/tags` | `conversation.work` | 있는 태그 붙이기. 새 태그 정의는 `library.manage` |
| DELETE | `/api/agent/conversations/{id}/tags/{tagId}` | `conversation.work` | 태그 떼기 |
| GET | `/api/agent/search?q=` | `search.all` | 워크스페이스 전체 메시지 검색 |
| GET | `/api/agent/snippets` | `conversation.work` | 빠른 답변 목록 |
| POST | `/api/agent/snippets` | `library.manage` | 빠른 답변 만들기 |
| PUT | `/api/agent/snippets/{id}` | `library.manage` | 빠른 답변 고치기. 단축어가 겹치면 409 |
| DELETE | `/api/agent/snippets/{id}` | `library.manage` | 빠른 답변 지우기 |
| GET | `/api/agent/agents` | `conversation.work` | 같은 워크스페이스 상담원 목록 |
| GET | `/api/agent/tags` | `conversation.work` | 태그 정의 목록 |
| POST | `/api/agent/conversations/{id}/draft` | `conversation.work` | AI 초안 (저장·전송 없음) |
| GET | `/api/agent/work?agent=&conversation=&from=&to=` | `conversation.work` | 업무 기록. 남의 기록은 `work.view_all`, 삭제한 상담의 기록은 `conversation.delete` |
| GET | `/api/agent/work/summary?from=&to=` | `conversation.work` | 상담원별 집계. 남의 줄은 `work.view_all` |
| GET | `/api/agent/metrics?from=&to=` | `work.view_all` | 상담 지표: AI 처리율, 첫 응답·평균 응답 시간(평균·p50·p90), 만족도(평균·CSAT·응답률), 종료 수와 처리 주체(고객 자체 해결 포함)·고객이 직접 종료한 수, 답한 주체별(D45·D48) |
| POST | `/api/ws-ticket` | 토큰 | WebSocket 티켓(30초, 일회용). 세션 토큰 대신 `/ws` 주소에 싣는다(D46) |
| GET | `/ws?ticket=` | 티켓 | WebSocket. 옛 `?token=` 은 제거 예정이고 받을 때마다 `ws.token_in_query` 를 남긴다 |
| GET | `/` | 없음 | 정적 화면(`/widget`, `/desk` 등)과 자산. `Cache-Control: no-cache` + `ETag`(바뀌지 않았으면 304) |

WebSocket 프레임은 JSON 이다. 브라우저가 보내는 건 `subscribe` · `send` · `read` · `typing` · `ping`,
서버가 보내는 건 `ready` · `message` · `ack` · `conversation` · `read` · `typing` · `error` ·
`inbox_changed` · `pong`. `send` 는 `internal`(내부 메모)과 `attachment_id` 를 받는다.
형식은 [internal/adapter/realtime/protocol.go](internal/adapter/realtime/protocol.go) 에 있다.

## 더 읽을 것

- [docs/design.md](docs/design.md) — 불변식 I1~I11, 결정 D1~D49, 실패 모드 표, 정책 886개(번호는 1~905, 지운 번호 19개는 표시만 남는다)
- [docs/api.md](docs/api.md) — HTTP·WebSocket 계약 전체(요청·응답·오류 코드·닫기 코드). 라우트 표는 위가 요약이다
- [docs/features.md](docs/features.md) — 되는 것과 안 되는 것, 근거 테스트
- [docs/onboarding.md](docs/onboarding.md) — 처음 설치해서 상담 하나를 받기까지
- [docs/deploy.md](docs/deploy.md) — AWS 배포 절차(CDN → ALB → ECS → RDS, D34)와 운영 런북
- [docs/adr/](docs/adr/README.md) — 기술 결정 기록: Go 서버, 화면 구조, 업로드 방식, 디자인 시스템
- [docs/perf/deploy-db.md](docs/perf/deploy-db.md) — 배포와 DB 를 뗀 근거와 전후 실측(D49)
- [docs/guide/INDEX.md](docs/guide/INDEX.md) — 백지에서 다시 쓰는 14단계 개발 가이드
- [docs/loadtest.md](docs/loadtest.md) · [docs/product-gaps.md](docs/product-gaps.md) — 측정치와 남은 일
- [docs/proposals/](docs/proposals/) — 아직 도입하지 않은 다음 결정(저장소·장기 보관·검색·런타임)
- 위젯 3D 아이콘 — Microsoft Fluent Emoji(MIT, https://github.com/microsoft/fluentui-emoji). 고지는 [web/assets/icons/LICENSE](web/assets/icons/LICENSE), 결정은 D38
- Figma 화면 캡처 — https://www.figma.com/design/4Vb3pwKsxKVEptsWzvTusp (위젯 홈·대화, 데스크, 설정 + app.css 토큰 변수)
- [internal/adapter/pgstore/messages.go](internal/adapter/pgstore/messages.go) — seq 발급 트랜잭션
- [internal/adapter/realtime/hub.go](internal/adapter/realtime/hub.go) — 신호만 옮기는 허브
- [web/assets/chat.js](web/assets/chat.js) — 브라우저 쪽 pending 큐와 갭 복구
