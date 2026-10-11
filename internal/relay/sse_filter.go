package relay

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// sseFilter operates on complete SSE events, including their blank-line boundary.
// Unknown protocols pass through; only OpenAI chat events are eligible for recovery.
type sseFilter struct {
	sc           *bufio.Scanner
	pending      []byte
	eof          bool
	pass         bool
	finish       []byte
	finishReason string
	partial      strings.Builder
	sawOpenAI    bool
	hasTools     bool
	readErr      error
	upstreamErr  bool
	blocked      bool
}

func newSSEFilter(r io.Reader) *sseFilter {
	f := &sseFilter{sc: bufio.NewScanner(r)}
	f.sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return f
}

func (f *sseFilter) fill() {
	for !f.eof && len(f.pending) == 0 {
		var lines []string
		for f.sc.Scan() {
			line := f.sc.Text()
			lines = append(lines, line)
			if line == "" {
				break
			}
		}
		if len(lines) == 0 || lines[len(lines)-1] != "" {
			f.eof = true
			f.readErr = f.sc.Err()
		}
		if len(lines) > 0 {
			f.filterEvent(lines)
		}
	}
}

func (f *sseFilter) filterEvent(lines []string) {
	event := []byte(strings.Join(lines, "\n") + "\n")
	if f.pass {
		f.pending = append(f.pending, event...)
		return
	}
	var data []string
	namedEvent := false
	for _, line := range lines {
		if strings.HasPrefix(line, "event:") {
			namedEvent = true
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if len(data) == 0 {
		f.pending = append(f.pending, event...)
		return
	}
	payload := strings.Join(data, "\n")
	if strings.TrimSpace(payload) == "[DONE]" && !namedEvent {
		f.sawOpenAI = true
		return
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		f.pending = append(f.pending, event...)
		return
	}
	choices, openAI := chunk["choices"].([]any)
	// Anthropic/Gemini/Responses events must retain their native protocol.
	if !openAI && (namedEvent || chunk["type"] != nil || chunk["candidates"] != nil) {
		f.pass = true
		f.pending = append(f.pending, event...)
		return
	}
	if ev, ok := chunk["error"]; ok && ev != nil {
		f.sawOpenAI = true
		f.upstreamErr = true
		return
	}
	if !openAI {
		if !f.sawOpenAI || chunk["usage"] == nil {
			f.pass = true
		}
		f.pending = append(f.pending, event...)
		return
	}
	if len(choices) > 1 {
		// Multiple independent choices cannot share one continuation context.
		f.pass = true
		f.pending = append(f.pending, event...)
		return
	}
	f.sawOpenAI = true
	// Recover a complete final JSON chunk even if the transport omitted its
	// blank delimiter. The synthesized finish event must remain a separate event.
	if !strings.HasSuffix(string(event), "\n\n") {
		event = append(event, '\n')
	}
	if len(choices) == 1 {
		if choice, ok := choices[0].(map[string]any); ok {
			delta, _ := choice["delta"].(map[string]any)
			f.partial.WriteString(anyContentToString(delta["content"]))
			if tools, ok := delta["tool_calls"].([]any); ok && len(tools) > 0 {
				f.hasTools = true
			}
			if delta["function_call"] != nil {
				f.hasTools = true
			}
			if reason, _ := choice["finish_reason"].(string); reason != "" {
				f.finishReason = reason
				f.blocked = blocksContent[reason]
				// A terminal event may also contain the last text/tool delta. Emit that
				// delta once now and retain a separate, empty finish delta for the tail.
				if len(delta) > 0 {
					choice["finish_reason"] = nil
					raw, _ := json.Marshal(chunk)
					f.pending = append(f.pending, []byte("data: "+string(raw)+"\n\n")...)
				}
				choice["finish_reason"] = reason
				choice["delta"] = map[string]any{}
				raw, _ := json.Marshal(chunk)
				f.finish = []byte("data: " + string(raw) + "\n\n")
				return
			}
		}
	}
	f.pending = append(f.pending, event...)
}

func (f *sseFilter) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(f.pending) == 0 {
		f.fill()
	}
	if len(f.pending) > 0 {
		n := copy(p, f.pending)
		f.pending = f.pending[n:]
		return n, nil
	}
	if f.readErr != nil {
		return 0, f.readErr
	}
	return 0, io.EOF
}
