package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agneshub/internal/config"
	"agneshub/internal/hub"
)

// newRelayEnv 建一个只含给定上游账号的 store + hub，节拍放宽到不影响测试。
func newRelayEnv(t *testing.T, timeoutMS int, upstreams ...string) (*config.Store, *hub.Hub, []*config.Account) {
	t.Helper()
	store, err := config.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = store.UpdateSettings(func(s *config.Settings) {
		s.RequestTimeoutMS = timeoutMS
		s.RetryBaseBackoffMS = 1
		s.RetryMaxBackoffMS = 1
		s.QueueMaxWaitMS = 5000
		s.CalibrationEnabled = false
	})
	var accs []*config.Account
	for i, u := range upstreams {
		a := store.AddAccount("acc-"+string(rune('a'+i)), "key-"+string(rune('a'+i)), "tokenplan", u+"/v1",
			&config.ModelManifest{Text: []string{"agnes-2.5-flash"}})
		accs = append(accs, a)
	}
	return store, hub.New(store), accs
}

func textOpts(session string) Options {
	return Options{SessionKey: session, PoolClass: "text", Method: http.MethodPost,
		Path: "/v1/chat/completions", Body: []byte(`{"model":"agnes-2.5-flash"}`), Idempotent: true}
}

// 超时只管「到响应头为止」：响应头及时返回后，正文慢慢流也不能被截断。
// 旧实现用 context.WithTimeout 包住整个请求，超过 30 s 的流式回答会被拦腰截断。
func TestTimeoutDoesNotCutSlowStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			_, _ = io.WriteString(w, "data: chunk\n\n")
			f.Flush()
			time.Sleep(60 * time.Millisecond) // 总计约 300 ms，远超 100 ms 的超时
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	_, h, _ := newRelayEnv(t, 100, up.URL)

	res, err := Do(context.Background(), h, BuildClient(), textOpts(""))
	if err != nil {
		t.Fatal(err)
	}
	body := string(res.ReadAll())
	if res.Status != 200 || !strings.Contains(body, "[DONE]") || strings.Count(body, "chunk") != 5 {
		t.Fatalf("慢速流被截断了：status=%d body=%q", res.Status, body)
	}
}

// 响应头迟迟不来才算超时。
func TestTimeoutAppliesToResponseHeaders(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer up.Close()
	_, h, _ := newRelayEnv(t, 100, up.URL)

	start := time.Now()
	res, err := Do(context.Background(), h, BuildClient(), textOpts(""))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusBadGateway {
		t.Fatalf("响应头超时应回 502，实际 %d", res.Status)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("超时没有生效，耗时 %s", time.Since(start))
	}
}

// 402 的账号要冷却并换号；之后绑定在它上面的会话也不能再被派回去。
func TestQuotaExhaustedFailsOverAndStaysAway(t *testing.T) {
	var brokeHits atomic.Int64
	broke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		brokeHits.Add(1)
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"error":{"message":"insufficient balance"}}`)
	}))
	defer broke.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer ok.Close()

	store, h, accs := newRelayEnv(t, 5000, broke.URL, ok.URL)
	// 会话先绑定在额度耗尽的账号上
	store.Bind("sess-1", accs[0].ID)

	for i := 0; i < 3; i++ {
		opts := textOpts("sess-1")
		opts.Idempotent = false // 生图/视频提交同样适用：402 是受理前拒绝
		res, err := Do(context.Background(), h, BuildClient(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != 200 {
			t.Fatalf("第 %d 次：402 后应换号成功，实际 %d %s", i+1, res.Status, res.ReadAll())
		}
		res.Close()
	}
	if n := brokeHits.Load(); n != 1 {
		t.Fatalf("额度耗尽的账号应只被打一次随后冷却，实际 %d 次", n)
	}
	if h.PenaltyRemaining(accs[0].ID) <= 0 {
		t.Fatal("402 后账号应处于冷却期")
	}
}

// 客户端断开不是上游的错：不记账号错误、不换号重试。
func TestClientCancelIsNotAnUpstreamError(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer up.Close()
	store, h, accs := newRelayEnv(t, 5000, up.URL, up.URL, up.URL)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := Do(ctx, h, BuildClient(), textOpts(""))
	if err == nil {
		t.Fatal("客户端取消后应返回错误")
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("客户端取消后不应换号重试，上游被打 %d 次", n)
	}
	for _, a := range accs {
		if got := store.AccountByID(a.ID); got.Stats.Errors != 0 {
			t.Fatalf("客户端取消不应记为账号错误：%s errors=%d", a.Name, got.Stats.Errors)
		}
	}
}
