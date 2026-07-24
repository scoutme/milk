package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// --- wrapLineIntoRows ---

func TestWrapLineIntoRows_ShortLine(t *testing.T) {
	runes := []rune("hello")
	rows := wrapLineIntoRows(runes, 20)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row for short line, got %d", len(rows))
	}
	if string(rows[0]) != "hello" {
		t.Errorf("expected 'hello', got %q", string(rows[0]))
	}
}

func TestWrapLineIntoRows_ExactWidth(t *testing.T) {
	runes := []rune("hello world") // 11 chars
	rows := wrapLineIntoRows(runes, 11)
	// Should fit in one row (not overflow).
	if len(rows) == 0 {
		t.Fatal("expected at least 1 row")
	}
}

func TestWrapLineIntoRows_WordWrap(t *testing.T) {
	// "hello world" wrapped at 8: "hello" fits (5), " world" would make 11 > 8 → wraps.
	runes := []rune("hello world")
	rows := wrapLineIntoRows(runes, 8)
	if len(rows) < 2 {
		t.Fatalf("expected ≥2 rows for word wrap at 8, got %d: %v", len(rows), rowStrings(rows))
	}
}

func TestWrapLineIntoRows_LongWord(t *testing.T) {
	// A single word longer than width should not panic.
	runes := []rune("abcdefghij") // 10 chars
	rows := wrapLineIntoRows(runes, 5)
	if len(rows) == 0 {
		t.Fatal("expected at least 1 row for long word")
	}
}

func TestWrapLineIntoRows_Empty(t *testing.T) {
	rows := wrapLineIntoRows(nil, 20)
	// Should return one empty row (textarea renders blank line for empty logical line).
	if len(rows) != 1 {
		t.Fatalf("expected 1 row for empty input, got %d", len(rows))
	}
}

func TestWrapLineIntoRows_PreservesRuneCount(t *testing.T) {
	runes := []rune("the quick brown fox jumps over the lazy dog")
	rows := wrapLineIntoRows(runes, 15)
	total := 0
	for _, r := range rows {
		total += len(r)
	}
	// Total runes across all rows should equal original (spaces at wrap boundaries
	// may be trimmed or kept depending on implementation — we only check no chars are dropped).
	if total < len(runes)-len(rows) { // allow one space lost per wrap boundary at most
		t.Errorf("too many chars lost: original %d, got %d across %d rows", len(runes), total, len(rows))
	}
}

func rowStrings(rows [][]rune) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = string(r)
	}
	return out
}

// --- applyInputHighlight ---

func TestApplyInputHighlight_FullLine(t *testing.T) {
	// Plain line, indent=0, select all.
	line := "hello world"
	result := applyInputHighlight(line, 0, 0, 11)
	if !strings.Contains(result, "\x1b[48;5;240m") {
		t.Error("expected selection highlight escape in output")
	}
	if !strings.Contains(result, "hello world") {
		t.Error("expected original text preserved in output")
	}
}

func TestApplyInputHighlight_PartialSelection(t *testing.T) {
	line := "hello world"
	result := applyInputHighlight(line, 0, 0, 5)
	// Should highlight "hello" and leave " world" unhighlighted.
	if !strings.Contains(result, "\x1b[48;5;240m") {
		t.Error("expected selection highlight escape")
	}
	if !strings.Contains(result, "\x1b[49m") {
		t.Error("expected background-reset escape after highlight")
	}
	plain := strings.ReplaceAll(strings.ReplaceAll(result, "\x1b[48;5;240m", ""), "\x1b[49m", "")
	if plain != "hello world" {
		t.Errorf("expected plain text unchanged, got %q", plain)
	}
}

func TestApplyInputHighlight_WithIndent(t *testing.T) {
	// 5-char indent prefix + "hello world" content.
	line := "     hello world"
	result := applyInputHighlight(line, 5, 0, 5)
	if !strings.Contains(result, "\x1b[48;5;240m") {
		t.Error("expected selection highlight inside content")
	}
	// Prefix should be preserved.
	if !strings.HasPrefix(result, "     ") {
		preview := result
		if len(preview) > 10 {
			preview = preview[:10]
		}
		t.Errorf("expected 5-space indent preserved, got %q", preview)
	}
}

func TestApplyInputHighlight_OutOfRange(t *testing.T) {
	line := "hello"
	// selLo > len → no change.
	result := applyInputHighlight(line, 0, 10, 20)
	if result != line {
		t.Errorf("out-of-range selection should return line unchanged, got %q", result)
	}
}

func TestApplyInputHighlight_EmptyLine(t *testing.T) {
	result := applyInputHighlight("", 0, 0, 5)
	if result != "" {
		t.Errorf("empty line should return empty, got %q", result)
	}
}

func TestApplyInputHighlight_NegativeLo(t *testing.T) {
	line := "hello world"
	// Negative selLo should be clamped to 0.
	result := applyInputHighlight(line, 0, -3, 5)
	if !strings.Contains(result, "\x1b[48;5;240m") {
		t.Error("expected highlight for negative selLo clamped to 0")
	}
}

// --- extendTranscriptSel ---

// selModel builds a minimal model with a 3-line transcript and an active
// selection anchor at line 1, col 2. The viewport is wide enough that each
// transcript line occupies exactly one wrapped line.
func selModel() model {
	tx := &strings.Builder{}
	tx.WriteString("line zero\nline one\nline two\n")
	m := model{
		transcript:        tx,
		transcriptNoThink: tx,
		selAnchorLine:     1,
		selAnchorCol:      2,
		selEndLine:        1,
		selEndCol:         4,
		colorizeMode:      ColorizeOff,
		vp:                viewport.New(80, 20),
	}
	m.vp.SetYOffset(0)
	return m
}

func extendSel(m model, kt tea.KeyType) model {
	result, _ := m.extendTranscriptSel(tea.KeyMsg{Type: kt})
	return result.(model)
}

func TestExtendTranscriptSel_ShiftDownMovesEndLine(t *testing.T) {
	nm := extendSel(selModel(), tea.KeyShiftDown)
	if nm.selEndLine != 2 {
		t.Errorf("shift+down: expected selEndLine=2, got %d", nm.selEndLine)
	}
}

func TestExtendTranscriptSel_ShiftUpMovesEndLine(t *testing.T) {
	nm := extendSel(selModel(), tea.KeyShiftUp) // anchor/end at line 1 → end moves to 0
	if nm.selEndLine != 0 {
		t.Errorf("shift+up: expected selEndLine=0, got %d", nm.selEndLine)
	}
}

func TestExtendTranscriptSel_ShiftUpClampedAtZero(t *testing.T) {
	m := selModel()
	m.selEndLine = 0
	nm := extendSel(m, tea.KeyShiftUp)
	if nm.selEndLine < 0 {
		t.Errorf("shift+up at line 0 should clamp, not go negative, got %d", nm.selEndLine)
	}
}

func TestExtendTranscriptSel_ShiftDownClampedAtMax(t *testing.T) {
	m := selModel()
	m.selEndLine = 2
	nm := extendSel(m, tea.KeyShiftDown)
	lines := nm.transcriptPlainLines()
	maxLine := len(lines) - 1
	if nm.selEndLine > maxLine {
		t.Errorf("shift+down at last line should clamp, got %d (max %d)", nm.selEndLine, maxLine)
	}
}

func TestExtendTranscriptSel_ShiftLeftDecrementsCol(t *testing.T) {
	nm := extendSel(selModel(), tea.KeyShiftLeft) // endCol=4 → 3
	if nm.selEndCol != 3 {
		t.Errorf("shift+left: expected selEndCol=3, got %d", nm.selEndCol)
	}
}

func TestExtendTranscriptSel_ShiftLeftAtLineStartWrapsUp(t *testing.T) {
	m := selModel()
	m.selEndLine = 1
	m.selEndCol = 0
	nm := extendSel(m, tea.KeyShiftLeft)
	if nm.selEndLine != 0 {
		t.Errorf("shift+left at col 0 should wrap to previous line, got line %d", nm.selEndLine)
	}
	if nm.selEndCol <= 0 {
		t.Errorf("shift+left wrap: expected col > 0 (end of prev line), got %d", nm.selEndCol)
	}
}

func TestExtendTranscriptSel_ShiftRightIncrementsCol(t *testing.T) {
	nm := extendSel(selModel(), tea.KeyShiftRight) // endCol=4 → 5
	if nm.selEndCol != 5 {
		t.Errorf("shift+right: expected selEndCol=5, got %d", nm.selEndCol)
	}
}

func TestExtendTranscriptSel_ShiftRightAtLineEndWrapsDown(t *testing.T) {
	m := selModel()
	lines := m.transcriptPlainLines()
	m.selEndLine = 1
	m.selEndCol = len([]rune(lines[1])) // at end of "line one"
	nm := extendSel(m, tea.KeyShiftRight)
	if nm.selEndLine != 2 {
		t.Errorf("shift+right at line end should wrap to next line, got line %d", nm.selEndLine)
	}
	if nm.selEndCol != 0 {
		t.Errorf("shift+right wrap: expected col=0, got %d", nm.selEndCol)
	}
}

func TestExtendTranscriptSel_NoEndUsesAnchor(t *testing.T) {
	// selEndLine=-1 means no drag has happened yet; end should start from anchor.
	m := selModel()
	m.selEndLine = -1
	m.selEndCol = 0
	nm := extendSel(m, tea.KeyShiftDown)
	// anchor was at line 1; end should move from 1 to 2.
	if nm.selEndLine != 2 {
		t.Errorf("selEndLine=-1: expected selEndLine=2 after shift+down, got %d", nm.selEndLine)
	}
}

func TestExtendTranscriptSel_BareDownIsNoop(t *testing.T) {
	m := selModel()
	nm := extendSel(m, tea.KeyDown) // unrecognised key — no change
	if nm.selEndLine != m.selEndLine {
		t.Errorf("bare down should not move transcript selection, got line %d", nm.selEndLine)
	}
}
