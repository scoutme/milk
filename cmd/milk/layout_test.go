package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/scoutme/milk/internal/session"
)

// layoutTestModel returns a model with just enough state initialised
// (viewport, textarea, session) to exercise handleResize/syncLayout/
// refreshPrompt without panicking on nil dependencies.
func layoutTestModel(t *testing.T, width, height int) model {
	t.Helper()
	m := model{
		transcript:        &strings.Builder{},
		transcriptNoThink: &strings.Builder{},
	}
	m.st = &interactiveState{sess: &session.Session{}}
	m.ta = textarea.New()
	nm, _ := m.handleResize(tea.WindowSizeMsg{Width: width, Height: height})
	return nm.(model)
}

// TestSyncLayout_KeepsTextareaWidthInSyncWithoutRefreshPrompt guards against a
// bug where opening/closing the workflow panel (which changes mainWidth/
// vpWidth) never re-synced the textarea's wrap width, because every
// workflowPanelOpen call site called only syncLayout(), not refreshPrompt() —
// the only place that used to set the textarea width. The input then kept
// wrapping at the width from before the panel toggled, producing incoherent
// wrapping whenever the memory/workflow panels were shown or hidden.
//
// refreshPrompt is used here only as an independent ground truth for what the
// textarea width *should* be at the current vpWidth() — the fix must make
// syncLayout alone converge to that value, without an explicit refreshPrompt call.
func TestSyncLayout_KeepsTextareaWidthInSyncWithoutRefreshPrompt(t *testing.T) {
	m := layoutTestModel(t, 120, 40)

	m.workflowPanelOpen = true
	m.syncLayout()
	gotWidth := m.ta.Width()

	m.refreshPrompt() // ground truth for the current vpWidth()
	want := m.ta.Width()

	if gotWidth != want {
		t.Errorf("syncLayout alone left ta.Width()=%d, want %d (what refreshPrompt computes for the same vpWidth) — "+
			"syncLayout must keep the textarea width coherent on every panel toggle, not just call sites that also call refreshPrompt",
			gotWidth, want)
	}
}

// TestRefreshPrompt_TextareaWidthMatchesViewportWidth guards against the
// textarea being set to mainWidth() instead of vpWidth(): the textarea is
// rendered as content inside the transcript viewport (see setViewportContent),
// so its wrap width must reflect vpWidth() (mainWidth() minus the scrollbar
// column), not the wider mainWidth() — otherwise every input line overflows
// the viewport's own column budget by one column.
func TestRefreshPrompt_TextareaWidthMatchesViewportWidth(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	baseline := m.ta.Width()

	// vpWidth() is exactly 1 less than mainWidth(); SetWidth(mainWidth())
	// vs SetWidth(vpWidth()) must produce textarea widths 1 apart.
	m.ta.SetWidth(m.mainWidth())
	withMainWidth := m.ta.Width()

	m.refreshPrompt()
	withVPWidth := m.ta.Width()

	if withVPWidth != baseline {
		t.Errorf("refreshPrompt should reproduce the original vpWidth()-based width %d, got %d", baseline, withVPWidth)
	}
	if withMainWidth-withVPWidth != 1 {
		t.Errorf("expected mainWidth()-based width to exceed vpWidth()-based width by exactly 1, got delta %d",
			withMainWidth-withVPWidth)
	}
}
