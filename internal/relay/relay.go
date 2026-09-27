// Package relay 负责向上游转发：排队、带退避的重试、故障转移、流式透传。
//
// 官方约束（docs/TROUBLESHOOTING.md 与 ERROR_CODES.md）：
//   - 需指数退避的状态码：408 / 429 / 500 / 502 / 503 / 504 / 520 / 522 / 524
//   - 不可重试、必须熔断：401 / 403 / 402
//
// 一处比 Python 基线更严格的地方：**区分幂等与非幂等重试**。
// 旧实现对生图 / 视频提交这类非幂等请求也做「退避后重试」，一旦上游其实已经
// 受理（5xx 发生在受理之后），就会重复扣费、重复建任务 —— 这种错误用户看不见，
// 只会在账单和任务列表里发现。现在的规则是：
//   - 429 / 402 / 401 / 403：上游是「在受理前拒绝」，重试安全，换账号重试
//     （402 额度耗尽的账号冷却一段时间，401/403 熔断待复活）；
//   - 408/5xx：仅当调用方声明幂等（GET 轮询，或显式允许）才重试，否则立即返回。
package relay

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"time"

	"agneshub/internal/config"
	"agneshub/internal/hub"
	"agneshub/internal/pool"
)

// RetryableStatus 需退避重试的状态码。
var RetryableStatus = map[int]bool{
	408: true, 429: true, 500: true, 502: true, 503: true, 504: true, 520: true, 522: true, 524: true,
}

// AuthFailStatus 不可重试、必须熔断的状态码。
var AuthFailStatus = map[int]bool{401: true, 403: true, 402: true}

// hopByHop 逐跳首部，不能原样透传。
var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailers": true,
	"transfer-encoding": true, "upgrade": true, "content-length": true,
	"content-encoding": true, "host": true,
}

// BuildClient 构造上游 HTTP 客户端。
//
// 不设 Client.Timeout（视频任务提交后响应可能很慢），改为由 ctx 控制；
// http.Client 的原生流式能力让我们无需缓冲即可透传 SSE。
func BuildClient() *http.Client {
	transport := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 60,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// UpstreamRoot 把账号的 base_url 归一化成「站点根」。
//
// 官方同时存在两种写法（带 /v1 与不带），而端点路径本身带 /v1。
// 若直接拼接，写成 /v1 的用户会得到 /v1/v1/chat/completions ——
// 即官方 ERROR_CODES.md 点名过的「第三方工具重复拼接 /v1」404 坑。
func UpstreamRoot(a *config.Account) string {
	base := strings.TrimRight(strings.TrimSpace(a.BaseURL), "/")
	if base == "" {
		base = config.DefaultBaseURL
	}
	if strings.HasSuffix(base, "/v1") {
		base = strings.TrimSuffix(base, "/v1")
	}
	return strings.TrimRight(base, "/")
}

// UpstreamURL 拼接上游完整 URL。
func UpstreamURL(a *config.Account, path string) string {
	root := UpstreamRoot(a)
	if strings.HasPrefix(path, "/") {
		return root + path
	}
	return root + "/" + path
}

// ClientHeaders 构造上游请求头。
func ClientHeaders(a *config.Account, extra map[string]string, anthropic bool) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+a.APIKey)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("User-Agent", "agnes-hub/1.0")
	if anthropic {
		// 官方文档：/v1/messages 走 Anthropic 兼容通道、以 x-api-key 鉴权。
		// 同时带上两种鉴权头以兼容。
		h.Set("x-api-key", a.APIKey)
		h.Set("anthropic-version", "2023-06-01")
	}
	for k, v := range extra {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		h.Set(k, v)
	}
	return h
}

// SanitizeHeaders 剔除逐跳首部与无法用 latin-1 表达的值。
//
// 后者是硬约束：HTTP 头只能承载 latin-1 字节，而账号名常含中文，
// 直接把中文写进响应头会让整个响应变成 500。
func SanitizeHeaders(in http.Header) http.Header {
	out := http.Header{}
	for k, values := range in {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range values {
			if !isLatin1(v) {
				continue
			}
			out.Add(k, v)
		}
	}
	return out
}

func isLatin1(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7F {
			return false
		}
	}
	return true
}

// Options 是一次转发的完整入参。
type Options struct {
	SessionKey    string
	PoolClass     string
	Pinned        string
	RequiredModel string
	Method        string
	Path          string
	Body          []byte
	// BodyFor 允许按「最终选中的账号」生成请求体
	// （agnes-auto 需要按该账号声明的清单改写 model 字段）。
	BodyFor      func(a *config.Account) ([]byte, string)
	ExtraHeaders map[string]string
	Anthropic    bool
	// Idempotent 声明该请求可否安全重试。GET 轮询与纯文本对话为 true，
	// 生图 / 视频提交为 false（重试会造成重复扣费与重复建任务）。
	Idempotent bool
}

// Result 是一次转发的产物。
type Result struct {
	Status    int
	Header    http.Header
	Body      []byte
	Stream    io.ReadCloser // 非 nil 表示上游仍在流式输出，调用方负责消费并关闭
	Account   *config.Account
	ModelUsed string
	WaitMS    int64
	Attempts  int
}

// Close 释放流。
func (r *Result) Close() {
	if r != nil && r.Stream != nil {
		_ = r.Stream.Close()
		r.Stream = nil
	}
}

// ReadAll 读取完整响应体（用于非流式路径）。
func (r *Result) ReadAll() []byte {
	if r == nil {
		return []byte("{}")
	}
	if r.Stream == nil {
		if len(r.Body) == 0 {
			return []byte("{}")
		}
		return r.Body
	}
	raw, _ := io.ReadAll(r.Stream)
	_ = r.Stream.Close()
	r.Stream = nil
	if len(raw) == 0 {
		return []byte("{}")
	}
	return raw
}

// Do 带排队 / 重试 / 换账号的转发入口。
func Do(ctx context.Context, h *hub.Hub, client *http.Client, opts Options) (*Result, error) {
	if opts.Method == "" {
		opts.Method = http.MethodPost
	}
	s := h.Settings()
	retryMax := s.RetryMax
	if retryMax < 0 {
		retryMax = 0
	}

	exclude := map[string]bool{}
	var totalWait int64
	var last *Result
	// 记录一次到达，供控制台「到达密度 vs 节拍」观测。
	h.Arrivals.Add()

	for attempt := 0; attempt <= retryMax; attempt++ {
		picked, err := h.Pick(opts.SessionKey, opts.PoolClass, opts.Pinned, opts.RequiredModel, exclude)
		if err != nil {
			return nil, err
		}
		account := picked.Account

		wait, err := h.Acquire(ctx, account, opts.PoolClass)
		if err != nil {
			return nil, err
		}
		totalWait += wait.Milliseconds()

		body := opts.Body
		modelUsed := opts.RequiredModel
		if opts.BodyFor != nil {
			generated, model := opts.BodyFor(account)
			body = generated
			if model != "" {
				modelUsed = model
			}
		}

		h.Metrics.RequestsTotal.Add(1)
		h.TrackInflight(account.ID, 1)
		result, retry, err := attemptOnce(ctx, h, client, account, opts, body, modelUsed, totalWait, attempt+1)
		h.TrackInflight(account.ID, -1)
		if err != nil {
			return nil, err
		}
		last = result

		if !retry {
			if result.Status < 400 {
				h.NoteSuccess(account)
				h.OnSuccess(account, opts.PoolClass)
			}
			h.Metrics.WaitMS.Add(result.WaitMS)
			return result, nil
		}

		// 非幂等请求只在「受理前拒绝」（429 限流 / 402 额度 / 401·403 鉴权）上换号重试，
		// 其余失败必须立即返回：5xx 可能发生在上游已受理之后，重试会造成重复副作用。
		if !opts.Idempotent && !RefusedBeforeAccept[result.Status] {
			h.Metrics.WaitMS.Add(result.WaitMS)
			return result, nil
		}

		exclude[account.ID] = true
		if attempt < retryMax {
			sleepBackoff(ctx, s, attempt)
			continue
		}
		h.Metrics.RequestsError.Add(1)
		h.Metrics.WaitMS.Add(result.WaitMS)
		return result, nil
	}
	if last == nil {
		return nil, errors.New("没有可用账号完成该请求")
	}
	return last, nil
}

// attemptOnce 单次尝试。返回 (结果, 是否应重试, 致命错误)。
func attemptOnce(ctx context.Context, h *hub.Hub, client *http.Client, account *config.Account,
	opts Options, body []byte, modelUsed string, totalWaitMS int64, attemptNo int) (*Result, bool, error) {

	url := UpstreamURL(account, opts.Path)

	// 请求超时只管「发出请求 → 收到响应头」这一段，防止上游 hang 住耗尽连接池。
	//
	// 不能用 context.WithTimeout 包住整个请求：context 的截止时间会一直作用到读完
	// 响应体，于是超过 30 s 的流式回答会被拦腰截断（客户端只看到半截、没有 [DONE]），
	// 慢一点的非流式回答被当成上游失败，再被重试到别的账号上重复计费。
	// 收到响应头之后，正文的生命周期交给调用方的 ctx（客户端断开即取消）。
	s := h.Settings()
	reqCtx, cancel := context.WithCancel(ctx)

	req, err := http.NewRequestWithContext(reqCtx, opts.Method, url, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, false, err
	}
	for k, values := range ClientHeaders(account, opts.ExtraHeaders, opts.Anthropic) {
		req.Header[k] = values
	}

	// 单账号并发上限（按模态分离，避免视频轮询挤占文本请求）
	sem := h.Semaphore(account, pool.ModalityOfPool(opts.PoolClass))
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		cancel()
		return nil, false, ctx.Err()
	}

	// 计时从拿到并发名额之后开始：排队等名额的时间不该吃掉上游的超时预算。
	// RequestTimeoutMS<=0 表示不限时（与配置项注释一致）。
	var headerTimer *time.Timer
	if s.RequestTimeoutMS > 0 {
		headerTimer = time.AfterFunc(time.Duration(s.RequestTimeoutMS)*time.Millisecond, cancel)
	}
	resp, err := client.Do(req)
	if headerTimer != nil && !headerTimer.Stop() {
		// 计时器已经触发：reqCtx 已被取消，即使 Do 恰好成功，正文也读不了了。
		if err == nil {
			resp.Body.Close()
		}
		err = fmt.Errorf("上游 %d ms 内未返回响应", s.RequestTimeoutMS)
	}
	if err != nil && ctx.Err() != nil {
		// 客户端已断开：不是上游的错，不记账号错误、不换号重试（旧实现会让后续
		// 每次重试都在别的账号上白白占一个限流名额，并记一次假错误）。
		<-sem
		cancel()
		return nil, false, ctx.Err()
	}
	if err != nil {
		<-sem
		cancel()
		h.NoteError(account, fmt.Sprintf("%T: %v", err, err))
		h.Metrics.RequestsError.Add(1)
		return &Result{
			Status:  http.StatusBadGateway,
			Header:  http.Header{"Content-Type": []string{"application/json"}},
			Body:    errorBody(describeUpstreamError(err), "upstream_error"),
			Account: account, ModelUsed: modelUsed, WaitMS: totalWaitMS, Attempts: attemptNo,
		}, true, nil
	}

	status := resp.StatusCode

	if status == 402 {
		// 402 Payment Required：额度耗尽。冷却该账号并换号重试 —— 402 是受理前拒绝，
		// 换号不会造成重复副作用。
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		<-sem
		cancel()
		h.OnQuotaExhausted(account, fmt.Sprintf("HTTP %d: %s", status, string(raw)))
		return &Result{
			Status: status, Header: SanitizeHeaders(resp.Header), Body: raw,
			Account: account, ModelUsed: modelUsed, WaitMS: totalWaitMS, Attempts: attemptNo,
		}, true, nil
	}

	if AuthFailStatus[status] { // 401 / 403
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		<-sem
		cancel()
		h.OnAuthFailure(account, fmt.Sprintf("HTTP %d: %s", status, string(raw)))
		return &Result{
			Status: status, Header: SanitizeHeaders(resp.Header), Body: raw,
			Account: account, ModelUsed: modelUsed, WaitMS: totalWaitMS, Attempts: attemptNo,
		}, true, nil
	}

	if status == http.StatusTooManyRequests {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		<-sem
		cancel()
		h.OnRateLimited(account, opts.PoolClass)
		return &Result{
			Status: status, Header: SanitizeHeaders(resp.Header), Body: raw,
			Account: account, ModelUsed: modelUsed, WaitMS: totalWaitMS, Attempts: attemptNo,
		}, true, nil
	}

	if RetryableStatus[status] {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		<-sem
		cancel()
		h.NoteError(account, fmt.Sprintf("HTTP %d: %s", status, string(raw)))
		return &Result{
			Status: status, Header: SanitizeHeaders(resp.Header), Body: raw,
			Account: account, ModelUsed: modelUsed, WaitMS: totalWaitMS, Attempts: attemptNo,
		}, true, nil
	}

	// 交给调用方消费：包一层以便在关闭时释放并发信号量并取消 context。
	// 注意：成功路径【绝不】在此提前关闭 body 或取消 reqCtx，否则调用方读取 body 时
	// 连接已关闭 / context 已取消，会读到空体（表现为网关回 {}）。
	// 直到调用方读完 body 触发 Close()，才在此 release 里 cancel()。
	wrapped := &releaseOnClose{ReadCloser: resp.Body, release: func() {
		cancel() // body 读取完毕后才取消 context
		select {
		case <-sem:
		default:
		}
	}}
	return &Result{
		Status: status, Header: SanitizeHeaders(resp.Header), Stream: wrapped,
		Account: account, ModelUsed: modelUsed, WaitMS: totalWaitMS, Attempts: attemptNo,
	}, false, nil
}

type releaseOnClose struct {
	io.ReadCloser
	release func()
	once    bool
}

func (s *releaseOnClose) Close() error {
	err := s.ReadCloser.Close()
	if !s.once {
		s.once = true
		s.release()
	}
	return err
}

// describeUpstreamError 把网络错误归成给调用方看的类别。
//
// Go 的错误文本里带完整的上游地址（Post "https://…/v1/chat/completions": dial tcp …），
// 回给下游等于把号池用的是哪家上游、哪个地址全告诉对方。完整原文仍记在账号的
// 「最近错误」里（NoteError），管理员在控制台照样看得到。
func describeUpstreamError(err error) string {
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var netErr net.Error
	switch {
	case errors.As(err, &dnsErr):
		return "上游连接失败：域名解析失败"
	case errors.As(err, &certErr):
		return "上游连接失败：TLS 证书校验失败"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "上游连接失败：连接或响应超时"
	case strings.Contains(err.Error(), "未返回响应"):
		return "上游连接失败：" + err.Error() // 我们自己的超时文案，不含地址
	case strings.Contains(err.Error(), "connection refused"):
		return "上游连接失败：连接被拒绝"
	case strings.Contains(err.Error(), "tls:"):
		return "上游连接失败：TLS 握手失败"
	}
	return "上游连接失败：网络错误"
}

// RefusedBeforeAccept 是上游「没受理就拒绝」的状态码：换号重试不会重复执行请求。
var RefusedBeforeAccept = map[int]bool{
	http.StatusUnauthorized:    true,
	http.StatusPaymentRequired: true,
	http.StatusForbidden:       true,
	http.StatusTooManyRequests: true,
}

func sleepBackoff(ctx context.Context, s config.Settings, attempt int) {
	base := time.Duration(s.RetryBaseBackoffMS) * time.Millisecond
	capDelay := time.Duration(s.RetryMaxBackoffMS) * time.Millisecond
	delay := base * time.Duration(1<<uint(attempt))
	if delay > capDelay {
		delay = capDelay
	}
	jitter := 0.7 + rand.Float64()*0.6
	select {
	case <-time.After(time.Duration(float64(delay) * jitter)):
	case <-ctx.Done():
	}
}

func errorBody(message, etype string) []byte {
	buf, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": etype},
	})
	return buf
}
