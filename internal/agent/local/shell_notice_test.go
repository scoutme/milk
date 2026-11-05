package local

// ADR-0100 tests: shell completion notices — the await opt-in (wake on exit,
// any status), the failure net (nonzero exits noticed without await), and the
// clean/killed exits that must stay pull-only. Real shells, real exits.

import (
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
			return info
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("shell %s did not finish in time", id)
	return ShellInfo{}
}

func TestShellWantsNotice(t *testing.T) {
	unfinished := func(a bool) ShellInfo { return ShellInfo{Await: a} }
	ok0 := func(a, killed bool) ShellInfo {
		return ShellInfo{Finished: true, Await: a, Killed: killed}
	}
	fail := func(a bool) ShellInfo {
		return ShellInfo{Finished: true, Await: a, ExitCode: 3}
	}
	tests := []struct {
		name string
		in   ShellInfo
		want bool
	}{
		{"unfinished never", unfinished(true), false},
		{"awaited clean exit", ok0(true, false), true}, // the await promise: any status
		{"unawaited clean exit", ok0(false, false), false},
		{"unawaited failure", fail(false), true}, // the failure net
		{"awaited failure", fail(true), true},
		{"killed without await", ok0(false, true), false}, // a deliberate stop is not news
		{"killed with await", ok0(true, true), true},      // await still delivers
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

func TestAwaitedShell_QueuesNoticeWithTail(t *testing.T) {
	drainShellEventsClean(t)
	info, err := StartBackgroundShellAwait("echo hello-from-await; exit 0", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !info.Await {
		t.Fatal("StartBackgroundShellAwait must produce an Await shell")
	}
	fin := waitShellFinished(t, info.ID)
	if !ShellWantsNotice(fin) {
		t.Fatal("awaited clean exit must want a notice")
	}
	if got := PendingShellEvents(); got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}
	ev := DrainShellEvents()
	if len(ev) != 1 {
		t.Fatalf("drained %d events, want 1", len(ev))
	}
	e := ev[0]
	if e.Info.ID != info.ID || !e.Info.Await || e.Info.ExitCode != 0 {
		t.Errorf("event info = %+v, want id=%s await=true exit=0", e.Info, info.ID)
	}
	if !strings.Contains(e.Tail, "hello-from-await") {
		t.Errorf("tail = %q, want the shell's output", e.Tail)
	}
	if got := PendingShellEvents(); got != 0 {
		t.Errorf("drain must clear the queue, pending = %d", got)
	}
}

func TestUnawaitedFailure_QueuesNoticeWithoutWake(t *testing.T) {
	drainShellEventsClean(t)
	info, err := StartBackgroundShell("echo boom-to-stderr >&2; exit 3", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	fin := waitShellFinished(t, info.ID)
	if fin.Await {
		t.Fatal("shell must not be awaited")
	}
	if got := PendingShellEvents(); got != 1 {
		t.Fatalf("failed unawaited shell: pending = %d, want 1 (failure net)", got)
	}
	ev := DrainShellEvents()
	if len(ev) != 1 || ev[0].Info.ExitCode != 3 {
		t.Fatalf("events = %+v, want one exit-3 event", ev)
	}
	if !strings.Contains(ev[0].Tail, "boom-to-stderr") {
		t.Errorf("tail = %q, want stderr captured", ev[0].Tail)
	}
}

func TestUnawaitedCleanExit_QueuesNothing(t *testing.T) {
	drainShellEventsClean(t)
	info, err := StartBackgroundShell("exit 0", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitShellFinished(t, info.ID)
	if got := PendingShellEvents(); got != 0 {
		t.Fatalf("clean unawaited exit: pending = %d, want 0 (pull-only)", got)
	}
}

func TestKilledShell_QueuesNothingWithoutAwait(t *testing.T) {
	drainShellEventsClean(t)
	info, err := StartBackgroundShell("sleep 30", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	killShell(info.ID, "ui", "") // requester "" — unattributed
	waitShellFinished(t, info.ID)
	if got := PendingShellEvents(); got != 0 {
		t.Fatalf("killed unawaited shell: pending = %d, want 0 (deliberate stop)", got)
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
