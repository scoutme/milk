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
	needHistory           []string // all needs expressed this session, oldest first
	lastLocalSummary      string
	lastEscalationSummary string
	escalationBrief       string
	primaryName           string // configured name of the primary agent (used as brick label)
	escalationName        string // configured name of the escalation agent (used as brick label)
	needStale             bool   // need changed since last escalation → will trigger fresh-start
	contextStale          bool   // local turn gap exceeded threshold → will trigger fresh-start
	localTurnsSince       int    // local assistant turns since last escalation (0 if never escalated)
	freshThreshold        int    // threshold at which contextStale fires
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

	bricks := m.currentSessionBricks()
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

// currentSessionBricks builds a sessionBricks snapshot from the current model state,
// including staleness flags derived from the active config and session.
func (m *model) currentSessionBricks() sessionBricks {
	escAC := m.st.cfg.EscalationAgentConfig()
	threshold := m.st.cfg.AgentReturningFreshStartLocalTurns(escAC)
	turnsSince := m.st.sess.LocalTurnsSinceLastEscalation()
	return sessionBricks{
		currentNeed:           m.st.sess.CurrentNeed,
		needHistory:           m.st.sess.NeedHistory,
		lastLocalSummary:      m.st.sess.LastLocalSummary,
		lastEscalationSummary: m.st.sess.LastEscalationSummary,
		escalationBrief:       m.st.sess.EscalationBrief,
		primaryName:           m.st.cfg.ActiveAgent().Name,
		escalationName:        escAC.Name,
		needStale:             m.st.sess.NeedChangedSinceLastEscalation(),
		contextStale:          threshold > 0 && turnsSince >= threshold,
		localTurnsSince:       turnsSince,
		freshThreshold:        threshold,
	}
}

// renderPanelScrollbar returns a 1-column string of h lines: a dim │ track with
// a ▌ thumb when the panel content overflows, or a blank column otherwise.
func (m *model) renderPanelScrollbar(h int) string {
	bricks := m.currentSessionBricks()
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
	// Compute staleness ratio for gradient colouring of escalation bricks.
	// ratio 0=fresh, 1=fully stale; shown as a colour gradient on the content text.
	var escRatio float64
	if bricks.contextStale {
		escRatio = 1
	} else if bricks.freshThreshold > 0 && bricks.localTurnsSince > 0 {
		escRatio = float64(bricks.localTurnsSince) / float64(bricks.freshThreshold)
	}
	escStale := bricks.contextStale
	addLine(stylePanelSection.Render("CONTEXT BRICKS"))
	addBrickLines(&lines, "need", bricks.currentNeed, inner, bricks.needStale, 0)
	// Show prior needs (all except the current one) dimmed below the current need.
	if len(bricks.needHistory) > 1 {
		prior := bricks.needHistory[:len(bricks.needHistory)-1]
		for i := len(prior) - 1; i >= 0; i-- {
			addBrickLines(&lines, "", dim(prior[i]), inner, false, 0)
		}
	}
	addBrickLines(&lines, primaryLabel, bricks.lastLocalSummary, inner, false, 0)
	addBrickLines(&lines, escalationLabel, bricks.lastEscalationSummary, inner, escStale, escRatio)
	addBrickLines(&lines, "brief", bricks.escalationBrief, inner, escStale, escRatio)
	addLine("")
	addLine(stalenessLegend(inner))

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

	// Badge (consumer hint) still shown when present, but no numeric weight.
	badge := consumerBadge(p)

	// Visual widths (ANSI chars do not affect display width)
	bulletW := utf8.RuneCountInString(bullet)
	idW := utf8.RuneCountInString(shortID)
	badgeW := 0
	if badge != "" {
		// badge is rendered dim, so strip ANSI for width calc
		badgeW = utf8.RuneCountInString(" [P]") // "[E]" same width
	}
	// First line: bullet + id + content (no weight string)
	firstW := max(inner-bulletW-idW-badgeW, 8)
	contW := max(inner-2, 8) // "  " continuation indent

	wrapped := wordWrap(p.Content, firstW, contW, 2)

	// Staleness gradient: ratio = 1 - W (high weight = fresh/bright, low = stale/orange)
	ratio := 1.0 - p.W
	tint := staleContentColor(ratio)

	for i, line := range wrapped {
		var out string
		idPart := dim(shortID)
		if i == 0 {
			if recent {
				// Recent highlight takes priority over gradient
				suffix := ""
				if badge != "" {
					suffix = " " + badge
				}
				out = bullet + idPart + colorize(line+suffix, "\033[1;33m")
			} else {
				suffix := ""
				if badge != "" {
					suffix = " " + badge
				}
				out = bullet + idPart + colorize(line+suffix, tint)
			}
		} else {
			// Continuation lines: indent only (no bullet/star/id)
			if recent {
				out = colorize("  "+line, "\033[1;33m")
			} else {
				out = colorize("  "+line, tint)
			}
		}
		*lines = append(*lines, out)
	}
}

// addBrickLines appends lines for a single summary brick.
// ratio in [0,1] drives a staleness gradient on the content text; ratio=0 is
// neutral, ratio=1 is fully stale. stale=true also appends a dim "(stale)" label.
func addBrickLines(lines *[]string, label, value string, inner int, stale bool, ratio float64) {
	suffix := ""
	if stale {
		suffix = "(stale)"
	}
	dimSuffix := ""
	suffixW := 0
	if suffix != "" {
		dimSuffix = dim(suffix + " ")
		suffixW = utf8.RuneCountInString(suffix + " ")
	}
	labelStr := dim("  "+label+": ") + dimSuffix
	labelW := utf8.RuneCountInString("  "+label+": ") + suffixW
	if value == "" {
		// Don't show staleness annotation when content is absent (dash placeholder).
		*lines = append(*lines, dim("  "+label+": ")+dim("—"))
		return
	}
	firstW := max(inner-labelW, 8)
	contW := max(inner-4, 8)
	wrapped := wordWrap(value, firstW, contW, 3)
	tint := staleContentColor(ratio)
	renderLine := func(s string) string { return colorize(s, tint) }
	for i, line := range wrapped {
		if i == 0 {
			*lines = append(*lines, labelStr+renderLine(line))
		} else {
			*lines = append(*lines, "    "+renderLine(line))
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
	// Prior needs: one addBrick call per entry so line counts match buildPanelLines.
	if len(bricks.needHistory) > 1 {
		prior := bricks.needHistory[:len(bricks.needHistory)-1]
		for i := len(prior) - 1; i >= 0; i-- {
			addBrick("", prior[i])
		}
	}
	addBrick(primaryLabel, bricks.lastLocalSummary)
	addBrick(escalationLabel, bricks.lastEscalationSummary)
	addBrick("brief", bricks.escalationBrief)
	add("") // blank before legend
	add("") // staleness legend line

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
		if len(bricks.needHistory) <= 1 {
			return bricks.currentNeed
		}
		var b strings.Builder
		b.WriteString("current: " + bricks.currentNeed)
		b.WriteString("\nhistory:")
		for i := len(bricks.needHistory) - 2; i >= 0; i-- {
			b.WriteString("\n  " + bricks.needHistory[i])
		}
		return b.String()
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
