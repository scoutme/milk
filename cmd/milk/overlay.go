package main

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// overlayLayer is a block of already-styled text placed at a screen offset
// over a base block.
type overlayLayer struct {
	content string
	x, y    int
}

// composeOverlays draws layers over base with Lip Gloss's cell compositor,
// later layers on top. The result covers the union of the layers' bounds, so a
// short base row still shows an overlay that sticks out, and styles are resolved per cell: a base span that is
// cut by an overlay is closed and re-opened on its own, no manual escape
// surgery. Base rows without overlay keep their bytes only when no layer is
// given, so callers skip the call in that case.
func composeOverlays(base string, layers ...overlayLayer) string {
	if len(layers) == 0 {
		return base
	}
	rows := strings.Split(base, "\n")
	want := overlayRowWidths(rows, layers)
	padRows(rows, want)

	root := lipgloss.NewLayer(strings.Join(rows, "\n"))
	for i, l := range layers {
		root.AddLayers(lipgloss.NewLayer(l.content).X(l.x).Y(l.y).Z(i + 1))
	}
	// Canvas.Render trims trailing blank cells; a row's width is part of the
	// layout (side panels sit to its right), so restore it.
	out := strings.Split(lipgloss.NewCompositor(root).Render(), "\n")
	padRows(out, want)
	return strings.Join(out, "\n")
}

// overlayRowWidths is the width each base row must keep: its own, widened to
// the right edge of any layer that covers it.
func overlayRowWidths(rows []string, layers []overlayLayer) []int {
	want := make([]int, len(rows))
	for i, row := range rows {
		want[i] = lipgloss.Width(row)
	}
	for _, l := range layers {
		right := l.x + lipgloss.Width(l.content)
		for r := l.y; r < l.y+lipgloss.Height(l.content) && r < len(rows); r++ {
			want[r] = max(want[r], right)
		}
	}
	return want
}

// padRows right-pads each row with spaces up to its wanted width.
func padRows(rows []string, want []int) {
	for i := range rows {
		if i >= len(want) {
			return
		}
		if gap := want[i] - lipgloss.Width(rows[i]); gap > 0 {
			rows[i] += strings.Repeat(" ", gap)
		}
	}
}

// overlayRowAt overlays content on a single row starting at column x. Used
// by the toasts, which paint row by row over a larger block.
func overlayRowAt(row string, x int, content string) string {
	return composeOverlays(row, overlayLayer{content: content, x: x})
}

// centeredX returns the column that centers a block of boxWidth in width.
func centeredX(width, boxWidth int) int {
	return max((width-boxWidth)/2, 0)
}

// joinLines is strings.Join(lines, "\n") named for the overlay callers.
func joinLines(lines []string) string { return strings.Join(lines, "\n") }
