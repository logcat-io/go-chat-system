// Package logleak 은 로그 원문 검사(logerr_test.go)의 표본이다. 빌드에 들어가지 않는다.
// "잡는다:" 표시가 붙은 줄은 오류 값이나 원문을 로그로 보낸다 — 검사가 그 줄을 잡아야 한다. 표시 없는 줄은 잡으면 안 된다.
// 감사 16 이 첫 검사를 피해 간 꼴(b1~b9)과 그 변형이다.
package logleak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"

	"github.com/logcat-io/go-chat-system/internal/logctx"
)

type wrapErr struct{ Err error }

type deep struct{ inner []wrapErr }

type record struct{ msg string }

func probe(ctx context.Context, err error, l *slog.Logger) {
	slog.Warn("b0", "err", err)                      // 잡는다: 오류 값 그대로
	slog.Warn("b0", "err", err.Error())              // 잡는다: 원문
	slog.Warn("ok", "err_kind", logctx.ErrKind(err)) // 종류만 — 잡지 않는다
	slog.Warn("ok", "failed", err != nil, "same", errors.Is(err, context.Canceled))

	msg := err.Error()
	slog.Warn("b1", "m", msg) // 잡는다: 변수에 담은 원문
	line := "실패: " + msg
	l.Info("b1", "m", line) // 잡는다: 원문을 이어 붙인 변수
	var later string
	later = fmt.Sprintf("%v", err)
	slog.Info("b1", "m", later) // 잡는다: 나중에 대입한 원문

	slog.Warn("b2", "w", wrapErr{Err: err}) // 잡는다: 구조체로 감싼 오류
	w := wrapErr{Err: err}
	slog.Warn("b2", "w", w)                              // 잡는다: 오류를 품은 구조체 변수
	slog.Warn("b2", "d", deep{})                         // 잡는다: 오류를 깊이 품은 타입
	slog.Warn("b3", "m", strings.TrimSpace(err.Error())) // 잡는다: 다른 함수에 넘긴 원문
	slog.Warn("b3", "m", strings.ToUpper(msg)[:3])       // 잡는다: 원문 변수를 다른 함수에 넘기고 자름
	slog.Warn("ok", "n", len(msg), "has", strings.Contains(msg, "x"))

	args := []any{"err", err}
	slog.Warn("b4", args...) // 잡는다: 펼친 인자
	more := []any{"k", 1}
	more = append(more, "err", err)
	slog.Warn("b4", more...) // 잡는다: append 로 더한 인자
	clean := []any{"k", 1}
	slog.Warn("ok", clean...)

	l.LogAttrs(ctx, slog.LevelWarn, "b5", slog.Attr{Key: "e", Value: slog.AnyValue(err)}) // 잡는다: Attr 리터럴
	l.LogAttrs(ctx, slog.LevelWarn, "b5", slog.String("e", msg))                          // 잡는다: slog.String 에 원문

	fmt.Fprintf(os.Stderr, "%v\n", err) // 잡는다: 표준 오류로 바로 쓴 오류
	fmt.Println("실패", msg)              // 잡는다: 표준 출력으로 바로 쓴 원문
	log.Printf("실패 %v", err)            // 잡는다: 표준 log
	fmt.Fprintf(os.Stderr, "%s\n", "설정 기본값을 쓴다")

	slog.Warn("b6", "m", fmt.Sprint(err))          // 잡는다: fmt.Sprint
	slog.Warn("b7", "m", []error{err})             // 잡는다: 오류 슬라이스
	slog.Warn("b8", "m", map[string]any{"e": err}) // 잡는다: 맵 리터럴
	slog.Warn("b9", "m", &err)                     // 잡는다: 포인터
	slog.Warn("b9", "r", &record{msg: msg})        // 잡는다: 원문을 담은 리터럴의 주소
	b, _ := json.Marshal(w)
	slog.Warn("b10", "m", string(b)) // 잡는다: JSON 으로 옮긴 오류

	var r record
	r.msg = err.Error()
	slog.Warn("b11", "m", r.msg) // 잡는다: 필드에 담은 원문

	for _, e := range []error{err} {
		slog.Warn("b12", "m", e) // 잡는다: 범위 변수
	}
}
