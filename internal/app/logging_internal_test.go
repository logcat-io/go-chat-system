package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// D47: 거절 로그의 code 는 응답 code 를 따르되, 임대 상실은 lease_lost 로 busy 와 가른다. 응답은 busy 그대로다.
// code 보다 좁은 고정 사유는 reason 으로, 서버 잘못은 err_kind 로 싣는다. 원문은 싣지 않는다(D30).
func TestLog_거절로그는임대상실을lease_lost로busy와가르고좁은사유는reason에싣는다(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	cases := []struct {
		name               string
		err                error
		code, reason, kind string
	}{
		{"임대를 잃은 완료", translate(core.ErrLeaseLost), "lease_lost", "", ""},
		{"다른 요청이 확인 중", ErrUploadBusy, "busy", "", ""},
		{"먼저 가져감", ErrAlreadyTaken, "conflict", "already_taken", ""},
		{"남의 메시지", translate(core.ErrNotOwner), "forbidden", "not_owner", ""},
		{"파일 없음", invalid("file_missing", fileMissing), "invalid", "file_missing", ""},
		{"빈 본문은 필드의 사유 코드", invalid("body_required", "본문이 비어 있습니다"), "invalid", "body_required", ""},
		{"사유 코드 없는 입력 거절(app 밖에서 만든 값)", InvalidError{Reason: "본문이 비어 있습니다"}, "invalid", "", ""},
		{"되풀이된 경합", core.ErrTooManyRetries, "internal", "", "too_many_retries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf.Reset()
			logRejected(context.Background(), "upload.rejected", tc.err, "step", "complete")
			var l map[string]any
			if err := json.Unmarshal(buf.Bytes(), &l); err != nil {
				t.Fatalf("로그 한 줄이 아니다: %q", buf.String())
			}
			reason, _ := l["reason"].(string)
			kind, _ := l["err_kind"].(string)
			if l["code"] != tc.code || reason != tc.reason || kind != tc.kind {
				t.Errorf("code=%v reason=%q err_kind=%q, code=%s reason=%q err_kind=%q 여야 한다", l["code"], reason, kind, tc.code, tc.reason, tc.kind)
			}
		})
	}
	if err := translate(core.ErrLeaseLost); !errors.Is(err, ErrUploadBusy) || logctx.ErrKind(err) != "lease_lost" {
		t.Errorf("임대 상실은 응답 분기에서 busy 이고 오류 종류는 lease_lost 여야 한다: busy=%v kind=%s", errors.Is(err, ErrUploadBusy), logctx.ErrKind(err))
	}
}

// 감사 26: 고객 종료(D48)가 더한 거절은 응답과 같은 code 의 WARN 이다. 매핑이 빠지면 internal ERROR 로 찍혀
// ERROR 알람(deploy/cloudwatch)에 사용자 쪽 거절이 섞인다.
func TestLog_고객종료의거절은응답과같은code의WARN이고internal로찍히지않는다(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	cases := []struct {
		name         string
		err          error
		code, reason string
	}{
		{"고객이 닫은 상담에 고객 글", translate(core.ErrClosedByCustomer), "closed_by_customer", ""},
		{"답 없이 고객이 닫은 상담의 별점", ErrNoReply, "no_reply", ""},
		{"종료되지 않은 상담의 별점", ErrNotClosed, "not_closed", ""},
		{"상담원이 고객 종료를 부름", ErrCustomerOnly, "forbidden", "customer_only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf.Reset()
			logRejected(context.Background(), "conversation.close_rejected", tc.err)
			var l map[string]any
			if err := json.Unmarshal(buf.Bytes(), &l); err != nil {
				t.Fatalf("로그 한 줄이 아니다: %q", buf.String())
			}
			reason, _ := l["reason"].(string)
			if l["code"] != tc.code || reason != tc.reason || l["level"] != "WARN" {
				t.Errorf("code=%v reason=%q level=%v, code=%s reason=%q level=WARN 이어야 한다", l["code"], reason, l["level"], tc.code, tc.reason)
			}
		})
	}
}

// 감사 28: 시스템 줄의 수정·삭제 거절은 사용자 쪽 거절이다. 매핑이 빠지면 internal ERROR 로 찍혀 ERROR 알람에 섞인다.
func TestLog_시스템줄의수정삭제거절은system_line의WARN이고internal로찍히지않는다(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	logRevised(context.Background(), core.KindDelete, "", core.AppendResult{}, translate(core.ErrSystemLine))
	var l map[string]any
	if err := json.Unmarshal(buf.Bytes(), &l); err != nil {
		t.Fatalf("로그 한 줄이 아니다: %q", buf.String())
	}
	if l["code"] != "system_line" || l["level"] != "WARN" {
		t.Errorf("code=%v level=%v, code=system_line level=WARN 이어야 한다", l["code"], l["level"])
	}
}
