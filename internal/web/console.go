package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"agneshub/internal/config"
	"agneshub/internal/intent"
	"agneshub/internal/pool"
	"agneshub/internal/relay"
)

//go:embed static/console.html
var consoleHTML []byte

//go:embed static/chat.html
var chatHTML []byte

//go:embed static/chat.main.js
var chatMainJS []byte

const cookieName = "agnes_hub_session"

func (s *Server) consoleRoutes() {
	m := s.mux
	m.HandleFunc("GET /console", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprint(len(consoleHTML)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(consoleHTML)
	})

	m.HandleFunc("POST /api/login", s.apiLogin)
	m.HandleFunc("POST /api/logout", s.apiLogout)
	m.HandleFunc("GET /api/session", s.apiSession)
	m.HandleFunc("POST /api/password", s.apiPassword)

	m.HandleFunc("GET /api/accounts", s.apiListAccounts)
	m.HandleFunc("POST /api/accounts", s.apiCreateAccount)
	m.HandleFunc("PATCH /api/accounts/{id}", s.apiUpdateAccount)
	m.HandleFunc("DELETE /api/accounts/{id}", s.apiDeleteAccount)
	m.HandleFunc("POST /api/accounts/{id}/test", s.apiTestAccount)
	m.HandleFunc("POST /api/accounts/{id}/reset-factors", s.apiResetFactors)
	m.HandleFunc("POST /api/accounts/bulk", s.apiBulkImport)
	m.HandleFunc("POST /api/accounts/bulk-update", s.apiBulkUpdate)

	m.HandleFunc("GET /api/keys", s.apiListKeys)
	m.HandleFunc("POST /api/keys", s.apiCreateKey)
	m.HandleFunc("PATCH /api/keys", s.apiUpdateKey)
	m.HandleFunc("DELETE /api/keys", s.apiDeleteKey)

	m.HandleFunc("GET /api/stats", s.apiStats)
	m.HandleFunc("GET /api/queue", s.apiQueue)
	m.HandleFunc("GET /api/logs", s.apiLogs)
	m.HandleFunc("GET /api/bindings", s.apiBindings)
	m.HandleFunc("POST /api/bindings/clear", s.apiClearBindings)
	m.HandleFunc("GET /api/video-jobs", s.apiVideoJobs)
	m.HandleFunc("GET /api/image-jobs", s.apiImageJobs)

	m.HandleFunc("GET /api/settings", s.apiGetSettings)
	m.HandleFunc("POST /api/settings", s.apiSetSettings)
	m.HandleFunc("GET /api/export", s.apiExport)
	m.HandleFunc("POST /api/import", s.apiImport)

	m.HandleFunc("POST /api/intent/preview", s.apiIntentPreview)
	m.HandleFunc("POST /api/probe", s.apiProbe)
	m.HandleFunc("GET /api/rpm-table", s.apiRPMTable)

	m.HandleFunc("GET /api/update/status", s.apiUpdateStatus)
	m.HandleFunc("GET /api/update/check", s.apiUpdateCheck)
	m.HandleFunc("POST /api/update/apply", s.apiUpdateApply)

	// 网页端：聊天 / 生图 / 生视频（免第三方 AI Coding 积分）
	m.HandleFunc("GET /chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprint(len(chatHTML)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(chatHTML)
	})
}

// ---------------------------------------------------------------------------
// 会话
// ---------------------------------------------------------------------------

func (s *Server) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	return c.Value == s.Store.SessionToken()
}

func (s *Server) deny(w http.ResponseWriter) {
	writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "未登录或会话已失效"}}, nil)
}

func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	if !s.Store.VerifyPassword(asStr(body["password"])) {
		writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "管理员密码错误"}}, nil)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.Store.SessionToken(),
		Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 7 * 86400})
	writeJSON(w, 200, map[string]any{"ok": true,
		"must_change_password": s.Store.SettingsSnapshot().MustChangePassword}, nil)
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

func (s *Server) apiSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"logged_in":            s.authed(r),
		"must_change_password": s.Store.SettingsSnapshot().MustChangePassword,
	}, nil)
}

func (s *Server) apiPassword(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	pw := asStr(body["new_password"])
	if len([]rune(pw)) < 6 {
		writeErr(w, badRequest("新密码至少 6 位"))
		return
	}
	if err := s.Store.SetPassword(pw); err != nil {
		writeErr(w, &apiError{Status: 500, Type: "internal_error", Message: err.Error()})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.Store.SessionToken(),
		Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 7 * 86400})
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

// ---------------------------------------------------------------------------
// 账号池
// ---------------------------------------------------------------------------

func mask(key string) string {
	if len(key) <= 10 {
		return strings.Repeat("*", len(key))
	}
	return key[:6] + "..." + key[len(key)-4:]
}

func (s *Server) apiListAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	settings := s.Store.SettingsSnapshot()
	out := []map[string]any{}
	for _, a := range s.Store.AccountsSnapshot() {
		base := map[string]any{}
		for _, cls := range config.PoolClasses {
			base[cls] = round2c(baseRPMOf(a, cls))
		}
		eff := map[string]any{}
		declared := map[string]any{}
		for _, cls := range config.PoolClasses {
			eff[cls] = round2c(s.Hub.EffectiveRPM(a, cls))
			declared[cls] = s.Hub.DeclaredModels(a, cls)
		}
		out = append(out, map[string]any{
			"id": a.ID, "name": a.Name, "group": a.Group,
			"base_url": a.BaseURL, "access_type": a.AccessType,
			"enabled": a.Enabled, "class_enabled": a.ClassesEnabled,
			"model_manifest": a.ModelManifest,
			"api_key_masked": mask(a.APIKey),
			"rpm_base":       base, "rpm_effective": eff, "declared": declared,
			"max_concurrency":      a.MaxConcurrency,
			"learned_factor":       round3c(a.LearnedFactor),
			"pool_factors":         a.PoolFactors,
			"penalty_remaining_ms": s.Hub.PenaltyRemaining(a.ID).Milliseconds(),
			"inflight":             s.Hub.Inflight(a.ID),
			"stats":                a.Stats,
		})
	}
	writeJSON(w, 200, map[string]any{
		"accounts": out, "pool_classes": config.PoolClasses,
		"access_types":     config.AccessTypes,
		"default_manifest": settings.ModelManifestDefault,
	}, nil)
}

func baseRPMOf(a *config.Account, poolClass string) float64 {
	table, ok := config.RPMTable[a.AccessType]
	if !ok {
		table = config.RPMTable["free"]
	}
	if v, ok := a.RPMOverrides[poolClass]; ok && v > 0 {
		return v
	}
	if v, ok := table[poolClass]; ok {
		return v
	}
	return 1
}

func (s *Server) apiCreateAccount(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	apiKey := strings.TrimSpace(asStr(body["api_key"]))
	if apiKey == "" {
		writeErr(w, badRequest("api_key 不能为空"))
		return
	}
	var manifest *config.ModelManifest
	if raw, ok := body["model_manifest"].(map[string]any); ok {
		manifest = manifestFromAny(raw)
	}
	account := s.Store.AddAccount(asStr(body["name"]), apiKey,
		asStr(body["access_type"]), asStr(body["base_url"]), manifest)
	if group := strings.TrimSpace(asStr(body["group"])); group != "" {
		s.Store.MutateAccount(account.ID, func(a *config.Account) bool { a.Group = group; return true })
	}
	if v := asInt(body["max_concurrency"]); v > 0 {
		s.Store.MutateAccount(account.ID, func(a *config.Account) bool { a.MaxConcurrency = v; return true })
	}
	if overrides, ok := body["rpm_overrides"].(map[string]any); ok {
		s.Store.MutateAccount(account.ID, func(a *config.Account) bool {
			for k, v := range overrides {
				switch n := v.(type) {
				case float64:
					a.RPMOverrides[k] = n
				case string:
					a.RPMOverrides[k] = parseFloat(n)
				}
			}
			return true
		})
	}
	s.Hub.Reload()
	writeJSON(w, 200, map[string]any{"ok": true, "id": account.ID}, nil)
}

func manifestFromAny(raw map[string]any) *config.ModelManifest {
	m := &config.ModelManifest{}
	if v, ok := raw["text"]; ok {
		m.Text = stringList(v)
	}
	if v, ok := raw["image"]; ok {
		m.Image = stringList(v)
	}
	if v, ok := raw["video"]; ok {
		m.Video = stringList(v)
	}
	return m
}

func (s *Server) apiUpdateAccount(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	id := r.PathValue("id")
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	found := s.Store.MutateAccount(id, func(a *config.Account) bool {
		changed := false
		setStr := func(key string, dst *string) {
			if v, ok := body[key]; ok {
				*dst, changed = asStr(v), true
			}
		}
		setBool := func(key string, dst *bool) {
			if v, ok := body[key]; ok {
				*dst, changed = truthy(v), true
			}
		}
		setStr("name", &a.Name)
		setStr("group", &a.Group)
		setStr("base_url", &a.BaseURL)
		setStr("access_type", &a.AccessType)
		setBool("enabled", &a.Enabled)
		if v, ok := body["api_key"]; ok && strings.TrimSpace(asStr(v)) != "" {
			a.APIKey, changed = strings.TrimSpace(asStr(v)), true
		}
		if v, ok := body["max_concurrency"]; ok {
			if n := asInt(v); n > 0 {
				a.MaxConcurrency, changed = n, true
			}
		}
		if v, ok := body["class_enabled"]; ok {
			a.ClassesEnabled, changed = stringList(v), true
		}
		if v, ok := body["model_manifest"].(map[string]any); ok {
			a.ModelManifest, changed = *manifestFromAny(v), true
		}
		if v, ok := body["rpm_overrides"].(map[string]any); ok {
			next := map[string]float64{}
			for k, raw := range v {
				next[k] = parseFloat(asStr(raw))
			}
			a.RPMOverrides, changed = next, true
		}
		if a.BaseURL != "" {
			a.BaseURL = strings.TrimRight(a.BaseURL, "/")
		}
		return changed
	})
	if !found {
		writeErr(w, &apiError{Status: 404, Type: "invalid_request_error", Message: "账号不存在"})
		return
	}
	if truthy(body["reset_calibration"]) {
		s.Store.MutateAccount(id, func(a *config.Account) bool {
			a.LearnedFactor = 1
			a.LastRateLimited = 0
			return true
		})
		s.Hub.ResetFactors(id)
	}
	s.Hub.Reload()
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

func (s *Server) apiResetFactors(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	id := r.PathValue("id")
	s.Store.MutateAccount(id, func(a *config.Account) bool {
		a.LearnedFactor = 1
		return true
	})
	s.Hub.ResetFactors(id)
	s.Hub.Reload()
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

func (s *Server) apiDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	ok := s.Store.DeleteAccount(r.PathValue("id"))
	s.Hub.Reload()
	writeJSON(w, 200, map[string]any{"ok": ok}, nil)
}

// apiTestAccount 连通性 + 模型权限探测。
func (s *Server) apiTestAccount(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	account := s.Store.AccountByID(r.PathValue("id"))
	if account == nil {
		writeErr(w, &apiError{Status: 404, Type: "invalid_request_error", Message: "账号不存在"})
		return
	}
	settings := s.Store.SettingsSnapshot()
	client := relay.BuildClient()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	started := time.Now()
	var models []string
	planned := []map[string]any{}

	// 1) 拉取上游真实模型清单（零配额，且能直接看出清单是否过期）
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		relay.UpstreamURL(account, "/v1/models"), nil); err == nil {
		for k, v := range relay.ClientHeaders(account, nil, false) {
			req.Header[k] = v
		}
		if resp, err := client.Do(req); err == nil {
			raw := readAllLimited(resp.Body, 1<<20)
			_ = resp.Body.Close()
			if resp.StatusCode < 400 {
				data := decodeMap(raw)
				models = stringList(data["data"])
				if len(models) == 0 {
					for _, m := range mapList(data["data"]) {
						if id := asStr(m["id"]); id != "" {
							models = append(models, id)
						}
					}
				}
			}
		}
	}

	// 2) 用一个最小文本请求验证 Key / 网络 / 模型权限
	payload, _ := json.Marshal(map[string]any{
		"model":      settings.ProbeModel,
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
		"max_tokens": 4, "stream": false,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		relay.UpstreamURL(account, "/v1/chat/completions"), strings.NewReader(string(payload)))
	for k, v := range relay.ClientHeaders(account, nil, false) {
		req.Header[k] = v
	}
	status := 0
	bodyText := ""
	if resp, err := client.Do(req); err == nil {
		raw := readAllLimited(resp.Body, 1<<20)
		_ = resp.Body.Close()
		status = resp.StatusCode
		bodyText = truncateStr(string(raw), 400)
	} else {
		bodyText = err.Error()
	}

	// 3) 清单与真实可用模型做差集，直接指出配置漂移
	declared := pool.ManifestOf(account, settings)
	known := map[string]bool{}
	for _, m := range models {
		known[m] = true
	}
	if len(models) > 0 {
		for name, list := range map[string][]string{
			"text": declared.Text, "image": declared.Image, "video": declared.Video,
		} {
			for _, m := range list {
				if !known[m] {
					planned = append(planned, map[string]any{
						"modality": name, "model": m,
						"issue": "上游模型清单里没有它（可能已下线或拼写错误）",
					})
				}
			}
		}
	}

	writeJSON(w, 200, map[string]any{
		"ok": status > 0 && status < 400, "status": status,
		"latency_ms":  time.Since(started).Milliseconds(),
		"probe_model": settings.ProbeModel,
		"models":      models,
		"drift":       planned,
		"hint":        hintForStatus(status),
		"body":        bodyText,
	}, nil)
}

func hintForStatus(status int) string {
	switch status {
	case 200:
		return "正常"
	case 0:
		return "网络层失败：无法连接上游（检查代理 / DNS / Base URL）"
	case 400:
		return "请求参数问题（检查模型名与请求体）"
	case 401:
		return "API Key 无效 / 格式错误 / 账号状态异常"
	case 402:
		return "余额或配额不足"
	case 403:
		return "无该模型权限 / 网络被策略拦截 / 地区受限"
	case 404:
		return "路径或模型名错误（注意 Base URL 不要重复拼接 /v1）"
	case 429:
		return "已触发 RPM 限流（说明当前有效 RPM 设得过高）"
	case 500, 502, 503:
		return "上游服务异常或波动，可重试"
	}
	return "未知状态码，参考官方 ERROR_CODES.md"
}

// apiBulkImport 批量导入账号：每行 `名称,key[,base_url[,access_type[,group]]]`。
//
// 这是「多账号」真正可用与否的分水岭：注册到 10 个账号后，手工点 50 次表单
// 是不可接受的。导入会**跳过硬校验失败的整行**并逐行回报原因，而不是整体失败。
func (s *Server) apiBulkImport(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	text := asStr(body["text"])
	if strings.TrimSpace(text) == "" {
		writeErr(w, badRequest("text 不能为空"))
		return
	}
	defaultManifest := s.Store.SettingsSnapshot().ModelManifestDefault
	added := []map[string]any{}
	failed := []map[string]any{}

	for idx, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		if len(parts) < 2 || parts[1] == "" {
			failed = append(failed, map[string]any{"line": idx + 1, "text": line,
				"reason": "格式应为「名称,key[,base_url[,access_type[,group]]]」"})
			continue
		}
		name, apiKey := parts[0], parts[1]
		baseURL := config.DefaultBaseURL
		if len(parts) > 2 && parts[2] != "" {
			baseURL = parts[2]
		}
		accessType := "free"
		if len(parts) > 3 && parts[3] != "" {
			accessType = parts[3]
		}
		group := ""
		if len(parts) > 4 {
			group = parts[4]
		}
		if _, ok := config.RPMTable[accessType]; !ok {
			failed = append(failed, map[string]any{"line": idx + 1, "text": line,
				"reason": "未知 access_type：" + accessType})
			continue
		}
		if strings.Contains(apiKey, "sk-") == false && len(apiKey) < 8 {
			failed = append(failed, map[string]any{"line": idx + 1, "text": line, "reason": "key 看起来不合法"})
			continue
		}
		m := defaultManifest.Clone()
		acc := s.Store.AddAccount(name, apiKey, accessType, baseURL, &m)
		if group != "" {
			s.Store.MutateAccount(acc.ID, func(a *config.Account) bool { a.Group = group; return true })
		}
		added = append(added, map[string]any{"id": acc.ID, "name": acc.Name, "group": group})
	}
	s.Hub.Reload()
	writeJSON(w, 200, map[string]any{"ok": true, "added": added, "failed": failed,
		"added_count": len(added), "failed_count": len(failed)}, nil)
}

// apiBulkUpdate 按分组 / ID 列表批量改配置（启用、停用、清单、并发、RPM 覆盖）。
func (s *Server) apiBulkUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	group := asStr(body["group"])
	ids := stringList(body["ids"])
	var targets []string
	for _, a := range s.Store.AccountsSnapshot() {
		if len(ids) > 0 {
			for _, id := range ids {
				if id == a.ID {
					targets = append(targets, a.ID)
				}
			}
			continue
		}
		if group == "" || a.Group == group {
			targets = append(targets, a.ID)
		}
	}
	changed := 0
	for _, id := range targets {
		if s.Store.MutateAccount(id, func(a *config.Account) bool {
			dirty := false
			if v, ok := body["enabled"]; ok {
				a.Enabled, dirty = truthy(v), true
			}
			if v, ok := body["max_concurrency"]; ok {
				if n := asInt(v); n > 0 {
					a.MaxConcurrency, dirty = n, true
				}
			}
			if v, ok := body["model_manifest"].(map[string]any); ok {
				a.ModelManifest, dirty = *manifestFromAny(v), true
			}
			if v, ok := body["class_enabled"]; ok {
				a.ClassesEnabled, dirty = stringList(v), true
			}
			return dirty
		}) {
			changed++
		}
	}
	s.Hub.Reload()
	writeJSON(w, 200, map[string]any{"ok": true, "updated": changed, "matched": len(targets)}, nil)
}

// ---------------------------------------------------------------------------
// 下游密钥
// ---------------------------------------------------------------------------

func (s *Server) apiListKeys(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	writeJSON(w, 200, map[string]any{"keys": s.Store.KeysSnapshot()}, nil)
}

func (s *Server) apiCreateKey(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	item := s.Store.AddKey(asStr(body["name"]), stringList(body["classes"]),
		int64(asInt(body["daily_quota"])), int64(asInt(body["total_quota"])), asStr(body["pinned_account"]))
	writeJSON(w, 200, map[string]any{"ok": true, "key": item}, nil)
}

func (s *Server) apiUpdateKey(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	target := asStr(body["key"])
	ok := s.Store.MutateKey(target, func(k *config.DownstreamKey) bool {
		if v, exists := body["name"]; exists {
			k.Name = asStr(v)
		}
		if v, exists := body["enabled"]; exists {
			k.Enabled = truthy(v)
		}
		if v, exists := body["classes"]; exists {
			k.Classes = stringList(v)
		}
		if v, exists := body["daily_quota"]; exists {
			k.DailyQuota = int64(asInt(v))
		}
		if v, exists := body["total_quota"]; exists {
			k.TotalQuota = int64(asInt(v))
		}
		if v, exists := body["pinned_account"]; exists {
			k.PinnedAccount = asStr(v)
		}
		if truthy(body["reset_usage"]) {
			k.UsedTotal, k.UsedToday = 0, 0
			k.UsedDate = time.Now().Format("2006-01-02")
		}
		return true
	})
	writeJSON(w, 200, map[string]any{"ok": ok}, nil)
}

func (s *Server) apiDeleteKey(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": s.Store.DeleteKey(r.URL.Query().Get("key"))}, nil)
}

// ---------------------------------------------------------------------------
// 观测
// ---------------------------------------------------------------------------

func (s *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	snapshot := s.Hub.Snapshot()
	snapshot["settings"] = s.Store.SettingsSnapshot()
	writeJSON(w, 200, snapshot, nil)
}

func (s *Server) apiQueue(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	writeJSON(w, 200, map[string]any{"queue": s.Hub.QueueView()}, nil)
}

func (s *Server) apiLogs(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	limit := asInt(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	writeJSON(w, 200, map[string]any{"logs": s.Store.TailUsage(limit)}, nil)
}

func (s *Server) apiBindings(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	rows := []map[string]any{}
	for session, b := range s.Store.BindingsSnapshot() {
		name := "已删除"
		if a := s.Store.AccountByID(b.AccountID); a != nil {
			name = a.Name
		}
		rows = append(rows, map[string]any{"session": session, "account": name, "updated": b.Updated})
	}
	writeJSON(w, 200, map[string]any{"bindings": rows}, nil)
}

func (s *Server) apiClearBindings(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	s.Store.ClearBindings()
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

func (s *Server) apiVideoJobs(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	writeJSON(w, 200, map[string]any{"jobs": s.Store.JobsSnapshot()}, nil)
}

func (s *Server) apiImageJobs(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	writeJSON(w, 200, map[string]any{"jobs": s.Store.ImageJobsSnapshot()}, nil)
}

// ---------------------------------------------------------------------------
// 设置
// ---------------------------------------------------------------------------

func (s *Server) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	settings := s.Store.SettingsSnapshot()
	settings.AdminPasswordHash = ""
	settings.AdminPasswordSalt = ""
	writeJSON(w, 200, settings, nil)
}

func (s *Server) apiSetSettings(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	err := s.Store.UpdateSettings(func(st *config.Settings) { applySettings(st, body) })
	if err != nil {
		writeErr(w, &apiError{Status: 500, Type: "internal_error", Message: err.Error()})
		return
	}
	s.Hub.Reload()
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

// applySettings 把控制台提交的字段写入设置（只认白名单，忽略未知键）。
func applySettings(st *config.Settings, p map[string]any) {
	f := func(key string, dst *float64) {
		if v, ok := p[key]; ok {
			*dst = parseFloat(asStr(v))
		}
	}
	i := func(key string, dst *int) {
		if v, ok := p[key]; ok {
			*dst = asInt(v)
		}
	}
	b := func(key string, dst *bool) {
		if v, ok := p[key]; ok {
			*dst = truthy(v)
		}
	}
	s2 := func(key string, dst *string) {
		if v, ok := p[key]; ok {
			*dst = asStr(v)
		}
	}
	f("safety_factor", &st.SafetyFactor)
	f("pacing_window_sec", &st.PacingWindowSec)
	b("calibration_enabled", &st.CalibrationEnabled)
	f("penalty_cooldown_sec", &st.PenaltyCooldownSec)
	f("penalty_factor", &st.PenaltyFactor)
	f("min_learned_factor", &st.MinLearnedFactor)
	f("recover_after_sec", &st.RecoverAfterSec)
	i("recover_successes", &st.RecoverSuccesses)
	i("queue_max_wait_ms", &st.QueueMaxWaitMS)
	i("queue_max_size", &st.QueueMaxSize)
	i("keepalive_interval_ms", &st.KeepaliveMS)
	s2("affinity_mode", &st.AffinityMode)
	s2("default_image_tier", &st.DefaultImageTier)
	s2("optimization_mode", &st.OptimizationMode)
	i("image_record_retention_days", &st.ImageRecordRetention)
	i("retry_max", &st.RetryMax)
	i("retry_base_backoff_ms", &st.RetryBaseBackoffMS)
	i("retry_max_backoff_ms", &st.RetryMaxBackoffMS)
	i("image_concurrency", &st.ImageConcurrency)
	i("video_max_inflight", &st.VideoMaxInFlight)
	i("breaker_revive_sec", &st.BreakerReviveSec)
	s2("video_poll_path", &st.VideoPollPath)
	b("video_poll_include_model_name", &st.VideoPollWithModel)
	i("video_poll_interval_ms", &st.VideoPollIntervalMS)
	i("log_retention_days", &st.LogRetentionDays)
	f("session_ttl_hours", &st.SessionTTLHours)
	s2("probe_model", &st.ProbeModel)

	if v, ok := p["model_aliases"].(map[string]any); ok {
		next := map[string]string{}
		for k, raw := range v {
			next[k] = asStr(raw)
		}
		st.ModelAliases = next
	}
	if v, ok := p["auto_model_name"]; ok && strings.TrimSpace(asStr(v)) != "" {
		st.AutoModelName = asStr(v)
	}
	if v, ok := p["model_manifest_default"].(map[string]any); ok {
		st.ModelManifestDefault = *manifestFromAny(v)
	}
	if v, ok := p["auto_intent"].(map[string]any); ok {
		ai := st.AutoIntent
		if x, ok := v["content_scan"]; ok {
			ai.ContentScan = truthy(x)
		}
		if x, ok := v["min_confidence"]; ok {
			ai.MinConfidence = parseFloat(asStr(x))
		}
		if x, ok := v["default_image_size"]; ok && asStr(x) != "" {
			ai.DefaultImageSize = asStr(x)
		}
		if x, ok := v["image_input_field"]; ok {
			ai.ImageInputField = asStr(x)
		}
		if x, ok := v["video_input_field"]; ok {
			ai.VideoInputField = asStr(x)
		}
		if x, ok := v["video_wait_sec"]; ok {
			ai.VideoWaitSec = asInt(x)
		}
		if x, ok := v["preferred_models"].(map[string]any); ok {
			next := map[string][]string{}
			for k, raw := range x {
				next[k] = stringList(raw)
			}
			ai.PreferredModels = next
		}
		st.AutoIntent = ai
	}
}

// ---------------------------------------------------------------------------
// 迁移
// ---------------------------------------------------------------------------

func (s *Server) apiExport(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	settings := s.Store.SettingsSnapshot()
	settings.AdminPasswordHash = ""
	settings.AdminPasswordSalt = ""
	writeJSON(w, 200, map[string]any{
		"version":     2,
		"exported_at": time.Now().Format("2006-01-02 15:04:05"),
		"accounts":    s.Store.AccountsSnapshot(),
		"keys":        s.Store.KeysSnapshot(),
		"settings":    settings,
	}, nil)
}

func (s *Server) apiImport(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	mode := asStr(body["mode"])
	if mode == "" {
		mode = "replace"
	}
	added := 0
	if raw, ok := body["accounts"].([]any); ok {
		for _, item := range raw {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			apiKey := strings.TrimSpace(asStr(m["api_key"]))
			if apiKey == "" || strings.Contains(apiKey, "...") {
				continue
			}
			acc := s.Store.AddAccount(asStr(m["name"]), apiKey, asStr(m["access_type"]),
				asStr(m["base_url"]), nil)
			s.Store.MutateAccount(acc.ID, func(a *config.Account) bool {
				if v, ok := m["model_manifest"].(map[string]any); ok {
					a.ModelManifest = *manifestFromAny(v)
				}
				if v, ok := m["group"]; ok {
					a.Group = asStr(v)
				}
				if v, ok := m["max_concurrency"]; ok {
					if n := asInt(v); n > 0 {
						a.MaxConcurrency = n
					}
				}
				if v, ok := m["enabled"]; ok {
					a.Enabled = truthy(v)
				}
				return true
			})
			added++
		}
	}
	s.Hub.Reload()
	writeJSON(w, 200, map[string]any{"ok": true, "accounts_imported": added}, nil)
}

// ---------------------------------------------------------------------------
// 意图干跑 与 一键实测
// ---------------------------------------------------------------------------

func (s *Server) apiIntentPreview(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	s.previewIntent(w, body)
}

func (s *Server) previewIntent(w http.ResponseWriter, body map[string]any) {
	settings := s.Store.SettingsSnapshot()
	path := strings.TrimSpace(asStr(body["_path"]))
	if path == "" {
		path = "/v1/chat/completions"
	}
	forced := strings.TrimSpace(asStr(body["_force_modality"]))
	cases := []map[string]any{}
	samples := body["_samples"]
	if list, ok := samples.([]any); ok {
		for _, raw := range list {
			text := asStr(raw)
			payload := map[string]any{
				"model": settings.AutoModelName,
				"messages": []map[string]any{
					{"role": "user", "content": text},
				},
			}
			decision := intent.Decide(path, payload, settings.AutoModelName,
				autoIntentConfig(settings), s.rules, settings.ModelAliases, pool.ModalityOfModel, forced)
			cases = append(cases, map[string]any{
				"input": text, "intent": decision.Modality, "by": decision.Source,
				"label": decision.SourceLabel(), "score": round3c(decision.Score),
				"reason": decision.Reason,
			})
		}
		writeJSON(w, 200, map[string]any{"cases": cases}, nil)
		return
	}

	decision := intent.Decide(path, body, firstNonEmpty(asStr(body["model"]), settings.AutoModelName),
		autoIntentConfig(settings), s.rules, settings.ModelAliases, pool.ModalityOfModel, forced)
	resolved, _ := s.resolveAutoModel(settings, decision.Modality)
	writeJSON(w, 200, map[string]any{
		"intent": decision.Modality, "intent_by": decision.Source,
		"intent_label": decision.SourceLabel(), "intent_score": round3c(decision.Score),
		"intent_reason": decision.Reason, "prompt": decision.Prompt.Text,
		"input_images":                  len(decision.Prompt.Images),
		"model_resolved":                resolved,
		"available_models_for_modality": s.autoModelUnion(settings, decision.Modality),
	}, nil)
}

// apiProbe 对一个账号做 RPM 阶梯实测，返回「零 429 的最高速率」。
//
// 这是唯一会消耗真实配额的接口，因此**必须显式带 confirm:true**。
// 用途：上游悄悄调限额后自动跟上，而不是等任务断了才发现。
func (s *Server) apiProbe(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	if !truthy(body["confirm"]) {
		writeErr(w, &apiError{Status: 400, Type: "invalid_request_error",
			Message: "该操作会消耗真实上游配额，请显式提交 confirm: true"})
		return
	}
	account := s.Store.AccountByID(asStr(body["account_id"]))
	if account == nil {
		writeErr(w, badRequest("账号不存在"))
		return
	}
	modality := firstNonEmpty(asStr(body["modality"]), "text")
	rates := floatList(body["rates"])
	if len(rates) == 0 {
		rates = []float64{18, 22, 30}
	}
	perRate := asInt(body["per_rate"])
	if perRate <= 0 {
		perRate = 7
	}
	results, err := s.probeRates(r.Context(), account, modality, rates, perRate)
	if err != nil {
		writeErr(w, &apiError{Status: 502, Type: "upstream_error", Message: err.Error()})
		return
	}
	best := 0.0
	for _, row := range results {
		if row["rate_limited"] == 0 && row["other_error"] == 0 {
			if rpm := row["target_rpm"].(float64); rpm > best {
				best = rpm
			}
		}
	}
	suggest := map[string]any{}
	if best > 0 {
		suggest["safe_rpm"] = int(best * 0.9)
		suggest["note"] = "已按 10% 安全余量给出建议值，可写入该账号的 rpm_overrides"
	}
	writeJSON(w, 200, map[string]any{"ok": true, "modality": modality,
		"results": results, "highest_clean_rpm": best, "suggest": suggest}, nil)
}

func (s *Server) probeRates(ctx context.Context, account *config.Account, modality string,
	rates []float64, perRate int) ([]map[string]any, error) {

	settings := s.Store.SettingsSnapshot()
	client := relay.BuildClient()
	var results []map[string]any

	for _, rpm := range rates {
		interval := time.Duration(float64(time.Second) * 60.0 / rpm)
		ok, limited, other := 0, 0, 0
		started := time.Now()
		for i := 0; i < perRate; i++ {
			if i > 0 {
				select {
				case <-time.After(interval):
				case <-ctx.Done():
					return results, ctx.Err()
				}
			}
			path, payload := probePayload(modality, account, settings)
			raw, _ := json.Marshal(payload)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				relay.UpstreamURL(account, path), strings.NewReader(string(raw)))
			if err != nil {
				other++
				continue
			}
			for k, v := range relay.ClientHeaders(account, nil, false) {
				req.Header[k] = v
			}
			resp, err := client.Do(req)
			if err != nil {
				other++
				continue
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
			switch {
			case resp.StatusCode == 200:
				ok++
			case resp.StatusCode == 429:
				limited++
			default:
				other++
			}
		}
		elapsed := time.Since(started).Seconds()
		observed := 0.0
		if elapsed > 0 {
			observed = float64(ok) * 60 / elapsed
		}
		results = append(results, map[string]any{
			"target_rpm": rpm, "sent": perRate, "ok": ok,
			"rate_limited": limited, "other_error": other,
			"elapsed_sec": round2c(elapsed), "effective_rpm_observed": round2c(observed),
		})
	}
	return results, nil
}

func probePayload(modality string, account *config.Account, settings config.Settings) (string, map[string]any) {
	manifest := pool.ManifestOf(account, settings)
	pick := func(list []string, fallback string) string {
		if len(list) > 0 {
			return list[0]
		}
		return fallback
	}
	switch modality {
	case "image":
		return "/v1/images/generations", map[string]any{
			"model":  pick(manifest.Image, pool.FallbackModel["image"]),
			"prompt": "a small blue circle icon", "size": "1K",
		}
	case "video":
		return "/v1/videos", map[string]any{
			"model":  pick(manifest.Video, pool.FallbackModel["video"]),
			"prompt": "a one second clip of a blue circle",
		}
	default:
		return "/v1/chat/completions", map[string]any{
			"model":      pick(manifest.Text, settings.ProbeModel),
			"messages":   []map[string]any{{"role": "user", "content": "ping"}},
			"max_tokens": 1, "stream": false,
		}
	}
}

func (s *Server) apiRPMTable(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	writeJSON(w, 200, map[string]any{
		"table": config.RPMTable, "pool_classes": config.PoolClasses,
		"access_types": config.AccessTypes,
		"note":         "官方限流表按「模型类型」而非单个模型 ID 给出；标称值与实际可执行值不一致，故默认留安全余量",
	}, nil)
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func readAllLimited(rc interface{ Read([]byte) (int, error) }, limit int64) []byte {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	var total int64
	for total < limit {
		n, err := rc.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			total += int64(n)
		}
		if err != nil {
			break
		}
	}
	return buf
}

func stringList(v any) []string {
	switch t := v.(type) {
	case []string:
		return append([]string(nil), t...)
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			switch x := item.(type) {
			case string:
				out = append(out, x)
			case map[string]any:
				if id := asStr(x["id"]); id != "" {
					out = append(out, id)
				}
			default:
				if s := asStr(item); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		parts := strings.Split(t, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if s := strings.TrimSpace(p); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func floatList(v any) []float64 {
	list := stringList(v)
	out := make([]float64, 0, len(list))
	for _, s := range list {
		out = append(out, parseFloat(s))
	}
	return out
}

func asInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	case string:
		return int(parseFloat(t))
	case bool:
		if t {
			return 1
		}
	}
	return 0
}

func parseFloat(s string) float64 {
	var f float64
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%g", &f)
	if err != nil {
		return 0
	}
	return f
}

func round2c(v float64) float64 { return float64(int(v*100+0.5)) / 100 }
func round3c(v float64) float64 { return float64(int(v*1000+0.5)) / 1000 }

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---------------------------------------------------------------------------
// 自更新
// ---------------------------------------------------------------------------

// updateStatus 汇总当前版本与最近一次检查结果，供控制台展示。
func (s *Server) updateStatus() map[string]any {
	cur := s.Version
	if s.Updater != nil {
		cur = s.Updater.Version()
	}
	out := map[string]any{
		"current_version": cur,
		"enabled":         s.Updater != nil,
	}
	if s.Updater == nil {
		return out
	}
	if last := s.Updater.LastCheck(); last != nil {
		out["last_check"] = last
	}
	return out
}

// apiUpdateStatus 返回当前版本与最近一次检查结果（不主动联网）。
func (s *Server) apiUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	writeJSON(w, 200, s.updateStatus(), nil)
}

// apiUpdateCheck 立即向 GitHub 查一次最新版本。
func (s *Server) apiUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	if s.Updater == nil {
		writeJSON(w, 200, map[string]any{
			"current_version": s.Version,
			"enabled":         false,
			"error":           "自更新未启用",
		}, nil)
		return
	}
	result, err := s.Updater.Check(r.Context())
	if err != nil {
		writeJSON(w, 200, map[string]any{
			"current_version": s.Updater.Version(),
			"error":           err.Error(),
		}, nil)
		return
	}
	writeJSON(w, 200, result, nil)
}

// apiUpdateApply 下载并替换二进制。
//
// 必须有管理员会话：这是唯一会改动磁盘上可执行文件的接口。
// 替换成功后置重启标志，主循环监听到就优雅退出 ——
// Windows 上随后由助手进程完成替换并重启，类 Unix 上由调用方重启。
func (s *Server) apiUpdateApply(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	if s.Updater == nil {
		writeJSON(w, 200, map[string]any{
			"success": false,
			"error":   "自更新未启用",
		}, nil)
		return
	}
	result, err := s.Updater.Apply(r.Context())
	if err != nil {
		writeJSON(w, 200, map[string]any{"success": false, "error": err.Error()}, nil)
		return
	}
	if result.Success {
		s.Updater.RequestRestart()
	}
	writeJSON(w, 200, result, nil)
}
