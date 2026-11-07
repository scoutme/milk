package main

import (
	"strings"
	"testing"
)

// rawToolMarkup is the verbatim unparsed tool-call text a turn can end on.
// Hex escapes keep the literal markup sequence out of this file's own source.
const rawToolMarkup = "\x3ctool_call\x3e=bash\x3e\x3cparameter=command\x3els\x3c/parameter\x3e\x3c/function\x3e\x3c/tool_call\x3e"

func TestRetractStreamed_ReplacesLastExactOccurrence(t *testing.T) {
	m := newTestModelForPanels(t)
	m.transcript.WriteString("earlier\n" + rawToolMarkup + "\n")
	m.retractStreamed(rawToolMarkup, "[clean summary]")
	got := m.transcript.String()
	if strings.Contains(got, "tool_call") || !strings.Contains(got, "[clean summary]") || !strings.HasPrefix(got, "earlier\n") {
		t.Errorf("transcript = %q", got)
	}
	if strings.Contains(got, retractFallbackNote) {
		t.Errorf("exact match must not add the fallback note: %q", got)
	}
}

func TestRetractStreamed_FallsBackToNoteWhenNotFound(t *testing.T) {
	m := newTestModelForPanels(t)
	m.transcript.WriteString("something else")
	m.retractStreamed("missing-markup", "[clean summary]")
	got := m.transcript.String()
	if !strings.HasPrefix(got, "something else\n[clean summary]\n") || !strings.Contains(got, retractFallbackNote) {
		t.Errorf("transcript = %q", got)
	}
}

func TestRetractStreamed_AdjustsThinkBlockAnchors(t *testing.T) {
	m := newTestModelForPanels(t)
	m.transcript.WriteString("AAA" + rawToolMarkup + "BBB")
	// One block anchored at the transcript end (opened after all text), one
	// anchored before the markup (inside the soon-to-be-replaced region is
	// covered by TestAdjustThinkAnchors).
	m.thinkBlocks = []thinkBlock{{anchor: m.transcript.Len(), closed: true}, {anchor: 3, closed: true}}
	m.retractStreamed(rawToolMarkup, "X")
	// [3, 3+len(rawToolMarkup)) replaced by 1 byte: the trailing block shifts
	// by the delta, the anchor at 3 (the replacement's start) stays put.
	wantTail := 3 + 1 + 3 // "AAA" + "X" + "BBB"
	if m.thinkBlocks[0].anchor != wantTail {
		t.Errorf("tail block anchor = %d, want %d", m.thinkBlocks[0].anchor, wantTail)
	}
	if m.thinkBlocks[1].anchor != 3 {
		t.Errorf("pre block anchor = %d, want 3", m.thinkBlocks[1].anchor)
	}
	// The display splices both blocks at their (adjusted) anchors, in
	// anchor order (blocksForRender): the block sitting before the retracted
	// region renders *inside* it — the old contiguous-text assertion pinned
	// the accidental clamp the old slice-order splice produced for
	// out-of-order anchors, not the adjusted positions themselves.
	disp := m.transcriptDisplay()
	want := "AAA\n▸ thinking · 0 lines\nXBBB\n▸ thinking · 0 lines\n"
	if disp != want {
		t.Errorf("display = %q, want %q", disp, want)
	}
}

func TestAdjustThinkAnchors(t *testing.T) {
	m := newTestModelForPanels(t)
	m.thinkBlocks = []thinkBlock{{anchor: 4}, {anchor: 12}, {anchor: 20}}
	m.adjustBlockAnchors(8, 6, 2) // replace [8,14) with 2 bytes: delta -4
	want := []int{4, 10, 16}
	for i := range m.thinkBlocks {
		if m.thinkBlocks[i].anchor != want[i] {
			t.Errorf("block %d anchor = %d, want %d", i, m.thinkBlocks[i].anchor, want[i])
		}
	}
}
