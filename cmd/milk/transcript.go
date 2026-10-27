package main

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/textbudget"
)

// colorizeLineThresh is the number of new lines that must accumulate before
// a mid-stream re-colorization is triggered. Keeps chroma/glamour from running
// on every individual streamed token. With per-turn caching, full re-colorize
// is O(last turn) not O(transcript), so a higher threshold is affordable.
const colorizeLineThresh = 32

// appendTranscript adds regular (non-reasoning) text to the transcript and
// refreshes the viewport. Sticky-bottom: only auto-scrolls when already at the bottom.
// A streaming thinking block ends at regular content: the block closes and
// folds (closeOpenThinkBlock), so the container boundary between "what the
// model thought" and "what the model said" is structural, not an escape pair.
func (m *model) appendTranscript(text string) {
	m.closeOpenThinkBlock()
	m.transcript.WriteString(text)
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
	m.closeOpenThinkBlock()
	m.transcript.WriteString(text)
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
// occurrence of from is replaced with to in the transcript buffer — exact
// verbatim surgery, no fuzzy matching, so ordinary text is never touched.
// Thinking blocks live out of band (ADR-0067), so their anchors shift with
// the replacement instead of the surgery having to hunt around interleaved
// reasoning text.
// When from isn't found (the streamed form diverged from the final response —
// interleaved status lines), the honest replacement is appended instead: to
// followed by a yellow note, so raw markup is never the transcript's final
// state.
func (m *model) retractStreamed(from, to string) {
	s := m.transcript.String()
	idx := -1
	if from != "" {
		idx = strings.LastIndex(s, from)
	}
	m.transcript.Reset()
	switch {
	case idx >= 0:
		m.transcript.WriteString(s[:idx] + to + s[idx+len(from):])
		m.adjustThinkAnchors(idx, len(from), len(to))
	default:
		// The fallback note replaces the transcript's tail state — end any
		// streaming block first so the note lands at the end, not above a
		// still-open container.
		m.closeOpenThinkBlock()
		m.transcript.WriteString(s)
		if s != "" && !strings.HasSuffix(s, "\n") {
			m.transcript.WriteString("\n")
		}
		m.transcript.WriteString(to + "\n" + yellow(retractFallbackNote) + "\n")
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

// appendThinkingStreamed adds thinking/reasoning text to the current think
// block (ADR-0067): the block's own buffer, not the transcript. The first
// chunk of a reasoning run opens a block anchored at the transcript's current
// end; the block renders as a fold container there — header always, body only
// while expanded. A new block keeps the session's current folding status
// (thinkFoldOpen — the status the previous block was left in): folded by
// default until the user opens one, and open-and-pinned when the user is
// watching. Throttled like appendTranscriptStreamed — see model.viewportDirty.
func (m *model) appendThinkingStreamed(text string) {
	for i := range m.thinkBlocks {
		if !m.thinkBlocks[i].closed {
			m.thinkBlocks[i].buf.WriteString(text)
			// The display changed while the transcript's length did not —
			// bump the invalidation generation so wrappedTranscript's memo
			// re-renders instead of serving the previous chunk (ADR-0088).
			// Deliberately not invalidateThinkDisplay: the inner cache's
			// cached-prefix + fresh-tail fast path is what makes streaming
			// cheap (its commit rule stops before an open container).
			m.thinkGen++
			m.syncViewportThrottled()
			return
		}
	}
	b := thinkBlock{
		buf:    &strings.Builder{},
		anchor: m.transcript.Len(),
		// Keep the folding status of the previous block (session state, not
		// the previous block's row — the turn-boundary tidy folds older blocks
		// without changing it). userSet matches so a block created in
		// watch-mode stays open past its close, like one opened directly.
		expanded: m.thinkFoldOpen,
		userSet:  m.thinkFoldOpen,
	}
	b.buf.WriteString(text)
	m.thinkBlocks = append(m.thinkBlocks, b)
	m.invalidateThinkDisplay()
	m.syncViewportThrottled()
}

// wrappedTranscript returns the transcript (or welcome screen) word-wrapped to
// the viewport content width. When a selection range is active, the selected
// text region is highlighted with an inverted background, respecting column
// boundaries on the first and last lines.
//
// Perf (ADR-0088): content-addressed memo around the whole render chain.
// The viewport rebuilds on every keystroke and this is the expensive half
// of it (wrap + per-row tint + selection highlight over the entire
// transcript), while cursor movement and typing change none of its inputs.
// The key covers everything that can change the output: the transcript's
// length (append-only), the width, the colorize mode, the invalidation flag
// invalidateThinkDisplay maintains (fold toggles, thinking-stream changes),
// a plain re-colorize request (colorizeForce — e.g. agentDoneMsg closing
// markdown fences), and the selection range.
func (m *model) wrappedTranscript() string {
	key := m.wrappedTranscriptKey()
	if m.wrapCacheValid && key == m.wrapCacheKey && !m.colorizeForce {
		return m.wrapCache
	}
	out := m.wrappedTranscriptUncached()
	// The recompute consumed the invalidation: whatever path ran above,
	// the display is fresh again.
	m.colorizeForce = false
	m.wrapCacheKey = m.wrappedTranscriptKey()
	m.wrapCache = out
	m.wrapCacheValid = true
	return out
}

// wrappedTranscriptKey snapshots the wrapped-transcript cache key. Taken
// after a recompute as well (the compute path consumes colorizeForce, so a
// pre-compute snapshot would poison the cache with a stale flag).
func (m *model) wrappedTranscriptKey() wrappedTxKey {
	return wrappedTxKey{
		txLen:         m.transcript.Len(),
		width:         m.vpWidth(),
		mode:          m.colorizeMode,
		thinkGen:      m.thinkGen,
		selAnchorLine: m.selAnchorLine,
		selAnchorCol:  m.selAnchorCol,
		selEndLine:    m.selEndLine,
		selEndCol:     m.selEndCol,
	}
}

// wrappedTxKey is the memo key for wrappedTranscript — see its doc comment
// for why every field is part of it.
type wrappedTxKey struct {
	txLen                       int
	width                       int
	mode                        ColorizeMode
	thinkGen                    int
	selAnchorLine, selAnchorCol int
	selEndLine, selEndCol       int
}

func (m *model) wrappedTranscriptUncached() string {
	raw, openAt := m.transcriptDisplayAt()
	if raw == "" {
		return m.applySelectionHighlight(m.welcomeScreen())
	}
	vw := m.vpWidth()
	if vw <= 0 {
		return raw
	}
	if m.colorizeMode == ColorizeOff {
		return m.applySelectionHighlight(tintFullRows(wrapRows(raw, vw), vw))
	}

	txLen := len(raw)
	// Check cache validity: width change requires re-wrap; YOffset is not a cache
	// key because colorization covers the full transcript regardless of scroll
	// position, and GotoBottom legitimately advances YOffset after every append.
	vpChanged := vw != m.colorizeVPWidth
	if vpChanged {
		// Tables are fitted to the viewport width, so the per-turn colorized
		// cache is only valid for the width it was built at.
		m.turnColorCache = nil
		m.turnRawCache = nil
	}
	txGrew := txLen - m.colorizeTransLen
	if txGrew < 0 {
		// The display shrank without an explicit invalidate (defensive — every
		// fold-state change goes through invalidateThinkDisplay): the cached
		// tail is no longer a prefix match, force a full rebuild.
		m.colorizeForce = true
	}

	// Count new lines since last re-colorize to decide if threshold is met.
	// Counted against the display's total length (colorizeSeenLen), not the
	// commit point: the commit is deliberately capped before an open think
	// block's container (see below), whose arriving lines still count toward
	// the threshold — but must not be re-counted on every render.
	if txLen > m.colorizeSeenLen {
		m.colorizeLinesSeen += strings.Count(raw[m.colorizeSeenLen:], "\n")
		m.colorizeSeenLen = txLen
	} else if txLen < m.colorizeSeenLen {
		m.colorizeSeenLen = txLen
	}

	if !m.colorizeForce && !vpChanged && m.colorizeCached != "" && m.colorizeLinesSeen < colorizeLineThresh && !m.colorizeMode.reflows() {
		// Return cached result — append plain-wrapped new text as a fast suffix
		// so the user sees new content immediately even without re-colorizing.
		// Row-preserving modes only (see ColorizeMode.reflows): a reflowing
		// render's cached prefix + raw suffix would have different rows than a
		// full rebuild of the same content, and that drift lands in every
		// screen-row→content-row mapping (clicks, selection).
		if txGrew > 0 {
			// The cache holds self-contained rows (wrapRows), so it never ends
			// with a style open and the fresh suffix can simply follow it.
			newText := wrapRows(raw[m.colorizeTransLen:], vw)
			return m.applySelectionHighlight(tintFullRows(m.colorizeCached+newText, vw))
		}
		return m.applySelectionHighlight(tintFullRows(m.colorizeCached, vw))
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
	// Never commit the cache into an open think block's container (its first
	// byte is openAt): the container is re-rendered from scratch on every
	// chunk — the header's live line count and the body's per-line styling
	// both rewrite bytes mid-display — so a cached prefix reaching into it
	// would go stale while the appended suffix started at shifted offsets:
	// garbled lines. The commit stops before the container; the streaming
	// tail is re-wrapped fresh on every render instead.
	if openAt < effLen {
		effLen = openAt
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
				result := colorizeSingleWidth(seg, m.colorizeMode, vw)
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
			colorized.WriteString(colorizeSingleWidth(seg, m.colorizeMode, vw))
		}
	}
	// Trim completed turns from cache if transcript shrank (shouldn't happen
	// in normal operation, but defensive).
	if len(m.turnColorCache) > len(segments)-1 && len(segments) > 0 {
		m.turnColorCache = m.turnColorCache[:len(segments)-1]
		m.turnRawCache = m.turnRawCache[:len(segments)-1]
	}

	wrapped := wrapRows(colorized.String(), vw)

	// Update cache. The cache stores the untinted wrap: the full-row pads
	// (tintFullRows — thinking surface and diff backgrounds) are width-
	// dependent and are applied to the returned render instead — the cached
	// bytes never change with the viewport width again after their own re-wrap,
	// so the seam between the cached prefix and the fresh suffix can't pick
	// up stale padding.
	m.colorizeCached = wrapped
	m.colorizeTransLen = effLen
	m.colorizeVPWidth = vw

	if effLen == txLen {
		return m.applySelectionHighlight(tintFullRows(wrapped, vw))
	}
	return m.applySelectionHighlight(tintFullRows(wrapped+wrapRows(raw[effLen:], vw), vw))
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
// transcriptPlainLines returns the transcript lines stripped of ANSI, using
// the same render the viewport shows so coordinates match it exactly —
// including an open think block's container, which the colorize cache
// deliberately never commits (a stale cache copy alone would omit the
// streaming block and skew click hit-testing and selection).
func (m *model) transcriptPlainLines() []string {
	wrapped := m.wrappedTranscript()
	lines := strings.Split(wrapped, "\n")
	for i, l := range lines {
		// Trailing spaces on full-row surfaces (thinking, diff) are the
		// render-time pad (tintFullRows), not content — trim them so the
		// plain coordinate space stays clean for hit-testing and copy.
		lines[i] = strings.TrimRight(ansi.Strip(l), " ")
	}
	return lines
}

// selectionText extracts the plain text between the selection anchor and end.
// It uses transcriptPlainLines so that coordinates match the rendered viewport
// exactly, avoiding drift caused by table padding or markdown colorization
// changing line lengths relative to the raw transcript.
func (m *model) selectionText() string {
	return selectedText(m.transcriptPlainLines(), m.selAnchorLine, m.selAnchorCol, m.selEndLine, m.selEndCol)
}

// applySelectionHighlight applies the selection highlight to the given
// content string. Returns content unchanged if no selection is active.
func (m *model) applySelectionHighlight(content string) string {
	if m.selAnchorLine < 0 || m.selEndLine < 0 {
		return content
	}
	lines := highlightSelection(strings.Split(content, "\n"), m.selAnchorLine, m.selAnchorCol, m.selEndLine, m.selEndCol)
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

// clipboardProgram is the running TUI program, set before Run. Nil outside
// the TUI (single-prompt mode, tests), where only the OS clipboard is used.
var clipboardProgram *tea.Program

// copyToClipboard writes text to the system clipboard via atotto/clipboard (which
// handles WSL via clip.exe, Wayland via wl-copy, X11 via xclip/xsel) and also
// asks the terminal to set it (OSC 52 through tea.SetClipboard) as a fallback
// for SSH or tmux environments.
var copyToClipboard = func(text string) {
	// Primary: OS clipboard (works on WSL, X11, Wayland, macOS).
	_ = clipboard.WriteAll(text)
	// Secondary: the terminal's clipboard, through the renderer. Send blocks
	// until the event loop reads it, and callers run inside Update, so it goes
	// out from a goroutine.
	if p := clipboardProgram; p != nil {
		go p.Send(tea.SetClipboard(text)())
	}
}

// copyOnSelectRelease copies text right after a mouse selection is released
// when copy_on_select is enabled; the caller clears its own selection state
// (so a later right-click still pastes) and returns the resulting command.
func (m *model) copyOnSelectRelease(text string) tea.Cmd {
	if text == "" || m.st == nil || !m.st.cfg.CopyOnSelectEnabled() {
		return nil
	}
	copyToClipboard(text)
	m.copyFeedback = fmt.Sprintf("copied %d chars", len([]rune(text)))
	return copyFeedbackClearCmd()
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
// Startup needs this; /new, /clear and /drop land on a fresh session and
// leave the visible transcript as it was. A /resume switch reseeds
// explicitly via reseedTranscriptFromHistory.
func (m *model) seedTranscriptFromHistory() {
	if m.st == nil || m.st.sess == nil || m.transcript.Len() > 0 {
		return
	}
	if text := seedTranscriptText(m.st, false); text != "" {
		m.transcript.WriteString(text)
	}
}

// reseedTranscriptFromHistory replaces the visible transcript with the newly
// bound session's retained conversation — the /resume switch, where keeping
// the old conversation on screen would misrepresent the binding. Unlike the
// startup seed it always writes a banner (even for an empty session), so a
// switch never silently erases the view.
func (m *model) reseedTranscriptFromHistory() {
	if m.st == nil || m.st.sess == nil {
		return
	}
	m.transcript.Reset()
	m.thinkBlocks = nil
	m.thinkBlocksTurnStart = 0
	// The starting fold status, re-seeded from config (issue #278): a
	// switch restores the configured default, not a hardcoded folded —
	// per-session actions still win for the rest of this session.
	m.thinkFoldOpen = m.st.cfg.ThinkFoldExpanded()
	if text := seedTranscriptText(m.st, true); text != "" {
		m.appendTranscript(text)
	}
}

// seedTranscriptText renders the seeded transcript for st.sess: the banner
// plus the windowed turns. switched selects the /resume wording ("switched
// to", banner also for an empty session) over the startup "resumed" form.
func seedTranscriptText(st *interactiveState, switched bool) string {
	hist := st.sess.History
	verb := "resumed"
	if switched {
		verb = "switched to"
	}
	if len(hist) == 0 {
		if !switched {
			return ""
		}
		return fmt.Sprintf("%s %s session %s — no turns yet\n", milkTag(), verb, sessLabel(st.sess))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s session %s (%d turns) — /export prints the full transcript\n\n", milkTag(), verb, sessLabel(st.sess), len(hist))

	n := len(hist)
	writeRange := func(from, to int) {
		for i := from; i < to; i++ {
			writeSeedTurn(&b, st, hist[i])
		}
	}
	if n > replayHeadTurns+replayTailTurns {
		writeRange(0, replayHeadTurns)
		b.WriteString(dim(fmt.Sprintf("[… %d earlier turns omitted — /export prints the full transcript …]", n-replayHeadTurns-replayTailTurns)) + "\n\n")
		writeRange(n-replayTailTurns, n)
	} else {
		writeRange(0, n)
	}
	return b.String()
}

func writeSeedTurn(b *strings.Builder, st *interactiveState, t session.Turn) {
	text := textbudget.SummarizeLong(t.Content, replayMaxMessageChars)
	switch t.Role {
	case session.RoleUser:
		if text == "" {
			return
		}
		b.WriteString(userEcho(promptLabel(st), colorizeTokens(text)))
	case session.RoleAssistant:
		if text == "" {
			return
		}
		name := t.AgentName
		role := "primary"
		if t.Agent == session.AgentEscalation {
			role = "escalation"
		}
		if name == "" {
			name = string(t.Agent)
		}
		b.WriteString(agentTag(name, role) + text + "\n")
		if !th.modern {
			b.WriteByte('\n')
		}
	}
}
