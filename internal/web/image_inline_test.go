package web

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// onePixelPNG 是一张合法的 1x1 PNG。
//
// 必须用真图：fetchImageDataURI 在 Content-Type 不可信时会用 http.DetectContentType
// 按魔数判断，随便一串字节过不了 "image/" 前缀校验。
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// TestFetchImageDataURIConservativeFallbacks 钉住回取代理的「宁可不内联」原则。
//
// 这个函数处在「渲染图片」和「整个响应」之间的关键位置：任何一步判断失误都可能
// 把一页 Cloudflare HTML 当成图片塞进正文（几万字符），或者让整个生图请求挂掉。
// 所以每条失败路径都必须原样退回入参。
func TestFetchImageDataURIConservativeFallbacks(t *testing.T) {
	pngBytes, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatalf("测试用 PNG 解码失败：%v", err)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)

	serve := func(ct string, status int, body []byte) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.WriteHeader(status)
			_, _ = w.Write(body)
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	normal := serve("image/png", http.StatusOK, pngBytes)
	// 上游经常把图片的 Content-Type 写成 application/octet-stream，必须靠魔数兜底
	octet := serve("application/octet-stream", http.StatusOK, pngBytes)
	// 边缘 WAF 拦下来的 HTML 页：绝不能内联
	htmlPage := serve("text/html", http.StatusOK,
		[]byte("<!doctype html><html><head><title>Attention Required!</title></head></html>"))
	notFound := serve("text/plain", http.StatusNotFound, []byte("gone"))

	s := &Server{}

	cases := []struct {
		name, in, want string
	}{
		{"已是 data URI 时原样返回", "data:image/png;base64,AAAA", "data:image/png;base64,AAAA"},
		{"非 http(s) 协议不处理", "ftp://example.test/a.png", "ftp://example.test/a.png"},
		{"相对路径不处理", "/local/a.png", "/local/a.png"},
		{"空串原样返回", "", ""},
		{"正常图片内联成 base64", normal.URL + "/a.png", want},
		{"Content-Type 不可信时按魔数兜底", octet.URL + "/a.png", want},
		{"HTML 响应不回取", htmlPage.URL + "/a.png", htmlPage.URL + "/a.png"},
		{"上游 404 时退回原地址", notFound.URL + "/a.png", notFound.URL + "/a.png"},
	}
	for _, c := range cases {
		if got := s.fetchImageDataURI(context.Background(), c.in); got != c.want {
			t.Errorf("%s：fetchImageDataURI(%q) = %q，期望 %q", c.name, c.in, got, c.want)
		}
	}
}

// inlinePNGServer 起一个返回 1x1 PNG 的假 CDN，并把它塞给模拟上游当生图结果地址。
func inlinePNGServer(t *testing.T, h *harness) {
	t.Helper()
	pngBytes, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatalf("测试用 PNG 解码失败：%v", err)
	}
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	t.Cleanup(img.Close)
	h.mock.setImageURL(img.URL + "/generated.png")
}

// TestChatImageIsInlinedAsBase64 钉住「服务端 base64 回取代理」在对话链路上生效。
//
// 背景：上游给的图片地址常带鉴权、或落在浏览器够不到的 CDN 上，直接写进 Markdown
// 只会渲染成裂图。正确做法是服务端取回、转 base64 内联，正文里就是
// ![prompt](data:image/png;base64,...)，前端 renderMarkdown 放行 data:image/。
func TestChatImageIsInlinedAsBase64(t *testing.T) {
	h := newHarness(t, 0, 1)
	inlinePNGServer(t, h)

	resp, body := h.post("/api/chat/v1/chat/completions", map[string]any{
		"model":    "agnes-auto",
		"messages": []any{map[string]any{"role": "user", "content": "画一幅山水画，比例 9:16。"}},
		"stream":   false,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("对话内生图应 200，实际 %d：%v", resp.StatusCode, body)
	}

	content := chatContent(t, body)
	if !strings.Contains(content, "data:image/png;base64,") {
		t.Errorf("正文里的图片必须是服务端回取的 base64，实际：%q", truncateStr(content, 300))
	}
	if strings.Contains(content, "![") && !strings.Contains(content, "](data:image/") {
		t.Errorf("Markdown 图片链接指向的应是 data URI，实际：%q", truncateStr(content, 300))
	}

	// images[] 是前端渲染历史缩略图用的，同样不能残留上游地址
	imgs, _ := body["images"].([]any)
	if len(imgs) == 0 {
		t.Fatalf("响应缺少 images[]：%v", body)
	}
	first, _ := imgs[0].(map[string]any)
	if u, _ := first["url"].(string); !strings.HasPrefix(u, "data:image/") {
		t.Errorf("images[0].url 应为 data URI，实际 %q", truncateStr(u, 120))
	}
}

// TestChatImageStreamIsInlined 对话页固定发 stream=true，SSE 帧里的正文同样要内联。
func TestChatImageStreamIsInlined(t *testing.T) {
	h := newHarness(t, 0, 1)
	inlinePNGServer(t, h)

	raw := h.postSSE("/api/chat/v1/chat/completions", map[string]any{
		"model":    "agnes-auto",
		"messages": []any{map[string]any{"role": "user", "content": "画一幅山水画。"}},
		"stream":   true,
	})
	if !strings.Contains(raw, "data:image/png;base64,") {
		t.Errorf("SSE 帧里应带出内联的 base64 图片，实际：%q", truncateStr(raw, 400))
	}
}

// TestWebImageTabGetsInlinedURLOfficialShape 对话页「生图」标签页打的是
// /api/chat/v1/images/generations：响应保持官方形态（data[].url），
// 但 url 必须是已回取的 data URI —— 前端拿 item.url 直接当 <img src> 用。
func TestWebImageTabGetsInlinedURLOfficialShape(t *testing.T) {
	h := newHarness(t, 0, 1)
	inlinePNGServer(t, h)

	resp, body := h.post("/api/chat/v1/images/generations", map[string]any{
		"model": "agnes-image-2.5-flash", "prompt": "一只在窗台晒太阳的橘猫", "n": 1, "size": "1024x1024",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("生图应 200，实际 %d：%v", resp.StatusCode, body)
	}
	data, _ := body["data"].([]any)
	if len(data) == 0 {
		t.Fatalf("响应缺少 data[]：%v", body)
	}
	first, _ := data[0].(map[string]any)
	u, _ := first["url"].(string)
	if !strings.HasPrefix(u, "data:image/png;base64,") {
		t.Errorf("网页生图页的 data[0].url 应为 data URI，实际 %q", truncateStr(u, 120))
	}
}

// TestBareV1KeepsUpstreamURL 裸 /v1 是给程序化客户端（curl / 官方 SDK）用的，
// 必须保留真实的上游地址：它们要自己下载或转存，塞几 MB 的 base64 只会把
// 响应撑大 33% 并破坏它们的解析逻辑。
func TestBareV1KeepsUpstreamURL(t *testing.T) {
	h := newHarness(t, 0, 1)
	inlinePNGServer(t, h)
	want := h.mock.getImageURL()

	resp, body := h.post("/v1/images/generations", map[string]any{
		"model": "agnes-image-2.5-flash", "prompt": "一只在窗台晒太阳的橘猫", "n": 1, "size": "1024x1024",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("生图应 200，实际 %d：%v", resp.StatusCode, body)
	}
	data, _ := body["data"].([]any)
	if len(data) == 0 {
		t.Fatalf("响应缺少 data[]：%v", body)
	}
	first, _ := data[0].(map[string]any)
	u, _ := first["url"].(string)
	if u != want {
		t.Errorf("裸 /v1 应原样返回上游地址 %q，实际 %q", want, truncateStr(u, 120))
	}
	if strings.HasPrefix(u, "data:") {
		t.Error("裸 /v1 不应内联 base64")
	}
}

// chatContent 从 chat 形态响应里取出正文。
func chatContent(t *testing.T, body map[string]any) string {
	t.Helper()
	choices, _ := body["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("响应必须是 chat 形态（含 choices），实际：%v", body)
	}
	c0, _ := choices[0].(map[string]any)
	msg, _ := c0["message"].(map[string]any)
	content, _ := msg["content"].(string)
	return content
}
