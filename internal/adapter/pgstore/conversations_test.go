package pgstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 고객은 대화를 여러 개 가질 수 있다. 채널톡 위젯의 대화 목록과 같은 자리다.
// 전송 대기 메시지가 고아가 되는 문제는 브라우저가 저장된 모든 대화의 큐를 밀어서 막는다.
func TestConversations_고객당여러개(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, err := store.CreateCustomer(ctx, core.DefaultWorkspaceID, "재방문고객")
	if err != nil {
		t.Fatal(err)
	}

	first, err := store.CreateConversation(ctx, core.DefaultWorkspaceID, customerID, "")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("열린 대화가 있으면 그걸 이어 쓴다", func(t *testing.T) {
		again, err := store.LatestOpenConversation(ctx, customerID)
		if err != nil {
			t.Fatal(err)
		}
		if again.ID != first.ID {
			t.Errorf("다른 대화를 집었다: %s → %s", first.ID, again.ID)
		}
	})

	t.Run("새 문의는 별도 대화로 열린다", func(t *testing.T) {
		second, err := store.CreateConversation(ctx, core.DefaultWorkspaceID, customerID, "")
		if err != nil {
			t.Fatal(err)
		}
		if second.ID == first.ID {
			t.Fatal("새 대화가 안 생겼다")
		}
		convs, err := store.CustomerConversations(ctx, customerID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(convs) != 2 {
			t.Errorf("대화가 둘이어야 한다: %d개", len(convs))
		}
	})

	t.Run("전부 닫히면 열린 대화가 없다고 알린다", func(t *testing.T) {
		convs, err := store.CustomerConversations(ctx, customerID, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range convs {
			if _, err := store.SetConversationState(ctx, c.ID, "CLOSED", nil, nil); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := store.LatestOpenConversation(ctx, customerID); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("ErrNotFound 여야 한다: %v", err)
		}
	})

	t.Run("Reopen 없이 보내면 상태를 건드리지 않는다", func(t *testing.T) {
		if _, err := store.AppendMessage(ctx, customerMessage(first.ID, customerID, id.New(), "조용히 기록")); err != nil {
			t.Fatal(err)
		}
		conv, err := store.Conversation(ctx, first.ID)
		if err != nil {
			t.Fatal(err)
		}
		if conv.State != "CLOSED" {
			t.Errorf("상태 전환은 호출자가 정한다: %s", conv.State)
		}
	})

	t.Run("Reopen 을 켜고 보내면 다시 열린다", func(t *testing.T) {
		p := customerMessage(first.ID, customerID, id.New(), "이어서 문의")
		p.Reopen = true
		if _, err := store.AppendMessage(ctx, p); err != nil {
			t.Fatal(err)
		}
		conv, err := store.Conversation(ctx, first.ID)
		if err != nil {
			t.Fatal(err)
		}
		if conv.State != "OPEN" {
			t.Errorf("다시 열려야 한다: %s", conv.State)
		}
	})
}

// 내부 메모는 고객 번호를 쓰지 않는다. 그래야 고객 쪽 번호에 빈칸이 안 생긴다.
func TestAppendMessage_내부메모는고객번호를쓰지않는다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	agent := testsupport.Agent(t, store)

	send := func(visibility core.Visibility, body string) core.AppendResult {
		t.Helper()
		p := core.AppendParams{
			ConversationID: conv.ID, ClientMsgID: id.New(),
			SenderKind: core.SenderAgent, SenderAgentID: agent.ID,
			Visibility: visibility, Body: body,
		}
		res, err := store.AppendMessage(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	send(core.Public, "공개 1")
	send(core.Private, "내부 메모")
	send(core.Public, "공개 2")
	send(core.Private, "내부 메모 2")
	send(core.Public, "공개 3")

	t.Run("전체 번호는 1..5 연속이다", func(t *testing.T) {
		seqs, err := store.MessageSeqs(ctx, conv.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := []int64{1, 2, 3, 4, 5}
		if len(seqs) != len(want) {
			t.Fatalf("5건이어야 한다: %v", seqs)
		}
		for i, s := range seqs {
			if s != want[i] {
				t.Fatalf("전체 번호에 빈칸: %v", seqs)
			}
		}
	})

	t.Run("고객 번호는 1..3 연속이다", func(t *testing.T) {
		seqs, err := store.PublicSeqs(ctx, conv.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := []int64{1, 2, 3}
		if len(seqs) != len(want) {
			t.Fatalf("3건이어야 한다: %v", seqs)
		}
		for i, s := range seqs {
			if s != want[i] {
				t.Fatalf("고객 번호에 빈칸이 있으면 클라이언트가 유실로 읽는다: %v", seqs)
			}
		}
	})

	t.Run("고객 조회에는 내부 메모가 안 나온다", func(t *testing.T) {
		msgs, err := store.ListMessages(ctx, conv.ID, 0, 100, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 3 {
			t.Fatalf("공개 3건만 나와야 한다: %d건", len(msgs))
		}
		for i, m := range msgs {
			if m.Visibility != core.Public {
				t.Errorf("내부 메모가 샜다: %+v", m)
			}
			if m.Seq != int64(i+1) {
				t.Errorf("고객이 보는 번호가 1..3 이어야 한다: %d", m.Seq)
			}
		}
	})

	t.Run("상담원 조회에는 전부 나온다", func(t *testing.T) {
		msgs, err := store.ListMessages(ctx, conv.ID, 0, 100, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 5 {
			t.Fatalf("5건이 나와야 한다: %d건", len(msgs))
		}
		if msgs[4].Seq != 5 {
			t.Errorf("상담원은 전체 번호를 본다: %d", msgs[4].Seq)
		}
	})

	_ = customerID
}

// 권한 검사는 저장소 앞에서 끝난다. 검사를 지난 뒤 삭제가 끼어든 요청이 저장소에 닿아도 지운 대화를 바꾸지 못한다.
func TestConversations_삭제된대화의쓰기(t *testing.T) {
	t.Run("삭제된 대화의 상태 변경·배정·태그 붙이기·팔로우는 ErrNotFound 이고 아무것도 바뀌지 않는다", func(t *testing.T) {
		store := testsupport.Store(t)
		ctx := context.Background()
		_, conv := testsupport.Conversation(t, store)
		agent := testsupport.Agent(t, store)
		tag, err := store.EnsureTag(ctx, core.DefaultWorkspaceID, "삭제후-"+id.New()[28:])
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteConversation(ctx, conv.ID, agent.ID, "정리"); err != nil {
			t.Fatal(err)
		}
		type snapshot struct {
			state, assignee, updatedAt string
			tags, followers            int
		}
		snap := func() snapshot {
			t.Helper()
			var s snapshot
			if err := store.Pool().QueryRow(ctx, `
				SELECT state, COALESCE(assignee_agent_id::text, ''), updated_at::text,
				       (SELECT count(*) FROM conversation_tags WHERE conversation_id = c.id),
				       (SELECT count(*) FROM conversation_followers WHERE conversation_id = c.id)
				  FROM conversations c WHERE id = $1`, conv.ID).
				Scan(&s.state, &s.assignee, &s.updatedAt, &s.tags, &s.followers); err != nil {
				t.Fatal(err)
			}
			return s
		}
		before := snap()

		if _, err := store.SetConversationState(ctx, conv.ID, "CLOSED", nil, nil); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("상태 변경: ErrNotFound 여야 한다: %v", err)
		}
		if _, err := store.AssignConversation(ctx, conv.ID, agent.ID, nil, nil); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("배정: ErrNotFound 여야 한다: %v", err)
		}
		if _, err := store.AssignConversation(ctx, conv.ID, "", nil, nil); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("배정 해제: ErrNotFound 여야 한다: %v", err)
		}
		if err := store.AddConversationTag(ctx, conv.ID, tag.ID); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("태그 붙이기: ErrNotFound 여야 한다: %v", err)
		}
		if err := store.Follow(ctx, conv.ID, agent.ID); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("팔로우: ErrNotFound 여야 한다: %v", err)
		}
		if after := snap(); after != before {
			t.Errorf("지운 대화가 바뀌었다: %+v → %+v", before, after)
		}

		// 되살리면 다시 바꾼다.
		if err := store.RestoreConversation(ctx, conv.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AssignConversation(ctx, conv.ID, agent.ID, nil, nil); err != nil {
			t.Errorf("되살린 대화는 배정된다: %v", err)
		}
		if err := store.AddConversationTag(ctx, conv.ID, tag.ID); err != nil {
			t.Errorf("되살린 대화에는 태그가 붙는다: %v", err)
		}
		if err := store.Follow(ctx, conv.ID, agent.ID); err != nil {
			t.Errorf("되살린 대화는 팔로우된다: %v", err)
		}
		if _, err := store.SetConversationState(ctx, conv.ID, "CLOSED", nil, nil); err != nil {
			t.Errorf("되살린 대화는 상태가 바뀐다: %v", err)
		}
	})

	t.Run("삭제된 대화의 첨부 신청·완료 도장·읽음은 ErrNotFound 이고 행이 생기지 않는다", func(t *testing.T) {
		store := testsupport.Store(t)
		ctx := context.Background()
		customerID, conv := testsupport.Conversation(t, store)
		agent := testsupport.Agent(t, store)
		if _, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), "지우기 전")); err != nil {
			t.Fatal(err)
		}
		newAttachment := func() core.NewAttachment {
			attID := id.New()
			return core.NewAttachment{ID: attID, ConversationID: conv.ID, UploaderKind: "CUSTOMER", UploaderID: customerID,
				Filename: "a.txt", ContentType: "text/plain", ByteSize: 5, StoragePath: "tmp/" + attID}
		}
		// 삭제 전에 신청해 둔 첨부. 권한 검사를 지난 완료 요청이 삭제 뒤에 도장을 찍으려는 모습이다.
		pre := newAttachment()
		if _, err := store.CreateAttachment(ctx, pre, 10); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := store.ClaimAttachment(ctx, pre.ID, "tok", time.Minute); err != nil || !claimed {
			t.Fatal(claimed, err)
		}
		// 삭제 전에 확인을 마친 첨부. 삭제 뒤의 재완료는 그 첨부를 돌려주지 않는다.
		done := newAttachment()
		if _, err := store.CreateAttachment(ctx, done, 10); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := store.ClaimAttachment(ctx, done.ID, "tok", time.Minute); err != nil || !claimed {
			t.Fatal(claimed, err)
		}
		if _, _, err := store.CompleteAttachment(ctx, done.ID, "tok", "ws/x/y/"+done.ID, "text/plain", 0, 0); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteConversation(ctx, conv.ID, agent.ID, "정리"); err != nil {
			t.Fatal(err)
		}
		count := func(q string) int {
			t.Helper()
			var n int
			if err := store.Pool().QueryRow(ctx, q, conv.ID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}

		if _, err := store.CreateAttachment(ctx, newAttachment(), 10); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("첨부 신청: ErrNotFound 여야 한다: %v", err)
		}
		if n := count("SELECT count(*) FROM attachments WHERE conversation_id = $1"); n != 2 {
			t.Errorf("삭제된 대화에 첨부 행이 생겼다: %d", n)
		}
		if _, _, err := store.CompleteAttachment(ctx, pre.ID, "tok", "ws/x/y/"+pre.ID, "text/plain", 0, 0); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("완료 도장: ErrNotFound 여야 한다: %v", err)
		}
		if _, _, err := store.CompleteAttachment(ctx, done.ID, "tok", "ws/x/y/"+done.ID, "text/plain", 0, 0); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("확인된 첨부의 재완료: ErrNotFound 여야 한다: %v", err)
		}
		if _, err := store.ConfirmedAttachment(ctx, done.ID); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("확인된 첨부 조회: ErrNotFound 여야 한다: %v", err)
		}
		if n := count("SELECT count(*) FROM attachments WHERE conversation_id = $1 AND uploaded_at IS NOT NULL"); n != 1 {
			t.Errorf("삭제된 대화의 첨부가 확인됨이 됐다: %d", n)
		}
		agentRead := core.LiveEvent{Kind: core.LiveRead, ConversationID: conv.ID, WorkspaceID: core.DefaultWorkspaceID,
			ActorKind: string(core.SenderAgent), ActorID: agent.ID, ActorName: "상담원"}
		if _, err := store.MarkRead(ctx, conv.ID, agentRead, 1, true); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("상담원 읽음: ErrNotFound 여야 한다: %v", err)
		}
		customerRead := core.LiveEvent{Kind: core.LiveRead, ConversationID: conv.ID, WorkspaceID: core.DefaultWorkspaceID,
			ActorKind: string(core.SenderCustomer), ActorID: customerID, ActorName: "고객"}
		if _, err := store.MarkRead(ctx, conv.ID, customerRead, 1, false); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("고객 읽음: ErrNotFound 여야 한다: %v", err)
		}
		if n := count("SELECT count(*) FROM conversation_reads WHERE conversation_id = $1"); n != 0 {
			t.Errorf("삭제된 대화에 읽음 행이 생겼다: %d", n)
		}

		// 되살리면 다시 된다.
		if err := store.RestoreConversation(ctx, conv.ID); err != nil {
			t.Fatal(err)
		}
		if _, stamped, err := store.CompleteAttachment(ctx, pre.ID, "tok", "ws/x/y/"+pre.ID, "text/plain", 0, 0); err != nil || !stamped {
			t.Errorf("되살린 대화의 첨부는 도장이 찍힌다: %v %v", stamped, err)
		}
		if _, err := store.ConfirmedAttachment(ctx, done.ID); err != nil {
			t.Errorf("되살린 대화의 확인된 첨부는 다시 보인다: %v", err)
		}
		if _, err := store.MarkRead(ctx, conv.ID, agentRead, 1, true); err != nil {
			t.Errorf("되살린 대화에는 읽음이 남는다: %v", err)
		}
	})

	t.Run("커밋 전의 삭제와 겹친 첨부 신청·완료 도장·읽음은 삭제를 기다렸다가 ErrNotFound 이고 행이 생기지 않는다", func(t *testing.T) {
		// 권한 검사를 지난 요청과 삭제가 겹친 모습이다. 대화 행을 FOR SHARE 로 잡지 않으면 커밋 전의 삭제를 못 보고 지나간다.
		store := testsupport.Store(t)
		ctx := context.Background()
		customerID, conv := testsupport.Conversation(t, store)
		agent := testsupport.Agent(t, store)
		if _, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), "지우기 전")); err != nil {
			t.Fatal(err)
		}
		newAttachment := func() core.NewAttachment {
			attID := id.New()
			return core.NewAttachment{ID: attID, ConversationID: conv.ID, UploaderKind: "CUSTOMER", UploaderID: customerID,
				Filename: "a.txt", ContentType: "text/plain", ByteSize: 5, StoragePath: "tmp/" + attID}
		}
		pre := newAttachment()
		if _, err := store.CreateAttachment(ctx, pre, 10); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := store.ClaimAttachment(ctx, pre.ID, "tok", time.Minute); err != nil || !claimed {
			t.Fatal(claimed, err)
		}

		tx, err := store.Pool().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "UPDATE conversations SET deleted_at = clock_timestamp() WHERE id = $1", conv.ID); err != nil {
			t.Fatal(err)
		}

		type result struct {
			what string
			err  error
		}
		results := make(chan result, 3)
		go func() {
			_, err := store.CreateAttachment(ctx, newAttachment(), 10)
			results <- result{"첨부 신청", err}
		}()
		go func() {
			_, _, err := store.CompleteAttachment(ctx, pre.ID, "tok", "ws/x/y/"+pre.ID, "text/plain", 0, 0)
			results <- result{"완료 도장", err}
		}()
		go func() {
			ev := core.LiveEvent{Kind: core.LiveRead, ConversationID: conv.ID, WorkspaceID: core.DefaultWorkspaceID,
				ActorKind: string(core.SenderAgent), ActorID: agent.ID, ActorName: "상담원"}
			_, err := store.MarkRead(ctx, conv.ID, ev, 1, true)
			results <- result{"읽음", err}
		}()

		time.Sleep(300 * time.Millisecond)
		if early := len(results); early != 0 {
			t.Errorf("%d개가 커밋 전의 삭제를 기다리지 않고 끝났다", early)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			select {
			case r := <-results:
				if !errors.Is(r.err, core.ErrNotFound) {
					t.Errorf("%s: ErrNotFound 여야 한다: %v", r.what, r.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("삭제가 커밋된 뒤에도 끝나지 않았다")
			}
		}

		count := func(q string) int {
			t.Helper()
			var n int
			if err := store.Pool().QueryRow(ctx, q, conv.ID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		if n := count("SELECT count(*) FROM attachments WHERE conversation_id = $1"); n != 1 {
			t.Errorf("삭제된 대화에 첨부 행이 생겼다: %d", n)
		}
		if n := count("SELECT count(*) FROM attachments WHERE conversation_id = $1 AND uploaded_at IS NOT NULL"); n != 0 {
			t.Errorf("삭제된 대화의 첨부가 확인됨이 됐다: %d", n)
		}
		if n := count("SELECT count(*) FROM conversation_reads WHERE conversation_id = $1"); n != 0 {
			t.Errorf("삭제된 대화에 읽음 행이 생겼다: %d", n)
		}
	})
}

// 위젯의 상담 카드 제목은 마지막 글이다(656). 자동 글을 고르면 고객이 첫 글을 보내자마자 접수 안내가 마지막 글이 되어 새 상담 카드가 전부 같아진다.
// AI 응대의 답(D45)은 고객에게 한 답이라 고른다(감사 20, logcat 결정 — 전에는 고객·상담원 글만 골랐다).
func TestCustomerConversations_고객목록의마지막글은고객상담원AI가쓴공개글이고시스템과자동응답은고르지않는다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	agent := testsupport.Agent(t, store)

	send := func(p core.AppendParams) {
		t.Helper()
		p.ConversationID, p.ClientMsgID = conv.ID, id.New()
		if p.Visibility == "" {
			p.Visibility = core.Public
		}
		if _, err := store.AppendMessage(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	bot := func(body string) { send(core.AppendParams{SenderKind: core.SenderBot, Body: body}) }
	system := func(body string) { send(core.AppendParams{SenderKind: core.SenderSystem, Body: body}) }
	mine := func() core.Conversation {
		t.Helper()
		convs, err := store.CustomerConversations(ctx, customerID, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range convs {
			if c.ID == conv.ID {
				return c
			}
		}
		t.Fatal("목록에 대화가 없다")
		return core.Conversation{}
	}

	t.Run("자동 글만 있으면 마지막 글은 비어 있다", func(t *testing.T) {
		bot("환영합니다")
		if c := mine(); c.LastBody != "" || c.LastSenderKind != "" {
			t.Errorf("자동 글을 골랐다: %q %q", c.LastBody, c.LastSenderKind)
		}
	})

	t.Run("고객의 첫 글 뒤에 접수 안내와 시스템 글이 와도 마지막 글은 고객의 글이다", func(t *testing.T) {
		send(customerMessage(conv.ID, customerID, "", "환불 문의"))
		bot("문의가 접수되었습니다")
		system("공개 시스템 글")
		c := mine()
		if c.LastBody != "환불 문의" || c.LastSenderKind != "CUSTOMER" {
			t.Errorf("고객의 글이어야 한다: %q %q", c.LastBody, c.LastSenderKind)
		}
		// 정렬 기준과 안 읽은 수는 그대로다 — 자동 응답도 고객이 읽을 메시지다.
		if c.Unread != 3 {
			t.Errorf("안 읽은 수는 환영·접수 안내·시스템 글 3이어야 한다: %d", c.Unread)
		}
	})

	t.Run("상담원의 답 뒤에 자동 글이 와도 마지막 글은 상담원의 답이다", func(t *testing.T) {
		send(core.AppendParams{SenderKind: core.SenderAgent, SenderAgentID: agent.ID, Body: "확인했습니다"})
		bot("부재중입니다")
		if c := mine(); c.LastBody != "확인했습니다" || c.LastSenderKind != "AGENT" {
			t.Errorf("상담원의 답이어야 한다: %q %q", c.LastBody, c.LastSenderKind)
		}
	})

	t.Run("AI 의 답 뒤에 자동 글이 와도 마지막 글은 AI 의 답이다", func(t *testing.T) {
		send(core.AppendParams{SenderKind: core.SenderAI, Body: "주문 번호를 알려 주시면 바로 확인해 드립니다"})
		bot("부재중입니다")
		if c := mine(); c.LastBody != "주문 번호를 알려 주시면 바로 확인해 드립니다" || c.LastSenderKind != "AI" {
			t.Errorf("AI 의 답이어야 한다: %q %q", c.LastBody, c.LastSenderKind)
		}
	})

	t.Run("글 없이 파일만 보낸 고객의 글도 마지막 글이다", func(t *testing.T) {
		attID := id.New()
		if _, err := store.CreateAttachment(ctx, core.NewAttachment{ID: attID, ConversationID: conv.ID, UploaderKind: "CUSTOMER", UploaderID: customerID,
			Filename: "a.txt", ContentType: "text/plain", ByteSize: 5, StoragePath: "tmp/" + attID}, 10); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := store.ClaimAttachment(ctx, attID, "tok", time.Minute); err != nil || !claimed {
			t.Fatal(claimed, err)
		}
		if _, _, err := store.CompleteAttachment(ctx, attID, "tok", "ws/x/y/"+attID, "text/plain", 0, 0); err != nil {
			t.Fatal(err)
		}
		p := customerMessage(conv.ID, customerID, "", "")
		p.AttachmentID = attID
		send(p)
		bot("자동 글")
		if c := mine(); c.LastBody != "" || c.LastSenderKind != "CUSTOMER" {
			t.Errorf("파일만 보낸 고객의 글이어야 한다: %q %q", c.LastBody, c.LastSenderKind)
		}
	})
}

// inboxRow 는 상담원 받은함(ListConversations)에서 그 대화의 줄을 찾는다.
func inboxRow(t *testing.T, store *pgstore.Store, conv core.Conversation) core.Conversation {
	t.Helper()
	list, err := store.ListConversations(context.Background(), core.InboxFilter{WorkspaceID: conv.WorkspaceID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if c.ID == conv.ID {
			return c
		}
	}
	t.Fatal("받은함에 대화가 없다")
	return core.Conversation{}
}

// 내부 메모는 상담원끼리의 글이다. 고객 목록이 그것을 고르면 메모 본문이 고객 위젯의 카드 제목으로 새어 나간다(감사 19 G1 — visibility 조건을 빼도 통과했다).
func TestCustomerConversations_고객목록의마지막글은내부메모를고르지않는다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	agent := testsupport.Agent(t, store)

	send := func(p core.AppendParams) {
		t.Helper()
		p.ConversationID, p.ClientMsgID = conv.ID, id.New()
		if _, err := store.AppendMessage(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	memo := func(body string) {
		send(core.AppendParams{SenderKind: core.SenderAgent, SenderAgentID: agent.ID, Visibility: core.Private, Body: body})
	}
	mine := func() core.Conversation {
		t.Helper()
		convs, err := store.CustomerConversations(ctx, customerID, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range convs {
			if c.ID == conv.ID {
				return c
			}
		}
		t.Fatal("목록에 대화가 없다")
		return core.Conversation{}
	}

	t.Run("고객 글 뒤에 상담원이 내부 메모를 남겨도 마지막 글은 고객의 글이다", func(t *testing.T) {
		send(customerMessage(conv.ID, customerID, "", "환불 문의"))
		memo("내부 메모 SECRET-1")
		c := mine()
		if c.LastBody != "환불 문의" || c.LastSenderKind != "CUSTOMER" {
			t.Errorf("고객의 글이어야 한다: %q %q", c.LastBody, c.LastSenderKind)
		}
		if strings.Contains(c.LastBody, "SECRET") {
			t.Errorf("내부 메모가 고객 목록에 나갔다: %q", c.LastBody)
		}
	})

	t.Run("상담원의 공개 답 뒤에 내부 메모가 와도 마지막 글은 공개 답이다", func(t *testing.T) {
		send(core.AppendParams{SenderKind: core.SenderAgent, SenderAgentID: agent.ID, Visibility: core.Public, Body: "확인했습니다"})
		memo("내부 메모 SECRET-2")
		if c := mine(); c.LastBody != "확인했습니다" || c.LastSenderKind != "AGENT" {
			t.Errorf("상담원의 공개 답이어야 한다: %q %q", c.LastBody, c.LastSenderKind)
		}
	})

	// 받은함은 상담원 화면이라 바꾸지 않았다 — 내부 메모도 마지막 글로 보인다.
	t.Run("받은함의 마지막 글은 그대로 내부 메모다", func(t *testing.T) {
		if c := inboxRow(t, store, conv); c.LastBody != "내부 메모 SECRET-2" || c.LastSenderKind != "AGENT" {
			t.Errorf("받은함이 바뀌었다: %q %q", c.LastBody, c.LastSenderKind)
		}
	})

	// 감사 19 G2: 받은함에 sender_kind 조건이 새어도 pgstore 테스트는 통과했다. 받은함은 자동 글도 마지막 글로 보인다.
	t.Run("받은함의 마지막 글은 그대로 자동 글도 고른다", func(t *testing.T) {
		send(core.AppendParams{SenderKind: core.SenderBot, Visibility: core.Public, Body: "부재중입니다"})
		if c := inboxRow(t, store, conv); c.LastBody != "부재중입니다" || c.LastSenderKind != "BOT" {
			t.Errorf("받은함이 자동 글을 건너뛰었다: %q %q", c.LastBody, c.LastSenderKind)
		}
		if c := mine(); c.LastBody != "확인했습니다" {
			t.Errorf("고객 목록은 자동 글을 고르지 않는다: %q", c.LastBody)
		}
	})
}

// 지운 글은 본문이 비어 위젯이 "파일을 보냈습니다" 로 그린다. 고객 목록은 그 글을 건너뛰고 그 앞 글을 제목으로 쓴다. 받은함은 그대로다.
func TestCustomerConversations_고객목록의마지막글은지운글을건너뛰고그앞글을고른다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)

	sendCustomer := func(body string) string {
		t.Helper()
		res, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), body))
		if err != nil {
			t.Fatal(err)
		}
		return res.Message.ID
	}
	remove := func(msgID string) {
		t.Helper()
		if _, err := store.ReviseMessage(ctx, core.KindDelete, core.ReviseParams{ConversationID: conv.ID, MessageID: msgID,
			ActorKind: core.SenderCustomer, ActorCustomer: customerID}); err != nil {
			t.Fatal(err)
		}
	}
	mine := func() core.Conversation {
		t.Helper()
		convs, err := store.CustomerConversations(ctx, customerID, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range convs {
			if c.ID == conv.ID {
				return c
			}
		}
		t.Fatal("목록에 대화가 없다")
		return core.Conversation{}
	}

	first := sendCustomer("환불 문의")
	last := sendCustomer("잘못 보냈어요")

	t.Run("마지막 고객 글을 지우면 그 앞 글이 제목이다", func(t *testing.T) {
		remove(last)
		if c := mine(); c.LastBody != "환불 문의" || c.LastSenderKind != "CUSTOMER" {
			t.Errorf("지운 글 앞의 글이어야 한다: %q %q", c.LastBody, c.LastSenderKind)
		}
		// 받은함은 바꾸지 않았다(감사 19 G3 — 받은함에 deleted_at 조건이 새어도 통과했다). 지운 글이 그대로 마지막 줄이다.
		if c := inboxRow(t, store, conv); c.LastBody != "" || c.LastSenderKind != "CUSTOMER" {
			t.Errorf("받은함이 지운 글을 건너뛰었다: %q %q", c.LastBody, c.LastSenderKind)
		}
	})

	t.Run("고객·상담원 글을 모두 지우면 마지막 글은 비어 있다", func(t *testing.T) {
		remove(first)
		if c := mine(); c.LastBody != "" || c.LastSenderKind != "" {
			t.Errorf("지운 글을 골랐다: %q %q", c.LastBody, c.LastSenderKind)
		}
	})
}
