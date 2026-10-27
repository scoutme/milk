package main

import (
	"regexp"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/livebuf"
	"github.com/scoutme/milk/internal/sgr"
)

// Thinking as a first-class view (ADR-0067, issue #208).
//
// Reasoning text no longer streams into the transcript buffer as dim-inline
// text with hand-managed escape bookkeeping. Each reasoning run becomes a
// thinkBlock: its text lives in its own buffer, out of band from the main
// transcript, anchored at the byte offset in the transcript it renders after.
// The renderer splices blocks in as fold containers — a one-line header
// ("▸ thinking · N lines") with the body shown only when expanded — so
// thinking is a region the renderer can style or fold instead of a
// whole-buffer variant (the old transcript/transcriptNoThink pair).
//
// Display rules (headers are information-only: they carry the line count and
// fold state, never a key hint — key hints live in /help and the tips line):
//   - Every block always renders its header row. There is no hidden mode and
//     no placeholder line: the header already shows the one thing a hidden
//     stand-in could say (how much reasoning there is), so the quietest
//     rendering is still informative.
//   - A block is folded by default — streaming or complete alike (issue #208:
//     long reasoning must not flood the transcript). There is no auto-open:
//     while reasoning streams in, the folded header's line count updates
//     live ("▸ thinking · N lines"), and the body shows only when the user
//     opens the block. A block the user never touched stays folded when it
//     closes.
//   - Clicking a fold header — or Ctrl+T for the most recent block — toggles
//     just that block; per-block state is remembered (userSet pins it against
//     auto-fold), so a block opened mid-stream STAYS open when streaming
//     ends. /think expand|collapse pin all blocks at once, /think view
//     swaps in a thinking-only stream.
//   - A new block keeps the session's current folding status (model
//     thinkFoldOpen — the status the previous block was left in): folded by
//     default, open-and-pinned while the user is watching. While that status
//     is folded, turn boundaries collapse the previous turns' blocks (a
//     display tidy that never changes the status); /think expand defines the
//     current default and opts the tidy out.
//   - Fold containers render on their own background (the thinking surface:
//     ansiThinkBg) so the thinking region reads as a different surface from
//     the standard transcript, at every fold state. The surface is a full-row
//     one: after wrapping, every row of a container — header, body lines,
//     blank body lines, and word-wrap continuation rows alike — is padded to
//     the viewport width and re-wrapped in the background (tintThinkSurface),
//     so the surface spans the row and not just the glyphs.
//
// Consumption/display separation is preserved: loop detection and the session
// turn record consume reasoning from their own accumulators regardless of
// what the renderer shows. The ACP `think` option (config show_reasoning) is
// the data-plane relay toggle for agent_thought_chunk updates — it decides
// whether thought chunks leave the process at all and is independent of these
// per-block fold states (ADR-0067).

// thinkBlock is one reasoning block of the visible transcript.
type thinkBlock struct {
	// buf holds the block's reasoning text — kept out of the main transcript
	// buffer so the render stack (colorize cache, wrapping, selection) keeps
	// working on one plain string and no ANSI escape bookkeeping is needed.
	// A pointer: blocks live in a slice and are copied by value (append,
	// growth, model copies), which strings.Builder panics on once written.
	buf *strings.Builder
	// anchor is the byte offset in model.transcript this block renders after.
	// The block text itself never lands in the transcript, so anchors only
	// shift on retractStreamed surgery (handled there).
	anchor int
	// closed is true once no more chunks will arrive (regular content
	// followed, or the turn ended). While open the block is the transcript's
	// last content.
	closed bool
	// expanded is true while the body is rendered. Blocks open folded —
	// streaming ones included — so a true here is always a user choice
	// (header click, Ctrl+T, /think expand) except in the force-expanded
	// /think view copy.
	expanded bool
	// userSet pins the expand state the user chose (fold-header click,
	// /think expand|collapse) so close-on-stream-end no longer overrides it:
	// a block the user opened while streaming stays open past its close.
	userSet bool
}

// text returns the block's reasoning text (empty for a block with no buffer).
func (b thinkBlock) text() string {
	if b.buf == nil {
		return ""
	}
	return b.buf.String()
}

// thinkLines reports the block's rendered line count (for the fold header).
func (b *thinkBlock) thinkLines() int {
	s := b.text()
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// thinkFoldedPrefix/thinkExpandedPrefix are the fold-header glyphs. The
// hit-test (thinkBlockAtLine) keys off these prefixes — see the note there on
// why that is safe enough.
const (
	thinkFoldedPrefix   = "▸ thinking · "
	thinkExpandedPrefix = "▾ thinking · "
)

// thinkBgParams is the thinking-surface background as the SGR parameters that
// set it — the form sgr.State.BG reports, which the full-row painter
// (tintThinkSurface) matches by value. ansiThinkBg is derived from it.
var thinkBgParams = originalThinkBgParams

// ansiThinkBg is the thinking-surface background: fold containers render on
// it so the thinking region is a visibly different surface from the standard
// transcript (header and body alike, every fold state). Applied at render
// time only — the transcript and block buffers stay plain.
var ansiThinkBg = "\033[" + thinkBgParams + "m"

// thinkSurfaceLines paints every line of s with code (see paintLines). A blank
// line still gets its code+reset pair: a blank reasoning line is a row of the
// thinking surface and must render as one (tintThinkSurface recognises surface
// rows by their surface escape, and an unstyled blank row would punch a hole
// in the surface).
func thinkSurfaceLines(s, code string) string { return paintLines(s, code, true) }

// thinkHeaderLine styles a fold-header line: dim on the thinking surface.
func thinkHeaderLine(s string) string {
	if th.modern {
		return thinkSurfaceLines(s, ansiThinkBg+"\033[3m"+tkMuted.sgrFg())
	}
	return thinkSurfaceLines(s, ansiThinkBg+ansiDim)
}

// thinkBodyLine styles a reasoning body: full-contrast text on the thinking
// surface (the background — not dimming — marks reasoning).
func thinkBodyLine(s string) string {
	if th.modern {
		return thinkSurfaceLines(compactBlankLines(s), ansiThinkBg+"\033[3m"+tkThink.sgrFg())
	}
	return thinkSurfaceLines(s, ansiThinkBg)
}

// tintThinkSurface makes the thinking surface a full-row one. content is the
// already-wrapped render, whose rows are self-contained (wrapRows): every row
// that shows the surface background — a container line's own code, including
// the code+reset pair of a blank body line, or the re-opened background on a
// word-wrap continuation of a long reasoning line — is padded to width cells
// and re-wrapped in the surface background (tintBlock's per-line convention —
// the background must span the row, not just the underlying text). The rows
// are final at this point, so the padding cannot re-wrap, and the pad lands
// inside the background run so the surface — not the terminal default — fills
// the rest of the row. Render-time only: the transcript and block buffers
// never see any of this, and transcriptPlainLines strips the escapes and trims
// the pad back off so hit-testing and copy stay plain.
func tintThinkSurface(content string, width int) string {
	if width <= 0 || !strings.Contains(content, ansiThinkBg) {
		return content
	}
	onSurface := func(bg string) bool { return bg == thinkBgParams }
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if sgr.LastBackground(line, onSurface) == "" {
			continue
		}
		if w := ansi.StringWidth(line); w < width {
			line += strings.Repeat(" ", width-w)
		}
		lines[i] = sgr.WithBackground(line, ansiThinkBg)
	}
	return strings.Join(lines, "\n")
}

// lineLabel renders "N lines"/"1 line" for the fold header.
func lineLabel(n int) string {
	if n == 1 {
		return "1 line"
	}
	return strconv.Itoa(n) + " lines"
}

// thinkHeader renders the one-line fold header for a block: glyph, line
// count, and — on a block the user opened while it is still streaming — a
// streaming marker. A folded block renders the same header line whether it is
// streaming or complete: the line count is the live signal (it ticks up as
// reasoning arrives). The header is information-only; it never carries a key
// hint.
func thinkHeader(b *thinkBlock, streaming bool) string {
	n := lineLabel(b.thinkLines())
	if b.expanded {
		if streaming {
			return thinkHeaderLine(thinkExpandedPrefix + n + " · streaming")
		}
		return thinkHeaderLine(thinkExpandedPrefix + n)
	}
	return thinkHeaderLine(thinkFoldedPrefix + n)
}

// renderThinkBlock renders one block's container (header + optional body).
// The returned text always ends with a newline and never starts mid-line —
// transcriptDisplay inserts a separating newline when the surrounding text
// needs one.
func (m *model) renderThinkBlock(b *thinkBlock) string {
	var sb strings.Builder
	sb.WriteString(thinkHeader(b, !b.closed))
	sb.WriteString("\n")
	if b.expanded {
		if body := b.text(); body != "" {
			sb.WriteString(thinkBodyLine(body))
			if !strings.HasSuffix(body, "\n") {
				sb.WriteString("\n")
			}
		}
	}
	return sb.String()
}

// transcriptDisplay renders the visible transcript: the regular-text buffer
// with every thinking block spliced in at its anchor as a fold container.
// Everything downstream (colorize + wrap + selection) runs on the returned
// string exactly like it ran on the old activeTranscript() variant.
func (m *model) transcriptDisplay() string {
	s, _ := m.transcriptDisplayAt()
	return s
}

// transcriptDisplayAt is transcriptDisplay plus the byte offset where the
// first open (streaming) block's container begins — the boundary of the
// display's stable prefix. The streaming tail beyond it is re-rendered from
// scratch on every chunk (the header's live count and the body's per-line
// styling rewrite bytes), so the render cache must never commit into it
// (wrappedTranscript caps the commit there). With no open block the offset is
// the display's full length.
func (m *model) transcriptDisplayAt() (string, int) {
	text := m.transcript.String()
	if len(m.thinkBlocks) == 0 {
		return text, len(text)
	}
	var sb strings.Builder
	openAt := -1
	pos := 0
	for i := range m.thinkBlocks {
		b := &m.thinkBlocks[i]
		at := b.anchor
		if at > len(text) {
			at = len(text)
		}
		if at < pos {
			at = pos
		}
		prefix := text[pos:at]
		sb.WriteString(prefix)
		if sb.Len() > 0 && !strings.HasSuffix(prefix, "\n") {
			sb.WriteString("\n")
		}
		if !b.closed && openAt < 0 {
			openAt = sb.Len()
		}
		sb.WriteString(m.renderThinkBlock(b))
		pos = at
	}
	sb.WriteString(text[pos:])
	if openAt < 0 {
		openAt = sb.Len()
	}
	return sb.String(), openAt
}

// isThinkHeaderLine reports whether a plain (ANSI-stripped) transcript line is
// a fold header at its start. The click hit-test no longer keys off this (see
// thinkHeaderCount); it stays as the plain-text predicate for tests and tools.
func isThinkHeaderLine(line string) bool {
	return strings.HasPrefix(line, thinkFoldedPrefix) || strings.HasPrefix(line, thinkExpandedPrefix)
}

// thinkHeaderRe matches a complete fold header anywhere in a row — the folded
// and expanded forms, including the "· streaming" marker ("▸ thinking · 12
// lines", "▾ thinking · 1 line · streaming").
var thinkHeaderRe = regexp.MustCompile(`[▸▾] thinking · [0-9]+ lines?( · streaming)?`)

// thinkSurfaceRow reports whether a render row paints the thinking surface
// (the render-time signature of a fold container). Matched by the surface's
// SGR parameters, not by a literal escape substring — the row wrap
// (sgr.SelfContain) rewrites escape sequences, so the exact ansiThinkBg bytes
// may no longer appear verbatim.
func thinkSurfaceRow(row string) bool {
	return sgr.LastBackground(row, func(bg string) bool { return bg == thinkBgParams }) != ""
}

// thinkHeaderCount reports how many fold headers one render row of the wrapped
// transcript carries (0 or 1 in practice). Two conditions must hold:
//
//   - the row carries a *complete* header, not just the prefix — ordinary
//     transcript text that mentions "▸ thinking · " without a line count never
//     counts, so it cannot skew the ordinal below; and
//   - the row paints the thinking surface — the render-time signature of a
//     fold container. The markdown reflow (colorizeFull) can glue a
//     container's header onto a neighbouring line ("intro text▸ thinking · 2
//     lines"), and that row still carries the surface background, so glued
//     headers keep counting; an answer that merely quotes the header format
//     carries no surface and never does.
//
// Reasoning text is model output, not adversarial input, and the worst case is
// a mis-targeted fold toggle — a quoted, surface-painted full header is the
// residual case accepted here.
func thinkHeaderCount(row string) int {
	// The surface check only exists in the painted render: paintLines is a
	// no-op off-TTY, where every row is plain and the pattern is all there is
	// (and nothing is clickable anyway).
	if isTTY && !thinkSurfaceRow(row) {
		return 0
	}
	return len(thinkHeaderRe.FindAllString(ansi.Strip(row), -1))
}

// thinkBlockAtLine maps a clicked transcript row (index into the wrapped
// transcript render — the same coordinate space selection uses) to the
// think-block index whose fold header sits there, or -1 when the click was not
// on a header. Headers appear in block order in the display, so the ordinal of
// the clicked header is the block index.
func (m *model) thinkBlockAtLine(line int) int {
	if line < 0 {
		return -1
	}
	rows := strings.Split(m.wrappedTranscript(), "\n")
	if line >= len(rows) || thinkHeaderCount(rows[line]) == 0 {
		return -1
	}
	idx := 0
	for i := 0; i < line && i < len(rows); i++ {
		idx += thinkHeaderCount(rows[i])
	}
	if idx >= len(m.thinkBlocks) {
		return -1
	}
	return idx
}

// toggleThinkBlock flips one block's expand state and pins it (userSet), so a
// mid-stream click keeps the body open past the block's close. The result
// becomes the session's fold status: the next reasoning block is created in
// it (new blocks keep the folding status of the previous one).
func (m *model) toggleThinkBlock(idx int) {
	if idx < 0 || idx >= len(m.thinkBlocks) {
		return
	}
	b := &m.thinkBlocks[idx]
	b.userSet = true
	b.expanded = !b.expanded
	m.thinkFoldOpen = b.expanded
	m.invalidateThinkDisplay()
}

// toggleLastThinkBlock is the Ctrl+T action: flip the most recent block —
// the one still streaming while reasoning is arriving, else the last
// completed block — like clicking that block's fold header. Explicit action
// wins over the auto-fold/pin rules exactly as a click does, so a pinned
// block still toggles. Empty session → a toast instead of silence.
func (m model) toggleLastThinkBlock() model {
	if len(m.thinkBlocks) == 0 {
		m.notify("no thinking blocks in this session yet", "")
		return m
	}
	idx := len(m.thinkBlocks) - 1
	for i := range m.thinkBlocks {
		if !m.thinkBlocks[i].closed {
			idx = i
			break
		}
	}
	m.toggleThinkBlock(idx)
	return m
}

// setAllThinkBlocks expands or folds every block, pinning each (userSet).
// Used by /think expand|collapse; the result becomes the session's fold
// status for new blocks.
func (m *model) setAllThinkBlocks(expanded bool) {
	for i := range m.thinkBlocks {
		m.thinkBlocks[i].userSet = true
		m.thinkBlocks[i].expanded = expanded
	}
	m.thinkFoldOpen = expanded
	m.invalidateThinkDisplay()
}

// collapseThinkBlocksBefore folds every block before index n — the
// turn-boundary tidy (a new turn collapses the previous turns' blocks). It
// applies only while the session's fold status is folded: /think expand means
// "expand all" and thereby defines the current default, and a tidy that
// folded the very blocks it just expanded would contradict it. Display-only
// either way: thinkFoldOpen is deliberately untouched.
func (m *model) collapseThinkBlocksBefore(n int) {
	if m.thinkFoldOpen {
		return
	}
	if n > len(m.thinkBlocks) {
		n = len(m.thinkBlocks)
	}
	changed := false
	for i := 0; i < n; i++ {
		if m.thinkBlocks[i].expanded {
			m.thinkBlocks[i].expanded = false
			m.thinkBlocks[i].userSet = true // explicitly tidied — no auto-reopen
			changed = true
		}
	}
	if changed {
		m.invalidateThinkDisplay()
	}
}

// closeOpenThinkBlock ends the streaming block (regular content or turn end
// followed). A block the user never touched stays folded (it was folded all
// along — no auto-open exists to undo); a block the user opened stays open
// (userSet pins against the close). No-op when nothing is open.
func (m *model) closeOpenThinkBlock() {
	for i := range m.thinkBlocks {
		b := &m.thinkBlocks[i]
		if b.closed {
			continue
		}
		b.closed = true
		if !b.userSet {
			b.expanded = false
		}
		m.invalidateThinkDisplay()
	}
}

// dropThinkBlocksFrom removes every block from index n on — the reasoning-
// promoted path (reasoningPromotedMsg), where the streamed reasoning became
// the turn's actual answer and keeping the blocks would show it twice.
func (m *model) dropThinkBlocksFrom(n int) {
	if n < 0 {
		n = 0
	}
	if n >= len(m.thinkBlocks) {
		return
	}
	m.thinkBlocks = m.thinkBlocks[:n]
	m.invalidateThinkDisplay()
}

// adjustThinkAnchors shifts block anchors after retractStreamed replaced
// [idx, idx+oldLen) with newLen bytes of text. Anchors are "render after this
// many bytes", so an anchor past the replaced region shifts by the delta; an
// anchor inside it (blocks sit on content boundaries, so this is defensive)
// clamps to the replacement's end.
func (m *model) adjustThinkAnchors(idx, oldLen, newLen int) {
	delta := newLen - oldLen
	for i := range m.thinkBlocks {
		a := &m.thinkBlocks[i].anchor
		switch {
		case *a >= idx+oldLen:
			*a += delta
		case *a > idx:
			*a = idx + newLen
		}
	}
}

// invalidateThinkDisplay forces the next wrappedTranscript render to rebuild:
// any fold-state change rewrites the displayed text mid-transcript, so the
// byte-offset-keyed colorize cache can't extend itself incrementally.
func (m *model) invalidateThinkDisplay() {
	m.colorizeForce = true
	m.colorizeTransLen = 0
	m.thinkGen++
}

// thinkViewMaxBytes caps the dedicated thinking view's buffer (issue #208's
// "toggle that swaps the main area to a thinking-only stream").
const thinkViewMaxBytes = 512 * 1024

// startThinkView swaps the main area to a thinking-only stream (/think view):
// every block's fold header plus its full body, in order, over the static
// preview attach view (ADR-0047) — so reasoning is inspectable on its own,
// independent of the transcript's scroll position.
func (m model) startThinkView() (model, tea.Cmd) {
	if len(m.thinkBlocks) == 0 {
		m.notify("no thinking blocks in this session yet", "")
		return m, nil
	}
	var sb strings.Builder
	for i := range m.thinkBlocks {
		b := &m.thinkBlocks[i]
		if i > 0 {
			sb.WriteString("\n")
		}
		// The view is force-expanded — copy the block (buf is a pointer, so
		// this is safe) and show the expanded glyph regardless of the
		// block's transcript fold state.
		v := *b
		v.expanded = true
		sb.WriteString(thinkHeader(&v, !b.closed))
		sb.WriteString("\n")
		if body := b.text(); body != "" {
			sb.WriteString(thinkBodyLine(body))
			if !strings.HasSuffix(body, "\n") {
				sb.WriteString("\n")
			}
		}
	}
	buf := livebuf.New(thinkViewMaxBytes)
	buf.Append([]byte(sb.String()))
	cmd := m.startAttach(attachPreview, "", "thinking", buf)
	return m, cmd
}
