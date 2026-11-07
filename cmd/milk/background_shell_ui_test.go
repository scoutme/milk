package main

import (
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/agent/local"
)

// startTestShell starts a background shell and waits for it to exit,
// returning its final snapshot (ADR-0064's UI tests need a real shell row).
func startTestShell(t *testing.T, command string) local.ShellInfo {
	t.Helper()
	info, err := local.StartBackgroundShell(command, "")
	if err != nil {
		t.Fatalf("StartBackgroundShell(%q): %v", command, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cur, _ := local.BackgroundShell(info.ID)
		if cur.Finished {
			local.WaitShellSettled(info.ID) // exit bookkeeping done: the notice is queued (or suppressed) — no late event can leak past the test
			return cur
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("shell %s did not finish in time", info.ID)
	return local.ShellInfo{}
}

func TestBackgroundPanelLayout_MergesJobsAndShells(t *testing.T) {
	jobs := []local.Job{
		{ID: "job_1", Label: "investigate X", Status: local.JobRunning, StartedAt: time.Now()},
	}
	shells := []local.ShellInfo{
		{ID: "s1", Command: "npm run dev", Running: true, StartedAt: time.Now().Add(-time.Minute)},
	}
	lines := buildBackgroundPanelLines(jobs, shells, backgroundPanelInner)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"agents (1)", "investigate X", "shells (1)", "npm run dev"} {
		if !strings.Contains(joined, want) {
			t.Errorf("panel missing %q:\n%s", want, joined)
		}
	}
	for _, line := range lines {
		if w := runeWidthNoANSI(line); w > backgroundPanelInner {
			t.Errorf("line exceeds panel width %d (got %d): %q", backgroundPanelInner, w, line)
		}
	}

	// The shared layout maps each rendered row to its click target.
	rows := backgroundPanelLayout(jobs, shells, backgroundPanelInner)
	jobRow, shellRow := -1, -1
	for i, r := range rows {
		switch {
		case r.kind == bgRowJob && r.jobIdx == 0:
			jobRow = i
		case r.kind == bgRowShell && r.shellIdx == 0:
			shellRow = i
		}
	}
	if jobRow < 0 || shellRow < 0 || jobRow == shellRow {
		t.Fatalf("row mapping wrong: jobRow=%d shellRow=%d", jobRow, shellRow)
	}
}

func TestBackgroundPanelLayout_ShellsOnly(t *testing.T) {
	shells := []local.ShellInfo{
		{ID: "s1", Command: "npm run dev", Running: true, StartedAt: time.Now()},
	}
	joined := strings.Join(buildBackgroundPanelLines(nil, shells, backgroundPanelInner), "\n")
	if strings.Contains(joined, "(none)") {
		t.Errorf("shells-only panel must not say (none):\n%s", joined)
	}
	if !strings.Contains(joined, "shells (1)") {
		t.Errorf("missing shells section:\n%s", joined)
	}
}

func TestHandleBackgroundPanelClick_ShellRowAttaches(t *testing.T) {
	m := attachTestModel()
	started := startTestShell(t, "printf distinctive-shell-output")

	shells := local.BackgroundShells()
	rows := backgroundPanelLayout(nil, shells, backgroundPanelInner)
	idx := -1
	for i, r := range rows {
		if r.kind == bgRowShell && shells[r.shellIdx].ID == started.ID {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("no shell row for %s in layout", started.ID)
	}
	// Pure action: the acting click attaches (the cadence — single =
	// deferred focus, double = attach without moving focus — is
	// panelClickAction's job, covered by TestPanelClickCadence_*).
	cmd := m.handleBackgroundPanelClick(idx)
	if m.attached == nil {
		t.Fatal("expected attach from the acting click")
	}
	if m.attached.kind != attachShell || m.attached.jobID != started.ID {
		t.Fatalf("attached = %+v, want kind=attachShell id=%s", m.attached, started.ID)
	}
	if got := m.attached.buf.Snapshot(); !strings.Contains(got, "distinctive-shell-output") {
		t.Fatalf("attached buf = %q, want the shell's output", got)
	}
	if cmd == nil {
		t.Fatal("expected startAttach to return the refresh-tick command")
	}
}

func TestRenderBgList_IncludesShells(t *testing.T) {
	shells := []local.ShellInfo{
		{ID: "s2", Command: "npm run dev", Running: true, StartedAt: time.Now()},
	}
	out := renderBgList(nil, shells)
	for _, want := range []string{"background shells:", "s2", "npm run dev", "running"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderBgList missing %q:\n%s", want, out)
		}
	}
}

func TestRenderBgShowShell_ShowsStateAndOutput(t *testing.T) {
	started := startTestShell(t, "printf distinctive-shell-output")
	info, ok := local.BackgroundShell(started.ID)
	if !ok {
		t.Fatalf("shell %s gone", started.ID)
	}
	out := renderBgShowShell(info)
	for _, want := range []string{"background shell " + started.ID, "exited 0", "distinctive-shell-output"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderBgShowShell missing %q:\n%s", want, out)
		}
	}
}
