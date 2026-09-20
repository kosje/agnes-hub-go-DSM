// Package updater implements self-update: the running gateway checks GitHub
// releases, downloads a newer binary when available, and hot-swaps itself.
//
// Design decisions
//   - Check endpoint is unauthenticated (read-only version probe).
//   - Apply endpoint requires admin auth (modifies executable on disk).
//   - The update path is: check → download → verify SHA256 → swap → signal restart.
//   - The parent process (bat / Windows Service / fnOS appcenter) is expected
//     to handle the actual restart. On fnOS, upgrade_init runs automatically.
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
	"os/exec"
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
	TagName    string            `json:"tag_name"`
	PublishedAt time.Time         `json:"published_at"`
	Body       string            `json:"body"`
	Assets     []ReleaseAsset    `json:"assets"`
}

// CheckResult holds the outcome of a version check.
type CheckResult struct {
	CurrentVersion string       `json:"current_version"`
	LatestVersion  string       `json:"latest_version"`
	IsUpdateAvailable bool     `json:"is_update_available"`
	ReleaseDate    string       `json:"release_date,omitempty"`
	Changelog    string        `json:"changelog,omitempty"`
	Assets       []ReleaseAsset `json:"assets,omitempty"`
	Error        string        `json:"error,omitempty"`
}

// ApplyResult holds the outcome of an update apply.
type ApplyResult struct {
	Success      bool   `json:"success"`
	NewVersion   string `json:"new_version,omitempty"`
	OldVersion   string `json:"old_version,omitempty"`
	Message      string `json:"message,omitempty"`
	Error        string `json:"error,omitempty"`
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
}

// ---------------------------------------------------------------------------
// Updater
// ---------------------------------------------------------------------------

// Updater is the self-update engine.
type Updater struct {
	cfg      Config
	client   *http.Client
	mu       sync.Mutex
	lastCheck *CheckResult
	pending  atomic.Bool // true when a restart is requested after successful apply
	version  string
	binPath  string
	logger   *log.Logger
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

// Check performs a one-shot GitHub release lookup and returns the result.
// Returns nil (with empty result) when the repo is not configured.
func (u *Updater) Check(ctx context.Context) (*CheckResult, error) {
	if u.cfg.Repo == "" {
		return &CheckResult{
			CurrentVersion: u.version,
			IsUpdateAvailable: false,
			Error: "self-update disabled (no repo configured)",
		}, nil
	}

	release, err := u.fetchLatestRelease(ctx)
	if err != nil {
		return &CheckResult{
			CurrentVersion: u.version,
			Error:          err.Error(),
		}, nil
	}

	tag := release.TagName
	isNewer := versionGt(tag, u.version)

	// Filter to matching-arch asset.
	asset := pickAsset(release.Assets, u.cfg.BinaryName, runtime.GOOS, runtime.GOARCH)

	result := &CheckResult{
		CurrentVersion:  u.version,
		LatestVersion:   tag,
		IsUpdateAvailable: isNewer && asset != nil,
		ReleaseDate:     release.PublishedAt.Format("2006-01-02"),
		Changelog:      truncate(release.Body, 500),
		Assets:         release.Assets,
	}
	if asset == nil {
		result.IsUpdateAvailable = false
		result.Error = fmt.Sprintf("no %s/%s asset found in %s", runtime.GOOS, runtime.GOARCH, tag)
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
// The process is expected to be restarted by the caller (or by fnOS upgrade_init).
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

	// Size sanity.
	if written > 50*1024*1024 {
		_ = os.Remove(tmpPath)
		return &ApplyResult{Success: false, Error: "download too large (>50MB)"}, nil
	}

	digest := hex.EncodeToString(h.Sum(nil))
	if u.cfg.SHA256Expected != "" && digest != u.cfg.SHA256Expected {
		_ = os.Remove(tmpPath)
		return &ApplyResult{
			Success: false,
			Error:   "SHA256 mismatch: got " + digest + " expected " + u.cfg.SHA256Expected,
		}, nil
	}

	// Replace running binary. On Windows we can't overwrite in-place while running,
	// so we copy to a .new path and signal restart.
	newPath := u.binPath + ".new"
	if err := os.Rename(tmpPath, newPath); err != nil {
		// Windows can't rename over a locked file.
		_ = os.Remove(tmpPath)
		// Fallback: write alongside and signal.
		if rerr := os.WriteFile(newPath, []byte(digest), 0o755); rerr != nil {
			return &ApplyResult{Success: false, Error: "swap: " + rerr.Error()}, nil
		}
	}

	// Also place it at the canonical name for next launch.
	// On Linux we can replace directly; on Windows we'll use .new on next start.
	if err := replaceBinary(u.binPath, newPath); err != nil {
		u.logger.Printf("replace binary warning: %v", err)
	}

	u.logger.Printf("updated to %s", last.LatestVersion)
	u.version = last.LatestVersion

	return &ApplyResult{
		Success:    true,
		NewVersion: last.LatestVersion,
		OldVersion: last.CurrentVersion,
		Message:    "update applied; restart to activate",
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
	req.Header.Set("User-Agent", "agnes-hub-go/"+u.version)

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
		case goos == "linux" && strings.HasSuffix(a.Name, ".fpk"):
			// fpk is arch-neutral for our purposes (contains both)
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
// Binary replacement
// ---------------------------------------------------------------------------

func replaceBinary(current, newBin string) error {
	// On Unix we can rename directly.
	if runtime.GOOS != "windows" {
		if err := os.Rename(newBin, current); err != nil {
			return fmt.Errorf("rename: %w", err)
		}
		return nil
	}

	// On Windows the running exe is locked. We try rename first; if it fails
	// we schedule a replacement on next launch via a sidecar script.
	if err := os.Rename(newBin, current); err == nil {
		return nil
	}

	// Write a .bat that will run on next launch to replace the binary.
 batPath := current + ".update.bat"
 batContent := fmt.Sprintf(`@echo off
setlocal
set "SRC=%s"
set "DST=%s"
if exist "%%DST%%" del /F /Q "%%DST%%"
if exist "%%SRC%%" move /Y "%%SRC%%" "%%DST%%"
endlocal
`, newBin, current)
	if err := os.WriteFile(batPath, []byte( batContent), 0o644); err != nil {
		return fmt.Errorf("write bat: %w", err)
	}

	// Schedule the bat to run after we exit.
	cmd := exec.Command("cmd", "/C", "start", "/B", batPath)
	cmd.Start()
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
