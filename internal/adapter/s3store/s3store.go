// Package s3store 는 첨부를 S3 호환 객체 저장소에 두는 어댑터다. AWS S3·MinIO 등 S3 API 를 따르는 저장소에서 같은 코드가 돈다.
//
// 서버는 파일 바이트를 받지 않는다. 받기는 주소 서명(presigned GET)을 발급한다. 올리기는 설정(Config.Upload)으로 둘 중 하나다(D42).
//   - post: 양식 업로드(POST policy). policy 의 content-length-range 로 저장소가 크기를 지킨다. AWS S3·MinIO.
//   - put: 서명된 PUT 주소. Content-Type 과 Content-Length 를 서명 헤더에 묶는다. 양식 업로드(POST policy)가 없는 저장소가 이쪽이다.
//     서명된 Content-Length 를 저장소가 지키는지는 저장소에 달렸다(MinIO 는 지킨다, 문서에 적지 않은 저장소도 있다). 지키지 못해도
//     앱이 완료 때 크기를 확인해 다르면 지운다.
//
// SDK 를 쓰지 않는다. 필요한 것이 서명 둘과 요청 넷(확인·복사·삭제·버킷 만들기)이고, SigV4 는 표준 라이브러리로 80줄이다.
// 자격 증명은 정적 키(로컬 MinIO) 또는 ECS 태스크 역할의 임시 자격 증명이다(credentials.go). 임시 자격 증명의 세션 토큰은
// 서명한 주소의 쿼리(X-Amz-Security-Token)와 양식 업로드의 필드·policy(x-amz-security-token)에 실린다.
package s3store

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/telemetry"
)

type Config struct {
	// Endpoint 는 서버가 저장소를 부를 주소다. 예: http://minio:9000, https://s3.ap-northeast-2.amazonaws.com
	Endpoint string
	// PublicEndpoint 는 브라우저가 쓰는 주소다. 서명에 호스트가 들어가므로 서버용과 다르면 따로 서명한다. 비우면 Endpoint.
	PublicEndpoint string
	Region         string
	Bucket         string
	// AccessKey·SecretKey 는 정적 키다. 둘 다 비우면 ECS 컨테이너 자격 증명 엔드포인트(태스크 역할)에서 받는다.
	AccessKey string
	SecretKey string
	// PathStyle 이 true 면 https://host/bucket/key, false 면 https://bucket.host/key. MinIO 는 true 가 기본이다.
	PathStyle bool
	// Upload 는 브라우저가 올리는 방식이다: UploadModePost(기본, 비우면 이것) 또는 UploadModePut.
	Upload string
	// Logger 는 자격 증명 갱신 실패를 남길 곳이다. 비우면 slog.Default().
	Logger *slog.Logger
}

// 올리기 방식 설정값(CHAT_S3_UPLOAD)이다.
const (
	UploadModePost = "post"
	UploadModePut  = "put"
)

type Store struct {
	cfg      Config
	internal *url.URL
	public   *url.URL
	client   *http.Client
	creds    *credentialCache
	now      func() time.Time
}

// New 는 저장소를 연다. 태스크 역할을 쓰면 여기서 자격 증명을 한 번 받아 본다 — 못 받으면 오류다.
// 자격 증명 없이 조용히 떠서 첫 업로드에서야 실패하는 것보다 기동이 실패하는 편이 낫다.
func New(cfg Config) (*Store, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, core.NewError("s3_config_missing", "s3store: CHAT_S3_ENDPOINT 와 CHAT_S3_BUCKET 이 둘 다 있어야 하는데 비어 있는 것이 있다")
	}
	switch cfg.Upload {
	case "":
		cfg.Upload = UploadModePost
	case UploadModePost, UploadModePut:
	default:
		return nil, core.NewError("s3_bad_upload_mode", "s3store: 올리기 방식(CHAT_S3_UPLOAD)이 post 도 put 도 아니다")
	}
	logger := func() *slog.Logger {
		if cfg.Logger != nil {
			return cfg.Logger
		}
		return slog.Default()
	}
	var creds *credentialCache
	switch {
	case cfg.AccessKey != "" && cfg.SecretKey != "":
		creds = staticCredentials(cfg.AccessKey, cfg.SecretKey)
	case cfg.AccessKey != "" || cfg.SecretKey != "":
		return nil, core.NewError("s3_partial_keys", "s3store: access key 와 secret key 중 하나만 있다. 둘 다 주거나(정적 키) 둘 다 비워야 한다(태스크 역할)")
	default:
		endpoint, token, ok, err := containerEndpoint()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, core.NewError("s3_no_credentials", "s3store: 자격 증명을 찾지 못했다. 정적 키(CHAT_S3_ACCESS_KEY·CHAT_S3_SECRET_KEY)나 ECS 태스크 역할(AWS_CONTAINER_CREDENTIALS_*)이 필요하다")
		}
		creds = &credentialCache{kind: "container", fetch: containerCredentials(endpoint, token), now: time.Now, log: logger}
		ctx, cancel := context.WithTimeout(context.Background(), 2*fetchTimeout)
		defer cancel()
		if _, err := creds.get(ctx); err != nil {
			return nil, err
		}
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.PublicEndpoint == "" {
		cfg.PublicEndpoint = cfg.Endpoint
	}
	internal, err := url.Parse(cfg.Endpoint)
	if err != nil || internal.Host == "" {
		return nil, core.NewError("s3_bad_endpoint", fmt.Sprintf("s3store: CHAT_S3_ENDPOINT 가 주소 모양이 아니다: %q", cfg.Endpoint))
	}
	public, err := url.Parse(cfg.PublicEndpoint)
	if err != nil || public.Host == "" {
		return nil, core.NewError("s3_bad_public_endpoint", fmt.Sprintf("s3store: CHAT_S3_PUBLIC_ENDPOINT 가 주소 모양이 아니다: %q", cfg.PublicEndpoint))
	}
	return &Store{cfg: cfg, internal: internal, public: public, creds: creds,
		client: &http.Client{Timeout: 15 * time.Second, Transport: telemetry.Transport("s3", bucketReplyPeek{http.DefaultTransport})}, now: time.Now}, nil
}

// CredentialSource 는 자격 증명이 어디서 오는지다: static(정적 키) 또는 container(태스크 역할). 기동 로그용이고 값은 싣지 않는다.
func (s *Store) CredentialSource() string { return s.creds.kind }

// Origin 은 브라우저가 파일을 올리고 받는 출처다. 페이지의 보안 정책이 이 출처를 허용해야 한다.
func (s *Store) Origin() string {
	scheme, host, _ := s.location(s.public, "")
	return scheme + "://" + host
}

// location 은 객체의 호스트와 경로다. 경로 방식과 가상 호스트 방식이 여기서 갈린다.
func (s *Store) location(base *url.URL, key string) (scheme, host, path string) {
	if s.cfg.PathStyle {
		path = "/" + s.cfg.Bucket
		if key != "" {
			path += "/" + key
		}
		return base.Scheme, base.Host, path
	}
	return base.Scheme, s.cfg.Bucket + "." + base.Host, "/" + key
}

// UploadMode 는 이 저장소가 발급하는 올리기 방식(post 또는 put)이다. 기동 로그용이다.
func (s *Store) UploadMode() string { return s.cfg.Upload }

// PresignUpload 는 설정한 방식의 올리기 양식을 만든다. size 는 신청한 크기다.
func (s *Store) PresignUpload(key, contentType string, size int64, ttl time.Duration) (core.UploadForm, error) {
	if s.cfg.Upload == UploadModePut {
		return s.presignPut(key, contentType, size, ttl)
	}
	return s.presignPost(key, contentType, size, ttl)
}

// presignPut 은 서명된 PUT 주소를 만든다. Content-Type 과 Content-Length 가 서명 헤더다 — 브라우저가 다른 종류나 다른 크기로
// 보내면 서명이 맞지 않아 저장소가 403 으로 거절한다. Content-Length 는 브라우저가 본문으로 정하므로 Headers 에 싣지 않는다.
// 키는 주소 경로에 있어 다른 키로 올릴 수 없다.
func (s *Store) presignPut(key, contentType string, size int64, ttl time.Duration) (core.UploadForm, error) {
	signed := map[string]string{"Content-Type": contentType, "Content-Length": strconv.FormatInt(size, 10)}
	link, err := s.presign(context.Background(), http.MethodPut, s.public, key, nil, signed, ttl)
	if err != nil {
		return core.UploadForm{}, err
	}
	return core.UploadForm{
		Method: core.UploadPut, URL: link,
		Headers:   map[string]string{"Content-Type": contentType},
		ExpiresAt: s.now().UTC().Add(ttl),
	}, nil
}

// presignPost 는 양식 업로드(POST policy)의 필드를 만든다. 크기는 content-length-range 로 저장소가 지킨다.
func (s *Store) presignPost(key, contentType string, maxBytes int64, ttl time.Duration) (core.UploadForm, error) {
	creds, err := s.creds.get(context.Background())
	if err != nil {
		return core.UploadForm{}, err
	}
	now := s.now().UTC()
	amzDate, scope := now.Format("20060102T150405Z"), s.scope(now)
	credential := creds.AccessKey + "/" + scope
	exp := now.Add(ttl)
	// 저장소는 이 조건을 모두 만족하는 양식만 받는다. 키·종류·크기를 브라우저가 바꾸면 거절된다.
	conditions := []any{
		map[string]string{"bucket": s.cfg.Bucket},
		map[string]string{"key": key},
		map[string]string{"Content-Type": contentType},
		[]any{"content-length-range", 1, maxBytes},
		map[string]string{"x-amz-algorithm": algorithm},
		map[string]string{"x-amz-credential": credential},
		map[string]string{"x-amz-date": amzDate},
	}
	fields := map[string]string{
		"key": key, "Content-Type": contentType,
		"x-amz-algorithm": algorithm, "x-amz-credential": credential, "x-amz-date": amzDate,
	}
	// 임시 자격 증명이면 세션 토큰이 양식 필드에도, policy 조건에도 있어야 저장소가 받는다.
	if creds.SessionToken != "" {
		conditions = append(conditions, map[string]string{"x-amz-security-token": creds.SessionToken})
		fields["x-amz-security-token"] = creds.SessionToken
	}
	policy, err := json.Marshal(map[string]any{
		"expiration": exp.Format("2006-01-02T15:04:05.000Z"),
		"conditions": conditions,
	})
	if err != nil {
		return core.UploadForm{}, err
	}
	encoded := base64.StdEncoding.EncodeToString(policy)
	fields["policy"] = encoded
	fields["x-amz-signature"] = hex.EncodeToString(hmacSHA256(s.signingKey(creds.SecretKey, now), encoded))
	scheme, host, path := s.location(s.public, "")
	return core.UploadForm{Method: core.UploadPost, URL: scheme + "://" + host + path, Fields: fields, ExpiresAt: exp}, nil
}

func (s *Store) PresignDownload(key, filename, contentType string, inline bool, ttl time.Duration) (string, error) {
	// 저장소가 응답에 이 헤더를 싣는다. 올릴 때 브라우저가 말한 종류가 아니라 서버가 내용으로 판정한 종류로 내려간다.
	query := url.Values{
		"response-content-type":        {contentType},
		"response-content-disposition": {core.ContentDisposition(inline, filename)},
	}
	return s.presign(context.Background(), http.MethodGet, s.public, key, query, nil, ttl)
}

// Stat 은 앞부분만 읽는 요청 하나로 크기와 머리 바이트를 같이 얻는다.
func (s *Store) Stat(ctx context.Context, key string) (core.ObjectInfo, error) {
	link, err := s.presign(ctx, http.MethodGet, s.internal, key, nil, nil, time.Minute)
	if err != nil {
		return core.ObjectInfo{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return core.ObjectInfo{}, transport("확인", err)
	}
	req.Header.Set("Range", "bytes=0-"+strconv.Itoa(core.ObjectHeadBytes-1))
	res, err := s.client.Do(req)
	if err != nil {
		return core.ObjectInfo{}, transport("확인", err)
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusNotFound:
		return core.ObjectInfo{}, core.ErrNotFound
	case http.StatusRequestedRangeNotSatisfiable: // 빈 객체
		return core.ObjectInfo{}, nil
	case http.StatusOK, http.StatusPartialContent:
	default:
		return core.ObjectInfo{}, s.failure("확인", res)
	}
	head, err := io.ReadAll(io.LimitReader(res.Body, core.ObjectHeadBytes))
	if err != nil {
		return core.ObjectInfo{}, transport("확인", err)
	}
	size := res.ContentLength
	// 206 이면 전체 크기는 Content-Range 의 끝에 있다: "bytes 0-65535/1234567"
	if _, total, ok := strings.Cut(res.Header.Get("Content-Range"), "/"); ok {
		if n, err := strconv.ParseInt(total, 10, 64); err == nil {
			size = n
		}
	}
	return core.ObjectInfo{Size: size, Head: head}, nil
}

// Promote 는 확인한 임시 객체를 확정 위치로 옮긴다. S3 에는 이름 바꾸기가 없어서 복사(CopyObject) 뒤 원본을 지운다.
// 복사는 저장소 안에서 일어나 서버가 바이트를 받지 않는다. 원본이 없으면 core.ErrNotFound.
func (s *Store) Promote(ctx context.Context, from, to string) error {
	_, _, srcPath := s.location(&url.URL{}, from)
	if !s.cfg.PathStyle {
		srcPath = "/" + s.cfg.Bucket + srcPath
	}
	// 복사 원본 헤더도 서명에 넣는다. 서명 밖에 두면 주소를 가로챈 쪽이 원본을 바꿔 다른 객체를 이 키로 복사할 수 있다.
	headers := map[string]string{"x-amz-copy-source": escapePath(srcPath)}
	link, err := s.presign(ctx, http.MethodPut, s.internal, to, nil, headers, time.Minute)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, link, nil)
	if err != nil {
		return transport("복사", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := s.client.Do(req)
	if err != nil {
		return transport("복사", err)
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return core.ErrNotFound
	default:
		return s.failure("복사", res)
	}
	// 복사는 200 을 먼저 보내고 본문에 오류를 실을 수 있다. 오류 본문이면 실패다.
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	if err != nil {
		return transport("복사", err)
	}
	if strings.Contains(string(body), "<Error>") {
		// 응답은 200 인데 본문이 오류다. 상태 코드로는 가를 수 없어 따로 이름을 붙인다.
		return core.NewError("s3_copy_error", "s3store: 복사 응답이 200 인데 본문이 오류다: "+errorCode(body))
	}
	return s.Remove(ctx, from)
}

func (s *Store) Remove(ctx context.Context, key string) error {
	return s.do(ctx, http.MethodDelete, key, "삭제", http.StatusOK, http.StatusNoContent, http.StatusNotFound)
}

// EnsureBucket 은 버킷이 없으면 만든다. 로컬 개발용이다. 운영 버킷은 수명 주기·CORS 와 함께 미리 만들어 둔다.
// 저장소 컨테이너가 서버보다 늦게 뜰 수 있어서 ctx 가 끝날 때까지 다시 시도한다.
func (s *Store) EnsureBucket(ctx context.Context) error {
	for {
		err := s.createBucket(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// createBucket 은 버킷 만들기 한 번이다. 409(이미 있다)도 성공이다.
//
// 두 번째 기동부터는 늘 409 BucketAlreadyOwnedByYou 라서, 그 호출의 스팬은 오류로 두지 않는다(정상인 답이 기동마다 오류 스팬으로
// 쌓이면 진짜 오류를 가린다). 다른 409(남의 버킷)와 다른 거절은 계측이 붙인 오류 그대로다. 오류 코드는 응답 본문에 있는데,
// 계측(otelhttp)은 본문을 끝까지 읽는 순간 스팬을 닫는다. 그래서 계측 한 칸 아래(bucketReplyPeek)가 본문을 먼저 읽어 코드를 남긴다.
func (s *Store) createBucket(ctx context.Context) error {
	const what = "버킷 만들기"
	link, err := s.presign(ctx, http.MethodPut, s.internal, "", nil, nil, time.Minute)
	if err != nil {
		return err
	}
	reply := &bucketReply{}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, bucketReplyKey{}, reply), http.MethodPut, link, nil)
	if err != nil {
		return transport(what, err)
	}
	res, err := s.client.Do(req)
	if err != nil {
		return transport(what, err)
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusConflict:
		if reply.code == "BucketAlreadyOwnedByYou" {
			// 스팬은 아직 열려 있다(본문을 닫지 않았다). Ok 는 계측이 붙인 Error 를 덮는다.
			telemetry.ExpectedReply(res.Request.Context())
		}
		return nil
	}
	return s.failure(what, res)
}

// bucketReplyKey 는 버킷 만들기 요청의 ctx 에 bucketReply 를 싣는 키다.
type bucketReplyKey struct{}

// bucketReply 는 버킷 만들기 409 응답의 오류 코드다. bucketReplyPeek 이 채운다.
type bucketReply struct{ code string }

// bucketReplyPeek 은 계측 아래의 전송이다. 버킷 만들기 요청(ctx 에 bucketReply 가 있다)의 409 응답만 본문을 읽어 코드를 남기고,
// 읽은 본문을 그대로 되돌려 둔다. 다른 요청은 손대지 않는다.
type bucketReplyPeek struct{ base http.RoundTripper }

func (p bucketReplyPeek) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := p.base.RoundTrip(r)
	reply, _ := r.Context().Value(bucketReplyKey{}).(*bucketReply)
	if err != nil || reply == nil || res.StatusCode != http.StatusConflict {
		return res, err
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	_ = res.Body.Close()
	res.Body = io.NopCloser(bytes.NewReader(body))
	reply.code = errorCode(body)
	return res, nil
}

func (s *Store) do(ctx context.Context, method, key, what string, okStatus ...int) error {
	link, err := s.presign(ctx, method, s.internal, key, nil, nil, time.Minute)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, link, nil)
	if err != nil {
		return transport(what, err)
	}
	res, err := s.client.Do(req)
	if err != nil {
		return transport(what, err)
	}
	defer res.Body.Close()
	for _, ok := range okStatus {
		if res.StatusCode == ok {
			return nil
		}
	}
	return s.failure(what, res)
}

// failure 는 저장소가 거절한 응답의 오류다. 오류 본문에는 버킷·키(Resource, Key)가 실려 오므로 오류 코드(Code)만 옮긴다.
// 고정 코드는 s3_<HTTP 상태>다(s3_403·s3_500 …, D47). 로그의 err_kind 가 이 값이라 "권한(403)" 과 "저장소 장애(5xx)" 를 로그로 가른다.
// S3 의 <Code>(AccessDenied …)는 저장소가 정한 글자라 고정 코드에 넣지 않고 개발자용 문구에만 둔다.
func (s *Store) failure(what string, res *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	return core.NewError("s3_"+strconv.Itoa(res.StatusCode),
		fmt.Sprintf("s3store: %s 요청을 저장소가 HTTP %d 로 거절했다(%s)", what, res.StatusCode, errorCode(body)))
}

// transport 는 요청이 저장소에 닿지 못한 오류다. net/http 의 *url.Error 는 요청 주소를 문자열에 싣는데, 그 주소는 서명된 주소라
// 자격 증명 id·서명·버킷·키가 들어 있다. 오류 문자열은 로그·스팬 어디로든 옮겨 갈 수 있으므로(전에는 "요청 처리 실패"(지금 http.failed) 로그의 err 로 찍혔다.
// 지금 그 로그는 종류만 싣는다) 주소를 빼고 동작과 원인만 남긴다.
// 원인은 감싸 둔다 — 앱이 errors.Is 로 시한 초과(context.DeadlineExceeded)를 가른다.
func transport(what string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("s3store: %s 요청 실패 (%s): %w", what, ue.Op, ue.Err)
	}
	return fmt.Errorf("s3store: %s 요청 실패: %w", what, err)
}

// errorCode 는 S3 오류 본문의 <Code> 값이다. 없으면 "알 수 없음" 이다.
func errorCode(body []byte) string {
	_, rest, ok := strings.Cut(string(body), "<Code>")
	if !ok {
		return "알 수 없음"
	}
	code, _, ok := strings.Cut(rest, "</Code>")
	if !ok || len(code) > 64 {
		return "알 수 없음"
	}
	return code
}

// ── SigV4 ────────────────────────────────────────────────────────

const algorithm = "AWS4-HMAC-SHA256"

func (s *Store) scope(t time.Time) string {
	return t.Format("20060102") + "/" + s.cfg.Region + "/s3/aws4_request"
}

func (s *Store) signingKey(secret string, t time.Time) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), t.Format("20060102"))
	k = hmacSHA256(k, s.cfg.Region)
	k = hmacSHA256(k, "s3")
	return hmacSHA256(k, "aws4_request")
}

// presign 은 서명을 쿼리에 실은 주소를 만든다. 서명하는 헤더는 host 와 headers 고, 본문은 서명하지 않는다.
// headers 를 넘겼으면 요청에도 같은 값으로 실어야 한다. 서버가 저장소에 보내는 요청(확인·복사·삭제)도 이 주소로 간다.
// 임시 자격 증명이면 세션 토큰을 X-Amz-Security-Token 쿼리에 싣고 서명에 넣는다. 쿼리 서명에서는 토큰도 쿼리에 있어야 한다.
func (s *Store) presign(ctx context.Context, method string, base *url.URL, key string, extra url.Values, headers map[string]string, ttl time.Duration) (string, error) {
	creds, err := s.creds.get(ctx)
	if err != nil {
		return "", err
	}
	now := s.now().UTC()
	amzDate, scope := now.Format("20060102T150405Z"), s.scope(now)
	scheme, host, path := s.location(base, key)

	signed := map[string]string{"host": host}
	for k, v := range headers {
		signed[strings.ToLower(k)] = strings.TrimSpace(v)
	}
	names := make([]string, 0, len(signed))
	for k := range signed {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, k := range names {
		canonicalHeaders.WriteString(k + ":" + signed[k] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	query := url.Values{
		"X-Amz-Algorithm":     {algorithm},
		"X-Amz-Credential":    {creds.AccessKey + "/" + scope},
		"X-Amz-Date":          {amzDate},
		"X-Amz-Expires":       {strconv.Itoa(int(ttl.Seconds()))},
		"X-Amz-SignedHeaders": {signedHeaders},
	}
	if creds.SessionToken != "" {
		query["X-Amz-Security-Token"] = []string{creds.SessionToken}
	}
	for k, v := range extra {
		query[k] = v
	}
	canonicalQuery, canonicalPath := canonicalQuery(query), escapePath(path)
	request := strings.Join([]string{method, canonicalPath, canonicalQuery, canonicalHeaders.String(), signedHeaders, "UNSIGNED-PAYLOAD"}, "\n")
	digest := sha256.Sum256([]byte(request))
	toSign := strings.Join([]string{algorithm, amzDate, scope, hex.EncodeToString(digest[:])}, "\n")
	signature := hex.EncodeToString(hmacSHA256(s.signingKey(creds.SecretKey, now), toSign))
	return scheme + "://" + host + canonicalPath + "?" + canonicalQuery + "&X-Amz-Signature=" + signature, nil
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func canonicalQuery(v url.Values) string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, escape(k)+"="+escape(v.Get(k)))
	}
	return strings.Join(parts, "&")
}

func escapePath(p string) string {
	segments := strings.Split(p, "/")
	for i, seg := range segments {
		segments[i] = escape(seg)
	}
	return strings.Join(segments, "/")
}

// escape 는 SigV4 가 정한 인코딩이다. url.QueryEscape 는 공백을 + 로 바꿔서 서명이 어긋난다.
func escape(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
