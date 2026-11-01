package main

// Panel content events (issue #347) — regression pins for the two layers:
//
//   - notification: per-event, granular toasts for every observed domain
//     occurrence (git status changes, changeset changes, task mutations),
//     fired whether or not the panel is open and never swallowed by the
//     auto-open gates;
//   - layout: edges only — auto-open on content from a clean previous
//     state (clean→dirty, branch switch, empty→non-empty changeset) and a
//     manual close's veto released by the domain's "no contents" state
//     (clean tree / empty changeset, see panelContentFinalized).

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/tasks"
)

// toastTexts returns the recorded toast texts (oldest first).
func toastTexts(m *model) []string {
	out := make([]string, 0, len(m.toastHistory))
	for _, ev := range m.toastHistory {
		out = append(out, ev.text)
	}
	return out
}

// toastCount returns how many recorded toasts contain substr.
func toastCount(m *model, substr string) int {
	n := 0
	for _, ev := range m.toastHistory {
		if strings.Contains(ev.text, substr) {
			n++
		}
	}
	return n
}

// lastToast returns the newest toast text ("" when none).
func lastToast(m *model) string {
	if len(m.toastHistory) == 0 {
		return ""
	}
	return m.toastHistory[len(m.toastHistory)-1].text
}

// sendChangeset applies one status probe through the real Update path.
func sendChangeset(t *testing.T, m model, branch string, dirty int) model {
	t.Helper()
	updated, _ := m.Update(changesetMsg{gitOK: true, branch: branch, dirty: dirty})
	return updated.(model)
}

// --- git panel ---

// TestGitStatusBaselineIsSilent: the first probe only establishes the
// baseline — pre-existing dirt at startup is state, not news: no toast, no
// auto-open. The *transition* into dirt afterwards is what fires.
func TestGitStatusBaselineIsSilent(t *testing.T) {
	m := newTestModelForPanels(t)
	m = sendChangeset(t, m, "main", 3)
	if got := toastTexts(&m); len(got) != 0 {
		t.Fatalf("baseline probe toasted: %v", got)
	}
	if rawPanelOpen(&m, regionGit) {
		t.Fatal("baseline dirt must not auto-open the git panel")
	}

	m = sendChangeset(t, m, "main", 5)
	if rawPanelOpen(&m, regionGit) {
		t.Fatal("dirty→dirty drift is not an edge — must not auto-open")
	}
	if toastCount(&m, "working tree has 5 change(s)") != 1 {
		t.Fatalf("dirty-count change must toast per occurrence, got %v", toastTexts(&m))
	}
}

// TestGitCleanToDirtyEdgeOpensPanel: clean at baseline, then dirt arrives
// — the layout edge auto-opens the panel and the event toasts.
func TestGitCleanToDirtyEdgeOpensPanel(t *testing.T) {
	m := newTestModelForPanels(t)
	m = sendChangeset(t, m, "main", 0)
	m = sendChangeset(t, m, "main", 2)
	if !rawPanelOpen(&m, regionGit) {
		t.Fatal("clean→dirty edge must auto-open the git panel")
	}
	if toastCount(&m, "working tree has 2 change(s)") != 1 {
		t.Errorf("missing dirty toast, got %v", toastTexts(&m))
	}
	if toastCount(&m, "git panel opened") != 1 {
		t.Errorf("missing panel-opened toast, got %v", toastTexts(&m))
	}
}

// TestGitBranchChangeIsEdge: a branch switch is a layout edge too, and it
// toasts independently of the dirty count.
func TestGitBranchChangeIsEdge(t *testing.T) {
	m := newTestModelForPanels(t)
	m = sendChangeset(t, m, "main", 0)
	m = sendChangeset(t, m, "feat/x", 0)
	if !rawPanelOpen(&m, regionGit) {
		t.Fatal("branch switch must auto-open the git panel")
	}
	if toastCount(&m, "branch switched to feat/x") != 1 {
		t.Errorf("missing branch toast, got %v", toastTexts(&m))
	}
}

// TestGitManualCloseSticksUntilClean walks the full issue #347 override
// cycle: closing while dirty sticks through further dirt (no layout
// movement — but per-event toasts keep flowing), the clean state releases
// the veto without force-opening, and the next clean→dirty transition
// auto-opens again.
func TestGitManualCloseSticksUntilClean(t *testing.T) {
	m := newTestModelForPanels(t)
	m.rememberPanelState = false // ADR-0059 expiry semantics
	m = sendChangeset(t, m, "main", 0)
	m = sendChangeset(t, m, "main", 2) // edge → opens
	if !rawPanelOpen(&m, regionGit) {
		t.Fatal("setup: clean→dirty should have opened the panel")
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyF6}) // close
	m = updated.(model)
	if rawPanelOpen(&m, regionGit) {
		t.Fatal("F6 should close the git panel")
	}
	if !m.panelManualOverride[regionGit] {
		t.Fatal("closing while dirty must record a manual override")
	}

	m = sendChangeset(t, m, "main", 5) // more dirt — close must stick
	if rawPanelOpen(&m, regionGit) {
		t.Error("manual close must stick through further dirt")
	}
	if toastCount(&m, "working tree has 5 change(s)") != 1 {
		t.Errorf("per-event toasts must keep flowing while closed, got %v", toastTexts(&m))
	}

	m = sendChangeset(t, m, "main", 0) // clean: releases the veto
	if rawPanelOpen(&m, regionGit) {
		t.Error("the finalizing clean state must not force the panel open")
	}
	if m.panelManualOverride[regionGit] {
		t.Error("clean state must release the manual override")
	}
	if toastCount(&m, "working tree clean") != 1 {
		t.Errorf("missing clean toast, got %v", toastTexts(&m))
	}

	m = sendChangeset(t, m, "main", 1) // new dirt from clean → opens again
	if !rawPanelOpen(&m, regionGit) {
		t.Error("new clean→dirty transition must auto-open the panel again")
	}
}

// TestGitStatus_NoRepoIsNoLiveContent: outside a repo the probe's final
// word counts as "no contents" — a manual close is never locked out.
func TestGitStatus_NoRepoIsNoLiveContent(t *testing.T) {
	m := newTestModelForPanels(t)
	updated, _ := m.Update(changesetMsg{noRepo: true})
	m = updated.(model)
	if !m.panelContentFinalized(regionGit) {
		t.Error("a non-repo must finalize git content (nothing in flight)")
	}
}

// TestPanelContentFinalized_Git: dirty is the git panel's live content;
// clean/unknown is not.
func TestPanelContentFinalized_Git(t *testing.T) {
	m := newTestModelForPanels(t)
	if !m.panelContentFinalized(regionGit) {
		t.Error("unknown state must count as clean (no live content)")
	}
	m = sendChangeset(t, m, "main", 3)
	if m.panelContentFinalized(regionGit) {
		t.Error("a dirty tree is live content — not finalized")
	}
	m = sendChangeset(t, m, "main", 0)
	if !m.panelContentFinalized(regionGit) {
		t.Error("a clean tree is git-world 'no contents' — finalized")
	}
}

// --- project panel ---

// sendChangesetFiles applies one probe carrying a file changeset.
func sendChangesetFiles(t *testing.T, m model, files map[string]session.ChangeKind) model {
	t.Helper()
	updated, _ := m.Update(changesetMsg{gitOK: true, branch: "main", files: files})
	return updated.(model)
}

// TestProjectBaselineSilentThenDriftToasts: pre-existing changes at
// startup are the baseline (silent); later changes toasts per occurrence
// but a non-empty→non-empty drift is not a layout edge.
func TestProjectBaselineSilentThenDriftToasts(t *testing.T) {
	m := newTestModelForPanels(t)
	m = sendChangesetFiles(t, m, map[string]session.ChangeKind{"a.go": session.ChangeModified})
	if got := toastTexts(&m); len(got) != 0 {
		t.Fatalf("baseline changeset toasted: %v", got)
	}
	if rawPanelOpen(&m, regionProject) {
		t.Fatal("baseline changes must not auto-open the project panel")
	}

	m = sendChangesetFiles(t, m, map[string]session.ChangeKind{
		"a.go": session.ChangeModified, "b.go": session.ChangeCreated,
	})
	if rawPanelOpen(&m, regionProject) {
		t.Fatal("non-empty drift is not an edge — must not auto-open")
	}
	if toastCount(&m, "2 file(s)") != 1 {
		t.Errorf("missing changeset toast, got %v", toastTexts(&m))
	}
}

// TestProjectManualCloseSticksUntilClean: the project panel reads the same
// override rule from its own domain — a close sticks while the changeset is
// non-empty and is released when it empties.
func TestProjectManualCloseSticksUntilClean(t *testing.T) {
	m := newTestModelForPanels(t)
	m.rememberPanelState = false
	m = sendChangesetFiles(t, m, nil) // baseline: clean
	m = sendChangesetFiles(t, m, map[string]session.ChangeKind{"a.go": session.ChangeModified})
	if !rawPanelOpen(&m, regionProject) {
		t.Fatal("setup: empty→non-empty should have opened the panel")
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyF5}) // close
	m = updated.(model)
	if !m.panelManualOverride[regionProject] {
		t.Fatal("closing while changes are live must record a manual override")
	}

	m = sendChangesetFiles(t, m, map[string]session.ChangeKind{
		"a.go": session.ChangeModified, "b.go": session.ChangeCreated,
	})
	if rawPanelOpen(&m, regionProject) {
		t.Error("manual close must stick while the changeset is non-empty")
	}
	if toastCount(&m, "2 file(s)") != 1 {
		t.Errorf("per-event toasts must keep flowing while closed, got %v", toastTexts(&m))
	}

	m = sendChangesetFiles(t, m, nil) // changeset emptied → release
	if m.panelManualOverride[regionProject] {
		t.Error("an empty changeset must release the manual override")
	}
	if toastCount(&m, "session changeset clean") != 1 {
		t.Errorf("missing clean toast, got %v", toastTexts(&m))
	}

	m = sendChangesetFiles(t, m, map[string]session.ChangeKind{"c.go": session.ChangeCreated})
	if !rawPanelOpen(&m, regionProject) {
		t.Error("the next empty→non-empty edge must auto-open the panel again")
	}
}

// TestPanelContentFinalized_Project: an empty changeset is project-world
// "no contents".
func TestPanelContentFinalized_Project(t *testing.T) {
	m := newTestModelForPanels(t)
	if !m.panelContentFinalized(regionProject) {
		t.Error("an empty/unknown changeset must be finalized")
	}
	m = sendChangesetFiles(t, m, map[string]session.ChangeKind{"a.go": session.ChangeDeleted})
	if m.panelContentFinalized(regionProject) {
		t.Error("a non-empty changeset is live content — not finalized")
	}
}

// --- tasks panel ---

// TestTaskEventsToasts: each store mutation is a task event (created /
// status flip / removed) and the all-done milestone toasts too — the first
// observation stays silent (restored tasks are state, not news).
func TestTaskEventsToasts(t *testing.T) {
	m := newTestModelForPanels(t)
	ts, err := tasks.New(t.TempDir(), "sess")
	if err != nil {
		t.Fatal(err)
	}
	m.taskStore = ts

	// Baseline: existing tasks at first observation are not events.
	m.taskEventPrev = taskEventSnapshot(ts)
	first, err := ts.Create("first task", nil)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(taskStoreChangedMsg{})
	m = updated.(model)
	if toastCount(&m, `task created: "first task"`) != 1 {
		t.Fatalf("missing created toast, got %v", toastTexts(&m))
	}

	// Status flips + the all-done milestone.
	second, err := ts.Create("second task", nil)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ = m.Update(taskStoreChangedMsg{}) // created
	m = updated.(model)
	for _, id := range []string{first.ID, second.ID} {
		if err := ts.Complete(id); err != nil {
			t.Fatal(err)
		}
	}
	updated, _ = m.Update(taskStoreChangedMsg{})
	m = updated.(model)
	if toastCount(&m, `task "second task": done`) != 1 {
		t.Errorf("missing status-flip toast, got %v", toastTexts(&m))
	}
	if toastCount(&m, "all tasks done") != 1 {
		t.Errorf("missing all-done milestone toast, got %v", toastTexts(&m))
	}

	// Baseline swallow: a store snapshot taken with tasks present is not
	// a burst of created events (the auto-open toast is layout, not an
	// event — only the task vocabulary must stay silent).
	m2 := newTestModelForPanels(t)
	m2.taskStore = ts
	m2.taskEventPrev = nil
	updated2, _ := m2.Update(taskStoreChangedMsg{})
	m3 := updated2.(model)
	if n := toastCount(&m3, "task ") + toastCount(&m3, "all tasks done"); n != 0 {
		t.Errorf("first observation must be the silent baseline, got %v", toastTexts(&m3))
	}
}

// --- poll chain keep-alive (the event source while panels are closed) ---

// TestPollChainSurvivesClosedPanels: the 3s ticks must keep themselves
// alive with both panels closed — otherwise the status probe (the panel
// event source) dies with the first closed tick.
func TestPollChainSurvivesClosedPanels(t *testing.T) {
	m := newTestModelForPanels(t)
	updated, cmd := m.Update(projectRefreshMsg{})
	m = updated.(model)
	if cmd == nil {
		t.Fatal("projectRefreshMsg on a closed panel must keep the tick chain alive")
	}
	updated2, cmd2 := m.Update(gitRefreshMsg{})
	_ = updated2
	if cmd2 == nil {
		t.Fatal("gitRefreshMsg on a closed panel must keep the tick chain alive")
	}
}

// --- session file-trace wakeup ---

// TestFileTraceWakeupCoalesces: a burst of traced writes schedules exactly
// one coalesced poke, and the poke syncs the changeset.
func TestFileTraceWakeupCoalesces(t *testing.T) {
	m := newTestModelForPanels(t)
	id := m.st.sess.ID
	updated, cmd := m.Update(fileTraceChangedMsg{sessID: id})
	m = updated.(model)
	if cmd == nil {
		t.Fatal("first trace wakeup must arm the coalescing poke")
	}
	updated2, cmd2 := m.Update(fileTraceChangedMsg{sessID: id})
	m = updated2.(model)
	if cmd2 != nil {
		t.Fatal("a burst must coalesce into one poke")
	}
	updated3, cmd3 := m.Update(fileTracePokeMsg{})
	m = updated3.(model)
	if m.fileTracePokePending {
		t.Error("the poke must clear the coalescing marker")
	}
	if cmd3 == nil {
		t.Error("the poke must schedule the changeset sync/probe")
	}

	// Foreign session ids are ignored entirely.
	updated4, cmd4 := m.Update(fileTraceChangedMsg{sessID: "other"})
	_ = updated4
	if cmd4 != nil {
		t.Error("a trace wakeup for another session must be ignored")
	}
}

// TestParseStatusFilesSkipsBranchHeader: the probe runs with -b, so its
// output carries a "##" header line that must never become a file entry.
func TestParseStatusFilesSkipsBranchHeader(t *testing.T) {
	out := "## main...origin/main [ahead 1]\n M go.mod\n?? new.txt\n"
	files := parseStatusFiles(out, "/repo", "/repo")
	if len(files) != 2 {
		t.Fatalf("parsed %d files (%v), want 2 — header leaked into the changeset", len(files), files)
	}
	if _, ok := files["new.txt"]; !ok {
		t.Errorf("missing untracked entry: %v", files)
	}
	branch, dirty := parseStatusBranch(out)
	if branch != "main" || dirty != 2 {
		t.Errorf("parseStatusBranch = %q, %d; want main, 2", branch, dirty)
	}
}

// TestChangesetEventSummary guards the toast tally text.
func TestChangesetEventSummary(t *testing.T) {
	got := changesetEventSummary(map[string]session.ChangeKind{
		"a": session.ChangeModified,
		"b": session.ChangeModified,
		"c": session.ChangeCreated,
	})
	if want := "3 file(s) (1A 2M)"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}
