package main

import (
	"encoding/json"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/livebuf"
)

// Tool-call fold containers — the tool-side sibling of ADR-0067's think
// blocks (issue: reuse the inline-preview machinery for tools). Every tool
// call the main turn runs becomes a fold container anchored where its
// streamed hint line used to be: a one-line header (`▸ bash · git status —
// 34 lines`, `· running` until the outcome lands, `· error` on failure) with
// the full input and output in the body — the data the transcript only ever
// hinted at (`» name: summary`), fed live by onToolCallView/onToolCallDone
// (internal/agent/local) and closed at turn end.
//
// Display rules — deliberately the think-block rules (the same machinery
// renders both, over the same fold surface, with disjoint header patterns):
//   - The header always renders; folded by default; a user-set expand state
//     pins against the turn-boundary tidy (/tools expand opts out, exactly
//     like /think expand).
//   - Clicking a header toggles that block (toolBlockAtLine — same ordinal
//     argument as thinkBlockAtLine, same accepted residual: a quoted,
//     surface-painted header could mis-target a toggle; reasoning and tool
//     summaries are not adversarial input).
//   - The header truncates to the viewport width so it is always one row —
//     the ordinal hit-test counts complete headers per row and a wrapped
//     header would never complete its first row.
//   - The container renders on the shared fold surface (ansiThinkBg — one
//     "inspectable region" family; the header pattern and body style
//     distinguish reasoning from execution), so tintThinkSurface paints its
//     rows full-width and transcriptPlainLines keeps copy/hit-testing plain.
//
// Ownership: blocks are created only for main-turn runs — the view callback
// is out-gated (isMainTurnOut) so job/workflow/buffer streams keep their
// streamed `»` hints untouched. workflowDisplaySend translates a start into
// a plain hint chunk for the workflow's live view instead.

// toolBlock is one tool-call fold container in the visible transcript.
type toolBlock struct {
	// anchor is the byte offset in model.transcript this block renders
	// after (same contract as thinkBlock.anchor; shifted by
	// adjustBlockAnchors on retract surgery).
	anchor int
	// seq orders containers that share an anchor (creation order across
	// both block families — a tool call and the reasoning block it closed
	// can sit on the same byte offset).
	seq int
	// closed is true once the outcome arrived or the turn ended. While
	// open the header carries `· running`.
	closed bool
	// expanded/userSet mirror thinkBlock: folded by default, a user
	// choice pins against the turn-boundary tidy.
	expanded bool
	userSet  bool

	id, name, summary string
	// argsText is the pretty-printed input, computed once at creation;
	// body() appends the outcome when it arrives.
	argsText string
	result   string
	// hasResult distinguishes "outcome landed and was empty" from "no
	// outcome yet" (a cancelled turn closes without one).
	hasResult bool
	isError   bool
}

// prettyArgs renders a tool call's raw argument JSON indented for display,
// falling back to the raw string when it isn't JSON (providers without a
// JSON argument blob).
func prettyArgs(argsJSON string) string {
	argsJSON = strings.TrimSpace(argsJSON)
	if argsJSON == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(argsJSON), &v); err != nil {
		return argsJSON
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return argsJSON
	}
	return string(out)
}

// body returns the block's full body text: the pretty input, and once the
// outcome landed, a blank line and the result.
func (b *toolBlock) body() string {
	if !b.hasResult {
		return b.argsText
	}
	if b.argsText == "" {
		return b.result
	}
	return b.argsText + "\n\n" + b.result
}

// toolLines reports the block's rendered body line count (for the fold
// header) — same counting as thinkLines.
func (b *toolBlock) toolLines() int {
	s := b.body()
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// toolHeader renders the one-line fold header: glyph, `name · summary`, the
// body line count, then ` · running` while the outcome is pending and
// ` · error` on a failed one. The summary truncates to fit the row (the
// ordinal hit-test needs a complete header on exactly one row). The ` — N
// lines` form is what makes the tool pattern disjoint from think headers
// (`▸ thinking · N lines` — no em-dash before the count).
func (m *model) toolHeader(b *toolBlock) string {
	glyph := "▸"
	if b.expanded {
		glyph = "▾"
	}
	tail := " — " + lineLabel(b.toolLines())
	if !b.closed {
		tail += " · running"
	}
	if b.isError {
		tail += " · error"
	}
	mid := b.name
	if b.summary != "" {
		mid += " · " + b.summary
	}
	head := glyph + " " + mid
	if w := m.vpWidth(); w > 0 {
		// Reserve the tail so it never gets cut: the count is the one
		// piece the hit-test and the reader both need.
		if room := w - ansi.StringWidth(tail) - 1; room > 8 {
			if ansi.StringWidth(head) > room {
				head = ansi.Truncate(head, room, "…")
			}
		}
	}
	return toolHeaderLine(head + tail)
}

// toolHeaderLine styles a tool fold header: accent-italic on the fold
// surface — the accent keeps execution distinct from reasoning's muted
// header on the same surface (ADR-0070's italic-tool-name vocabulary).
func toolHeaderLine(s string) string {
	if th.modern {
		return thinkSurfaceLines(s, ansiThinkBg+"\033[3m"+tkAccent.sgrFg())
	}
	return thinkSurfaceLines(s, ansiThinkBg+ansiDim)
}

// toolBodyLine styles an expanded body: plain full-contrast text on the
// fold surface (no reasoning tint, no italic — input/output is data, not
// thought).
func toolBodyLine(s string) string {
	if th.modern {
		return thinkSurfaceLines(compactBlankLines(s), ansiThinkBg+tkText.sgrFg())
	}
	return thinkSurfaceLines(s, ansiThinkBg)
}

// renderToolBlock renders one block's container (header + optional body).
// Same contract as renderThinkBlock: always ends with a newline, never
// starts mid-line.
func (m *model) renderToolBlock(b *toolBlock) string {
	var sb strings.Builder
	sb.WriteString(m.toolHeader(b))
	sb.WriteString("\n")
	if b.expanded {
		if body := b.body(); body != "" {
			sb.WriteString(toolBodyLine(body))
			if !strings.HasSuffix(body, "\n") {
				sb.WriteString("\n")
			}
		}
	}
	return sb.String()
}

// toolHeaderRe matches a complete tool fold header anywhere in a row —
// folded or expanded, running or error. The ` — N lines` anchor keeps it
// disjoint from think headers (checked in both directions by tests).
var toolHeaderRe = regexp.MustCompile(`[▸▾] .+ — [0-9]+ lines?( · running| · error)?`)

// toolHeaderCount reports how many tool fold headers one render row carries.
// Same argument as thinkHeaderCount: a complete header, and the fold surface
// as the container signature (so a model that merely quotes a header format
// without the surface never counts; off-TTY — paintLines no-op — the surface
// check is skipped, mirroring the think side).
func toolHeaderCount(row string) int {
	if isTTY && !thinkSurfaceRow(row) {
		return 0
	}
	return len(toolHeaderRe.FindAllString(ansi.Strip(row), -1))
}

// toolBlockAtLine maps a clicked transcript row to the tool-block index whose
// fold header sits there, or -1. Headers render in block order, so the
// ordinal of the clicked tool header is the block index (think headers are
// invisible to this count — disjoint patterns).
func (m *model) toolBlockAtLine(line int) int {
	if line < 0 {
		return -1
	}
	rows := strings.Split(m.wrappedTranscript(), "\n")
	if line >= len(rows) || toolHeaderCount(rows[line]) == 0 {
		return -1
	}
	idx := 0
	for i := 0; i < line && i < len(rows); i++ {
		idx += toolHeaderCount(rows[i])
	}
	if idx >= len(m.toolBlocks) {
		return -1
	}
	return idx
}

// openToolBlock creates the block for a starting tool call, anchored at the
// transcript's current end — the position its streamed hint line used to
// occupy. Always folded (no auto-open: the header's `· running` is the
// live signal, exactly like a streaming think header's line count).
func (m *model) openToolBlock(msg toolCallStartMsg) {
	b := toolBlock{
		anchor:   m.transcript.Len(),
		seq:      m.takeBlockSeq(),
		id:       msg.id,
		name:     msg.name,
		summary:  msg.summary,
		argsText: prettyArgs(msg.argsJSON),
	}
	m.toolBlocks = append(m.toolBlocks, b)
	m.invalidateThinkDisplay()
}

// completeToolBlock lands a tool outcome on its block: body gains the
// result, `· running` drops, `· error` appears on failure. Unknown ids are
// no-ops (a job's outcome can never reach here — the view callback is
// out-gated — but a stale or duplicated id must not misfire either).
func (m *model) completeToolBlock(id, result string, isError bool) {
	found := false
	for i := range m.toolBlocks {
		b := &m.toolBlocks[i]
		if b.id == "" || b.id != id || b.closed {
			continue
		}
		b.result = result
		b.hasResult = true
		b.isError = isError
		b.closed = true
		if !b.userSet {
			b.expanded = false
		}
		found = true
	}
	if found {
		m.invalidateThinkDisplay()
	}
}

// closeOpenToolBlock ends every still-running block — turn end (a cancelled
// or interrupted call never produced an outcome; args-only body stays
// truthful) and the next turn's start as a defensive sweep. No-op when
// nothing is open.
func (m *model) closeOpenToolBlock() {
	changed := false
	for i := range m.toolBlocks {
		b := &m.toolBlocks[i]
		if b.closed {
			continue
		}
		b.closed = true
		if !b.userSet {
			b.expanded = false
		}
		changed = true
	}
	if changed {
		m.invalidateThinkDisplay()
	}
}

// toggleToolBlock flips one block's expand state and pins it (userSet) —
// the click action, same semantics as toggleThinkBlock minus the fold-status
// bookkeeping (toolFoldOpen is the session status; toggling one block does
// not redefine what new blocks do — /tools expand does).
func (m *model) toggleToolBlock(idx int) {
	if idx < 0 || idx >= len(m.toolBlocks) {
		return
	}
	b := &m.toolBlocks[idx]
	b.userSet = true
	b.expanded = !b.expanded
	m.invalidateThinkDisplay()
}

// setAllToolBlocks expands or folds every block, pinning each (userSet) and
// setting the session's tool fold status for the turn-boundary tidy —
// /tools expand|collapse, mirroring setAllThinkBlocks.
func (m *model) setAllToolBlocks(expanded bool) {
	changed := false
	for i := range m.toolBlocks {
		if m.toolBlocks[i].expanded != expanded || !m.toolBlocks[i].userSet {
			m.toolBlocks[i].userSet = true
			m.toolBlocks[i].expanded = expanded
			changed = true
		}
	}
	m.toolFoldOpen = expanded
	if changed {
		m.invalidateThinkDisplay()
	}
}

// ctrlTChordWindow is the double-press window for Ctrl+T's fold-all chord —
// a package var so tests can drive chord legs without sleeping.
var ctrlTChordWindow = 400 * time.Millisecond

// handleCtrlT is Ctrl+T: one press toggles the most recent thinking block
// (unchanged); two presses within ctrlTChordWindow fold every fold
// container — thinking and tool blocks alike (the "collapse all"
// counterpart), pinning each and setting both fold statuses so the
// turn-boundary tidy stays coherent (/think collapse + /tools collapse in
// one gesture).
//
// The window compares against the previous press and EVERY press stamps
// lastCtrlTPress — chord legs included. Held-key auto-repeat (first repeat
// ~500ms apart, then rapid) therefore settles into repeated fold-alls
// (idempotent) instead of a toggle cascade, and a press outside the window
// is a fresh single toggle. Fold-all is silent, like every keyboard fold.
func (m model) handleCtrlT() model {
	now := time.Now()
	chord := !m.lastCtrlTPress.IsZero() && now.Sub(m.lastCtrlTPress) <= ctrlTChordWindow
	m.lastCtrlTPress = now
	if chord {
		m.setAllThinkBlocks(false)
		m.setAllToolBlocks(false)
		return m
	}
	return m.toggleLastThinkBlock()
}

// collapseToolBlocksBefore folds every block before index n — the
// turn-boundary tidy, gated on the tool fold status exactly like the think
// tidy (/tools expand defines the current default and opts out).
func (m *model) collapseToolBlocksBefore(n int) {
	if m.toolFoldOpen {
		return
	}
	if n > len(m.toolBlocks) {
		n = len(m.toolBlocks)
	}
	changed := false
	for i := 0; i < n; i++ {
		if m.toolBlocks[i].expanded {
			m.toolBlocks[i].expanded = false
			m.toolBlocks[i].userSet = true
			changed = true
		}
	}
	if changed {
		m.invalidateThinkDisplay()
	}
}

// isMainTurnOut reports whether the agent's run writer is the main turn's
// transcript stream (runTurn's touchWriter wrapping the sendWriter) — the
// ownership test behind out-gated view callbacks. Job live buffers, workflow
// buffers and any other writer return false and keep their streamed hints.
func isMainTurnOut(out io.Writer) bool {
	for {
		switch w := out.(type) {
		case *touchWriter:
			out = w.w
		case *sendWriter:
			return true
		default:
			return false
		}
	}
}

// startToolView swaps the main area to a tool-only view (/tools view): every
// block's fold header plus its full body, force-expanded, over the static
// preview attach view — the tool-side mirror of startThinkView, so full
// inputs and outputs are inspectable independent of the transcript's scroll
// position.
func (m model) startToolView() (model, tea.Cmd) {
	if len(m.toolBlocks) == 0 {
		m.notify("no tool blocks in this session yet", "")
		return m, nil
	}
	var sb strings.Builder
	for i := range m.toolBlocks {
		b := &m.toolBlocks[i]
		if i > 0 {
			sb.WriteString("\n")
		}
		v := *b
		v.expanded = true
		sb.WriteString(m.toolHeader(&v))
		sb.WriteString("\n")
		if body := b.body(); body != "" {
			sb.WriteString(toolBodyLine(body))
			if !strings.HasSuffix(body, "\n") {
				sb.WriteString("\n")
			}
		}
	}
	buf := livebuf.New(thinkViewMaxBytes)
	buf.Append([]byte(sb.String()))
	cmd := m.startAttach(attachPreview, "", "tools", buf)
	return m, cmd
}

// adjustBlockAnchors shifts BOTH families' anchors after retractStreamed
// replaced [idx, idx+oldLen) with newLen bytes of text (renamed from
// adjustThinkAnchors: the surgery no longer knows which family sits past
// the replaced region).
func (m *model) adjustBlockAnchors(idx, oldLen, newLen int) {
	delta := newLen - oldLen
	shift := func(a *int) {
		switch {
		case *a >= idx+oldLen:
			*a += delta
		case *a > idx:
			*a = idx + newLen
		}
	}
	for i := range m.thinkBlocks {
		shift(&m.thinkBlocks[i].anchor)
	}
	for i := range m.toolBlocks {
		shift(&m.toolBlocks[i].anchor)
	}
}

// takeBlockSeq hands out the next creation order for either block family
// (splice tie-break — see blocksForRender). Monotonic for the model's
// lifetime; never reset, it only orders.
func (m *model) takeBlockSeq() int {
	m.blockSeq++
	return m.blockSeq
}

// withMainToolCallView attaches the fold-container view callbacks to a
// session agent (buildTUIAgents — never the shared wiring, so jobs, workflow
// runners and agent-as-tool copies built from da.agentWiring keep their
// streamed `»` hints untouched). The callbacks are out-gated: only the main
// turn's stream (isMainTurnOut) yields a message, and `send` is the caller's
// display route — raw program send for main turns, workflowDisplaySend for
// workflow steps, which translates a start into its plain hint chunk.
func withMainToolCallView(a *local.Agent, send func(tea.Msg)) *local.Agent {
	return a.WithOnToolCallView(mainToolCallView(send)).
		WithOnToolCallDone(mainToolCallDone(send))
}

// mainToolCallView builds the start-side view callback for a display route:
// out-gated (only the main turn's stream yields a message) and send-captured.
// Split from withMainToolCallView so the gate is testable without an agent.
func mainToolCallView(send func(tea.Msg)) func(out io.Writer, id, name, summary, argsJSON, diff, hintText string) bool {
	return func(out io.Writer, id, name, summary, argsJSON, diff, hintText string) bool {
		if !isMainTurnOut(out) {
			return false
		}
		send(toolCallStartMsg{id: id, name: name, summary: summary,
			argsJSON: argsJSON, diff: diff, hintText: hintText})
		return true
	}
}

// mainToolCallDone is the outcome-side twin (same gate, same route).
func mainToolCallDone(send func(tea.Msg)) func(out io.Writer, id, result string, isError bool) bool {
	return func(out io.Writer, id, result string, isError bool) bool {
		if !isMainTurnOut(out) {
			return false
		}
		send(toolCallEndMsg{id: id, result: result, isError: isError})
		return true
	}
}

// sortBlocksForRender orders both families for the display splice: by
// anchor, ties by creation sequence (a tool call and the reasoning block it
// closed share the transcript end; the one created first renders first).
type renderBlock struct {
	tool        bool
	idx         int
	anchor, seq int
}

func blocksForRender(think []thinkBlock, tools []toolBlock) []renderBlock {
	all := make([]renderBlock, 0, len(think)+len(tools))
	for i := range think {
		all = append(all, renderBlock{tool: false, idx: i, anchor: think[i].anchor, seq: think[i].seq})
	}
	for i := range tools {
		all = append(all, renderBlock{tool: true, idx: i, anchor: tools[i].anchor, seq: tools[i].seq})
	}
	// SliceStable: a full tie (manually-built blocks sharing anchor and a
	// zero seq) keeps the concatenation order — deterministic for tests and
	// harmless in production, where every created block has a unique seq.
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].anchor != all[j].anchor {
			return all[i].anchor < all[j].anchor
		}
		return all[i].seq < all[j].seq
	})
	return all
}
