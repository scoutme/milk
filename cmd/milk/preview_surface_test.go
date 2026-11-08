package main

// Preview-surface interactions:
//
//  1. While a preview (attach view) owns the main area, panel navigation
//     stays fully available — the mouse path already dispatches panel
//     clicks ahead of the attach view; the keys must match (reported:
//     "input other than ESC is ignored — panels navigation should be
//     available").
//  2. The project panel's top changes-summary line owns the project-wide
//     diff; a file row's preview stays scoped to its own path (reported:
//     "previewing a modified file opens the full project diff instead of
//     the single file full diff. The project diff should be the preview
//     of the top 'changes' line, at most").

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/livebuf"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/tasks"
)

// previewAttachModel returns a model with a preview open over a long body
// (so viewport scrolling is observable) and the tasks panel focused.
func previewAttachModel(t *testing.T) (model, *tasks.Store) {
	t.Helper()
	m, ts := cursorTestModel(t, 4)
	body := strings.Repeat("preview line\n", 300)
	buf := livebuf.New(len(body) + 64)
	buf.Append([]byte(body))
	m.startAttach(attachPreview, "", "file preview", buf)
	if m.attached == nil {
		t.Fatal("fixture: preview did not open")
	}
	return m, ts
}

// --- 1: panel navigation while a preview is open ---

func TestAttachKey_PanelNavigationWhilePreviewOpen(t *testing.T) {
	// Rotation: shift+pgdn leaves the focused panel (cycle [main, visible…]
	// wraps to main when tasks is the only open panel).
	m, _ := previewAttachModel(t)
	if m.focusedPanel() != regionTasks {
		t.Fatalf("fixture: focus = %v, want tasks", m.focusedPanel())
	}
	next, _ := m.handleAttachKey(tea.KeyPressMsg{Code: tea.KeyPgDown, Mod: tea.ModShift})
	nm := next.(model)
	if nm.focusedPanel() == regionTasks {
		t.Error("shift+pgdn while preview open must rotate focus (it was swallowed)")
	}
	// Tab is the alias with no completion pending.
	nextT, _ := nm.handleAttachKey(tea.KeyPressMsg{Code: tea.KeyTab})
	nt := nextT.(model)
	if nt.focusedPanel() == nm.focusedPanel() {
		t.Error("tab while preview open must rotate focus (completion none pending)")
	}

	// Row cursor: up/down move it without detaching.
	m2, ts := previewAttachModel(t)
	item, err := ts.Create("nav me", nil)
	if err != nil {
		t.Fatal(err)
	}
	m2.panelCursorLine = taskRowLineIndex(t, ts, "", item.ID)
	m2.panelCursorLine++ // step off the item row so up has somewhere to go
	start := m2.panelCursorLine
	next2, _ := m2.handleAttachKey(tea.KeyPressMsg{Code: tea.KeyUp})
	nm2 := next2.(model)
	if nm2.panelCursorLine != start-1 {
		t.Errorf("up while preview open: cursor %d → %d, want %d", start, nm2.panelCursorLine, start-1)
	}
	if nm2.attached == nil {
		t.Error("arrow key must not detach the preview")
	}

	// Enter on the focused panel's item row runs its action (a preview open
	// does not block the row action — for rows with items; here expand).
	m3, ts3 := previewAttachModel(t)
	task3, err := ts3.Create("enter me", nil)
	if err != nil {
		t.Fatal(err)
	}
	m3.panelCursorLine = taskRowLineIndex(t, ts3, "", task3.ID)
	next3, _ := m3.handleAttachKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	nm3 := next3.(model)
	if nm3.taskExpanded != task3.ID {
		t.Errorf("enter while preview open: expanded %q, want %q", nm3.taskExpanded, task3.ID)
	}
	if nm3.attached == nil {
		t.Error("enter on a row action must not detach the preview")
	}

	// Paging routing: with a panel focused, pgdown pages the PANEL and
	// leaves the preview's viewport alone; with main focus it pages the
	// preview (the visible main content).
	m4, ts4 := previewAttachModel(t)
	for i := range 60 {
		if _, err := ts4.Create("page "+string(rune('a'+i%26))+" "+string(rune('a'+i/26)), nil); err != nil {
			t.Fatal(err)
		}
	}
	base := m4.attached.vp.YOffset() // sticky-bottom may open the preview elsewhere — route, don't assume 0
	next4, _ := m4.handleAttachKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	nm4 := next4.(model)
	if nm4.attached.vp.YOffset() != base {
		t.Errorf("pgdown with a focused panel must page the panel, preview moved %d → %d", base, nm4.attached.vp.YOffset())
	}
	if nm4.panelScrollOffset(regionTasks) == 0 {
		t.Error("pgdown with a focused panel must scroll that panel")
	}
	// Value-copy models share the *attachState pointer, so the offset must be
	// captured BEFORE the keypress — a post-hoc comparison against nm4 would
	// read the same viewport the key just moved.
	nm4.panelFocus = regionNone
	mainBase := nm4.attached.vp.YOffset()
	next5, _ := nm4.handleAttachKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	nm5 := next5.(model)
	if nm5.attached.vp.YOffset() == mainBase {
		t.Errorf("pgdown with main focus must page the preview viewport (offset stuck at %d)", mainBase)
	}

	// A main-focused Enter stays a no-op (no submittable input in this view).
	m6, _ := previewAttachModel(t)
	m6.panelFocus = regionNone
	before := m6.attached.label
	next6, _ := m6.handleAttachKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	nm6 := next6.(model)
	if nm6.attached == nil || nm6.attached.label != before {
		t.Error("main-focused enter must not detach or switch the preview")
	}
}

// --- 2: the changes-summary row owns the project-wide diff ---

// twoFileRepo builds a repo with two committed files, both modified after
// the commit — scope leakage between them is then visible in any output.
func twoFileRepo(t *testing.T) string {
	t.Helper()
	gitAvailable(t)
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	if err := os.WriteFile(filepath.Join(dir, "one.txt"), []byte("one base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "two.txt"), []byte("two base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "base")
	if err := os.WriteFile(filepath.Join(dir, "one.txt"), []byte("one CHANGED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "two.txt"), []byte("two CHANGED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestProjectFileRow_TwoModifiedFiles_ScopedToItsOwnPath(t *testing.T) {
	dir := twoFileRepo(t)
	m := dragTestModel()
	m.st.cwd = dir
	m.projectFiles = []string{"one.txt", "two.txt"}
	m.changesetFiles = map[string]session.ChangeKind{
		"one.txt": session.ChangeModified,
		"two.txt": session.ChangeModified,
	}
	m.rebuildProjectTree()

	idx := -1
	for i, id := range m.projectIDs {
		if id == "one.txt" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("one.txt row not found in ids %v", m.projectIDs)
	}
	if cmd := m.handleProjectPanelClick(idx); cmd == nil || m.attached == nil {
		t.Fatalf("modified file click should attach a preview, got attached=%v", m.attached)
	}
	if m.attached.label != "diff one.txt" {
		t.Errorf("label = %q, want the file's own diff label", m.attached.label)
	}
	snap := m.attached.buf.Snapshot()
	if !strings.Contains(snap, "one.txt") {
		t.Errorf("preview does not mention one.txt:\n%s", snap)
	}
	if strings.Contains(snap, "two.txt") {
		t.Errorf("leak: file row preview contains the OTHER file's diff (this is the reported full-project-diff bug):\n%s", snap)
	}
}

func TestProjectChangesRow_PreviewsWorkingTree(t *testing.T) {
	dir := twoFileRepo(t)
	m := dragTestModel()
	m.st.cwd = dir
	m.projectFiles = []string{"one.txt", "two.txt"}
	m.changesetFiles = map[string]session.ChangeKind{
		"one.txt": session.ChangeModified,
		"two.txt": session.ChangeModified,
	}
	m.rebuildProjectTree()

	idx := -1
	for i, id := range m.projectIDs {
		if id == changesRowID {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("changes-summary row not found in ids %v", m.projectIDs)
	}
	// Enter-path gate (panelActivateRow) must accept the row.
	if cmd, ok := m.panelActivateRow(regionProject, idx); !ok || cmd == nil || m.attached == nil {
		t.Fatalf("changes row must be activatable (ok=%v attached=%v)", ok, m.attached)
	}
	if m.attached.label != "working tree" {
		t.Errorf("label = %q, want %q", m.attached.label, "working tree")
	}
	snap := m.attached.buf.Snapshot()
	if !strings.Contains(snap, "one.txt") || !strings.Contains(snap, "two.txt") {
		t.Errorf("project-wide preview must cover both changes:\n%s", snap)
	}

	// The row must never be recorded as an @-mention target (it's a
	// preview target, not a path): click1 + click2 run the action, and the
	// triple-click machinery must find no path to mention.
	m2 := dragTestModel()
	m2.st.cwd = dir
	m2.projectFiles = []string{"one.txt", "two.txt"}
	m2.changesetFiles = m.changesetFiles
	m2.rebuildProjectTree()
	changesIdx := -1
	for i, id := range m2.projectIDs {
		if id == changesRowID {
			changesIdx = i
		}
	}
	m2.panelClickAction(regionProject, changesIdx)
	m2.panelClickAction(regionProject, changesIdx)
	if m2.lastTreeClickPath != "" {
		t.Errorf("changes row leaked into the @-mention path tracker: %q", m2.lastTreeClickPath)
	}
}
