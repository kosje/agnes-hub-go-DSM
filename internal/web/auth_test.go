package web

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agneshub/internal/hub"
	"agneshub/internal/relay"
)

// doReq 发一个 JSON 请求；cookies 为空表示匿名。
func doReq(t *testing.T, method, url string, payload any, headers map[string]string, cookies ...*http.Cookie) (*http.Response, string) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		buf, _ := json.Marshal(payload)
		body = strings.NewReader(string(buf))
	}
	req, _ := http.NewRequest(method, url, body)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s 失败：%v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

func findCookie(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// 旧版本 /api/chat/v1/* 完全不鉴权：能连上端口的任何人都能耗光账号池。
func TestChatProxyRequiresSession(t *testing.T) {
	h := newHarness(t, 0, 1)
	body := map[string]any{"model": "agnes-2.5-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for _, path := range []string{"/api/chat/v1/chat/completions", "/api/chat/v1/images/generations", "/api/chat/v1/videos"} {
		resp, raw := doReq(t, http.MethodPost, h.ts.URL+path, body, nil)
		if resp.StatusCode != 401 {
			t.Errorf("匿名调用 %s 应 401，实际 %d：%s", path, resp.StatusCode, raw)
		}
		// 旧版本任意非空的 chat cookie 都能过
		resp, _ = doReq(t, http.MethodPost, h.ts.URL+path, body, nil,
			&http.Cookie{Name: chatSessionCookie, Value: "authenticated"},
			&http.Cookie{Name: "agnes_chat_password", Value: "x"})
		if resp.StatusCode != 401 {
			t.Errorf("伪造 chat cookie 调用 %s 应 401，实际 %d", path, resp.StatusCode)
		}
	}
	if n := h.mock.hits("/v1/chat/completions") + h.mock.hits("/v1/images/generations") + h.mock.hits("/v1/videos"); n != 0 {
		t.Fatalf("未鉴权的请求不应打到上游，实际 %d 次", n)
	}
	resp, _ := doReq(t, http.MethodGet, h.ts.URL+"/api/chat-logs", nil, nil)
	if resp.StatusCode != 401 {
		t.Errorf("未设 Chat 密码时匿名读聊天记录应 401，实际 %d", resp.StatusCode)
	}
}

// Chat 会话只能用 Chat 页，进不了控制台。
func TestChatPasswordSessionScope(t *testing.T) {
	h := newHarness(t, 0, 1)
	resp, _ := doReq(t, http.MethodPost, h.ts.URL+"/api/chat/login", map[string]any{"password": "x"}, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("未设置 Chat 密码时 Chat 登录应被拒绝（400），实际 %d", resp.StatusCode)
	}
	resp, raw := doReq(t, http.MethodPost, h.ts.URL+"/api/settings",
		map[string]any{"chat_password": "abc"}, nil, h.cookie())
	if resp.StatusCode != 400 {
		t.Fatalf("过短的 Chat 密码应报错而不是静默忽略，实际 %d：%s", resp.StatusCode, raw)
	}
	resp, raw = doReq(t, http.MethodPost, h.ts.URL+"/api/settings",
		map[string]any{"chat_password": "chat-secret"}, nil, h.cookie())
	if resp.StatusCode != 200 {
		t.Fatalf("设置 Chat 密码失败：%d %s", resp.StatusCode, raw)
	}
	resp, _ = doReq(t, http.MethodPost, h.ts.URL+"/api/chat/login", map[string]any{"password": "chat-secret"}, nil)
	chat := findCookie(resp, chatSessionCookie)
	if resp.StatusCode != 200 || chat == nil || !chat.HttpOnly {
		t.Fatalf("Chat 登录应签发 HttpOnly 会话，实际 %d %+v", resp.StatusCode, chat)
	}
	resp, _ = doReq(t, http.MethodGet, h.ts.URL+"/api/chat-logs", nil, nil, chat)
	if resp.StatusCode != 200 {
		t.Errorf("Chat 会话应能读聊天记录，实际 %d", resp.StatusCode)
	}
	resp, _ = doReq(t, http.MethodGet, h.ts.URL+"/api/accounts", nil, nil, chat)
	if resp.StatusCode != 401 {
		t.Errorf("Chat 会话不应能读账号池，实际 %d", resp.StatusCode)
	}
	// 保存其它设置不能把 Chat 密码冲掉
	doReq(t, http.MethodPost, h.ts.URL+"/api/settings", map[string]any{"keepalive_ms": 300}, nil, h.cookie())
	if !h.store.HasChatPassword() {
		t.Fatal("保存其它设置不应清除 Chat 密码")
	}
	doReq(t, http.MethodPost, h.ts.URL+"/api/settings", map[string]any{"chat_password_clear": true}, nil, h.cookie())
	if h.store.HasChatPassword() {
		t.Fatal("chat_password_clear 应清除 Chat 密码")
	}
	resp, _ = doReq(t, http.MethodGet, h.ts.URL+"/api/chat-logs", nil, nil, chat)
	if resp.StatusCode != 401 {
		t.Errorf("清除 Chat 密码后旧 Chat 会话应失效，实际 %d", resp.StatusCode)
	}
}

// 初始密码状态：能登录，但除改密外的管理接口一律 403；改密必须验证当前密码。
func TestMustChangePasswordEnforced(t *testing.T) {
	store := newTempStore(t) // 未改过初始密码
	ts := httptestServer(t, New(store, hub.New(store), relay.BuildClient()))

	resp, _ := doReq(t, http.MethodPost, ts+"/api/login", map[string]any{"password": "admin123"}, nil)
	sess := findCookie(resp, cookieName)
	if resp.StatusCode != 200 || sess == nil {
		t.Fatalf("初始密码应能登录，实际 %d", resp.StatusCode)
	}
	resp, _ = doReq(t, http.MethodGet, ts+"/api/accounts", nil, nil, sess)
	if resp.StatusCode != 403 {
		t.Fatalf("未改初始密码时管理接口应 403，实际 %d", resp.StatusCode)
	}
	resp, _ = doReq(t, http.MethodPost, ts+"/api/password",
		map[string]any{"old_password": "wrong", "new_password": "brand-new-pass"}, nil, sess)
	if resp.StatusCode != 401 {
		t.Fatalf("当前密码错误时改密应 401，实际 %d", resp.StatusCode)
	}
	resp, raw := doReq(t, http.MethodPost, ts+"/api/password",
		map[string]any{"old_password": "admin123", "new_password": "brand-new-pass"}, nil, sess)
	if resp.StatusCode != 200 {
		t.Fatalf("改密失败：%d %s", resp.StatusCode, raw)
	}
	newSess := findCookie(resp, cookieName)
	resp, _ = doReq(t, http.MethodGet, ts+"/api/accounts", nil, nil, sess)
	if resp.StatusCode != 401 {
		t.Errorf("改密后旧会话应失效，实际 %d", resp.StatusCode)
	}
	resp, _ = doReq(t, http.MethodGet, ts+"/api/accounts", nil, nil, newSess)
	if resp.StatusCode != 200 {
		t.Errorf("改密后新会话应可用，实际 %d", resp.StatusCode)
	}
	// 退出登录：服务端作废，cookie 值再拿来用也不行
	doReq(t, http.MethodPost, ts+"/api/logout", nil, nil, newSess)
	resp, _ = doReq(t, http.MethodGet, ts+"/api/accounts", nil, nil, newSess)
	if resp.StatusCode != 401 {
		t.Errorf("退出后会话应在服务端失效，实际 %d", resp.StatusCode)
	}
}

func TestLoginRateLimited(t *testing.T) {
	h := newHarness(t, 0, 0)
	for i := 0; i < loginMaxFailures; i++ {
		doReq(t, http.MethodPost, h.ts.URL+"/api/login", map[string]any{"password": "wrong"}, nil)
	}
	resp, _ := doReq(t, http.MethodPost, h.ts.URL+"/api/login", map[string]any{"password": "admin123"}, nil)
	if resp.StatusCode != 429 {
		t.Fatalf("连续输错后应被锁定（429），实际 %d", resp.StatusCode)
	}
}

// 写接口必须同源：同一台 NAS 其它端口上的页面也不行。
func TestCrossOriginWriteRejected(t *testing.T) {
	h := newHarness(t, 0, 0)
	host := strings.TrimPrefix(h.ts.URL, "http://")
	hostname := host[:strings.LastIndex(host, ":")]
	payload := map[string]any{"keepalive_ms": 300}

	for _, origin := range []string{"http://evil.example", "http://" + hostname + ":5000", "null"} {
		resp, _ := doReq(t, http.MethodPost, h.ts.URL+"/api/settings", payload,
			map[string]string{"Origin": origin}, h.cookie())
		if resp.StatusCode != 403 {
			t.Errorf("Origin=%s 的写请求应 403，实际 %d", origin, resp.StatusCode)
		}
	}
	resp, _ := doReq(t, http.MethodPost, h.ts.URL+"/api/settings", payload,
		map[string]string{"Sec-Fetch-Site": "same-site"}, h.cookie())
	if resp.StatusCode != 403 {
		t.Errorf("Sec-Fetch-Site=same-site 且无 Origin 的写请求应 403，实际 %d", resp.StatusCode)
	}
	resp, raw := doReq(t, http.MethodPost, h.ts.URL+"/api/settings", payload,
		map[string]string{"Origin": h.ts.URL}, h.cookie())
	if resp.StatusCode != 200 {
		t.Errorf("同源写请求应放行，实际 %d：%s", resp.StatusCode, raw)
	}
	// 反向代理把 Host 改写成不带端口的主机名（NPM 默认）时，按主机名比对
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/api/settings", strings.NewReader(`{"keepalive_ms":300}`))
	req.Host = "hub.example.com"
	req.Header.Set("Origin", "https://hub.example.com:52325")
	req.AddCookie(h.cookie())
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Errorf("经反代（Host 无端口）的同主机请求应放行，实际 %d", r2.StatusCode)
	}
}

// /api/stats 与导出不得下发口令散列；「不含密钥」的导出必须真的打码。
func TestSecretsNotLeaked(t *testing.T) {
	h := newHarness(t, 0, 1)
	_, raw := doReq(t, http.MethodGet, h.ts.URL+"/api/stats", nil, nil, h.cookie())
	if strings.Contains(raw, h.store.SettingsSnapshot().AdminPasswordHash) {
		t.Fatal("/api/stats 不应包含管理员口令散列")
	}
	_, raw = doReq(t, http.MethodGet, h.ts.URL+"/api/export?redact=1", nil, nil, h.cookie())
	if strings.Contains(raw, "key-a") || strings.Contains(raw, h.apiKey) {
		t.Fatalf("redact=1 的导出不应含密钥明文：%s", raw)
	}
	_, raw = doReq(t, http.MethodGet, h.ts.URL+"/api/export", nil, nil, h.cookie())
	if !strings.Contains(raw, "key-a") {
		t.Fatal("完整导出应含上游密钥")
	}
	if strings.Contains(raw, h.store.SettingsSnapshot().AdminPasswordHash) {
		t.Fatal("完整导出也不应含管理员口令散列")
	}
	// 重复导入同一份导出，不应产生重复账号
	var exported map[string]any
	_ = json.Unmarshal([]byte(raw), &exported)
	before := len(h.store.AccountsSnapshot())
	doReq(t, http.MethodPost, h.ts.URL+"/api/import", exported, nil, h.cookie())
	if after := len(h.store.AccountsSnapshot()); after != before {
		t.Fatalf("重复导入不应新增账号：%d → %d", before, after)
	}
}

// /chat 在未登录时直接给页面（旧版本会无限重定向）。
func TestChatPageNoRedirectLoop(t *testing.T) {
	h := newHarness(t, 0, 0)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(h.ts.URL + "/chat")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/chat 应直接返回页面，实际 %d", resp.StatusCode)
	}
}

func TestMediaServedWithSafetyHeaders(t *testing.T) {
	if imageKind.allowsExt(".svg") {
		t.Fatal("图片白名单不应接受 SVG（可内嵌脚本）")
	}
	h := newHarness(t, 0, 0)
	name := strings.Repeat("a", 32) + ".png"
	if err := writeTestMedia(h, name); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(h.ts.URL + imageKind.route + name)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(csp, "sandbox") || !strings.Contains(csp, "default-src 'none'") ||
		strings.Contains(csp, "allow-scripts") {
		t.Fatalf("媒体响应应带 nosniff，且 CSP 禁止一切脚本，实际 %v", resp.Header)
	}
	// 浏览器直接打开 .mp4 时生成的内置播放页继承此策略：没有 allow-same-origin，
	// 播放页被放进匿名源，取视频的请求不算 'self'，视频停在 0:00 放不出来。
	if !strings.Contains(csp, "allow-same-origin") {
		t.Fatalf("CSP 必须带 allow-same-origin，否则直接打开的视频无法播放，实际 %q", csp)
	}
}

func httptestServer(t *testing.T, h http.Handler) string {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts.URL
}

func writeTestMedia(h *harness, name string) error {
	dir := h.srv.mediaDir(imageKind)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	png, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), png, 0o600)
}
