package web

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agneshub/internal/config"
)

// TestChatImageReturnsAbsoluteURL 钉住这次的真实故障。
//
// 网关自己网页里的 /api/chat/images/xxx.png 是相对地址，靠页面的 origin 解析；
// 而 AI 客户端拿到的只是一段 Markdown，没有任何基准可以解析它 —— 图片必然加载
// 失败，客户端只能退化成显示 alt 文本（用户看到的就是一串莫名其妙的提示词）。
// 所以给客户端的地址必须是绝对的。
func TestChatImageReturnsAbsoluteURL(t *testing.T) {
	h := newHarness(t, 0, 1)
	servePNGUpstream(t, h)

	_, body := h.post("/api/chat/v1/chat/completions", map[string]any{
		"model": "agnes-auto",
		"messages": []any{
			map[string]any{"role": "user", "content": "帮我画一张水墨国风佳人图片，比例：9:16。"},
			map[string]any{"role": "user", "content": "<craft_mode>You are now in Agent mode.</craft_mode>"},
		},
		"stream":       false,
		"aspect_ratio": "9:16",
	})
	content := chatContent(t, body)

	wantPrefix := h.ts.URL + imageKind.route
	if !strings.Contains(content, "]("+wantPrefix) {
		t.Fatalf("正文里的图片地址应是绝对地址 %s…，实际：%q", wantPrefix, truncateStr(content, 300))
	}
	if strings.Contains(content, "]("+imageKind.route) {
		t.Errorf("不该再出现相对地址：%q", truncateStr(content, 300))
	}
}

// TestChatImageUsesRealUserTurn 端到端：客户端形态的请求要拿用户原话去生图。
func TestChatImageUsesRealUserTurn(t *testing.T) {
	h := newHarness(t, 0, 1)
	servePNGUpstream(t, h)

	h.post("/api/chat/v1/chat/completions", map[string]any{
		"model": "agnes-auto",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a coding agent."},
			map[string]any{"role": "user", "content": "<user_query>帮我画一张水墨国风佳人图片，比例：9:16。</user_query>"},
			map[string]any{"role": "user", "content": "<craft_mode>You are now in Agent mode. Continue with the task in the new mode.</craft_mode>"},
		},
		"stream":       false,
		"aspect_ratio": "9:16",
	})

	got := h.mock.lastImagePrompt()
	if got != "帮我画一张水墨国风佳人图片，比例：9:16。" {
		t.Errorf("上游应收到用户原话，实际 %q", got)
	}
	if strings.Contains(got, "craft_mode") {
		t.Errorf("上游不该收到框架注入块：%q", got)
	}
}

// TestMediaRouteIsPublic 媒体回放必须免鉴权。
//
// 外部 AI 客户端没有、也不可能带上网关的会话 cookie；一旦要求登录，客户端里
// 就永远是一张裂图。文件名是内容的 sha256 前 16 字节（128 位），不可枚举 ——
// 这是一个 capability URL。
func TestMediaRouteIsPublic(t *testing.T) {
	h := newHarness(t, 0, 1)
	servePNGUpstream(t, h)

	_, body := h.post("/api/chat/v1/chat/completions", map[string]any{
		"model": "agnes-auto",
		"messages": []any{
			map[string]any{"role": "user", "content": "画一幅山水画。"},
		},
		"stream":       false,
		"aspect_ratio": "9:16",
	})
	content := chatContent(t, body)
	m := localImageRe.FindStringSubmatch(content)
	if m == nil {
		t.Fatalf("正文里应含图片地址：%q", truncateStr(content, 300))
	}

	// 刻意不带任何 cookie
	req, _ := http.NewRequest(http.MethodGet, h.ts.URL+imageKind.route+m[1], nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("取媒体失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("免鉴权的媒体回放应 200，实际 %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if len(got) == 0 {
		t.Error("回放内容为空")
	}
}

// TestPublicBaseURL 基地址的推导顺序：显式配置 > X-Forwarded-* > Host。
func TestPublicBaseURL(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://10.0.0.5:4142/api/chat/v1/chat/completions", nil)
	req.Host = "10.0.0.5:4142"

	if got := publicBaseURL(req, config.Settings{}); got != "http://10.0.0.5:4142" {
		t.Errorf("无配置时应按 Host 推断，实际 %q", got)
	}

	req.Header.Set("X-Forwarded-Host", "hub.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	if got := publicBaseURL(req, config.Settings{}); got != "https://hub.example.com" {
		t.Errorf("应优先用 X-Forwarded-*，实际 %q", got)
	}

	// 代理链会追加多个值，取第一个
	req.Header.Set("X-Forwarded-Proto", "https, http")
	if got := publicBaseURL(req, config.Settings{}); got != "https://hub.example.com" {
		t.Errorf("逗号分隔应取第一个，实际 %q", got)
	}

	// 显式配置优先级最高，且要容忍结尾斜杠
	cfg := config.Settings{PublicBaseURL: "https://my.hub/nas/"}
	if got := publicBaseURL(req, cfg); got != "https://my.hub/nas" {
		t.Errorf("显式配置应胜出且去掉结尾斜杠，实际 %q", got)
	}
}

// TestPublicBaseURLFromTLS 直连 HTTPS 时按请求本身推断。
func TestPublicBaseURLFromTLS(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://nas.local:4142/x", nil)
	req.Host = "nas.local:4142"
	req.TLS = &tls.ConnectionState{}
	if got := publicBaseURL(req, config.Settings{}); got != "https://nas.local:4142" {
		t.Errorf("TLS 请求应推断成 https，实际 %q", got)
	}
}

// TestLocalizeURLHonoursExplicitBase 显式配置要真的作用到落盘地址上。
func TestLocalizeURLHonoursExplicitBase(t *testing.T) {
	s := &Server{Store: newTempStore(t)}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G'})
	}))
	defer cdn.Close()

	got := s.localizeImageURL(context.Background(), cdn.URL+"/a.png", "https://hub.example.com")
	if !strings.HasPrefix(got, "https://hub.example.com"+imageKind.route) {
		t.Errorf("应拼上显式基地址，实际 %q", got)
	}
}
