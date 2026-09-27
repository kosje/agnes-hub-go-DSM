package web

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"agneshub/internal/config"
	"agneshub/internal/hub"
	"agneshub/internal/relay"
)

func bearerGet(t *testing.T, url, key string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

// 视频任务只有提交它的那把密钥能查；/agnesapi 不再替人查不认识的 video_id。
func TestVideoJobOwnership(t *testing.T) {
	h := newHarness(t, 0, 1)
	_, submit := h.post("/v1/videos", map[string]any{"model": "agnes-video-2.5-flash", "prompt": "海浪"})
	jobID, _ := submit["job_id"].(string)
	videoID, _ := submit["video_id"].(string)
	if jobID == "" || videoID == "" {
		t.Fatalf("提交应返回 job_id 与 video_id：%v", submit)
	}
	other := h.store.AddKey("other", []string{"*"}, 0, 0, "")
	textOnly := h.store.AddKey("text-only", []string{"text"}, 0, 0, "")

	if resp, raw := bearerGet(t, h.ts.URL+"/v1/videos/"+jobID, h.apiKey); resp.StatusCode != 200 {
		t.Fatalf("提交者本人应能查询，实际 %d：%s", resp.StatusCode, raw)
	}
	if resp, _ := bearerGet(t, h.ts.URL+"/v1/videos/"+jobID, other.Key); resp.StatusCode != 404 {
		t.Errorf("别的密钥查询他人任务应 404，实际 %d", resp.StatusCode)
	}
	if resp, _ := bearerGet(t, h.ts.URL+"/v1/videos/"+jobID, textOnly.Key); resp.StatusCode != 403 {
		t.Errorf("不允许 video 池的密钥应 403，实际 %d", resp.StatusCode)
	}

	before := h.mock.hits("/agnesapi")
	if resp, _ := bearerGet(t, h.ts.URL+"/agnesapi?video_id=video_never_submitted_here", h.apiKey); resp.StatusCode != 404 {
		t.Errorf("/agnesapi 查不认识的 video_id 应 404，实际 %d", resp.StatusCode)
	}
	if resp, _ := bearerGet(t, h.ts.URL+"/agnesapi?video_id="+videoID, other.Key); resp.StatusCode != 404 {
		t.Errorf("/agnesapi 查他人任务应 404，实际 %d", resp.StatusCode)
	}
	if n := h.mock.hits("/agnesapi"); n != before {
		t.Fatalf("被拒绝的查询不应打到上游，实际多打了 %d 次", n-before)
	}
	if resp, raw := bearerGet(t, h.ts.URL+"/agnesapi?video_id="+videoID+"&extra=injected", h.apiKey); resp.StatusCode != 200 {
		t.Fatalf("/agnesapi 查自己的任务应 200，实际 %d：%s", resp.StatusCode, raw)
	}
}

// 网页创建的视频任务不能被任何下游密钥查到。
func TestWebVideoJobNotVisibleToKeys(t *testing.T) {
	h := newHarness(t, 0, 1)
	_, submit := h.post("/api/chat/v1/videos", map[string]any{"model": "agnes-auto", "prompt": "山谷"})
	jobID, _ := submit["job_id"].(string)
	if jobID == "" {
		t.Fatalf("提交应返回 job_id：%v", submit)
	}
	if resp, _ := bearerGet(t, h.ts.URL+"/v1/videos/"+jobID, h.apiKey); resp.StatusCode != 404 {
		t.Fatalf("网页任务不应被下游密钥查到，实际 %d", resp.StatusCode)
	}
}

// 生图失败不计费、不进记录（旧实现连上游 502 都扣额度并记一条 completed）。
func TestFailedImageNotCharged(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer up.Close()
	store := newTempStore(t)
	store.AddAccount("img", "k", "free", up.URL+"/v1",
		&config.ModelManifest{Image: []string{"agnes-image-2.5-flash"}})
	key := store.AddKey("k", []string{"*"}, 0, 5, "")
	ts := httptest.NewServer(New(store, hub.New(store), relay.BuildClient()))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/images/generations",
		strings.NewReader(`{"model":"agnes-image-2.5-flash","prompt":"猫"}`))
	req.Header.Set("Authorization", "Bearer "+key.Key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("上游 500 时应返回错误，实际 %d", resp.StatusCode)
	}
	if got := store.KeyByValue(key.Key).UsedTotal; got != 0 {
		t.Errorf("失败的生图不应计费，实际已用 %d", got)
	}
	if n := len(store.ImageJobsSnapshot()); n != 0 {
		t.Errorf("失败的生图不应进图片记录，实际 %d 条", n)
	}
}

// 额度只剩 1 时，并发请求只能有 1 个通过额度检查。
func TestQuotaReservationUnderConcurrency(t *testing.T) {
	store := newTempStore(t)
	key := store.AddKey("k", []string{"*"}, 1, 0, "")
	var mu sync.Mutex
	passed := 0
	var releases []func()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, msg := store.ReserveQuota(key.Key)
			if msg == "" {
				mu.Lock()
				passed++
				releases = append(releases, release)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if passed != 1 {
		t.Fatalf("每日额度 1 时只应有 1 个请求通过，实际 %d", passed)
	}
	// 失败请求释放名额后可以再试；成功请求计费后额度用尽
	releases[0]()
	if _, msg := store.ReserveQuota(key.Key); msg != "" {
		t.Fatalf("释放后应能再次占用：%s", msg)
	}
}

// 回取上游产出：没有任何账号信任的内网 / 本机地址一律拒绝。
func TestMediaFetchRefusesPrivateAddresses(t *testing.T) {
	var hits int
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nsecret"))
	}))
	defer internal.Close()

	s := &Server{Store: newTempStore(t)} // 没有账号：127.0.0.1 不受信任
	raw := internal.URL + "/router-admin.png"
	if got := s.localizeImageURL(context.Background(), raw, ""); got != raw {
		t.Fatalf("内网地址不应被回取，实际返回 %q", got)
	}
	if hits != 0 {
		t.Fatalf("拨号阶段就应拒绝，不该有请求到达内网服务，实际 %d 次", hits)
	}
	if entries, _ := os.ReadDir(s.mediaDir(imageKind)); len(entries) != 0 {
		t.Fatalf("不应落盘任何文件，实际 %d 个", len(entries))
	}
}

func TestIsPublicIP(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8": true, "1.1.1.1": true, "2606:4700::1111": true,
		"198.18.8.188": true, // Clash fake-IP 段：必须放行
		"127.0.0.1":    false, "10.1.2.3": false, "172.16.0.9": false, "192.168.1.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::1": false,
		"fe80::1": false, "fd00::1": false, "::ffff:192.168.1.1": false,
	}
	for s, want := range cases {
		if got := isPublicIP(net.ParseIP(s)); got != want {
			t.Errorf("isPublicIP(%s) = %v，应为 %v", s, got, want)
		}
	}
}

// /metrics 不再对任何人开放；账号名里的特殊字符不能破坏输出格式。
func TestMetricsRequiresAuth(t *testing.T) {
	h := newHarness(t, 0, 1)
	_ = h.store.MutateAccount(h.store.AccountsSnapshot()[0].ID, func(a *config.Account) bool {
		a.Name = "evil\"} 1\nfake_metric{x=\"y"
		return true
	})
	remote := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	remote.RemoteAddr = "192.168.1.50:51234"
	rec := httptest.NewRecorder()
	h.srv.handleMetrics(rec, remote)
	if rec.Code != 401 {
		t.Fatalf("局域网匿名访问 /metrics 应 401，实际 %d", rec.Code)
	}
	withKey := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	withKey.RemoteAddr = "192.168.1.50:51234"
	withKey.Header.Set("Authorization", "Bearer "+h.apiKey)
	rec = httptest.NewRecorder()
	h.srv.handleMetrics(rec, withKey)
	if rec.Code != 200 {
		t.Fatalf("带下游密钥应能抓取，实际 %d", rec.Code)
	}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "fake_metric") {
			t.Fatalf("账号名注入了伪造的指标行：%q", line)
		}
	}
}

// 清空密钥的池列表不再悄悄变成「允许全部池」。
func TestKeyClassesCannotBeCleared(t *testing.T) {
	h := newHarness(t, 0, 0)
	resp, _ := doReq(t, http.MethodPatch, h.ts.URL+"/api/keys",
		map[string]any{"key": h.apiKey, "classes": []any{}}, nil, h.cookie())
	if resp.StatusCode != 400 {
		t.Fatalf("清空池列表应被拒绝（400），实际 %d", resp.StatusCode)
	}
	resp, _ = doReq(t, http.MethodPatch, h.ts.URL+"/api/keys",
		map[string]any{"key": h.apiKey, "classes": []any{"text", "bogus"}}, nil, h.cookie())
	if resp.StatusCode != 400 {
		t.Fatalf("未知池名应被拒绝（400），实际 %d", resp.StatusCode)
	}
}

// 下游密钥调用方拿不到承载账号的名称与 ID；管理员会话照常能看到（排障用）。
func TestAccountHeadersOnlyForAdmin(t *testing.T) {
	h := newHarness(t, 0, 1)
	body := `{"model":"agnes-2.5-flash","messages":[{"role":"user","content":"hi"}]}`
	keyOnly, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/v1/chat/completions", strings.NewReader(body))
	keyOnly.Header.Set("Authorization", "Bearer "+h.apiKey)
	resp, err := http.DefaultClient.Do(keyOnly)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("请求应成功，实际 %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Agnes-Hub-Account") != "" || resp.Header.Get("X-Agnes-Hub-Account-Id") != "" {
		t.Fatalf("下游密钥调用方不应看到账号信息，实际 %q / %q",
			resp.Header.Get("X-Agnes-Hub-Account"), resp.Header.Get("X-Agnes-Hub-Account-Id"))
	}
	if resp.Header.Get("X-Agnes-Hub-Model") == "" {
		t.Error("模型等非敏感的排障头应保留")
	}
}

// 连不上上游时，回给调用方的错误里不能带上游地址。
func TestUpstreamConnectErrorHidesAddress(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // 端口立刻失效：连接被拒绝
	store := newTempStore(t)
	store.AddAccount("x", "k", "free", deadURL+"/v1", &config.ModelManifest{Text: []string{"agnes-2.5-flash"}})
	_ = store.UpdateSettings(func(s *config.Settings) { s.RetryMax = 0 })
	key := store.AddKey("k", []string{"*"}, 0, 0, "")
	ts := httptest.NewServer(New(store, hub.New(store), relay.BuildClient()))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"agnes-2.5-flash","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key.Key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	host := strings.TrimPrefix(deadURL, "http://")
	if resp.StatusCode < 500 || !strings.Contains(string(raw), "上游连接失败") {
		t.Fatalf("应回可读的上游错误，实际 %d：%s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), host) || strings.Contains(string(raw), "/v1/chat/completions") {
		t.Fatalf("错误信息泄露了上游地址：%s", raw)
	}
	// 完整原文仍记在账号上，管理员看得到
	if last := store.AccountsSnapshot()[0].Stats.LastError; !strings.Contains(last, host) {
		t.Errorf("账号的最近错误应保留完整原文，实际 %q", last)
	}
}

// 控制台手动停用会清掉熔断的复活计划，否则冷却到期账号会被自动拉起来。
func TestConsoleDisableClearsBreakerPlan(t *testing.T) {
	h := newHarness(t, 0, 1)
	id := h.store.AccountsSnapshot()[0].ID
	h.hub.OnAuthFailure(h.store.AccountByID(id), "401")
	if h.store.AccountByID(id).ReviveAt == 0 {
		t.Fatal("熔断后应有复活计划")
	}
	resp, raw := doReq(t, http.MethodPatch, h.ts.URL+"/api/accounts/"+id,
		map[string]any{"enabled": false}, nil, h.cookie())
	if resp.StatusCode != 200 {
		t.Fatalf("PATCH 失败：%d %s", resp.StatusCode, raw)
	}
	if got := h.store.AccountByID(id); got.ReviveAt != 0 || got.BreakerTrips != 0 {
		t.Fatalf("手动停用应清掉复活计划，实际 revive_at=%v trips=%d", got.ReviveAt, got.BreakerTrips)
	}
}
