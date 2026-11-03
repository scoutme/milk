package main

import (
	"fmt"
	"reflect"
	"sort"
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

// TestBlurDoesNotGatePollingWork pins issue #389: focus is presentation-
// only. The project and git refresh ticks must produce the same probe
// messages while blurred as while focused — those probes are #347's
// panel-event source (toasts, auto-open, veto release), and a
// multi-display viewer keeps an eye on an unfocused window. The poll
// timers stay armed either way; the input-area dimming (#367) is the
// focus-dependent part, pinned separately below.
func TestBlurDoesNotGatePollingWork(t *testing.T) {
	m := layoutTestModel(t, 80, 24)
	m.panelGit = true
	m.panelProject = true

	// msgTypes flattens the returned command tree (batches included) and
	// records each leaf's message type — the shape a gate would change.
	msgTypes := func(cmd tea.Cmd) []string {
		t.Helper()
		var out []string
		var walk func(tea.Cmd)
		walk = func(c tea.Cmd) {
			if c == nil {
				return
			}
			got := c()
			if b, ok := got.(tea.BatchMsg); ok {
				for _, inner := range b {
					walk(inner)
				}
				return
			}
			out = append(out, fmt.Sprintf("%T", got))
		}
		walk(cmd)
		sort.Strings(out)
		return out
	}

	for _, tc := range []struct {
		name  string
		msg   tea.Msg
		setup func(*model)
	}{
		{"project tick, panel open", projectRefreshMsg{}, func(m *model) { m.panelProject = true }},
		{"project tick, panel closed (the #347 event source)", projectRefreshMsg{}, func(m *model) { m.panelProject = false }},
		{"git tick, panel open", gitRefreshMsg{}, func(m *model) { m.panelGit = true }},
		{"git tick, panel closed", gitRefreshMsg{}, func(m *model) { m.panelGit = false }},
	} {
		m.panelGit, m.panelProject = false, false
		tc.setup(&m)
		m.blurred = false
		_, focused := m.Update(tc.msg)
		m.blurred = true
		_, blurred := m.Update(tc.msg)
		if focused == nil {
			t.Fatalf("%s: the poll timer must stay armed while focused", tc.name)
		}
		if blurred == nil {
			t.Fatalf("%s: the poll timer must stay armed while blurred", tc.name)
		}
		gotF, gotB := msgTypes(focused), msgTypes(blurred)
		if len(gotF) == 0 {
			t.Fatalf("%s: focused refresh ran no probe", tc.name)
		}
		if !reflect.DeepEqual(gotB, gotF) {
			t.Errorf("%s: blurred produced message types %v, focused produced %v — blur must not gate the probe (issue #389)", tc.name, gotB, gotF)
		}
	}

	// The flag itself still tracks focus (the dim rendering depends on it).
	next, _ := m.Update(tea.FocusMsg{})
	if next.(model).blurred {
		t.Error("focus should clear the blurred flag")
	}
}

// TestBlurKeepsTipRotation pins issue #389 the same way for the idle tip
// line: an idle unfocused window keeps cycling tips (the multi-display
// viewer's idle view stays alive). The ADR-0061 attention gates (busy,
// pending prompt) keep freezing rotation regardless of focus — the busy
// half is pinned in tips_test.go.
func TestBlurKeepsTipRotation(t *testing.T) {
	m := layoutTestModel(t, 80, 24)
	m.blurred = true
	before := m.tipIdx
	next, _ := m.Update(tipsTickMsg{gen: m.tipsGen})
	if next.(model).tipIdx == before {
		t.Error("tips must keep rotating while blurred (issue #389)")
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
