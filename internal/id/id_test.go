package id

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// 시계를 주입하려고 내부 테스트로 둔다. 바깥에서 쓰는 New·Valid·Normalize 만 단언한다.

func TestNew(t *testing.T) {
	t.Run("id 는 표준 형식이고 버전 니블이 7 이고 variant 가 10 이다", func(t *testing.T) {
		for i := 0; i < 1000; i++ {
			v := New()
			if len(v) != 36 || !Valid(v) {
				t.Fatalf("형식이 틀렸다: %q", v)
			}
			if v[14] != '7' {
				t.Fatalf("version 7 이어야 한다: %q", v)
			}
			if c := v[19]; c != '8' && c != '9' && c != 'a' && c != 'b' {
				t.Fatalf("variant 가 10 이 아니다: %q", v)
			}
		}
	})

	t.Run("id 의 앞 48비트는 만든 시각의 Unix 밀리초다", func(t *testing.T) {
		at := time.UnixMilli(0x0192_3456_789a)
		g := &generator{now: func() time.Time { return at }}
		if v := g.next(); !strings.HasPrefix(v, "01923456-789a-7") {
			t.Errorf("앞부분이 시각이어야 한다: %q", v)
		}
	})

	t.Run("같은 밀리초에 연달아 만든 id 십만 개가 문자열로 단조 증가한다", func(t *testing.T) {
		at := time.UnixMilli(1_760_000_000_000)
		g := &generator{now: func() time.Time { return at }}
		prev := g.next()
		for i := 0; i < 100_000; i++ {
			v := g.next()
			if v <= prev {
				t.Fatalf("%d 번째에서 줄었다: %s 다음 %s", i, prev, v)
			}
			if v[14] != '7' {
				t.Fatalf("카운터가 넘쳐도 버전 니블은 7 이다: %q", v)
			}
			prev = v
		}
	})

	t.Run("시계가 뒤로 가도 id 는 감소하지 않는다", func(t *testing.T) {
		now := time.UnixMilli(1_760_000_000_000)
		g := &generator{now: func() time.Time { return now }}
		before := g.next()
		now = now.Add(-10 * time.Second)
		after := g.next()
		if after <= before {
			t.Fatalf("시계가 뒤로 간 뒤에 줄었다: %s 다음 %s", before, after)
		}
		if after[:13] != before[:13] {
			t.Errorf("뒤로 간 시각 대신 마지막 밀리초를 그대로 써야 한다: %s 다음 %s", before, after)
		}
	})

	t.Run("새 밀리초의 카운터는 랜덤에서 시작하고 맨 윗비트가 비어 있다", func(t *testing.T) {
		// 같은 밀리초 안에서 올릴 자리(2048개 이상)를 남긴다. 윗비트가 차 있으면 몇 번 만에 넘쳐 밀리초를 앞당긴다.
		now := time.UnixMilli(1_760_000_000_000)
		g := &generator{now: func() time.Time { return now }}
		starts := map[string]bool{}
		for i := 0; i < 10000; i++ {
			now = now.Add(time.Millisecond)
			v := g.next()
			// 버전 니블(v[14]) 다음 세 자리가 12비트 카운터다. 맨 윗비트가 비면 첫 자리가 0~7 이다.
			if c := v[15]; c < '0' || c > '7' {
				t.Fatalf("새 밀리초의 카운터 맨 윗비트가 차 있다: %s", v)
			}
			starts[v[15:18]] = true
		}
		if len(starts) < 1000 {
			t.Errorf("새 밀리초의 카운터 시작값이 %d 가지뿐이다. 랜덤이어야 한다", len(starts))
		}
	})

	t.Run("고루틴 여러 개가 동시에 만든 id 는 겹치지 않는다", func(t *testing.T) {
		const workers, each = 16, 5000
		out := make([][]string, workers)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				vs := make([]string, each)
				for i := range vs {
					vs[i] = New()
				}
				out[w] = vs
			}(w)
		}
		wg.Wait()
		seen := make(map[string]bool, workers*each)
		for _, vs := range out {
			for i, v := range vs {
				if seen[v] {
					t.Fatalf("중복: %s", v)
				}
				seen[v] = true
				if i > 0 && v <= vs[i-1] {
					t.Fatalf("한 고루틴 안에서 줄었다: %s 다음 %s", vs[i-1], v)
				}
			}
		}
	})

	t.Run("같은 밀리초에 만든 id 둘도 마지막 12자리는 서로 다르다", func(t *testing.T) {
		// 짧은 꼬리값(슬러그 접미사·워크스페이스 슬러그·테스트 DB 이름)은 이 뒷부분을 자른다.
		at := time.UnixMilli(1_760_000_000_000)
		g := &generator{now: func() time.Time { return at }}
		seen := make(map[string]bool, 10000)
		for i := 0; i < 10000; i++ {
			tail := g.next()[24:]
			if seen[tail] {
				t.Fatalf("꼬리 중복: %s", tail)
			}
			seen[tail] = true
		}
	})
}

func TestValid(t *testing.T) {
	cases := []struct {
		name, in string
		want     bool
	}{
		{"정상", "550e8400-e29b-41d4-a716-446655440000", true},
		{"v7 도 받는다", "01923456-789a-7abc-8def-0123456789ab", true},
		{"대문자도 받는다", "550E8400-E29B-41D4-A716-446655440000", true},
		{"길이가 짧다", "550e8400-e29b-41d4-a716-44665544000", false},
		{"하이픈 위치가 틀렸다", "550e84-00e29b-41d4-a716-446655440000", false},
		{"16진수가 아니다", "550e8400-e29b-41d4-a716-44665544000z", false},
		{"빈 문자열", "", false},
		{"SQL 조각", "' OR 1=1 --                         ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Valid(tc.in); got != tc.want {
				t.Errorf("%v 여야 한다: %q", tc.want, tc.in)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	t.Run("대문자와 공백을 정규화한다", func(t *testing.T) {
		got, err := Normalize("  550E8400-E29B-41D4-A716-446655440000 ")
		if err != nil {
			t.Fatal(err)
		}
		if got != "550e8400-e29b-41d4-a716-446655440000" {
			t.Errorf("소문자로 맞춰야 한다: %q", got)
		}
	})
	t.Run("형식이 아니면 오류다", func(t *testing.T) {
		if _, err := Normalize("nope"); err == nil {
			t.Error("오류여야 한다")
		}
	})
}
