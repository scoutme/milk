package main

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Thinking-as-a-view tests (ADR-0067, issue #208): blocks live out of band,
// render as fold containers, fold on close, and stay click-toggleable.

func TestThinkBlocks_StreamIntoFoldedBlock(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendThinkingStreamed("first\nsecond\n")
	disp := m.transcriptDisplay()
	// Folded by default while streaming — same single header line as a
	// completed block, with the live line count.
	if !strings.Contains(disp, thinkFoldedPrefix+"2 lines") {
		t.Errorf("streaming block must render its folded header, got %q", disp)
	}
	if strings.Contains(disp, "first\nsecond") {
		t.Errorf("streaming block body must not render unless opened, got %q", disp)
	}
	// The count ticks up live as reasoning arrives.
	m.appendThinkingStreamed("third\n")
	if disp = m.transcriptDisplay(); !strings.Contains(disp, thinkFoldedPrefix+"3 lines") {
		t.Errorf("folded header must update its line count live, got %q", disp)
	}
	// Regular content closes the block: it stays folded to its header and the
	// body is gone from the display (inspectable via expand, not dumped inline).
	m.appendTranscript("the answer\n")
	disp = m.transcriptDisplay()
	if !strings.Contains(disp, thinkFoldedPrefix+"3 lines") {
		t.Errorf("completed block must render its folded header, got %q", disp)
	}
	if strings.Contains(disp, "first") || strings.Contains(disp, "third") {
		t.Errorf("folded body must not render, got %q", disp)
	}
	// The transcript buffer itself holds only regular text — no dim escapes,
	// no reasoning.
	if got := m.transcript.String(); got != "the answer\n" {
		t.Errorf("transcript buffer = %q", got)
	}
}

// Blocks are copied by value (append, slice growth); a value-embedded
// strings.Builder panicked on the second write after such a copy.
func TestThinkBlocks_MultiChunkAndSliceGrowth(t *testing.T) {
	m := newTestModelForPanels(t)
	for run := 0; run < 5; run++ {
		m.appendThinkingStreamed("a")
		m.appendThinkingStreamed("b")
		m.appendThinkingStreamed("c\n")
		m.appendTranscript("answer\n")
	}
	if len(m.thinkBlocks) != 5 {
		t.Fatalf("blocks = %d, want 5", len(m.thinkBlocks))
	}
	for i, b := range m.thinkBlocks {
		if got := b.text(); got != "abc\n" {
			t.Errorf("block %d buf = %q", i, got)
		}
	}
}

// There is no hidden mode: a block always renders its header row — folded
// with its line count once complete. The old one-line placeholder is gone
// for good, and the folded body stays inspectable, not dumped inline.
func TestThinkBlocks_NoHiddenMode(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendThinkingStreamed("secret\nthoughts\n")
	m.appendTranscript("answer\n") // closes + folds
	disp := m.transcriptDisplay()
	if !strings.Contains(disp, thinkFoldedPrefix+"2 lines") {
		t.Errorf("completed block must render its fold header, got %q", disp)
	}
	if strings.Contains(disp, "[thinking") {
		t.Errorf("no hidden placeholder may render anywhere, got %q", disp)
	}
	if strings.Contains(disp, "secret") {
		t.Errorf("folded body must not render, got %q", disp)
	}
}

// Fold headers are information-only: glyph, line count, fold/streaming
// state — never a key hint (key hints live in /help and the tips line). A
// folded block renders the same header whether streaming or complete; the
// streaming marker appears only on a block the user opened mid-stream.
func TestThinkBlockHeaders_InfoOnly(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendThinkingStreamed("one\ntwo\nthree\n")
	disp := m.transcriptDisplay()
	if !strings.Contains(disp, thinkFoldedPrefix+"3 lines") {
		t.Errorf("streaming header must be the folded line with the count, got %q", disp)
	}
	if strings.Contains(disp, "· streaming") {
		t.Errorf("folded streaming header must look like a completed one, got %q", disp)
	}
	// Opened explicitly mid-stream: the expanded header carries the marker.
	m.toggleThinkBlock(0)
	disp = m.transcriptDisplay()
	if !strings.Contains(disp, thinkExpandedPrefix+"3 lines · streaming") {
		t.Errorf("opened streaming header must carry the marker, got %q", disp)
	}
	m.appendTranscript("done\n")
	disp = m.transcriptDisplay()
	if !strings.Contains(disp, thinkExpandedPrefix+"3 lines") || strings.Contains(disp, "· streaming") {
		t.Errorf("completed header must drop the streaming marker, got %q", disp)
	}
	if strings.Contains(disp, "Ctrl+T") || strings.Contains(disp, "/think") {
		t.Errorf("headers must not carry key hints, got %q", disp)
	}
}

// Ctrl+T toggles the most recent block — the streaming one while reasoning
// is arriving, else the last completed block — and pins it like a header
// click (explicit action wins over the auto-fold rules). Streaming blocks
// start folded like everything else; opening one mid-stream pins it against
// the close, so it stays open when streaming ends.
func TestToggleLastThinkBlock_TogglesMostRecent(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendThinkingStreamed("first\n") // folded by default while streaming
	if m.thinkBlocks[0].expanded {
		t.Fatal("streaming block must start folded — no auto-open")
	}
	m = m.toggleLastThinkBlock() // opens + pins the streaming block
	if !m.thinkBlocks[0].expanded || !m.thinkBlocks[0].userSet {
		t.Errorf("Ctrl+T must open and pin the streaming block, got %+v", m.thinkBlocks[0])
	}
	m.appendTranscript("done\n")
	if !m.thinkBlocks[0].expanded {
		t.Error("a block the user opened mid-stream must stay open when streaming ends")
	}
	m.appendThinkingStreamed("second\n") // keeps the session's open status
	if !m.thinkBlocks[1].expanded || !m.thinkBlocks[1].userSet {
		t.Fatalf("a new block must keep the folding status of the previous one, got %+v", m.thinkBlocks[1])
	}
	m = m.toggleLastThinkBlock() // the streaming block again, not block 0 — folds it
	if m.thinkBlocks[1].expanded || !m.thinkBlocks[1].userSet {
		t.Errorf("Ctrl+T must toggle the streaming block, not a completed one, got %+v", m.thinkBlocks[1])
	}
	m = m.toggleLastThinkBlock() // open it again, still pinned
	if !m.thinkBlocks[1].expanded {
		t.Errorf("Ctrl+T must re-open the streaming block, got %+v", m.thinkBlocks[1])
	}
	m.appendTranscript("done\n")
	m = m.toggleLastThinkBlock() // nothing streaming → the last block
	if m.thinkBlocks[1].expanded || !m.thinkBlocks[1].userSet {
		t.Errorf("Ctrl+T with nothing streaming must toggle the last block, got %+v", m.thinkBlocks[1])
	}
	if !m.thinkBlocks[0].userSet || !m.thinkBlocks[0].expanded {
		t.Errorf("earlier blocks must be untouched, got %+v", m.thinkBlocks[0])
	}
}

func TestThinkBlocks_ClickTogglesAndPins(t *testing.T) {
	// Wide viewport: no line wraps, so display line indexes and the wrapped
	// coordinate space the mouse handler feeds thinkBlockAtLine coincide.
	m := layoutTestModel(t, 200, 50)
	m.colorizeMode = ColorizeOff
	m.appendThinkingStreamed("a\nb\n")
	m.appendTranscript("done\n") // closes + folds the block
	disp := m.transcriptDisplay()
	lines := strings.Split(disp, "\n")
	if !isThinkHeaderLine(lines[0]) {
		t.Fatalf("first line must be the fold header, got %q", lines[0])
	}
	if idx := m.thinkBlockAtLine(0); idx != 0 {
		t.Fatalf("header click maps to block %d, want 0", idx)
	}
	if idx := m.thinkBlockAtLine(1); idx != -1 {
		t.Errorf("body line click maps to block %d, want -1", idx)
	}
	if idx := m.thinkBlockAtLine(99); idx != -1 {
		t.Errorf("out-of-range click maps to block %d, want -1", idx)
	}
	m.toggleThinkBlock(0)
	if !m.thinkBlocks[0].expanded || !m.thinkBlocks[0].userSet {
		t.Fatalf("click must expand and pin, got %+v", m.thinkBlocks[0])
	}
	if d := m.transcriptDisplay(); !strings.Contains(d, "a\nb") {
		t.Errorf("expanded body must render, got %q", d)
	}
	// The pinned state survives later blocks — and the new block keeps the
	// session's fold status (open, set by the click above) without touching
	// block 0.
	m.appendThinkingStreamed("more\n")
	m.appendTranscript("again\n")
	if !m.thinkBlocks[0].expanded {
		t.Error("pinned expansion must survive later closes")
	}
	if !m.thinkBlocks[1].expanded {
		t.Errorf("a new block must keep the session's open status, got %+v", m.thinkBlocks[1])
	}
	// A click on the second header toggles the second block, not the first.
	disp = m.transcriptDisplay()
	lines = strings.Split(disp, "\n")
	second := -1
	for i, l := range lines {
		if isThinkHeaderLine(l) && i > 0 {
			second = i
			break
		}
	}
	if second < 0 {
		t.Fatalf("second header not found in %q", disp)
	}
	if idx := m.thinkBlockAtLine(second); idx != 1 {
		t.Errorf("second header maps to block %d, want 1", idx)
	}
}

func TestThinkBlocks_PromotedReasoningDropped(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendThinkingStreamed("turn one thoughts\n")
	m.appendTranscript("turn one answer\n")
	m.thinkBlocksTurnStart = len(m.thinkBlocks) // dispatchAgent boundary
	m.appendThinkingStreamed("turn two thoughts\n")
	if len(m.thinkBlocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(m.thinkBlocks))
	}
	m.dropThinkBlocksFrom(m.thinkBlocksTurnStart)
	if len(m.thinkBlocks) != 1 {
		t.Errorf("promotion must drop only the current turn's blocks, got %d", len(m.thinkBlocks))
	}
	if d := m.transcriptDisplay(); strings.Contains(d, "turn two thoughts") {
		t.Errorf("promoted reasoning must not render twice, got %q", d)
	}
}

// A block pinned open mid-stream stays open past its close: the fold is
// per-block state (userSet wins over the close). A new block keeps the
// session's fold status, so a folded session stays folded block after block.
func TestThinkBlocks_PinnedOpenSurvivesClose(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendThinkingStreamed("watching\n")
	m.toggleThinkBlock(0) // open + pin while streaming
	m.appendTranscript("done\n")
	if !m.thinkBlocks[0].expanded || !m.thinkBlocks[0].userSet {
		t.Fatalf("a block pinned open must stay expanded after close, got %+v", m.thinkBlocks[0])
	}
	if d := m.transcriptDisplay(); !strings.Contains(d, "watching") {
		t.Errorf("pinned body must render, got %q", d)
	}
	// Fold it back — the status is folded again, and a block the user never
	// touches in a folded session stays folded.
	m.toggleThinkBlock(0)
	m.appendThinkingStreamed("quiet\n")
	m.appendTranscript("done\n")
	if m.thinkBlocks[1].expanded {
		t.Errorf("a block created in a folded session must stay folded, got %+v", m.thinkBlocks[1])
	}
}

func TestThinkBlockHeaderLabels(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendThinkingStreamed("one line")
	m.appendTranscript("\ndone\n")
	d := m.transcriptDisplay()
	if !strings.Contains(d, thinkFoldedPrefix+"1 line") {
		t.Errorf("singular label expected, got %q", d)
	}
}

func TestTranscriptDisplay_SplicesBlocksAtAnchors(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendTranscript("one\n")
	m.appendThinkingStreamed("a\nb\n")
	m.appendTranscript("two\n")
	m.appendThinkingStreamed("c\n")
	m.appendTranscript("three\n")
	d := m.transcriptDisplay()
	iOne := strings.Index(d, "one\n")
	iHdr2 := strings.Index(d, thinkFoldedPrefix+"2 lines")
	iTwo := strings.Index(d, "two\n")
	iHdr1 := strings.Index(d, thinkFoldedPrefix+"1 line")
	iThree := strings.Index(d, "three\n")
	if !(iOne >= 0 && iHdr2 > iOne && iTwo > iHdr2 && iHdr1 > iTwo && iThree > iHdr1) {
		t.Errorf("block splice order wrong:\n%s", d)
	}
}

func TestThinkBlocks_ExpandCollapseAll(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendThinkingStreamed("one\n")
	m.appendTranscript("a\n")
	m.appendThinkingStreamed("two\n")
	m.appendTranscript("b\n")
	m.setAllThinkBlocks(true)
	for i := range m.thinkBlocks {
		if !m.thinkBlocks[i].expanded || !m.thinkBlocks[i].userSet {
			t.Errorf("block %d not pinned open: %+v", i, m.thinkBlocks[i])
		}
	}
	m.setAllThinkBlocks(false)
	for i := range m.thinkBlocks {
		if m.thinkBlocks[i].expanded {
			t.Errorf("block %d still open after collapse-all", i)
		}
	}
}

func TestStartThinkView_ShowsAllBodies(t *testing.T) {
	m := newTestModelForPanels(t)
	// Nothing yet: no attach, just a toast.
	empty, cmd := m.startThinkView()
	if cmd != nil || empty.attached != nil {
		t.Errorf("empty session must not attach, got cmd=%v attached=%v", cmd != nil, empty.attached != nil)
	}
	m.appendThinkingStreamed("first thought\n")
	m.appendTranscript("a\n")
	m.appendThinkingStreamed("second thought\n")
	m.appendTranscript("b\n")
	m2, cmd := m.startThinkView()
	if cmd == nil {
		t.Fatal("expected an attach refresh cmd")
	}
	if m2.attached == nil || m2.attached.kind != attachPreview {
		t.Fatalf("attached = %+v, want a preview attach", m2.attached)
	}
	snap := m2.attached.buf.Snapshot()
	for _, want := range []string{"first thought", "second thought"} {
		if !strings.Contains(snap, want) {
			t.Errorf("thinking view missing %q:\n%s", want, snap)
		}
	}
	// Force-expanded: bodies are shown, so the view must use the expanded
	// glyph even for blocks that are folded in the transcript.
	if !strings.Contains(snap, thinkExpandedPrefix) || strings.Contains(snap, thinkFoldedPrefix) {
		t.Errorf("thinking view headers must render expanded:\n%s", snap)
	}
}

// A new reasoning block keeps the session's folding status — the status the
// previous block was left in — instead of resetting to folded. Folding status
// is one session-wide bit (model.thinkFoldOpen), set only by explicit fold
// actions: open one block and the following arrive open; fold one and the
// following arrive folded.
func TestThinkBlocks_NewBlockKeepsFoldStatus(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendThinkingStreamed("first\n")
	if m.thinkBlocks[0].expanded {
		t.Fatal("default status is folded — the first block must start folded")
	}
	m.toggleThinkBlock(0) // open + pin → the status is open
	m.appendTranscript("a\n")
	m.appendThinkingStreamed("second\n")
	if !m.thinkBlocks[1].expanded || !m.thinkBlocks[1].userSet {
		t.Errorf("new block must keep the open status (open and pinned past close), got %+v", m.thinkBlocks[1])
	}
	m.toggleThinkBlock(1) // fold → the status is folded again
	m.appendTranscript("b\n")
	m.appendThinkingStreamed("third\n")
	if m.thinkBlocks[2].expanded {
		t.Errorf("new block must keep the folded status, got %+v", m.thinkBlocks[2])
	}
	m.setAllThinkBlocks(true) // /think expand → the status is open again
	m.appendTranscript("c\n")
	m.appendThinkingStreamed("fourth\n")
	if !m.thinkBlocks[3].expanded {
		t.Errorf("new block must keep the /think expand status, got %+v", m.thinkBlocks[3])
	}
}

// A new turn collapses the previous turns' blocks (the dispatchAgent tidy) —
// a display-only change: the fold status new blocks are created with is
// deliberately untouched, so a user watching reasoning keeps watching into
// the new turn.
func TestThinkBlocks_NewTurnCollapsesPreviousTurnBlocks(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendThinkingStreamed("one\n")
	m.toggleThinkBlock(0) // open + pin → the status is open
	m.appendTranscript("a\n")
	m.appendThinkingStreamed("two\n") // inherits the open status
	m.toggleThinkBlock(1)             // fold it → the status is folded again
	m.appendTranscript("b\n")
	// The turn boundary as dispatchAgent runs it:
	m.closeOpenThinkBlock()
	m.collapseThinkBlocksBefore(len(m.thinkBlocks))
	m.thinkBlocksTurnStart = len(m.thinkBlocks)
	for i := range m.thinkBlocks {
		if m.thinkBlocks[i].expanded {
			t.Errorf("block %d must be collapsed at the new turn, got %+v", i, m.thinkBlocks[i])
		}
	}
	d := m.transcriptDisplay()
	if strings.Contains(d, "one\n") || strings.Contains(d, "two\n") {
		t.Errorf("collapsed bodies must not render after the tidy, got %q", d)
	}
	// The tidy is display-only: the folded status stays, so the new turn's
	// blocks arrive folded.
	m.appendThinkingStreamed("three\n")
	if m.thinkBlocks[2].expanded {
		t.Errorf("the tidy must not change the fold status, got %+v", m.thinkBlocks[2])
	}
}

// /think expand means "expand all" and thereby defines the current default:
// the turn-boundary tidy does not run in that mode — it would fold the very
// blocks the command just expanded.
func TestThinkBlocks_ExpandAllOptsOutOfTurnTidy(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendThinkingStreamed("one\n")
	m.appendTranscript("a\n")
	m.setAllThinkBlocks(true) // /think expand — the default is now expanded
	// The turn boundary as dispatchAgent runs it:
	m.closeOpenThinkBlock()
	m.collapseThinkBlocksBefore(len(m.thinkBlocks))
	m.thinkBlocksTurnStart = len(m.thinkBlocks)
	if !m.thinkBlocks[0].expanded || !m.thinkBlocks[0].userSet {
		t.Errorf("/think expand must survive the turn boundary, got %+v", m.thinkBlocks[0])
	}
	if d := m.transcriptDisplay(); !strings.Contains(d, "one\n") {
		t.Errorf("expanded body must still render after the boundary, got %q", d)
	}
	// And the default carries on: the new turn's blocks arrive expanded.
	m.appendThinkingStreamed("two\n")
	if !m.thinkBlocks[1].expanded {
		t.Errorf("new blocks must keep the expand-all status, got %+v", m.thinkBlocks[1])
	}
}

// Fold containers render on the thinking surface (their own background), so
// the thinking region is a visibly different surface from the standard
// transcript. The styling is render-time only: the buffers stay plain.
func TestThinkBlocks_ThinkingSurfaceBackground(t *testing.T) {
	old := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = old })
	m := newTestModelForPanels(t)
	m.appendThinkingStreamed("a thought\n")
	m.appendTranscript("done\n")
	disp := m.transcriptDisplay()
	if !strings.Contains(disp, ansiThinkBg) {
		t.Errorf("fold container must render on the thinking background:\n%q", disp)
	}
	// Expanded: the body sits on the surface too.
	m.toggleThinkBlock(0)
	if disp = m.transcriptDisplay(); !strings.Contains(disp, "a thought") {
		t.Fatalf("expanded body must render:\n%q", disp)
	}
	if strings.Contains(m.transcript.String(), ansiThinkBg) {
		t.Error("the thinking surface must not leak into the transcript buffer")
	}
	if b := m.thinkBlocks[0].text(); strings.Contains(b, ansiThinkBg) {
		t.Error("the thinking surface must not leak into the block buffer")
	}
}

// The thinking surface is a full-row one: every row of a fold container —
// header, body lines, blank body lines included — is padded to the viewport
// width inside the surface background, so the background spans the row and
// not just the glyphs. Rows off the surface are left alone, and the pad
// itself is render-time only (it never reaches the plain coordinate space).
func TestThinkSurface_FullRowCoverage(t *testing.T) {
	old := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = old })
	m := layoutTestModel(t, 40, 20)
	m.colorizeMode = ColorizeFenced // plain lines pass through untouched
	m.appendTranscript("before\n")
	m.appendThinkingStreamed("short\n\nalpha beta gamma delta epsilon zeta eta theta iota kappa\n")
	m.appendTranscript("after\n") // closes + folds
	m.toggleThinkBlock(0)         // expand: body rows must carry the surface too
	vw := m.vpWidth()
	rows := strings.Split(m.wrappedTranscript(), "\n")

	var surfacePlain, otherPlain []string
	for i, row := range rows {
		plain := strings.TrimRight(ansi.Strip(row), " ")
		if !strings.Contains(row, ansiThinkBg) {
			otherPlain = append(otherPlain, plain)
			if w := ansi.StringWidth(row); w != utf8.RuneCountInString(plain) {
				t.Errorf("row %d off the surface must not be padded: %q", i, row)
			}
			continue
		}
		surfacePlain = append(surfacePlain, plain)
		if w := ansi.StringWidth(row); w != vw {
			t.Errorf("surface row %d width = %d, want full-row %d: %q", i, w, vw, row)
		}
		// The pad must sit inside the surface run — background-coloured
		// spaces before the row's closing reset, never bare tail cells. A
		// full-width row legitimately carries zero pad.
		if !surfaceRowShape.MatchString(row) {
			t.Errorf("surface row %d must end pad-then-reset: %q", i, row)
		}
	}
	// Full coverage: only the regular rows sit off the surface.
	if want := []string{"before", "after", ""}; !reflect.DeepEqual(otherPlain, want) {
		t.Errorf("off-surface rows = %q, want %q", otherPlain, want)
	}
	// Header, body rows and the blank body row are all surface rows, and the
	// word-wrapped fragments of the long line rejoin into it.
	if len(surfacePlain) < 4 {
		t.Fatalf("surface rows = %q, want header + body rows + wrapped fragments", surfacePlain)
	}
	if want := thinkExpandedPrefix + "3 lines"; surfacePlain[0] != want {
		t.Errorf("first surface row = %q, want %q", surfacePlain[0], want)
	}
	if surfacePlain[1] != "short" || surfacePlain[2] != "" {
		t.Errorf("body rows = %q, want %q", surfacePlain[1:], []string{"short", "", "(wrapped fragments)"})
	}
	if joined := strings.Join(surfacePlain[3:], " "); joined != "alpha beta gamma delta epsilon zeta eta theta iota kappa" {
		t.Errorf("wrapped body rows rejoin to %q", joined)
	}
}

// surfaceRowShape matches a row whose background-coloured pad (possibly
// empty on a full-width row) runs right up to the closing reset —
// tintThinkSurface's full-row shape; a row ending in bare pad spaces would
// leave the tail on the terminal default instead of the surface.
var surfaceRowShape = regexp.MustCompile(` *\x1b\[0m$`)

// Full-row coverage holds on the moving target too: while reasoning streams
// into an open block — partial line, live header count, the render cache warm
// between chunks — and after a terminal resize, every surface row still pads
// to the current viewport width, and the plain coordinate space (hit-testing,
// selection, copy) stays free of the pad.
func TestThinkSurface_FullRowCoverageStreamingAndResize(t *testing.T) {
	old := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = old })
	m := layoutTestModel(t, 120, 30)
	m.colorizeMode = ColorizeFenced
	m.thinkFoldOpen = true // watch mode: the block streams open and pinned
	m.appendThinkingStreamed("first line\nsecond line\n")
	_ = m.wrappedTranscript() // warm/extend the render cache mid-stream
	m.appendThinkingStreamed("partial")
	_ = m.wrappedTranscript()
	m.appendThinkingStreamed(" line\n")

	check := func(stage string) {
		vw := m.vpWidth()
		surface := 0
		for i, row := range strings.Split(m.wrappedTranscript(), "\n") {
			if !strings.Contains(row, ansiThinkBg) {
				continue
			}
			surface++
			if w := ansi.StringWidth(row); w != vw {
				t.Errorf("%s: surface row %d width = %d, want full-row %d: %q", stage, i, w, vw, row)
			}
			if !surfaceRowShape.MatchString(row) {
				t.Errorf("%s: surface row %d must end pad-then-reset: %q", stage, i, row)
			}
		}
		if surface < 4 {
			t.Errorf("%s: %d surface rows, want header + body rows (incl. the partial line)", stage, surface)
		}
		for i, l := range m.transcriptPlainLines() {
			if strings.HasSuffix(l, " ") {
				t.Errorf("%s: plain line %d carries render-time pad: %q", stage, i, l)
			}
		}
	}
	check("streaming")

	// Resize: the rows re-wrap and the pad follows the new width.
	nm, _ := m.handleResize(tea.WindowSizeMsg{Width: 80, Height: 30})
	m = nm.(model)
	check("resized")
}

// The surface survives every colorization mode: glamour (colorizeFull) needs
// its input ANSI-stripped, so thinking rows must be routed around it — a
// container row that loses its background there loses the surface entirely.
func TestThinkSurface_SurvivesFullColorize(t *testing.T) {
	old := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = old })
	m := layoutTestModel(t, 80, 30)
	m.colorizeMode = ColorizeFull
	m.appendThinkingStreamed("a thought\n")
	m.appendTranscript("done\n")
	m.toggleThinkBlock(0) // expand: the body row must keep the surface too
	vw := m.vpWidth()
	found := map[string]bool{}
	for i, row := range strings.Split(m.wrappedTranscript(), "\n") {
		plain := strings.TrimSpace(ansi.Strip(row))
		if plain != "a thought" && !isThinkHeaderLine(plain) {
			continue
		}
		found[plain] = true
		if !strings.Contains(row, ansiThinkBg) {
			t.Errorf("row %d lost the thinking surface under full colorize: %q", i, row)
		}
		if w := ansi.StringWidth(row); w != vw {
			t.Errorf("row %d width = %d, want full-row %d: %q", i, w, vw, row)
		}
	}
	if !found["a thought"] || !found[thinkExpandedPrefix+"1 line"] {
		t.Errorf("container rows not found in render: %v", found)
	}
}

// Streaming into an open block must never garble earlier lines: the render
// cache is capped before the open block's container, because the container is
// re-rendered from scratch on every chunk — the header's live line count and
// the body's per-line styling both rewrite bytes mid-display, so a cached
// prefix reaching into it goes stale while the appended suffix starts at
// shifted offsets. This exercises the hot streaming path end to end with
// styling on, including a header digit roll (9 → 10 lines).
func TestWrappedTranscript_StreamingBlockNoGarbling(t *testing.T) {
	old := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = old })
	m := layoutTestModel(t, 120, 40)
	m.colorizeMode = ColorizeFenced // plain lines pass through untouched
	m.appendTranscript("before\n")
	m.thinkFoldOpen = true // watch mode: the block is born open and pinned
	for i := 1; i <= 9; i++ {
		m.appendThinkingStreamed(fmt.Sprintf("line %d\n", i))
		_ = m.wrappedTranscript() // warm/extend the render cache between chunks
	}
	m.appendThinkingStreamed("line 10") // partial line + the header count rolls 9 → 10
	_ = m.wrappedTranscript()
	m.appendThinkingStreamed(" tail\n")
	got := m.transcriptPlainLines()
	var want []string
	want = append(want, "before", thinkExpandedPrefix+"10 lines · streaming")
	for i := 1; i <= 9; i++ {
		want = append(want, fmt.Sprintf("line %d", i))
	}
	want = append(want, "line 10 tail", "") // trailing newline
	if len(got) != len(want) {
		t.Fatalf("render lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("render line %d = %q, want %q (full: %q)", i, got[i], want[i], got)
		}
	}
}
