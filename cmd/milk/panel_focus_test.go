package main

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	viewport "github.com/scoutme/milk/internal/vp"
)

// Issue #412: keyboard-focusable side panels — rotation, scroll routing,
// click-to-focus, and the two render consequences (input dim, focus tint).

// TestShiftPageKeys_UseTheDocumentedNames pins the key-string contract the
// bindings live on: bubbletea must format the shift-modified page keys the
// way handleKey's cases read them. If this fails, the bindings are dead
// keys, not cosmetic ones.
func TestShiftPageKeys_UseTheDocumentedNames(t *testing.T) {
	if s := (tea.KeyPressMsg{Code: tea.KeyPgUp, Mod: tea.ModShift}).String(); s != "shift+pgup" {
		t.Errorf("shift+pgup String() = %q — handleKey matches %q", s, "shift+pgup")
	}
	s := (tea.KeyPressMsg{Code: tea.KeyPgDown, Mod: tea.ModShift}).String()
	if s != "shift+pgdown" && s != "shift+pgdn" {
		t.Errorf("shift+pgdown String() = %q — handleKey matches %q / %q", s, "shift+pgdown", "shift+pgdn")
	}
}

// TestRotatePanelFocus_CycleMainAndVisible walks the full cycle through the
// real key binding: [main, six visible panels] in panelRenderOrder, wrapping
// at both ends, with main reachable from both directions (it is the only
// keyboard path back to the input area).
func TestRotatePanelFocus_CycleMainAndVisible(t *testing.T) {
	m := newTestModelForPanels(t)
	m.width = 400 // fits everything: all six free, no group
	m.panelMemory, m.panelTasks, m.panelBackground = true, true, true
	m.workflowPanelOpen, m.panelProject, m.panelGit = true, true, true

	want := []panelRegion{regionNone, regionMemory, regionTasks, regionBackground,
		regionWorkflow, regionProject, regionGit, regionNone}
	for i := 1; i < len(want); i++ {
		next, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyPgDown, Mod: tea.ModShift})
		m = next.(model) // handleKey is a value receiver: the focus lives in the returned model
		if m.focusedPanel() != want[i] {
			t.Fatalf("step %d forward: focus = %v, want %v", i, m.focusedPanel(), want[i])
		}
	}
	// Backward from main wraps to the last panel, not out of bounds.
	next, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyPgUp, Mod: tea.ModShift})
	m = next.(model)
	if m.focusedPanel() != regionGit {
		t.Fatalf("backward from main: focus = %v, want git (wrap)", m.focusedPanel())
	}
}

// TestRotatePanelFocus_SkipsHiddenAndClosed: only actually-rendered panels
// join the cycle (ADR-0089 budget), and a focused-but-closed panel reads as
// main through the self-healing accessor.
func TestRotatePanelFocus_SkipsHiddenAndClosed(t *testing.T) {
	m := newTestModelForPanels(t)
	m.width = 73 // floor((73-40)/33) = 1 slot: only the freshest open panel fits
	m.panelMemory, m.panelProject = true, true
	m.stampPanelActivity(regionMemory) // memory is the top; project is open but hidden

	if v := m.focusedPanel(); v != regionNone {
		t.Fatalf("no focus yet = %v, want main", v)
	}
	next, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyPgDown, Mod: tea.ModShift})
	m = next.(model)
	if got := m.focusedPanel(); got != regionMemory {
		t.Fatalf("first stop = %v, want memory (the only visible panel)", got)
	}
	next, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyPgDown, Mod: tea.ModShift})
	m = next.(model)
	if got := m.focusedPanel(); got != regionNone {
		t.Fatalf("second stop = %v, want main — project is hidden and must be skipped", got)
	}

	// Self-heal: focus parked on a closed (or hidden) panel reads as main.
	m.panelFocus = regionProject
	if got := m.focusedPanel(); got != regionNone {
		t.Fatalf("focus on hidden project = %v, want main (self-healing)", got)
	}
	m.panelFocus = regionGit
	m.panelGit = false
	if got := m.focusedPanel(); got != regionNone {
		t.Fatalf("focus on closed git = %v, want main (self-healing)", got)
	}
}

// TestPgUpPgDn_RouteToFocusedPanel: paging follows focus — the focused
// panel's offset moves while the transcript viewport doesn't, and with main
// focused the transcript scrolls exactly as before.
func TestPgUpPgDn_RouteToFocusedPanel(t *testing.T) {
	m := newTestModelForPanels(t)
	m.width, m.height = 400, 40
	m.panelProject = true
	m.projectLines = []string{"files"} // line 0 is the title row
	for i := 0; i < 80; i++ {
		m.projectLines = append(m.projectLines, fmt.Sprintf("row %02d", i))
	}
	m.vp = viewport.New(viewport.WithWidth(200), viewport.WithHeight(20))
	m.vp.SetLines(func() []string {
		out := make([]string, 100)
		for i := range out {
			out[i] = fmt.Sprintf("t%03d", i)
		}
		return out
	}())
	m.vp.HalfPageDown()
	y0 := m.vp.YOffset()
	if y0 == 0 {
		t.Fatal("transcript did not scroll — fixture broken, nothing to prove routing with")
	}

	// Focus the project panel: paging now moves its offset, not the transcript.
	m.panelFocus = regionProject
	h := m.viewportHeight()
	step := max(h/2, 1)
	next, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	m = next.(model)
	if got := m.projectOffset; got != step {
		t.Errorf("projectOffset after pgdown = %d, want %d (half panel)", got, step)
	}
	if got := m.vp.YOffset(); got != y0 {
		t.Errorf("transcript YOffset = %d, want %d — paging leaked to the main area", got, y0)
	}
	next, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m = next.(model)
	if got := m.projectOffset; got != 0 {
		t.Errorf("projectOffset after pgup = %d, want 0", got)
	}
	if got := m.vp.YOffset(); got != y0 {
		t.Errorf("transcript YOffset = %d, want %d — paging leaked to the main area", got, y0)
	}

	// Back on main: transcript paging is the original behavior.
	m.panelFocus = regionNone
	next, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	m = next.(model)
	if got := m.vp.YOffset(); got <= y0 {
		t.Errorf("transcript after main-focused pgdown = %d, want > %d", got, y0)
	}
	if got := m.projectOffset; got != 0 {
		t.Errorf("projectOffset with main focus = %d, want 0 (untouched)", got)
	}

	// Clamped exactly like the wheel: [0, panelMaxOffset].
	m.panelFocus = regionProject
	for range 60 {
		m.scrollFocusedPanel(1)
	}
	if maxOff := m.panelMaxOffset(regionProject, h); m.projectOffset != maxOff {
		t.Errorf("scroll clamped at %d, want %d", m.projectOffset, maxOff)
	}
	for range 60 {
		m.scrollFocusedPanel(-1)
	}
	if m.projectOffset != 0 {
		t.Errorf("scroll floored at %d, want 0", m.projectOffset)
	}
}

// TestClickSetsFocus_WheelDoesNot: left press moves focus (panel in, main
// out); wheel scrolling is not a change of intent and never moves it.
func TestClickSetsFocus_WheelDoesNot(t *testing.T) {
	m := newTestModelForPanels(t)
	m.width, m.height = 400, 40
	m.panelMemory = true

	var memX, mainX int
	foundMain := false
	for x := 0; x < m.width; x++ {
		r, _ := m.regionAt(x)
		switch {
		case r == regionMemory && memX == 0:
			memX = x
		case r == regionNone && x > 0 && !foundMain:
			mainX, foundMain = x, true
		}
	}
	if memX == 0 || !foundMain {
		t.Fatalf("could not probe layout (memX=%d mainX=%d)", memX, mainX)
	}
	y, _ := m.viewportRows()

	press := func(x int) {
		m.handleMouse(mouseEvent{X: x, Y: y, Button: tea.MouseLeft, Action: mousePress}.msg())
	}
	press(memX)
	if m.focusedPanel() != regionMemory {
		t.Fatalf("click in memory panel: focus = %v, want memory", m.focusedPanel())
	}
	press(mainX)
	if m.focusedPanel() != regionNone {
		t.Fatalf("click in main area: focus = %v, want main", m.focusedPanel())
	}

	// Wheel over a panel: scrolls (offset may move), focus must not.
	m.handleMouse(mouseEvent{X: memX, Y: y, Button: tea.MouseWheelUp, Action: mouseMotion}.msg())
	if m.focusedPanel() != regionNone {
		t.Fatalf("wheel moved focus to %v — wheel is not an intent change", m.focusedPanel())
	}
	m.panelFocus = regionMemory
	m.handleMouse(mouseEvent{X: memX, Y: y, Button: tea.MouseWheelDown, Action: mouseMotion}.msg())
	if m.focusedPanel() != regionMemory {
		t.Fatalf("wheel moved focus to %v, want memory (unchanged)", m.focusedPanel())
	}
}

// TestPanelFocus_DimsInputLikeUnfocusedWindow: while a panel holds focus the
// input area renders byte-identically to the unfocused-window dim (#367) —
// same pipeline, same result — and text structure is untouched.
func TestPanelFocus_DimsInputLikeUnfocusedWindow(t *testing.T) {
	withTTY(t)

	// Same fixture as the shipped #367 blur test, but wider/taller: this
	// fixture starts with a ~35-line transcript block, and at 80 cols the
	// input rows sit below the window the tests read (measured, not
	// guessed: at 160x40 the input is visible with the panel open).
	m := layoutTestModel(t, 160, 40)
	m.panelMemory = true
	m.ta.SetValue("hello world")

	m.setViewportContent()
	focused := m.vp.View()
	if !strings.Contains(ansi.Strip(focused), "hello world") {
		t.Fatalf("test setup: typed text missing from viewport: %q", focused)
	}

	// Panel focused: input dims.
	m.panelFocus = regionMemory
	m.setViewportContent()
	panelDim := m.vp.View()

	// Window unfocused (#367): the render this must match byte-for-byte.
	m.panelFocus = regionNone
	m.blurred = true
	m.setViewportContent()
	blurred := m.vp.View()

	if panelDim == focused {
		t.Fatal("focused panel did not dim the input — identical to the focused-window render")
	}
	if panelDim != blurred {
		t.Errorf("panel-focus dim differs from the unfocused-window dim:\n panel: %q\n blur:  %q", panelDim, blurred)
	}
	if ansi.Strip(focused) != ansi.Strip(panelDim) {
		t.Error("dim changed the visible text or row structure — scroll position would not survive")
	}
}

// TestFocusTint_RendersFromThemeToken: the focused panel's rows (body and
// scrollbar) carry the theme's focus tint — the token, never a literal —
// overriding the alternating parity, with the palette declared per theme.
func TestFocusTint_RendersFromThemeToken(t *testing.T) {
	oldTTY := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = oldTTY })
	setTheme("supermilk")
	t.Cleanup(func() { setTheme("off") })

	m := newTestModelForPanels(t)
	m.width = 400
	m.panelMemory = true

	// The token exists and is the supermilk value (RGB of #e7e9f5).
	tint := focusBgCode()
	if tint != "\033[48;2;231;233;245m" {
		t.Fatalf("focusBgCode() = %q, want the supermilk focus token", tint)
	}

	// Unfocused: alternating parity, never the focus tint.
	m.panelFocus = regionNone
	if body := m.renderSidePanel(regionMemory, 12); strings.Contains(body, tint) {
		t.Fatal("unfocused panel carries the focus tint")
	}
	if alt := panelAltBackgroundCode(); alt == "" || !strings.Contains(m.renderSidePanel(regionMemory, 12), alt) {
		t.Fatal("unfocused parity background missing — fixture must exercise the alt path")
	}

	// Focused: body and scrollbar carry the tint — focus wins over parity
	// (memory sits at parity index 0, where altBg is true).
	m.panelFocus = regionMemory
	if body := m.renderSidePanel(regionMemory, 12); !strings.Contains(body, tint) {
		t.Fatal("focused panel body lacks the focus tint")
	}
	if bar := m.renderSidePanelScrollbar(regionMemory, 12); !strings.Contains(bar, tint) {
		t.Fatal("focused panel scrollbar lacks the focus tint")
	}
	// Neighbouring panels stay untouched.
	m.panelTasks = true
	if body := m.renderSidePanel(regionTasks, 12); strings.Contains(body, tint) {
		t.Fatal("focus tint leaked to an unfocused panel")
	}

	// Non-TTY: no tint (and no escape materializes at all).
	isTTY = false
	if got := focusBgCode(); got != "" {
		t.Fatalf("focusBgCode() on non-TTY = %q, want empty", got)
	}
}

// TestFocusBgCode_OffThemeStillDeclaresATint: theme "off" keeps its colors
// in code by design, but focus is declared as a token there too — so the
// render path is theme-independent and escape-free for both themes.
func TestFocusBgCode_OffThemeStillDeclaresATint(t *testing.T) {
	oldTTY := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = oldTTY })
	setTheme("off")
	t.Cleanup(func() { setTheme("off") })

	if got := focusBgCode(); got == "" || !strings.HasPrefix(got, "\033[48;2;") {
		t.Fatalf("focusBgCode() on theme off = %q, want a truecolor tint", got)
	}
}
