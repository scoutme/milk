package main

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/sgr"
)

// Full-row diff backgrounds (issue #273).
//
// internal/diff renders +/- rows as bright glyphs on a dark background —
// ESC[38;5;119;48;5;22m for additions, ESC[38;5;203;48;5;52m for deletions —
// and closes each row with a reset before the newline. The background thus
// only covers the glyphs: on a short row the rest of the line sits on the
// terminal default, which reads as a truncated colour block. This painter
// reuses the thinking surface's full-row technique (tintThinkSurface, ADR-0067)
// for those rows: after wrapping, every row that carries a diff background is
// padded to the viewport width and the pad lands *inside* the background so
// the colour spans the row.
//
// Scope and invariants (the reasoning is in ADR-0069):
//   - Render-time only, exactly like the think painter: buffers are untouched,
//     the colorize cache keeps storing the untinted wrap, and
//     transcriptPlainLines strips the escapes and trims the pad — copy,
//     selection and hit-testing stay plain.
//   - Emission (internal/diff) is unchanged. Its per-line reset stays exactly
//     where it is; the pad is appended after it and re-opens the same colour
//     just for the pad, so what follows the row is byte-order-identical to
//     before. This matters: a row whose background were left open at the
//     newline would bleed green/red onto the next line (SGR state carries
//     across \n, and a following dim code does not clear the background).
//   - Tight recognition: only the two background colours internal/diff emits
//     (256-colour 22/52) count as diff rows. Arbitrary inline spans with
//     other backgrounds are never padded — the painter must not second-guess
//     styling it does not own. ```diff fences are chroma-highlighted
//     foreground-only (monokai emits 38;5;148/197/231), so there is nothing
//     to pad there; colorize full (glamour) strips diff backgrounds entirely —
//     a pre-existing gap tracked separately, not regressed here.
//   - Wrap continuations: ansi.Wrap splits a long +/- line mid-background, and
//     wrapRows turns each resulting row into a self-contained one (the
//     background is re-opened at its start and closed at its end). Every row
//     of the line is therefore classified and padded on its own, the same
//     close-then-reopen way, with no state carried between rows.

const (
	// diffBgGreen/diffBgRed are the backgrounds internal/diff's ansiGreen/
	// ansiRed set (diff.go), as the SGR parameters sgr.State.BG reports.
	// Recognition matches the colour values, never the sequence bytes, because
	// the emission is a combined fg+bg SGR: ESC[38;5;119;48;5;22m.
	diffBgGreen = "48;5;22"
	diffBgRed   = "48;5;52"
)

func isDiffBackground(bg string) bool { return bg == diffBgGreen || bg == diffBgRed }

// tintDiffSurface makes the diff backgrounds full-row ones: every row of the
// already-wrapped render that carries an internal/diff background is padded
// to width cells, with the pad inside the background (the colour spans the
// row, not just the glyphs). Rows without a diff colour are left byte-
// identical. Width-dependent, so — like the think painter — it runs on the
// returned render only and the colorize cache keeps the untinted wrap.
func tintDiffSurface(content string, width int) string {
	if width <= 0 {
		return content
	}
	if !strings.Contains(content, diffBgGreen) && !strings.Contains(content, diffBgRed) {
		return content // neither diff background present — nothing to do
	}
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		bg := sgr.LastBackground(line, isDiffBackground)
		if bg == "" {
			continue
		}
		w := ansi.StringWidth(line)
		if w >= width {
			continue // full-width already — nothing to pad
		}
		// Append — never rewrite — so the emission's reset order stays as-is,
		// then reopen the row's colour just for the pad and close again, so
		// nothing after the row can inherit the background.
		lines[i] = line + "\x1b[" + bg + "m" + strings.Repeat(" ", width-w) + ansiReset
	}
	return strings.Join(lines, "\n")
}

// tintFullRows applies the full-row painters to an already-wrapped render:
// the platform-annotation tag first (dimMilkAnnotationRows — plain echoed
// [milk]/legacy manifest lines get the standard dim tag), then the thinking
// surface (ADR-0067), then the diff backgrounds (issue #273). All three
// recognise their rows locally, leave every other row untouched, and are
// render-only — so the single call-site convention holds: applied to the
// returned render, never to the cached wrap.
func tintFullRows(content string, width int) string {
	content = dimMilkAnnotationRows(content)
	if th.modern {
		content = modernToolRows(content)
	}
	return tintDiffSurface(tintThinkSurface(content, width), width)
}
