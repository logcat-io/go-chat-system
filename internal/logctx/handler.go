package logctx

import (
	"context"
	"log/slog"
)

// handler 는 다른 slog.Handler 를 감싸 ctx 의 키를 레코드 맨 위에 붙인다.
//
// 그룹을 안쪽 핸들러에 바로 넘기지 않고 직접 들고 있는 이유: 안쪽에 그룹을 열면 그 뒤에 붙이는 속성은 전부 그룹 안으로
// 들어간다. 그러면 logger.WithGroup("x") 를 쓴 줄에서만 conversation_id 가 x.conversation_id 가 되고,
// 키 하나로 거르는 조회가 그 줄을 놓친다. 그래서 그룹이 열리기 전의 속성만 안쪽에 넘기고,
// 그룹과 그 뒤의 속성은 Handle 에서 레코드를 다시 짤 때 감싼다.
type handler struct {
	inner slog.Handler
	// top 은 그룹이 열리기 전에 WithAttrs 로 붙은 키다. ctx 의 같은 키를 다시 내지 않으려고 기억한다.
	top map[string]struct{}
	// groups 는 WithGroup 으로 열린 그룹과 그 안에 붙은 속성이다. 바깥부터 차례로.
	groups []group
}

type group struct {
	name  string
	attrs []slog.Attr
}

// NewHandler 는 inner 를 감싼다. 서버는 이걸 기본 로거로 쓴다.
func NewHandler(inner slog.Handler) slog.Handler {
	return &handler{inner: inner}
}

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	ctxAttrs := attrs(ctx)
	if len(ctxAttrs) == 0 && len(h.groups) == 0 {
		return h.inner.Handle(ctx, r)
	}

	// 호출부가 직접 적은 키는 ctx 보다 우선한다. 호출부가 더 가까이에서 본 값이다 —
	// 예를 들어 연결이 구독한 대화와 다른 대화에 쓴 프레임이면 ctx 의 conversation_id 는 틀린 값이다.
	recordKeys := map[string]struct{}{}
	if len(h.groups) == 0 {
		r.Attrs(func(a slog.Attr) bool {
			recordKeys[a.Key] = struct{}{}
			return true
		})
	}

	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	for _, a := range ctxAttrs {
		if _, ok := h.top[a.Key]; ok {
			continue
		}
		if _, ok := recordKeys[a.Key]; ok {
			continue
		}
		out.AddAttrs(a)
	}

	if len(h.groups) == 0 {
		r.Attrs(func(a slog.Attr) bool {
			out.AddAttrs(a)
			return true
		})
		return h.inner.Handle(ctx, out)
	}

	// 가장 안쪽 그룹에 레코드의 속성을 넣고 바깥으로 감싸 나온다.
	var own []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		own = append(own, a)
		return true
	})
	nested := own
	for i := len(h.groups) - 1; i >= 0; i-- {
		g := h.groups[i]
		members := make([]any, 0, len(g.attrs)+len(nested))
		for _, a := range g.attrs {
			members = append(members, a)
		}
		for _, a := range nested {
			members = append(members, a)
		}
		nested = []slog.Attr{slog.Group(g.name, members...)}
	}
	out.AddAttrs(nested...)
	return h.inner.Handle(ctx, out)
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	if len(h.groups) > 0 {
		groups := make([]group, len(h.groups))
		copy(groups, h.groups)
		last := &groups[len(groups)-1]
		last.attrs = append(append([]slog.Attr(nil), last.attrs...), as...)
		return &handler{inner: h.inner, top: h.top, groups: groups}
	}
	top := make(map[string]struct{}, len(h.top)+len(as))
	for k := range h.top {
		top[k] = struct{}{}
	}
	for _, a := range as {
		top[a.Key] = struct{}{}
	}
	return &handler{inner: h.inner.WithAttrs(as), top: top}
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	groups := make([]group, len(h.groups), len(h.groups)+1)
	copy(groups, h.groups)
	groups = append(groups, group{name: name})
	return &handler{inner: h.inner, top: h.top, groups: groups}
}
