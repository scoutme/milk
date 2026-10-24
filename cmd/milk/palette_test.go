package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func paletteKey(text string) tea.KeyPressMsg {
	r := []rune(text)
	return tea.KeyPressMsg{Code: r[0], Text: text}
}

func TestComposeOverlays_KeepsRowWidthAndInsertsLayer(t *testing.T) {
	out := composeOverlays("hello world", overlayLayer{content: "XY", x: 3})
	if got := ansi.Strip(out); got != "helXY world" {
		t.Errorf("overlay = %q", got)
	}
	// Trailing blank cells are trimmed by the canvas; the row keeps its width.
	out = composeOverlays("ab      ", overlayLayer{content: "X", x: 0})
	if w := ansi.StringWidth(out); w != 8 {
		t.Errorf("row width = %d, want 8", w)
	}
}

func TestComposeOverlays_NoLayersReturnsBase(t *testing.T) {
	if got := composeOverlays("a\nb"); got != "a\nb" {
		t.Errorf("got %q", got)
	}
}

func TestPaletteEntries_RanksPrefixBeforeSubstring(t *testing.T) {
	all := paletteEntries("")
	if len(all) == 0 {
		t.Fatal("palette should list the TUI commands")
	}
	got := paletteEntries("esc")
	if len(got) == 0 || !strings.HasPrefix(got[0].name, "/esc") {
		t.Fatalf("prefix match must come first, got %+v", got)
	}
	if len(paletteEntries("zzzzqq")) != 0 {
		t.Error("no command should match gibberish")
	}
}

func TestPalette_OpenFilterInsertAndClose(t *testing.T) {
	m := layoutTestModel(t, 100, 30)
	next, _ := m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	m = next.(model)
	if m.palette == nil {
		t.Fatal("ctrl+o should open the palette")
	}
	if !strings.Contains(ansi.Strip(m.viewString()), "commands") {
		t.Error("open palette should be drawn over the main area")
	}
	for _, r := range "escal" {
		next, _ = m.Update(paletteKey(string(r)))
		m = next.(model)
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(model)
	if m.palette != nil {
		t.Error("enter should close the palette")
	}
	if !strings.HasPrefix(m.ta.Value(), "/escalate") {
		t.Errorf("input should hold the chosen command, got %q", m.ta.Value())
	}

	next, _ = m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	m = next.(model)
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if next.(model).palette != nil {
		t.Error("esc should close the palette without touching the input")
	}
}

func TestPalette_BlockedWhileAPromptIsPending(t *testing.T) {
	m := layoutTestModel(t, 100, 30)
	m.pendingPerm = &permRequestMsg{}
	if m.paletteAvailable() {
		t.Error("the palette must not open over a pending permission prompt")
	}
}

func TestContextMeter_FillsWithPressure(t *testing.T) {
	empty := ansi.Strip(contextMeter(0))
	half := ansi.Strip(contextMeter(50))
	full := ansi.Strip(contextMeter(150)) // clamped
	if ansi.StringWidth(empty) != contextMeterCells || ansi.StringWidth(full) != contextMeterCells {
		t.Fatalf("meter must be %d cells, got %q / %q", contextMeterCells, empty, full)
	}
	if strings.Count(full, "▰") != contextMeterCells || strings.Count(empty, "▰") != 0 {
		t.Errorf("fill must span empty..full, got %q / %q", empty, full)
	}
	if c := strings.Count(half, "▰"); c <= 0 || c >= contextMeterCells {
		t.Errorf("half fill should be partial, got %q", half)
	}
}
