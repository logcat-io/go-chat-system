// Package web 은 정적 자산을 바이너리에 넣는다. 배포물은 실행 파일 하나다.
package web

import (
	"embed"
	"io/fs"
)

// 배포하는 종류만 고른다. 같은 디렉토리의 클라이언트 테스트(*.test.mjs)는 바이너리에도, 공개 서빙에도 들어가지 않는다.
// 새 종류를 더하면 여기에도 적는다 — 빠뜨리면 404 로 바로 드러난다.
// icons/ 는 위젯의 3D 아이콘(Fluent Emoji, MIT)과 그 라이선스 고지다(D38). fonts/ 는 제목 글꼴 Sora(OFL)와 그 고지다.
//
//go:embed assets/*.html assets/*.css assets/*.js assets/*.svg assets/icons/*.png assets/icons/LICENSE assets/fonts/*.woff2 assets/fonts/LICENSE
var assets embed.FS

// FS 는 assets/ 아래를 루트로 하는 파일시스템이다.
func FS() fs.FS {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic("web: 임베드 자산을 열 수 없다: " + err.Error())
	}
	return sub
}
