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

// TestChatVideoSubmitReturnsLocalJobID 网页提交视频后必须拿到本地 job_id。
//
// 上游只回 video_id，而前端是拿 job_id 去轮询的。以前这条链路走的是
// serveChatMedia，它不做任何任务记账，于是「视频任务」里看不到记录，
// 前端也拿不到 job_id —— 提交完就没了下文。
func TestChatVideoSubmitReturnsLocalJobID(t *testing.T) {
	h := newHarness(t, 0, 1)

	resp, body := h.post("/api/chat/v1/videos", map[string]any{
		"model": "agnes-auto", "prompt": "夕阳下的海滩，海浪轻拍沙滩",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("视频提交应 200，实际 %d：%v", resp.StatusCode, body)
	}
	jobID, _ := body["job_id"].(string)
	if !strings.HasPrefix(jobID, "job") {
		t.Fatalf("应返回本地 job_id，实际 %v", body)
	}
	if poll, _ := body["poll_url"].(string); !strings.HasSuffix(poll, jobID) {
		t.Errorf("poll_url 应指向本地任务，实际 %q", poll)
	}
	// 上游返回的 video_id 也要带出来，方便和上游侧核对
	if vid, _ := body["video_id"].(string); vid == "" {
		t.Errorf("应保留上游的 video_id，实际 %v", body)
	}
	if n := h.mock.hits("/v1/videos"); n != 1 {
		t.Errorf("上游应收到 1 次提交，实际 %d 次", n)
	}
}

// TestChatVideoStatusPollsUpstreamNotResubmit 钉住视频轮询的路径。
//
// 上游的查询接口是 GET {root}/agnesapi?video_id=...，和提交接口完全不是一个
// 路径。早先 GET /api/chat/v1/videos/<id> 直接落到媒体代理上，被当成「又一次
// 提交」以 POST /v1/videos/<id> 发出去 —— 上游只会回 400/404，视频即使提交
// 成功也永远等不到结果。
func TestChatVideoStatusPollsUpstreamNotResubmit(t *testing.T) {
	h := newHarness(t, 0, 1)

	_, submit := h.post("/api/chat/v1/videos", map[string]any{
		"model": "agnes-auto", "prompt": "夕阳下的海滩",
	})
	jobID, _ := submit["job_id"].(string)
	if jobID == "" {
		t.Fatalf("提交应返回 job_id：%v", submit)
	}
	beforeVideos := h.mock.hits("/v1/videos")
	beforePoll := h.mock.hits("/agnesapi")

	resp, err := http.Get(h.ts.URL + "/api/chat/v1/videos/" + jobID)
	if err != nil {
		t.Fatalf("查询任务失败：%v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("查询应 200，实际 %d：%s", resp.StatusCode, raw)
	}
	var status map[string]any
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("响应不是 JSON：%v（%s）", err, raw)
	}
	// mock 上游的 /agnesapi 直接回 completed + video_url
	if got, _ := status["status"].(string); got != "completed" {
		t.Errorf("轮询应把上游的 completed 反映出来，实际 %q（%v）", got, status)
	}
	if u, _ := status["video_url"].(string); !strings.HasPrefix(u, "http") {
		t.Errorf("完成时应带出 video_url，实际 %v", status)
	}
	if n := h.mock.hits("/agnesapi"); n <= beforePoll {
		t.Errorf("轮询应打到上游的 /agnesapi，实际命中 %d（之前 %d）", n, beforePoll)
	}
	if n := h.mock.hits("/v1/videos"); n != beforeVideos {
		t.Errorf("轮询不该再次提交到 /v1/videos，实际从 %d 变成 %d", beforeVideos, n)
	}
}

// TestChatVideoStatusUnknownJob 查一个不存在的任务要明确说「找不到」，
// 而不是悄悄转发给上游。
func TestChatVideoStatusUnknownJob(t *testing.T) {
	h := newHarness(t, 0, 1)
	resp, err := http.Get(h.ts.URL + "/api/chat/v1/videos/job-does-not-exist")
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 404 {
		t.Fatalf("不存在的任务应 404，实际 %d：%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "找不到") {
		t.Errorf("应给出可读说明，实际 %s", raw)
	}
	if n := h.mock.hits("/v1/videos"); n != 0 {
		t.Errorf("查不到任务时不该打扰上游，实际命中 %d 次", n)
	}
}
