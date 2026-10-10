// Package loginput 은 로그 입력 검사(loginput_test.go)의 표본이다. 빌드에 들어가지 않는다.
// "잡는다:" 표시가 붙은 줄은 클라이언트가 정한 글자를 로그로 보낸다 — 검사가 그 줄을 잡아야 한다. 표시 없는 줄은 잡으면 안 된다.
// 감사 23 이 본 꼴(프레임의 type, client_msg_id, 메서드, 경로 값)과 그 변형이다.
package loginput

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/logctx"
)

type frame struct {
	Type        string `json:"type"`
	ClientMsgID string `json:"client_msg_id"`
	Seq         int64  `json:"seq"`
}

// params 는 태그 없이 입력을 나르는 유스케이스 매개변수다(검사의 types 에 넣는다).
type params struct {
	ClientMsgID string
	Size        int64
}

// known 은 로그에 싣는 값이 입력이 아닌 구조체다(필드가 없다). 잡지 않는다.
type known struct{}

func knownKind(t string) string { return t }

func probe(ctx context.Context, r *http.Request, f frame, p params, l *slog.Logger) {
	slog.Info("a1", "method", r.Method)               // 잡는다: 요청의 메서드
	slog.Info("a2", "path", r.URL.Path)               // 잡는다: 요청 주소의 경로
	slog.Info("a3", "q", r.URL.Query().Get("q"))      // 잡는다: 쿼리 값
	slog.Info("a4", "ua", r.Header.Get("User-Agent")) // 잡는다: 헤더 값
	slog.Info("a5", "id", r.PathValue("id"))          // 잡는다: 경로 값
	slog.Info("a6", "frame", f.Type)                  // 잡는다: 프레임의 type
	l.Warn("a7", "client_msg_id", f.ClientMsgID)      // 잡는다: 프레임의 client_msg_id
	slog.Info("a8", "cmid", p.ClientMsgID)            // 잡는다: 유스케이스 매개변수의 입력
	kind := f.Type
	slog.Info("a9", "frame", kind) // 잡는다: 변수에 담은 입력
	line := "frame=" + strings.ToLower(f.Type)
	slog.Info("a10", "m", line)                              // 잡는다: 이어 붙이고 글자 함수를 지난 입력
	slog.Info("a11", "m", fmt.Sprintf("%s", r.Host))         // 잡는다: Sprintf 로 옮긴 Host
	ctx = logctx.With(ctx, "route", r.URL.String())          // 잡는다: ctx 키로 얹은 주소
	slog.Info("a12", slog.String("ref", r.FormValue("ref"))) // 잡는다: slog.String 안의 폼 값

	// 잡지 않는다: 거르개를 지난 값, 수, 라우트 패턴, 정규화한 id
	slog.InfoContext(ctx, "ok", "pattern", r.Pattern, "seq", f.Seq, "size", p.Size)
	if v, err := id.Normalize(r.PathValue("id")); err == nil {
		slog.Info("ok", "conversation_id", v)
	}
	slog.Info("ok", "frame", knownSanitized(f.Type), "len", len(f.ClientMsgID))
	slog.Info("ok", "known", known{})
}

// knownSanitized 는 거르개(frameKind 처럼 아는 값만 돌려주는 함수)를 흉내 낸다. 테스트가 이 이름을 sanitizers 에 넣는다.
func knownSanitized(t string) string { return knownKind(t) }
