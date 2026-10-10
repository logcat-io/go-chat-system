package app

import (
	"context"
	"time"
)

// DBClock 은 DB 시계다. 기간의 끝을 정하지 않은 조회(업무 집계·상담 지표)의 위쪽 끝을 이 시계로 잡는다(DEF-148).
//
// 세는 기록(업무 기록·종료·응답)의 시각은 DB 가 적는다(clock_timestamp()). 끝을 앱 시계(time.Now)로 잡으면 두 시계의 차이만큼
// 방금 남긴 기록이 창 밖이 된다 — DB 시계가 앞선 개발 환경(Docker Desktop)에서 방금 닫은 상담이 집계에 없었다.
// 안전망의 시작점을 DB 시계로 잡는 것(D4)과 같은 까닭이다. 저장소가 이 능력을 가지면 New 가 꽂고, 없으면 앱 시계다.
type DBClock interface {
	Now(ctx context.Context) (time.Time, error)
}

// appClock 은 DB 시계를 모르는 저장소(가짜 저장소)에서 쓰는 앱 시계다.
type appClock struct{}

func (appClock) Now(context.Context) (time.Time, error) { return time.Now(), nil }

// windowEnd 는 기간의 위쪽 끝이다. 준 값이 있으면 그대로, 없으면 DB 시계의 지금이다.
func (s *Queries) windowEnd(ctx context.Context, to time.Time) (time.Time, error) {
	if !to.IsZero() {
		return to, nil
	}
	now, err := s.clock.Now(ctx)
	if err != nil {
		return time.Time{}, translate(err)
	}
	return now, nil
}
