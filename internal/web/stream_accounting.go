package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"agneshub/internal/config"
	"agneshub/internal/intent"
	"agneshub/internal/relay"
)

func (t *sseTail) observe(p []byte) {
	t.lineBuf = append(t.lineBuf, p...)
	for {
		line, rest, ok := bytes.Cut(t.lineBuf, []byte{'\n'})
		if !ok {
			break
		}
		t.lineBuf = rest
		text := strings.TrimSuffix(string(line), "\r")
		if strings.HasPrefix(text, ": agnes-hub continuation round ") {
			t.completedTokens += t.segmentTokens
			t.segmentTokens, t.inputTokens, t.outputTokens = 0, 0, 0
		} else if text == "" {
			t.observeEvent()
		} else if strings.HasPrefix(text, "data:") {
			t.frameData = append(t.frameData, strings.TrimSpace(strings.TrimPrefix(text, "data:")))
			t.frameBytes += len(text)
		}
		if t.frameBytes > 4<<20 {
			t.failedFrame = true
			t.frameData = nil
			t.frameBytes = 0
		}
	}
	if len(t.lineBuf) > 4<<20 {
		t.failedFrame = true
		t.lineBuf = nil
	}
	t.observed = true
}

func (t *sseTail) observeEvent() {
	if len(t.frameData) == 0 {
		return
	}
	raw := []byte(strings.Join(t.frameData, "\n"))
	t.frameData = nil
	t.frameBytes = 0
	if bytes.Equal(raw, []byte("[DONE]")) {
		return
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		t.failedFrame = true
		return
	}
	if m["error"] != nil || m["type"] == "error" {
		t.failedFrame = true
	}
	u, _ := m["usage"].(map[string]any)
	if message, ok := m["message"].(map[string]any); ok {
		if nested, ok := message["usage"].(map[string]any); ok {
			u = nested
		}
	}
	for _, key := range []string{"prompt_tokens", "input_tokens"} {
		if v, ok := u[key].(float64); ok && v >= 0 {
			t.inputTokens = int64(v)
		}
	}
	for _, key := range []string{"completion_tokens", "output_tokens"} {
		if v, ok := u[key].(float64); ok && v >= 0 {
			t.outputTokens = int64(v)
		}
	}
	tokens := extractUsage(raw)
	if combined := t.inputTokens + t.outputTokens; combined > tokens {
		tokens = combined
	}
	if tokens > t.segmentTokens {
		t.segmentTokens = tokens
	}
}

func (t *sseTail) finishObserver() {
	if !t.observed {
		t.observe(t.buf)
	}
	if len(t.lineBuf) > 0 {
		t.observe([]byte("\n"))
	}
	t.observeEvent()
}

func (s *Server) copyBilledStream(w http.ResponseWriter, r *http.Request, item *config.DownstreamKey,
	result *relay.Result, decision intent.Result, opts relay.Options, flusher http.Flusher) {
	defer result.Close()
	tail := &sseTail{w: &flushWriter{w: w, f: flusher}, cap: 1 << 16}
	var source io.Reader = result.Stream
	if source == nil {
		source = bytes.NewReader(result.Body)
	}
	_, err := io.Copy(tail, source)
	tail.finishObserver()
	if err != nil || r.Context().Err() != nil || tail.failedFrame {
		message := "流式响应未完整完成"
		if r.Context().Err() != nil {
			message = "客户端已断开"
		}
		s.logUsageError(item, decision, opts.PoolClass, opts.Path, true, result.Account, result.WaitMS, result.Attempts, decision.Prompt.Text, 502, message)
		return
	}
	s.Store.ChargeKey(item.Key, tail.usage())
	s.logUsageFull(item, decision, opts.PoolClass, opts.Path, true, result.Account, result.WaitMS, result.Attempts, decision.Prompt.Text)
}

func usageNumber(m map[string]any, key string) int64 {
	if n, ok := m[key].(float64); ok && n > 0 {
		return int64(n)
	}
	return 0
}
