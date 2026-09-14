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

// --- DualWatcher tests ---

func TestDualWatcher_FiresOnGlobalChange(t *testing.T) {
	dir := t.TempDir()
	globalPath := filepath.Join(dir, "config.json")
	localPath := filepath.Join(dir, ".milk", "config.json")
	os.MkdirAll(filepath.Join(dir, ".milk"), 0o700)

	// Write initial configs.
	os.WriteFile(globalPath, []byte(`{"agent":"global"}`), 0o644)
	os.WriteFile(localPath, []byte(`{"colorization":"full"}`), 0o644)

	var (
		mu     sync.Mutex
		gotCfg Config
		fired  = make(chan struct{}, 5)
	)

	dw, err := NewDualWatcher(globalPath, localPath, func(cfg Config, err error) {
		if err != nil {
			return
		}
		mu.Lock()
		gotCfg = cfg
		mu.Unlock()
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatalf("NewDualWatcher: %v", err)
	}
	defer dw.Close()

	// Change global config.
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(globalPath, []byte(`{"agent":"new-global"}`), 0o644)

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("DualWatcher did not fire on global change")
	}

	mu.Lock()
	cfg := gotCfg
	mu.Unlock()

	// Should be merged: new-global agent + local colorization.
	if cfg.Agent != "new-global" {
		t.Errorf("expected new-global, got %q", cfg.Agent)
	}
	if cfg.Colorization != "full" {
		t.Errorf("expected local colorization preserved, got %q", cfg.Colorization)
	}
}

func TestDualWatcher_FiresOnLocalChange(t *testing.T) {
	dir := t.TempDir()
	globalPath := filepath.Join(dir, "config.json")
	localPath := filepath.Join(dir, ".milk", "config.json")
	os.MkdirAll(filepath.Join(dir, ".milk"), 0o700)

	os.WriteFile(globalPath, []byte(`{"agent":"global"}`), 0o644)
	os.WriteFile(localPath, []byte(`{"colorization":"full"}`), 0o644)

	var (
		mu     sync.Mutex
		gotCfg Config
		fired  = make(chan struct{}, 5)
	)

	dw, _ := NewDualWatcher(globalPath, localPath, func(cfg Config, err error) {
		if err != nil {
			return
		}
		mu.Lock()
		gotCfg = cfg
		mu.Unlock()
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	defer dw.Close()

	// Change local config.
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(localPath, []byte(`{"colorization":"off","agent":"local-override"}`), 0o644)

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("DualWatcher did not fire on local change")
	}

	mu.Lock()
	cfg := gotCfg
	mu.Unlock()

	if cfg.Agent != "local-override" {
		t.Errorf("expected local-override, got %q", cfg.Agent)
	}
	if cfg.Colorization != "off" {
		t.Errorf("expected off, got %q", cfg.Colorization)
	}
}

func TestDualWatcher_NoLocalFile(t *testing.T) {
	dir := t.TempDir()
	globalPath := filepath.Join(dir, "config.json")
	localPath := filepath.Join(dir, ".milk", "config.json") // doesn't exist

	os.WriteFile(globalPath, []byte(`{"agent":"global"}`), 0o644)

	var (
		mu     sync.Mutex
		gotCfg Config
		fired  = make(chan struct{}, 5)
	)

	dw, _ := NewDualWatcher(globalPath, localPath, func(cfg Config, err error) {
		if err != nil {
			return
		}
		mu.Lock()
		gotCfg = cfg
		mu.Unlock()
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	defer dw.Close()

	// Change global.
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(globalPath, []byte(`{"agent":"updated"}`), 0o644)

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("DualWatcher did not fire")
	}

	mu.Lock()
	cfg := gotCfg
	mu.Unlock()

	if cfg.Agent != "updated" {
		t.Errorf("expected updated, got %q", cfg.Agent)
	}
}

func TestDualWatcher_LocalDeletionTriggersReload(t *testing.T) {
	dir := t.TempDir()
	globalPath := filepath.Join(dir, "config.json")
	localPath := filepath.Join(dir, ".milk", "config.json")
	os.MkdirAll(filepath.Join(dir, ".milk"), 0o700)

	os.WriteFile(globalPath, []byte(`{"agent":"global"}`), 0o644)
	os.WriteFile(localPath, []byte(`{"agent":"local"}`), 0o644)

	var (
		mu     sync.Mutex
		gotCfg Config
		fired  = make(chan struct{}, 5)
	)

	dw, _ := NewDualWatcher(globalPath, localPath, func(cfg Config, err error) {
		if err != nil {
			return
		}
		mu.Lock()
		gotCfg = cfg
		mu.Unlock()
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	defer dw.Close()

	// Delete local config.
	time.Sleep(10 * time.Millisecond)
	os.Remove(localPath)

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("DualWatcher did not fire on local deletion")
	}

	mu.Lock()
	cfg := gotCfg
	mu.Unlock()

	// Should fall back to global.
	if cfg.Agent != "global" {
		t.Errorf("expected global after local deletion, got %q", cfg.Agent)
	}
}
