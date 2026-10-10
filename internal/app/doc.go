// Package app 은 유스케이스다. 입력 검증·권한 판정·공개 범위가 여기 있고, SQL 과 HTTP 를 모른다.
//
// 의존은 한 방향이다.
//
//	adapter (pgstore · httpapi · realtime · ollama · filestore)  →  app  →  core
//
// 세 가지를 지킨다.
//
//  1. 포트는 쓰는 쪽이 정의한다 (ports.go). 구현 패키지는 app 을 import 하지 않아도 메서드 집합으로 충족한다.
//  2. 명령과 조회를 타입으로 가른다. Queries 는 ReadStore 만 들고 있어서 조회 경로에 쓰기가 끼면 컴파일이 안 된다.
//     Commands 는 상태를 바꾸고, 판단에 필요한 읽기는 저장소에서 직접 하거나 Queries 의 권한 검사·조회를 쓴다.
//     복제나 읽기 전용 DB 를 뜻하지 않는다 — 같은 저장소를 두 얼굴로 볼 뿐이다.
//  3. 공개 범위는 여기서 정한다. 전달 계층이 늘어도 "고객에게 안 보인다" 를 다시 구현하지 않는다.
//
// 파일은 기능별로 묶는다(conversation·message·team·workspace…). 한 파일 안에 명령과 조회가 같이 있어도
// 수신자 타입(*Commands / *Queries)이 어느 쪽인지 말한다.
package app
