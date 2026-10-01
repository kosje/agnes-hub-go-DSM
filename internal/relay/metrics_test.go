package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"agneshub/internal/config"
)

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

func TestErrorMetricCountsEveryFailedAttemptOnce(t *testing.T) {
	for _, status := range []int{200, 400, 401, 402, 403, 404, 429, 500, 503, 0} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer up.Close()
			store, h, _ := newRelayEnv(t, 1000, up.URL)
			_ = store.UpdateSettings(func(s *config.Settings) { s.RetryMax = 0 })
			h.Reload()
			client := BuildClient()
			if status == 0 {
				client.Transport = failingTransport{}
			}
			res, err := Do(context.Background(), h, client, textOpts(""))
			if err != nil {
				t.Fatal(err)
			}
			res.Close()
			want := int64(1)
			if status == 200 {
				want = 0
			}
			if got := h.Metrics.RequestsError.Load(); got != want {
				t.Fatalf("errors=%d want=%d", got, want)
			}
			if got := h.Metrics.RequestsTotal.Load(); got != 1 {
				t.Fatalf("total=%d", got)
			}
		})
	}
}

func TestErrorMetricCountsFailoverAttempts(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer good.Close()
	store, h, accounts := newRelayEnv(t, 1000, bad.URL, good.URL)
	_ = store.UpdateSettings(func(s *config.Settings) { s.RetryMax = 1 })
	h.Reload()
	store.Bind("metric-session", accounts[0].ID)
	res, err := Do(context.Background(), h, BuildClient(), textOpts("metric-session"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	if res.Status != 200 || res.Attempts != 2 || h.Metrics.RequestsTotal.Load() != 2 || h.Metrics.RequestsError.Load() != 1 {
		t.Fatalf("result=%+v total=%d errors=%d", res, h.Metrics.RequestsTotal.Load(), h.Metrics.RequestsError.Load())
	}
}
