package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agneshub/internal/config"
)

// newProbeServer 起一个假上游，/v1/models 返回给定的模型名。
func newProbeServer(t *testing.T, ids []string, raw string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/models") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if raw != "" {
			_, _ = w.Write([]byte(raw))
			return
		}
		data := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]any{"id": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestProbeUpstreamModelsDetectsMissingVideoModel 钉住核心诊断能力。
//
// 这正是「生视频一直 400」最可能的原因：上游把视频模型下架了，而控制台里的
// 清单是手填的、不会自己更新。探测要能一眼指出「本地填了、上游没给」。
func TestProbeUpstreamModelsDetectsMissingVideoModel(t *testing.T) {
	up := newProbeServer(t, []string{"agnes-2.5-flash", "agnes-image-2.5-flash"}, "", 200)
	store := newTempStore(t)
	acc := store.AddAccount("测试账号", "sk-test", "free", up.URL+"/v1", &config.ModelManifest{
		Text:  []string{"agnes-2.5-flash"},
		Image: []string{"agnes-image-2.5-flash"},
		Video: []string{"agnes-video-2.5-flash", "agnes-video-v2.0"},
	})

	s := &Server{Store: store}
	res := s.probeUpstreamModels(context.Background(), acc, store.SettingsSnapshot())

	if !res.OK {
		t.Fatalf("探测应成功，实际 error=%q", res.Error)
	}
	if res.Count != 2 {
		t.Errorf("上游给了 2 个模型，实际 %d", res.Count)
	}
	got := res.Missing["video"]
	if len(got) != 2 {
		t.Fatalf("应报出 2 个缺失的视频模型，实际 %v（missing=%v）", got, res.Missing)
	}
	if !strings.Contains(strings.Join(got, ","), "agnes-video-2.5-flash") {
		t.Errorf("缺失列表应包含 agnes-video-2.5-flash，实际 %v", got)
	}
	// 上游有的模态不该误报
	if _, ok := res.Missing["text"]; ok {
		t.Errorf("text 模型上游有，不该报缺失：%v", res.Missing["text"])
	}
}

// TestProbeUpstreamModelsAllPresent 全部命中时不该报缺失。
func TestProbeUpstreamModelsAllPresent(t *testing.T) {
	up := newProbeServer(t, []string{"agnes-2.5-flash", "agnes-image-2.5-flash", "agnes-video-2.5-flash"}, "", 200)
	store := newTempStore(t)
	acc := store.AddAccount("测试账号", "sk-test", "free", up.URL+"/v1", &config.ModelManifest{
		Text:  []string{"agnes-2.5-flash"},
		Image: []string{"agnes-image-2.5-flash"},
		Video: []string{"agnes-video-2.5-flash"},
	})

	s := &Server{Store: store}
	res := s.probeUpstreamModels(context.Background(), acc, store.SettingsSnapshot())

	if !res.OK || res.Count != 3 {
		t.Fatalf("探测应成功且拿到 3 个模型，实际 ok=%v count=%d err=%q", res.OK, res.Count, res.Error)
	}
	if len(res.Missing) != 0 {
		t.Errorf("不该报缺失，实际 %v", res.Missing)
	}
}

// TestProbeUpstreamModelsSurfacesUpstreamError 上游报错时要把原因带出来，
// 而不是只留一个状态码。
func TestProbeUpstreamModelsSurfacesUpstreamError(t *testing.T) {
	up := newProbeServer(t, nil,
		`{"error":{"code":"","message":"Token not provided","type":"AgnesAI_error"}}`, 401)
	store := newTempStore(t)
	acc := store.AddAccount("测试账号", "sk-test", "free", up.URL+"/v1", &config.ModelManifest{})

	s := &Server{Store: store}
	res := s.probeUpstreamModels(context.Background(), acc, store.SettingsSnapshot())

	if res.OK {
		t.Fatal("401 时不该判成功")
	}
	if res.Status != 401 {
		t.Errorf("应记录状态码 401，实际 %d", res.Status)
	}
	if !strings.Contains(res.Error, "Token not provided") {
		t.Errorf("错误信息应含上游原话，实际 %q", res.Error)
	}
}

// TestParseModelListShapes 上游的模型列表结构并不统一，几种常见形态都要认得。
func TestParseModelListShapes(t *testing.T) {
	cases := []struct {
		name, raw string
		want      []string
	}{
		{"OpenAI 官方形态", `{"data":[{"id":"b"},{"id":"a"}]}`, []string{"a", "b"}},
		{"data 里直接给字符串", `{"data":["b","a"]}`, []string{"a", "b"}},
		{"models 字段", `{"models":["a"]}`, []string{"a"}},
		{"裸数组", `["b","a"]`, []string{"a", "b"}},
		{"去重", `{"data":[{"id":"a"},{"id":"a"}]}`, []string{"a"}},
		{"HTML 不是模型列表", `<!doctype html><html></html>`, nil},
	}
	for _, c := range cases {
		got := parseModelList([]byte(c.raw))
		if len(got) != len(c.want) {
			t.Errorf("%s：得到 %v，期望 %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s：得到 %v，期望 %v", c.name, got, c.want)
				break
			}
		}
	}
}

// TestErrorPayloadAlwaysHasReadableMessage 钉住「界面不能只剩一个 HTTP 400」。
//
// 生视频失败时界面只有「HTTP 400」四个字，完全看不出是模型不可用、额度不够、
// 还是参数不合法 —— 用户没法自查，我们也拿不到线索。这个函数保证无论上游
// 用什么字段名，客户端都能拿到一句人能看懂的话。
func TestErrorPayloadAlwaysHasReadableMessage(t *testing.T) {
	cases := []struct {
		name, raw, wantSub string
	}{
		{"标准形态原样透传", `{"error":{"message":"额度不足","type":"x"}}`, "额度不足"},
		{"顶层 message", `{"message":"模型不存在"}`, "模型不存在"},
		{"msg 字段", `{"msg":"参数不合法"}`, "参数不合法"},
		{"FastAPI 的 detail", `{"detail":"model not found"}`, "model not found"},
		{"error 是字符串", `{"error":"boom"}`, "boom"},
		{"error 对象里只有 code", `{"error":{"code":"invalid_model"}}`, "invalid_model"},
		{"空对象", `{}`, "HTTP 400"},
		{"HTML 拦截页", `<!doctype html><html><head></head></html>`, "HTML"},
		{"裸数组", `[1,2,3]`, "HTTP 400"},
	}
	for _, c := range cases {
		payload := errorPayload(400, []byte(c.raw))
		m, ok := payload.(map[string]any)
		if !ok {
			t.Errorf("%s：应返回对象，实际 %T", c.name, payload)
			continue
		}
		e, ok := m["error"].(map[string]any)
		if !ok {
			t.Errorf("%s：应含 error 对象，实际 %v", c.name, m)
			continue
		}
		msg := asStr(e["message"])
		if strings.TrimSpace(msg) == "" {
			t.Errorf("%s：error.message 不能为空（前端会退化成 HTTP 400）", c.name)
			continue
		}
		if !strings.Contains(msg, c.wantSub) {
			t.Errorf("%s：error.message = %q，应包含 %q", c.name, msg, c.wantSub)
		}
	}
}

// TestErrorPayloadKeepsOriginalFields 补齐 message 时不能丢掉上游原有字段。
func TestErrorPayloadKeepsOriginalFields(t *testing.T) {
	payload := errorPayload(400, []byte(`{"error":{"code":"invalid_model","type":"AgnesAI_error"},"request_id":"abc"}`))
	m, _ := payload.(map[string]any)
	if m["request_id"] != "abc" {
		t.Errorf("顶层字段应保留，实际 %v", m)
	}
	e, _ := m["error"].(map[string]any)
	if e["code"] != "invalid_model" || e["type"] != "AgnesAI_error" {
		t.Errorf("error 内的原有字段应保留，实际 %v", e)
	}
	if asStr(e["message"]) == "" {
		t.Error("应补上 message")
	}
}
