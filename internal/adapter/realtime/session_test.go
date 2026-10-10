package realtime

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 열린 연결도 세션을 다시 확인한다. 로그아웃·폐기된 세션이면 왜 끊는지 알리고 정상 종료(1000)로 닫는다.
// 알리는 문구는 내부 오류 문구가 아니다. 1006 으로 끊기면 클라이언트는 장애로 읽고 같은 토큰으로 다시 붙는다.
func TestSession_세션이끝난연결은unauthorized와다시로그인하라는문구를받고1000으로닫힌다(t *testing.T) {
	store := testsupport.Store(t)
	custID, conv := testsupport.Conversation(t, store)
	srv := NewServer(app.New(store), NewHub(), 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// 세션 재확인 주기(1분)를 기다리지 않는다. 틱마다 다시 확인한다.
	srv.connTick, srv.sessionRecheck = 50*time.Millisecond, 0

	var alive atomic.Bool
	alive.Store(true)
	actor := app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.Handle(w, r, actor, func(context.Context) error {
			if alive.Load() {
				return nil
			}
			return app.ErrNotFound
		})
	}))
	t.Cleanup(hs.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hs.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	sub, _ := json.Marshal(map[string]any{"type": "subscribe", "conversation_id": conv.ID, "after_seq": 0})
	if err := ws.Write(ctx, websocket.MessageText, sub); err != nil {
		t.Fatal(err)
	}

	type frame struct{ Type, Code, Message string }
	var frames []frame
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			if code := websocket.CloseStatus(err); code != websocket.StatusNormalClosure {
				t.Fatalf("close 코드가 %d 다(%v). 1000 이어야 한다. 받은 프레임: %+v", code, err, frames)
			}
			break
		}
		var f frame
		_ = json.Unmarshal(data, &f)
		frames = append(frames, f)
		if f.Type == frameReady {
			alive.Store(false) // 로그아웃했다
		}
	}
	last := frames[len(frames)-1]
	if last.Type != frameError || last.Code != "unauthorized" || last.Message != "로그인 정보가 만료되었습니다. 다시 로그인하거나 페이지를 새로 고쳐 주세요" {
		t.Errorf("닫기 전 마지막 프레임이 %+v 다. unauthorized 와 다시 로그인하라는 문구여야 한다", last)
	}
}
