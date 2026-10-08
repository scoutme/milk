package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/aymanbagabas/go-osc52/v2"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/textbudget"
)

// colorizeLineThresh is the number of new lines that must accumulate before
// a mid-stream re-colorization is triggered. Keeps chroma/glamour from running
// on every individual streamed token. With per-turn caching, full re-colorize
// is O(last turn) not O(transcript), so a higher threshold is affordable.
const colorizeLineThresh = 32

// appendTranscript adds text to both transcript variants and refreshes the viewport.
// Sticky-bottom: only auto-scrolls when already at the bottom.
func (m *model) appendTranscript(text string) {
	// If regular content follows thinking, close the open dim escape (opened once
	// in appendThinking, not per-chunk, so it must be closed explicitly here) and
	// ensure both variants end with a newline so the final content starts on its
	// own line rather than the last thinking row.
	if m.thinkingActiveInTurn {
		if isTTY {
			m.transcript.WriteString(ansiReset)
		}
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

// appendTranscriptStreamed is appendTranscript's throttled counterpart for
// the actual per-token streaming path (chunkMsg/prefixChunkMsg) — see
// viewportDirty's doc comment on model for why every other call site (this
// function included, for one-off system/status messages) keeps rebuilding
// the viewport immediately instead.
func (m *model) appendTranscriptStreamed(text string) {
	if m.thinkingActiveInTurn {
		if isTTY {
			m.transcript.WriteString(ansiReset)
		}
		if s := m.transcript.String(); len(s) > 0 && s[len(s)-1] != '\n' {
			m.transcript.WriteByte('\n')
		}
		if s := m.transcriptNoThink.String(); len(s) > 0 && s[len(s)-1] != '\n' {
			m.transcriptNoThink.WriteByte('\n')
		}
	}
	m.transcript.WriteString(text)
	m.transcriptNoThink.WriteString(text)
	m.thinkingActiveInTurn = false
	m.syncViewportThrottled()
}

// retractFallbackNote is appended in place of an exact retraction when the
// retracted text can't be found verbatim in the transcript (see
// retractStreamed). Honest about what happened — a tool call was NOT executed
// and the clean summary replaced it — instead of silently leaving raw markup
// as the transcript's final state.
const retractFallbackNote = "[milk] an unparsed tool call was not executed — the summary above replaces it"

// retractStreamed repairs the transcript after the agent retracted response
// from in favor of to (a turn that ended on unparsed tool-call markup, whose
// raw text already streamed out before the source-side strip replaced it —
// see local.stripUnparsedToolMarkup and Agent.WithOnRetract). The LAST
// occurrence of from is replaced with to in both transcript variants — exact
// verbatim surgery, no fuzzy matching, so ordinary text is never touched.
// When from isn't found (the streamed form diverged from the final response —
// interleaved status lines or thinking-block breaks), the honest replacement
// is appended instead: to followed by a yellow note, so raw markup is never
// the transcript's final state.
func (m *model) retractStreamed(from, to string) {
	for _, b := range []*strings.Builder{m.transcript, m.transcriptNoThink} {
		s := b.String()
		idx := -1
		if from != "" {
			idx = strings.LastIndex(s, from)
		}
		b.Reset()
		switch {
		case idx >= 0:
			b.WriteString(s[:idx] + to + s[idx+len(from):])
		default:
			b.WriteString(s)
			if s != "" && !strings.HasSuffix(s, "\n") {
				b.WriteString("\n")
			}
			b.WriteString(to + "\n" + yellow(retractFallbackNote) + "\n")
		}
	}
	// Content changed mid-transcript, so the offset-keyed colorize caches
	// can't be trusted any more — force a full re-colorize.
	m.colorizeForce = true
	if m.ready {
		atBottom := m.vp.AtBottom()
		m.setViewportContent()
		if atBottom {
			m.vp.GotoBottom()
		}
	}
}

// appendThinkingStreamed adds thinking/reasoning text to the full transcript
// (dim-styled) and a single "[thinking…]" placeholder to transcriptNoThink
// (only on the first chunk of a new thinking block, to avoid repeated
// placeholders per token). The only source of thinking text is the per-token
// reasoning stream (thinkChunkMsg), so this is throttled the same way
// appendTranscriptStreamed is — see its doc comment on model.viewportDirty.
//
// The dim escape is opened once per block (not re-wrapped per chunk) and closed
// in appendTranscript when regular content follows: back-to-back self-terminated
// \x1b[2m...\x1b[0m pairs from per-chunk wrapping could otherwise land right at
// a line-wrap boundary and leave a stray, orphaned "m" terminator visible.
func (m *model) appendThinkingStreamed(text string) {
	if !m.thinkingActiveInTurn {
		if isTTY {
			m.transcript.WriteString(ansiDim)
		}
		m.transcriptNoThink.WriteString(dim("[thinking… Ctrl+T to show]"))
		m.thinkingActiveInTurn = true
	}
	m.transcript.WriteString(text)
	m.syncViewportThrottled()
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
			// bleed into the next chunk of streamed content. The cache boundary
			// itself never lands mid-escape (see the full-recolor branch below),
			// so it always ends either with ansiReset or with plain text.
			base := m.colorizeCached
			if !strings.HasSuffix(base, ansiReset) {
				base += ansiReset
			}
			return m.applySelectionHighlight(base + newText)
		}
		return m.applySelectionHighlight(m.colorizeCached)
	}

	// Full re-colorize with per-turn caching: colorize on the raw (unwrapped)
	// transcript so that multi-line constructs like tables are detected on intact
	// rows, then word-wrap the colorized output. Completed turns (segments
	// ending with "\n\n") never change, so their colorized output is cached in
	// turnColorCache/turnRawCache. Only the last (incomplete) turn is re-colorized
	// on each cache miss, making this O(last turn) instead of O(full transcript).
	//
	// Content streams into the transcript byte-by-byte (internal/tags.TagWriter/
	// PerceptWriter must scan one byte at a time to detect tags across chunk
	// boundaries), so raw can genuinely end with an incomplete ANSI escape — a
	// lone ESC, or "ESC[" with no terminating 'm' yet — even for text a caller
	// wrote as one complete string. Clip the cached boundary to just before any
	// such dangling escape so colorizeCached/colorizeTransLen never commit to a
	// cut point mid-sequence; the incomplete tail stays in the "new" region.
	effLen := txLen
	if idx := danglingEscapeStart(raw); idx >= 0 {
		effLen = idx
	}
	m.colorizeForce = false
	m.colorizeLinesSeen = 0

	// Split into turns and colorize with per-turn caching.
	segments := splitTurns(raw[:effLen])
	var colorized strings.Builder
	for i, seg := range segments {
		if i < len(segments)-1 {
			// Completed turn (followed by another segment): check per-turn cache.
			if i < len(m.turnColorCache) && i < len(m.turnRawCache) && m.turnRawCache[i] == seg {
				colorized.WriteString(m.turnColorCache[i])
			} else {
				result := colorizeSingle(seg, m.colorizeMode)
				if i < len(m.turnColorCache) {
					m.turnColorCache[i] = result
					m.turnRawCache[i] = seg
				} else {
					m.turnColorCache = append(m.turnColorCache, result)
					m.turnRawCache = append(m.turnRawCache, seg)
				}
				colorized.WriteString(result)
			}
		} else {
			// Last (incomplete) turn: always re-colorize.
			colorized.WriteString(colorizeSingle(seg, m.colorizeMode))
		}
	}
	// Trim completed turns from cache if transcript shrank (shouldn't happen
	// in normal operation, but defensive).
	if len(m.turnColorCache) > len(segments)-1 && len(segments) > 0 {
		m.turnColorCache = m.turnColorCache[:len(segments)-1]
		m.turnRawCache = m.turnRawCache[:len(segments)-1]
	}

	wrapped := ansi.Wrap(expandTabsForWrap(colorized.String()), vw, "")

	// Update cache.
	m.colorizeCached = wrapped
	m.colorizeTransLen = effLen
	m.colorizeVPWidth = vw

	if effLen == txLen {
		return m.applySelectionHighlight(wrapped)
	}
	newText := ansi.Wrap(expandTabsForWrap(raw[effLen:]), vw, "")
	base := wrapped
	if !strings.HasSuffix(base, ansiReset) {
		base += ansiReset
	}
	return m.applySelectionHighlight(base + newText)
}

// danglingEscapeStart returns the byte offset of an incomplete trailing ANSI
// escape sequence at the end of s — either a lone ESC with nothing after it
// yet, or an "ESC[" run with no terminating 'm' yet — or -1 if s ends cleanly.
func danglingEscapeStart(s string) int {
	idx := strings.LastIndexByte(s, 0x1B)
	if idx == -1 {
		return -1
	}
	if idx+1 >= len(s) || s[idx+1] != '[' {
		return idx
	}
	if strings.IndexByte(s[idx:], 'm') == -1 {
		return idx
	}
	return -1
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

// dragResetCmd returns a command that fires the drag-timeout message after
// 500ms.  Scheduled on mouse press, rescheduled on every motion event, and
// cancelled on release.  If the release is dropped by the terminal (pointer
// outside viewport bounds), the timeout resets the mouse-tracking mode to
// basic (1000) so wheel-scroll keeps working — but only when the pointer
// actually left the drag area (dragSawOutside); a mid-drag pause keeps mode
// 1002 and reschedules the timer instead.
//
// gen is the scheduling-generation counter; the returned message carries it so
// the handler can discard stale timeouts from earlier scheduling calls.
func dragResetCmd(gen uint64) tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return dragResetMsg{gen: gen} })
}

// seedTranscriptFromHistory pre-populates an empty transcript with the
// retained conversation of a resumed session, so reopening milk shows where
// the last session left off instead of the welcome screen. Windowed like the
// ACP history replay (first replayHeadTurns + last replayTailTurns turns, a
// gap marker between, each message capped at replayMaxMessageChars) and
// rendered with the live transcript's own styling. Tool turns and reasoning
// are not replayed. A fresh session (no history) leaves the transcript empty.
// Only startup needs this: /new, /clear and /drop always land on a fresh
// session and leave the visible transcript as it was.
func (m *model) seedTranscriptFromHistory() {
	if m.st == nil || m.st.sess == nil || m.transcript.Len() > 0 {
		return
	}
	hist := m.st.sess.History
	if len(hist) == 0 {
		return
	}
	var b strings.Builder
	id := m.st.sess.ID
	if len(id) > 8 {
		id = id[:8]
	}
	fmt.Fprintf(&b, "%s resumed session %s (%d turns) — /export prints the full transcript\n\n", milkTag(), id, len(hist))

	n := len(hist)
	writeRange := func(from, to int) {
		for i := from; i < to; i++ {
			writeSeedTurn(&b, m.st, hist[i])
		}
	}
	if n > replayHeadTurns+replayTailTurns {
		writeRange(0, replayHeadTurns)
		b.WriteString(dim(fmt.Sprintf("[… %d earlier turns omitted — /export prints the full transcript …]", n-replayHeadTurns-replayTailTurns)) + "\n\n")
		writeRange(n-replayTailTurns, n)
	} else {
		writeRange(0, n)
	}
	m.transcript.WriteString(b.String())
	m.transcriptNoThink.WriteString(b.String())
}

func writeSeedTurn(b *strings.Builder, st *interactiveState, t session.Turn) {
	text := textbudget.SummarizeLong(t.Content, replayMaxMessageChars)
	switch t.Role {
	case session.RoleUser:
		if text == "" {
			return
		}
		b.WriteString(promptLabel(st) + colorizeTokens(text) + "\n")
	case session.RoleAssistant:
		if text == "" {
			return
		}
		name := t.AgentName
		paint := green
		if t.Agent == session.AgentEscalation {
			paint = blue
		}
		if name == "" {
			name = string(t.Agent)
		}
		b.WriteString(bold(paint(name+":")) + " " + text + "\n\n")
	}
}
