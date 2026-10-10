.PHONY: up down run migrate build lint test verify fmt logs psql reset clean-test-dbs

up:            ## Postgres 를 띄우고 준비될 때까지 기다린다
	docker compose up -d --wait

down:
	docker compose down

reset:         ## DB 를 볼륨까지 지우고 다시 만든다
	docker compose down -v && docker compose up -d --wait

# 서버는 스스로 마이그레이션하지 않는다(D49). 개발 서버는 migrate(데모 계정 시드 포함) 뒤에 띄운다.
# 데모 계정: agent@example.com / chat1234. 비밀번호 기본값은 코드에 없고 여기서만 준다(로컬 전용).
run: migrate   ## 개발 서버: migrate → 서버
	./bin/server

migrate: up build  ## 마이그레이션 + 보정 + 데모 시드(로컬). 운영의 배포 앞 단계와 같은 명령이다
	CHAT_SEED_DEMO=$${CHAT_SEED_DEMO:-true} CHAT_SEED_AGENT_PASSWORD=$${CHAT_SEED_AGENT_PASSWORD:-chat1234} ./bin/server migrate

build:
	go build -o bin/server ./cmd/server

fmt:
	gofmt -w ./cmd ./internal ./web
	go vet ./...

lint:          ## golangci-lint (.golangci.yml, 버전은 scripts/verify.sh 에 고정)
	golangci-lint run ./...

test: up       ## 경쟁 검사(-race)를 켜고 돈다. -p 1 은 설정이 DB 전역이라서다
	go test ./... -race -count=1 -p 1

verify:        ## 커밋 전에 돈다: 포맷·tidy·vet·린트·빌드·프론트·-race 테스트(건너뛰면 실패)·가이드
	./scripts/verify.sh

logs:
	docker compose logs -f db

psql:
	docker compose exec db psql -U chat -d chat

clean-test-dbs: ## 테스트가 남긴 격리 DB(chat_t_*)를 지운다
	docker compose exec -T db psql -U chat -d chat -Atc "SELECT datname FROM pg_database WHERE datname LIKE 'chat_t_%'" | xargs -I{} docker compose exec -T db psql -U chat -d chat -c 'DROP DATABASE "{}" WITH (FORCE)'
