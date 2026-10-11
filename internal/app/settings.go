package app

import (
	"context"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 워크스페이스 설정. 운영시간·자동 메시지·위젯 모양.
// 저장은 JSON 한 덩어리지만 모양과 검증은 여기서 정한다.

const (
	settingHours    = "business_hours"
	settingMessages = "auto_messages"
	settingWidget   = "widget"
	settingSignup   = "agent_signup"
)

// HoursRange 는 "09:00"~"18:00" 처럼 하루 안의 한 구간이다.
type HoursRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// BusinessHours 는 요일별 운영 구간이다. 비어 있는 요일은 쉬는 날이다.
// 구간이 하나도 없으면 "항상 운영" 으로 본다. 설정을 안 한 설치가 밤마다 부재중 안내를 띄우면 안 된다.
type BusinessHours struct {
	Timezone string                  `json:"timezone"`
	Days     map[string][]HoursRange `json:"days"` // mon tue wed thu fri sat sun
}

// AutoMessages 는 사람 대신 나가는 문장들이다. 비워 두면 그 자동 메시지는 안 나간다.
type AutoMessages struct {
	Welcome    string `json:"welcome"`     // 새 대화를 열 때
	FirstReply string `json:"first_reply"` // 고객이 처음 말했고 아직 아무도 답하지 않았을 때
	Away       string `json:"away"`        // 운영시간 밖에 고객이 말했을 때
	// DraftStyle 은 AI 초안에 붙는 한 줄 지시다. 예: "존댓말, 환불 약속 금지, 세 문장 이내".
	DraftStyle string `json:"draft_style"`
}

// WidgetSettings 는 위젯 모양이다. 호스트 페이지의 임베드 스크립트가 받아 간다.
type WidgetSettings struct {
	Title    string `json:"title"`
	Greeting string `json:"greeting"`
	Color    string `json:"color"`
	Position string `json:"position"` // right | left
}

// Settings 는 설정을 한 번에 담는다. 관리 화면이 한 화면이라 한 번에 주고받는다.
type Settings struct {
	Hours    BusinessHours  `json:"business_hours"`
	Messages AutoMessages   `json:"auto_messages"`
	Widget   WidgetSettings `json:"widget"`
	// AgentSignup 은 상담원 직접 가입 입구를 여는지다. 기본은 연다.
	// 포인터인 까닭: 이 필드를 모르는 옛 화면이 설정을 저장하면서 입구를 말없이 닫지 않게 한다. 비어 오면 지금 값을 둔다.
	AgentSignup *bool `json:"agent_signup,omitempty"`
}

// signupOpen 은 저장된 값이 없으면 연 것으로 본다.
func (st Settings) signupOpen() bool { return st.AgentSignup == nil || *st.AgentSignup }

func defaultSettings() Settings {
	return Settings{
		Hours: BusinessHours{Timezone: "Asia/Seoul", Days: map[string][]HoursRange{}},
		Messages: AutoMessages{
			Welcome:    WelcomeBody,
			FirstReply: "문의가 접수되었습니다. 상담원이 확인하고 곧 답해 드리겠습니다.",
			Away:       "지금은 운영시간이 아닙니다. 남겨 주신 문의는 운영시간에 순서대로 답해 드립니다.",
			DraftStyle: "",
		},
		Widget: WidgetSettings{
			// Title 은 위젯 머리의 로고 옆 이름이다. 문장("무엇을 도와드릴까요?")을 두면 대화 머리에 질문이 이름처럼 뜬다(감사 18) — 인사 문장은 Greeting 이 맡는다.
			Title: "고객센터", Greeting: "궁금한 점을 남겨 주시면 상담원이 확인하고 답해 드립니다.",
			Color: "#c2410c", Position: "right", // 기본 관리자 색: 주황(흰 글자 5.18:1, D41)
		},
	}
}

var weekdayKeys = map[time.Weekday]string{
	time.Monday: "mon", time.Tuesday: "tue", time.Wednesday: "wed", time.Thursday: "thu",
	time.Friday: "fri", time.Saturday: "sat", time.Sunday: "sun",
}

// Open 은 그 시각이 운영시간 안인지 판정한다.
func (h BusinessHours) Open(at time.Time) bool {
	if h.empty() {
		return true
	}
	loc, err := time.LoadLocation(h.Timezone)
	if err != nil {
		loc = time.UTC
	}
	local := at.In(loc)
	minutes := local.Hour()*60 + local.Minute()
	for _, r := range h.Days[weekdayKeys[local.Weekday()]] {
		from, okFrom := parseClock(r.From)
		to, okTo := parseClock(r.To)
		if okFrom && okTo && from <= minutes && minutes < to {
			return true
		}
	}
	return false
}

func (h BusinessHours) empty() bool {
	for _, ranges := range h.Days {
		if len(ranges) > 0 {
			return false
		}
	}
	return true
}

func (h BusinessHours) validate() error {
	if _, err := time.LoadLocation(h.Timezone); err != nil {
		return invalid("timezone_invalid", "시간대가 올바르지 않습니다: %s", h.Timezone)
	}
	// 요일은 키 오름차순으로 본다. map 순서를 따르면 둘 이상 틀렸을 때 문구가 실행마다 달라진다.
	for _, day := range slices.Sorted(maps.Keys(h.Days)) {
		ranges := h.Days[day]
		if _, ok := map[string]bool{"mon": true, "tue": true, "wed": true, "thu": true, "fri": true, "sat": true, "sun": true}[day]; !ok {
			return invalid("weekday_invalid", "요일이 올바르지 않습니다: %s", day)
		}
		for _, r := range ranges {
			from, okFrom := parseClock(r.From)
			to, okTo := parseClock(r.To)
			if !okFrom || !okTo {
				return invalid("hours_format", "시각은 HH:MM 이어야 합니다: %s~%s", r.From, r.To)
			}
			if from >= to {
				return invalid("hours_order", "시작이 끝보다 앞서야 합니다: %s~%s", r.From, r.To)
			}
		}
	}
	return nil
}

var clockPattern = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)

// parseClock 은 "HH:MM" 을 자정 기준 분으로 바꾼다. 앞뒤에 다른 글자가 붙으면 거절한다.
// Sscanf 는 꼬리를 검사하지 않아 "09:00<img …>" 가 통과했고 화면에 그대로 꽂혔다.
func parseClock(v string) (int, bool) {
	m0 := clockPattern.FindStringSubmatch(v)
	if m0 == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(m0[1])
	m, _ := strconv.Atoi(m0[2])
	if h < 0 || h > 24 || m < 0 || m > 59 || (h == 24 && m != 0) {
		return 0, false
	}
	return h*60 + m, true
}

func (m AutoMessages) validate() error {
	// 필드 선언 순서로 본다. 둘 이상 넘쳐도 문구가 매번 같다. 사유 코드는 필드마다 리터럴이다(감사 24).
	tooLong := func(v string) bool { return utf8.RuneCountInString(v) > 1000 }
	switch {
	case tooLong(m.Welcome):
		return invalid("welcome_too_long", "환영 메시지는 1000자까지입니다")
	case tooLong(m.FirstReply):
		return invalid("first_reply_too_long", "접수 안내는 1000자까지입니다")
	case tooLong(m.Away):
		return invalid("away_too_long", "부재중 안내는 1000자까지입니다")
	case tooLong(m.DraftStyle):
		return invalid("draft_style_too_long", "초안 지시는 1000자까지입니다")
	}
	return nil
}

func (w WidgetSettings) validate() error {
	if utf8.RuneCountInString(w.Title) > 40 || utf8.RuneCountInString(w.Greeting) > 200 {
		return invalid("widget_text_too_long", "위젯 제목은 40자, 인사말은 200자까지입니다")
	}
	if !isHexColor(w.Color) {
		return invalid("color_format", "색은 #rrggbb 형식이어야 합니다")
	}
	if w.Position != "right" && w.Position != "left" {
		return invalid("position_invalid", "위치는 right 또는 left 여야 합니다")
	}
	return nil
}

func isHexColor(v string) bool {
	if len(v) != 7 || v[0] != '#' {
		return false
	}
	for _, c := range strings.ToLower(v[1:]) {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// settings 는 저장된 값에 기본값을 겹쳐 돌려준다. 하나도 저장 안 한 설치도 모양이 같다.
func (s *Queries) settings(ctx context.Context, wsID string) (Settings, error) {
	out := defaultSettings()
	for key, target := range map[string]any{
		settingHours: &out.Hours, settingMessages: &out.Messages, settingWidget: &out.Widget,
	} {
		err := s.read.Setting(ctx, wsID, key, target)
		if err != nil && !errors.Is(err, core.ErrSettingMissing) {
			return Settings{}, err
		}
	}
	if out.Hours.Days == nil {
		out.Hours.Days = map[string][]HoursRange{}
	}
	open := true
	if err := s.read.Setting(ctx, wsID, settingSignup, &open); err != nil && !errors.Is(err, core.ErrSettingMissing) {
		return Settings{}, err
	}
	out.AgentSignup = &open
	return out, nil
}

// AdminSettings 는 관리자 설정 화면이 보는 설정이다. 워크스페이스 관리 권한이 본다.
func (s *Queries) AdminSettings(ctx context.Context, actor Actor) (Settings, error) {
	if err := require(ctx, actor, PermWorkspaceManage); err != nil {
		return Settings{}, err
	}
	return s.settings(ctx, actor.WorkspaceID)
}

// UpdateSettings 는 워크스페이스 관리 권한이 한다. 다 검증한 뒤에 저장하므로 반쯤 저장되는 일이 없다.
func (s *Commands) UpdateSettings(ctx context.Context, actor Actor, in Settings) (Settings, error) {
	if err := require(ctx, actor, PermWorkspaceManage); err != nil {
		return Settings{}, err
	}
	in.Hours.Timezone = strings.TrimSpace(in.Hours.Timezone)
	if in.Hours.Timezone == "" {
		in.Hours.Timezone = "Asia/Seoul"
	}
	if in.Hours.Days == nil {
		in.Hours.Days = map[string][]HoursRange{}
	}
	in.Widget.Color = strings.ToLower(strings.TrimSpace(in.Widget.Color))
	// 위젯 이름은 앞뒤 공백을 떼고 저장한다. 공백뿐이면 빈 이름이고 위젯은 기본 이름(고객센터)을 쓴다(정책 670).
	// 떼지 않으면 공백 이름이 그대로 저장돼 위젯 머리가 빈 칸으로 보였다(감사 19).
	in.Widget.Title = strings.TrimSpace(in.Widget.Title)
	for _, check := range []func() error{in.Hours.validate, in.Messages.validate, in.Widget.validate} {
		if err := check(); err != nil {
			return Settings{}, err
		}
	}
	values := map[string]any{settingHours: in.Hours, settingMessages: in.Messages, settingWidget: in.Widget}
	if in.AgentSignup != nil {
		values[settingSignup] = *in.AgentSignup
	}
	err := s.write.PutSettings(ctx, actor.WorkspaceID, values, actor.AgentID)
	if err != nil {
		return Settings{}, translate(err)
	}
	s.audit(ctx, actor.WorkspaceID, actor, "settings.update", "", nil)
	return s.q.settings(ctx, actor.WorkspaceID)
}

// PublicWidgetSettings 는 토큰 없이도 볼 수 있는 부분이다. 임베드 스크립트가 부트 때 받아 간다.
type PublicWidgetSettings struct {
	WidgetSettings
	OpenNow bool `json:"open_now"`
	// FAQ 는 위젯 홈의 자주 묻는 질문이다(D36). 저장한 적이 없으면 기본 세 줄, 빈 목록을 저장했으면 빈 배열이다.
	FAQ []core.FAQItem `json:"faq"`
}

func (s *Queries) PublicWidgetSettings(ctx context.Context, wsID string, now time.Time) (PublicWidgetSettings, error) {
	st, err := s.settings(ctx, wsID)
	if err != nil {
		return PublicWidgetSettings{}, err
	}
	faq, err := s.widgetFAQ(ctx, wsID)
	if err != nil {
		return PublicWidgetSettings{}, err
	}
	return PublicWidgetSettings{WidgetSettings: st.Widget, OpenNow: st.Hours.Open(now), FAQ: faq.Items}, nil
}
