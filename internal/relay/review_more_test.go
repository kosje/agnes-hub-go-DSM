package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agneshub/internal/config"
)

func TestContinuationPreservesNewToolCall(t *testing.T) {
	base := []byte(`{"choices":[{"message":{"role":"assistant","content":"prefix"},"finish_reason":"length"}]}`)
	next := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"test","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	merged, ok := mergeChatResponse(base, next, "openai")
	if !ok || !strings.Contains(string(merged), "call_1") || !strings.Contains(string(merged), "prefix") {
		t.Fatalf("tool call lost: %s", merged)
	}
	mixed := []byte(`{"choices":[{"message":{"content":"partial","tool_calls":[{"function":{"arguments":"{"}}]},"finish_reason":"length"}]}`)
	if partialText(mixed, "openai") != "" {
		t.Fatal("incomplete tools must not be replayed as plain text")
	}
}

func TestGeminiContinuationKeepsNativePartsAndTools(t *testing.T) {
	base := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"one"}]},"finishReason":"MAX_TOKENS"}]}`)
	next := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"two"},{"functionCall":{"name":"test","args":{}}}]},"finishReason":"STOP"}]}`)
	merged, ok := mergeChatResponse(base, next, "gemini")
	if !ok {
		t.Fatal("merge failed")
	}
	var m map[string]any
	json.Unmarshal(merged, &m)
	content := m["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)
	if geminiText(content) != "onetwo" || len(content["parts"].([]any)) != 3 {
		t.Fatalf("native parts damaged: %s", merged)
	}
	request := appendContinuationMessages([]byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`), "one")
	json.Unmarshal(request, &m)
	if len(m["contents"].([]any)) != 3 {
		t.Fatalf("request not continued: %s", request)
	}
	if classifyResponse(next) != respOK {
		t.Fatal("Gemini tool response misclassified")
	}
	if _, ok := mergeChatResponse([]byte(`{"candidates":[null]}`), next, "gemini"); ok {
		t.Fatal("malformed candidate accepted")
	}
}

type reviewBrokenBody struct{}

func (*reviewBrokenBody) Read(p []byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (*reviewBrokenBody) Close() error               { return nil }

func TestReadAllCannotReportBrokenBodyAsSuccess(t *testing.T) {
	r := &Result{Status: 200, Stream: &reviewBrokenBody{}}
	body := r.ReadAll()
	if r.Status != 502 || r.ReadErr == nil || !strings.Contains(string(body), "upstream_read_error") {
		t.Fatalf("status=%d err=%v body=%s", r.Status, r.ReadErr, body)
	}
}

func TestInflightLivesUntilBodyClosed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) }))
	defer up.Close()
	_, h, _ := newRelayEnv(t, 1000, up.URL)
	r, err := Do(context.Background(), h, BuildClient(), textOpts(""))
	if err != nil {
		t.Fatal(err)
	}
	if h.InflightTotal() != 1 {
		t.Fatalf("body not counted as inflight: %d", h.InflightTotal())
	}
	r.ReadAll()
	if h.InflightTotal() != 0 {
		t.Fatal("body did not release inflight")
	}
}

func TestBudgetChargesEveryActualRoundOnce(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}],\"usage\":{\"neurons\":7}}\n\ndata: [DONE]\n\n")
				} else {
					io.WriteString(w, `{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}],"usage":{"neurons":7}}`)
				}
			}))
			defer up.Close()
			s, h, acc := newRelayEnv(t, 1000, up.URL)
			s.MutateAccount(acc[0].ID, func(a *config.Account) bool { a.DailyBudget = 100; return true })
			h.Reload()
			opts := textOpts("")
			opts.Stream = stream
			r, err := Do(context.Background(), h, BuildClient(), opts)
			if err != nil {
				t.Fatal(err)
			}
			r.ReadAll()
			r.Close()
			r.ReadAll()
			if got := s.AccountByID(acc[0].ID).BudgetUsedToday; got != 7 {
				t.Fatalf("used=%v", got)
			}
		})
	}
}

func TestZeroTimeoutAllowsHeadersAfterOldDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("real 31-second timeout regression")
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(31 * time.Second):
			io.WriteString(w, `{}`)
		case <-r.Context().Done():
		}
	}))
	defer up.Close()
	s, h, _ := newRelayEnv(t, 0, up.URL)
	s.UpdateSettings(func(st *config.Settings) { st.RetryMax = 0 })
	h.Reload()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	r, err := Do(ctx, h, BuildClient(), textOpts(""))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Status != 200 {
		t.Fatalf("zero timeout was replaced by 30 seconds: status=%d", r.Status)
	}
}

func TestNonStreamIdleBodyCannotHoldAccountForever(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer up.Close()
	_, h, _ := newRelayEnv(t, 80, up.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, err := Do(ctx, h, BuildClient(), textOpts(""))
	if err != nil {
		t.Fatal(err)
	}
	r.ReadAll()
	if r.Status != 502 || r.ReadErr == nil || h.InflightTotal() != 0 {
		t.Fatalf("status=%d err=%v inflight=%d", r.Status, r.ReadErr, h.InflightTotal())
	}
}

type oversizedBody struct{ remaining int }

func (b *oversizedBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > b.remaining {
		n = b.remaining
	}
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	b.remaining -= n
	return n, nil
}
func (*oversizedBody) Close() error { return nil }
func TestBufferedResponseLimit(t *testing.T) {
	r := &Result{Status: 200, Stream: &oversizedBody{remaining: maxBufferedResponse + 1}}
	r.ReadAll()
	if r.Status != 502 || r.ReadErr == nil {
		t.Fatal("oversized upstream body accepted")
	}
}
