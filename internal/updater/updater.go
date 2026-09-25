// Package updater implements self-update: the running gateway checks GitHub
// releases, downloads a newer binary when available, and hot-swaps itself.
//
// Design decisions
//   - Check endpoint is unauthenticated (read-only version probe).
//   - Apply endpoint requires admin auth (modifies executable on disk).
//   - The update path is: check → download → verify SHA256 → swap → signal restart.
//   - The parent process (bat / Windows Service) is expected to handle the
//     actual restart.
//   - On Windows we replace the .exe only after all in-flight requests finish
//     (graceful shutdown signal first, then swap).
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// ReleaseAsset is a single downloadable file from a GitHub release.
type ReleaseAsset struct {
	Name          string `json:"name"`
	BrowserURL    string `json:"browser_download_url"`
	ContentType   string `json:"content_type"`
	Size          int64  `json:"size"`
	DownloadCount int    `json:"download_count"`
}

// GitHubRelease is the shape we care about from a GitHub release JSON.
type GitHubRelease struct {
	TagName     string         `json:"tag_name"`
	PublishedAt time.Time      `json:"published_at"`
	Body        string         `json:"body"`
	Assets      []ReleaseAsset `json:"assets"`
}

// CheckResult holds the outcome of a version check.
type CheckResult struct {
	CurrentVersion    string         `json:"current_version"`
	LatestVersion     string         `json:"latest_version"`
	IsUpdateAvailable bool           `json:"is_update_available"`
	ReleaseDate       string         `json:"release_date,omitempty"`
	Changelog         string         `json:"changelog,omitempty"`
	Assets            []ReleaseAsset `json:"assets,omitempty"`
	Error             string         `json:"error,omitempty"`
}

// ApplyResult holds the outcome of an update apply.
type ApplyResult struct {
	Success    bool   `json:"success"`
	NewVersion string `json:"new_version,omitempty"`
	OldVersion string `json:"old_version,omitempty"`
	Message    string `json:"message,omitempty"`
	Error      string `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

// Config controls updater behaviour.
type Config struct {
	// Repo is "owner/repo" for GitHub release lookups. Empty disables checking.
	Repo string
	// BinaryName is the name of the executable to replace (e.g. "agnes-hub-go").
	BinaryName string
	// DataDir is where runtime data lives; updates must not touch it.
	DataDir string
	// CheckInterval is how often to auto-check in background. 0 = disabled.
	CheckInterval time.Duration
	// AllowPreRelease controls whether pre-release tags are considered.
	AllowPreRelease bool
	// SHA256Expected is an optional known-good hash to verify against.
	SHA256Expected string
	// NoRelaunch 只影响 Windows：置 true 时替换完成后不自动把新版本拉起来，
	// 交给调用方（服务管理器 / 用户）决定何时重启。默认 false = 自动重启。
	NoRelaunch bool
}

// ---------------------------------------------------------------------------
// Updater
// ---------------------------------------------------------------------------

// Updater is the self-update engine.
type Updater struct {
	cfg       Config
	client    *http.Client
	mu        sync.Mutex
	lastCheck *CheckResult
	pending   atomic.Bool // true when a restart is requested after successful apply
	version   string
	binPath   string
	logger    *log.Logger
}

// New constructs an Updater.
func New(cfg Config, version, binPath string, logger *log.Logger) *Updater {
	if logger == nil {
		logger = log.New(os.Stderr, "[updater] ", log.LstdFlags|log.Lmicroseconds)
	}
	u := &Updater{
		cfg:     cfg,
		client:  &http.Client{Timeout: 30 * time.Second},
		version: version,
		binPath: binPath,
		logger:  logger,
	}
	return u
}

// Repo 返回配置的 GitHub 仓库（owner/name）。空字符串表示自更新未启用。
// 控制台用它拼「查看全部版本」的跳转地址，保证链接与自更新实际拉取的仓库一致。
func (u *Updater) Repo() string { return u.cfg.Repo }

// Version 返回当前版本。加锁是因为应用更新后会在运行期改写它。
func (u *Updater) Version() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.version
}

func (u *Updater) setVersion(v string) {
	u.mu.Lock()
	u.version = v
	u.mu.Unlock()
}

// Check performs a one-shot GitHub release lookup and returns the result.
// Returns nil (with empty result) when the repo is not configured.
func (u *Updater) Check(ctx context.Context) (*CheckResult, error) {
	cur := u.Version()
	if u.cfg.Repo == "" {
		return &CheckResult{
			CurrentVersion:    cur,
			IsUpdateAvailable: false,
			Error:             "self-update disabled (no repo configured)",
		}, nil
	}

	release, err := u.fetchLatestRelease(ctx)
	if err != nil {
		return &CheckResult{
			CurrentVersion: cur,
			Error:          err.Error(),
		}, nil
	}

	tag := release.TagName
	isNewer := versionGt(tag, cur)

	// 只挑本平台能用的那个资产；挑不到就不算「有更新」，
	// 否则点了更新只会在 Apply 阶段失败。
	asset := pickAsset(release.Assets, u.cfg.BinaryName, runtime.GOOS, runtime.GOARCH)

	result := &CheckResult{
		CurrentVersion:    cur,
		LatestVersion:     tag,
		IsUpdateAvailable: isNewer && asset != nil,
		ReleaseDate:       release.PublishedAt.Format("2006-01-02"),
		Changelog:         truncate(release.Body, 500),
		Assets:            release.Assets,
	}
	// 只有「确实有新版本、但没提供本平台的包」才值得报错；
	// 已经是最新版时资产列表里没有本平台的包是正常的，不该弹错误。
	if isNewer && asset == nil {
		result.Error = fmt.Sprintf("release %s 没有 %s/%s 的资产", tag, runtime.GOOS, runtime.GOARCH)
	}

	u.mu.Lock()
	u.lastCheck = result
	u.mu.Unlock()
	return result, nil
}

// LastCheck returns the most recent check result (may be nil).
func (u *Updater) LastCheck() *CheckResult {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastCheck
}

// Apply downloads the new binary, verifies it, and replaces the running one.
// The process is expected to be restarted by the caller.
func (u *Updater) Apply(ctx context.Context) (*ApplyResult, error) {
	last := u.LastCheck()
	if last == nil || !last.IsUpdateAvailable {
		return &ApplyResult{
			Success: false,
			Error:   "no update available",
		}, nil
	}

	asset := pickAsset(last.Assets, u.cfg.BinaryName, runtime.GOOS, runtime.GOARCH)
	if asset == nil {
		return &ApplyResult{
			Success: false,
			Error:   "no suitable asset for this platform",
		}, nil
	}

	u.logger.Printf("downloading %s → %s", asset.BrowserURL, u.cfg.BinaryName)

	// Download to a temp file first (atomic-ish).
	tmpDir := u.cfg.DataDir
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	tmpPath := filepath.Join(tmpDir, ".update-tmp-"+strings.ReplaceAll(asset.Name, ".", "-"))

	resp, err := u.client.Get(asset.BrowserURL)
	if err != nil {
		return &ApplyResult{Success: false, Error: "download failed: " + err.Error()}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &ApplyResult{Success: false, Error: "HTTP " + fmt.Sprint(resp.StatusCode)}, nil
	}

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return &ApplyResult{Success: false, Error: "create temp: " + err.Error()}, nil
	}

	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	f.Close()
	if err != nil {
		_ = os.Remove(tmpPath)
		return &ApplyResult{Success: false, Error: "write: " + err.Error()}, nil
	}

	// 尺寸合理性：下载被截断、或拿到的是错误页/重定向页面时都会明显偏小。
	// 上界防「下成别的大文件」，下界防「下成一段 HTML 就当二进制换上」。
	if written < 32*1024 || written > 50*1024*1024 {
		_ = os.Remove(tmpPath)
		return &ApplyResult{Success: false,
			Error: fmt.Sprintf("downloaded %d bytes, out of sane range (32KB-50MB)", written)}, nil
	}

	// 魔数校验：SHA256 只有在调用方显式给了期望值时才比得了，
	// 那时才挡不住「GitHub 返回一页 HTML 却被当成新版本」。
	if err := checkExecutable(tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return &ApplyResult{Success: false, Error: err.Error()}, nil
	}

	digest := hex.EncodeToString(h.Sum(nil))
	if u.cfg.SHA256Expected != "" && digest != u.cfg.SHA256Expected {
		_ = os.Remove(tmpPath)
		return &ApplyResult{
			Success: false,
			Error:   "SHA256 mismatch: got " + digest + " expected " + u.cfg.SHA256Expected,
		}, nil
	}

	// 先把新二进制放到正式文件旁边的 .new：
	// 这一步只是改名，不碰正在运行的旧文件，任何平台都能成功。
	newPath := u.binPath + ".new"
	if err := os.Rename(tmpPath, newPath); err != nil {
		_ = os.Remove(tmpPath)
		return &ApplyResult{Success: false,
			Error: "stage new binary to " + filepath.Base(newPath) + ": " + err.Error()}, nil
	}

	// 再让平台各自决定怎么把它变成「正在运行的那个名字」。
	// 类 Unix 就地 rename 即可；Windows 必须等本进程退出，见 swap_windows.go。
	if err := replaceBinary(u.binPath, newPath, u.cfg.NoRelaunch); err != nil {
		u.logger.Printf("replace binary failed: %v", err)
		return &ApplyResult{Success: false,
			Error: "downloaded and verified, but swap failed: " + err.Error()}, nil
	}

	u.setVersion(last.LatestVersion)
	u.logger.Printf("updated %s -> %s (sha256 %s)", last.CurrentVersion, last.LatestVersion, digest[:12])

	msg := "update applied; restart to activate"
	if runtime.GOOS == "windows" {
		msg = "update staged; 旧进程退出后自动替换并重启"
	}
	return &ApplyResult{
		Success:    true,
		NewVersion: last.LatestVersion,
		OldVersion: last.CurrentVersion,
		Message:    msg,
	}, nil
}

// RequestRestart sets a flag that tells the caller to exit gracefully.
// The web server's main should watch this and call cancel() when set.
func (u *Updater) RequestRestart() {
	u.pending.Store(true)
}

// NeedRestart returns true after Apply succeeded and the caller should exit.
func (u *Updater) NeedRestart() bool {
	return u.pending.Load()
}

// ClearPending clears the restart flag (e.g. after a failed restart attempt).
func (u *Updater) ClearPending() {
	u.pending.Store(false)
}

// ---------------------------------------------------------------------------
// Background checker
// ---------------------------------------------------------------------------

// StartBackground checks periodically and returns a cleanup func.
// Pass 0 interval to disable.
func (u *Updater) StartBackground(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := u.Check(ctx); err != nil {
					u.logger.Printf("check error: %v", err)
				}
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// GitHub API
// ---------------------------------------------------------------------------

func (u *Updater) fetchLatestRelease(ctx context.Context) (*GitHubRelease, error) {
	if u.cfg.Repo == "" {
		return nil, errors.New("repo not configured")
	}
	url := "https://api.github.com/repos/" + u.cfg.Repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "baiPiao-hub/"+u.Version())

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var release GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, err
	}
	return &release, nil
}

// ---------------------------------------------------------------------------
// Asset selection
// ---------------------------------------------------------------------------

func pickAsset(assets []ReleaseAsset, binaryName, goos, goarch string) *ReleaseAsset {
	// Prefer exact match first.
	for i := range assets {
		a := &assets[i]
		if !strings.Contains(a.Name, binaryName) {
			continue
		}
		switch {
		case goos == "windows" && strings.HasSuffix(a.Name, ".exe"):
			return a
		case goos == "linux" && goarch == "amd64" && strings.Contains(a.Name, "amd64"):
			return a
		case goos == "linux" && goarch == "arm64" && strings.Contains(a.Name, "arm64"):
			return a
		}
	}
	// Fallback: any asset with the binary name.
	for i := range assets {
		if strings.Contains(assets[i].Name, binaryName) {
			return &assets[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Version comparison
// ---------------------------------------------------------------------------

// versionGt returns true if v1 is strictly newer than v2.
// Handles prefixes like "v1.0.0" and "1.0.0-go".
func versionGt(v1, v2 string) bool {
	n1 := stripPrefix(v1)
	n2 := stripPrefix(v2)
	p1 := parseVersion(n1)
	p2 := parseVersion(n2)
	return compareParts(p1, p2) > 0
}

func stripPrefix(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "v") {
		s = s[1:]
	}
	// Strip trailing suffix like "-go" or "-linux-amd64".
	if i := strings.Index(s, "-"); i >= 0 {
		s = s[:i]
	}
	return s
}

type versionParts [3]int

func parseVersion(s string) versionParts {
	var p versionParts
	fmt.Sscanf(s, "%d.%d.%d", &p[0], &p[1], &p[2])
	return p
}

func compareParts(a, b versionParts) int {
	for i := 0; i < 3; i++ {
		if a[i] > b[i] {
			return 1
		}
		if a[i] < b[i] {
			return -1
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// 二进制校验
// ---------------------------------------------------------------------------

// checkExecutable 校验文件开头的魔数，确认它确实是本平台的二进制。
//
// 为什么不能只靠 SHA256：SHA256 只在调用方事先知道期望值时才起作用，
// 而自动更新场景下「期望值」恰恰只能从 release 页面拿，等于形同虚设。
// 魔数校验很便宜，却能挡住最常见的失败模式 —— 把一页 HTML 错误信息
// 或一段重定向内容当成新版本换上，让服务再也起不来。
func checkExecutable(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open downloaded file: %w", err)
	}
	defer f.Close()

	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		return fmt.Errorf("read magic of downloaded file: %w", err)
	}

	var want string
	var ok bool
	switch runtime.GOOS {
	case "windows":
		want = "MZ"
		ok = head[0] == 0x4D && head[1] == 0x5A
	case "linux":
		want = "ELF"
		ok = head[0] == 0x7F && head[1] == 'E' && head[2] == 'L' && head[3] == 'F'
	case "darwin":
		want = "Mach-O"
		ok = (head[0] == 0xFE && head[1] == 0xED) || // 32/64 位 fat 之外的常见形态
			(head[0] == 0xCF && head[1] == 0xFA) || // 64 位 Mach-O
			(head[0] == 0xCA && head[1] == 0xFE) // fat / universal
	default:
		return nil // 未知平台不拦，交给调用方
	}
	if !ok {
		return fmt.Errorf("downloaded file is not a %s executable (magic % X, want %s)",
			runtime.GOOS, head, want)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 助手
// ---------------------------------------------------------------------------

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
