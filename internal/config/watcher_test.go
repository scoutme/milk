package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestWatcher_FiresOnValidConfig verifies that the watcher calls onChange when
// the file is modified *after* the watcher is created, with a valid parsed config.
func TestWatcher_FiresOnValidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// Write the initial config before creating the watcher (should NOT trigger).
	initial := Config{Agent: "initial-agent"}
	data, _ := json.Marshal(initial)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	var (
		mu     sync.Mutex
		gotCfg Config
		gotErr error
		fired  = make(chan struct{}, 1)
	)

	w, err := NewWatcher(path, func(cfg Config, err error) {
		mu.Lock()
		gotCfg = cfg
		gotErr = err
		mu.Unlock()
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()

	// No firing yet — the watcher primed itself with the initial mod time.
	select {
	case <-fired:
		t.Error("onChange fired before any file change; expected no fire on startup")
	case <-time.After(500 * time.Millisecond):
		// Good — no spurious fire.
	}

	// Now write a changed config.
	time.Sleep(10 * time.Millisecond) // ensure mod time advances
	updated := Config{Agent: "updated-agent"}
	data2, _ := json.Marshal(updated)
	if err := os.WriteFile(path, data2, 0o644); err != nil {
		t.Fatalf("write updated config: %v", err)
	}

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("onChange not called within 2s after update")
	}

	mu.Lock()
	cfg, watchErr := gotCfg, gotErr
	mu.Unlock()

	if watchErr != nil {
		t.Fatalf("unexpected error after update: %v", watchErr)
	}
	if cfg.Agent != "updated-agent" {
		t.Errorf("updated cfg.Agent = %q, want %q", cfg.Agent, "updated-agent")
	}
}

// TestWatcher_FiresWithErrorOnInvalidJSON verifies that onChange is called with
// a non-nil error when the file contains invalid JSON.
func TestWatcher_FiresWithErrorOnInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// Write valid config before watcher creation (primes the mod time).
	if err := os.WriteFile(path, []byte(`{"agent":"x"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var (
		mu    sync.Mutex
		errs  []error
		fired = make(chan struct{}, 5)
	)

	w, err := NewWatcher(path, func(_ Config, err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()

	// Write invalid JSON.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte(`{invalid`), 0o644); err != nil {
		t.Fatalf("write invalid: %v", err)
	}

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("onChange not called after invalid write")
	}

	mu.Lock()
	lastErr := errs[len(errs)-1]
	mu.Unlock()

	if lastErr == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

// TestWatcher_CloseStopsPolling verifies that Close prevents further callbacks.
func TestWatcher_CloseStopsPolling(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"agent":"x"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var count int
	var mu sync.Mutex
	fired := make(chan struct{}, 5)

	w, _ := NewWatcher(path, func(_ Config, _ error) {
		mu.Lock()
		count++
		mu.Unlock()
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	// Trigger one fire by writing.
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(path, []byte(`{"agent":"y"}`), 0o644) //nolint:errcheck

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("onChange not called for first write")
	}

	w.Close()

	// Write after close — should NOT trigger another callback.
	time.Sleep(50 * time.Millisecond)
	os.WriteFile(path, []byte(`{"agent":"z"}`), 0o644) //nolint:errcheck
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	n := count
	mu.Unlock()
	if n > 1 {
		t.Errorf("watcher fired %d times after Close; want 1", n)
	}
}
