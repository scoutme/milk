package main

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/scoutme/milk/internal/agent/local"
)

const backgroundPanelWidth = 33 // 32 inner + 1 right scrollbar column
const backgroundPanelInner = 32

// renderBackgroundPanel returns a vertical panel string of exactly h lines
// and backgroundPanelInner columns (scrollbar rendered separately by
// renderBackgroundPanelScrollbar), listing every spawn_background_agent job
// (ADR-0043) this session has spawned — agent- and user-initiated alike.
// Body shared with every other side panel — see renderSidePanel.
func (m *model) renderBackgroundPanel(h int) string {
	return m.renderSidePanel(regionBackground, h)
}

// renderBackgroundPanelScrollbar returns a 1-column string of h lines: a dim
// │ track with a ▌ thumb when the panel content overflows, or a blank column
// otherwise. Shared with every other side panel — see renderSidePanelScrollbar.
func (m *model) renderBackgroundPanelScrollbar(h int) string {
	return m.renderSidePanelScrollbar(regionBackground, h)
}

func buildBackgroundPanelLines(jobs []local.Job, inner int) []string {
	var lines []string
	addLine := func(s string) { lines = append(lines, s) }

	addLine(panelTitleLine("background agents", regionBackground, inner))
	addLine("")

	if len(jobs) == 0 {
		addLine(dim("(none)"))
		return lines
	}

	for _, j := range jobs {
		addLine(renderBackgroundJobLine(j, inner))
	}
	return lines
}

func renderBackgroundJobLine(j local.Job, inner int) string {
	badge := backgroundJobStatusBadge(j.Status)
	elapsed := backgroundJobElapsed(j)
	suffix := fmt.Sprintf(" %s", dim(elapsed))
	suffixW := utf8.RuneCountInString(elapsed) + 1
	prefix := fmt.Sprintf("  %s ", badge)
	prefixW := utf8.RuneCountInString(stripANSI(prefix))
	maxLabelW := inner - prefixW - suffixW
	if maxLabelW < 4 {
		maxLabelW = 4
	}
	label := j.Label
	labelRunes := []rune(label)
	if len(labelRunes) > maxLabelW {
		label = string(labelRunes[:maxLabelW-1]) + "…"
	}
	line := prefix + label
	lineW := utf8.RuneCountInString(stripANSI(line))
	// suffix (the elapsed badge) already starts with its own leading space,
	// so pad may legitimately be 0 when the label fills its entire budget —
	// only clamp the negative case (shouldn't happen given maxLabelW above,
	// but truncation math is fiddly enough to guard anyway).
	pad := inner - lineW - suffixW
	if pad < 0 {
		pad = 0
	}
	return line + strings.Repeat(" ", pad) + suffix
}

func backgroundJobStatusBadge(status local.JobStatus) string {
	switch status {
	case local.JobCompleted:
		return dim("✓")
	case local.JobFailed:
		return red("✗")
	default: // running
		return green("▶")
	}
}

// backgroundJobElapsed returns a short "12s"/"3m" duration string: time
// since start for a running job, or total run time for a finished one.
func backgroundJobElapsed(j local.Job) string {
	end := j.EndedAt
	if end.IsZero() {
		end = time.Now()
	}
	d := end.Sub(j.StartedAt)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}
