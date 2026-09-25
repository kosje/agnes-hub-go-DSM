package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestChatUpstreamPathStripsAPIChatPrefix 钉住控制台路由 → 上游路径的映射。
//
// 控制台路由是 /api/chat/v1/...（为了和裸 /v1 代理区分），转发给上游时必须
// 摘掉 /api/chat。漏摘会拼成 {root}/api/chat/v1/images/generations，
// 上游只认 {root}/v1/images/generations —— 实测会被边缘 WAF 拦成一张 HTML 错误页。
func TestChatUpstreamPathStripsAPIChatPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/api/chat/v1/images/generations", "/v1/images/generations"},
		{"/api/chat/v1/videos", "/v1/videos"},
		{"/api/chat/v1/videos/job-123", "/v1/videos/job-123"},
		// 已经是不带前缀的形态时保持原样，避免误伤
		{"/v1/images/generations", "/v1/images/generations"},
		// 只是开头相似，不能误剥
		{"/api/chatterbox/v1/x", "/api/chatterbox/v1/x"},
	}
	for _, c := range cases {
		if got := chatUpstreamPath(c.in); got != c.want {
			t.Errorf("chatUpstreamPath(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestConsoleImageRouteHitsUpstreamV1Path 复现并钉住「控制台生图页打不到上游」的问题：
// 路由带 /api/chat 前缀，若原样透传，上游会收到一个不存在的路径。
func TestConsoleImageRouteHitsUpstreamV1Path(t *testing.T) {
	h := newHarness(t, 0, 1)

	resp, body := h.post("/api/chat/v1/images/generations", map[string]any{
		"model": "agnes-image-2.5-flash", "prompt": "一只在窗台晒太阳的橘猫", "n": 1, "size": "1024x1024",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("生图应 200，实际 %d：%v", resp.StatusCode, body)
	}
	if n := h.mock.hits("/v1/images/generations"); n != 1 {
		t.Errorf("上游应收到 1 次 /v1/images/generations，实际 %d 次", n)
	}
	if n := h.mock.hits("/api/chat/v1/images/generations"); n != 0 {
		t.Errorf("上游不该收到带 /api/chat 前缀的路径，实际 %d 次", n)
	}
	data, _ := body["data"].([]any)
	if len(data) == 0 {
		t.Fatalf("响应缺少 data[]：%v", body)
	}
}

// TestChatAutoImageIsWrappedAsChat 钉住「对话页选 agnes-auto 说一句画图」这条链路。
//
// 这里同时覆盖两个真实缺陷：
//  1. 请求体被读第二遍变空 —— 提示词丢失，上游会以「prompt 不能为空」拒绝；
//  2. 上游返回的是 images 结构（没有 choices），若不回译成 chat 形态，
//     前端按 choices[0].message.content 取值会拿到空串，显示「（无内容返回）」。
func TestChatAutoImageIsWrappedAsChat(t *testing.T) {
	h := newHarness(t, 0, 1)

	resp, body := h.post("/api/chat/v1/chat/completions", map[string]any{
		"model":    "agnes-auto",
		"messages": []any{map[string]any{"role": "user", "content": "画一幅山水画。"}},
		"stream":   false,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("对话内生图应 200，实际 %d：%v", resp.StatusCode, body)
	}

	choices, _ := body["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("响应必须是 chat 形态（含 choices），实际：%v", body)
	}
	c0, _ := choices[0].(map[string]any)
	msg, _ := c0["message"].(map[string]any)
	content, _ := msg["content"].(string)
	if strings.TrimSpace(content) == "" {
		t.Fatalf("choices[0].message.content 不能为空（这正是「无内容返回」的成因）：%v", body)
	}
	if !strings.Contains(content, "![") {
		t.Errorf("正文应包含 Markdown 图片语法，实际：%q", content)
	}
	// 提示词必须真的传到了上游（mock 对缺 prompt 的请求回 400）
	if n := h.mock.hits("/v1/images/generations"); n != 1 {
		t.Errorf("上游应收到 1 次 /v1/images/generations，实际 %d 次", n)
	}
}

// TestChatAutoImageStreamIsSSE 流式分支：对话页固定发 stream=true，
// 必须拿到 text/event-stream，且帧里带得出图片 Markdown。
func TestChatAutoImageStreamIsSSE(t *testing.T) {
	h := newHarness(t, 0, 1)

	buf, _ := json.Marshal(map[string]any{
		"model":    "agnes-auto",
		"messages": []any{map[string]any{"role": "user", "content": "画一幅山水画。"}},
		"stream":   true,
	})
	req, _ := http.NewRequest(http.MethodPost,
		h.ts.URL+"/api/chat/v1/chat/completions", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Fatalf("流式应 200，实际 %d：%s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应为 text/event-stream，实际 %q", ct)
	}
	text := string(raw)
	if !strings.Contains(text, "data:") {
		t.Fatalf("SSE 应含 data: 帧，实际：%q", text)
	}
	if !strings.Contains(text, "![") {
		t.Errorf("SSE 帧里应带出图片 Markdown，实际：%q", text)
	}
}
