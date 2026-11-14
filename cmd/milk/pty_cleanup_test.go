package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// killPaneBounded tears down a test-launched PTY pane without letting the
// suite hang on it — and without ever failing the test over the reap.
//
// History (every incident, so nobody re-litigates it):
//   - run 42430317832: unbounded Kill()+Wait() sat in wait4 for NINE
//     minutes inside a t.Cleanup on the macOS runner; the captured
//     goroutine stack pins syscall wait4 with the SIGKILL already sent —
//     the kernel never handed the child back. go test's 10m package
//     timeout then killed every test in that run.
//   - run 42496586217: a 3s bound fired (3.02s) — pure runner starvation,
//     child reaped shortly after.
//   - run 42506559007: the 10s bound fired the same way (10.13s).
//   - run 43651860581 (PR #432): even the two-stage 10s+60s went the full
//     70s ("genuinely wedged") — and the rerun passed with no code change.
//
// The verdict that mattered was in run 42430317832's stack: wait4 blocked
// in [syscall] for nine minutes for a child that had ALREADY been sent
// SIGKILL. A blocking wait4 only returns when the child becomes waitable;
// SIGKILL cannot be caught, blocked, or deferred. A child that stays
// un-waitable under SIGKILL is the darwin runner's kernel/VM failing to
// deliver, not anything milk does — no wait bound we write can fix that,
// and a red at the end of it has carried zero actionable information on
// all four occurrences (every one green on rerun).
//
// So the contract is deliberately different from the old "fail at 70s":
// a slow or wedged reap is EVIDENCE, logged — never a failure of the test
// whose assertions already passed. Teardown must not be able to indict a
// healthy test, and the suite must never be held hostage (go test's 10m
// package timeout is the enemy; every bound below is far under it).
//
// What teardown does, mirroring forceKillPTYPane: close the master (the
// reader goroutine's Read fails and its cmd.Wait() — the waiter production
// relies on — takes over) and kill the whole process group, then take one
// bounded observational wait. Stage A (10s) dumps evidence and keeps
// waiting; stage B (60s more) dumps evidence again and RETURNS ANYWAY —
// an unreaped child is abandoned to init (reaped when the test binary
// exits), never turned into a red.
func killPaneBounded(t *testing.T, state *ptyPaneState) {
	t.Helper()
	if state == nil || state.cmd == nil || state.cmd.Process == nil {
		return
	}
	// forceKillPTYPane's two steps: group + direct child, master closed.
	killPTYProcessGroup(state.cmd)
	if state.ptm != nil {
		_ = state.ptm.Close() //nolint:errcheck // second close after forceKill is fine
	}
	pid := state.cmd.Process.Pid

	done := make(chan struct{})
	go func() {
		_, _ = state.cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(10 * time.Second):
		ev := reapEvidence(pid)
		t.Logf("pty child unreaped 10s after SIGKILL — runner pathology is the standing finding (3s: run 42496586217, 10s: run 42506559007, 70s: run 43651860581; wait4 stuck 9min for a SIGKILLed child: run 42430317832); evidence:\n%s", ev)
		fmt.Fprintf(os.Stderr, "[milk-test] pty reap slow after SIGKILL (pid %d):\n%s", pid, ev)
		select {
		case <-done:
			t.Logf("pty child reaped late — starvation confirmed, not a wedge")
		case <-time.After(60 * time.Second):
			// NOT a failure: see the history above. The child is
			// abandoned (init reaps it when the test binary exits);
			// the stuck observer goroutine holds nothing the suite
			// needs. Evidence is logged so the next occurrence can be
			// labelled in seconds instead of re-debated.
			t.Logf("pty child still unreaped 70s after SIGKILL — abandoning it (log-only by design; evidence above); a red here has never carried actionable information")
			fmt.Fprintf(os.Stderr, "[milk-test] abandoning unreaped pty child (pid %d) after 70s\n", pid)
		}
	}
}

// reapEvidence is everything needed to label a slow reap in seconds: where
// milk's goroutines actually are (stuck in wait4 = kernel side; stuck
// anywhere else = a real teardown bug) and the child's state as the kernel
// reports it (gone/zombie + blocked wait4 is physically impossible, so it
// discriminates the two cases too). ps may be unavailable on some
// platforms — its absence is itself noted.
func reapEvidence(pid int) string {
	var b strings.Builder
	stacks := make([]byte, 1<<20)
	n := runtime.Stack(stacks, true)
	fmt.Fprintf(&b, "--- goroutines (%d bytes) ---\n%s\n", n, stacks[:n])
	out, err := exec.Command("ps", "-o", "pid,ppid,stat,command", "-p", fmt.Sprint(pid)).CombinedOutput()
	if err != nil {
		fmt.Fprintf(&b, "--- ps unavailable: %v ---\n", err)
	} else {
		fmt.Fprintf(&b, "--- ps ---\n%s", out)
	}
	return b.String()
}
