package pgstore

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 트랜잭션 자문 잠금. pg_advisory_xact_lock(키, hashtext(대상 id 의 text)) 의 두 정수 형태로 잡고, 트랜잭션이 끝나면 풀린다.
// 키는 "무엇을 지키는 줄인가" 다. 같은 불변식을 지키는 경로는 같은 키와 같은 대상 문자열로 잡아야 같은 줄에 선다.
// 대상 문자열은 uuid 의 text 표현(소문자, 하이픈)이다. 컬럼에서 꺼낸 uuid::text 와 앱이 넘긴 id 가 같은 문자열이어야 같은 줄이다.
//
// 행 잠금 대신 자문 잠금을 쓰는 이유: 고객·대화 행을 잠그면 메시지 저장의 외래 키 확인과 엇갈려 교착이 난다.
// 자문 잠금은 다른 잠금과 순서가 얽히지 않는다. 키 값은 바꾸지 않는다 — 배포가 겹치는 동안 옛 키와 새 키가 서로를 못 본다.
const (
	// lockKeyWorkspaceStaff 는 워크스페이스의 상담원 구성을 "세고 나서 바꾸는" 줄이다. 대상은 워크스페이스 id.
	// 마지막 관리자 보호(강등·비활성화), 상담원 구성 변경(재활성화·초대 수락·승인), 가입 대기 상한이 같은 줄에 선다.
	// 세기를 트랜잭션 밖에서 하면 두 관리자가 서로를 동시에 강등해 0명이 되고, 동시 가입이 대기 상한을 넘는다.
	lockKeyWorkspaceStaff = 7301
	// lockKeyCustomer 는 회원의 열린 상담 판정 줄이다. 대상은 고객 id.
	// 상담 열기(OpenMemberConversation), 닫힌 상담 다시 열기(guardReopen), 상담 가리기·되살리기(setDeleted)가 같은 줄에 선다.
	// 줄이 갈리면 동시 요청이 각자 "열린 상담이 없다" 를 보고 방을 둘 만들거나, 가려질 방으로 새 문의가 들어간다.
	lockKeyCustomer = 7302
	// lockKeyPendingAttachments 는 대화의 미전송 첨부 상한을 "세고 나서 넣는" 줄이다. 대상은 대화 id.
	lockKeyPendingAttachments = 7303
)

// lockWorkspaceStaff 는 워크스페이스의 상담원 구성 줄에 선다.
func lockWorkspaceStaff(ctx context.Context, tx pgx.Tx, wsID string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, hashtext($2))", lockKeyWorkspaceStaff, wsID)
	return err
}

// lockCustomer 는 고객 id 로 회원의 열린 상담 판정 줄에 선다.
func lockCustomer(ctx context.Context, tx pgx.Tx, customerID string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, hashtext($2::text))", lockKeyCustomer, customerID)
	return err
}

// lockCustomerOfConversation 은 대화의 고객으로 lockCustomer 와 같은 줄에 선다. 고객은 대화 행에서 찾는다.
// 대화가 없으면 잠그지 않고 오류도 없다. 없는 대화는 뒤따르는 쿼리가 답한다.
func lockCustomerOfConversation(ctx context.Context, tx pgx.Tx, convID string) error {
	_, err := tx.Exec(ctx,
		"SELECT pg_advisory_xact_lock($1, hashtext(customer_id::text)) FROM conversations WHERE id = $2", lockKeyCustomer, convID)
	return err
}

// lockPendingAttachments 는 대화의 미전송 첨부 상한 줄에 선다.
func lockPendingAttachments(ctx context.Context, tx pgx.Tx, convID string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, hashtext($2::text))", lockKeyPendingAttachments, convID)
	return err
}
