package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// plainToolHeaderRe is the header *shape* without the paint gate — used to
// find rendered header rows independently of the count predicate under test.
var plainToolHeaderRe = regexp.MustCompile(`[▸▾] .+? — [0-9]+ lines?( · running| · error)?`)

// TestFoldHeaderCount_GluedRows pins the bug behind "clicking a tool block
// folds the wrong entry, 3 or 4 before": the markdown reflow can glue two
// fold headers onto one render row (the documented "intro text▸ thinking · 2
// lines" glue, but header+header — parallel tool batches render adjacent
// headers), and a greedy `.+` span swallowed both glued tool headers into ONE
// match, silently dropping every glued header after the first from the click
// ordinal. The span is lazy now: every header on a row counts.
func TestFoldHeaderCount_GluedRows(t *testing.T) {
	toolPair := "▸ bash · find things 0 — 3 lines" + "▸ grep · find things 1 — 2 lines"
	if n := toolHeaderCount(toolPair); n != 2 {
		t.Errorf("glued tool headers counted %d, want 2 (row=%q)", n, toolPair)
	}
	thinkPair := "▸ thinking · 2 lines" + "▾ thinking · 3 lines"
	if n := thinkHeaderCount(thinkPair); n != 2 {
		t.Errorf("glued think headers counted %d, want 2 (row=%q)", n, thinkPair)
	}
	// Mixed glue: one row, one header of each family — each family counts 1.
	mixed := "▸ bash · x — 3 lines" + "▸ thinking · 2 lines"
	if n := toolHeaderCount(mixed); n != 1 {
		t.Errorf("mixed glue tool count = %d, want 1", n)
	}
	if n := thinkHeaderCount(mixed); n != 1 {
		t.Errorf("mixed glue think count = %d, want 1", n)
	}
	// A summary quoting a count keeps counting as ONE header.
	quoted := "▸ grep · see '▸ x — 9 lines' — 3 lines"
	if n := toolHeaderCount(quoted); n != 1 {
		t.Errorf("quoted-count summary counted %d, want 1 (row=%q)", n, quoted)
	}
	// Single header with the running suffix is still exactly one.
	if n := toolHeaderCount("▸ bash · find things 0 — 3 lines · running"); n != 1 {
		t.Errorf("running header counted %d, want 1", n)
	}
}

// TestFoldHitAt_ColumnDisambiguation pins the click-column rule on glued
// rows: the click claims the header it landed in, not "whatever family
// happens to be checked first".
func TestFoldHitAt_ColumnDisambiguation(t *testing.T) {
	tool := "▸ bash · x — 3 lines"
	think := "▸ thinking · 2 lines"
	mixed := tool + think

	// Click on the tool half → tool family; on the think half → think.
	if fam, ord := foldHitAt(mixed, 2); fam != foldFamilyTool || ord != 0 {
		t.Errorf("tool-half click = (%d,%d), want (tool,0)", fam, ord)
	}
	if fam, ord := foldHitAt(mixed, len(tool)+2); fam != foldFamilyThink || ord != 0 {
		t.Errorf("think-half click = (%d,%d), want (think,0)", fam, ord)
	}
	// Tool pair: the click column picks the FIRST or SECOND of the pair —
	// the in-row ordinal that decides which block toggles.
	pair := "▸ bash · a — 3 lines" + "▸ grep · b — 2 lines"
	if fam, ord := foldHitAt(pair, 1); fam != foldFamilyTool || ord != 0 {
		t.Errorf("pair first-half click = (%d,%d), want (tool,0)", fam, ord)
	}
	secondCol := len("▸ bash · a — 3 lines") + 3
	if fam, ord := foldHitAt(pair, secondCol); fam != foldFamilyTool || ord != 1 {
		t.Errorf("pair second-half click = (%d,%d), want (tool,1)", fam, ord)
	}
	// A click on full-width padding (beyond every span) still claims a
	// single-header row — the whole row is the header's hit area.
	padded := tool + "        "
	if fam, ord := foldHitAt(padded, len(tool)+4); fam != foldFamilyTool || ord != 0 {
		t.Errorf("padding click = (%d,%d), want (tool,0)", fam, ord)
	}
	// No header → no claim.
	if fam, _ := foldHitAt("just text", 3); fam != foldFamilyNone {
		t.Errorf("text row claimed family %d", fam)
	}
}

// TestToolBlock_OrdinalMatchesEveryHeader: clicking a tool fold header must
// toggle THAT block, never one a few entries before it. The hit-test counts
// headers in the rendered rows above the click; under TTY paint it also
// requires the header paint signature (foldHeaderStyleRow), which the family
// tests never exercise (they run off-TTY, where the gate is skipped).
func TestToolBlock_OrdinalMatchesEveryHeader(t *testing.T) {
	oldTTY := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = oldTTY })
	for _, skin := range []bool{false, true} {
		for _, mode := range []ColorizeMode{ColorizeFenced, ColorizeFull, ColorizeBalanced} {
			t.Run(fmt.Sprintf("skin=%v/%s", skin, mode), func(t *testing.T) {
				oldSkin := th.skin
				th.skin = skin
				t.Cleanup(func() { th.skin = oldSkin })
				m := layoutTestModel(t, 120, 50)
				m.colorizeMode = mode
				m.appendTranscript("## step one\n\nlong text before the tools\n")
				for i := 0; i < 6; i++ {
					id := fmt.Sprintf("t%d", i)
					m = toolUpdate(t, m, startMsg(id, "bash", fmt.Sprintf("find things %d", i), `{"command":"ls -la"}`, ""))
					m = toolUpdate(t, m, toolCallEndMsg{id: id, result: "ok\nline2\n", isError: i == 3})
					m.appendTranscript(fmt.Sprintf("between %d\n", i))
				}
				m.appendTranscript("done\n")
				assertEveryHeaderMaps(t, &m, 6)
			})
		}
	}
}

// TestToolBlock_OrdinalAdjacentBlocks is the parallel-batch shape: tool
// blocks back-to-back with nothing between their fold rows. The reflow may
// glue their headers onto shared rows; the column-aware hit-test must still
// land on exactly the clicked block.
func TestToolBlock_OrdinalAdjacentBlocks(t *testing.T) {
	oldTTY := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = oldTTY })
	m := layoutTestModel(t, 120, 50)
	m.colorizeMode = ColorizeFull
	m.appendTranscript("batch:\n")
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("t%d", i)
		m = toolUpdate(t, m, startMsg(id, "read_file", fmt.Sprintf("file %d", i), `{"path":"f"}`, ""))
		m = toolUpdate(t, m, toolCallEndMsg{id: id, result: "ok\n", isError: false})
	}
	m.appendTranscript("done\n")
	assertEveryHeaderMaps(t, &m, 5)
}

// assertEveryHeaderMaps clicks every rendered fold header at its exact
// column and requires it to map to the block whose position in the render
// it is — the whole user-visible contract of a fold-header click.
func assertEveryHeaderMaps(t *testing.T, m *model, wantBlocks int) {
	t.Helper()
	rows := strings.Split(m.wrappedTranscript(), "\n")
	seen := 0
	for i, l := range rows {
		s := ansi.Strip(l)
		for _, mt := range plainToolHeaderRe.FindAllStringIndex(s, -1) {
			tool, idx := m.foldBlockAtClick(i, mt[0])
			if !tool || idx != seen {
				t.Errorf("row %d (%q) col %d: foldBlockAtClick = (tool=%v,%d), want (true,%d)",
					i, s, mt[0], tool, idx, seen)
			}
			seen++
		}
	}
	if seen != wantBlocks {
		t.Fatalf("found %d rendered tool headers, want %d (display = %q)", seen, wantBlocks, m.wrappedTranscript())
	}
}
