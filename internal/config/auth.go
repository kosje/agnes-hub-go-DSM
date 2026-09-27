package config

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- 口令散列 ----
//
// 新格式：pbkdf2-sha256$<迭代次数>$<hex>。
// 旧格式（1.0.23 及以前）：hex(sha256(salt+password))，单轮、可被 GPU 快速穷举。
// 旧散列仍能校验通过，并在下一次登录成功时原地升级为新格式，用户无感。
//
// 为什么手写 PBKDF2 而不用 crypto/pbkdf2：标准库版本需要 Go 1.24，
// 而 go.mod 刻意声明 1.22（见 go.mod 的说明）。

const (
	pbkdf2Prefix = "pbkdf2-sha256"
	// 12 万轮：NAS 上的低功耗 CPU 单次校验约 0.1 s，对登录无感，对穷举足够慢。
	pbkdf2Iterations = 120000
)

// HashPassword 生成新格式散列。
func HashPassword(password, salt string) string {
	return hashPBKDF2(password, salt, pbkdf2Iterations)
}

func hashPBKDF2(password, salt string, iter int) string {
	key := pbkdf2SHA256([]byte(password), []byte(salt), iter, 32)
	return pbkdf2Prefix + "$" + strconv.Itoa(iter) + "$" + hex.EncodeToString(key)
}

func legacyHash(password, salt string) string {
	sum := sha256.Sum256([]byte(salt + password))
	return hex.EncodeToString(sum[:])
}

// checkPassword 校验口令；legacy=true 表示命中的是旧格式散列，调用方应升级。
func checkPassword(stored, password, salt string) (ok, legacy bool) {
	if stored == "" {
		return false, false
	}
	if !strings.HasPrefix(stored, pbkdf2Prefix+"$") {
		return subtleEqual(stored, legacyHash(password, salt)), true
	}
	parts := strings.Split(stored, "$")
	if len(parts) != 3 {
		return false, false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return false, false
	}
	return subtleEqual(stored, hashPBKDF2(password, salt, iter)), false
}

// pbkdf2SHA256 是 RFC 8018 PBKDF2 的最小实现（PRF = HMAC-SHA256）。
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	var idx [4]byte
	u := make([]byte, hashLen)
	t := make([]byte, hashLen)
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(idx[:], uint32(block))
		prf.Write(idx[:])
		u = prf.Sum(u[:0])
		copy(t, u)
		for n := 1; n < iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for i := range t {
				t[i] ^= u[i]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// ---- 管理员口令 ----

// VerifyPassword 校验管理员口令。命中旧格式散列时顺手升级。
func (s *Store) VerifyPassword(password string) bool {
	s.mu.RLock()
	hash, salt := s.Settings.AdminPasswordHash, s.Settings.AdminPasswordSalt
	s.mu.RUnlock()
	ok, legacy := checkPassword(hash, password, salt)
	if ok && legacy {
		s.mu.Lock()
		if s.Settings.AdminPasswordHash == hash { // 期间没被改过密才升级
			s.Settings.AdminPasswordSalt = randHex(16)
			s.Settings.AdminPasswordHash = HashPassword(password, s.Settings.AdminPasswordSalt)
			_ = s.saveSettingsLocked()
		}
		s.mu.Unlock()
	}
	return ok
}

// SetPassword 改密，并让所有已登录的管理员会话失效。
func (s *Store) SetPassword(password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	salt := randHex(16)
	s.Settings.AdminPasswordSalt = salt
	s.Settings.AdminPasswordHash = HashPassword(password, salt)
	s.Settings.MustChangePassword = false
	s.adminSessions.clear()
	return s.saveSettingsLocked()
}

// MustChangePassword 报告是否仍在使用必须修改的初始口令。
func (s *Store) MustChangePassword() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Settings.MustChangePassword
}

// ---- Chat 访问口令 ----

// HasChatPassword 报告是否设置了 Chat 访问口令。
func (s *Store) HasChatPassword() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Settings.ChatPasswordHash != ""
}

// VerifyChatPassword 校验 Chat 访问口令。未设置口令时一律拒绝 ——
// 此时 Chat 页只接受管理员会话，不存在「无密码即放行」。
func (s *Store) VerifyChatPassword(password string) bool {
	s.mu.RLock()
	hash, salt := s.Settings.ChatPasswordHash, s.Settings.ChatPasswordSalt
	s.mu.RUnlock()
	ok, legacy := checkPassword(hash, password, salt)
	if ok && legacy {
		s.mu.Lock()
		if s.Settings.ChatPasswordHash == hash {
			s.Settings.ChatPasswordSalt = randHex(16)
			s.Settings.ChatPasswordHash = HashPassword(password, s.Settings.ChatPasswordSalt)
			_ = s.saveSettingsLocked()
		}
		s.mu.Unlock()
	}
	return ok
}

// SetChatPassword 设置/清除 Chat 访问口令（空串=清除），并让所有 Chat 会话失效。
func (s *Store) SetChatPassword(password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(password) == "" {
		s.Settings.ChatPasswordHash = ""
		s.Settings.ChatPasswordSalt = ""
	} else {
		salt := randHex(16)
		s.Settings.ChatPasswordSalt = salt
		s.Settings.ChatPasswordHash = HashPassword(password, salt)
	}
	s.chatSessions.clear()
	return s.saveSettingsLocked()
}

// ---- 会话 ----
//
// 会话令牌是 32 字节随机数，只存在内存里：
//   - 与口令散列无关，拿到 settings.json 或 /api/stats 也推不出 cookie；
//   - 退出登录即服务端作废，7 天绝对过期；
//   - 进程重启后需重新登录，这是刻意的取舍（换来令牌永不落盘）。

const (
	SessionTTL     = 7 * 24 * time.Hour
	maxSessions    = 256
	sessionTokenSz = 32
)

type sessionTable struct {
	mu sync.Mutex
	m  map[string]time.Time // token → 过期时间
}

func newSessionTable() *sessionTable { return &sessionTable{m: map[string]time.Time{}} }

func (t *sessionTable) create() string {
	buf := make([]byte, sessionTokenSz)
	if _, err := rand.Read(buf); err != nil {
		return "" // 拿不到安全随机数就不发会话，绝不退化成可预测令牌
	}
	tok := hex.EncodeToString(buf)
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, exp := range t.m {
		if now.After(exp) {
			delete(t.m, k)
		}
	}
	for len(t.m) >= maxSessions { // 超限时淘汰最早过期的
		var oldest string
		var oldestExp time.Time
		for k, exp := range t.m {
			if oldest == "" || exp.Before(oldestExp) {
				oldest, oldestExp = k, exp
			}
		}
		delete(t.m, oldest)
	}
	t.m[tok] = now.Add(SessionTTL)
	return tok
}

func (t *sessionTable) valid(tok string) bool {
	if len(tok) != sessionTokenSz*2 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	exp, ok := t.m[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(t.m, tok)
		return false
	}
	return true
}

func (t *sessionTable) revoke(tok string) {
	t.mu.Lock()
	delete(t.m, tok)
	t.mu.Unlock()
}

func (t *sessionTable) clear() {
	t.mu.Lock()
	t.m = map[string]time.Time{}
	t.mu.Unlock()
}

// NewAdminSession 签发管理员会话令牌；返回空串表示系统随机源不可用。
func (s *Store) NewAdminSession() string { return s.adminSessions.create() }

// ValidAdminSession 校验管理员会话令牌。
func (s *Store) ValidAdminSession(tok string) bool { return s.adminSessions.valid(tok) }

// RevokeAdminSession 作废一个管理员会话（退出登录）。
func (s *Store) RevokeAdminSession(tok string) { s.adminSessions.revoke(tok) }

// NewChatSession 签发 Chat 会话令牌。
func (s *Store) NewChatSession() string { return s.chatSessions.create() }

// ValidChatSession 校验 Chat 会话令牌。
func (s *Store) ValidChatSession(tok string) bool { return s.chatSessions.valid(tok) }

// RevokeChatSession 作废一个 Chat 会话。
func (s *Store) RevokeChatSession(tok string) { s.chatSessions.revoke(tok) }

// ---- 初始口令 ----

// initialPasswordFile 由 DSM 安装向导写入（见 tools/build_spk.py 的 postinst）：
// 用户在安装时填的管理员密码。首次启动读入后立即删除，不留明文。
const initialPasswordFile = ".initial_admin_password"

// consumeInitialPassword 读取并删除安装向导留下的初始口令；没有则返回空串。
func (s *Store) consumeInitialPassword() string {
	p := s.path(initialPasswordFile)
	buf, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	_ = os.Remove(p)
	return strings.TrimRight(string(buf), "\r\n")
}
