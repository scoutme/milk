package main

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"
)

// Tool-block click-target parity with the think side (ADR-0080): every
// rendered tool header must toggle THE block it displays. The reported bug —
// "clicking a tool block expands the entry two before the clicked one", the
// very class 844c125 fixed for thinking — is what this pins.

// buildToolClickModel streams n tool blocks through the real message path
// (start → end → closed), interleaved with realistic transcript content
// including a quoted header-shaped line in plain text (the tool equivalent of
// the think ordinal hole), then parks the viewport at the bottom.
func buildToolClickModel(t *testing.T, theme string, width, height int, mode string, nBlocks int) model {
	t.Helper()
	oldTTY := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = oldTTY })
	oldTheme := th.name
	setTheme(theme)
	t.Cleanup(func() { setTheme(oldTheme) })

	m := newTestModelForPanels(t)
	nm, _ := m.handleResize(tea.WindowSizeMsg{Width: width, Height: height})
	m = nm.(model)
	m.colorizeMode = ParseColorizeMode(mode)

	m.appendTranscript("**user**: do some work please\n\n")
	for i := 0; i < nBlocks; i++ {
		if i%3 == 1 {
			// Reasoning interleaved — the two families share one anchor space
			// (blocksForRender) and must not skew each other's ordinals.
			for j := 0; j <= i%4; j++ {
				m.appendThinkingStreamed(fmt.Sprintf("thought %d line %d\n", i, j))
			}
			m.closeOpenThinkBlock()
		}
		id := "t" + strconv.Itoa(i)
		m = toolUpdate(t, m, startMsg(id, "bash",
			fmt.Sprintf("command %02d with a summary long enough to threaten wrapping at narrow widths", i),
			`{"command":"echo hi"}`, ""))
		// Realistic result: grep/sed output can contain header-shaped text
		// (this session's own transcripts printed toolHeaderRe's source).
		result := strings.Repeat(fmt.Sprintf("result line %d\n", i), i+1)
		if i%2 == 0 {
			result += "assert.Contains(t, disp, \"▸ bash · git status — 34 lines\")\n"
		}
		m = toolUpdate(t, m, toolCallEndMsg{id: id, result: result, isError: false})
		m.appendTranscript(fmt.Sprintf("## step %d\n\nhere is what I did, with `code` in it and text that wraps once or twice at narrow widths.\n\n", i))
	}
	// Quoted header-shaped text in ordinary transcript (no fold surface):
	// must never count toward the ordinal (mirrors TestQuotedHeadersDoNot…).
	m.appendTranscript("the header format is: `▸ bash · x — 9 lines` and again: `▾ bash · y — 3 lines`\n\n")
	m.closeOpenToolBlock()
	m.setViewportContent()
	m.vp.GotoBottom()
	m.setViewportContent()
	return m
}

// TestToolHeaderClickTogglesOwnBlock clicks every rendered tool header on
// screen and asserts the toggled block is the one whose header was shown.
func TestToolHeaderClickTogglesOwnBlock(t *testing.T) {
	for _, theme := range []string{"off", "supermilk"} {
		for _, width := range []int{160, 100, 72, 48} {
			for _, mode := range []string{"off", "fenced", "balanced", "full"} {
				for _, scroll := range []string{"bottom", "mid"} {
					t.Run(fmt.Sprintf("%s/w%d/%s/%s", theme, width, mode, scroll), func(t *testing.T) {
						m := buildToolClickModel(t, theme, width, 40, mode, 8)
						if scroll == "mid" && m.vp.YOffset() > 2 {
							m.vp.SetYOffset(m.vp.YOffset() - 8)
							m.setViewportContent()
						}
						checked := 0
						// One header at a time, always from the ALL-EXPANDED
						// session state: bodies (with header-shaped quoted
						// output) sit above every header — the exact ordinal
						// stress of a real click session — and the state is
						// stable across iterations so screen rows are re-read
						// fresh for every click.
						for target := 0; target < len(m.toolBlocks); target++ {
							for bi := range m.toolBlocks {
								m.toolBlocks[bi].expanded = true
								m.toolBlocks[bi].userSet = true
							}
							m.invalidateThinkDisplay()
							m.setViewportContent()
							m.vp.GotoBottom()
							m.setViewportContent()
							rows := strings.Split(m.viewString(), "\n")
							wantLabel := fmt.Sprintf("command %02d", target)
							clickRow := -1
							for i, r := range rows {
								if toolHeaderCount(r) > 0 && strings.Contains(ansi.Strip(r), wantLabel) {
									clickRow = i
								}
							}
							if clickRow < 0 {
								continue // scrolled out of the visible window
							}
							before := expandedSet(&m)
							m.handleMouse(leftEv(mousePress, 2, clickRow))
							m.handleMouse(leftEv(mouseRelease, 2, clickRow))
							after := expandedSet(&m)
							changed := []int{}
							for bi := range m.toolBlocks {
								if before[bi] != after[bi] {
									changed = append(changed, bi)
								}
							}
							if len(changed) != 1 || changed[0] != target {
								t.Errorf("all-expanded, click block %d at row %d → changed %v", target, clickRow, changed)
							}
							checked++
						}
						if checked == 0 {
							t.Fatalf("no tool headers clickable (%d blocks)", len(m.toolBlocks))
						}
					})
				}
			}
		}
	}
}

// expandedSet snapshots which tool blocks are expanded (click cumulative state).
func expandedSet(m *model) map[int]bool {
	out := map[int]bool{}
	for i := range m.toolBlocks {
		out[i] = m.toolBlocks[i].expanded
	}
	return out
}

// Body rows carry the fold surface — the OLD gate (surface only) counted
// header-shaped text quoted inside an expanded body toward the ordinal,
// skewing every click below it by the number of quotes (reproduced as
// "click block 3 → block 5 toggled": +2 from two quoted lines above). The
// header's own paint is what separates header from body.
func TestQuotedHeadersInBodyDoNotCount(t *testing.T) {
	for _, tc := range []struct{ name, row string }{
		{
			"tool body quote classic",
			thinkSurfaceLines(`assert.Contains(t, disp, "▸ bash · git status — 34 lines")`, ansiThinkBg),
		},
		{
			"think body quote classic",
			thinkSurfaceLines(`docs: the header looks like "▸ thinking · 9 lines"`, ansiThinkBg),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldTTY := isTTY
			isTTY = true
			defer func() { isTTY = oldTTY }()
			if n := toolHeaderCount(tc.row); n != 0 {
				t.Errorf("toolHeaderCount(quoted body) = %d, want 0", n)
			}
			if n := thinkHeaderCount(tc.row); n != 0 {
				t.Errorf("thinkHeaderCount(quoted body) = %d, want 0", n)
			}
		})
	}

	// The painted header itself must still count (both signatures).
	oldTTY := isTTY
	isTTY = true
	defer func() { isTTY = oldTTY }()
	oldTheme := th.name
	setTheme("off")
	defer setTheme(oldTheme)
	hdr := toolHeaderLine("▸ bash · git status — 34 lines")
	if toolHeaderCount(hdr) != 1 {
		t.Errorf("toolHeaderCount(painted header) = %d, want 1: %q", toolHeaderCount(hdr), hdr)
	}
	thdr := thinkHeaderLine("▸ thinking · 9 lines")
	if thinkHeaderCount(thdr) != 1 {
		t.Errorf("thinkHeaderCount(painted header) = %d, want 1: %q", thinkHeaderCount(thdr), thdr)
	}
}
