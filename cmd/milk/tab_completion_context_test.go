package main

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// Tab inside completion context must never fall through to panel focus
// rotation (user report, 2026-11-08: "tab MUST NOT be swallowed by panel
// focus management"). Reproduced case: a parameter-value position ("/agent
// switch ") shows its inline hint, but the value build has no candidates
// (empty namespace) — before the guard, handleTab returned !handled and the
// focus rotated away mid-completion.
//
// Scope note (second user report, same day): the completion contract holds
// while the MAIN area holds focus — the only state typing can actually
// produce (type-to-focus means completion context implies main focus; these
// fixtures manufacture it with SetValue, which bypasses both). With a panel
// focused, Tab/Shift+Tab rotate outright regardless of leftover context —
// pinned separately by TestTabKey_PanelFocus_OwnsTabEvenWithCompletionContext.

func TestTabKey_CompletionContextNeverRotates(t *testing.T) {
	m := newTestModelForPanels(t)
	m.width = 400
	m.panelMemory, m.panelTasks = true, true

	// Parameter-value position: hint visible, no buildable values.
	m.ta.SetValue("/agent switch ")
	m.rebuildInlineHints()
	if len(m.tabHints) == 0 {
		t.Fatal("fixture: expected an inline hint at /agent switch ")
	}
	for _, dir := range []struct {
		name string
		msg  tea.KeyPressMsg
	}{
		{"tab", tea.KeyPressMsg{Code: tea.KeyTab}},
		{"shift+tab", tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}},
	} {
		m.panelFocus = regionNone
		next, _ := m.handleKey(dir.msg)
		got := next.(model)
		if got.focusedPanel() != regionNone {
			t.Errorf("%s in completion context: focus moved to %v — Tab must not rotate on the input",
				dir.name, got.focusedPanel())
		}
	}

	// @-token context (path completion) with no candidates — e.g. the
	// index still building — likewise keeps focus: onAtToken is the
	// context signal even in bang mode (where hints are off).
	m.tabMatches = nil
	m.hintIdx = -1
	m.ta.SetValue("cat @nosuchfile")
	m.setHints(nil)
	m.panelFocus = regionNone
	next, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	if got := next.(model); got.focusedPanel() != regionNone {
		t.Errorf("@-token Tab: focus moved to %v — path completion context must not rotate on the input", got.focusedPanel())
	}

	// Control 1: genuinely non-completable input still rotates (no hints).
	m.ta.Reset()
	m.tabMatches = nil
	m.hintIdx = -1
	m.rebuildInlineHints()
	m.panelFocus = regionNone
	next, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	if got := next.(model); got.focusedPanel() == regionNone {
		t.Error("empty input without completion context: Tab should still rotate into the panels")
	}

	// Control 2: completion still wins over rotation when candidates exist.
	m.panelFocus = regionNone
	m.tabMatches = nil
	m.hintIdx = -1
	m.ta.SetValue("/pan")
	m.rebuildInlineHints()
	next, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	got := next.(model)
	if got.focusedPanel() != regionNone || len(got.tabMatches) == 0 {
		t.Errorf("completable input: focus=%v matches=%d — completion must win", got.focusedPanel(), len(got.tabMatches))
	}
}
