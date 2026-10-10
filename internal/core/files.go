package core

import (
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// 브라우저가 저장소로 올리는 방식이다(D42). 어느 쪽인지는 어댑터가 정하고 브라우저는 UploadForm.Method 를 보고 따른다.
const (
	// UploadPost 는 multipart 양식 업로드다. Fields 를 그대로 싣고 마지막에 file 을 붙여 POST 한다(S3 POST policy, 로컬 저장소).
	UploadPost = "POST"
	// UploadPut 은 본문이 파일 그 자체인 업로드다. Headers 를 그대로 싣고 PUT 한다(서명된 PUT 주소. POST 양식이 없는 S3 호환 저장소).
	UploadPut = "PUT"
)

// UploadForm 은 브라우저가 저장소로 파일을 직접 올릴 때 쓰는 방식·주소와, 함께 실을 양식 필드 또는 헤더다.
// 서버는 파일 바이트를 받지 않는다.
//
// POST 면 Fields 를 싣고 마지막에 file 을 붙인다. PUT 이면 파일을 본문으로 보내고 Headers 를 그대로 싣는다.
// Headers 에는 Content-Length 를 넣지 않는다 — 브라우저가 본문으로 정하는 헤더라 스크립트가 실을 수 없다.
type UploadForm struct {
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Fields    map[string]string `json:"fields,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// ObjectInfo 는 저장소에 실제로 올라온 것이다. Head 는 앞부분 바이트로, 종류와 이미지 크기를 판정하는 데 쓴다.
type ObjectInfo struct {
	Size int64
	Head []byte
}

// ObjectHeadBytes 는 판정에 읽는 앞부분의 크기다. PNG·GIF 는 수십 바이트면 되고, JPEG 는 메타데이터가 길면 더 뒤에 크기가 있다.
const ObjectHeadBytes = 64 << 10

// maxFilenameBytes 는 저장하고 헤더에 싣는 이름의 상한이다.
const maxFilenameBytes = 200

// SafeFilename 은 내려받기 헤더에 쓸 이름을 다듬는다.
// 상한은 바이트지만 글자 경계에서 자른다. 바이트로 자르면 한글 이름이 깨진 UTF-8 이 되어 DB 가 거절한다.
func SafeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	// strings.Map 은 깨진 바이트를 U+FFFD 로 바꿔 쓴다. 여기서부터 이름은 올바른 UTF-8 이다.
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' || r == '/' {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	if len(name) > maxFilenameBytes {
		cut := maxFilenameBytes
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = name[:cut]
	}
	return name
}

// ContentDisposition 은 내려받기 응답의 Content-Disposition 값이다. 로컬 저장소와 S3 가 같은 값을 쓴다.
// filename= 자리에는 ASCII 만 넣고 원래 이름은 filename*= 로 보낸다. 헤더 값에 제어 문자나 ASCII 밖의 글자를 그대로 넣지 않는다.
func ContentDisposition(inline bool, filename string) string {
	disposition := "attachment"
	if inline {
		disposition = "inline"
	}
	name := SafeFilename(filename)
	return disposition + `; filename="` + asciiFilename(name) + `"; filename*=UTF-8''` + url.PathEscape(name)
}

// asciiFilename 은 filename= 자리에 넣을 이름이다. 제어 문자(0x7f 포함)와 ASCII 밖의 글자, 따옴표와 역슬래시를 _ 로 바꾼다.
func asciiFilename(name string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
}
