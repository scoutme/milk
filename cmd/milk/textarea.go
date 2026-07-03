package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	rw "github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// undoDebugLog is a debug-only file writer for undo/redo diagnostics.
// Set undoDebugLogPath to a non-empty path to enable; leave empty to disable.
const undoDebugLogPath = ""

var undoDebugFile *os.File

func undoDebugLog(format string, args ...any) {
	if undoDebugLogPath == "" {
		return
	}
	if undoDebugFile == nil {
		var err error
		undoDebugFile, err = os.OpenFile(undoDebugLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return
		}
	}
	fmt.Fprintf(undoDebugFile, "[%s] "+format+"\n", append([]any{time.Now().Format("15:04:05.000")}, args...)...)
}

func buildTextarea() textarea.Model {
	ta := textarea.New()
	ta.Placeholder = ""
	ta.ShowLineNumbers = false
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.BlurredStyle.Base = lipgloss.NewStyle()
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Prompt = lipgloss.NewStyle()
	ta.BlurredStyle.Prompt = lipgloss.NewStyle()
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.SetHeight(1)

	km := textarea.DefaultKeyMap
	km.InsertNewline.SetKeys("shift+enter", "alt+enter", "ctrl+n")
	km.WordBackward.SetKeys("alt+left", "ctrl+left")
	km.WordForward.SetKeys("alt+right", "ctrl+right")
	ta.KeyMap = km

	ta.Focus() //nolint:errcheck
	return ta
}

// taCursorOffset returns the global rune offset of the textarea cursor within ta.Value().
func (m *model) taCursorOffset() int {
	lines := strings.Split(m.ta.Value(), "\n")
	row := m.ta.Line()
	li := m.ta.LineInfo()
	col := li.StartColumn + li.ColumnOffset // rune offset within logical line
	offset := 0
	for i := 0; i < row && i < len(lines); i++ {
		offset += len([]rune(lines[i])) + 1 // +1 for the '\n'
	}
	return offset + col
}

// taClearSel clears the keyboard selection in the input area.
func (m *model) taClearSel() {
	m.taSelAnchor = -1
	m.taSelEnd = -1
}

// taDeleteSelection removes the selected rune range from the textarea, placing
// the cursor at the start of the deleted range. Does nothing if no selection.
func (m model) taDeleteSelection() model {
	if m.taSelText() == "" {
		return m
	}
	m.undoPush(false)
	runes := []rune(m.ta.Value())
	lo, hi := m.taSelAnchor, m.taSelEnd
	if lo > hi {
		lo, hi = hi, lo
	}
	if lo < 0 {
		lo = 0
	}
	if hi > len(runes) {
		hi = len(runes)
	}
	// SetValue(prefix) positions cursor at end of prefix (= lo), then InsertString
	// appends the suffix without moving the cursor.
	m.ta.SetValue(string(runes[:lo]))
	m.ta.InsertString(string(runes[hi:]))
	m.taClearSel()
	return m
}

// taSelText returns the plain text of the current keyboard selection, or "".
func (m *model) taSelText() string {
	if m.taSelAnchor < 0 || m.taSelEnd < 0 || m.taSelAnchor == m.taSelEnd {
		return ""
	}
	runes := []rune(m.ta.Value())
	lo, hi := m.taSelAnchor, m.taSelEnd
	if lo > hi {
		lo, hi = hi, lo
	}
	if lo < 0 {
		lo = 0
	}
	if hi > len(runes) {
		hi = len(runes)
	}
	if lo > hi {
		return ""
	}
	return string(runes[lo:hi])
}

// taRows returns the number of display rows the textarea content needs.
// It replicates the exact word-wrap logic from bubbles/textarea wrap() so the
// count matches what the textarea will render — using uniseg.StringWidth for
// display widths and word-boundary splitting, same as the upstream code.
func (m *model) taRows() int {
	w := m.ta.Width()
	if w <= 0 {
		w = 1
	}
	lines := strings.Split(m.ta.Value(), "\n")
	total := 0
	for _, line := range lines {
		total += taWrapRows([]rune(line), w)
	}
	if total < 1 {
		return 1
	}
	return total
}

// taWrapRows counts soft-wrapped rows for a single logical line, mirroring
// the wrap() function in charmbracelet/bubbles/textarea/textarea.go.
func taWrapRows(runes []rune, width int) int {
	var (
		lineW  int    // display width of current soft row
		word   []rune // current word being accumulated
		spaces int    // pending spaces after last word
		rows   = 1
	)
	flush := func() {
		pending := uniseg.StringWidth(string(word)) + spaces
		if lineW+pending > width {
			rows++
			lineW = uniseg.StringWidth(string(word)) + spaces
		} else {
			lineW += pending
		}
		word = nil
		spaces = 0
	}
	for _, r := range runes {
		if r == ' ' || r == '\t' {
			if len(word) > 0 || spaces > 0 {
				spaces++
			}
		} else {
			if spaces > 0 {
				flush()
			}
			word = append(word, r)
			// hard-wrap a single word that fills the entire width
			lastW := rw.RuneWidth(word[len(word)-1])
			if uniseg.StringWidth(string(word))+lastW > width {
				if lineW > 0 {
					rows++
					lineW = 0
				}
				lineW += uniseg.StringWidth(string(word))
				word = nil
			}
		}
	}
	// flush remaining word+spaces
	pending := uniseg.StringWidth(string(word)) + spaces
	if lineW+pending >= width {
		rows++
	}
	return rows
}

func (m *model) undoPush(coalesce bool) {
	val := m.ta.Value()
	now := time.Now()
	if coalesce && len(m.undoStack) > 0 && val == m.lastUndoValue &&
		now.Sub(m.lastUndoTime) < undoCoalesceWindow {
		undoDebugLog("undoPush COALESCE-SKIP coalesce=%v val=%q lastUndoValue=%q stack=%d", coalesce, val, m.lastUndoValue, len(m.undoStack))
		return
	}
	if val == m.lastUndoValue {
		undoDebugLog("undoPush SAME-VALUE-SKIP val=%q stack=%d", val, len(m.undoStack))
		return
	}
	entry := undoEntry{value: val, cursor: m.taCursorOffset()}
	m.undoStack = append(m.undoStack, entry)
	if len(m.undoStack) > undoMaxDepth {
		m.undoStack = m.undoStack[1:]
	}
	m.redoStack = m.redoStack[:0]
	m.lastUndoValue = val
	m.lastUndoTime = now
	undoDebugLog("undoPush PUSHED coalesce=%v val=%q cursor=%d stack=%d", coalesce, val, entry.cursor, len(m.undoStack))
}

// undoApply pops from src, pushes current state to dst, restores the entry.
// Cursor is placed at the end of the restored value (textarea default after SetValue).
func (m *model) undoApply(src, dst *[]undoEntry) bool {
	if len(*src) == 0 {
		undoDebugLog("undoApply EMPTY-STACK src=%d dst=%d", len(*src), len(*dst))
		return false
	}
	cur := undoEntry{value: m.ta.Value(), cursor: m.taCursorOffset()}
	entry := (*src)[len(*src)-1]
	*src = (*src)[:len(*src)-1]
	*dst = append(*dst, cur)
	undoDebugLog("undoApply RESTORE cur=%q entry=%q src=%d dst=%d lastUndoValue=%q", cur.value, entry.value, len(*src), len(*dst), m.lastUndoValue)
	m.ta.SetValue(entry.value)
	m.ta.CursorEnd()
	m.lastUndoValue = entry.value
	m.taClearSel()
	return true
}

func (m *model) updateTA(msg tea.Msg) tea.Cmd {
	// Pre-expand the textarea by one row before the update so that
	// repositionView() inside ta.Update never scrolls when a new wrap row
	// appears. After the update, setViewportContent / syncLayout trims it back
	// to the exact row count via taRows().
	if km, ok := msg.(tea.KeyMsg); ok {
		undoDebugLog("updateTA KEY=%q val_before=%q undoStack=%d lastUndoValue=%q", km.String(), m.ta.Value(), len(m.undoStack), m.lastUndoValue)
	}
	m.ta.SetHeight(m.ta.Height() + 1)
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return cmd
}
