package main

import (
	"context"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/updater"
)

// TestUpdateCheck_PatchesBookkeepingNotSnapshot pins #404's lost-update fix
// at the call site: after a (possibly long) release check, the only disk
// write is a surgical bookkeeping patch of update_last_check — never a save
// of the in-memory config snapshot, which could stomp an edit made while the
// network call was in flight.
func TestUpdateCheck_PatchesBookkeepingNotSnapshot(t *testing.T) {
	oldCheck, oldPatch := checkLatest, bookkeepingPatch
	t.Cleanup(func() { checkLatest, bookkeepingPatch = oldCheck, oldPatch })

	checkLatest = func(context.Context, string, bool) (*updater.Release, error) {
		return nil, nil // up to date: the boring path must still stamp
	}
	var gotDir string
	var gotKV map[string]string
	patchCalls := 0
	bookkeepingPatch = func(dir string, kv map[string]string) error {
		patchCalls++
		gotDir, gotKV = dir, kv
		return nil
	}

	cfg := config.Config{
		Agent:  "real",
		Agents: []config.AgentConfig{{Name: "real", URL: "http://x", Model: "m", Provider: "local"}},
	}
	rel, err := updateCheck(context.Background(), &cfg, "/sessions/bind-dir")
	if err != nil {
		t.Fatalf("updateCheck: %v", err)
	}
	if rel != nil {
		t.Errorf("rel = %+v, want nil for up-to-date", rel)
	}
	if cfg.UpdateLastCheck == "" {
		t.Error("in-memory UpdateLastCheck not stamped")
	}
	if patchCalls != 1 {
		t.Fatalf("bookkeepingPatch calls = %d, want exactly 1", patchCalls)
	}
	if gotDir != "/sessions/bind-dir" {
		t.Errorf("patch dir = %q, want the caller's dir", gotDir)
	}
	if gotKV["update_last_check"] != cfg.UpdateLastCheck {
		t.Errorf("patch stamp = %q, want %q", gotKV["update_last_check"], cfg.UpdateLastCheck)
	}
	if len(gotKV) != 1 {
		t.Errorf("patch carries %d keys, want only update_last_check: %v", len(gotKV), gotKV)
	}
}

// TestSkipUpdateRelease_PatchesTheSkipStamp: same surgical rule for the
// /update skip stamp.
func TestSkipUpdateRelease_PatchesTheSkipStamp(t *testing.T) {
	oldPatch := bookkeepingPatch
	t.Cleanup(func() { bookkeepingPatch = oldPatch })

	var gotKV map[string]string
	bookkeepingPatch = func(_ string, kv map[string]string) error {
		gotKV = kv
		return nil
	}

	cfg := config.Config{Agent: "real"}
	skipUpdateRelease(&cfg, "v9.9.9", "")
	if cfg.UpdateSkippedVersion != "v9.9.9" {
		t.Errorf("in-memory skip = %q, want v9.9.9", cfg.UpdateSkippedVersion)
	}
	if gotKV["update_skipped_version"] != "v9.9.9" {
		t.Errorf("patched keys = %v, want update_skipped_version=v9.9.9", gotKV)
	}
	if len(gotKV) != 1 {
		t.Errorf("patch carries %d keys, want only the skip stamp: %v", len(gotKV), gotKV)
	}
}
