package web

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"agneshub/internal/config"
)

// ---------------------------------------------------------------------------
// 会话与鉴权
//
// 两种网页会话：
//   - 管理员会话（agnes_hub_session）：控制台全部接口 + Chat 页；
//   - Chat 会话（agnes_chat_session）：只能用 Chat 页（对话/生图/生视频与其历史），
//     仅在控制台设置了「Chat 访问密码」时才能签发。
//
// 未设置 Chat 访问密码时 Chat 页只认管理员会话 —— 旧版本在这里「无密码即放行」，
// 等于把账号池开放给能访问端口的所有人。
// ---------------------------------------------------------------------------

const cookieName = "agnes_hub_session"
const chatSessionCookie = "agnes_chat_session"

// adminSessionValid 只看管理员会话是否有效，不管是否还欠着「改初始密码」。
func (s *Server) adminSessionValid(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	return s.Store.ValidAdminSession(c.Value)
}

// authed 是管理员接口的门槛：会话有效，且已改掉初始密码。
// 初始密码（admin123）人尽皆知，只在界面上提示改密等于没设防，必须服务端强制。
func (s *Server) authed(r *http.Request) bool {
	return s.adminSessionValid(r) && !s.Store.MustChangePassword()
}

// chatAuthed 检查 Chat 会话。
func (s *Server) chatAuthed(r *http.Request) bool {
	c, err := r.Cookie(chatSessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	return s.Store.ValidChatSession(c.Value)
}

// authedOrChat 同时接受管理员会话或 Chat 会话（Chat 页用到的接口）。
func (s *Server) authedOrChat(r *http.Request) bool {
	return s.authed(r) || s.chatAuthed(r)
}

func (s *Server) deny(w http.ResponseWriter, r *http.Request) {
	if s.adminSessionValid(r) && s.Store.MustChangePassword() {
		writeJSON(w, 403, map[string]any{"error": map[string]any{
			"type": "must_change_password", "message": "请先修改初始管理员密码"}}, nil)
		return
	}
	writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "未登录或会话已失效"}}, nil)
}

func sessionCookie(r *http.Request, name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true,
		// Strict：网页里的请求全是同源 fetch，不需要跨站携带。
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsHTTPS(r),
	}
}

func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(firstHeaderValue(r.Header.Get("X-Forwarded-Proto")), "https")
}

// ---------------------------------------------------------------------------
// 登录限速：同一来源 IP 在窗口期内连续输错 N 次即锁定一段时间。
// 管理员登录、Chat 登录、改密校验旧密码共用一张表。
// ---------------------------------------------------------------------------

const (
	loginMaxFailures = 5
	loginWindow      = 15 * time.Minute
	loginLockout     = 15 * time.Minute
	loginMaxEntries  = 4096
)

type loginAttempt struct {
	failures    int
	first       time.Time
	lockedUntil time.Time
}

type loginLimiter struct {
	mu sync.Mutex
	m  map[string]*loginAttempt
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{m: map[string]*loginAttempt{}} }

// clientIP 取 TCP 对端地址。刻意不信任 X-Forwarded-For：它可被客户端随意伪造，
// 用来绕过限速。经反向代理时所有请求同源，限速退化为全局 —— 宁可误伤不可放过。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// blocked 返回仍需等待的时长；0 表示可以尝试。
func (l *loginLimiter) blocked(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.m[ip]
	if a == nil {
		return 0
	}
	if d := time.Until(a.lockedUntil); d > 0 {
		return d
	}
	return 0
}

func (l *loginLimiter) fail(ip string) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) >= loginMaxEntries {
		for k, a := range l.m {
			if now.Sub(a.first) > loginWindow && now.After(a.lockedUntil) {
				delete(l.m, k)
			}
		}
	}
	a := l.m[ip]
	if a == nil || now.Sub(a.first) > loginWindow {
		a = &loginAttempt{first: now}
		l.m[ip] = a
	}
	a.failures++
	if a.failures >= loginMaxFailures {
		a.lockedUntil = now.Add(loginLockout)
		a.failures = 0
		a.first = now
	}
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	delete(l.m, ip)
	l.mu.Unlock()
}

// checkPassword 在限速保护下校验口令；返回 false 时已写好响应。
func (s *Server) checkPassword(w http.ResponseWriter, r *http.Request, verify func() bool, wrongMsg string) bool {
	ip := clientIP(r)
	if d := s.logins.blocked(ip); d > 0 {
		writeJSON(w, 429, map[string]any{"error": map[string]any{"type": "rate_limit_error",
			"message": fmt.Sprintf("密码错误次数过多，请 %d 分钟后再试", int(d.Minutes())+1)}}, nil)
		return false
	}
	if !verify() {
		s.logins.fail(ip)
		writeJSON(w, 401, map[string]any{"error": map[string]any{"type": "authentication_error",
			"message": wrongMsg}}, nil)
		return false
	}
	s.logins.reset(ip)
	return true
}

// ---------------------------------------------------------------------------
// 同源校验（CSRF）
//
// 网页接口靠 cookie 鉴权，必须挡住「别的页面借用户浏览器发请求」。SameSite 只
// 挡跨站，而同一台 NAS 上别的端口（DSM 5000/5001、其他套件）算同站，照样带 cookie。
// 所以对 /api/ 下的写请求再核对 Origin：浏览器对 POST/PATCH/DELETE 一定会带它，
// 且页面脚本无法伪造。
// ---------------------------------------------------------------------------

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func (s *Server) sameOrigin(r *http.Request) bool {
	if isSafeMethod(r.Method) || !strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		// 没有 Origin：非浏览器客户端（curl、脚本），它们本来就得自己带 cookie。
		// 老浏览器不带 Origin 时退而看 Sec-Fetch-Site。
		site := r.Header.Get("Sec-Fetch-Site")
		return site == "" || site == "same-origin" || site == "none"
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false // 包括 "null"（沙箱 iframe、file:// 页面）
	}
	candidates := []string{r.Host, firstHeaderValue(r.Header.Get("X-Forwarded-Host"))}
	if pub := strings.TrimSpace(s.Store.SettingsSnapshot().PublicBaseURL); pub != "" {
		if pu, err := url.Parse(pub); err == nil {
			candidates = append(candidates, pu.Host)
		}
	}
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if strings.EqualFold(c, u.Host) {
			return true
		}
		// 反向代理（如 NPM 默认配置）会把 Host 改写成不带端口的 $host，
		// 此时只能按主机名比对。直连网关时 Host 一定带端口，走不到这里。
		if _, _, err := net.SplitHostPort(c); err != nil && strings.EqualFold(c, u.Hostname()) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 管理员登录
// ---------------------------------------------------------------------------

func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	pw := asStr(body["password"])
	if !s.checkPassword(w, r, func() bool { return s.Store.VerifyPassword(pw) }, "管理员密码错误") {
		return
	}
	tok := s.Store.NewAdminSession()
	if tok == "" {
		writeErr(w, &apiError{Status: 500, Type: "internal_error", Message: "系统随机源不可用，无法签发会话"})
		return
	}
	http.SetCookie(w, sessionCookie(r, cookieName, tok, int(config.SessionTTL.Seconds())))
	writeJSON(w, 200, map[string]any{"ok": true,
		"must_change_password": s.Store.MustChangePassword()}, nil)
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.Store.RevokeAdminSession(c.Value)
	}
	if c, err := r.Cookie(chatSessionCookie); err == nil {
		s.Store.RevokeChatSession(c.Value)
	}
	http.SetCookie(w, sessionCookie(r, cookieName, "", -1))
	http.SetCookie(w, sessionCookie(r, chatSessionCookie, "", -1))
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

func (s *Server) apiSession(w http.ResponseWriter, r *http.Request) {
	loggedIn := s.adminSessionValid(r)
	writeJSON(w, 200, map[string]any{
		"logged_in": loggedIn,
		// 只告诉已登录的人：未登录者不该知道这台机器是否还在用初始密码。
		"must_change_password": loggedIn && s.Store.MustChangePassword(),
	}, nil)
}

func (s *Server) apiPassword(w http.ResponseWriter, r *http.Request) {
	if !s.adminSessionValid(r) {
		s.deny(w, r)
		return
	}
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	oldPW := asStr(body["old_password"])
	pw := asStr(body["new_password"])
	if len([]rune(pw)) < 6 {
		writeErr(w, badRequest("新密码至少 6 位"))
		return
	}
	if pw == oldPW || pw == "admin123" {
		writeErr(w, badRequest("新密码不能与当前密码或初始密码相同"))
		return
	}
	// 改密必须验证当前密码：否则拿到一次会话（或一次 CSRF）就能永久夺走控制台。
	if !s.checkPassword(w, r, func() bool { return s.Store.VerifyPassword(oldPW) }, "当前密码错误") {
		return
	}
	if err := s.Store.SetPassword(pw); err != nil {
		writeErr(w, &apiError{Status: 500, Type: "internal_error", Message: err.Error()})
		return
	}
	// SetPassword 已作废全部旧会话，给当前浏览器重新签发一个。
	tok := s.Store.NewAdminSession()
	http.SetCookie(w, sessionCookie(r, cookieName, tok, int(config.SessionTTL.Seconds())))
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

// ---------------------------------------------------------------------------
// Chat 页
// ---------------------------------------------------------------------------

// handleChat 总是返回页面本身；登录与否由页面脚本调 /api/chat/session 决定。
// （旧版本在未登录时重定向到 /chat?need_password=1，而那个地址又命中同一个
// 处理函数，形成无限重定向。）
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprint(len(chatHTML)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(chatHTML)
}

func (s *Server) apiChatLogin(w http.ResponseWriter, r *http.Request) {
	body, _, e := readBody(r)
	if e != nil {
		writeErr(w, e)
		return
	}
	if !s.Store.HasChatPassword() {
		writeErr(w, badRequest("未设置 Chat 访问密码，请使用管理员密码登录"))
		return
	}
	pw := asStr(body["password"])
	if !s.checkPassword(w, r, func() bool { return s.Store.VerifyChatPassword(pw) }, "密码错误") {
		return
	}
	tok := s.Store.NewChatSession()
	if tok == "" {
		writeErr(w, &apiError{Status: 500, Type: "internal_error", Message: "系统随机源不可用，无法签发会话"})
		return
	}
	http.SetCookie(w, sessionCookie(r, chatSessionCookie, tok, int(config.SessionTTL.Seconds())))
	writeJSON(w, 200, map[string]any{"ok": true}, nil)
}

func (s *Server) apiChatSession(w http.ResponseWriter, r *http.Request) {
	admin := s.adminSessionValid(r)
	writeJSON(w, 200, map[string]any{
		"requires_password":    s.Store.HasChatPassword(),
		"authenticated":        s.authedOrChat(r),
		"admin":                s.authed(r),
		"must_change_password": admin && s.Store.MustChangePassword(),
	}, nil)
}
