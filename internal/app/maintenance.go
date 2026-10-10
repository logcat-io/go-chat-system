package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// 주기 작업의 유스케이스다. 무엇을 어떤 순서로 하고 무엇을 남기는지는 여기서 정하고,
// 언제 부를지(주기)는 조립부(cmd/server)가 정한다. 한 번 부르는 것이 한 주기이고 trace 하나다.

// MaintenanceStore 는 주기 작업이 저장소에 바라는 것이다. 쓰는 쪽(여기)이 정의하고 Store 에 더하지 않는다.
type MaintenanceStore interface {
	// ReapOrphanAttachments 는 메시지에 붙지 못한 채 olderThan 보다 오래된 첨부 행을 한 문장으로 선점해 지우고
	// 그 파일 키를 돌려준다. 선점이 한 문장이라, 고르는 것과 지우는 것 사이에 메시지가 붙인 행은 걸리지 않는다.
	ReapOrphanAttachments(ctx context.Context, olderThan time.Duration, limit int) ([]string, error)
	// WakeSnoozed 는 보류 시각이 지난 대화를 다시 열고 그 id 를 돌려준다. 실시간 알림은 저장소가 낸다.
	WakeSnoozed(ctx context.Context) ([]string, error)
	// DeleteExpiredSessions 는 만료 세션을 지우고 지운 수를 돌려준다.
	DeleteExpiredSessions(ctx context.Context) (int64, error)
}

// TempSweeper 는 저장소가 가지면 쓰는 능력이다. 행 없이 남은 오래된 임시 파일을 지우고 지운 수를 돌려준다.
// 확인한 뒤 같은 양식으로 다시 올린 임시 파일은 행이 없어 고아 정리에 걸리지 않는다.
// 로컬 디스크 저장소가 가진다. S3 는 버킷 수명 주기 규칙(tmp/ 접두사 1일)에 맡긴다.
type TempSweeper interface {
	SweepTemp(olderThan time.Duration) (int, error)
}

const (
	// orphanUploadAge 가 지나도록 메시지에 붙지 못한 업로드는 고아다.
	orphanUploadAge = 6 * time.Hour
	// orphanReapBatch 는 한 주기에 치우는 고아 행의 상한이다.
	orphanReapBatch = 500
	// staleTempAge 가 지난 임시 파일은 행이 없으면 치운다.
	staleTempAge = time.Hour
)

// Maintenance 는 주기 작업 묶음이다. 파일 삭제는 첨부 규칙이 쓰는 것과 같은 길(Commands.RemoveFiles)을 탄다.
type Maintenance struct {
	cmd     *Commands
	store   MaintenanceStore
	sweeper TempSweeper
}

// NewMaintenance 는 주기 작업을 조립한다. sweeper 는 저장소가 그 능력을 가질 때만 넘기고, 없으면 nil 이다.
func NewMaintenance(cmd *Commands, store MaintenanceStore, sweeper TempSweeper) *Maintenance {
	return &Maintenance{cmd: cmd, store: store, sweeper: sweeper}
}

// CleanupUploads 는 메시지에 붙지 못한 업로드를 치운다.
// 행을 먼저 선점해 지우고 그 파일만 지운다. 파일 삭제가 실패하면 파일이 남는다 — 행이 남고 파일이 없는 것보다 낫다.
// 그 뒤 저장소가 할 수 있으면 행 없는 오래된 임시 파일을 지운다.
func (m *Maintenance) CleanupUploads(ctx context.Context) {
	ctx = logctx.NewTrace(ctx)
	paths, err := m.store.ReapOrphanAttachments(ctx, orphanUploadAge, orphanReapBatch)
	if err != nil {
		slog.WarnContext(ctx, "maintenance.failed", "task", "orphan_uploads", "err_kind", logctx.ErrKind(err))
	} else {
		m.cmd.RemoveFiles(ctx, paths)
		if len(paths) > 0 {
			slog.InfoContext(ctx, "maintenance.done", "task", "orphan_uploads", "rows", len(paths))
		}
	}
	if m.sweeper == nil {
		return
	}
	n, err := m.sweeper.SweepTemp(staleTempAge)
	if err != nil {
		slog.WarnContext(ctx, "maintenance.failed", "task", "temp_uploads", "err_kind", logctx.ErrKind(err))
		return
	}
	if n > 0 {
		slog.InfoContext(ctx, "maintenance.done", "task", "temp_uploads", "files", n)
	}
}

// WakeSnoozed 는 보류 시각이 지난 대화를 다시 연다.
func (m *Maintenance) WakeSnoozed(ctx context.Context) {
	ctx = logctx.NewTrace(ctx)
	woken, err := m.store.WakeSnoozed(ctx)
	if err != nil {
		slog.WarnContext(ctx, "maintenance.failed", "task", "wake_snoozed", "err_kind", logctx.ErrKind(err))
		return
	}
	if len(woken) > 0 {
		slog.InfoContext(ctx, "maintenance.done", "task", "wake_snoozed", "count", len(woken))
	}
	// 보류가 풀린 것도 상담의 상태 변화다. conversation_id 로 거른 생애에 빠지지 않게 한 줄씩 남긴다.
	for _, convID := range woken {
		slog.InfoContext(ctx, "conversation.state_changed", "conversation_id", convID,
			"actor_kind", "system", "from", core.StateSnoozed, "to", core.StateOpen)
	}
}

// CleanupSessions 는 만료 세션을 치운다.
func (m *Maintenance) CleanupSessions(ctx context.Context) {
	ctx = logctx.NewTrace(ctx)
	n, err := m.store.DeleteExpiredSessions(ctx)
	if err != nil {
		slog.WarnContext(ctx, "maintenance.failed", "task", "expired_sessions", "err_kind", logctx.ErrKind(err))
		return
	}
	if n > 0 {
		slog.InfoContext(ctx, "maintenance.done", "task", "expired_sessions", "rows", n)
	}
}
