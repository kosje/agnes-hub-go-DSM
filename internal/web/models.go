package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"agneshub/internal/config"
	"agneshub/internal/pool"
	"agneshub/internal/relay"
)

// upstreamProbeClient 专用于探测类请求（模型列表等）。
//
// 与 relay 的客户端分开：relay 那套把 CheckRedirect 设成 ErrUseLastResponse
// （要把上游 302 原样透给调用方），而探测请求需要正常跟随重定向 ——
// 有些 relay 会把 /v1/models 跳到别的主机。
var upstreamProbeClient = &http.Client{Timeout: 25 * time.Second}

// ---------------------------------------------------------------------------
// 上游模型清单探测
//
// 控制台里的模型清单是**手填**的：上游改了可售清单之后本地不会知道，
// 表现出来就是「模型明明填了却一直报错」。生视频这条链路尤其容易撞上 ——
// 视频模型的开放程度在各家 relay 上变化最频繁。
//
// 这里拿账号自己的 Key 去问上游的 /v1/models，把「上游实际给的」与
// 「本地清单里填的」对一遍，一眼就能看出是上游停了模型，还是本地填错了。
// ---------------------------------------------------------------------------

// maxUpstreamModelsBody 限制上游模型列表的读取量，防止被一页 HTML 撑爆内存。
const maxUpstreamModelsBody = 2 << 20

// upstreamModelsResult 是单个账号的探测结果。
type upstreamModelsResult struct {
	AccountID   string   `json:"account_id"`
	AccountName string   `json:"account_name"`
	BaseURL     string   `json:"base_url"`
	OK          bool     `json:"ok"`
	Status      int      `json:"status,omitempty"`
	Error       string   `json:"error,omitempty"`
	Count       int      `json:"count"`
	Models      []string `json:"models,omitempty"`
	// Missing 是「本地清单填了、但上游列表里没有」的模型，按模态分组。
	// 这就是「模型不可用」的直接证据。
	Missing map[string][]string `json:"missing,omitempty"`
}

// apiUpstreamModels 向上游拉取可用模型清单。
//
// 请求体可选 {"account_id": "..."}；留空则探测全部启用的账号。
func (s *Server) apiUpstreamModels(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	var req struct {
		AccountID string `json:"account_id"`
	}
	if body, _, err := readBody(r); err == nil && body != nil {
		req.AccountID = strings.TrimSpace(asStr(body["account_id"]))
	}

	settings := s.Store.SettingsSnapshot()
	accounts := s.Store.AccountsSnapshot()

	results := make([]upstreamModelsResult, 0, len(accounts))
	union := map[string]bool{}

	for _, a := range accounts {
		if req.AccountID != "" && a.ID != req.AccountID {
			continue
		}
		if !a.Enabled {
			continue
		}
		res := s.probeUpstreamModels(r.Context(), a, settings)
		results = append(results, res)
		for _, m := range res.Models {
			union[m] = true
		}
	}

	all := make([]string, 0, len(union))
	for m := range union {
		all = append(all, m)
	}
	sort.Strings(all)

	// 按模态把上游列表归一下类，方便直接回答「视频模型还剩哪些」。
	byModality := map[string][]string{"text": {}, "image": {}, "video": {}}
	for _, m := range all {
		switch pool.ModalityOfModel(m, settings.ModelAliases) {
		case "image":
			byModality["image"] = append(byModality["image"], m)
		case "video":
			byModality["video"] = append(byModality["video"], m)
		default:
			byModality["text"] = append(byModality["text"], m)
		}
	}

	writeJSON(w, 200, map[string]any{
		"results":     results,
		"union":       all,
		"by_modality": byModality,
		"probed_at":   time.Now().Format("2006-01-02 15:04:05"),
		"probe_note":  "拿各账号自己的 Key 调上游 GET /v1/models；missing 表示本地清单填了但上游没给",
	}, nil)
}

// probeUpstreamModels 探测单个账号。
func (s *Server) probeUpstreamModels(ctx context.Context, a *config.Account, settings config.Settings) upstreamModelsResult {
	res := upstreamModelsResult{
		AccountID:   a.ID,
		AccountName: a.Name,
		BaseURL:     relay.UpstreamRoot(a),
	}
	if strings.TrimSpace(a.APIKey) == "" {
		res.Error = "账号没有填 API Key"
		return res
	}

	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	url := relay.UpstreamURL(a, "/v1/models")
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	for k, v := range relay.ClientHeaders(a, nil, false) {
		req.Header[k] = v
	}

	// 刻意复用 imageFetchClient 之外的独立客户端：这个请求要跟随重定向
	// （有些 relay 会把 /v1/models 跳到别的主机），不能带上 relay 那套
	// 「原样透传 302」的 CheckRedirect。
	resp, err := upstreamProbeClient.Do(req)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamModelsBody))
	if err != nil {
		res.Error = "读取响应失败：" + err.Error()
		return res
	}
	if resp.StatusCode != http.StatusOK {
		res.Error = upstreamTextSummary(raw)
		return res
	}

	models := parseModelList(raw)
	if len(models) == 0 {
		res.Error = "上游返回的模型列表为空（或不是预期的 {data:[{id:...}]} 结构）。" +
			"原文开头：" + truncateStr(strings.TrimSpace(string(raw)), 200)
		return res
	}
	res.OK = true
	res.Models = models
	res.Count = len(models)
	res.Missing = missingModels(models, a, settings)
	return res
}

// parseModelList 兼容几种常见的模型列表结构：
//
//	{"data":[{"id":"x"}]}   —— OpenAI 官方
//	{"data":["x"]}          —— 部分 relay 直接给字符串
//	{"models":["x"]}        —— 另一批 relay
//	["x"]                   —— 裸数组
func parseModelList(raw []byte) []string {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	add := func(v any) {
		s := strings.TrimSpace(asStr(v))
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	switch d := doc.(type) {
	case []any:
		for _, item := range d {
			add(itemID(item))
		}
	case map[string]any:
		for _, key := range []string{"data", "models"} {
			list, ok := d[key].([]any)
			if !ok {
				continue
			}
			for _, item := range list {
				add(itemID(item))
			}
		}
	}
	sort.Strings(out)
	return out
}

// itemID 从列表元素里取模型名（元素可能是字符串，也可能是 {"id":...} 对象）。
func itemID(item any) string {
	if m, ok := item.(map[string]any); ok {
		for _, k := range []string{"id", "name", "model"} {
			if s := strings.TrimSpace(asStr(m[k])); s != "" {
				return s
			}
		}
		return ""
	}
	return asStr(item)
}

// missingModels 找出本地清单里填了、但上游列表里没有的模型。
func missingModels(upstream []string, a *config.Account, settings config.Settings) map[string][]string {
	have := make(map[string]bool, len(upstream))
	for _, m := range upstream {
		have[m] = true
	}
	manifest := pool.ManifestOf(a, settings)
	out := map[string][]string{}
	for _, mod := range []string{"text", "image", "video"} {
		for _, m := range manifest.For(mod) {
			m = strings.TrimSpace(m)
			if m == "" || have[m] {
				continue
			}
			out[mod] = append(out[mod], m)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
