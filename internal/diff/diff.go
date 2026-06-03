// Package diff provides inline unified-diff rendering for file edits.
package diff

import (
	"fmt"
	"os"
	"strings"
)

const (
	ansiRed   = "\033[38;5;203;48;5;52m" // bright red fg + dark red bg (single SGR sequence)
	ansiGreen = "\033[38;5;119;48;5;22m" // bright green fg + dark green bg (single SGR sequence)
	ansiDim   = "\033[2m"
	ansiReset = "\033[0m"
)

// ForEdit returns a colored inline diff for an edit_file operation.
// oldStr and newStr are the exact strings being replaced.
// context is the number of surrounding lines to include.
// Returns "" when oldStr is empty or not found in the file.
func ForEdit(path, oldStr, newStr string, context int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return forStrings(path, string(data), oldStr, newStr, context)
}

// ForWrite returns a colored inline diff for a write_file operation.
// newContent is the full file content being written.
// context is the number of surrounding lines to include per hunk.
// Returns "" when the file doesn't exist yet (new file) or is identical.
func ForWrite(path, newContent string, context int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		// New file — show first `context` lines as additions.
		lines := strings.Split(strings.TrimRight(newContent, "\n"), "\n")
		if len(lines) == 0 {
			return ""
		}
		end := len(lines)
		if end > context*2 {
			end = context * 2
		}
		var b strings.Builder
		fmt.Fprintf(&b, ansiDim+"--- %s (new file)"+ansiReset+"\n", path)
		for _, l := range lines[:end] {
			fmt.Fprintf(&b, ansiGreen+"+%s"+ansiReset+"\n", l)
		}
		if len(lines) > end {
			fmt.Fprintf(&b, ansiDim+"  [+%d lines]"+ansiReset+"\n", len(lines)-end)
		}
		return b.String()
	}
	return forStrings(path, string(data), string(data), newContent, context)
}

// forStrings computes a diff between oldContent and newContent using oldStr
// as the anchor to locate the changed region in oldContent.
// When oldStr == oldContent (write_file), the full file is diffed.
func forStrings(path, oldContent, oldStr, newStr string, context int) string {
	if oldStr == newStr {
		return ""
	}

	oldLines := splitLines(oldContent)
	newContent := strings.Replace(oldContent, oldStr, newStr, 1)
	newLines := splitLines(newContent)

	hunks := computeHunks(oldLines, newLines, context)
	if len(hunks) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, ansiDim+"--- %s"+ansiReset+"\n", path)
	for _, h := range hunks {
		renderHunk(&b, h)
	}
	return b.String()
}

type line struct {
	text string
	kind int // -1 del, 0 ctx, +1 add
	oldN int
	newN int
}

type hunk struct {
	lines []line
}

func computeHunks(oldLines, newLines []string, ctx int) []hunk {
	// Myers LCS-based diff on lines.
	ops := lcs(oldLines, newLines)

	// Collect raw lines with kinds.
	type rawLine struct {
		text string
		kind int // -1 del, 0 ctx, +1 add
		oln  int
		nln  int
	}
	var raw []rawLine
	oi, ni := 0, 0
	for _, op := range ops {
		switch op {
		case '=':
			raw = append(raw, rawLine{oldLines[oi], 0, oi + 1, ni + 1})
			oi++
			ni++
		case '-':
			raw = append(raw, rawLine{oldLines[oi], -1, oi + 1, 0})
			oi++
		case '+':
			raw = append(raw, rawLine{newLines[ni], 1, 0, ni + 1})
			ni++
		}
	}

	// Group into hunks: regions of changes ± ctx context lines.
	type span struct{ start, end int }
	var spans []span
	for i, r := range raw {
		if r.kind != 0 {
			s := i - ctx
			if s < 0 {
				s = 0
			}
			e := i + ctx + 1
			if e > len(raw) {
				e = len(raw)
			}
			if len(spans) > 0 && s <= spans[len(spans)-1].end {
				spans[len(spans)-1].end = e
			} else {
				spans = append(spans, span{s, e})
			}
		}
	}

	var hunks []hunk
	for _, sp := range spans {
		var h hunk
		for _, r := range raw[sp.start:sp.end] {
			h.lines = append(h.lines, line{r.text, r.kind, r.oln, r.nln})
		}
		hunks = append(hunks, h)
	}
	return hunks
}

func renderHunk(b *strings.Builder, h hunk) {
	for _, l := range h.lines {
		switch l.kind {
		case -1:
			fmt.Fprintf(b, ansiRed+"-%s"+ansiReset+"\n", l.text)
		case +1:
			fmt.Fprintf(b, ansiGreen+"+%s"+ansiReset+"\n", l.text)
		default:
			fmt.Fprintf(b, ansiDim+" %s"+ansiReset+"\n", l.text)
		}
	}
}

func splitLines(s string) []string {
	lines := strings.Split(s, "\n")
	// Drop the trailing empty element from a final newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lcs returns an edit script as a []byte of '=', '-', '+' operations using
// the Myers diff algorithm (O(ND) space-optimised).
func lcs(a, b []string) []byte {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	if n == 0 {
		ops := make([]byte, m)
		for i := range ops {
			ops[i] = '+'
		}
		return ops
	}
	if m == 0 {
		ops := make([]byte, n)
		for i := range ops {
			ops[i] = '-'
		}
		return ops
	}

	max := n + m
	// v maps diagonal k → furthest x reached.
	v := make([]int, 2*max+1)
	// trace records v snapshots per d.
	trace := make([][]int, 0, max+1)

	for d := 0; d <= max; d++ {
		snap := make([]int, 2*max+1)
		copy(snap, v)
		trace = append(trace, snap)

		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[k-1+max] < v[k+1+max]) {
				x = v[k+1+max]
			} else {
				x = v[k-1+max] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[k+max] = x
			if x >= n && y >= m {
				// Found — backtrack.
				return backtrack(trace, a, b, max)
			}
		}
	}
	// Fallback: replace all.
	var ops []byte
	for range a {
		ops = append(ops, '-')
	}
	for range b {
		ops = append(ops, '+')
	}
	return ops
}

func backtrack(trace [][]int, a, b []string, max int) []byte {
	x, y := len(a), len(b)
	var ops []byte
	for d := len(trace) - 1; d > 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[k-1+max] < v[k+1+max]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[prevK+max]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			ops = append(ops, '=')
			x--
			y--
		}
		if d > 0 {
			if x == prevX {
				ops = append(ops, '+')
				y--
			} else {
				ops = append(ops, '-')
				x--
			}
		}
	}
	for x > 0 && y > 0 {
		ops = append(ops, '=')
		x--
		y--
	}
	// Reverse.
	for i, j := 0, len(ops)-1; i < j; i, j = i+1, j-1 {
		ops[i], ops[j] = ops[j], ops[i]
	}
	return ops
}
