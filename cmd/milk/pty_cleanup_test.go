package main

import (
	"testing"
	"time"
)

// killPaneBounded tears down a test-launched PTY pane without letting the
// suite hang on it.
//
// History: the plain Kill()+Wait() pattern this replaces blocked for nine
// minutes inside a t.Cleanup on the macOS CI runner (run 42430317832,
// TestHandleBusyKey_BangModeRePrepended): the captured goroutine stack pins
// wait4 — the SIGKILL was sent yet the child never changed state, which the
// log alone cannot explain (candidate: darwin runner starvation, the same
// family as every prior macOS-only red). go test's 10m package timeout then
// killed every test in that run and reported only the hang.
//
// The contract here is deliberately different: an un-reaped child surfaces
// as evidence on THIS test (loud t.Errorf, 10s bound — SIGKILL cannot be caught, so reap delay is pure runner starvation: the 3s bound fired on exactly that (run 42496586217, 3.02s) and turned starvation into a red; the disease was unbounded (9 minutes, package-wide), the cure stays bounded but must outlast a starved scheduling gap) instead of poisoning
// the whole package — a slow or wedged child must never be able to hold the
// suite hostage.
func killPaneBounded(t *testing.T, state *ptyPaneState) {
	t.Helper()
	if state == nil || state.cmd == nil || state.cmd.Process == nil {
		return
	}
	_ = state.cmd.Process.Kill()
	done := make(chan struct{})
	go func() {
		_, _ = state.cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Errorf("pty child survived SIGKILL for 10s — unbounded Process.Wait here previously hung the macOS CI package (run 42430317832); a 3s bound fired on starved runners (run 42496586217)")
	}
}
