package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/scoutme/milk/internal/session"
)

// layoutTestModel returns a model with just enough state initialised
// (viewport, textarea, session) to exercise handleResize/syncLayout/
// refreshPrompt without panicking on nil dependencies.
func layoutTestModel(t *testing.T, width, height int) model {
	t.Helper()
	m := model{
		transcript: &strings.Builder{},
		// Content-driven auto-open defaults on (ADR-0087) — this bare
		// construction means "unset", matching newModel with default config.
		panelAutoOpen: true,
	}
	m.st = &interactiveState{sess: &session.Session{}}
	m.ta = buildTextarea()
	// A zero-value model would read as a live selection on line 0 (anchor and
	// end both 0) and applySelectionHighlight would highlight that row —
	// start with no selection, like newModel does.
	m.clearSelection()
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

// TestSyncViewportThrottled_DefersRebuildWithinWindow verifies the streaming
// chunk perf fix: a burst of appendTranscriptStreamed calls within the same
// viewportRebuildThrottle window must not each trigger a full viewport
// rebuild (bubbles/viewport.SetContent re-measures the entire content every
// call) — only the first one should rebuild; the rest just mark the viewport
// dirty for flushViewportIfDirty to catch up later.
func TestSyncViewportThrottled_DefersRebuildWithinWindow(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	// layoutTestModel's initial handleResize already did one rebuild moments
	// ago; reset the clock so the throttle window starts fresh for this test.
	m.lastViewportRebuild = time.Time{}

	m.appendTranscriptStreamed("first chunk\n")
	rebuildAfterFirst := m.lastViewportRebuild
	if rebuildAfterFirst.IsZero() {
		t.Fatal("expected the first streamed chunk (outside any throttle window) to rebuild immediately")
	}
	if m.viewportDirty {
		t.Fatal("expected no pending dirty state right after an immediate rebuild")
	}

	m.appendTranscriptStreamed("second chunk\n")
	if !m.viewportDirty {
		t.Error("expected the second chunk (within the throttle window) to be deferred, not rebuilt immediately")
	}
	if m.lastViewportRebuild != rebuildAfterFirst {
		t.Error("expected no new rebuild timestamp while still within the throttle window")
	}
	if !strings.Contains(m.transcript.String(), "second chunk") {
		t.Error("expected the throttled chunk's text to still be appended to the transcript builder immediately")
	}

	// flushViewportIfDirty (the spinnerTickMsg catch-up) must pick up the
	// deferred content even though the throttle window hasn't elapsed yet —
	// callers outside the hot streaming path always get an immediate, correct view.
	m.flushViewportIfDirty()
	if m.viewportDirty {
		t.Error("expected flushViewportIfDirty to clear the pending dirty state")
	}
	if !strings.Contains(m.vp.View(), "second chunk") {
		t.Error("expected the deferred chunk's text to be visible in the viewport after flushViewportIfDirty")
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
