#!/usr/bin/env bash
# 이 저장소의 검증 한 번에 돌리기. 실패하면 멈춘다. 커밋 전에 언제나 이것을 돌린다(린트와 -race 포함).
#
# 순서: gofmt → go mod tidy -diff → go vet → golangci-lint → go build → 프론트(npm) → compose up
#       → go test -race(건너뛴 테스트가 있으면 실패) → 가이드 코드 블록 대조
# 싼 검사를 앞에 둔다. DB 가 필요한 단계는 뒤에 모은다.
set -euo pipefail
cd "$(dirname "$0")/.."

# golangci-lint 는 버전마다 지적이 달라진다. 설정(.golangci.yml)을 맞춘 버전으로 고정한다.
# 올릴 때는 이 값과 deploy/github-actions/ci.yml 의 version 을 같이 바꾸고, 새로 나온 지적을 처리한다.
GOLANGCI_LINT_VERSION="2.14.0"

echo "== gofmt =="
unformatted=$(gofmt -l ./cmd ./internal ./web)
if [ -n "$unformatted" ]; then
  echo "포맷이 안 된 파일이 있다:"; echo "$unformatted"; exit 1
fi

echo "== go mod tidy =="
# -diff 는 파일을 고치지 않고 차이만 보이며, 차이가 있으면 0 이 아닌 값으로 끝난다.
if ! go mod tidy -diff; then
  echo "go.mod 또는 go.sum 이 tidy 결과와 다르다. 'go mod tidy' 를 돌리고 커밋해라."; exit 1
fi

echo "== go vet =="
go vet ./...

echo "== golangci-lint =="
if ! command -v golangci-lint >/dev/null; then
  echo "golangci-lint 가 없다. 'brew install golangci-lint' (v$GOLANGCI_LINT_VERSION) 로 깐다."; exit 1
fi
have=$(golangci-lint version --short 2>/dev/null || true)
have=${have#v}
if [ "$have" != "$GOLANGCI_LINT_VERSION" ]; then
  echo "golangci-lint $have 이다. 이 리포는 $GOLANGCI_LINT_VERSION 에 맞춰 두었다(버전마다 지적이 다르다)."; exit 1
fi
golangci-lint run ./...

echo "== build =="
go build ./...

echo "== front =="
# 프론트 검사는 package.json 의 스크립트(css:check·lint·test)가 정한다.
# package.json 이 없던 때의 리포면 클라이언트 유실 방지 테스트만 node 로 바로 돌린다.
if ! command -v node >/dev/null; then
  echo "node 가 없다. 클라이언트 유실 방지 테스트를 돌릴 수 없다."; exit 1
fi
if [ -f package.json ]; then
  if [ ! -d node_modules ]; then
    npm ci
  fi
  npm run css:check
  npm run lint
  npm test
else
  echo "package.json 없음 — npm 단계를 건너뛰고 node --test 만 돈다."
  node --test web/assets/*.test.mjs
fi

echo "== postgres =="
docker compose up -d --wait

echo "== test (race) =="
# -race 는 빼지 않는다. 전송 경로(hub·conn·리스너)는 고루틴끼리 상태를 나눠 쓴다.
# -p 1: 설정(운영시간·자동 메시지)이 DB 전역이라 패키지가 병렬로 돌면 서로 간섭한다.
# -count=1: 캐시된 통과를 믿지 않는다. DB 와 MinIO 상태가 바뀌었을 수 있다.
test_log=$(mktemp -t go-chat-verify.XXXXXX)
if ! go test -race -p 1 -count=1 -v ./... >"$test_log" 2>&1; then
  grep -E -- '^(--- FAIL|FAIL|panic:|WARNING: DATA RACE)' "$test_log" || true
  echo "테스트가 실패했다. 전체 출력: $test_log"; exit 1
fi
grep -E '^(ok|FAIL)[[:space:]]' "$test_log" || true

# 리포의 t.Skip 은 전부 "기반이 없어 못 돈다" 다: Postgres 에 못 붙음, 격리 DB 를 못 만듦, MinIO 에 못 닿음.
# 건너뛰면 유실 방지 테스트가 하나도 안 돈 채 통과로 보인다. 문구를 고르지 않고 `--- SKIP` 줄 자체를 실패로 본다(CI 와 같다).
if grep -E -- '--- SKIP' "$test_log"; then
  echo "건너뛴 테스트가 있다. 위 줄의 사유(DB·격리 DB·MinIO)를 본다. 전체 출력: $test_log"; exit 1
fi
rm -f "$test_log"

echo "== guide =="
# 가이드의 코드 블록이 지금 코드와 같은지만 본다. 파일을 쓰지 않는다.
# docs/ 는 로컬 전용이다(.gitignore). 클론에는 없으니 그때는 건너뛴다.
if [ -d docs/guide ]; then
  python3 scripts/guide-embed.py --check
else
  echo "docs/guide 없음 — 가이드 대조를 건너뛴다."
fi

echo "전부 통과했다."
