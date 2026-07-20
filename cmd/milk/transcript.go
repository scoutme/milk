package main

import (
	"os"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/aymanbagabas/go-osc52/v2"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// colorizeLineThresh is the number of new lines that must accumulate before
// a mid-stream re-colorization is triggered. Keeps chroma/glamour from running
// on every individual streamed token.
const colorizeLineThresh = 8

// appendTranscript adds text to both transcript variants and refreshes the viewport.
// Sticky-bottom: only auto-scrolls when already at the bottom.
func (m *model) appendTranscript(text string) {
	// If regular content follows thinking, ensure both variants end with a newline
	// so the final content starts on its own line rather than the last thinking row.
	if m.thinkingActiveInTurn {
		if s := m.transcript.String(); len(s) > 0 && s[len(s)-1] != '\n' {
			m.transcript.WriteByte('\n')
		}
		if s := m.transcriptNoThink.String(); len(s) > 0 && s[len(s)-1] != '\n' {
			m.transcriptNoThink.WriteByte('\n')
		}
	}
	m.transcript.WriteString(text)
	m.transcriptNoThink.WriteString(text)
	// If regular content arrives after thinking, mark that the think block ended
	// (the placeholder was already written when the block started).
	m.thinkingActiveInTurn = false
	if m.ready {
		atBottom := m.vp.AtBottom()
		m.setViewportContent()
		if atBottom {
			m.vp.GotoBottom()
		}
	}
}

// appendThinking adds thinking/reasoning text to the full transcript (dim-styled)
// and a single "[thinking…]" placeholder to transcriptNoThink (only on the first
// chunk of a new thinking block, to avoid repeated placeholders per token).
func (m *model) appendThinking(text string) {
	m.transcript.WriteString(dim(text))
	if !m.thinkingActiveInTurn {
		m.transcriptNoThink.WriteString(dim("[thinking… Ctrl+T to show]"))
		m.thinkingActiveInTurn = true
	}
	if m.ready {
		atBottom := m.vp.AtBottom()
		m.setViewportContent()
		if atBottom {
			m.vp.GotoBottom()
		}
	}
}

// activeTranscript returns the transcript variant to render based on showThinking.
func (m *model) activeTranscript() *strings.Builder {
	if m.showThinking {
		return m.transcript
	}
	return m.transcriptNoThink
}

// wrappedTranscript returns the transcript (or welcome screen) word-wrapped to
// the viewport content width. When a selection range is active, the selected
// text region is highlighted with an inverted background, respecting column
// boundaries on the first and last lines.
func (m *model) wrappedTranscript() string {
	tx := m.activeTranscript()
	if tx.Len() == 0 {
		return m.applySelectionHighlight(m.welcomeScreen())
	}
	vw := m.vpWidth()
	raw := tx.String()
	if vw <= 0 {
		return raw
	}
	if m.colorizeMode == ColorizeOff {
		return m.applySelectionHighlight(ansi.Wrap(expandTabsForWrap(raw), vw, ""))
	}

	txLen := tx.Len()
	// Check cache validity: width change requires re-wrap; YOffset is not a cache
	// key because colorization covers the full transcript regardless of scroll
	// position, and GotoBottom legitimately advances YOffset after every append.
	vpChanged := vw != m.colorizeVPWidth
	txGrew := txLen - m.colorizeTransLen

	// Count new lines since last re-colorize to decide if threshold is met.
	newLines := 0
	if txGrew > 0 {
		newLines = strings.Count(raw[m.colorizeTransLen:], "\n")
		m.colorizeLinesSeen += newLines
	}

	if !m.colorizeForce && !vpChanged && m.colorizeCached != "" && m.colorizeLinesSeen < colorizeLineThresh {
		// Return cached result — append plain-wrapped new text as a fast suffix
		// so the user sees new content immediately even without re-colorizing.
		if txGrew > 0 {
			newText := ansi.Wrap(expandTabsForWrap(raw[m.colorizeTransLen:]), vw, "")
			// Close any open ANSI sequence from the cache before appending raw
			// text, so a trailing dim/color from e.g. a tool hint line doesn't
			// bleed into the next chunk of streamed content.
			base := m.colorizeCached
			if !strings.HasSuffix(base, ansiReset) {
				base += ansiReset
			}
			return m.applySelectionHighlight(base + newText)
		}
		return m.applySelectionHighlight(m.colorizeCached)
	}

	// Full re-colorize: colorize on the raw (unwrapped) transcript so that
	// multi-line constructs like tables are detected on intact rows, then
	// word-wrap the colorized output. Wrapping before colorization would break
	// long table rows mid-cell, preventing table detection entirely.
	m.colorizeForce = false
	m.colorizeLinesSeen = 0
	colorized := colorizeTranscriptWrapped(raw, m.colorizeMode)
	wrapped := ansi.Wrap(expandTabsForWrap(colorized), vw, "")

	// Update cache.
	m.colorizeCached = wrapped
	m.colorizeTransLen = txLen
	m.colorizeVPWidth = vw

	return m.applySelectionHighlight(wrapped)
}

// transcriptPlainLines returns the transcript lines stripped of ANSI, using the
// cached colorized content when available so coordinates match the viewport exactly.
// Falls back to a fresh render when the cache is empty (e.g. ColorizeOff mode or
// before first paint).
func (m *model) transcriptPlainLines() []string {
	var wrapped string
	if m.colorizeCached != "" {
		// Fast path: cache already holds the wrapped+colorized content.
		wrapped = m.colorizeCached
	} else {
		// Slow path: render fresh so selection coordinates are still correct.
		vw := m.vpWidth()
		if m.transcript.Len() == 0 {
			wrapped = m.welcomeScreen()
		} else {
			raw := m.activeTranscript().String()
			if vw <= 0 {
				wrapped = raw
			} else {
				colorized := colorizeTranscriptWrapped(raw, m.colorizeMode)
				wrapped = ansi.Wrap(expandTabsForWrap(colorized), vw, "")
			}
		}
	}
	lines := strings.Split(wrapped, "\n")
	for i, l := range lines {
		lines[i] = ansi.Strip(l)
	}
	return lines
}

// selectionText extracts the plain text between the selection anchor and end,
// respecting column boundaries on the first and last lines. It uses
// transcriptPlainLines so that coordinates match the rendered viewport exactly,
// avoiding drift caused by table padding or markdown colorization changing line
// lengths relative to the raw transcript.
func (m *model) selectionText() string {
	lines := m.transcriptPlainLines()
	loLine, loCol := m.selAnchorLine, m.selAnchorCol
	hiLine, hiCol := m.selEndLine, m.selEndCol
	if hiLine < loLine || (hiLine == loLine && hiCol < loCol) {
		loLine, loCol, hiLine, hiCol = hiLine, hiCol, loLine, loCol
	}
	if loLine < 0 {
		loLine = 0
	}
	if hiLine >= len(lines) {
		hiLine = len(lines) - 1
	}
	var sb strings.Builder
	for i := loLine; i <= hiLine; i++ {
		plain := []rune(lines[i]) // already stripped by transcriptPlainLines
		start, end := 0, len(plain)
		if i == loLine {
			if loCol < len(plain) {
				start = loCol
			} else {
				start = len(plain)
			}
		}
		if i == hiLine {
			if hiCol < len(plain) {
				end = hiCol
			}
		}
		if start > end {
			start = end
		}
		sb.WriteString(string(plain[start:end]))
		if i < hiLine {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// applySelectionHighlight applies the selection background highlight to the
// given content string. Returns content unchanged if no selection is active.
func (m *model) applySelectionHighlight(content string) string {
	if m.selAnchorLine < 0 || m.selEndLine < 0 {
		return content
	}
	loLine, loCol := m.selAnchorLine, m.selAnchorCol
	hiLine, hiCol := m.selEndLine, m.selEndCol
	if hiLine < loLine || (hiLine == loLine && hiCol < loCol) {
		loLine, loCol, hiLine, hiCol = hiLine, hiCol, loLine, loCol
	}
	lines := strings.Split(content, "\n")
	selStyle := lipgloss.NewStyle().Reverse(true)
	for i := range lines {
		if i < loLine || i > hiLine {
			continue
		}
		plain := []rune(ansi.Strip(lines[i]))
		start, end := 0, len(plain)
		if i == loLine {
			if loCol < len(plain) {
				start = loCol
			} else {
				start = len(plain)
			}
		}
		if i == hiLine {
			if hiCol < len(plain) {
				end = hiCol
			}
		}
		if start > end {
			start = end
		}
		before := string(plain[:start])
		sel := selStyle.Render(string(plain[start:end]))
		after := string(plain[end:])
		lines[i] = before + sel + after
	}
	return strings.Join(lines, "\n")
}

// clearSelection resets selection state.
func (m *model) clearSelection() {
	m.selAnchorLine = -1
	m.selAnchorCol = 0
	m.selEndLine = -1
	m.selEndCol = 0
	m.selDragging = false
	m.selText = ""
}

// copyToClipboard writes text to the system clipboard via atotto/clipboard (which
// handles WSL via clip.exe, Wayland via wl-copy, X11 via xclip/xsel) and also
// emits an OSC 52 sequence as a fallback for SSH or tmux environments.
func copyToClipboard(text string) {
	// Primary: OS clipboard (works on WSL, X11, Wayland, macOS).
	_ = clipboard.WriteAll(text)
	// Secondary: OSC 52 — picked up by terminals that support it (kitty, iTerm2, tmux).
	osc52.New(text).WriteTo(os.Stderr)
}

// copyFeedbackClearCmd returns a command that clears the copy feedback after 2s.
func copyFeedbackClearCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return copyFeedbackClearMsg{} })
}

// busyHintClearCmd returns a command that clears the busy hint after 3s.
func busyHintClearCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return busyHintClearMsg{} })
}

// quitPendingClearCmd clears the quit-confirmation state after 3s of inaction.
func quitPendingClearCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return quitPendingClearMsg{} })
}
