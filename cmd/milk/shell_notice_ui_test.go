package main

// ADR-0100 (as amended) TUI tests: background-shell completion delivery —
// a transcript block for every finished shell except deliberate stops, the
// follow-up wake (fires when idle, defers when busy), and the prompt-prefix
// renderer that the drain hands to the next turn.
//
// The wake tests stub shellEventsPendingForFollowup: the completion queue is
// process-global and ANY concurrent turn dispatch in the test binary drains
// it (ACP tests run turns in goroutines), so asserting on it here races the
// world. The registry's queue contract — real shells, tails, drain-once — is
// owned by internal/agent/local's tests. One end-to-end case below still runs
// a real shell for the visibility path (block + live-buffer tail), which
// never touches the queue.

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
)

// stubShellPending pins the wake decision's pending count for one test.
func stubShellPending(t *testing.T, n int) {
	t.Helper()
	orig := shellEventsPendingForFollowup
	shellEventsPendingForFollowup = func() int { return n }
	t.Cleanup(func() { shellEventsPendingForFollowup = orig })
}

func TestRenderShellDoneBlock_AllFinishedExceptKilled(t *testing.T) {
	clean := local.ShellInfo{ID: "s11", Command: "git status", Finished: true, ExitCode: 0}
	failed := local.ShellInfo{ID: "s10", Command: "racy watcher", Finished: true, ExitCode: 1}
	killed := local.ShellInfo{ID: "s12", Command: "dev server", Finished: true, Killed: true}

	block := renderShellDoneBlock(clean, "working tree clean")
	if !strings.Contains(block, "background shell s11") || !strings.Contains(block, "exited 0") || !strings.Contains(block, "working tree clean") {
		t.Errorf("clean exit block missing content:\n%s", block)
	}
	block = renderShellDoneBlock(failed, "boom")
	if !strings.Contains(block, "background shell s10") || !strings.Contains(block, "exited 1") || !strings.Contains(block, "boom") {
		t.Errorf("failure block missing content:\n%s", block)
	}
	if block := renderShellDoneBlock(killed, ""); block != "" {
		t.Errorf("a deliberate stop must stay toast-only, got block:\n%s", block)
	}
}

func TestBackgroundShellDoneMsg_BusyDefersWake(t *testing.T) {
	stubShellPending(t, 1)
	m := newTestModelForPanels(t)
	m.busy = true
	info := local.ShellInfo{ID: "s12", Command: "watch", Finished: true, ExitCode: 0}

	next, _ := m.Update(backgroundShellDoneMsg{info: info})
	nm := next.(model)

	if !nm.pendingShellFollowup {
		t.Error("completion while busy must remember the wake")
	}
	if strings.Contains(nm.transcript.String(), backgroundFollowupPrompt) {
		t.Error("no follow-up may start while busy")
	}
	if !strings.Contains(nm.transcript.String(), "background shell s12") {
		t.Errorf("completion must land in the transcript; transcript:\n%s", nm.transcript.String())
	}
}

func TestBackgroundShellDoneMsg_IdleFiresFollowup(t *testing.T) {
	stubShellPending(t, 1)
	m := newTestModelForPanels(t)
	info := local.ShellInfo{ID: "s13", Command: "release watcher", Finished: true, ExitCode: 0}

	next, cmd := m.Update(backgroundShellDoneMsg{info: info})
	nm := next.(model)
	if cmd == nil {
		t.Fatal("completion while idle must start the follow-up turn")
	}
	if nm.pendingShellFollowup {
		t.Error("an immediate follow-up must not leave the retry flag set")
	}
	if !strings.Contains(nm.transcript.String(), backgroundFollowupPrompt) {
		t.Errorf("follow-up echo missing from transcript:\n%s", nm.transcript.String())
	}
	if !strings.Contains(nm.transcript.String(), "[background]") {
		t.Error("follow-up must echo under the [background] label")
	}
}

// A wake whose events were already drained by an intervening turn must not
// burn an empty turn — decideFollowup's pending==0 guard, pinned at the UI.
func TestBackgroundShellDoneMsg_DrainedEventsNoWake(t *testing.T) {
	stubShellPending(t, 0)
	m := newTestModelForPanels(t)
	info := local.ShellInfo{ID: "s14", Command: "racy watcher", Finished: true, ExitCode: 1}

	next, _ := m.Update(backgroundShellDoneMsg{info: info})
	nm := next.(model)
	if nm.pendingShellFollowup {
		t.Error("a drained wake must not set the retry flag")
	}
	if strings.Contains(nm.transcript.String(), backgroundFollowupPrompt) {
		t.Error("a drained wake must not start a follow-up turn")
	}
	if !strings.Contains(nm.transcript.String(), "background shell s14") {
		t.Errorf("the block must still land in the transcript:\n%s", nm.transcript.String())
	}
}

func TestBackgroundShellDoneMsg_KilledStaysSilent(t *testing.T) {
	stubShellPending(t, 0)
	m := newTestModelForPanels(t)
	info := local.ShellInfo{ID: "s15", Command: "dev server", Finished: true, Killed: true, ExitCode: -1}

	next, _ := m.Update(backgroundShellDoneMsg{info: info})
	nm := next.(model)
	if nm.pendingShellFollowup {
		t.Error("a deliberate stop must not wake")
	}
	if strings.Contains(nm.transcript.String(), "background shell s15") {
		t.Error("a deliberate stop must not write a transcript block")
	}
}

// The visibility path end-to-end: a real failed shell's exit writes the
// transcript block with its live-buffer tail — the half of delivery that
// never touches the shared queue (so no stub, no race).
func TestBackgroundShellDoneMsg_RealFailedShellBlock(t *testing.T) {
	stubShellPending(t, 0)
	t.Cleanup(func() { local.DrainShellEvents() }) // the registry still queues this exit — don't leak it to neighbors
	m := newTestModelForPanels(t)
	info := startTestShell(t, "echo boom-real-output >&2; exit 7")

	next, _ := m.Update(backgroundShellDoneMsg{info: info})
	nm := next.(model)
	block := nm.transcript.String()
	if !strings.Contains(block, "background shell "+info.ID) || !strings.Contains(block, "exited 7") {
		t.Errorf("failed shell must write its block:\n%s", block)
	}
	if !strings.Contains(block, "boom-real-output") {
		t.Errorf("block must carry the live-buffer tail:\n%s", block)
	}
	if nm.pendingShellFollowup {
		t.Error("a drained wake must never set the flag")
	}
}

func TestMaybeAutoFollowupShells_SkipsWhenNothingPending(t *testing.T) {
	stubShellPending(t, 0)
	m := newTestModelForPanels(t)
	m.pendingShellFollowup = true

	updated, cmd := m.maybeAutoFollowupShells()
	if cmd != nil {
		t.Error("no queued shell events: follow-up must not start")
	}
	if updated.(model).pendingShellFollowup {
		t.Error("a skipped shell follow-up must clear its retry flag")
	}
}

func TestMaybeAutoFollowupShells_BusyDefersKeepsFlag(t *testing.T) {
	stubShellPending(t, 1)
	m := newTestModelForPanels(t)
	m.busy = true
	info := local.ShellInfo{ID: "s16", Command: "w", Finished: true, ExitCode: 0}

	next, _ := m.Update(backgroundShellDoneMsg{info: info}) // pending=1 + busy → retryLater
	if !next.(model).pendingShellFollowup {
		t.Fatal("busy update must set the retry flag")
	}
	n2, cmd := next.(model).maybeAutoFollowupShells()
	if cmd != nil {
		t.Error("busy: no turn may start")
	}
	if !n2.(model).pendingShellFollowup {
		t.Error("busy: the wake must be remembered, not dropped")
	}
}

func TestRenderShellNotices_PromptPrefix(t *testing.T) {
	if got := renderShellNotices(nil); got != "" {
		t.Errorf("empty drain = %q, want \"\"", got)
	}
	events := []local.ShellEvent{
		{Info: local.ShellInfo{ID: "s1", Command: "watch 42", Finished: true, ExitCode: 0}, Tail: "ready"},
		{Info: local.ShellInfo{ID: "s2", Command: "watch 9", Finished: true, ExitCode: 2}, Tail: "conflict"},
	}
	got := renderShellNotices(events)
	for _, want := range []string{
		"[shell-finished] background shell s1 (\"watch 42\") — exited 0",
		"ready",
		"[shell-finished] background shell s2 (\"watch 9\") — exited 2",
		"conflict",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("notice missing %q:\n%s", want, got)
		}
	}
}
