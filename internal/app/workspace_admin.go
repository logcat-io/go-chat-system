package app

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
)

// 워크스페이스 = 테넌트. 설정·감사 로그·내보내기·고객 삭제.
// 회사 셀프 가입(Signup)은 2026-10-05 에 걷어냈다 — 설치형 단일 워크스페이스다(D35).

// WorkspaceView 는 관리자 설정 화면이 보는 것이다. 회원 비밀은 여기에만 실리고 다른 응답엔 없다.
// PublicURL 은 설치 코드에 들어갈 서버 주소다. app 은 설정을 모르니 HTTP 어댑터가 채운다.
type WorkspaceView struct {
	core.Workspace
	MemberSecret string `json:"member_secret"`
	PublicURL    string `json:"public_url,omitempty"`
}

func (s *Queries) Workspace(ctx context.Context, actor Actor) (WorkspaceView, error) {
	if err := require(ctx, actor, PermWorkspaceManage); err != nil {
		return WorkspaceView{}, err
	}
	ws, err := s.read.Workspace(ctx, actor.WorkspaceID)
	if err != nil {
		return WorkspaceView{}, translate(err)
	}
	return WorkspaceView{Workspace: ws, MemberSecret: ws.MemberSecret}, nil
}

// WorkspaceSummary 는 상담원 누구나 보는 소속 정보다. 키·비밀은 없다.
type WorkspaceSummary struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

func (s *Queries) WorkspaceSummary(ctx context.Context, actor Actor) (WorkspaceSummary, error) {
	ws, err := s.read.Workspace(ctx, actor.WorkspaceID)
	if err != nil {
		return WorkspaceSummary{}, translate(err)
	}
	return WorkspaceSummary{ID: ws.ID, Slug: ws.Slug, Name: ws.Name}, nil
}

// UpdateWorkspace 는 이름과 허용 오리진을 바꾼다. 오리진은 scheme://host[:port] 만 받는다.
func (s *Commands) UpdateWorkspace(ctx context.Context, actor Actor, name string, origins []string) (WorkspaceView, error) {
	if err := require(ctx, actor, PermWorkspaceManage); err != nil {
		return WorkspaceView{}, err
	}
	name, err := trimmedRunes(name, 80, "workspace_name_too_long", "워크스페이스 이름")
	if err != nil {
		return WorkspaceView{}, err
	}
	if name == "" {
		return WorkspaceView{}, invalid("workspace_name_required", "워크스페이스 이름이 비어 있습니다")
	}
	clean := make([]string, 0, len(origins))
	for _, o := range origins {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" {
			return WorkspaceView{}, invalid("origin_format", "허용 오리진은 https://example.com 모양이어야 합니다: %s", o)
		}
		clean = append(clean, strings.ToLower(u.Scheme+"://"+u.Host))
	}
	if len(clean) > 50 {
		return WorkspaceView{}, invalid("origins_too_many", "허용 오리진은 50개까지입니다")
	}
	if _, err := s.write.UpdateWorkspace(ctx, actor.WorkspaceID, name, clean); err != nil {
		return WorkspaceView{}, translate(err)
	}
	s.audit(ctx, actor.WorkspaceID, actor, "workspace.update", "", map[string]any{"name": name, "allowed_origins": clean})
	return s.q.Workspace(ctx, actor)
}

// RotateMemberSecret 은 돌리는 순간 옛 비밀로 서명한 토큰이 전부 죽는다. 호스트 서버부터 바꾸고 눌러야 한다.
func (s *Commands) RotateMemberSecret(ctx context.Context, actor Actor) (WorkspaceView, error) {
	if err := require(ctx, actor, PermWorkspaceManage); err != nil {
		return WorkspaceView{}, err
	}
	if _, err := s.write.RotateMemberSecret(ctx, actor.WorkspaceID); err != nil {
		return WorkspaceView{}, translate(err)
	}
	s.audit(ctx, actor.WorkspaceID, actor, "workspace.rotate_member_secret", "", nil)
	return s.q.Workspace(ctx, actor)
}

// ── 감사 로그 ──────────────────────────────────────────────────────

// audit 은 실패를 삼킨다. 기록이 빠지는 것보다 설정 변경이 막히는 게 더 나쁘다.
func (s *Commands) audit(ctx context.Context, wsID string, actor Actor, action, target string, detail map[string]any) {
	_ = s.write.Audit(ctx, core.AuditEntry{
		WorkspaceID: wsID, ActorID: actor.AgentID, ActorName: actor.Name,
		Action: action, Target: target, Detail: detail,
	})
}

func (s *Queries) AuditLog(ctx context.Context, actor Actor, limit int) ([]core.AuditEntry, error) {
	if err := require(ctx, actor, PermWorkspaceManage); err != nil {
		return nil, err
	}
	entries, err := s.read.AuditLog(ctx, actor.WorkspaceID, limit)
	return entries, translate(err)
}

// ── 내보내기 ───────────────────────────────────────────────────────

// Export 는 워크스페이스의 모든 대화와 메시지(내부 메모 포함)를 쓴다. 워크스페이스 관리 권한으로, 자기 워크스페이스만.
// 가린 상담도 남긴다 — deleted_at 과 함께. 내보내기는 기록이고, 가리기는 화면에서 치우는 일이다.
// format 은 json 또는 csv. 전체를 메모리에 올리지 않고 대화 단위로 흘린다.
func (s *Commands) Export(ctx context.Context, actor Actor, format string, w io.Writer) error {
	if err := require(ctx, actor, PermWorkspaceManage); err != nil {
		return err
	}
	switch format {
	case "json":
		return s.exportJSON(ctx, actor, w)
	case "csv":
		return s.exportCSV(ctx, actor, w)
	default:
		return invalid("bad_format", "format 은 json 또는 csv 여야 합니다")
	}
}

func (s *Queries) eachMessage(ctx context.Context, convID string, fn func(core.Message) error) error {
	var after int64
	for {
		msgs, err := s.read.ListMessages(ctx, convID, after, 500, true)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if err := fn(m); err != nil {
				return err
			}
			after = m.Seq
		}
		if len(msgs) < 500 {
			return nil
		}
	}
}

func (s *Commands) exportJSON(ctx context.Context, actor Actor, w io.Writer) error {
	ws, err := s.read.Workspace(ctx, actor.WorkspaceID)
	if err != nil {
		return translate(err)
	}
	head, _ := json.Marshal(map[string]any{"workspace": ws.Slug, "exported_at": time.Now().UTC()})
	if _, err := fmt.Fprintf(w, `{"meta":%s,"conversations":[`, head); err != nil {
		return err
	}
	first := true
	err = s.read.EachConversation(ctx, ws.ID, func(conv core.Conversation) error {
		if !first {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		first = false
		msgs := make([]core.Message, 0, 32)
		if err := s.q.eachMessage(ctx, conv.ID, func(m core.Message) error { msgs = append(msgs, m); return nil }); err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(map[string]any{"conversation": conv, "messages": msgs})
	})
	if err != nil {
		return translate(err)
	}
	if _, err := io.WriteString(w, "]}"); err != nil {
		return err
	}
	s.audit(ctx, ws.ID, actor, "workspace.export", "json", nil)
	return nil
}

func (s *Commands) exportCSV(ctx context.Context, actor Actor, w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"conversation_id", "customer", "state", "seq", "created_at", "sender_kind", "sender", "visibility", "body", "attachment", "deleted_at"}); err != nil {
		return err
	}
	err := s.read.EachConversation(ctx, actor.WorkspaceID, func(conv core.Conversation) error {
		return s.q.eachMessage(ctx, conv.ID, func(m core.Message) error {
			attachment := ""
			if m.Attachment != nil {
				attachment = m.Attachment.Filename
			}
			deletedAt := ""
			if conv.DeletedAt != nil {
				deletedAt = conv.DeletedAt.UTC().Format(time.RFC3339)
			}
			return cw.Write([]string{conv.ID, csvSafe(conv.CustomerName), string(conv.State), fmt.Sprint(m.Seq),
				m.CreatedAt.UTC().Format(time.RFC3339), string(m.SenderKind), csvSafe(m.SenderName), string(m.Visibility), csvSafe(m.Body), csvSafe(attachment), deletedAt})
		})
	})
	if err != nil {
		return translate(err)
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}
	s.audit(ctx, actor.WorkspaceID, actor, "workspace.export", "csv", nil)
	return nil
}

// csvSafe 는 스프레드시트가 수식으로 읽는 첫 글자를 무력화한다. 고객이 쓴 "=HYPERLINK(...)" 가 관리자 엑셀에서 실행되면 안 된다.
func csvSafe(cell string) string {
	if cell == "" {
		return cell
	}
	switch cell[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + cell
	}
	return cell
}

// ── 고객 삭제 ──────────────────────────────────────────────────────

// DeleteCustomer 는 개인정보를 지우고 대화는 남긴다. 번호에 빈칸을 만들지 않는다.
// 익명화가 커밋된 뒤 그 고객의 첨부 파일을 파일 저장소에서 지운다. DB 저장소는 파일 저장소를 모르므로 지울 키만 돌려받는다.
// 파일 삭제는 요청이 끊겨도 끝까지 간다. 익명화는 이미 커밋됐고, 여기서 멈추면 지울 키를 다시 알 길이 없다.
// 대신 customerFileRemoval 안에 끝낸다. 이 시점에 쥔 DB 커넥션은 없다(D28: 커넥션을 쥔 채 저장소를 기다리지 않는다).
func (s *Commands) DeleteCustomer(ctx context.Context, actor Actor, customerID string) error {
	if err := require(ctx, actor, PermCustomerDelete); err != nil {
		return err
	}
	customerID, err := id.Normalize(customerID)
	if err != nil {
		return invalid("customer_id_format", "customer_id 형식이 올바르지 않습니다")
	}
	paths, err := s.write.AnonymizeCustomer(ctx, actor.WorkspaceID, customerID)
	if err != nil {
		return translate(err)
	}
	s.audit(ctx, actor.WorkspaceID, actor, "customer.delete", customerID, map[string]any{"attachments": len(paths)})
	removal, cancel := context.WithTimeout(context.WithoutCancel(ctx), customerFileRemoval)
	defer cancel()
	s.RemoveFiles(removal, paths)
	return nil
}

// customerFileRemoval 은 고객 한 명의 첨부 파일을 지우는 데 주는 시간이다. 요청 취소와 상관없이 이만큼은 지운다.
const customerFileRemoval = 30 * time.Second
