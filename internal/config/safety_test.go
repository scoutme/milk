package config

// Tests for the #404 write-safety layer: persist guard, consent knob, write
// observer, bookkeeping patch, atomicity and lock behavior. Every test that
// can trip the process-global guard cleans it up — the guard is shared state
// in one test binary, exactly as it is in one milk process.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// tripGuard puts the process into the post-failed-load state: an unparseable
// config with no usable backup.
func tripGuard(t *testing.T) string {
	t.Helper()
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(path); err == nil {
		t.Fatal("expected LoadFrom of corrupt config to fail")
	}
	if !LoadFailed() {
		t.Fatal("guard did not arm after a failed load")
	}
	t.Cleanup(ResetLoadFailedState)
	return path
}

func TestPersistGuard_RefusesAfterFailedLoad(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tripGuard(t)

	if err := Save(Config{Agent: "x"}); !errors.Is(err, ErrPersistRefused) {
		t.Fatalf("Save after failed load = %v, want ErrPersistRefused", err)
	}

	// config_consent: "open" restores the legacy behavior.
	open := Config{Agent: "x", ConfigConsent: "open"}
	if err := Save(open); err != nil {
		t.Fatalf("open consent save: %v", err)
	}
	if !LoadFailed() {
		t.Error("a consent-open save must not claim a successful load happened")
	}

	// The init wizard's forced path writes and clears the guard.
	if err := SaveForced(Config{Agent: "rebuilt", Agents: []AgentConfig{{Name: "rebuilt"}}}); err != nil {
		t.Fatalf("SaveForced: %v", err)
	}
	if LoadFailed() {
		t.Error("forced save must clear the persist guard")
	}
	if err := Save(Config{Agent: "normal"}); err != nil {
		t.Errorf("save after forced recovery: %v", err)
	}
}

func TestPersistGuard_CoversLocalScopeAndScopeDispatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tripGuard(t)

	localDir := t.TempDir()
	if err := SaveLocalIn(Config{Agent: "x"}, localDir); !errors.Is(err, ErrPersistRefused) {
		t.Errorf("SaveLocalIn after failed load = %v, want ErrPersistRefused", err)
	}
	if err := SaveScopeIn(Config{Agent: "x"}, "local", localDir); !errors.Is(err, ErrPersistRefused) {
		t.Errorf("SaveScopeIn(local) after failed load = %v, want ErrPersistRefused", err)
	}
	if err := SaveScopeIn(Config{Agent: "x"}, "global", ""); !errors.Is(err, ErrPersistRefused) {
		t.Errorf("SaveScopeIn(global) after failed load = %v, want ErrPersistRefused", err)
	}
	if err := SaveLocalInForced(Config{Agent: "rebuilt"}, localDir); err != nil {
		t.Fatalf("SaveLocalInForced: %v", err)
	}
	if LoadFailed() {
		t.Error("forced local save must clear the persist guard")
	}
}

func TestPersistGuard_RecoveryAndReadErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := tripGuard(t)

	// A read error (missing file) must NOT clear a genuine trip — greenfield
	// reads stay neutral in both directions.
	if _, err := LoadFrom(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("expected read error for absent file")
	}
	if !LoadFailed() {
		t.Fatal("missing-file read cleared the corruption flag")
	}

	// A usable backup recovers real content → guard clears.
	if err := os.WriteFile(path+configBackupSuffix, []byte(`{"agent":"good"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err == nil {
		t.Fatal("expected wrapped recovery error")
	}
	var rec *ErrConfigRecovered
	if !errors.As(err, &rec) {
		t.Fatalf("err = %v, want ErrConfigRecovered", err)
	}
	if cfg.Agent != "good" {
		t.Errorf("recovered Agent = %q, want good", cfg.Agent)
	}
	if LoadFailed() {
		t.Error("successful recovery must clear the persist guard")
	}
}

func TestConfigConsentOpen_AndDeepMerge(t *testing.T) {
	if (Config{}).ConfigConsentOpen() {
		t.Error("empty consent must default to strict")
	}
	for _, s := range []string{"open", "OPEN", " Open "} {
		if !(Config{ConfigConsent: s}).ConfigConsentOpen() {
			t.Errorf("%q must read as open", s)
		}
	}
	if (Config{ConfigConsent: "strict"}).ConfigConsentOpen() {
		t.Error("strict must not read as open")
	}
	if got := DeepMerge(Config{ConfigConsent: "open"}, Config{}).ConfigConsent; got != "open" {
		t.Errorf("merge dropped consent: %q", got)
	}
	if got := DeepMerge(Config{ConfigConsent: "open"}, Config{ConfigConsent: "strict"}).ConfigConsent; got != "strict" {
		t.Errorf("merge overwrote consent: %q", got)
	}
}

// --- observer ---

func TestSaveObserver_SeesSavedPatchedRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var mu sync.Mutex
	var events []SaveEvent
	SetSaveObserver(func(ev SaveEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	t.Cleanup(func() { SetSaveObserver(nil) })

	if err := Save(Config{Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := PatchBookkeeping("", map[string]string{"update_last_check": "2026-11-05T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	tripGuard(t)
	_ = Save(Config{Agent: "b"}) // refused

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("got %d events (%+v), want 3", len(events), events)
	}
	if events[0].Kind != "saved" || !strings.HasSuffix(events[0].Path, "config.json") || events[0].Bytes == 0 {
		t.Errorf("event[0] = %+v, want a saved event with bytes", events[0])
	}
	if events[1].Kind != "patched" {
		t.Errorf("event[1] = %+v, want patched", events[1])
	}
	if events[2].Kind != "refused" || !errors.Is(events[2].Err, ErrPersistRefused) {
		t.Errorf("event[2] = %+v, want refused ErrPersistRefused", events[2])
	}
}

// --- patch ---

func TestPatchBookkeeping_SurgicalAndRefusing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")

	// Absent file (greenfield): no write, no event, nil.
	if err := PatchBookkeeping("", map[string]string{"update_last_check": "t1"}); err != nil {
		t.Fatalf("absent file: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("patch created a greenfield config")
	}

	// Real config: patch adds only the requested key.
	if err := Save(Config{Agent: "a", Agents: []AgentConfig{{Name: "a", URL: "http://x", Model: "m", Provider: "local"}}}); err != nil {
		t.Fatal(err)
	}
	if err := PatchBookkeeping("", map[string]string{"update_last_check": "2026-11-05T08:57:39Z"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["update_last_check"] != "2026-11-05T08:57:39Z" {
		t.Errorf("stamp not applied: %v", m["update_last_check"])
	}
	agents, ok := m["agents"].([]any)
	if !ok || len(agents) != 1 {
		t.Fatalf("agents lost by patch: %v", m["agents"])
	}

	// No-op patch: value already equal → no event, file untouched.
	var mu sync.Mutex
	var events []SaveEvent
	SetSaveObserver(func(ev SaveEvent) { mu.Lock(); events = append(events, ev); mu.Unlock() })
	t.Cleanup(func() { SetSaveObserver(nil) })
	before, _ := os.Stat(path)
	if err := PatchBookkeeping("", map[string]string{"update_last_check": "2026-11-05T08:57:39Z"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(events) != 0 {
		t.Errorf("no-op patch emitted %+v", events)
	}
	mu.Unlock()
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("no-op patch touched the file")
	}

	// Non-whitelisted key: refused without a write.
	if err := PatchBookkeeping("", map[string]string{"agents": "[]"}); err == nil {
		t.Fatal("patch of a non-bookkeeping key must be rejected")
	}
	data2, _ := os.ReadFile(path)
	if string(data2) != string(data) {
		t.Error("rejected patch modified the file")
	}

	// Corrupt file: patch refuses instead of writing over it.
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PatchBookkeeping("", map[string]string{"update_last_check": "t2"}); err == nil {
		t.Fatal("patch over corrupt file must be refused")
	}
	if got, _ := os.ReadFile(path); string(got) != `{not json` {
		t.Error("refused patch modified the corrupt file")
	}
}

func TestPatchBookkeeping_LocalScopeAndBackupStability(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	localDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(localDir, ".milk"), 0o700); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(localDir, ".milk", "config.json")
	if err := os.WriteFile(localPath, []byte(`{"colorization":"balanced"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A global config exists so scope detection has something to NOT touch.
	dir, _ := Dir()
	globalPath := filepath.Join(dir, "config.json")
	if err := Save(Config{Agent: "global"}); err != nil {
		t.Fatal(err)
	}
	globalBefore, _ := os.ReadFile(globalPath)

	if err := PatchBookkeeping(localDir, map[string]string{"update_last_check": "t1"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(localPath)
	if !strings.Contains(string(got), "update_last_check") || !strings.Contains(string(got), "balanced") {
		t.Errorf("local patch wrong: %s", got)
	}
	globalAfter, _ := os.ReadFile(globalPath)
	if string(globalAfter) != string(globalBefore) {
		t.Error("local-scope patch wrote the global config")
	}

	// A bookkeeping stamp must not churn the backup (#405 stays true for the
	// patch path): rotate skips volatile-only diffs.
	if err := Save(Config{Agent: "global", Agents: []AgentConfig{{Name: "global"}}}); err != nil {
		t.Fatal(err)
	}
	bak := globalPath + configBackupSuffix
	bakBefore, _ := os.ReadFile(bak)
	bakStatBefore, _ := os.Stat(bak)
	if err := PatchBookkeeping("", map[string]string{"update_last_check": "t9"}); err != nil {
		t.Fatal(err)
	}
	bakAfter, _ := os.ReadFile(bak)
	if string(bakAfter) != string(bakBefore) {
		t.Error("bookkeeping patch rewrote the backup")
	}
	bakStatAfter, _ := os.Stat(bak)
	if !bakStatAfter.ModTime().Equal(bakStatBefore.ModTime()) {
		t.Error("bookkeeping patch touched the backup mtime")
	}
}

// --- atomicity / lock ---

func TestSave_AtomicLeavesNoTempResidue(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := Save(Config{Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(Config{Agent: "b"}); err != nil {
		t.Fatal(err)
	}
	dir, _ := Dir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp residue after save: %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal(err)
	}
}

func TestSave_ConcurrentWritersSerialize(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := Save(Config{Agent: "seed"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := Save(Config{Agent: string(rune('a' + i))}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent save: %v", err)
	}
	dir, _ := Dir()
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("final config unparseable (torn write): %v", err)
	}
	if cfg.Agent == "" {
		t.Error("final config lost its agent")
	}
}

func TestLockConfig_AcquireReleaseReacquire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for i := 0; i < 2; i++ {
		release, err := lockConfig(path)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		release()
	}
}
