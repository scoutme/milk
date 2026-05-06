package claude

import (
	"io"
	"strings"
	"sync"
	"unicode"

	"github.com/vito/midterm"
)

// claudeSpinners are the Unicode chars Claude's Ink TUI uses for thinking spinners.
var claudeSpinners = map[rune]bool{
	// Unicode spinners
	'✻': true, '✽': true, '✶': true, '✢': true, '·': true,
	'⠋': true, '⠙': true, '⠹': true, '⠸': true, '⠼': true,
	'⠴': true, '⠦': true, '⠧': true, '⠇': true, '⠏': true,
	// ASCII spinner / list marker
	'*': true,
	// Claude logo block-drawing chars
	'▐': true, '▝': true, '▘': true,
	// Claude's input prompt
	'❯': true,
}

// claudeChrome patterns that appear only in Claude's TUI chrome, never in output.
var claudeChrome = []string{
	"Infusing…", "Infusing...",
	"Cogitated for",
	"Tip: Run /", "Tip: Hit ",
	"Reading ", "Read ",
	"(ctrl+o to expand)", "ctrl+o to expand",
	"esc to interrupt",
	"In PROJECT_DOCS",
	"Claude Code v",
	"API Usage Billing",
	"· /effort", "· /low", "· /medium", "· /high",
}

// isUIChrome returns true if a line is TUI chrome that should be suppressed.
func isUIChrome(line string) bool {
	if line == "" {
		return false
	}
	// Separator lines: all box-drawing or dash characters
	if strings.IndexFunc(line, func(r rune) bool {
		return r != '─' && r != '━' && r != '-' && r != ' '
	}) == -1 {
		return true
	}
	first := []rune(line)[0]
	if claudeSpinners[first] {
		return true
	}
	// Tip / artifact marker (may be indented with spaces)
	if strings.ContainsRune(line, '⎿') {
		return true
	}
	// Known chrome substrings
	for _, pat := range claudeChrome {
		if strings.Contains(line, pat) {
			return true
		}
	}
	return false
}

// cleanLine strips known TUI prefixes from content lines, e.g. "● response" → "response".
func cleanLine(line string) string {
	// Response bullet
	line = strings.TrimPrefix(line, "● ")
	return line
}

// vtFilter sits between a PTY master and the user's terminal. It feeds raw
// bytes through a VT100 emulator, extracts clean text via the scrollback hook
// and periodic screen diffs, and writes only plain output lines to out.
// The Ink TUI chrome (cursor positioning, alternate screen, UI widgets) is
// absorbed by the emulator and never forwarded.
type vtFilter struct {
	term    *midterm.Terminal
	out     io.Writer
	mu      sync.Mutex
	lastRow []string // snapshot of visible screen rows after last flush
}

func newVTFilter(cols, rows int, out io.Writer) *vtFilter {
	if cols <= 0 {
		cols = 220
	}
	if rows <= 0 {
		rows = 50
	}
	f := &vtFilter{
		term: midterm.NewTerminal(rows, cols),
		out:  out,
	}
	f.lastRow = make([]string, rows)
	// Lines that scroll off the top are finished output — emit them immediately.
	f.term.OnScrollback(func(line midterm.Line) {
		text := lineText(line.Content)
		if text == "" || isUIChrome(text) {
			return
		}
		text = cleanLine(text)
		f.mu.Lock()
		defer f.mu.Unlock()
		io.WriteString(f.out, text+"\r\n") //nolint:errcheck
	})
	return f
}

// Write feeds raw PTY bytes into the VT emulator and flushes any new visible
// lines to out. Implements io.Writer so it can replace io.Copy(os.Stdout, ptmx).
func (f *vtFilter) Write(p []byte) (int, error) {
	n, err := f.term.Write(p)
	f.flush()
	return n, err
}

// flush diffs the current visible screen against the last snapshot and emits
// lines that changed. Only lines above the cursor are considered settled.
func (f *vtFilter) flush() {
	f.mu.Lock()
	defer f.mu.Unlock()

	cursorY := f.term.Cursor.Y
	height := f.term.Height

	for row := 0; row < height && row < cursorY; row++ {
		if row >= len(f.term.Content) {
			break
		}
		text := lineText(f.term.Content[row])
		if row < len(f.lastRow) && f.lastRow[row] == text {
			continue
		}
		if row >= len(f.lastRow) {
			f.lastRow = append(f.lastRow, make([]string, row+1-len(f.lastRow))...)
		}
		f.lastRow[row] = text
		if text == "" || isUIChrome(text) {
			continue
		}
		io.WriteString(f.out, cleanLine(text)+"\r\n") //nolint:errcheck
	}
}

// lineText converts a row of runes to a trimmed string, stripping trailing spaces.
func lineText(content []rune) string {
	s := strings.TrimRightFunc(string(content), unicode.IsSpace)
	return s
}

// Lines returns all non-empty lines currently visible on screen.
// Used by the approval detector to scan for prompts.
func (f *vtFilter) Lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for row := range f.term.Content {
		if text := lineText(f.term.Content[row]); text != "" {
			out = append(out, text)
		}
	}
	return out
}
