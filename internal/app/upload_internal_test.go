package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// 첨부 완료의 작업 시한은 임대보다 짧다. 시한이 임대를 넘으면 임대가 끝나 다른 요청이 이어받은 뒤에도 옛 요청이 일한다.
func TestUploadLimits_작업시한과임대(t *testing.T) {
	t.Run("임대는 30초, 작업 시한은 20초다", func(t *testing.T) {
		if attachmentLease != 30*time.Second {
			t.Errorf("임대: %s", attachmentLease)
		}
		if claimedWork != 20*time.Second {
			t.Errorf("작업 시한: %s", claimedWork)
		}
	})

	t.Run("작업 시한은 임대보다 짧다", func(t *testing.T) {
		if claimedWork >= attachmentLease {
			t.Errorf("작업 시한 %s 가 임대 %s 보다 짧지 않다", claimedWork, attachmentLease)
		}
	})

	t.Run("설정하지 않으면 정한 값을 쓴다", func(t *testing.T) {
		l := UploadLimits{}.normalized()
		if l.Lease != attachmentLease || l.Work != claimedWork {
			t.Errorf("기본값: 임대 %s, 작업 %s", l.Lease, l.Work)
		}
	})

	t.Run("작업 시한을 임대보다 길게 주면 임대보다 짧게 줄인다", func(t *testing.T) {
		l := UploadLimits{Lease: 3 * time.Second, Work: 5 * time.Second}.normalized()
		if l.Work >= l.Lease {
			t.Errorf("임대 %s, 작업 %s", l.Lease, l.Work)
		}
	})
}

// 작업 시한이 지난 뒤에 돌아온 실패라도 이미 뜻이 정해진 거절은 503 storage_timeout 으로 바꾸지 않는다.
// 바꾸면 "다시 올려라"(400)·"확인 중"(409)·"없다"(404)를 받아야 할 사용자가 저장소 탓의 503 을 받는다.
func TestStorageTimeout_정해진거절(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	t.Run("작업 시한이 지났어도 busy·invalid·not_found 는 그대로 통과시킨다", func(t *testing.T) {
		for _, err := range []error{ErrUploadBusy, invalid("file_missing", fileMissing), ErrNotFound} {
			if got := storageTimeout(expired, err); got != err { //nolint:errorlint // "그대로 통과" 는 같은 값이라는 뜻이다. errors.Is 는 감싼 것도 받아 느슨하다
				t.Errorf("%v → %v", err, got)
			}
		}
	})

	t.Run("작업 시한이 지난 뒤의 다른 실패는 storage_timeout 이다", func(t *testing.T) {
		if got := storageTimeout(expired, errors.New("저장소 오류")); !errors.Is(got, ErrStorageTimeout) {
			t.Errorf("시한이 지난 뒤의 실패: %v", got)
		}
	})
}

// removeFails 는 지우기만 실패하는 저장소다. 오류 문구에 키를 싣는다 — 실제 저장소 오류도 그럴 수 있다.
type removeFails struct{ FileStore }

func (removeFails) Remove(_ context.Context, key string) error {
	return errors.New("지우지 못했다: " + key)
}

func TestRemoveFiles_로그(t *testing.T) {
	t.Run("지우지 못한 파일의 로그에는 키와 저장소 오류 문구 없이 건수만 남는다", func(t *testing.T) {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
		t.Cleanup(func() { slog.SetDefault(prev) })

		keys := []string{"ws/w/c/a/lease-token-1", "ws/w/c/b/lease-token-2"}
		(&Commands{files: removeFails{}}).RemoveFiles(context.Background(), keys)
		out := buf.String()
		if !strings.Contains(out, `"failed":2`) {
			t.Errorf("실패 건수가 없다: %s", out)
		}
		if strings.Contains(out, "lease-token") || strings.Contains(out, "ws/w/c") {
			t.Errorf("로그에 키가 있다: %s", out)
		}
	})
}

// 첨부 종류는 앞부분 내용으로 판정한다. 영상은 ftyp 상자(mp4·mov·m4v)와 EBML 머리(webm)를 본다(D37).
// 표본은 실제 파일의 앞부분과 같은 바이트다. 판정은 앞부분 64KB 만 보므로 앞부분만 있으면 된다.
func TestInspect_영상(t *testing.T) {
	t.Run("영상은 내용으로 판정한다 — mp4(isom·mp42·avc1)·m4v 는 video/mp4, mov(qt) 는 video/quicktime, webm 은 video/webm", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			head []byte
			want string
		}{
			{"mp4 isom", ftyp("isom", "isom", "iso2", "avc1", "mp41"), "video/mp4"},
			{"mp4 isom 만", ftyp("isom", "isom"), "video/mp4"},
			{"mp4 mp42", ftyp("mp42", "mp42", "isom"), "video/mp4"},
			{"mp4 avc1", ftyp("avc1", "avc1"), "video/mp4"},
			{"m4v", ftyp("M4V ", "M4V ", "M4A ", "mp42", "isom"), "video/mp4"},
			{"mov(아이폰)", ftyp("qt  ", "qt  "), "video/quicktime"},
			{"webm", webmHead("webm"), "video/webm"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got, _, _ := inspect(tc.head, 0, 0); got != tc.want {
					t.Errorf("%s 여야 한다: %s", tc.want, got)
				}
				if !PlayableVideo(tc.want) {
					t.Errorf("%s 는 화면에서 재생하는 영상이어야 한다", tc.want)
				}
			})
		}
	})

	t.Run("영상처럼 생겼어도 재생하는 영상이 아니면 그렇게 판정하지 않는다 — m4a·HEIC·AVIF·CR3·mkv", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			head []byte
			want string
		}{
			{"m4a 음성", ftyp("M4A ", "M4A ", "mp42", "isom"), "audio/mp4"},
			{"HEIC 사진", ftyp("heic", "mif1", "heic"), "application/octet-stream"},
			{"AVIF 사진(호환 브랜드에 isom 이 있어도)", ftyp("avif", "avif", "mif1", "miaf", "isom"), "application/octet-stream"},
			{"CR3 사진(호환 브랜드에 isom)", ftyp("crx ", "crx ", "isom"), "application/octet-stream"},
			{"mkv", webmHead("matroska"), "video/x-matroska"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got, _, _ := inspect(tc.head, 0, 0)
				if got != tc.want {
					t.Errorf("%s 여야 한다: %s", tc.want, got)
				}
				if PlayableVideo(got) {
					t.Errorf("%s 를 화면에서 재생하는 영상으로 봤다", got)
				}
			})
		}
	})
}

// ftyp 는 ISO 미디어 파일의 첫 상자(ftyp)와 그 뒤 mdat 상자 머리다.
func ftyp(major string, compatible ...string) []byte {
	var b bytes.Buffer
	size := 16 + 4*len(compatible)
	b.Write([]byte{0, 0, 0, byte(size)})
	b.WriteString("ftyp" + major)
	b.Write([]byte{0, 0, 2, 0}) // 마이너 버전
	for _, c := range compatible {
		b.WriteString(c)
	}
	b.Write([]byte{0, 0, 0, 8})
	b.WriteString("mdat")
	b.Write(bytes.Repeat([]byte{0}, 64))
	return b.Bytes()
}

// webmHead 는 Matroska 계열의 EBML 머리다. docType 이 webm 이면 webm, matroska 면 mkv 다.
func webmHead(docType string) []byte {
	var body bytes.Buffer
	body.Write([]byte{0x42, 0x86, 0x81, 0x01}) // EBMLVersion 1
	body.Write([]byte{0x42, 0xF7, 0x81, 0x01}) // EBMLReadVersion 1
	body.Write([]byte{0x42, 0x82, 0x80 | byte(len(docType))})
	body.WriteString(docType)
	body.Write([]byte{0x42, 0x87, 0x81, 0x04}) // DocTypeVersion 4
	var b bytes.Buffer
	b.Write([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x80 | byte(body.Len())})
	b.Write(body.Bytes())
	b.Write([]byte{0x18, 0x53, 0x80, 0x67}) // Segment
	b.Write(bytes.Repeat([]byte{0}, 32))
	return b.Bytes()
}

func TestUploadLimits_기본한도(t *testing.T) {
	t.Run("한도를 정하지 않으면 첨부 한도는 100MB 다", func(t *testing.T) {
		if got := (UploadLimits{}).normalized().MaxBytes; got != 100<<20 {
			t.Errorf("기본 한도 %d, 100MB 여야 한다", got)
		}
	})
}
