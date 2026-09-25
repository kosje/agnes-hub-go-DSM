package web

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"agneshub/internal/config"
)

// newTempStore 造一个落在临时目录的 Store，供不依赖模拟上游的单元测试使用。
//
// 图片缓存目录挂在 Store.Dir 下，所以这里天然是一个一次性的干净沙箱，
// 测试之间不会互相看到对方的图片。
func newTempStore(t *testing.T) *config.Store {
	t.Helper()
	store, err := config.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("初始化 store 失败：%v", err)
	}
	return store
}

// onePixelPNG 是一张合法的 1x1 PNG。
//
// 必须用真图：mediaFetchClient 在 Content-Type 不可信时会用 http.DetectContentType
// 按魔数判断，随便一串字节过不了白名单校验。
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// localImageRe 匹配本地图片地址，并捕获文件名。
var localImageRe = regexp.MustCompile(`/api/chat/images/([0-9a-f]{32}\.[a-z]{3,4})`)

// TestLocalizeImageURLConservativeFallbacks 钉住「宁可不本地化」原则。
//
// 这个函数处在「渲染图片」和「整个响应」之间的关键位置：任何一步判断失误都可能
// 把一页 Cloudflare HTML 当成图片存进缓存目录，或者让整个生图请求挂掉。
// 所以每条失败路径都必须原样退回入参。
func TestLocalizeImageURLConservativeFallbacks(t *testing.T) {
	pngBytes, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatalf("测试用 PNG 解码失败：%v", err)
	}

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
	// 边缘 WAF 拦下来的 HTML 页：绝不能存成本地图片
	htmlPage := serve("text/html", http.StatusOK,
		[]byte("<!doctype html><html><head><title>Attention Required!</title></head></html>"))
	notFound := serve("text/plain", http.StatusNotFound, []byte("gone"))

	s := &Server{Store: newTempStore(t)}

	// 原样返回的几种情况
	for _, c := range []struct{ name, in string }{
		{"已是本地地址时原样返回", imageKind.route + "0123456789abcdef0123456789abcdef.png"},
		{"data URI 不处理", "data:image/png;base64,AAAA"},
		{"非 http(s) 协议不处理", "ftp://example.test/a.png"},
		{"相对路径不处理", "/local/a.png"},
		{"空串原样返回", ""},
		{"HTML 响应不本地化", htmlPage.URL + "/a.png"},
		{"上游 404 时退回原地址", notFound.URL + "/a.png"},
	} {
		if got := s.localizeImageURL(context.Background(), c.in, ""); got != c.in {
			t.Errorf("%s：localizeImageURL(%q) = %q，期望原样返回", c.name, c.in, got)
		}
	}

	// 正常图片：换成本地地址，并且文件真的落了盘
	for _, c := range []struct{ name, in string }{
		{"正常图片落盘", normal.URL + "/a.png"},
		{"Content-Type 不可信时按魔数兜底", octet.URL + "/a.png"},
	} {
		got := s.localizeImageURL(context.Background(), c.in, "")
		m := localImageRe.FindStringSubmatch(got)
		if m == nil {
			t.Fatalf("%s：应返回本地图片地址，实际 %q", c.name, got)
		}
		if !strings.HasSuffix(m[1], ".png") {
			t.Errorf("%s：扩展名应为 .png，实际 %q", c.name, m[1])
		}
		raw, err := os.ReadFile(filepath.Join(s.mediaDir(imageKind), m[1]))
		if err != nil {
			t.Fatalf("%s：文件应已落盘，读取失败 %v", c.name, err)
		}
		if string(raw) != string(pngBytes) {
			t.Errorf("%s：落盘内容与上游不一致", c.name)
		}
	}
}

// TestLocalizeImageURLDeduplicates 内容寻址：同一张图重复生成只落一份盘。
func TestLocalizeImageURLDeduplicates(t *testing.T) {
	pngBytes, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()

	s := &Server{Store: newTempStore(t)}
	first := s.localizeImageURL(context.Background(), srv.URL+"/a.png", "")
	second := s.localizeImageURL(context.Background(), srv.URL+"/b.png", "")
	if first != second {
		t.Errorf("同一内容应得到同一地址，实际 %q vs %q", first, second)
	}
	entries, _ := os.ReadDir(s.mediaDir(imageKind))
	if len(entries) != 1 {
		t.Errorf("缓存目录应只有 1 个文件，实际 %d 个", len(entries))
	}
}

// TestValidImageNameRejectsTraversal 文件名是路径穿越的唯一闸门。
//
// 文件名会被拼进 filepath.Join，放任 ../ 进来就能读到数据目录里的
// settings.json（里面存着上游 API Key 与口令哈希）。
func TestValidImageNameRejectsTraversal(t *testing.T) {
	good := []string{
		"0123456789abcdef0123456789abcdef.png",
		"0123456789abcdef0123456789abcdef.webp",
		"ffffffffffffffffffffffffffffffff.jpg",
	}
	for _, n := range good {
		if !validMediaName(imageKind, n) {
			t.Errorf("%q 应被接受", n)
		}
	}
	bad := []string{
		"../settings.json", "..", ".", "settings.json",
		"/etc/passwd", "0123456789abcdef0123456789abcdef.png/../settings.json",
		"0123456789ABCDEF0123456789abcdef.png", // 大写不是我们生成的文件名
		"0123456789abcdef0123456789abcde.png",  // 少一位
		"0123456789abcdef0123456789abcdef.sh",  // 扩展名不在白名单
		"0123456789abcdef0123456789abcdef",     // 没有扩展名
		"",
	}
	for _, n := range bad {
		if validMediaName(imageKind, n) {
			t.Errorf("%q 应被拒绝", n)
		}
	}
}

// TestChatImageRouteServesSavedFile 端到端：正文里的本地地址必须真的能取回图片。
func TestChatImageRouteServesSavedFile(t *testing.T) {
	h := newHarness(t, 0, 1)
	servePNGUpstream(t, h)

	_, body := h.postWeb("/api/chat/v1/chat/completions", map[string]any{
		"model":    "agnes-auto",
		"messages": []any{map[string]any{"role": "user", "content": "画一幅山水画。"}},
		"stream":   false,
	})
	content := chatContent(t, body)
	m := localImageRe.FindStringSubmatch(content)
	if m == nil {
		t.Fatalf("正文里应含本地图片地址，实际：%q", truncateStr(content, 300))
	}

	resp, err := http.Get(h.ts.URL + imageKind.route + m[1])
	if err != nil {
		t.Fatalf("取本地图片失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("本地图片应 200，实际 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "image/png") {
		t.Errorf("Content-Type 应为 image/png，实际 %q", ct)
	}
	got, _ := io.ReadAll(resp.Body)
	want, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	if string(got) != string(want) {
		t.Error("回放的图片内容与上游不一致")
	}
}

// TestChatImageIsLinkedLocally 钉住诉求「图片落盘、聊天窗口显示本地链接」。
//
// 之前是把图片 base64 内联进正文，一张 3 MB 的图会让响应涨到 8 MB
// （正文一份 + images[] 一份），聊天记录也会被同一条撑爆。
func TestChatImageIsLinkedLocally(t *testing.T) {
	h := newHarness(t, 0, 1)
	servePNGUpstream(t, h)
	upstream := h.mock.getImageURL()

	resp, body := h.postWeb("/api/chat/v1/chat/completions", map[string]any{
		"model":    "agnes-auto",
		"messages": []any{map[string]any{"role": "user", "content": "画一幅山水画，比例 9:16。"}},
		"stream":   false,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("对话内生图应 200，实际 %d：%v", resp.StatusCode, body)
	}

	content := chatContent(t, body)
	// 地址是绝对的（外部客户端需要），所以只断言含本地路由而非以它开头
	if !strings.Contains(content, "](") || !strings.Contains(content, imageKind.route) {
		t.Errorf("Markdown 图片应指向本地地址，实际：%q", truncateStr(content, 300))
	}
	if strings.Contains(content, upstream) {
		t.Errorf("正文不该出现上游地址（浏览器加载不出来）：%q", truncateStr(content, 300))
	}
	if strings.Contains(content, "base64") {
		t.Errorf("正文不该内联 base64：%q", truncateStr(content, 300))
	}

	// images[] 是前端渲染历史缩略图用的，同样要指向本地
	imgs, _ := body["images"].([]any)
	if len(imgs) == 0 {
		t.Fatalf("响应缺少 images[]：%v", body)
	}
	first, _ := imgs[0].(map[string]any)
	if u, _ := first["url"].(string); !strings.Contains(u, imageKind.route) {
		t.Errorf("images[0].url 应为本地地址，实际 %q", truncateStr(u, 120))
	}
}

// TestChatImageStreamIsLinkedLocally 对话页固定发 stream=true，SSE 帧里的正文同样要指向本地。
func TestChatImageStreamIsLinkedLocally(t *testing.T) {
	h := newHarness(t, 0, 1)
	servePNGUpstream(t, h)

	raw := h.postSSEWeb("/api/chat/v1/chat/completions", map[string]any{
		"model":    "agnes-auto",
		"messages": []any{map[string]any{"role": "user", "content": "画一幅山水画。"}},
		"stream":   true,
	})
	if !strings.Contains(raw, imageKind.route) {
		t.Errorf("SSE 帧里应带出本地图片地址，实际：%q", truncateStr(raw, 400))
	}
}

// TestWebImageTabGetsLocalURL 对话页「生图」标签页打的是
// /api/chat/v1/images/generations：响应保持官方形态（data[].url），
// 但 url 必须是本地地址 —— 前端拿 item.url 直接当 <img src> 用。
func TestWebImageTabGetsLocalURL(t *testing.T) {
	h := newHarness(t, 0, 1)
	servePNGUpstream(t, h)

	resp, body := h.postWeb("/api/chat/v1/images/generations", map[string]any{
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
	if !strings.Contains(u, imageKind.route) {
		t.Errorf("网页生图页的 data[0].url 应为本地地址，实际 %q", truncateStr(u, 120))
	}
}

// TestBareV1KeepsUpstreamURL 裸 /v1 是给程序化客户端（curl / 官方 SDK）用的，
// 必须保留真实的上游地址：它们要自己下载或转存，拿一个本机内网地址毫无用处。
func TestBareV1KeepsUpstreamURL(t *testing.T) {
	h := newHarness(t, 0, 1)
	servePNGUpstream(t, h)
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
}

// servePNGUpstream 起一个返回 1x1 PNG 的假 CDN，并把它塞给模拟上游当生图结果地址。
func servePNGUpstream(t *testing.T, h *harness) {
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
