package main

// Regression pins for session-start panel restore (folder-bound state,
// ADR-0078): a panel restored open must never show empty space — the
// renderer shows a loading row before the first data msg, and Init arms the
// same population the /panel toggle fires (first refresh + 3s poll). The
// population normally rides commands.go's toggle path, which loadPanelState
// never goes through; before this fix a restored git/project panel stayed
// blank until an unrelated focus cycle.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// initBatchLen runs m.Init() and returns the size of its top-level batch.
func initBatchLen(t *testing.T, m model) int {
	t.Helper()
	cmd := m.Init()
	if cmd == nil {
		return 0
	}
	msg := cmd()
	b, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init() msg = %T, want tea.BatchMsg", msg)
	}
	return len(b)
}

// TestRestoredDataPanelRendersLoadingNotBlank: before the first git/project
// data msg, the panel body must carry its title and a loading row — never a
// blank box (the reported session-start screenshot: empty space where git
// and files panels should be).
func TestRestoredDataPanelRendersLoadingNotBlank(t *testing.T) {
	m := newTestModelForPanels(t)

	for _, tc := range []struct {
		name   string
		region panelRegion
	}{{"git", regionGit}, {"project", regionProject}} {
		lines, inner := m.sidePanelLines(tc.region)
		if inner <= 0 {
			t.Fatalf("%s: inner width = %d", tc.name, inner)
		}
		if len(lines) == 0 {
			t.Fatalf("%s: no lines — panel would render blank", tc.name)
		}
		if strings.TrimSpace(lines[0]) == "" {
			t.Fatalf("%s: first line (title/tab strip) is empty", tc.name)
		}
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "(loading…)") {
			t.Errorf("%s: no loading row in %q", tc.name, joined)
		}
	}
}

// TestRestoreThenInitArmsPopulation: the real path — loadPanelState from a
// state file, then Init — must add exactly the git/project population cmds
// (first refresh + poll tick per open data panel) beyond the baseline init
// batch.
func TestRestoreThenInitArmsPopulation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel-state.json")
	state := `{"version":1,"panels":{"git":true,"project":true}}`
	if err := os.WriteFile(path, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}

	base := newTestModelForPanels(t) // no restore: baseline batch size
	baseLen := initBatchLen(t, base)

	restored := newTestModelForPanels(t)
	restored.loadPanelState(path)
	if !restored.panelGit || !restored.panelProject {
		t.Fatalf("restore did not open panels: git=%v project=%v", restored.panelGit, restored.panelProject)
	}
	got := initBatchLen(t, restored)
	if want := baseLen + 5; got != want { // git: tick+refresh; project: tick+refresh+changeset status
		t.Errorf("Init batch = %d cmds, want %d (population for restored git+project)", got, want)
	}

	// Closed panels add nothing: the same restore with both panels closed
	// must match the baseline exactly (no stray ticks in closed sessions).
	closed := newTestModelForPanels(t)
	closed.loadPanelState(writePanelState(t, `{"version":1,"panels":{"git":false,"project":false}}`))
	if n := initBatchLen(t, closed); n != baseLen {
		t.Errorf("Init batch with closed panels = %d, want baseline %d", n, baseLen)
	}
}

// writePanelState writes raw state JSON into a fresh temp file.
func writePanelState(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "panel-state.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRefreshProjectCmdFilelessFolder: a scanned folder with no files must
// arrive as a non-nil empty slice so the builder's "(empty)" row renders —
// the nil sentinel means "loading" and would leave the panel pending forever.
func TestRefreshProjectCmdFilelessFolder(t *testing.T) {
	dir := t.TempDir() // no files at all
	msg := refreshProjectCmd(dir)()
	pdm, ok := msg.(projectDataMsg)
	if !ok {
		t.Fatalf("msg = %T, want projectDataMsg", msg)
	}
	if pdm.files == nil {
		t.Fatalf("fileless folder sent nil files — panel would show (loading…) forever")
	}
	lines, _ := buildProjectTreeLines(pdm.files, pdm.truncated, nil, projectPanelInner, nil)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "(empty)") {
		t.Errorf("fileless folder should render (empty); got %q", joined)
	}
}
