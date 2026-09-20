package updater

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestVersionGt(t *testing.T) {
	tests := []struct{ a, b string; want bool }{
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
