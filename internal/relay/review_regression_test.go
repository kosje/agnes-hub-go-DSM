package relay

import (
	"agneshub/internal/config"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuditFinishEventDelimiter(t *testing.T) {
	input := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	first := &Result{Stream: io.NopCloser(strings.NewReader(input))}
	c := &continuationStream{cur: first, f: newSSEFilter(first.Stream), rounds: 0}
	b, _ := io.ReadAll(c)
	out := string(b)
	for _, event := range strings.Split(out, "\n\n") {
		if strings.Contains(event, "finish_reason") && strings.Contains(event, "[DONE]") {
			t.Fatalf("finish JSON and [DONE] merged into one SSE event: %q", event)
		}
	}
}
func TestAuditSingleConcurrencyContinuation(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"part\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()
	h, s := newTestHubForRelay(t, func(c *config.Settings) { c.PacingWindowSec = 0.01; c.TextTwoLevelQueue = false })
	addTestUpstream(t, h, s, up.URL)
	a := s.AccountsSnapshot()[0]
	s.MutateAccount(a.ID, func(a *config.Account) bool { a.MaxConcurrency = 1; return true })
	h.Reload()
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	res, err := Do(ctx, h, up.Client(), chatOptions([]byte(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`), true))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	io.ReadAll(res.Stream)
	if calls.Load() < 2 {
		t.Fatalf("continuation never sent: calls=%d ctx=%v", calls.Load(), ctx.Err())
	}
}
func TestAuditThirdRoundKeepsAllContext(t *testing.T) {
	var calls atomic.Int64
	var third atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		n := calls.Add(1)
		if n == 3 {
			third.Store(string(raw))
		}
		reason := "length"
		if n == 3 {
			reason = "stop"
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"part%d\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"%s\"}]}\n\ndata: [DONE]\n\n", n, reason)
	}))
	defer up.Close()
	h, s := newTestHubForRelay(t, func(c *config.Settings) { c.PacingWindowSec = 0.01; c.TextTwoLevelQueue = false })
	addTestUpstream(t, h, s, up.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := Do(ctx, h, up.Client(), chatOptions([]byte(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`), true))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	io.ReadAll(res.Stream)
	thirdBody, _ := third.Load().(string)
	if !strings.Contains(thirdBody, "part1") || !strings.Contains(thirdBody, "part2") {
		t.Fatalf("third request lost prior output: %s", thirdBody)
	}
}

type auditBrokenReader struct{ sent bool }

func (r *auditBrokenReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, errors.New("transport reset")
	}
	r.sent = true
	return copy(p, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"), nil
}
func TestAuditFilterPreservesTransportError(t *testing.T) {
	f := newSSEFilter(&auditBrokenReader{})
	_, err := io.ReadAll(f)
	if err == nil {
		t.Fatal("transport error swallowed as EOF")
	}
}
func TestAuditAnthropicToolUseIsNotEmpty(t *testing.T) {
	body := []byte(`{"type":"message","content":[{"type":"tool_use","id":"toolu_1","name":"weather","input":{"city":"Paris"}}],"stop_reason":"tool_use"}`)
	if classifyResponse(body) != respOK {
		t.Fatalf("valid tool call classified as %s", classifyResponse(body))
	}
}
func TestAuditNonOpenAIStreamNoSyntheticOpenAI(t *testing.T) {
	input := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	first := &Result{Stream: io.NopCloser(strings.NewReader(input))}
	c := &continuationStream{cur: first, f: newSSEFilter(first.Stream), rounds: 0}
	raw, _ := io.ReadAll(c)
	if strings.Contains(string(raw), "chat.completion.chunk") || strings.Contains(string(raw), "[DONE]") {
		t.Fatalf("non-OpenAI stream mutated: %s", raw)
	}
	if string(raw) != input {
		t.Fatalf("native events lost: %q", raw)
	}
}

// Parse actual SSE events instead of testing substrings; clients join multiple
// data lines within one event before attempting to decode JSON.
func readChatEvents(t *testing.T, out string) (content string, finishes, done int) {
	t.Helper()
	for _, event := range strings.Split(out, "\n\n") {
		var payload []string
		for _, line := range strings.Split(event, "\n") {
			if strings.HasPrefix(line, "data:") {
				payload = append(payload, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(payload) == 0 {
			continue
		}
		data := strings.Join(payload, "\n")
		if data == "[DONE]" {
			done++
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("invalid SSE JSON: %s: %v", data, err)
		}
		choices, _ := chunk["choices"].([]any)
		for _, item := range choices {
			choice, _ := item.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			content += anyContentToString(delta["content"])
			if reason, _ := choice["finish_reason"].(string); reason != "" {
				finishes++
			}
		}
	}
	return
}

func TestSSEFinalDeltaAndSmallBuffers(t *testing.T) {
	for _, size := range []int{1, 3, 64, 32768} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			input := "data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"world\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
			first := &Result{Stream: io.NopCloser(strings.NewReader(input))}
			c := &continuationStream{cur: first, f: newSSEFilter(first.Stream)}
			defer c.Close()
			var out strings.Builder
			buf := make([]byte, size)
			for {
				n, err := c.Read(buf)
				out.Write(buf[:n])
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			content, finishes, done := readChatEvents(t, out.String())
			if content != "hello world" || finishes != 1 || done != 1 {
				t.Fatalf("content=%q finishes=%d done=%d", content, finishes, done)
			}
			if n, err := c.Read(buf); n != 0 || err != io.EOF {
				t.Fatalf("tail repeated: n=%d err=%v", n, err)
			}
		})
	}
}

func TestSSEUnterminatedNativeEventKeepsBufferedData(t *testing.T) {
	input := "event: message_stop\ndata: {\"type\":\"message_stop\"}"
	f := newSSEFilter(strings.NewReader(input))
	raw, err := io.ReadAll(f)
	if err != nil || string(raw) != input+"\n" {
		t.Fatalf("raw=%q err=%v", raw, err)
	}
}

func TestSSEUnterminatedChatEventStillHasSeparateFinish(t *testing.T) {
	input := "data: {\"choices\":[{\"delta\":{\"content\":\"last\"}}]}"
	first := &Result{Stream: io.NopCloser(strings.NewReader(input))}
	c := &continuationStream{cur: first, f: newSSEFilter(first.Stream)}
	defer c.Close()
	raw, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	content, finishes, done := readChatEvents(t, string(raw))
	if content != "last" || finishes != 1 || done != 1 {
		t.Fatalf("content=%q finishes=%d done=%d", content, finishes, done)
	}
}

func TestContinuationDisableResumeDoesNotDisableLengthContinuation(t *testing.T) {
	c := &continuationStream{rounds: 2, resume: false, f: &sseFilter{sawOpenAI: true, finishReason: "length"}}
	if !c.shouldResume() {
		t.Fatal("length continuation must use continuation_max_rounds independently")
	}
	c.f.finishReason = ""
	if c.shouldResume() {
		t.Fatal("implicit recovery must respect stream_auto_resume=false")
	}
}

func TestSSEToolDeltaWithFinishIsPreserved(t *testing.T) {
	input := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	first := &Result{Stream: io.NopCloser(strings.NewReader(input))}
	c := &continuationStream{cur: first, f: newSSEFilter(first.Stream), resume: true, rounds: 2}
	defer c.Close()
	raw, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "arguments") != 1 {
		t.Fatalf("tool delta lost or duplicated: %s", raw)
	}
	_, finishes, done := readChatEvents(t, string(raw))
	if finishes != 1 || done != 1 {
		t.Fatalf("finish=%d done=%d", finishes, done)
	}
}
