package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agneshub/internal/config"
	"agneshub/internal/intent"
)

func TestReadBodyLimitAndTrailingJSON(t *testing.T) {
	for _, tc := range []struct {
		name          string
		body          string
		unknownLength bool
		status        int
	}{
		{"at limit", `{}` + strings.Repeat(" ", maxRequestBody-2), false, 0},
		{"known oversize", `{}` + strings.Repeat(" ", maxRequestBody-1), false, 413},
		{"chunked oversize", `{}` + strings.Repeat(" ", maxRequestBody-1), true, 413},
		{"truncated json", `{"a":`, false, 400},
		{"two objects", `{} {}`, false, 400},
		{"trailing garbage", `{}x`, false, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			if tc.unknownLength {
				req.ContentLength = -1
			}
			_, _, err := readBody(req)
			if tc.status == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Status != tc.status {
				t.Fatalf("want status %d, got %+v", tc.status, err)
			}
		})
	}
}

func TestOversizeRequestNeverReachesUpstream(t *testing.T) {
	h := newHarness(t, 0, 1)
	req, err := http.NewRequest(http.MethodPost, h.ts.URL+"/v1/chat/completions", strings.NewReader(`{}`+strings.Repeat(" ", maxRequestBody-1)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.apiKey)
	req.ContentLength = -1
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 413 || !strings.Contains(string(body), "request_too_large") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if n := h.hub.Metrics.RequestsTotal.Load(); n != 0 {
		t.Fatalf("upstream calls=%d", n)
	}
}

func TestIntentCacheSeparatesPathAndAliases(t *testing.T) {
	s := &Server{intentCache: intent.NewCache(0, 0), rules: intent.DefaultRules()}
	settings := config.DefaultSettings()
	body := map[string]any{"prompt": "hello"}
	first := s.decideCached("/v1/images/generations", body, "auto", settings)
	second := s.decideCached("/v1/videos", body, "auto", settings)
	if first.Modality != intent.Image || second.Modality != intent.Video {
		t.Fatalf("cross-path cache reuse: %s / %s", first.Modality, second.Modality)
	}
	settings.ModelAliases = map[string]string{"custom": "agnes-image-2.5-flash"}
	first = s.decideCached("/v1/chat/completions", body, "custom", settings)
	settings.ModelAliases["custom"] = "agnes-video-2.5-flash"
	second = s.decideCached("/v1/chat/completions", body, "custom", settings)
	if first.Modality != intent.Image || second.Modality != intent.Video {
		t.Fatalf("stale alias cache: %s / %s", first.Modality, second.Modality)
	}
	third := s.decideCached("/v1/chat/completions", body, "custom", settings)
	if third.Source != "cache" || third.Modality != intent.Video {
		t.Fatalf("cache hit: %+v", third)
	}
}
