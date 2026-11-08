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
// as evidence on THIS test instead of poisoning the whole package — a slow
// or wedged child must never be able to hold the suite hostage (go test's
// 10m package timeout is the enemy; every bound below is far under it).
//
// The bound is two-stage because single bounds have burned twice: unbounded
// = a 9-minute package-wide hang (run 42430317832); 3s = fired on pure
// runner starvation (run 42496586217, 3.02s) and turned a healthy child
// into a red; 10s = the same starvation exceeded even that (run
// 42506559007, 10.13s). SIGKILL cannot be caught, so a reap delay is
// scheduling starvation until proven otherwise: stage A (10s) logs loudly
// and keeps waiting — verbose runs keep the evidence; stage B (60s more)
// fails the test, which is the genuinely-wedged case worth a red. Total
// ceiling 70s per child.
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
		t.Logf("pty child still unreaped 10s after SIGKILL — scheduling starvation is the standing hypothesis (3s fired run 42496586217, 10s fired run 42506559007); waiting up to 60s more before failing")
		select {
		case <-done:
			t.Logf("pty child reaped late — starvation confirmed, not a wedge")
		case <-time.After(60 * time.Second):
			t.Errorf("pty child survived SIGKILL for 70s — genuinely wedged (previously an unbounded Wait hung the macOS package, run 42430317832)")
		}
	}
}
