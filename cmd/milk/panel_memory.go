package main

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/scoutme/milk/internal/memory"
)

// sessionBricks holds the summary-brick fields from the active session for
// display in the memory panel.
type sessionBricks struct {
	currentNeed           string
	lastLocalSummary      string
	lastEscalationSummary string
	escalationBrief       string
	primaryName           string // configured name of the primary agent (used as brick label)
	escalationName        string // configured name of the escalation agent (used as brick label)
}

const recentThreshold = 60 * time.Second

var (
	stylePanelTitle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#555", Dark: "#888"}).
			Bold(true)

	stylePanelSection = lipgloss.NewStyle().
				Foreground(lipgloss.AdaptiveColor{Light: "#777", Dark: "#666"})
)

// renderMemoryPanel returns a vertical panel string of exactly h lines and
// memoryPanelInner columns (32 chars; scrollbar is rendered separately via renderPanelScrollbar).
func (m *model) renderMemoryPanel(h int) string {
	inner := memoryPanelInner
	if !isTTY {
		return strings.Repeat("\n", h)
	}

	bricks := sessionBricks{
		currentNeed:           m.st.sess.CurrentNeed,
		lastLocalSummary:      m.st.sess.LastLocalSummary,
		lastEscalationSummary: m.st.sess.LastEscalationSummary,
		escalationBrief:       m.st.sess.EscalationBrief,
		primaryName:           m.st.cfg.ActiveAgent().Name,
		escalationName:        m.st.cfg.EscalationAgentConfig().Name,
	}
	all := buildPanelLines(m.mem, inner, bricks)
	total := len(all)

	// Clamp offset so we never scroll past the last screenful.
	maxOffset := max(total-h, 0)
	if m.panelOffset > maxOffset {
		m.panelOffset = maxOffset
	}

	// Slice the visible window.
	lines := all[m.panelOffset:]
	for len(lines) < h {
		lines = append(lines, "")
	}
	lines = lines[:h]

	// Pad each row to exactly inner cols (no scrollbar — that's a separate column).
	var rows []string
	for _, line := range lines {
		lineW := utf8.RuneCountInString(stripANSI(line))
		if lineW < inner {
			line += strings.Repeat(" ", inner-lineW)
		}
		rows = append(rows, line)
	}
	return strings.Join(rows, "\n")
}

// renderPanelScrollbar returns a 1-column string of h lines: a dim │ track with
// a ▌ thumb when the panel content overflows, or a blank column otherwise.
func (m *model) renderPanelScrollbar(h int) string {
	bricks := sessionBricks{
		currentNeed:           m.st.sess.CurrentNeed,
		lastLocalSummary:      m.st.sess.LastLocalSummary,
		lastEscalationSummary: m.st.sess.LastEscalationSummary,
		escalationBrief:       m.st.sess.EscalationBrief,
		primaryName:           m.st.cfg.ActiveAgent().Name,
		escalationName:        m.st.cfg.EscalationAgentConfig().Name,
	}
	all := buildPanelLines(m.mem, memoryPanelInner, bricks)
	total := len(all)
	needsBar := total > h

	var rows []string
	if !needsBar {
		for range h {
			rows = append(rows, " ")
		}
		return strings.Join(rows, "\n")
	}

	thumbTop, thumbBot := scrollThumb(h, total, m.panelOffset)
	for i := range h {
		if i >= thumbTop && i <= thumbBot {
			rows = append(rows, dim("▌"))
		} else {
			rows = append(rows, dim("│"))
		}
	}
	return strings.Join(rows, "\n")
}

// scrollThumb computes the inclusive [top, bot] row indices of the scroll thumb
// within a viewport of height h showing content of length total starting at offset.
func scrollThumb(h, total, offset int) (top, bot int) {
	thumbH := max(h*h/total, 1)
	top = offset * (h - thumbH) / (total - h)
	bot = top + thumbH - 1
	if bot >= h {
		bot = h - 1
	}
	return top, bot
}

func buildPanelLines(mem *memory.Store, inner int, bricks sessionBricks) []string {
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

	// --- CONTEXT BRICKS ---
	primaryLabel := bricks.primaryName
	if primaryLabel == "" {
		primaryLabel = "primary"
	}
	escalationLabel := bricks.escalationName
	if escalationLabel == "" {
		escalationLabel = "escalation"
	}
	addLine("")
	addLine(stylePanelSection.Render("CONTEXT BRICKS"))
	addBrickLines(&lines, "need", bricks.currentNeed, inner)
	addBrickLines(&lines, primaryLabel, bricks.lastLocalSummary, inner)
	addBrickLines(&lines, escalationLabel, bricks.lastEscalationSummary, inner)
	addBrickLines(&lines, "brief", bricks.escalationBrief, inner)

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
	if badge := consumerBadge(p); badge != "" {
		wStr = badge + " " + wStr
	}

	// Visual widths (ANSI chars do not affect display width)
	bulletW := utf8.RuneCountInString(bullet)
	idW := utf8.RuneCountInString(shortID)
	// First line: bullet + id + content + pad + " " + wStr
	firstW := max(inner-bulletW-idW-1-len(wStr), 8)
	contW := max(inner-2, 8) // "  " continuation indent

	wrapped := wordWrap(p.Content, firstW, contW, 2)

	for i, line := range wrapped {
		var out string
		if i == 0 {
			textPad := max(firstW-utf8.RuneCountInString(line), 0)
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

// addBrickLines appends lines for a single summary brick: a label line and up
// to 3 wrapped content lines, or "(empty)" when value is blank.
func addBrickLines(lines *[]string, label, value string, inner int) {
	labelStr := dim("  " + label + ": ")
	labelW := utf8.RuneCountInString("  " + label + ": ")
	if value == "" {
		*lines = append(*lines, labelStr+dim("—"))
		return
	}
	firstW := max(inner-labelW, 8)
	contW := max(inner-4, 8)
	wrapped := wordWrap(value, firstW, contW, 3)
	for i, line := range wrapped {
		if i == 0 {
			*lines = append(*lines, labelStr+line)
		} else {
			*lines = append(*lines, "    "+line)
		}
	}
}

// consumerBadge returns a short display badge for non-all consumers: "[P]" for
// primary-only percepts and "[E]" for escalation-only. Returns "" for ConsumerAll.
func consumerBadge(p memory.Percept) string {
	switch p.Consumer {
	case memory.ConsumerLocal:
		return dim("[P]")
	case memory.ConsumerEscalation:
		return dim("[E]")
	}
	return ""
}

// buildPanelLineIDs returns one percept ID per line in the same order as
// buildPanelLines. Non-percept lines (titles, section headers, blank lines)
// get an empty string.
func buildPanelLineIDs(mem *memory.Store, bricks sessionBricks) []string {
	var ids []string
	add := func(id string) { ids = append(ids, id) }

	add("") // title
	add("") // blank

	if mem == nil {
		add("")
		return ids
	}

	addPercept := func(p memory.Percept) {
		// Mirror addPerceptLines width calculation so line counts match exactly.
		const inner = memoryPanelInner
		bulletW := utf8.RuneCountInString("• ") // same for both bullet types
		idW := utf8.RuneCountInString(perceptIDShort(p) + " ")
		wStr := fmt.Sprintf("%.2f", p.W)
		if badge := consumerBadge(p); badge != "" {
			wStr = badge + " " + wStr
		}
		firstW := max(inner-bulletW-idW-1-len(wStr), 8)
		contW := max(inner-2, 8)
		wrapped := wordWrap(p.Content, firstW, contW, 2)
		for range wrapped {
			add(p.ID)
		}
	}

	addBrick := func(label, value string) {
		const inner = memoryPanelInner
		labelW := utf8.RuneCountInString("  " + label + ": ")
		if value == "" {
			add("") // label + "—" is one line, not clickable
			return
		}
		firstW := max(inner-labelW, 8)
		contW := max(inner-4, 8)
		wrapped := wordWrap(value, firstW, contW, 3)
		for range wrapped {
			add(label) // static brick ID makes lines clickable
		}
	}

	sessionPercepts := mem.List(memory.ListOpts{Scope: "session"})
	add("") // SESSION header
	if len(sessionPercepts) == 0 {
		add("")
	} else {
		for _, p := range sessionPercepts {
			addPercept(p)
		}
	}
	add("") // blank

	allGlobal := mem.List(memory.ListOpts{Scope: "global"})
	var corePercepts, normalPercepts []memory.Percept
	for _, p := range allGlobal {
		if p.Core {
			corePercepts = append(corePercepts, p)
		} else {
			normalPercepts = append(normalPercepts, p)
		}
	}

	add("") // GLOBAL header
	if len(normalPercepts) == 0 {
		add("")
	} else {
		for _, p := range normalPercepts {
			addPercept(p)
		}
	}
	add("") // blank

	add("") // GLOBAL (core) header
	if len(corePercepts) == 0 {
		add("")
	} else {
		for _, p := range corePercepts {
			addPercept(p)
		}
	}

	// CONTEXT BRICKS section
	primaryLabel := bricks.primaryName
	if primaryLabel == "" {
		primaryLabel = "primary"
	}
	escalationLabel := bricks.escalationName
	if escalationLabel == "" {
		escalationLabel = "escalation"
	}
	add("") // blank
	add("") // CONTEXT BRICKS header
	addBrick("need", bricks.currentNeed)
	addBrick(primaryLabel, bricks.lastLocalSummary)
	addBrick(escalationLabel, bricks.lastEscalationSummary)
	addBrick("brief", bricks.escalationBrief)

	return ids
}

// brickContent returns the full text for a brick ID, or "" if unknown/empty.
func brickContent(id string, bricks sessionBricks) string {
	primaryLabel := bricks.primaryName
	if primaryLabel == "" {
		primaryLabel = "primary"
	}
	escalationLabel := bricks.escalationName
	if escalationLabel == "" {
		escalationLabel = "escalation"
	}
	switch id {
	case "need":
		return bricks.currentNeed
	case primaryLabel:
		return bricks.lastLocalSummary
	case escalationLabel:
		return bricks.lastEscalationSummary
	case "brief":
		return bricks.escalationBrief
	}
	return ""
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
