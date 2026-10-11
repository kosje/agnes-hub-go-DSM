package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAuditFlushBindingsRetriesFailure(t *testing.T) {
	s := newTestStore(t)
	s.Bind("keep", "acc")
	original := s.Dir
	blocker := filepath.Join(t.TempDir(), "blocker")
	os.WriteFile(blocker, []byte("x"), 0600)
	s.Dir = blocker
	s.FlushBindings()
	s.Dir = original
	s.FlushBindings()
	r, e := NewStore(original)
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := r.BindingsGet("keep"); !ok {
		t.Fatal("dirty flag cleared despite failed write; later flush lost binding")
	}
}

func TestUsageFailedAppendRetriesBatch(t *testing.T) {
	s := newTestStore(t)
	s.AppendUsage(map[string]any{"id": "retained"})
	path := filepath.Join(s.Dir, "usage.jsonl")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushUsage(); err == nil {
		t.Fatal("expected append failure")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	rows := s.TailUsageLive(10)
	if len(rows) != 1 || rows[0]["id"] != "retained" {
		t.Fatalf("rows=%v", rows)
	}
}

func TestUsageConcurrentPruneFlushAndLiveRead(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpdateSettings(func(st *Settings) { st.UsageLogMaxLines = -1 }); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).Format("2006-01-02 15:04:05")
	for i := 0; i < 30; i++ {
		s.AppendUsage(map[string]any{"ts": old, "id": fmt.Sprint("old-", i)})
	}
	if err := s.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				s.AppendUsage(map[string]any{"ts": time.Now().Format("2006-01-02 15:04:05"), "id": fmt.Sprintf("new-%d-%d", worker, i)})
				if err := s.FlushUsage(); err != nil {
					t.Error(err)
				}
				s.TailUsageLive(20)
			}
		}(worker)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			s.pruneUsageLog(time.Now().Add(-24 * time.Hour))
		}
	}()
	wg.Wait()
	if err := s.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	s.pruneUsageLog(time.Now().Add(-24 * time.Hour))
	raw, err := os.ReadFile(filepath.Join(s.Dir, "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		id := row["id"].(string)
		if seen[id] || strings.HasPrefix(id, "old-") {
			t.Fatalf("unexpected record %s", id)
		}
		seen[id] = true
	}
	if len(seen) != 160 {
		t.Fatalf("lost records: retained %d of 160", len(seen))
	}
}
func TestAuditFlushKeysRetriesFailure(t *testing.T) {
	s := newTestStore(t)
	key := s.AddKey("review", []string{"*"}, 0, 0, "")
	s.ChargeKey(key.Key, 7)
	original := s.Dir
	blocker := filepath.Join(t.TempDir(), "blocker")
	os.WriteFile(blocker, []byte("x"), 0600)
	s.Dir = blocker
	s.FlushKeys()
	s.Dir = original
	s.FlushKeys()
	r, e := NewStore(original)
	if e != nil {
		t.Fatal(e)
	}
	if r.KeyByValue(key.Key).UsedTotal != 1 {
		t.Fatal("dirty flag cleared despite failed write; quota use lost")
	}
}
func TestAuditLogBoundEnforcedWithBatches(t *testing.T) {
	s := newTestStore(t)
	s.UpdateSettings(func(st *Settings) { st.UsageLogMaxLines = 3 })
	for i := 0; i < 10; i++ {
		s.AppendUsage(map[string]any{"ts": 1, "id": fmt.Sprint(i)})
	}
	s.FlushUsage()
	raw, e := os.ReadFile(filepath.Join(s.Dir, "usage.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	n := strings.Count(string(raw), "\n")
	if n > 3 {
		t.Fatalf("max=3 but retained %d lines", n)
	}
}
func TestAuditFinalFlushAfterMaintenance(t *testing.T) {
	s := newTestStore(t)
	key := s.AddKey("review", []string{"*"}, 0, 0, "")
	s.FlushKeys()
	s.FlushUsage()
	s.FlushBindings()
	s.FlushAccounts()
	s.ChargeKey(key.Key, 10)
	s.AppendUsage(map[string]any{"ts": 1, "id": "last-handler"})
	if e := s.FlushPending(); e != nil {
		t.Fatal(e)
	}
	r, e := NewStore(s.Dir)
	if e != nil {
		t.Fatal(e)
	}
	if r.KeyByValue(key.Key).UsedTotal != 1 {
		t.Fatal("shutdown final flush omitted keys")
	}
	if _, e = os.Stat(filepath.Join(s.Dir, "usage.jsonl")); e != nil {
		t.Fatal("shutdown final flush omitted usage")
	}
}
