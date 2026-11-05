package main

// Search-mode UX tests (Ctrl+R/Ctrl+S): what the prompt shows as the query
// changes, and how arrows leave the search. Corpus-independent — the
// corpus-side regressions live in shellhistory_test.go.

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func searchKeyRune(c rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: c, Text: string(c)} }

func searchKeyCtrlR() tea.KeyPressMsg { return tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl} }

// TestSearch_BackspaceToEmptyRestoresInput: erasing the whole query must
// clear the finding too — restore the input as it was when the search
// started, instead of leaving the last match on screen. The search stays
// active (Esc/Enter/arrow keys are what exit it).
func TestSearch_BackspaceToEmptyRestoresInput(t *testing.T) {
	m := newTestModelForPanels(t)
	m.sessionHistory = []string{"git status", "git push origin main"}
	m.ta.SetValue("draft")

	next, _ := m.handleKey(searchKeyCtrlR()) // entry captures saved = "draft"
	m = next.(model)
	next, _ = m.handleKey(searchKeyRune('p')) // query "p" → newest match
	m = next.(model)
	if got := m.ta.Value(); got != "git push origin main" {
		t.Fatalf("setup: want the match in the input, got %q", got)
	}

	next, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyBackspace})
	m = next.(model)
	if m.searchQuery.Len() != 0 {
		t.Fatalf("query not erased: %q", m.searchQuery.String())
	}
	if got := m.ta.Value(); got != "draft" {
		t.Errorf("erased query must restore the pre-search input, got %q", got)
	}
	if !m.searching {
		t.Error("backspace-to-empty should stay in search mode; Esc/Enter/arrows exit it")
	}
}

// TestSearch_EscWithoutTypingRestoresCurrentInput_NotStale: `saved` is
// captured at search ENTRY. Before that it was captured on the first typed
// character, so a no-typing Esc restored the PREVIOUS search's saved text —
// the input the user was looking at got clobbered by a stale value.
func TestSearch_EscWithoutTypingRestoresCurrentInput_NotStale(t *testing.T) {
	m := newTestModelForPanels(t)
	m.sessionHistory = []string{"match entry"}

	// Search 1: type, match, cancel — leaves saved behind as it must.
	m.ta.SetValue("first draft")
	next, _ := m.handleKey(searchKeyCtrlR())
	m = next.(model)
	next, _ = m.handleKey(searchKeyRune('m'))
	m = next.(model)
	if m.ta.Value() != "match entry" {
		t.Fatalf("setup: want match, got %q", m.ta.Value())
	}
	next, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = next.(model)
	if m.ta.Value() != "first draft" {
		t.Fatalf("search 1 cancel: got %q, want first draft", m.ta.Value())
	}

	// Search 2: enter and cancel without typing a character.
	m.ta.SetValue("second draft")
	next, _ = m.handleKey(searchKeyCtrlR())
	m = next.(model)
	next, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = next.(model)
	if got := m.ta.Value(); got != "second draft" {
		t.Errorf("no-typing Esc must restore THIS search's input, got %q", got)
	}
}

// TestSearch_ArrowsExitKeepingMatch: arrows mean "let me use and modify
// what I found". left/right exit the search and land the cursor in the
// match on the same press; up/down exit but are consumed (forwarding them
// would run historyBack/Forward and replace the match the press must
// preserve — historyBack's newest entry differs from the match here, so a
// walk would be visible).
func TestSearch_ArrowsExitKeepingMatch(t *testing.T) {
	setup := func(t *testing.T) model {
		t.Helper()
		m := newTestModelForPanels(t)
		// Bang mode navigates the SHELL corpus, not sessionHistory.
		m.shellHist = &shellHistCache{
			loaded: true, milkLoaded: true,
			lines: []string{"git status", "git push origin main"},
		}
		next, _ := m.handleKey(searchKeyRune('!')) // bang mode
		m = next.(model)
		if !m.bangMode {
			t.Fatal("setup: bang mode not entered")
		}
		next, _ = m.handleKey(searchKeyCtrlR())
		m = next.(model)
		for _, r := range "st" { // matches only the OLDER entry ("push" has no "st")
			next, _ = m.handleKey(searchKeyRune(r))
			m = next.(model)
		}
		if m.ta.Value() != "git status" { // matches only the OLDER entry
			t.Fatalf("setup: want match %q, got %q", "git status", m.ta.Value())
		}
		return m
	}

	t.Run("left exits and moves the cursor into the match", func(t *testing.T) {
		m := setup(t)
		end := m.taCursorOffset()
		next, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyLeft})
		m = next.(model)
		if m.searching {
			t.Error("left must exit search mode")
		}
		if m.searchQuery.Len() != 0 {
			t.Errorf("query must be cleared on exit, got %q", m.searchQuery.String())
		}
		if got := m.ta.Value(); got != "git status" {
			t.Errorf("match must stay in the input, got %q", got)
		}
		if m.taCursorOffset() != end-1 {
			t.Errorf("left must reach the textarea (cursor %d, want %d)", m.taCursorOffset(), end-1)
		}
	})

	t.Run("right exits and moves the cursor", func(t *testing.T) {
		m := setup(t)
		end := m.taCursorOffset()
		next, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyRight})
		m = next.(model)
		if m.searching {
			t.Error("right must exit search mode")
		}
		// Cursor at the end cannot move right — mode + text are the contract.
		if got := m.ta.Value(); got != "git status" {
			t.Errorf("match must stay, got %q", got)
		}
		_ = end
	})

	for _, key := range []struct {
		name string
		msg  tea.KeyPressMsg
	}{{"up", tea.KeyPressMsg{Code: tea.KeyUp}}, {"down", tea.KeyPressMsg{Code: tea.KeyDown}}} {
		t.Run(key.name+" exits without walking history", func(t *testing.T) {
			m := setup(t)
			next, _ := m.handleKey(key.msg)
			m = next.(model)
			if m.searching {
				t.Errorf("%s must exit search mode", key.name)
			}
			if got := m.ta.Value(); got != "git status" {
				t.Errorf("%s must not replace the kept match with a history walk, got %q", key.name, got)
			}
		})
	}
}

// TestSearch_QueryAdmitsSpacesAndNonASCII: the query must accept what a
// shell command contains. bubbletea reports space as the key name "space"
// (String()="space", Text="" — the printable-only default rejected it),
// and byte-length rune checks rejected every multi-byte character.
func TestSearch_QueryAdmitsSpacesAndNonASCII(t *testing.T) {
	chr := func(c rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: c, Text: string(c)} }
	space := tea.KeyPressMsg{Code: ' '}

	t.Run("space joins query tokens", func(t *testing.T) {
		m := newTestModelForPanels(t)
		m.sessionHistory = []string{"tool a b run", "make build"}
		next, _ := m.handleKey(searchKeyCtrlR())
		m = next.(model)
		for _, k := range []tea.KeyPressMsg{chr('a'), space, chr('b')} {
			next, _ = m.handleKey(k)
			m = next.(model)
		}
		if m.searchQuery.String() != "a b" {
			t.Errorf("query = %q, want %q (space dropped?)", m.searchQuery.String(), "a b")
		}
		if m.ta.Value() != "tool a b run" {
			t.Errorf("match = %q, want %q", m.ta.Value(), "tool a b run")
		}
	})

	t.Run("non-ASCII rune admitted", func(t *testing.T) {
		m := newTestModelForPanels(t)
		m.sessionHistory = []string{"grep café beans"}
		next, _ := m.handleKey(searchKeyCtrlR())
		m = next.(model)
		next, _ = m.handleKey(chr('é'))
		m = next.(model)
		if m.searchQuery.String() != "é" {
			t.Errorf("query = %q, want é (multi-byte rune rejected?)", m.searchQuery.String())
		}
		if m.ta.Value() != "grep café beans" {
			t.Errorf("match = %q, want the café entry", m.ta.Value())
		}
	})
}
