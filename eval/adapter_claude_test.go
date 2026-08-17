package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withTempActivityState redirects claudeActivityStatePath to a file under a
// fresh temp dir for the duration of the test, so tests never touch the real
// ~/.milk state file, and restores the original afterward.
func withTempActivityState(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-cache-activity")
	orig := claudeActivityStatePath
	claudeActivityStatePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { claudeActivityStatePath = orig })
	return path
}

func writeActivityTimestamp(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(when.Format(time.RFC3339Nano)), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeAdapter_SetArgs_CacheCooldown(t *testing.T) {
	a := &claudeAdapter{}
	a.SetArgs([]string{"--foo", "bar", "--cache-cooldown", "5m", "--baz"})

	if a.cacheCooldownErr != nil {
		t.Fatalf("unexpected error: %v", a.cacheCooldownErr)
	}
	if a.cacheCooldown != 5*time.Minute {
		t.Errorf("got cacheCooldown=%v, want 5m", a.cacheCooldown)
	}
	want := []string{"--foo", "bar", "--baz"}
	if len(a.extraArgs) != len(want) {
		t.Fatalf("got extraArgs=%v, want %v", a.extraArgs, want)
	}
	for i, w := range want {
		if a.extraArgs[i] != w {
			t.Errorf("extraArgs[%d] = %q, want %q", i, a.extraArgs[i], w)
		}
	}
}

func TestClaudeAdapter_SetArgs_NoCooldown(t *testing.T) {
	a := &claudeAdapter{}
	a.SetArgs([]string{"--agent", "mimo-local"})

	if a.cacheCooldownErr != nil {
		t.Fatalf("unexpected error: %v", a.cacheCooldownErr)
	}
	if a.cacheCooldown != 0 {
		t.Errorf("got cacheCooldown=%v, want 0 (unset)", a.cacheCooldown)
	}
	if len(a.extraArgs) != 2 || a.extraArgs[0] != "--agent" || a.extraArgs[1] != "mimo-local" {
		t.Errorf("extraArgs mutated unexpectedly: %v", a.extraArgs)
	}
}

func TestClaudeAdapter_SetArgs_CacheCooldown_InvalidDuration(t *testing.T) {
	a := &claudeAdapter{}
	a.SetArgs([]string{"--cache-cooldown", "not-a-duration"})

	if a.cacheCooldownErr == nil {
		t.Fatal("expected an error for invalid duration, got nil")
	}
}

func TestClaudeAdapter_SetArgs_CacheCooldown_MissingValue(t *testing.T) {
	a := &claudeAdapter{}
	a.SetArgs([]string{"--cache-cooldown"})

	if a.cacheCooldownErr == nil {
		t.Fatal("expected an error for missing duration value, got nil")
	}
}

func TestClaudeAdapter_WaitForCacheCooldown_Disabled(t *testing.T) {
	withTempActivityState(t)
	a := &claudeAdapter{} // cacheCooldown left at zero value: disabled
	start := time.Now()
	if err := a.waitForCacheCooldown(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("expected no wait when cooldown is disabled, took %v", elapsed)
	}
}

func TestClaudeAdapter_WaitForCacheCooldown_NeverRecorded(t *testing.T) {
	withTempActivityState(t) // no file written: simulates "never run before"
	a := &claudeAdapter{cacheCooldown: 5 * time.Minute}
	start := time.Now()
	if err := a.waitForCacheCooldown(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("expected no wait when nothing was ever recorded, took %v", elapsed)
	}
}

func TestClaudeAdapter_WaitForCacheCooldown_WaitsOutRemainder(t *testing.T) {
	path := withTempActivityState(t)
	writeActivityTimestamp(t, path, time.Now())

	a := &claudeAdapter{cacheCooldown: 100 * time.Millisecond}
	start := time.Now()
	if err := a.waitForCacheCooldown(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Errorf("expected to wait close to 100ms, only waited %v", elapsed)
	}
}

func TestClaudeAdapter_WaitForCacheCooldown_AlreadyElapsed(t *testing.T) {
	path := withTempActivityState(t)
	writeActivityTimestamp(t, path, time.Now().Add(-time.Hour))

	a := &claudeAdapter{cacheCooldown: 5 * time.Minute}
	start := time.Now()
	if err := a.waitForCacheCooldown(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("expected no wait when cooldown already elapsed, took %v", elapsed)
	}
}

func TestClaudeAdapter_WaitForCacheCooldown_RespectsContextCancellation(t *testing.T) {
	path := withTempActivityState(t)
	writeActivityTimestamp(t, path, time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	a := &claudeAdapter{cacheCooldown: time.Hour}
	err := a.waitForCacheCooldown(ctx)
	if err == nil {
		t.Fatal("expected context deadline error, got nil")
	}
}

// TestClaudeAdapter_CooldownPersistsAcrossProcesses simulates two separate
// `milk eval run` invocations sharing the same state file: the second
// "process" (a fresh path lookup, no in-memory state carried over) must still
// see the first one's recorded activity and wait accordingly.
func TestClaudeAdapter_CooldownPersistsAcrossProcesses(t *testing.T) {
	withTempActivityState(t)

	recordClaudeActivity() // "process 1" finishes a turn

	a := &claudeAdapter{cacheCooldown: 150 * time.Millisecond}
	start := time.Now()
	if err := a.waitForCacheCooldown(context.Background()); err != nil { // "process 2"
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("expected the second call to wait out the persisted cooldown, only waited %v", elapsed)
	}
}
