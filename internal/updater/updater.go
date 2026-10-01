// Package updater checks GitHub releases. Installation is handled externally.
package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
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

// Config controls updater behaviour.
type Config struct {
	// Repo is "owner/repo" for GitHub release lookups. Empty disables checking.
	Repo string
}

// ---------------------------------------------------------------------------
// Updater
// ---------------------------------------------------------------------------

// Updater is a read-only release checker.
type Updater struct {
	cfg       Config
	client    *http.Client
	mu        sync.Mutex
	lastCheck *CheckResult
	version   string
	logger    *log.Logger
}

// New constructs an Updater.
func New(cfg Config, version string, logger *log.Logger) *Updater {
	if logger == nil {
		logger = log.New(os.Stderr, "[updater] ", log.LstdFlags|log.Lmicroseconds)
	}
	u := &Updater{
		cfg:     cfg,
		client:  &http.Client{Timeout: 30 * time.Second},
		version: version,
		logger:  logger,
	}
	return u
}

// Repo 返回版本检查使用的 GitHub 仓库（owner/name），也用于控制台发布页链接。
func (u *Updater) Repo() string { return u.cfg.Repo }

// Version 返回运行中程序的版本。
func (u *Updater) Version() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.version
}

// Check performs a one-shot GitHub release lookup and returns the result.
// An unconfigured repo returns a disabled result without accessing the network.
func (u *Updater) Check(ctx context.Context) (*CheckResult, error) {
	cur := u.Version()
	if u.cfg.Repo == "" {
		return &CheckResult{
			CurrentVersion:    cur,
			IsUpdateAvailable: false,
			Error:             "version checking disabled (no repo configured)",
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

	result := &CheckResult{
		CurrentVersion:    cur,
		LatestVersion:     displayVersion(tag),
		IsUpdateAvailable: isNewer,
		// 带上时分：控制台「发布时间」要能看出这次检查拿到的到底是不是刚发的版本。
		// 用服务器本地时区，用户看到的就是自己 NAS 上的时间。
		ReleaseDate: release.PublishedAt.Local().Format("2006-01-02 15:04"),
		Changelog:   truncate(release.Body, 500),
		Assets:      release.Assets,
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

// StartBackground checks periodically until ctx is canceled. Zero interval disables it.
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
	req.Header.Set("User-Agent", "agnes-hub/"+u.Version())

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

// displayVersion 把 release tag 变成给用户看的版本号：只去掉开头的 v。
//
// 为什么不能直接显示 tag：控制台的「当前版本」来自编译期常量（1.0.13），
// 而 tag 是 v1.0.13 —— 同一个版本两个写法，用户会以为系统没认出来。
// 这里刻意**不**复用 stripPrefix：那个函数还会在第一个 '-' 处截断，
// 是给「比较」用的宽松口径，拿来显示会把 1.0.13-rc1 显示成 1.0.13。
func displayVersion(tag string) string {
	s := strings.TrimSpace(tag)
	if strings.HasPrefix(s, "v") || strings.HasPrefix(s, "V") {
		s = s[1:]
	}
	return s
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
// 助手
// ---------------------------------------------------------------------------

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
