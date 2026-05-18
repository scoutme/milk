package main

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/scoutme/milk/internal/memory"
)

const recentThreshold = 60 * time.Second

var (
	stylePanelBorder = lipgloss.NewStyle().
				BorderStyle(lipgloss.NormalBorder()).
				BorderLeft(true).
				BorderForeground(lipgloss.AdaptiveColor{Light: "#AAA", Dark: "#555"})

	stylePanelTitle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#555", Dark: "#888"}).
			Bold(true)

	stylePanelSection = lipgloss.NewStyle().
				Foreground(lipgloss.AdaptiveColor{Light: "#777", Dark: "#666"})
)

// renderMemoryPanel returns a vertical panel string of exactly h lines and
// memoryPanelWidth columns (including left border). When mem is nil or the
// panel height is too small it returns a blank panel.
func (m *model) renderMemoryPanel(h int) string {
	inner := memoryPanelInner
	if !isTTY {
		return strings.Repeat("\n", h)
	}

	lines := buildPanelLines(m.mem, inner, h)

	// Pad or trim to exactly h lines.
	for len(lines) < h {
		lines = append(lines, "")
	}
	lines = lines[:h]

	// Join, then apply left-border style at full panel width.
	content := strings.Join(lines, "\n")
	return stylePanelBorder.Width(inner).Height(h).Render(content)
}

func buildPanelLines(mem *memory.Store, inner, maxLines int) []string {
	var lines []string

	addLine := func(s string) {
		lines = append(lines, s)
	}

	// Title
	addLine(stylePanelTitle.Render(truncate(" memory", inner)))
	addLine("")

	if mem == nil {
		addLine(dim("(unavailable)"))
		return lines
	}

	now := time.Now()

	// --- SESSION ---
	sessionPercepts := mem.List(memory.ListOpts{Scope: "session"})
	addLine(stylePanelSection.Render("SESSION"))
	if len(sessionPercepts) == 0 {
		addLine(dim("  (empty)"))
	} else {
		for _, p := range sessionPercepts {
			addPerceptLines(&lines, p, inner, now)
		}
	}
	addLine("")

	// --- GLOBAL / GLOBAL (core) ---
	allGlobal := mem.List(memory.ListOpts{Scope: "global"})
	var corePercepts, normalPercepts []memory.Percept
	for _, p := range allGlobal {
		if p.Core {
			corePercepts = append(corePercepts, p)
		} else {
			normalPercepts = append(normalPercepts, p)
		}
	}

	addLine(stylePanelSection.Render("GLOBAL"))
	if len(normalPercepts) == 0 {
		addLine(dim("  (empty)"))
	} else {
		for _, p := range normalPercepts {
			addPerceptLines(&lines, p, inner, now)
		}
	}
	addLine("")

	addLine(stylePanelSection.Render("GLOBAL (core)"))
	if len(corePercepts) == 0 {
		addLine(dim("  (empty)"))
	} else {
		for _, p := range corePercepts {
			addPerceptLines(&lines, p, inner, now)
		}
	}

	return lines
}

// addPerceptLines appends 1–3 lines for a single percept:
// content wrapped to max 2 lines (then "…"), weight on the first line right-aligned.
// perceptIDShort returns the first 6 hex chars of p.ID prefixed with "#".
func perceptIDShort(p memory.Percept) string {
	if len(p.ID) >= 6 {
		return "#" + p.ID[:6]
	}
	return "#" + p.ID
}

func addPerceptLines(lines *[]string, p memory.Percept, inner int, now time.Time) {
	recent := now.Sub(p.UpdatedAt) < recentThreshold
	bullet := "• "
	if p.Core {
		bullet = "★ "
	}
	shortID := perceptIDShort(p) + " " // e.g. "#a3f2c1 "
	wStr := fmt.Sprintf("%.2f", p.W)

	// Visual widths (ANSI chars do not affect display width)
	bulletW := utf8.RuneCountInString(bullet)
	idW := utf8.RuneCountInString(shortID)
	// First line: bullet + id + content + pad + " " + wStr
	firstW := inner - bulletW - idW - 1 - len(wStr)
	if firstW < 8 {
		firstW = 8
	}
	contW := inner - 2 // "  " continuation indent
	if contW < 8 {
		contW = 8
	}

	wrapped := wordWrap(p.Content, firstW, contW, 2)

	for i, line := range wrapped {
		var out string
		if i == 0 {
			textPad := firstW - utf8.RuneCountInString(line)
			if textPad < 0 {
				textPad = 0
			}
			idPart := dim(shortID)
			raw := bullet + idPart + line + strings.Repeat(" ", textPad) + " " + wStr
			if recent {
				// Re-colorize: keep id dim but highlight the rest bold+yellow
				raw = bullet + idPart + colorize(line+strings.Repeat(" ", textPad)+" "+wStr, "\033[1;33m")
			}
			out = raw
		} else {
			// Continuation lines: indent only (no bullet/star/id)
			raw := "  " + line
			if recent {
				out = colorize(raw, "\033[1;33m")
			} else {
				out = raw
			}
		}
		*lines = append(*lines, out)
	}
}

// wordWrap splits text into at most maxLines lines. The first line has width
// firstW, continuation lines have width contW. If text overflows, the last
// line ends with "…".
func wordWrap(text string, firstW, contW, maxLines int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}

	var result []string
	var cur strings.Builder
	lineW := firstW

	flush := func() {
		result = append(result, cur.String())
		cur.Reset()
		lineW = contW
	}

	for _, w := range words {
		wlen := utf8.RuneCountInString(w)
		if cur.Len() == 0 {
			if len(result) == maxLines-1 {
				// Last allowed line — check if it will overflow
				if wlen > lineW {
					w = truncate(w, lineW-1) + "…"
				}
			}
			cur.WriteString(w)
		} else {
			if 1+wlen <= lineW-cur.Len() {
				cur.WriteByte(' ')
				cur.WriteString(w)
			} else {
				// Overflow — start new line if room
				if len(result) < maxLines-1 {
					flush()
					cur.WriteString(w)
				} else {
					// No more lines — truncate current line to fit + ellipsis, then stop
					cur.WriteString(" " + w)
					s := truncate(cur.String(), lineW-1) + "…"
					cur.Reset()
					cur.WriteString(s)
					break
				}
			}
		}
	}
	if cur.Len() > 0 {
		result = append(result, cur.String())
	}
	return result
}

// truncate cuts s to at most n runes.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
