package updater

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVersionGt(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"1.0.1", "1.0.0", true},
		{"1.1.0", "1.0.0", true},
		{"2.0.0", "1.9.9", true},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "1.0.1", false},
		{"v2.0.0", "v1.9.9", true},
		{"1.0.0-go", "1.0.0", false}, // same version, different suffix
		{"1.0.0-linux-amd64", "1.0.0", false},
	}
	for _, tc := range tests {
		got := versionGt(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("versionGt(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestStripPrefix(t *testing.T) {
	tests := []struct{ in, want string }{
		{"v1.2.3", "1.2.3"},
		{"1.2.3-go", "1.2.3"},
		{"1.2.3-linux-amd64", "1.2.3"},
		{"  v2.0.0  ", "2.0.0"},
	}
	for _, tc := range tests {
		got := stripPrefix(tc.in)
		if got != tc.want {
			t.Errorf("stripPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPickAsset(t *testing.T) {
	assets := []ReleaseAsset{
		{Name: "agnes-hub-go.exe", BrowserURL: "https://example.com/win.exe"},
		{Name: "agnes-hub-go-linux-amd64", BrowserURL: "https://example.com/linux-amd64"},
		{Name: "agnes-hub-go-linux-arm64", BrowserURL: "https://example.com/linux-arm64"},
		{Name: "agnes-hub-go-1.0.0.fpk", BrowserURL: "https://example.com/fpk"},
	}

	cases := []struct {
		goos, goarch, wantName string
	}{
		{"windows", "amd64", "agnes-hub-go.exe"},
		{"linux", "amd64", "agnes-hub-go-linux-amd64"},
		{"linux", "arm64", "agnes-hub-go-linux-arm64"},
	}

	for _, tc := range cases {
		a := pickAsset(assets, "agnes-hub-go", tc.goos, tc.goarch)
		if a == nil || a.Name != tc.wantName {
			t.Errorf("pickAsset(%s/%s) = %v, want %s", tc.goos, tc.goarch, a, tc.wantName)
		}
	}
}

func TestCheck_Disabled(t *testing.T) {
	u := New(Config{}, "1.0.0", "/fake/bin", nil)
	ctx := context.Background()
	result, err := u.Check(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Error, "disabled") {
		t.Errorf("expected disabled message, got: %s", result.Error)
	}
}

func TestApply_NoUpdateAvailable(t *testing.T) {
	u := New(Config{Repo: "", BinaryName: "test"}, "1.0.0", "/fake/bin", nil)
	result, err := u.Apply(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected no update available")
	}
}

func TestBackground_SkipsWhenDisabled(t *testing.T) {
	u := New(Config{}, "1.0.0", "/fake/bin", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	u.StartBackground(ctx, 50*time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	// Should not panic or block
}

// checkExecutable 是「别把一页 HTML 当成新版本换上」的最后一道闸。
// 这里两个方向都要锁住：真正的二进制要放行，非二进制要拦下。
func TestCheckExecutable(t *testing.T) {
	dir := t.TempDir()

	notBinary := filepath.Join(dir, "page.html")
	if err := os.WriteFile(notBinary, []byte("<html><body>404 Not Found</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkExecutable(notBinary); err == nil {
		t.Error("HTML 文件应该被拒绝，但它通过了")
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkExecutable(empty); err == nil {
		t.Error("空文件应该被拒绝，但它通过了")
	}

	// 当前测试进程自身就是本平台的合法可执行文件。
	self, err := os.Executable()
	if err != nil {
		t.Skipf("取不到自身路径，跳过正向用例：%v", err)
	}
	if err := checkExecutable(self); err != nil {
		t.Errorf("本测试进程自身应通过魔数校验，却被拒：%v", err)
	}
}

// 助手标记不能误判：普通启动必须返回 false，
// 否则服务会把自己当成替换助手直接退出。
func TestSwapHelperRequested(t *testing.T) {
	orig := os.Args
	defer func() { os.Args = orig }()

	os.Args = []string{"agnes-hub-go", "-host", "127.0.0.1", "-port", "4142"}
	if SwapHelperRequested() {
		t.Error("普通启动被误判为替换助手")
	}

	os.Args = []string{"agnes-hub-go", swapHelperFlag, swapSrcArg, "a", swapDstArg, "b"}
	if !SwapHelperRequested() {
		t.Error("带助手标记的启动没被识别")
	}
}

// 助手必须能在 dst 仍被占用时耐心等待，而不是一脚踩空就放弃。
// 这里用「先占用再释放」模拟父进程退出，验证它会等下去并最终完成替换。
// 平台的锁定语义不同，具体用例放在各自的 _windows_test.go / 通用超时用例里。

// 超时后必须放弃，而且**绝对不能删掉源文件**：
// 留着它下次还能重试，删了就彻底没得救。
func TestRunSwapHelper_GivesUpButKeepsSource(t *testing.T) {
	origTimeout, origPoll := swapHelperTimeout, swapPollInterval
	swapHelperTimeout, swapPollInterval = 300*time.Millisecond, 30*time.Millisecond
	defer func() { swapHelperTimeout, swapPollInterval = origTimeout, origPoll }()

	dir := t.TempDir()
	src := filepath.Join(dir, "new.bin")
	if err := os.WriteFile(src, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 目标目录不存在 → 改名永远失败 → 走超时分支。
	dst := filepath.Join(dir, "no-such-dir", "old.bin")

	orig := os.Args
	defer func() { os.Args = orig }()
	os.Args = []string{"agnes-hub-go", swapHelperFlag,
		swapSrcArg, src, swapDstArg, dst, swapNoRelaunchArg}

	if code := RunSwapHelper(); code != 1 {
		t.Errorf("超时应返回退出码 1，实际 %d", code)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("超时后源文件必须保留，实际已丢失：%v", err)
	}
}

// src/dst 缺失时助手应当快速返回错误码，而不是干等到超时。
func TestRunSwapHelper_BadArgs(t *testing.T) {
	orig := os.Args
	defer func() { os.Args = orig }()

	os.Args = []string{"agnes-hub-go", swapHelperFlag}
	if code := RunSwapHelper(); code != 2 {
		t.Errorf("缺少参数应返回 2，实际 %d", code)
	}
}
