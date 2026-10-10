package httpapi

import (
	"net/http"

	"github.com/logcat-io/go-chat-system/internal/app"
)

// 상담 지표와 만족도 조사(D45). 만족도는 그 상담의 고객이 내고, 그 상담을 보는 고객·상담원이 읽는다. 지표는 work.view_all 이 본다.

type rateRequest struct {
	Score   int    `json:"score"`
	Comment string `json:"comment"`
}

// rating 은 상담 하나의 만족도 조사 상태다. 고객은 자기 상담만(I6), 상담원은 볼 수 있는 상담만.
func (a *API) rating(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	out, err := a.qry.Rating(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// rate 는 그 상담의 고객이 별점과 한 줄 의견을 낸다. 종료 뒤 24시간 안에는 다시 내서 고친다.
func (a *API) rate(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req rateRequest
	if !decodeOr(w, r, &req) {
		return
	}
	out, err := a.cmd.Rate(r.Context(), actor, r.PathValue("id"), app.RateParams{Score: req.Score, Comment: req.Comment})
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// metrics 는 기간의 상담 지표다. from/to 는 RFC3339, 비면 최근 7일이다.
func (a *API) metrics(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	q := r.URL.Query()
	from, to, ok := timeRange(w, q.Get("from"), q.Get("to"))
	if !ok {
		return
	}
	out, err := a.qry.Metrics(r.Context(), actor, from, to)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
