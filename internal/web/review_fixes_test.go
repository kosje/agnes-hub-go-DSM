package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agneshub/internal/config"
	"agneshub/internal/intent"
	"agneshub/internal/relay"
)

func TestSettingsApplyScenarioAndAdvancedControls(t *testing.T) {
	h := newHarness(t, 0, 1)
	resp, raw := doReq(t, "POST", h.ts.URL+"/api/settings", map[string]any{"scenario": "multi"}, nil, h.cookie())
	if resp.StatusCode != 200 {
		t.Fatal(raw)
	}
	st := h.store.SettingsSnapshot()
	preset, _ := config.FindScenario("multi")
	if st.Scenario != "multi" || st.QueueMaxSize != preset.QueueSize || !st.TextTwoLevelQueue {
		t.Fatalf("preset not applied: %+v", st)
	}
	patch := map[string]any{"free_text_rpm": 13.5, "auto_scenario": true, "text_two_level_queue": false, "rate_limit_retry_max": 9, "rate_limit_wait_budget_ms": 1234, "stream_idle_timeout_ms": 2000, "continuation_max_rounds": 4, "soft_fail_retry_max": 5, "stream_auto_resume": false, "upstream_request_gzip": false, "strip_content_policy": false, "anthropic_prompt_cache": false, "routing_mode": "budget_first", "public_base_url": "https://hub.example.com:52325", "image_max_capacity": 0, "video_max_capacity": 0, "image_record_retention_days": 0}
	patch["raw_capture_enabled"] = true
	patch["usage_log_max_lines"] = 77
	resp, raw = doReq(t, "POST", h.ts.URL+"/api/settings", patch, nil, h.cookie())
	if resp.StatusCode != 200 {
		t.Fatal(raw)
	}
	st = h.store.SettingsSnapshot()
	if !st.RawCaptureEnabled || st.UsageLogMaxLines != 77 {
		t.Fatal("capture/log controls ignored")
	}
	if st.FreeTextRPM != 13.5 || !st.AutoScenario || st.TextTwoLevelQueue || st.RateLimitRetryMax != 9 || st.ContinuationMaxRounds != 4 || st.StreamAutoResume || st.SoftFailRetryMax != 5 || st.StreamIdleTimeoutMS != 2000 || st.RoutingMode != "budget_first" {
		t.Fatalf("controls ignored: %+v", st)
	}
	r, e := config.NewStore(h.store.Dir)
	if e != nil {
		t.Fatal(e)
	}
	st = r.SettingsSnapshot()
	if st.ImageMaxCapacity != 0 || st.VideoMaxCapacity != 0 || st.ImageRecordRetention != 0 || st.PublicBaseURL != "https://hub.example.com:52325" {
		t.Fatal("zero/URL settings did not persist")
	}
	for _, bad := range []map[string]any{{"scenario": "missing"}, {"public_base_url": "javascript:alert(1)"}, {"public_base_url": "https://host:99999"}, {"client_media_url": "bad"}, {"continuation_max_rounds": 1.5}, {"request_timeout_ms": -1}} {
		bad["free_text_rpm"] = 88
		resp, _ = doReq(t, "POST", h.ts.URL+"/api/settings", bad, nil, h.cookie())
		if resp.StatusCode != 400 || h.store.SettingsSnapshot().FreeTextRPM != 13.5 {
			t.Fatalf("invalid patch partially applied: %v", bad)
		}
	}
}

func TestFailedMediaResponsesAreNotChargedOrStored(t *testing.T) {
	for _, endpoint := range []string{"/v1/images/generations", "/v1/videos", "/api/chat/v1/images/generations", "/api/chat/v1/videos"} {
		t.Run(endpoint, func(t *testing.T) {
			h := newHarness(t, 0, 1)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"error":{"message":"failed"}}`) }))
			defer up.Close()
			id := h.store.AccountsSnapshot()[0].ID
			h.store.MutateAccount(id, func(a *config.Account) bool { a.BaseURL = up.URL; return true })
			h.hub.Reload()
			model := "agnes-image-2.5-flash"
			if strings.Contains(endpoint, "videos") {
				model = "agnes-video-2.5-flash"
			}
			status, out := h.post(endpoint, map[string]any{"model": model, "prompt": "test"})
			if status.StatusCode != 502 {
				t.Fatalf("status=%d out=%v", status.StatusCode, out)
			}
			if h.store.KeyByValue(h.apiKey).UsedTotal != 0 || len(h.store.JobsSnapshot()) != 0 || len(h.store.ImageJobsSnapshot()) != 0 {
				t.Fatal("failed generation was charged/stored")
			}
		})
	}
}

func TestStreamUsageAcrossRoundsAndAnthropic(t *testing.T) {
	stream := "data: {\"usage\":{\"total_tokens\":10}}\n\n: agnes-hub continuation round 1\n\ndata: {\"usage\":{\"total_tokens\":15}}\n\ndata: [DONE]\n\n"
	tail := &sseTail{w: io.Discard, cap: 16}
	for _, b := range []byte(stream) {
		tail.Write([]byte{b})
	}
	if tail.usage() != 25 {
		t.Fatalf("usage=%d", tail.usage())
	}
	tail = &sseTail{w: io.Discard, cap: 16}
	tail.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n"))
	if tail.usage() != 10 {
		t.Fatalf("Anthropic usage=%d", tail.usage())
	}
	for _, raw := range []string{`{"usage":{"input_tokens":7,"output_tokens":3}}`, `{"usageMetadata":{"totalTokenCount":10}}`} {
		if extractUsage([]byte(raw)) != 10 {
			t.Fatal(raw)
		}
	}
}

type failingStream struct{ sent bool }

func (b *failingStream) Read(p []byte) (int, error) {
	if b.sent {
		return 0, io.ErrUnexpectedEOF
	}
	b.sent = true
	return copy(p, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"), nil
}
func (*failingStream) Close() error { return nil }

func TestInterruptedStreamDoesNotCharge(t *testing.T) {
	h := newHarness(t, 0, 1)
	item := h.store.KeyByValue(h.apiKey)
	result := &relay.Result{Status: 200, Stream: &failingStream{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	h.srv.copyBilledStream(w, r, item, result, intent.Result{}, relay.Options{PoolClass: "text", Path: "/v1/chat/completions"}, w)
	if h.store.KeyByValue(h.apiKey).UsedTotal != 0 {
		t.Fatal("interrupted stream charged")
	}
	logs := h.store.TailUsageLive(1)
	if len(logs) != 1 || logs[0]["phase"] == "done" {
		t.Fatal("failure logged as success")
	}
}

type closingBody struct {
	io.Reader
	closed chan struct{}
	once   sync.Once
}

func (b *closingBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

type reviewTransport func(*http.Request) (*http.Response, error)

func (f reviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCanceledAsyncResultReleasesAccountSlot(t *testing.T) {
	h := newHarness(t, 0, 1)
	closed := make(chan struct{})
	h.srv.Client = &http.Client{Transport: reviewTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &closingBody{Reader: strings.NewReader("data: [DONE]\n\n"), closed: closed}}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.srv.relayAsync(ctx, relay.Options{PoolClass: "text", Path: "/v1/chat/completions", Stream: true})
	deadline := time.Now().Add(2 * time.Second)
	for h.hub.Metrics.RequestsOK.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.hub.Metrics.RequestsOK.Load() == 0 {
		t.Fatal("relay did not return headers")
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("abandoned body leaked")
	}
	for h.hub.InflightTotal() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.hub.InflightTotal() != 0 {
		t.Fatal("account inflight leaked")
	}
}

func TestOriginChecksSchemeAndExactPort(t *testing.T) {
	h := newHarness(t, 0, 0)
	for _, tc := range []struct {
		origin string
		want   bool
	}{{"http://hub.example.com", true}, {"http://hub.example.com:80", true}, {"http://hub.example.com:5001", false}, {"https://hub.example.com", false}, {"null", false}} {
		r := httptest.NewRequest("POST", "http://hub.example.com/api/settings", nil)
		r.Header.Set("Origin", tc.origin)
		if h.srv.sameOrigin(r) != tc.want {
			t.Fatalf("origin=%s", tc.origin)
		}
	}
}

func TestShutdownWaitsForFinalHandlerAccounting(t *testing.T) {
	h := newHarness(t, 0, 0)
	started, release := make(chan struct{}), make(chan struct{})
	h.srv.mux.HandleFunc("GET /review/drain", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		h.store.ChargeKey(h.apiKey, 7)
		h.store.AppendUsage(map[string]any{"ts": "2026-10-11 00:00:00", "stage": "last"})
		w.WriteHeader(200)
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, e := h.ts.Client().Get(h.ts.URL + "/review/drain")
		if e == nil {
			resp.Body.Close()
		}
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	shutdown := make(chan error, 1)
	go func() { shutdown <- h.ts.Config.Shutdown(ctx) }()
	close(release)
	if err := <-shutdown; err != nil {
		t.Fatal(err)
	}
	h.srv.WaitHandlers()
	<-done
	if err := h.store.FlushPending(); err != nil {
		t.Fatal(err)
	}
	reloaded, e := config.NewStore(h.store.Dir)
	if e != nil {
		t.Fatal(e)
	}
	if reloaded.KeyByValue(h.apiKey).UsedTotal != 1 || len(reloaded.TailUsage(1)) != 1 {
		t.Fatal("final handler state lost")
	}
}

func TestStreamErrorFrameSurvivesLongOutput(t *testing.T) {
	tail := &sseTail{w: io.Discard, cap: 16}
	tail.Write([]byte("data: {\"error\":{\"message\":\"bad\"}}\n\n"))
	tail.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", 100000) + "\"}}]}\n\n"))
	tail.finishObserver()
	if !tail.failedFrame {
		t.Fatal("earlier error forgotten")
	}
}

func TestAccountDailyBudgetCanBeCreatedAndEdited(t *testing.T) {
	h := newHarness(t, 0, 0)
	resp, raw := doReq(t, "POST", h.ts.URL+"/api/accounts", map[string]any{"api_key": "budget-fixture-key", "access_type": "tokenplan", "daily_budget": 100, "budget_unit": "neurons"}, nil, h.cookie())
	if resp.StatusCode != 200 {
		t.Fatal(raw)
	}
	accounts := h.store.AccountsSnapshot()
	if len(accounts) != 1 || accounts[0].DailyBudget != 100 {
		t.Fatal("budget not created")
	}
	id := accounts[0].ID
	resp, raw = doReq(t, "PATCH", h.ts.URL+"/api/accounts/"+id, map[string]any{"daily_budget": 150}, nil, h.cookie())
	if resp.StatusCode != 200 || h.store.AccountByID(id).DailyBudget != 150 {
		t.Fatal(raw)
	}
	resp, _ = doReq(t, "PATCH", h.ts.URL+"/api/accounts/"+id, map[string]any{"daily_budget": -1}, nil, h.cookie())
	if resp.StatusCode != 400 || h.store.AccountByID(id).DailyBudget != 150 {
		t.Fatal("invalid budget applied")
	}
	resp, raw = doReq(t, "GET", h.ts.URL+"/api/accounts", nil, nil, h.cookie())
	if resp.StatusCode != 200 || !strings.Contains(raw, "daily_budget") {
		t.Fatal("budget omitted from account response")
	}
}

func TestProxyCannotUseLoopbackMetricsBypass(t *testing.T) {
	h := newHarness(t, 0, 0)
	r := httptest.NewRequest("GET", "http://127.0.0.1:4142/metrics", nil)
	r.RemoteAddr = "127.0.0.1:8080"
	if !h.srv.metricsAllowed(r) {
		t.Fatal("direct local scrape blocked")
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	if h.srv.metricsAllowed(r) {
		t.Fatal("proxy request bypassed authentication")
	}
	r.Header.Del("X-Forwarded-For")
	r.Host = "public.example.com"
	if h.srv.metricsAllowed(r) {
		t.Fatal("public proxy host bypassed authentication")
	}
}

func TestInvalidImagePayloadsRejected(t *testing.T) {
	for _, raw := range []string{`{"data":[{"b64_json":"aGVsbG8="}]}`, `{"data":[{"url":"javascript:alert(1)"}]}`, `{"data":[]}`} {
		if validImageResult([]byte(raw)) {
			t.Fatal(raw)
		}
	}
}
