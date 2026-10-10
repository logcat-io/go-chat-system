// Package id 는 UUID v7(RFC 9562) 문자열을 만든다.
// 외부 의존을 늘리지 않으려고 직접 둔다. pgx 는 uuid 컬럼에 Go string 을 그대로 쓴다.
//
// 앞 48비트가 Unix 밀리초라 PK 인덱스 삽입이 끝으로 몰린다(D31). 그래서 앞부분은 랜덤이 아니다.
// 짧은 꼬리값이 필요하면 뒤쪽을 자른다. 마지막 12자리(48비트)는 늘 랜덤이다.
package id

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

var ErrInvalid = errors.New("id: 형식이 UUID 가 아니다")

// generator 는 마지막으로 쓴 밀리초와 12비트 카운터(rand_a)를 기억한다.
// 같은 밀리초 안이거나 시계가 뒤로 가면 마지막 밀리초를 그대로 쓰고 카운터를 올린다.
// 카운터가 넘치면 밀리초를 하나 앞당긴다(RFC 9562 6.2 Method 1). 그래서 한 프로세스 안에서는 늘 증가한다.
type generator struct {
	mu     sync.Mutex
	now    func() time.Time
	lastMS int64
	seq    uint16 // 아래 12비트만 쓴다
}

var std = &generator{now: time.Now}

// New 는 RFC 9562 v7 UUID 를 소문자 표준 형식으로 돌려준다.
// 같은 프로세스에서 만든 값은 문자열로 비교해도 단조 증가한다.
func New() string { return std.next() }

func (g *generator) next() string {
	var r [10]byte
	if _, err := rand.Read(r[:]); err != nil {
		// crypto/rand 가 실패하면 토큰·식별자 보장이 깨진다. 계속 진행하지 않는다.
		panic("id: crypto/rand 실패: " + err.Error())
	}

	g.mu.Lock()
	ms := g.now().UnixMilli()
	if ms > g.lastMS {
		g.lastMS = ms
		// 새 밀리초의 카운터는 랜덤에서 시작하되 맨 윗비트를 비워 같은 밀리초 안에서 올릴 자리를 남긴다.
		g.seq = (uint16(r[0])<<8 | uint16(r[1])) & 0x07ff
	} else {
		g.seq++
		if g.seq > 0x0fff {
			g.lastMS++
			g.seq = 0
		}
	}
	ms, seq := g.lastMS, g.seq
	g.mu.Unlock()
	return layout(ms, seq, r)
}

// layout 은 밀리초·카운터·랜덤을 v7 바이트 자리에 놓는다.
// 밀리초는 큰 자리부터 아래 48비트, 카운터는 아래 12비트만 쓴다. 바이트로 자르는 일은 encoding/binary 가 하고 정수 변환은 한 곳뿐이다.
func layout(ms int64, seq uint16, r [10]byte) string {
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], uint64(ms)) //nolint:gosec // G115: ms 는 시계의 Unix 밀리초라 음수가 아니고, 아래 48비트만 쓴다.
	var c [2]byte
	binary.BigEndian.PutUint16(c[:], seq)
	var b [16]byte
	copy(b[0:6], t[2:8])
	b[6] = 0x70 | c[0]&0x0f // version 7 + 카운터 위 4비트
	b[7] = c[1]
	copy(b[8:], r[2:])
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return format(b)
}

func format(b [16]byte) string {
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}

// Valid 는 36자 표준 형식인지만 본다. 버전·variant 는 보지 않는다.
func Valid(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range []byte(s) {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHex(c) {
				return false
			}
		}
	}
	return true
}

// Normalize 는 대문자로 들어온 UUID 를 소문자로 맞춘다.
func Normalize(s string) (string, error) {
	low := strings.ToLower(strings.TrimSpace(s))
	if !Valid(low) {
		return "", ErrInvalid
	}
	return low, nil
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
