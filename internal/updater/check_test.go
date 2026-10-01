package updater

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type releaseTransport func(*http.Request) (*http.Response, error)

func (f releaseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCheckOnlyFetchesReleaseMetadata(t *testing.T) {
	u := New(Config{Repo: "kosje/agnes-hub-go-DSM"}, "1.0.27", nil)
	calls := 0
	u.client.Transport = releaseTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.URL.Host != "api.github.com" || r.URL.Path != "/repos/kosje/agnes-hub-go-DSM/releases/latest" {
			t.Fatalf("unexpected download: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"tag_name":"v1.0.28","assets":[{"name":"agnes-hub-x86_64-1.0.28.spk","browser_download_url":"https://example.com/package.spk"}]}`)), Header: make(http.Header)}, nil
	})
	result, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsUpdateAvailable || result.LatestVersion != "1.0.28" || result.Error != "" || calls != 1 {
		t.Fatalf("result=%+v calls=%d", result, calls)
	}
	if u.Version() != "1.0.27" {
		t.Fatal("version check changed running version")
	}
}
