# 실행 파일 하나짜리 서버. 정적 자산은 바이너리에 들어 있다.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata curl \
 && addgroup -S chat && adduser -S -G chat chat
# RDS 전역 CA 번들. 운영 DB 주소는 sslmode=verify-full&sslrootcert=/etc/ssl/certs/rds-global-bundle.pem 로 서버 인증서를 확인한다.
# 빌드 때 받으므로 이미지마다 그 시점의 번들이 들어간다. 받지 못했거나 인증서가 없으면 빌드가 실패한다. 로컬 compose 는 쓰지 않는다(sslmode=disable).
RUN curl -fsSL -o /etc/ssl/certs/rds-global-bundle.pem https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem \
 && chmod 644 /etc/ssl/certs/rds-global-bundle.pem \
 && test "$(grep -c 'BEGIN CERTIFICATE' /etc/ssl/certs/rds-global-bundle.pem)" -gt 0
WORKDIR /app
COPY --from=build /out/server /app/server
# 첨부는 여기 쌓인다. 볼륨으로 빼야 재배포에도 남는다.
RUN mkdir -p /app/data/uploads && chown -R chat:chat /app
USER chat
ENV CHAT_ADDR=:8080 CHAT_UPLOAD_DIR=/app/data/uploads
EXPOSE 8080
# 헬스체크는 프로세스 생존(/healthz)만 본다. /readyz 는 종료 중·DB 장애 조치 중에 503 이라 멀쩡한 태스크를 죽인다(D32·D34).
HEALTHCHECK --interval=10s --timeout=3s --retries=3 CMD curl -fsS http://localhost:8080/healthz || exit 1
# 인자 없이는 서버다. 같은 이미지로 `/app/server migrate`(배포의 앞 단계: ECS 1회성 태스크)와 `/app/server bootstrap` 을 돌린다(D49).
# ENTRYPOINT 가 아니라 CMD 다. ENTRYPOINT 가 있으면 명령을 바꾸는 쪽의 명령이 그 뒤에 붙어 `/app/server /app/server migrate` 가 된다.
# 명령을 바꾸는 쪽(ECS command, compose command)은 실행 파일 경로부터 쓴다.
CMD ["/app/server"]
