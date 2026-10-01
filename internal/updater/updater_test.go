package updater

import (
	"context"
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

func TestCheck_Disabled(t *testing.T) {
	u := New(Config{}, "1.0.0", nil)
	ctx := context.Background()
	result, err := u.Check(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Error, "disabled") {
		t.Errorf("expected disabled message, got: %s", result.Error)
	}
}

func TestBackground_SkipsWhenDisabled(t *testing.T) {
	u := New(Config{}, "1.0.0", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	u.StartBackground(ctx, 50*time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	// Should not panic or block
}
