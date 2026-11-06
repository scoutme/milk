package local

// ADR-0100 tests (as amended): shell completion delivery is deterministic —
// spawn registers the interest, every finished shell is noticed whatever its
// status, and only stops (deliberate kill_bash / the lifetime cap) stay
// silent. Real shells, real exits.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// waitShellFinished polls the registry until the shell exits (bounded).
func waitShellFinished(t *testing.T, id string) ShellInfo {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		info, ok := BackgroundShell(id)
		if ok && info.Finished {
			WaitShellSettled(id) // exit bookkeeping done: the notice is queued (or suppressed) before the test proceeds
			return info
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("shell %s did not finish in time", id)
	return ShellInfo{}
}

func TestShellWantsNotice(t *testing.T) {
	tests := []struct {
		name string
		in   ShellInfo
		want bool
	}{
		{"unfinished never", ShellInfo{}, false},
		{"clean exit delivers", ShellInfo{Finished: true}, true},
		{"failure delivers", ShellInfo{Finished: true, ExitCode: 3}, true},
		{"exec error delivers", ShellInfo{Finished: true, Err: errors.New("boom")}, true},
		{"killed stays silent", ShellInfo{Finished: true, Killed: true}, false},
		{"killed failure stays silent", ShellInfo{Finished: true, Killed: true, ExitCode: 3}, false},
	}
	for _, tt := range tests {
		if got := ShellWantsNotice(tt.in); got != tt.want {
			t.Errorf("%s: ShellWantsNotice = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// DrainShellEvents is global state — every test starts from a clean queue so
// leftovers from earlier tests (other packages' failures) can't leak in.
func drainShellEventsClean(t *testing.T) {
	t.Helper()
	if n := len(DrainShellEvents()); n > 0 {
		t.Logf("discarded %d pre-existing shell event(s)", n)
	}
}

func TestCleanExit_QueuesNoticeWithTail(t *testing.T) {
	drainShellEventsClean(t)
	info, err := StartBackgroundShell("echo hello-from-exit; exit 0", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	fin := waitShellFinished(t, info.ID)
	if !ShellWantsNotice(fin) {
		t.Fatal("clean exit must want a notice (spawn registers the interest)")
	}
	if got := PendingShellEvents(); got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}
	ev := DrainShellEvents()
	if len(ev) != 1 {
		t.Fatalf("drained %d events, want 1", len(ev))
	}
	e := ev[0]
	if e.Info.ID != info.ID || e.Info.ExitCode != 0 {
		t.Errorf("event info = %+v, want id=%s exit=0", e.Info, info.ID)
	}
	if !strings.Contains(e.Tail, "hello-from-exit") {
		t.Errorf("tail = %q, want the shell's output", e.Tail)
	}
	if got := PendingShellEvents(); got != 0 {
		t.Errorf("drain must clear the queue, pending = %d", got)
	}
}

func TestFailure_QueuesNoticeWithTail(t *testing.T) {
	drainShellEventsClean(t)
	info, err := StartBackgroundShell("echo boom-to-stderr >&2; exit 3", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitShellFinished(t, info.ID)
	if got := PendingShellEvents(); got != 1 {
		t.Fatalf("failed shell: pending = %d, want 1", got)
	}
	ev := DrainShellEvents()
	if len(ev) != 1 || ev[0].Info.ExitCode != 3 {
		t.Fatalf("events = %+v, want one exit-3 event", ev)
	}
	if !strings.Contains(ev[0].Tail, "boom-to-stderr") {
		t.Errorf("tail = %q, want stderr captured", ev[0].Tail)
	}
}

func TestKilledShell_QueuesNothing(t *testing.T) {
	drainShellEventsClean(t)
	info, err := StartBackgroundShell("sleep 30", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	killShell(info.ID, "ui", "") // requester "" — unattributed
	waitShellFinished(t, info.ID)
	if got := PendingShellEvents(); got != 0 {
		t.Fatalf("killed shell: pending = %d, want 0 (a deliberate stop is not news)", got)
	}
}

func TestShellOutputTail_TrimsAndBounds(t *testing.T) {
	if got := shellOutputTail(""); got != "" {
		t.Errorf("empty = %q, want \"\"", got)
	}
	if got := shellOutputTail("a\nb\n\n"); got != "a\nb" {
		t.Errorf("trailing newlines: got %q", got)
	}
	many := strings.Repeat("line\n", shellEventTailMax+5)
	tail := shellOutputTail(many)
	if n := strings.Count(tail, "\n") + 1; n != shellEventTailMax {
		t.Errorf("kept %d lines, want %d", n, shellEventTailMax)
	}
}
