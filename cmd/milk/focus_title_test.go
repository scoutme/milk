package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestWindowTitle_ReflectsState(t *testing.T) {
	m := layoutTestModel(t, 80, 24)
	if got := m.windowTitle(); got == "" || got[:4] != "milk" {
		t.Fatalf("title should start with the app name, got %q", got)
	}
	idle := m.windowTitle()
	// Issue #367: the state is always present as `<glyph> <word>`, idle
	// being the quiet default with a static glyph.
	if got := m.windowTitle(); !strings.HasSuffix(got, titleGlyphIdle+" "+titleIdle) {
		t.Errorf("idle title should end with %q, got %q", titleGlyphIdle+" "+titleIdle, got)
	}
	// Busy: the glyph is a braille spinner frame and it MOVES with the
	// spinner tick — two frames must yield two titles (motion proof), and
	// the word suffix keeps the vocabulary pinned below.
	m.busy = true
	m.spinnerFrame = 0
	busyA := m.windowTitle()
	if got := busyA; got == idle || !strings.HasSuffix(got, spinnerFrames[0]+" "+titleWorking) {
		t.Errorf("busy title should end with %q, got %q", spinnerFrames[0]+" "+titleWorking, got)
	}
	m.spinnerFrame = 1
	busyB := m.windowTitle()
	if busyA == busyB {
		t.Errorf("busy glyph must rotate with the spinner frame: both %q", busyA)
	}
	if !strings.HasSuffix(busyB, spinnerFrames[1]+" "+titleWorking) {
		t.Errorf("busy title frame 1 should end with %q, got %q", spinnerFrames[1]+" "+titleWorking, busyB)
	}
	m.busy = false
	m.pendingPerm = &permRequestMsg{}
	if got := m.windowTitle(); !strings.HasSuffix(got, titleGlyphAsk+" "+titleNeedsInput) {
		t.Errorf("pending prompt title should end with %q, got %q", titleGlyphAsk+" "+titleNeedsInput, got)
	}
}

func TestView_DeclaresFocusReportingAndTitle(t *testing.T) {
	m := layoutTestModel(t, 80, 24)
	v := m.View()
	if !v.ReportFocus {
		t.Error("view must request focus reports")
	}
	if !v.AltScreen {
		t.Error("view must stay on the alt screen")
	}
	if v.WindowTitle == "" {
		t.Error("view must set a window title")
	}
}

func TestBlurPausesPanelPolling(t *testing.T) {
	m := layoutTestModel(t, 80, 24)
	m.panelGit = true
	next, _ := m.Update(tea.BlurMsg{})
	m = next.(model)
	if !m.blurred {
		t.Fatal("blur should mark the model")
	}
	_, cmd := m.Update(gitRefreshMsg{})
	if cmd == nil {
		t.Fatal("the poll timer must stay armed while blurred")
	}
	next, _ = m.Update(tea.FocusMsg{})
	if next.(model).blurred {
		t.Error("focus should clear the blurred flag")
	}
}

func TestBlurFreezesTipRotation(t *testing.T) {
	m := layoutTestModel(t, 80, 24)
	m.blurred = true
	before := m.tipIdx
	next, _ := m.Update(tipsTickMsg{gen: m.tipsGen})
	if next.(model).tipIdx != before {
		t.Error("tips must not rotate while the window is blurred")
	}
}

// TestBlurRendersInputInactive_FocusRestoresIt pins the focus-aware input
// area (issue #367): on BlurMsg the input tail rebuilds dimmed and without
// the reverse-video cursor — nothing keeps blinking in a backgrounded tab —
// while visible text and layout stay byte-identical under ANSI stripping;
// on FocusMsg the exact focused rendering returns, since focus arrives with
// no keystroke that would refresh the viewport otherwise.
func TestBlurRendersInputInactive_FocusRestoresIt(t *testing.T) {
	withTTY(t)
	m := layoutTestModel(t, 80, 24)
	m.ta.SetValue("hello world")
	m.setViewportContent()
	focused := m.vp.View()
	if !strings.Contains(ansi.Strip(focused), "hello world") {
		t.Fatalf("test setup: typed text missing from viewport: %q", focused)
	}

	next, _ := m.Update(tea.BlurMsg{})
	m2 := next.(model)
	if !m2.blurred {
		t.Fatal("blur should mark the model")
	}
	blurred := m2.vp.View()
	if !strings.Contains(blurred, dimCode()) {
		t.Errorf("blurred input must be dimmed, got %q", blurred)
	}
	if _, _, ok := findReverseCell(blurred); ok {
		t.Errorf("cursor cell must not survive a blur (no blink in a background tab): %q", blurred)
	}
	if ansi.Strip(blurred) != ansi.Strip(focused) {
		t.Errorf("blur must not change visible content/layout:\nblur  %q\nfocus %q",
			ansi.Strip(blurred), ansi.Strip(focused))
	}

	next, _ = m2.Update(tea.FocusMsg{})
	if got := next.(model).vp.View(); got != focused {
		t.Errorf("focus must restore the exact focused rendering:\ngot  %q\nwant %q", got, focused)
	}
}
