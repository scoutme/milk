package main

// Bang-mode completion: the buffer is a shell command, so slash-command
// completion — inline hints, Tab, subcommands, slash parameters — has no
// business completing it ("/usr/bin/thing" is a command, not a slash token:
// isSlashCmdToken matches any single-slash word). @-path completion stays —
// commands take file arguments.

import (
	"strings"
	"testing"
	"time"
)

func TestBuildTabMatchesMode_BangSuppressesSlashKeepsPaths(t *testing.T) {
	lookup := fakeLookup(testAgents)

	// Each slash case: empty in bang mode, non-empty in the plain prompt
	// (the control pins that the normal path still completes it).
	slashCases := []string{
		"/panel",           // top-level prefix completion
		"/memory ",         // trailing-space subcommand listing
		"/memory sh",       // partial subcommand
		"/agent switch cl", // slash-parameter values (needs lookup)
	}
	for _, in := range slashCases {
		bang := buildTabMatchesMode(in, ".", lookup, true)
		if len(bang.matches) != 0 || bang.replaceBase != "" || bang.valueMode {
			t.Errorf("bang: buildTabMatchesMode(%q) = %+v, want empty", in, bang)
		}
		normal := buildTabMatches(in, ".", lookup)
		if len(normal.matches) == 0 {
			t.Errorf("control: plain prompt lost completion for %q", in)
		}
	}

	// Trailing space after a shell word stays inert (no param fallthrough).
	if res := buildTabMatchesMode("git ", ".", lookup, true); len(res.matches) != 0 || res.valueMode {
		t.Errorf("bang trailing-space = %+v, want empty", res)
	}

	// @-path completion keeps working in bang mode (commands take paths) —
	// but as PLAIN paths: "@" is milk's trigger, the shell reads it literally.
	repo := gitRepo(t)
	writeFiles(t, repo, "docs/acp-session-resume-plan.md", "main.go")
	// The @-index builds asynchronously (fileindex.go get/runBuild) and a
	// lookup only waits fileIndexTabWait on the very first build — a starved
	// -race runner can answer mid-build with an empty snapshot (CI: macOS,
	// 2026-11-08: "bang @-path matches = [], want the plain indexed file").
	// Warm the index with a generous wait first so the assertions below run
	// against a built snapshot and stay timing-independent.
	if indexFor(repo).get(5*time.Second) == nil {
		t.Fatal("file index did not build for the fixture repo")
	}
	res := buildTabMatchesMode("look at @sess", repo, lookup, true)
	if len(res.matches) != 1 || res.matches[0] != "docs/acp-session-resume-plan.md" {
		t.Errorf("bang @-path matches = %v, want the plain indexed file", res.matches)
	}
	// Control: the plain prompt keeps the mention sigil.
	ctrl := buildTabMatches("look at @sess", repo, lookup)
	if len(ctrl.matches) != 1 || ctrl.matches[0] != "@docs/acp-session-resume-plan.md" {
		t.Errorf("plain prompt @-path matches = %v, want the @-prefixed file", ctrl.matches)
	}
}

// End-to-end through handleTab: pins that the model's bang state reaches the
// match builder (not just the free function in isolation).
func TestHandleTab_BangModeGatesTheModel(t *testing.T) {
	m := newTestModelForPanels(t)
	m.ta.SetValue("/panel")

	m, _ = m.handleTab(1)
	if len(m.tabMatches) == 0 {
		t.Fatal("plain prompt: expected /panel completion")
	}

	m.tabMatches = nil // fresh build on the next press
	m.bangMode = true
	m, _ = m.handleTab(1)
	if len(m.tabMatches) != 0 {
		t.Errorf("bang mode: tabMatches = %v, want empty", m.tabMatches)
	}
}

// Inline hints follow the same rule: slash hints off in bang mode, @-path
// hints still on.
func TestRebuildInlineHints_BangMode(t *testing.T) {
	m := newTestModelForPanels(t)

	m.ta.SetValue("/")
	m.rebuildInlineHints()
	if len(m.tabHints) == 0 {
		t.Fatal("plain prompt: expected slash hints for \"/\"")
	}

	m.bangMode = true
	m.ta.SetValue("/panel")
	m.rebuildInlineHints()
	if len(m.tabHints) != 0 {
		t.Errorf("bang mode: slash hints = %v, want none", m.tabHints)
	}

	// @-path hints survive: cwd gets a real file for the index to find.
	m.st.cwd = gitRepo(t)
	writeFiles(t, m.st.cwd, "README.md")
	m.ta.SetValue("@read")
	m.rebuildInlineHints()
	if len(m.tabHints) == 0 {
		t.Errorf("bang mode: @-path hints for @read = none, want the file")
	}
	if strings.Contains(m.tabHints[0], "@") {
		t.Errorf("bang mode: hint %q still shows the @ trigger", m.tabHints[0])
	}

	// Control: the plain prompt shows the mention sigil.
	m.bangMode = false
	m.rebuildInlineHints()
	if len(m.tabHints) == 0 || !strings.Contains(m.tabHints[0], "@") {
		t.Errorf("plain prompt: @-path hints = %v, want the @-prefixed file", m.tabHints)
	}
}

// Bang mode inserts the PLAIN path — Tab cycling, arrow-insert (insertActive
// Completion) and hint-commit all replace the "@" trigger token while
// preserving the rest of the command: "cat @sess" + Tab must become
// "cat docs/…", never "@docs/…" (sh reads it literally) and never lose
// "cat " (applyTabCompletion's plain path replaces the LAST SLASH token).
func TestBangCompletion_InsertsPlainPath(t *testing.T) {
	repo := gitRepo(t)
	writeFiles(t, repo, "docs/sess-a.md", "docs/sess-b.md")

	file := func(t *testing.T, line string) string {
		t.Helper()
		if !strings.HasPrefix(line, "cat ") {
			t.Errorf("command prefix lost: %q", line)
		}
		if strings.Contains(line, "@") {
			t.Errorf("@ trigger survived into the buffer: %q", line)
		}
		rest := strings.TrimPrefix(line, "cat ")
		if rest != "docs/sess-a.md" && rest != "docs/sess-b.md" {
			t.Errorf("not a completed candidate: %q", line)
		}
		return rest
	}

	// Tab, then Tab again: both candidates cycle through the snapshot.
	m := newTestModelForPanels(t)
	m.st.cwd = repo
	m.bangMode = true
	m.ta.SetValue("cat @sess")
	m, _ = m.handleTab(1)
	first := file(t, m.ta.Value())
	m, _ = m.handleTab(1)
	second := file(t, m.ta.Value())
	if first == second {
		t.Errorf("cycling did not advance: %q twice", first)
	}

	// Arrow-insert path (up/down during cycling) uses the same builder.
	n := newTestModelForPanels(t)
	n.st.cwd = repo
	n.bangMode = true
	n.ta.SetValue("cat @sess")
	n, _ = n.handleTab(1)
	n = n.insertActiveCompletion()
	file(t, n.ta.Value())

	// Hint-commit path (arrow-hint selection + Enter).
	h := newTestModelForPanels(t)
	h.st.cwd = repo
	h.bangMode = true
	h.ta.SetValue("cat @sess")
	h.rebuildInlineHints()
	if len(h.tabHints) == 0 {
		t.Fatal("bang mode: expected @-path hints for @sess")
	}
	h.hintIdx = 0
	if !h.commitHintSelection() {
		t.Fatal("hint selection did not commit")
	}
	file(t, h.ta.Value())
}
