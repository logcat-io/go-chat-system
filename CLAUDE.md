# go-chat-system

상담 채팅 플랫폼 + AI 상담사(둠둠디라 데모)를 담은 logcat 의 개인 포트폴리오. Go 서버 + Postgres, 첨부는 S3 호환 저장소. 1순위 품질 속성은 **메시지를 잃지 않는 것**이다.
둠둠디라는 1:1 베이스 레슨 스튜디오이고 AI 상담사의 데모 무대다.

## 먼저 읽는다

`docs/` 는 로컬 전용이다(`.gitignore`, 2026-10-07). 리포 클론에는 없고 이 기계의 작업 트리에만 있다. 2026-10-07 이전 커밋 이력에는 남아 있다.

0. `../go-chat-system-work/HANDOFF.md` — 직전 세션의 인계(미커밋 변경의 내용, logcat 이 정할 것, 다음 순서). 리포 밖이다. 없으면 건너뛴다.
1. `docs/design.md` — 불변식 I1~I11, 결정 D1~D47, 정책 목록(번호 = 테스트 이름). 결정을 뒤집으려면 여기를 먼저 고친다.
1-1. `docs/defects.md` — 테스트·감사에서 만난 결함 기록(문제·원인·해결·지키는 테스트)과 열린 결함의 수정 계획. 같은 종류가 나오면 여기부터 찾는다.
2. `docs/refactoring.md` — 구조를 고칠 때의 기준(1절 철학), 하지 않는 것(2절), 알려진 결함 목록(5절).
3. `docs/proposals/` — 아직 구현하지 않은 방향: 바깥 채널 발송(outbox), 과거 대화 검색(pgvector), 보관, Jev. 로그 저장소와 관측은 결정·구현됐다(D33).
4. `README.md` — 환경 변수와 API 표(라우트 전부).
5. `docs/guide/INDEX.md` — 단계별 구현 가이드. 코드 블록은 `scripts/guide-embed.py` 가 만든다. 손으로 고치지 않는다.
6. `docs/api.md` — HTTP·WebSocket 계약 전체(요청·응답·오류 코드·닫기 코드). 응답 모양을 바꾸면 여기를 고친다.
7. `docs/features.md` — 되는 것과 안 되는 것, 근거 테스트. 기능을 넣거나 빼면 표를 고친다.
8. `docs/onboarding.md` — 처음 설치해서 상담 하나를 받기까지의 화면 순서.
9. `docs/deploy.md` — AWS 배포 절차와 운영 런북(D34). 배포에 닿는 코드(종료 순서, 헬스체크, 환경 변수, 지표 이름)를 바꾸면 같이 본다.
10. `docs/adr/` — 되돌리기 어려운 기술 결정의 맥락·선택지·다시 볼 신호(언어, 화면 구조, 업로드, 디자인 시스템). 결정 자체는 design.md D 목록이 단일 출처다.

## 구조

의존은 한 방향이다: `internal/core` ← `internal/app` ← `internal/adapter/*`. `internal/archtest` 가 빌드에서 막는다.

- `internal/core` — 타입과 오류 값. 표준 라이브러리만 쓴다.
- `internal/app` — 유스케이스. `Commands`(상태를 바꾼다)와 `Queries`(읽는다). 포트는 `ports.go`. 권한은 `permission.go` 한 곳.
- `internal/adapter/{pgstore,httpapi,realtime,filestore,s3store,ollama}` — 번역. 어댑터끼리는 서로를 모른다.
- `internal/logctx` — 컨텍스트로 추적 키를 나르는 로거. 잎 패키지.
- `internal/telemetry` — OpenTelemetry 트레이스·지표의 시작·종료와 공용 계기(D33). OTel 과 logctx 만 아는 잎이고, core·app 은 이걸 모른다.
- `cmd/server` — 조립은 여기 한 곳(`assemble`).
- `web/assets` — 위젯과 데스크(바닐라 JS). `chat.js` 가 유실 방지 클라이언트다.

규칙 몇 개:
- 동시에 와도 깨지면 안 되는 불변식(번호 발급, 멱등 키, 임대, 마지막 관리자)은 DB 가 지킨다. 누가 무엇을 할 수 있는지와 순서는 app 이 정한다.
- 새 기능은 자기 포트를 새 파일에 정의한다. `Store` 에 메서드를 더하지 않는다.
- DB 커넥션을 쥔 채 다른 자원(저장소, 다른 커넥션)을 기다리지 않는다. 세션 자문 잠금으로 서버 전체가 멈춘 적이 있다(D28).
- 로그에는 id 만 적는다. 본문, 이름, 이메일, 토큰, 파일 이름, 서명된 주소, 저장소 키를 적지 않는다(D30).
- 마이그레이션은 추가만 한다. 적용된 파일을 고치지 않는다.

## 테스트는 정책이다

테스트 이름이 정책 문장이고 관찰 가능한 결과를 단언한다. 내부 호출을 검증하지 않는다. 정책에 묶인 숫자는 테스트가 리터럴로 고정한다.
구조만 바꾸는 작업에서 테스트의 단언을 바꿔야 통과한다면 그건 리팩토링이 아니라 정책 변경이다.
새 정책은 `docs/design.md` 끝의 목록에 테스트 이름과 같은 문장으로 잇는다.

## 검증

커밋 전에는 언제나 `./scripts/verify.sh` 를 끝까지 통과시킨다. 린트(golangci-lint)와 `go test -race` 가 들어 있고 둘 다 뺄 수 없다.

```bash
./scripts/verify.sh                       # gofmt → tidy -diff → vet → golangci-lint → build → 프론트(npm) → compose up → go test -race -p 1(SKIP 이면 실패) → 가이드 대조
make lint                                 # golangci-lint 만. 설정은 .golangci.yml(켠 이유·끈 이유가 주석에), 버전은 verify.sh 에 고정(2.14.0)
python3 scripts/guide-embed.py            # 가이드 다시 만들기. 새 파일은 OWNERS 에 단계를 적어야 통과한다
python3 scripts/guide-embed.py --check    # 쓰지 않고 차이만 본다(verify.sh 가 부른다)
go test -race -p 1 -count=1 ./internal/adapter/httpapi/ -run '<이름>'   # 부분 실행. -p 1 은 필수(설정이 DB 전역)
```

- 린트 지적을 무시할 때는 그 줄에 `//nolint:<린터> // <이유>` 를 단다. 이유 없는 nolint 는 두지 않는다.

- Postgres 는 `docker compose up -d --wait`(포트 56432), MinIO 는 같은 compose(59100, `chat` / `chatchat123`).
- 테스트의 기본 DB 는 같은 Postgres 의 `chat_test` 다(없으면 만든다). 개발 서버의 `chat` 을 쓰면 테스트 상담 수천 건이 데스크를 덮고 열린 데스크 탭이 테스트에 읽음을 남긴다(2026-10-11).
- 서버: `make run`(빈 DB 면 `migrate` 와 데모 시드를 먼저 한다 — 서버 기동은 DB 에 쓰지 않는다, D49) → 데스크 `http://localhost:8090/desk`(`agent@example.com` / `chat1234`), 위젯 `/widget`.
- S3 모드로 띄우기: `.env.example` 의 `CHAT_STORAGE=s3` 묶음.
- 브라우저 검증과 전체 검증 한 번에: `../go-chat-system-work/run-all.sh`(리포 밖. 개발 DB 볼륨을 지운다). 화면을 고쳤으면 여기의 `uitest/demo.mjs`·`roles.mjs` 를 돌린다.
- 전송 경로(`pgstore/messages.go`, `realtime/conn.go`)를 건드렸으면 `go run ./cmd/loadtest` 로 다시 잰다. 기준은 `docs/loadtest.md`.

## 워커로 작업할 때

전역 규칙(`lc-teams`): 메인은 감독이고 구현은 워커가 한다. 이 리포에서 겪은 것:

- 워커마다 **리포의 사본**과 **전용 테스트 DB** 를 준다(`CHAT_TEST_DATABASE_URL=postgres://chat:chat@127.0.0.1:56432/<이름>?sslmode=disable`).
  한 작업 트리를 같이 쓰면 반쯤 고친 코드와 마이그레이션이 서로의 빌드와 테스트 DB 를 깬다.
- 사본을 같은 시작 커밋에서 뜨면 끝난 뒤 `git merge` 로 합친다. 파일 소유를 나눠 주면 충돌이 나지 않았다.
- `verify.sh`, 브라우저 데모, `docker compose down` 은 감독이 합친 뒤에 한 번만 돌린다. 워커와 감사는 8080·8090 을 쓰지 않는다.
- 워커의 보고는 증거가 아니다. 독립 감사가 변이(수정을 되돌리면 테스트가 실패하는가)와 실제 서버로 다시 확인한다.

## 커밋

logcat 이 요청할 때만 한다. 기본 브랜치는 `master`, 리모트는 `origin` = `git@github.com:logcat-io/go-chat-system.git`(비공개).
메시지는 영어 Conventional Commits(`feat:`, `fix:`, `chore(deploy):` …) 제목에 본문을 붙인다. 이력은 2026-10-11 에 새로 시작했다(첫 커밋 294b3c6). 그 전 이력은 이 저장소에 없다.
