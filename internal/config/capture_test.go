package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
)

func TestConcurrentRawCaptureHasHardBoundAndCompleteJSON(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 150; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.CaptureRawRequest([]byte(fmt.Sprintf(`{"id":%d,"message":"test"}`, i)))
		}(i)
	}
	wg.Wait()
	entries, err := os.ReadDir(s.RawCaptureDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 100 {
		t.Fatalf("retained %d captures instead of 100", len(entries))
	}
	for _, e := range entries {
		raw, err := os.ReadFile(s.path("raw_capture/" + e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(raw) {
			t.Fatalf("partial capture %s", e.Name())
		}
	}
}
