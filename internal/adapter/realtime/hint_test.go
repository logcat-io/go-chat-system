package realtime

import (
	"testing"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 힌트는 성능 최적화의 핵심이면서 유실로 이어질 수 있는 유일한 지름길이다.
// "질의를 건너뛴다" 는 판단이 틀리면 메시지가 조용히 안 간다.
//
// 여기서 고정하는 정책은 셋이다.
//   - 겹쳐 온 알림 중 가장 앞선 번호가 남는다 (뒤처진 알림이 앞선 걸 덮지 않는다)
//   - 메타 변경 표식은 한 번이라도 켜지면 꺼지지 않는다
//   - 번호를 모르는 알림(안전망 경로)은 반드시 메타 확인을 시킨다

func TestHint_겹친알림(t *testing.T) {
	hub := NewHub()
	sub := hub.Join("")
	hub.Bind(sub, "conv-1")
	sub.TakeHint() // Bind 가 남긴 것 비우기

	t.Run("가장 앞선 번호가 남는다", func(t *testing.T) {
		hub.NotifyChangeHint("conv-1", Hint{Seq: 5, PublicSeq: 3})
		hub.NotifyChangeHint("conv-1", Hint{Seq: 9, PublicSeq: 6})
		hub.NotifyChangeHint("conv-1", Hint{Seq: 7, PublicSeq: 4})

		got := sub.TakeHint()
		if got.Seq != 9 || got.PublicSeq != 6 {
			t.Errorf("뒤처진 알림이 앞선 번호를 덮었다: %+v", got)
		}
	})

	t.Run("꺼낸 뒤에는 비어 있다", func(t *testing.T) {
		if got := sub.TakeHint(); got.Seq != 0 || got.PublicSeq != 0 || got.Meta {
			t.Errorf("힌트가 안 비워졌다: %+v", got)
		}
	})

	t.Run("메타 표식은 한 번 켜지면 꺼지지 않는다", func(t *testing.T) {
		hub.NotifyChangeHint("conv-1", Hint{Seq: 10, Meta: true})
		hub.NotifyChangeHint("conv-1", Hint{Seq: 11, Meta: false})

		if got := sub.TakeHint(); !got.Meta {
			t.Error("메타 변경을 놓치면 상태·담당자·태그가 화면에 안 반영된다")
		}
	})

	t.Run("번호를 모르는 알림은 메타 확인을 시킨다", func(t *testing.T) {
		hub.NotifyChange("conv-1") // 안전망 경로. 번호를 모른다
		if got := sub.TakeHint(); !got.Meta {
			t.Error("번호를 모르면 받는 쪽이 직접 확인해야 한다")
		}
	})
}

func TestHint_보는사람에따라다른번호(t *testing.T) {
	// 같은 알림이라도 상담원은 전체 번호를, 고객은 공개 번호를 본다.
	// 이 환산이 틀리면 한쪽이 "보낼 것 없음" 으로 잘못 판단한다.
	h := Hint{Seq: 9, PublicSeq: 6}

	if got := h.ViewerSeq(true); got != 9 {
		t.Errorf("상담원은 전체 번호를 본다: %d", got)
	}
	if got := h.ViewerSeq(false); got != 6 {
		t.Errorf("고객은 공개 번호를 본다: %d", got)
	}
}

func TestHint_대화전환(t *testing.T) {
	hub := NewHub()
	sub := hub.Join("")

	hub.Bind(sub, "conv-a")
	hub.NotifyChangeHint("conv-a", Hint{Seq: 50, PublicSeq: 40})

	t.Run("대화를 바꾸면 옛 대화의 번호를 버린다", func(t *testing.T) {
		hub.Bind(sub, "conv-b")
		got := sub.TakeHint()
		if got.Seq != 0 || got.PublicSeq != 0 {
			t.Errorf("옛 대화 번호를 들고 가면 새 대화의 메시지를 건너뛴다: %+v", got)
		}
		if !got.Meta {
			t.Error("대화를 바꾼 직후에는 전체 확인을 시켜야 한다")
		}
	})
}

func TestHint_재동기화(t *testing.T) {
	hub := NewHub()
	sub := hub.Join("")
	hub.Bind(sub, "conv-1")
	sub.TakeHint()

	// 알림 리스너가 재접속했다. 그 사이 알림은 복구할 수 없다.
	hub.Resync()

	if got := sub.TakeHint(); !got.Meta {
		t.Error("놓친 구간이 있으면 번호를 믿지 말고 전부 다시 확인해야 한다")
	}
}

func TestHint_휘발성이벤트는버려도된다(t *testing.T) {
	hub := NewHub()
	sub := hub.Join("")
	hub.Bind(sub, "conv-1")

	// 읽는 쪽이 없는 동안 버퍼보다 많이 보낸다.
	for i := 0; i < liveBuffer*3; i++ {
		hub.DeliverLive(core.LiveEvent{
			Kind: core.LiveTyping, ConversationID: "conv-1", ActorKind: "AGENT",
		})
	}

	t.Run("버퍼가 넘쳐도 막히지 않는다", func(t *testing.T) {
		// 여기까지 왔으면 막히지 않은 것이다. 버린 만큼은 복구하지 않는다.
		got := 0
		for {
			select {
			case <-sub.Live():
				got++
				continue
			default:
			}
			break
		}
		if got == 0 {
			t.Error("적어도 몇 개는 전달돼야 한다")
		}
		if got > liveBuffer {
			t.Errorf("버퍼보다 많이 들고 있다: %d", got)
		}
	})
}

// 저장소가 알린 변경은 Hub.OnChange 한 곳에서 힌트로 옮겨진다. 전체 번호는 Seq 로, 공개 번호는 PublicSeq 로 간다.
// 뒤바뀌면 고객이 내부 메모의 번호까지 기다리거나 상담원이 공개 번호에서 멈춘다.
func TestHub_OnChange는번호를같은이름의힌트필드로옮긴다(t *testing.T) {
	hub := NewHub()
	sub := hub.Join("ws-1")
	hub.Bind(sub, "conv-1")
	sub.TakeHint() // Bind 가 남긴 것 비우기

	hub.OnChange(core.Change{ConversationID: "conv-1", WorkspaceID: "ws-1", Seq: 9, PublicSeq: 6})

	if got := sub.TakeHint(); got.Seq != 9 || got.PublicSeq != 6 || got.Meta {
		t.Errorf("힌트가 변경과 다르다: %+v", got)
	}
	select {
	case <-sub.InboxWake():
	default:
		t.Error("같은 워크스페이스의 받은함 신호가 오지 않았다")
	}
}
