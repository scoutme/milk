package main

// Tests for issue #212: panel word-wrap must never let a single unbreakable
// word (e.g. a full file path) overflow the panel's inner width — it has to be
// hard-broken into width-sized chunks, measured in display cells rather than
// bytes, and the rendered panel must clamp any residual overflow so the frame
// and scrollbar column never move.

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	rw "github.com/mattn/go-runewidth"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
)

// longPath is the exact repro from issue #212: 36 display cells, wider than
// the 32-cell inner width of every side panel.
const longPath = "/home/scoutme/altworkspace/SWARMDEMO"

func TestWordWrap_SplitsOverlongWord(t *testing.T) {
	got := wordWrap(longPath, 29, 29, 3)
	if len(got) < 2 {
		t.Fatalf("expected the path to be hard-broken across lines, got %d line: %q", len(got), got)
	}
	for i, l := range got {
		if w := rw.StringWidth(l); w > 29 {
			t.Errorf("line %d width %d exceeds 29: %q", i, w, l)
		}
	}
	// A single hard-broken word must reassemble without losing characters.
	if joined := strings.Join(got, ""); joined != longPath {
		t.Errorf("hard-break lost characters: joined %q, want %q", joined, longPath)
	}
}

func TestWordWrap_HardBreakHonorsMaxLinesWithEllipsis(t *testing.T) {
	word := strings.Repeat("x", 100)
	got := wordWrap(word, 30, 30, 2)
	if len(got) != 2 {
		t.Fatalf("got %d lines, want exactly 2 (maxLines): %q", len(got), got)
	}
	if !strings.HasSuffix(got[1], "…") {
		t.Errorf("last line must end with an ellipsis when the word outlives maxLines, got %q", got[1])
	}
	for i, l := range got {
		if w := rw.StringWidth(l); w > 30 {
			t.Errorf("line %d width %d exceeds 30: %q", i, w, l)
		}
	}
}

func TestWordWrap_MeasuresCellsNotBytes(t *testing.T) {
	// Each 漢 is 3 display cells but 9 UTF-8 bytes — the old byte-based
	// accounting wrote it past the line budget.
	word := strings.Repeat("漢", 30) // 90 cells, 270 bytes
	got := wordWrap(word, 32, 32, 4)
	if len(got) < 2 {
		t.Fatalf("expected the wide-character word to be hard-broken, got %d line", len(got))
	}
	for i, l := range got {
		if w := rw.StringWidth(l); w > 32 {
			t.Errorf("line %d display width %d exceeds 32: %q", i, w, l)
		}
	}
	if joined := strings.Join(got, ""); joined != word {
		t.Errorf("hard-break lost characters: %d runes rejoined, want %d", len([]rune(joined)), len([]rune(word)))
	}
}

func TestWordWrap_WrapsAtSpacesWhenWordsFit(t *testing.T) {
	got := wordWrap("the quick brown fox jumps", 12, 12, 5)
	want := []string{"the quick", "brown fox", "jumps"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWordWrapPanel_SplitsOverlongWord(t *testing.T) {
	got := wordWrapPanel(longPath, 25)
	if len(got) < 2 {
		t.Fatalf("expected the path to be hard-broken across lines, got %d line: %q", len(got), got)
	}
	for i, l := range got {
		if w := rw.StringWidth(l); w > 25 {
			t.Errorf("line %d width %d exceeds 25: %q", i, w, l)
		}
	}
	if joined := strings.Join(got, ""); joined != longPath {
		t.Errorf("hard-break lost characters: joined %q, want %q", joined, longPath)
	}
}

func TestWordWrapPanel_WrapsAtSpacesWhenWordsFit(t *testing.T) {
	const src = "build a thing with milk"
	got := wordWrapPanel(src, 10)
	for i, l := range got {
		if w := rw.StringWidth(l); w > 10 {
			t.Errorf("line %d width %d exceeds 10: %q", i, w, l)
		}
	}
	if joined := strings.Join(got, " "); joined != src {
		t.Errorf("word boundaries must survive wrapping: joined %q, want %q", joined, src)
	}
}

// TestWorkflowPanelLines_LongTaskPathStaysWithinInner is the end-to-end repro
// from the issue: a workflow whose Task is the long path, rendered into the
// workflow panel's content lines.
func TestWorkflowPanelLines_LongTaskPathStaysWithinInner(t *testing.T) {
	st := &workflow.State{WorkflowName: "refactor", Task: longPath}
	lines := buildWorkflowPanelLines(st, workflowPanelInner)
	if len(lines) < 3 {
		t.Fatalf("expected the task path to add wrapped lines, got %d: %q", len(lines), lines)
	}
	for i, l := range lines {
		if w := rw.StringWidth(ansi.Strip(l)); w > workflowPanelInner {
			t.Errorf("line %d width %d exceeds inner %d: %q", i, w, workflowPanelInner, l)
		}
	}
}

// TestRenderWorkflowPanel_ClampsOverflowingLine exercises the render-side
// clamp in renderSidePanel: a builder that doesn't wrap its own text (the
// workflow panel's "role: …" line) must still never outgrow the panel.
func TestRenderWorkflowPanel_ClampsOverflowingLine(t *testing.T) {
	old := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = old })

	m := &model{
		st:    &interactiveState{sess: &session.Session{}},
		width: 200,
		workflowState: &workflow.State{
			WorkflowName: "dev",
			Role:         strings.Repeat("R", 100),
		},
	}
	rendered := m.renderWorkflowPanel(8)
	for i, l := range strings.Split(rendered, "\n") {
		if w := rw.StringWidth(ansi.Strip(l)); w != workflowPanelInner {
			t.Errorf("row %d width %d, want exactly %d: %q", i, w, workflowPanelInner, l)
		}
	}
}

// TestMemoryPanel_LongContentWithinInnerAndIDsAligned covers both halves of
// the issue on the memory panel: every rendered line stays within
// memoryPanelInner, and buildPanelLineIDs hands out exactly one ID per
// rendered line so double-click mapping can't drift (the two had independent —
// and disagreeing — wrap-width calculations before the fix).
func TestMemoryPanel_LongContentWithinInnerAndIDsAligned(t *testing.T) {
	mem, err := memory.NewStore(t.TempDir(), "test-session")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ctx := context.Background()
	contents := []string{
		// 16 cells: fits the rendered first-line budget (18 with badge) but
		// wrapped under the old ID-side budget (8) — the parity trap.
		"alpha beta gamma",
		// Pure over-long word: the old code wrote it verbatim (overflow).
		strings.Repeat("y", 40),
		// The issue's repro path, preceded by a word.
		"stored " + longPath,
	}
	for i, c := range contents {
		if _, err := mem.Record(ctx, c, memory.ProducerLocal, memory.ConsumerLocal, memory.Roles{}, false); err != nil {
			t.Fatalf("Record(%d): %v", i, err)
		}
	}

	bricks := sessionBricks{
		currentNeed:           "short need",
		lastLocalSummary:      "short local summary",
		lastEscalationSummary: strings.Repeat("e", 60), // stale-brick budget (see below)
		escalationBrief:       "brief text here",
		contextStale:          true, // exercises the "(stale) " suffix budget on both sides
	}

	lines := buildPanelLines(mem, memoryPanelInner, bricks)
	ids := buildPanelLineIDs(mem, bricks)
	if len(lines) != len(ids) {
		t.Fatalf("line/ID count mismatch: %d lines vs %d IDs — click mapping would be off", len(lines), len(ids))
	}
	for i, l := range lines {
		if w := rw.StringWidth(ansi.Strip(l)); w > memoryPanelInner {
			t.Errorf("line %d width %d exceeds inner %d: %q", i, w, memoryPanelInner, l)
		}
	}
}

// TestPerceptAndBrickWidths_SharedByRenderAndIDs pins the shared helpers both
// sides must use: any percept/brick (badge or not, stale or not) yields the
// identical wrap widths no matter which side asks.
func TestPerceptAndBrickWidths_SharedByRenderAndIDs(t *testing.T) {
	p := memory.Percept{ID: "abcdef123456", W: 0.7, Consumer: memory.ConsumerLocal}
	fw, cw := perceptWrapWidths(p, memoryPanelInner)
	// bullet "• " (2) + id "#abcdef " (8) + badge " [P]" (4) → 32-14 = 18.
	if fw != 18 || cw != 30 {
		t.Errorf("perceptWrapWidths = (%d, %d), want (18, 30)", fw, cw)
	}
	p.Consumer = memory.ConsumerAll
	if fw, _ = perceptWrapWidths(p, memoryPanelInner); fw != 22 {
		t.Errorf("unbadged perceptWrapWidths first = %d, want 22", fw)
	}

	// Stale bricks pay for the "(stale) " suffix (8 cells) out of firstW.
	fw, _ = brickWrapWidths("escalation", true, memoryPanelInner)
	// "  escalation: " = 14 + 8 = 22 → 32-22 = 10.
	if fw != 10 {
		t.Errorf("stale brickWrapWidths first = %d, want 10", fw)
	}
	fw, _ = brickWrapWidths("escalation", false, memoryPanelInner)
	if fw != 18 {
		t.Errorf("fresh brickWrapWidths first = %d, want 18", fw)
	}
}
