package main

import (
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	rw "github.com/mattn/go-runewidth"
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
	needStale             bool   // unused in panel — kept for future use
	contextStale          bool   // claude-cli only: turn gap exceeded threshold → will trigger fresh-start (non-CLI runners keep history — ADR-0032 update 2026-10-29)
	localTurnsSince       int    // local assistant turns since last escalation (0 if never escalated)
	freshThreshold        int    // threshold at which contextStale fires
}

const recentThreshold = 60 * time.Second

// Panel title/section styles are filled by rebuildStyles (theme.go).
// styleChange* are the files-panel changeset styles (issue #331): per change
// kind, both skins; the zero value renders plain.
var (
	stylePanelTitle   lipgloss.Style
	stylePanelSection lipgloss.Style

	styleChangeCreated  lipgloss.Style
	styleChangeModified lipgloss.Style
	styleChangeDeleted  lipgloss.Style
	styleChangeRenamed  lipgloss.Style
)

// renderMemoryPanel returns a vertical panel string of exactly h lines and
// memoryPanelInner columns (scrollbar is rendered separately via renderPanelScrollbar).
// Body shared with every other side panel — see renderSidePanel.
func (m *model) renderMemoryPanel(h int) string {
	return m.renderSidePanel(regionMemory, h)
}

// currentSessionBricks builds a sessionBricks snapshot from the current model state,
// including staleness flags derived from the active config and session.
func (m *model) currentSessionBricks() sessionBricks {
	escAC := m.st.cfg.EscalationAgentConfig()
	threshold := m.st.cfg.AgentReturningFreshStartLocalTurns(escAC)
	turnsSince := m.st.sess.LocalTurnsSinceLastEscalation()
	return sessionBricks{
		currentNeed:           m.st.sess.CurrentNeed,
		lastLocalSummary:      m.st.sess.LastLocalSummary,
		lastEscalationSummary: m.st.sess.LastEscalationSummary,
		escalationBrief:       m.st.sess.EscalationBrief,
		primaryName:           m.st.cfg.ActiveAgent().Name,
		escalationName:        escAC.Name,
		needStale:             m.st.sess.NeedChangedSinceLastEscalation(),
		contextStale:          escAC.IsCLI() && threshold > 0 && turnsSince >= threshold,
		localTurnsSince:       turnsSince,
		freshThreshold:        threshold,
	}
}

// renderPanelScrollbar returns a 1-column string of h lines: a dim │ track with
// a ▌ thumb when the panel content overflows, or a blank column otherwise.
// Shared with every other side panel — see renderSidePanelScrollbar.
func (m *model) renderPanelScrollbar(h int) string {
	return m.renderSidePanelScrollbar(regionMemory, h)
}

func buildPanelLines(mem *memory.Store, inner int, bricks sessionBricks) []string {
	lines, _ := memoryPanelRows(mem, inner, bricks, "")
	return lines
}

// Rendering and hit testing share rows so expanding text cannot shift click targets.
func memoryPanelRows(mem *memory.Store, inner int, bricks sessionBricks, expanded string) ([]string, []string) {
	var lines []string
	var ids []string

	addLine := func(s string) {
		lines = append(lines, s)
		ids = append(ids, "")
	}
	addBrick := func(label, value string, stale bool, ratio float64) {
		if strings.TrimSpace(value) == "" {
			return
		}
		start := len(lines)
		limit := 3
		if expanded == label {
			limit = utf8.RuneCountInString(value) + 1
		}
		addBrickLines(&lines, label, value, inner, stale, ratio, limit)
		for range lines[start:] {
			ids = append(ids, label)
		}
	}
	addSection := func(label string, percepts []memory.Percept) {
		if len(percepts) == 0 {
			addLine(dim(label + " (0)"))
			return
		}
		addLine(stylePanelSection.Render(label))
		for _, p := range percepts {
			start := len(lines)
			limit := 3
			if expanded == p.ID {
				limit = utf8.RuneCountInString(p.Content) + 1
			}
			addPerceptLines(&lines, p, inner, time.Now(), limit)
			for range lines[start:] {
				ids = append(ids, p.ID)
			}
		}
		addLine("")
	}

	// Title
	addLine(panelTitleLine(" memory", regionMemory, inner))
	addLine(dim("F1 close · double-click expand"))

	if mem == nil {
		addLine(dim("(unavailable)"))
		return lines, ids
	}

	primaryLabel := bricks.primaryName
	if primaryLabel == "" {
		primaryLabel = "primary"
	}
	escalationLabel := bricks.escalationName
	if escalationLabel == "" {
		escalationLabel = "escalation"
	}
	// Compute staleness ratio for gradient colouring of escalation bricks.
	// ratio 0=fresh, 1=fully stale; shown as a colour gradient on the content text.
	var escRatio float64
	if bricks.contextStale {
		escRatio = 1
	} else if bricks.freshThreshold > 0 && bricks.localTurnsSince > 0 {
		escRatio = float64(bricks.localTurnsSince) / float64(bricks.freshThreshold)
	}
	escStale := bricks.contextStale
	if strings.TrimSpace(bricks.currentNeed+bricks.lastLocalSummary+bricks.lastEscalationSummary+bricks.escalationBrief) == "" {
		addLine(dim("CONTEXT BRICKS (0)"))
	} else {
		addLine(stylePanelSection.Render("CONTEXT BRICKS"))
		addBrick("need", bricks.currentNeed, false, 0)
		addBrick(primaryLabel, bricks.lastLocalSummary, false, 0)
		addBrick(escalationLabel, bricks.lastEscalationSummary, escStale, escRatio)
		addBrick("brief", bricks.escalationBrief, escStale, escRatio)
		addLine("")
		addLine(stalenessLegend(inner))
		addLine("")
	}
	// --- SESSION ---
	sessionPercepts := mem.List(memory.ListOpts{Scope: "session"})
	addSection("SESSION", sessionPercepts)

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

	addSection("GLOBAL", normalPercepts)
	addSection("GLOBAL (core)", corePercepts)
	return lines, ids
}

// perceptIDShort returns the first 6 hex chars of p.ID prefixed with "#".
func perceptIDShort(p memory.Percept) string {
	if len(p.ID) >= 6 {
		return "#" + p.ID[:6]
	}
	return "#" + p.ID
}

func addPerceptLines(lines *[]string, p memory.Percept, inner int, now time.Time, limits ...int) {
	recent := now.Sub(p.UpdatedAt) < recentThreshold
	bullet := "• "
	if p.Core {
		bullet = "★ "
	}
	shortID := perceptIDShort(p) + " " // e.g. "#a3f2c1 "

	// Badge (consumer hint) still shown when present, but no numeric weight.
	badge := consumerBadge(p)

	// First line: bullet + id + content (no weight string)
	firstW, contW := perceptWrapWidths(p, inner)

	limit := 3
	if len(limits) > 0 {
		limit = limits[0]
	}
	wrapped := wordWrap(p.Content, firstW, contW, limit)

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
func addBrickLines(lines *[]string, label, value string, inner int, stale bool, ratio float64, limits ...int) {
	suffix := ""
	if stale {
		suffix = "(stale)"
	}
	dimSuffix := ""
	if suffix != "" {
		dimSuffix = dim(suffix + " ")
	}
	labelStr := dim("  "+label+": ") + dimSuffix
	if value == "" {
		// Don't show staleness annotation when content is absent (dash placeholder).
		*lines = append(*lines, dim("  "+label+": ")+dim("—"))
		return
	}
	firstW, contW := brickWrapWidths(label, stale, inner)
	limit := 3
	if len(limits) > 0 {
		limit = limits[0]
	}
	wrapped := wordWrap(value, firstW, contW, limit)
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
	_, ids := memoryPanelRows(mem, memoryPanelInner, bricks, "")
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

// perceptWrapWidths returns the first-line and continuation-line wrap widths
// for a percept's content, in display cells. Shared by addPerceptLines
// (rendering) and buildPanelLineIDs (double-click → ID mapping) so the two can
// never wrap at different points and hand out misaligned IDs (issue #212 —
// they had already drifted apart: the render side no longer draws a weight
// string, the ID side still subtracted one, and the ID side counted the badge
// in ANSI bytes).
func perceptWrapWidths(p memory.Percept, inner int) (firstW, contW int) {
	bulletW := utf8.RuneCountInString("• ") // "★ " renders the same width
	idW := utf8.RuneCountInString(perceptIDShort(p) + " ")
	badgeW := 0
	if consumerBadge(p) != "" {
		badgeW = utf8.RuneCountInString(" [P]") // "[E]" renders the same width
	}
	return max(inner-bulletW-idW-badgeW, 8), max(inner-2, 8)
}

// brickWrapWidths returns the wrap widths for a context brick's value, shared
// by addBrickLines (rendering) and buildPanelLineIDs (click mapping) for the
// same drift-proofing reason as perceptWrapWidths (issue #212): stale=true
// means the rendered first line also carries a "(stale) " suffix, which eats
// into the content budget.
func brickWrapWidths(label string, stale bool, inner int) (firstW, contW int) {
	labelW := utf8.RuneCountInString("  " + label + ": ")
	if stale {
		labelW += utf8.RuneCountInString("(stale) ")
	}
	return max(inner-labelW, 8), max(inner-4, 8)
}

// wordWrap splits text into at most maxLines lines. The first line has width
// firstW, continuation lines have width contW, measured in display cells
// (runewidth — not bytes: see issue #212). A word that cannot fit on a line of
// its own is hard-broken into width-sized chunks instead of being written past
// the line budget; only the last allowed line may be shortened, and it then
// ends with "…".
func wordWrap(text string, firstW, contW, maxLines int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}
	if maxLines < 1 {
		maxLines = 1
	}
	if firstW < 1 {
		firstW = 1
	}
	if contW < 1 {
		contW = 1
	}

	var result []string
	var cur strings.Builder
	curW := 0 // display width of cur
	lineW := firstW

	flush := func() {
		result = append(result, cur.String())
		cur.Reset()
		curW = 0
		lineW = contW
	}
	lastLine := func() bool { return len(result) >= maxLines-1 }

	for _, w := range words {
		for w != "" {
			ww := rw.StringWidth(w)
			switch {
			case curW == 0 && ww <= lineW:
				cur.WriteString(w)
				curW = ww
				w = ""
			case curW == 0 && lastLine():
				// Last allowed line — shorten the word to fit, then stop.
				result = append(result, rw.Truncate(w, lineW, "…"))
				return result
			case curW == 0:
				// Word wider than the line it lands on: hard-break it into
				// line-sized chunks instead of letting it overflow (issue #212).
				chunk, rest := splitAtWidth(w, lineW)
				result = append(result, chunk)
				w = rest
				lineW = contW
			case curW+1+ww <= lineW:
				cur.WriteByte(' ')
				cur.WriteString(w)
				curW += 1 + ww
				w = ""
			case lastLine():
				// No more lines — squeeze the word in, cut the line to fit.
				result = append(result, rw.Truncate(cur.String()+" "+w, lineW, "…"))
				return result
			default:
				flush()
			}
		}
	}
	if curW > 0 {
		result = append(result, cur.String())
	}
	return result
}

// splitAtWidth splits s into a prefix of at most w display cells and the
// remainder. The prefix is never empty for a non-empty s — a single grapheme
// wider than w is taken whole so callers always make progress.
func splitAtWidth(s string, w int) (head, rest string) {
	if w < 1 {
		w = 1
	}
	acc := 0
	for i, r := range s {
		cw := rw.RuneWidth(r)
		if acc+cw > w && i > 0 {
			return s[:i], s[i:]
		}
		acc += cw
	}
	return s, ""
}

// truncate cuts s to at most n runes.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
