package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestBindingsBatchPersistenceAndClear(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveBindings(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir, "bindings.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		s.Bind(fmt.Sprint(i), "account")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("Bind wrote the full file on the request path")
	}
	if _, ok := s.BindingsGet("49"); !ok {
		t.Fatal("binding not visible immediately")
	}
	if err := s.FlushBindings(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.BindingsSnapshot()) != 50 {
		t.Fatal("batch did not persist")
	}
	s.Bind("pending", "account")
	s.ClearBindings()
	if err := s.FlushBindings(); err != nil {
		t.Fatal(err)
	}
	reloaded, err = NewStore(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.BindingsSnapshot()) != 0 {
		t.Fatal("pending flush resurrected cleared bindings")
	}
}

func TestBindingsRetryFailedFlush(t *testing.T) {
	s := newTestStore(t)
	s.Bind("pending", "account")
	// A file where a directory is required fails on every platform, including root.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	original := s.Dir
	s.Dir = blocker
	if err := s.FlushBindings(); err == nil {
		t.Fatal("expected write failure")
	}
	s.Dir = original
	if err := s.FlushBindings(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.BindingsGet("pending"); !ok {
		t.Fatal("failed flush discarded dirty flag")
	}
}

func TestBindingsConcurrentFlush(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.Bind(fmt.Sprint(i), "account")
			if err := s.FlushBindings(); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if err := s.FlushBindings(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.BindingsSnapshot()) != 20 {
		t.Fatal("concurrent flush lost bindings")
	}
}
