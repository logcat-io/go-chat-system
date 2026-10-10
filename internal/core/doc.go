// Package core 는 도메인 타입과 규칙이다. DB·HTTP·WebSocket 을 모른다.
//
// 의존은 한 방향이다: adapter(pgstore·httpapi·realtime·ollama·filestore) → app → core.
// core 는 표준 라이브러리만 쓴다. 여기 있는 타입이 계층 사이를 오가는 말이다.
package core
