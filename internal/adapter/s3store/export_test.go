package s3store

// SetContainerHost 는 RELATIVE_URI 를 붙일 ECS 에이전트 주소를 테스트의 가짜 엔드포인트로 바꾼다. 되돌리는 함수를 돌려준다.
func SetContainerHost(host string) (restore func()) {
	prev := containerHost
	containerHost = host
	return func() { containerHost = prev }
}
