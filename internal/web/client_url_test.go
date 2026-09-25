package web

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"agneshub/internal/config"
)

// TestChatImageReturnsUpstreamURLForClient 钉住这次的真实故障。
//
// 外部 AI 客户端（截图里的 WorkBuddy）加载不了指向 NAS 的 http 地址：
// 它在另一个网络位置、甚至跑在 https 页面里。用户对比过 —— 上游那个公网
// https 产出地址在客户端里能正常显示，而本地地址不行（表现为图片位置出现
// 一串文字，那其实是 markdown 的 alt 文本）。
//
// 所以对不带「来自网关自家网页」标记的调用方，必须回上游地址。
func TestChatImageReturnsUpstreamURLForClient(t *testing.T) {
	h := newHarness(t, 0, 1)
	servePNGUpstream(t, h)
	upstream := h.mock.getImageURL()

	_, body := h.post("/api/chat/v1/chat/completions", map[string]any{
		"model": "agnes-auto",
		"messages": []any{
			map[string]any{"role": "user", "content": "帮我画一张水墨国风佳人图片，比例：9:16。"},
		},
		"stream":       false,
		"aspect_ratio": "9:16",
	})
	content := chatContent(t, body)

	if !strings.Contains(content, "]("+upstream+")") {
		t.Fatalf("外部客户端应拿到上游地址 %q，实际：%q", upstream, truncateStr(content, 300))
	}
	if strings.Contains(content, imageKind.route) {
		t.Errorf("外部客户端不该拿到本地地址：%q", truncateStr(content, 300))
	}
	// 但仍然要落盘一份，控制台与聊天记录长期回看靠它
	entries, _ := os.ReadDir(h.srv.mediaDir(imageKind))
	if len(entries) == 0 {
		t.Error("外部客户端请求也该落盘一份本地副本")
	}
}

// TestChatImageReturnsLocalURLForWebPage 网关自己的网页要拿到本地绝对地址。
//
// 浏览器与网关同源，本地地址一定可达 —— 这也正是「落盘」的意义（离线可看）。
func TestChatImageReturnsLocalURLForWebPage(t *testing.T) {
	h := newHarness(t, 0, 1)
	servePNGUpstream(t, h)

	_, body := h.postWeb("/api/chat/v1/chat/completions", map[string]any{
		"model": "agnes-auto",
		"messages": []any{
			map[string]any{"role": "user", "content": "帮我画一张水墨国风佳人图片，比例：9:16。"},
		},
		"stream":       false,
		"aspect_ratio": "9:16",
	})
	content := chatContent(t, body)

	// 必须是**绝对**地址：相对路径只有本页能解析，而正文可能被复制到别处
	if !strings.Contains(content, "]("+h.ts.URL+imageKind.route) {
		t.Fatalf("网页端应拿到本地绝对地址 %s…，实际：%q", h.ts.URL+imageKind.route, truncateStr(content, 300))
	}
}

// TestClientMediaURLSettingForcesLocal 设置项可以把客户端也强制成本地地址
// （适用于把网关放到 https 反代后面、客户端能访问到网关的部署）。
func TestClientMediaURLSettingForcesLocal(t *testing.T) {
	h := newHarness(t, 0, 1)
	servePNGUpstream(t, h)
	_ = h.store.UpdateSettings(func(st *config.Settings) { st.ClientMediaURL = "local" })

	_, body := h.post("/api/chat/v1/chat/completions", map[string]any{
		"model": "agnes-auto",
		"messages": []any{
			map[string]any{"role": "user", "content": "画一幅山水画。"},
		},
		"stream":       false,
		"aspect_ratio": "9:16",
	})
	content := chatContent(t, body)
	if !strings.Contains(content, "]("+h.ts.URL+imageKind.route) {
		t.Errorf("设置 client_media_url=local 后应回本地地址，实际：%q", truncateStr(content, 300))
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

	_, body := h.postWeb("/api/chat/v1/chat/completions", map[string]any{
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

// TestPublicBaseURLProxyDropsPort 记录反向代理的经典坑。
//
// NPM / nginx 默认把 Host 与 X-Forwarded-Host 都设成 $host（**不带端口**），
// 于是网关自动推断出来的基地址会少一段端口 —— 生成的媒体地址在客户端里打不开。
// 这种部署必须在「对外访问地址」里显式填全（含端口）。
func TestPublicBaseURLProxyDropsPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://hub/api/x", nil)
	req.Host = "agens.example.com" // 代理把 Host 改写成了 $host
	req.Header.Set("X-Forwarded-Host", "agens.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")

	if got := publicBaseURL(req, config.Settings{}); got != "https://agens.example.com" {
		t.Errorf("自动推断拿不到端口，应得到不带端口的地址，实际 %q", got)
	}
	cfg := config.Settings{PublicBaseURL: "https://agens.example.com:52325"}
	if got := publicBaseURL(req, cfg); got != "https://agens.example.com:52325" {
		t.Errorf("显式配置应胜出并补回端口，实际 %q", got)
	}
	if src := mediaBaseSource(req, cfg); !strings.Contains(src, "显式配置") {
		t.Errorf("应说明基地址来自显式配置，实际 %q", src)
	}
}

// TestMediaBaseEndpoint 自检接口要把诊断信息摊开，供排查代理配置。
func TestMediaBaseEndpoint(t *testing.T) {
	h := newHarness(t, 0, 1)

	req, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/api/media-base", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: h.store.SessionToken()})
	req.Header.Set("X-Forwarded-Host", "agens.example.com:52325")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("自检请求失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("自检应 200，实际 %d", resp.StatusCode)
	}
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)

	if got, _ := out["base_url"].(string); got != "https://agens.example.com:52325" {
		t.Errorf("应优先按 X-Forwarded-* 推断，实际 %q", got)
	}
	if got, _ := out["x_forwarded_proto"].(string); got != "https" {
		t.Errorf("应回显 X-Forwarded-Proto，实际 %q", got)
	}
	// 默认给客户端上游地址
	if got, _ := out["url_for_client"].(string); strings.Contains(got, imageKind.route) {
		t.Errorf("默认（client_media_url=upstream）不该给客户端本地地址，实际 %q", got)
	}
	// 改成 local 后应给本地地址
	_ = h.store.UpdateSettings(func(st *config.Settings) { st.ClientMediaURL = "local" })
	req2, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/api/media-base", nil)
	req2.AddCookie(&http.Cookie{Name: cookieName, Value: h.store.SessionToken()})
	req2.Header.Set("X-Forwarded-Host", "agens.example.com:52325")
	req2.Header.Set("X-Forwarded-Proto", "https")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("自检请求失败：%v", err)
	}
	defer resp2.Body.Close()
	var out2 map[string]any
	raw2, _ := io.ReadAll(resp2.Body)
	_ = json.Unmarshal(raw2, &out2)
	if got, _ := out2["url_for_client"].(string); !strings.Contains(got, "https://agens.example.com:52325"+imageKind.route) {
		t.Errorf("client_media_url=local 时应给客户端本地绝对地址，实际 %q", got)
	}
}
