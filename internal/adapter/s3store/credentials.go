package s3store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 자격 증명은 두 곳에서 온다. 순서대로 본다.
//
//  1. 정적 키(CHAT_S3_ACCESS_KEY·CHAT_S3_SECRET_KEY). 로컬 MinIO 용이다. 만료가 없다.
//  2. 둘 다 비었으면 ECS 컨테이너 자격 증명 엔드포인트. 태스크 역할의 임시 자격 증명(키·비밀·세션 토큰·만료)을 준다.
//     AWS_CONTAINER_CREDENTIALS_RELATIVE_URI 면 http://169.254.170.2 + 그 경로,
//     아니면 AWS_CONTAINER_CREDENTIALS_FULL_URI 에 AWS_CONTAINER_AUTHORIZATION_TOKEN(_FILE) 을 Authorization 헤더로 싣는다.
//
// 임시 자격 증명은 만료 15분 전부터 갱신한다. 갱신은 한 번에 하나만 돌고, 그동안 다른 호출은 아직 유효한 옛 자격 증명을 쓴다.
// 갱신이 실패해도 옛 것이 유효하면 그대로 쓰고 WARN 사건 하나(s3.credentials_refresh_failed)를 남긴다. 다음 시도는 30초 뒤다.
// 로그에는 키·비밀·토큰·엔드포인트 주소를 싣지 않는다(D30). 이유 코드와 숫자만 싣는다.

const (
	// refreshBefore 는 만료 몇 분 전부터 새로 받나다. 서명한 주소·양식은 서명에 쓴 자격 증명이 만료되면 같이 죽는다.
	// 내려받기 주소(5분)와 올리기 양식(10분, app.uploadTTL)보다 길게 둬서, 발급한 주소·양식이 수명 안에서는 늘 쓸 수 있게 한다.
	refreshBefore = 15 * time.Minute
	// retryAfter 는 갱신이 실패한 뒤 다음 시도까지의 간격이다. 실패하는 엔드포인트를 요청마다 두드리지 않는다.
	retryAfter = 30 * time.Second
	// fetchTimeout 은 엔드포인트 한 번 호출의 시한이다. 같은 호스트의 링크 로컬 주소라 길 이유가 없다.
	fetchTimeout = 5 * time.Second
)

// containerHost 는 RELATIVE_URI 를 붙일 ECS 에이전트 주소다. 테스트만 바꾼다.
var containerHost = "http://169.254.170.2"

type credentials struct {
	AccessKey, SecretKey, SessionToken string
	// Expires 가 0 이면 만료가 없다(정적 키).
	Expires time.Time
}

func (c credentials) usable(now time.Time) bool {
	return c.AccessKey != "" && (c.Expires.IsZero() || now.Before(c.Expires))
}

func (c credentials) stale(now time.Time) bool {
	return !c.Expires.IsZero() && !now.Before(c.Expires.Add(-refreshBefore))
}

// credentialCache 는 자격 증명 하나를 쥐고 필요할 때 갱신한다. 동시 호출에 안전하다.
type credentialCache struct {
	kind  string // "static" 또는 "container". 기동 로그용
	fetch func(ctx context.Context) (credentials, error)
	now   func() time.Time
	log   func() *slog.Logger

	mu         sync.Mutex
	cur        credentials
	refreshing chan struct{} // 갱신이 도는 동안만 nil 이 아니다. 끝나면 닫힌다
	nextTry    time.Time
}

func staticCredentials(access, secret string) *credentialCache {
	return &credentialCache{kind: "static", cur: credentials{AccessKey: access, SecretKey: secret}, now: time.Now}
}

// get 은 지금 서명에 쓸 자격 증명이다. 쓸 수 있는 것이 없으면 오류다.
func (c *credentialCache) get(ctx context.Context) (credentials, error) {
	for {
		c.mu.Lock()
		now := c.now()
		cur := c.cur
		if cur.usable(now) && (!cur.stale(now) || c.refreshing != nil || now.Before(c.nextTry)) {
			c.mu.Unlock()
			return cur, nil
		}
		if wait := c.refreshing; wait != nil {
			// 쓸 수 있는 것이 없고 누가 받는 중이다. 그 결과를 기다린다.
			c.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return credentials{}, ctx.Err()
			}
		}
		done := make(chan struct{})
		c.refreshing = done
		c.mu.Unlock()

		fresh, err := c.fetch(ctx)

		c.mu.Lock()
		c.refreshing = nil
		close(done)
		now = c.now()
		if err == nil && !fresh.usable(now) {
			err = &fetchError{reason: "expired"}
		}
		if err != nil {
			c.nextTry = now.Add(retryAfter)
			cur = c.cur
			c.mu.Unlock()
			c.warn(ctx, err, cur, now)
			if cur.usable(now) {
				return cur, nil
			}
			return credentials{}, core.WrapError(err, "s3_credentials_unavailable", "s3store: 태스크 역할 자격 증명을 받지 못했고 쓸 수 있는 옛 자격 증명도 없다: "+err.Error())
		}
		c.cur, c.nextTry = fresh, time.Time{}
		c.mu.Unlock()
		return fresh, nil
	}
}

func (c *credentialCache) warn(ctx context.Context, err error, cur credentials, now time.Time) {
	attrs := []any{"source", c.kind}
	var fe *fetchError
	if errors.As(err, &fe) {
		attrs = append(attrs, "reason", fe.reason)
		if fe.status != 0 {
			attrs = append(attrs, "status", fe.status)
		}
	} else {
		attrs = append(attrs, "reason", "canceled")
	}
	validFor := int64(0)
	if cur.usable(now) {
		validFor = int64(cur.Expires.Sub(now).Seconds())
	}
	attrs = append(attrs, "valid_for_s", validFor)
	c.log().WarnContext(ctx, "s3.credentials_refresh_failed", attrs...)
}

// fetchError 는 엔드포인트 호출의 실패다. 주소·응답 본문을 싣지 않는다 — 주소에는 자격 증명 경로가, 본문에는 키가 들 수 있다.
type fetchError struct {
	reason string // transport·status·decode·token_file·expired
	status int
	cause  error
}

func (e *fetchError) Error() string {
	if e.status != 0 {
		return fmt.Sprintf("컨테이너 자격 증명 엔드포인트 %s (HTTP %d)", e.reason, e.status)
	}
	return "컨테이너 자격 증명 엔드포인트 " + e.reason
}

// Unwrap 은 시한 초과(context.DeadlineExceeded) 같은 원인을 가르게 한다. 원인 문자열은 Error 에 싣지 않는다.
func (e *fetchError) Unwrap() error { return e.cause }

// containerEndpoint 는 환경 변수에서 엔드포인트를 정한다. 둘 다 없으면 ok 가 false 다.
func containerEndpoint() (endpoint string, token func() (string, error), ok bool, err error) {
	if rel := os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"); rel != "" {
		return containerHost + rel, func() (string, error) { return "", nil }, true, nil
	}
	full := os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI")
	if full == "" {
		return "", nil, false, nil
	}
	u, perr := url.Parse(full)
	if perr != nil || u.Host == "" {
		return "", nil, false, core.NewError("s3_bad_credentials_uri", "s3store: AWS_CONTAINER_CREDENTIALS_FULL_URI 가 주소 모양이 아니다")
	}
	// 토큰을 엉뚱한 곳에 보내지 않는다. http 는 같은 호스트(루프백)와 ECS·EKS 에이전트 주소만 받는다. AWS SDK 와 같은 규칙이다.
	if u.Scheme != "https" && !allowedHTTPHost(u.Hostname()) {
		return "", nil, false, core.NewError("s3_insecure_credentials_uri", "s3store: AWS_CONTAINER_CREDENTIALS_FULL_URI 가 https 가 아니고 루프백·ECS 에이전트 주소도 아니다. 토큰을 보내지 않는다")
	}
	file, inline := os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"), os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN")
	return full, func() (string, error) {
		if file == "" {
			return inline, nil
		}
		// 파일은 바뀔 수 있어서(토큰 회전) 받을 때마다 읽는다.
		b, err := os.ReadFile(file) //nolint:gosec // G304: 경로는 운영자가 준 환경 변수(AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE)다.
		if err != nil {
			return "", &fetchError{reason: "token_file", cause: err}
		}
		return strings.TrimSpace(string(b)), nil
	}, true, nil
}

func allowedHTTPHost(host string) bool {
	switch host {
	case "169.254.170.2", "169.254.170.23", "fd00:ec2::23", "localhost":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func containerCredentials(endpoint string, token func() (string, error)) func(ctx context.Context) (credentials, error) {
	client := &http.Client{Timeout: fetchTimeout}
	return func(ctx context.Context) (credentials, error) {
		ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return credentials{}, &fetchError{reason: "transport", cause: err}
		}
		tok, err := token()
		if err != nil {
			return credentials{}, err
		}
		if tok != "" {
			req.Header.Set("Authorization", tok)
		}
		res, err := client.Do(req)
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err // 주소를 뺀다
			}
			return credentials{}, &fetchError{reason: "transport", cause: err}
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
			return credentials{}, &fetchError{reason: "status", status: res.StatusCode}
		}
		var body struct {
			AccessKeyID     string `json:"AccessKeyId"`
			SecretAccessKey string `json:"SecretAccessKey"`
			Token           string `json:"Token"`
			Expiration      string `json:"Expiration"`
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&body); err != nil {
			return credentials{}, &fetchError{reason: "decode"}
		}
		exp, err := time.Parse(time.RFC3339, body.Expiration)
		if err != nil || body.AccessKeyID == "" || body.SecretAccessKey == "" || body.Token == "" {
			return credentials{}, &fetchError{reason: "decode"}
		}
		return credentials{AccessKey: body.AccessKeyID, SecretKey: body.SecretAccessKey, SessionToken: body.Token, Expires: exp}, nil
	}
}
