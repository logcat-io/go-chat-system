package app

import (
	"testing"
	"time"
)

// 운영시간 판정은 순수 함수다. DB 없이 경계를 전부 찍는다.
func TestBusinessHours_Open(t *testing.T) {
	seoul := func(y, mo, d, h, mi int) time.Time {
		loc, _ := time.LoadLocation("Asia/Seoul")
		return time.Date(y, time.Month(mo), d, h, mi, 0, 0, loc)
	}
	weekday := BusinessHours{Timezone: "Asia/Seoul", Days: map[string][]HoursRange{
		"mon": {{From: "09:00", To: "18:00"}}, "tue": {{From: "09:00", To: "18:00"}},
		"wed": {{From: "09:00", To: "12:00"}, {From: "13:00", To: "18:00"}},
	}}

	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"월요일 운영시간 안", seoul(2026, 10, 5, 10, 0), true},
		{"월요일 시작 시각은 안이다", seoul(2026, 10, 5, 9, 0), true},
		{"월요일 끝 시각은 밖이다", seoul(2026, 10, 5, 18, 0), false},
		{"월요일 새벽", seoul(2026, 10, 5, 3, 0), false},
		{"수요일 점심 구간 사이는 밖이다", seoul(2026, 10, 7, 12, 30), false},
		{"수요일 오후 구간", seoul(2026, 10, 7, 15, 0), true},
		{"쉬는 날(일요일)", seoul(2026, 10, 11, 10, 0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := weekday.Open(tc.at); got != tc.want {
				t.Errorf("%v 에 Open=%v 여야 한다", tc.at, tc.want)
			}
		})
	}

	t.Run("구간이 하나도 없으면 항상 운영이다", func(t *testing.T) {
		none := BusinessHours{Timezone: "Asia/Seoul", Days: map[string][]HoursRange{}}
		if !none.Open(seoul(2026, 10, 11, 3, 0)) {
			t.Error("설정을 안 한 설치가 밤마다 부재중 안내를 띄우면 안 된다")
		}
	})

	t.Run("시간대를 따라 요일이 바뀐다", func(t *testing.T) {
		// UTC 로 월요일 23:30 은 서울로는 화요일 08:30 — 아직 열기 전이다.
		utc := time.Date(2026, 10, 5, 23, 30, 0, 0, time.UTC)
		if weekday.Open(utc) {
			t.Error("서울 시각으로 판정해야 한다")
		}
	})
}

func TestBusinessHours_validate(t *testing.T) {
	cases := []struct {
		name  string
		hours BusinessHours
		ok    bool
	}{
		{"정상", BusinessHours{Timezone: "Asia/Seoul", Days: map[string][]HoursRange{"mon": {{From: "09:00", To: "18:00"}}}}, true},
		{"없는 시간대", BusinessHours{Timezone: "Mars/Olympus", Days: map[string][]HoursRange{}}, false},
		{"없는 요일", BusinessHours{Timezone: "Asia/Seoul", Days: map[string][]HoursRange{"funday": {}}}, false},
		{"시각 형식", BusinessHours{Timezone: "Asia/Seoul", Days: map[string][]HoursRange{"mon": {{From: "9am", To: "18:00"}}}}, false},
		{"시각 뒤에 다른 글자", BusinessHours{Timezone: "Asia/Seoul", Days: map[string][]HoursRange{"mon": {{From: `09:00"><img src=x onerror=alert(1)>`, To: "18:00"}}}}, false},
		{"분이 한 자리", BusinessHours{Timezone: "Asia/Seoul", Days: map[string][]HoursRange{"mon": {{From: "9:5", To: "18:00"}}}}, false},
		{"시가 한 자리", BusinessHours{Timezone: "Asia/Seoul", Days: map[string][]HoursRange{"mon": {{From: "9:05", To: "18:00"}}}}, true},
		{"시작이 끝보다 늦음", BusinessHours{Timezone: "Asia/Seoul", Days: map[string][]HoursRange{"mon": {{From: "18:00", To: "09:00"}}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.hours.validate(); (err == nil) != tc.ok {
				t.Errorf("ok=%v 여야 한다: %v", tc.ok, err)
			}
		})
	}
}

func TestWidgetSettings_validate(t *testing.T) {
	good := WidgetSettings{Title: "상담", Greeting: "안녕하세요", Color: "#0c64e8", Position: "right"}
	if err := good.validate(); err != nil {
		t.Fatalf("통과해야 한다: %v", err)
	}
	bad := good
	bad.Color = "blue"
	if err := bad.validate(); err == nil {
		t.Error("색 형식을 막아야 한다")
	}
	bad = good
	bad.Position = "center"
	if err := bad.validate(); err == nil {
		t.Error("위치는 둘 중 하나다")
	}
}

// 위젯 머리 이름의 기본값은 이름이다. 저장한 적 없는 설치도 머리에 질문 문장이 아니라 "고객센터" 가 뜬다. 인사 문장은 인사말(greeting)이 따로 갖는다.
func TestDefaultWidgetSettings(t *testing.T) {
	t.Run("위젯 머리 이름의 기본값은 고객센터이고 인사 문장은 인사말이 따로 갖는다", func(t *testing.T) {
		w := defaultSettings().Widget
		if w.Title != "고객센터" {
			t.Errorf("기본 이름: %q", w.Title)
		}
		if w.Greeting == "" || w.Greeting == w.Title {
			t.Errorf("기본 인사말: %q", w.Greeting)
		}
	})
	t.Run("기본 위젯 색은 갈색 #6e6047 이다", func(t *testing.T) {
		if c := defaultSettings().Widget.Color; c != "#6e6047" {
			t.Errorf("기본 위젯 색: %q", c)
		}
	})
}
