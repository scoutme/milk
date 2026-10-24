package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func wideTable() []string {
	return []string{
		"| File | When | Palette |",
		"|---|---|---|",
		"| `Screenshot … 165153.png`, `183539.png`, `milk_crush_inspired_tui.png` | 16:51 → 18:35 | dark, near-black navy `#080e16` with `#202933` panels |",
		"| `milk-redesign.exe~` | 18:37 | the build you were screenshotting |",
	}
}

func TestRenderTableBlock_FitsViewportAndKeepsPipesAligned(t *testing.T) {
	const width = 80
	rows := renderTableBlock(wideTable(), width)
	var physical []string
	for _, row := range rows {
		physical = append(physical, strings.Split(row, "\n")...)
	}
	pipeCols := -1
	for _, line := range physical {
		plain := ansi.Strip(line)
		if w := ansi.StringWidth(plain); w > width {
			t.Fatalf("line is %d wide, viewport is %d: %q", w, width, plain)
		}
		if !strings.HasPrefix(plain, "|") || !strings.HasSuffix(plain, "|") {
			t.Errorf("every physical line must keep its frame pipes: %q", plain)
		}
		if cols := strings.Count(plain, "|"); pipeCols == -1 {
			pipeCols = cols
		} else if cols != pipeCols {
			t.Errorf("pipe count changed (%d vs %d): %q", cols, pipeCols, plain)
		}
	}
	if len(physical) <= len(wideTable()) {
		t.Error("long cells should wrap onto extra lines")
	}
}

func TestRenderTableBlock_NoLimitKeepsOneLinePerRow(t *testing.T) {
	for i, row := range renderTableBlock(wideTable(), 0) {
		if strings.Contains(row, "\n") {
			t.Errorf("row %d wrapped without a width limit", i)
		}
	}
}

func TestRenderTableBlock_TooNarrowFallsBackToNaturalWidth(t *testing.T) {
	rows := renderTableBlock(wideTable(), 12)
	for i, row := range rows {
		if strings.Contains(row, "\n") {
			t.Errorf("row %d: columns are never squeezed under the minimum", i)
		}
	}
}

func TestFitTableColumns_NarrowsWidestFirst(t *testing.T) {
	got := fitTableColumns([]int{40, 10, 10}, 50)
	total := 1 + 3*3
	for _, w := range got {
		total += w
	}
	if total > 50 {
		t.Errorf("fitted table is %d wide: %v", total, got)
	}
	if got[1] != 10 || got[2] != 10 {
		t.Errorf("short columns must stay untouched: %v", got)
	}
}

func TestColorizeSingleWidth_ReflowsTableInBalancedMode(t *testing.T) {
	seg := strings.Join(wideTable(), "\n") + "\n"
	out := ansi.Strip(colorizeSingleWidth(seg, ColorizeBalanced, 80))
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if ansi.StringWidth(line) > 80 {
			t.Errorf("line wider than the viewport: %q", line)
		}
	}
}
