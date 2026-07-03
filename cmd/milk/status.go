package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/x/ansi"
	rw "github.com/mattn/go-runewidth"

	"github.com/scoutme/milk/internal/session"
)

func (m *model) headerBar() string {
	frame := 8 // static peak (bright gold) when idle
	if m.busy {
		frame = m.spinnerFrame
	}
	logo := headerLogo(frame)
	tagline := dim("switch models, not context.")
	taglinePlain := "switch models, not context."

	sessID := m.st.sess.ID
	if len(sessID) > 8 {
		sessID = sessID[:8]
	}
	var totalPrompt, totalCompletion, totalCacheRead, totalCacheCreation int64
	for _, u := range m.st.sess.Tokens {
		totalPrompt += u.Prompt
		totalCompletion += u.Completion
		totalCacheRead += u.CacheRead
		totalCacheCreation += u.CacheCreation
	}
	sessLabel := fmt.Sprintf("sess:%s (total:↑%s↓%s)", sessID, formatTokenCount(totalPrompt), formatTokenCount(totalCompletion))
	if cacheTotal := totalCacheRead + totalCacheCreation; cacheTotal > 0 {
		hitPct := int(100 * float64(totalCacheRead) / float64(cacheTotal))
		sessLabel += fmt.Sprintf(" cache:%d%%", hitPct)
	}
	const repoURL = "github.com/scoutme/milk"
	rightFull := dim(repoURL + "  " + sessLabel + "  /help")
	rightFulPlain := repoURL + "  " + sessLabel + "  /help"
	rightShort := dim(sessLabel + "  /help")
	rightShortPlain := sessLabel + "  /help"

	logoPlain := stripANSI(logo)
	available := m.width - 2
	rightPart, rightPlain := rightFull, rightFulPlain
	if available < len(logoPlain)+2+len(taglinePlain)+2+len(rightFulPlain) {
		rightPart, rightPlain = rightShort, rightShortPlain
	}
	left := " " + logo + "  " + tagline
	leftPlain := " " + logoPlain + "  " + taglinePlain
	gap := max(available-len(leftPlain)-len(rightPlain), 1)
	bar := left + strings.Repeat(" ", gap) + rightPart + " "
	if isTTY {
		return styleHeaderBar.Width(m.width).Render(bar)
	}
	return bar
}

// statusBar renders the one-line status bar.
func (m *model) statusBar() string {
	tokenStr := m.statusTokens()
	left := fmt.Sprintf(" %s  %s%s", dim("role:")+dim(sessionRole(m.st.sess.State)), dim("agent:")+m.statusAgent(), tokenStr)
	right := dim(m.statusCwd() + " ")
	if m.credRefreshing {
		left += dim(" [refreshing " + m.credLabel + " credentials…]")
	} else if m.credStatus != "" {
		if m.credOK {
			left += dim(" [" + m.credLabel + " creds: " + m.credStatus + "]")
		} else {
			left += yellow(" [" + m.credLabel + " creds failed: " + m.credStatus + "]")
		}
	}
	if m.quitPending {
		left += yellow(" [press ctrl+c again to exit]")
	} else if m.busyHint != "" {
		left += yellow(" [" + m.busyHint + "]")
	} else if m.copyFeedback != "" {
		left += green(" [" + m.copyFeedback + "]")
	} else if m.taSelAnchor >= 0 && m.taSelEnd >= 0 && m.taSelAnchor != m.taSelEnd {
		n := len([]rune(m.taSelText()))
		left += yellow(fmt.Sprintf(" [%d chars selected — ctrl+c copy · ctrl+x cut · del delete · type to replace]", n))
	} else if m.selAnchorLine >= 0 && m.selDragging {
		var selStatus string
		if m.selText != "" {
			selStatus = yellow(fmt.Sprintf(" [%d chars — ctrl+c / right-click to copy]", len([]rune(m.selText))))
		} else {
			selStatus = yellow(fmt.Sprintf(" [selecting: line %d col %d — release to end]", m.selAnchorLine+1, m.selAnchorCol+1))
		}
		left += selStatus
	}
	// Truncate right (cwd) if it alone exceeds terminal width
	{
		maxRight := m.width - 2 // leave at least 1 char for left + gap
		if maxRight < 1 {
			maxRight = 1
		}
		plainRight := ansi.Strip(right)
		if rw.StringWidth(plainRight) > maxRight {
			runes := []rune(plainRight)
			w := 0
			start := len(runes)
			for i := len(runes) - 1; i >= 0; i-- {
				cw := rw.RuneWidth(runes[i])
				if w+cw > maxRight-1 { // -1 for the "…" prefix
					break
				}
				w += cw
				start = i
			}
			right = dim("…" + string(runes[start:]) + " ")
		}
	}
	rightWidth := rw.StringWidth(ansi.Strip(right))
	leftWidth := rw.StringWidth(ansi.Strip(left))
	maxLeftWidth := m.width - rightWidth - 1
	if maxLeftWidth < 1 {
		maxLeftWidth = 1
	}
	if leftWidth > maxLeftWidth {
		// truncate left to maxLeftWidth visual chars, preserving dim styling
		plain := ansi.Strip(left)
		runes := []rune(plain)
		w := 0
		cut := 0
		for i, r := range runes {
			cw := rw.RuneWidth(r)
			if w+cw > maxLeftWidth {
				break
			}
			w += cw
			cut = i + 1
		}
		left = dim(string(runes[:cut]))
		leftWidth = w
	}
	gap := max(m.width-leftWidth-rightWidth, 1)
	bar := left + strings.Repeat(" ", gap) + right
	if isTTY {
		if m.pendingPerm != nil {
			return styleStatusBarPerm.Width(m.width).MaxWidth(m.width).Render(bar)
		}
		return styleStatusBar.Width(m.width).MaxWidth(m.width).Render(bar)
	}
	return bar
}

func (m *model) statusAgent() string {
	if m.searching {
		label := "reverse-i-search"
		if m.searchForward {
			label = "forward-i-search"
		}
		return dim("(" + label + ")`" + m.searchQuery.String() + "'")
	}
	agent := dim(agentLabel(m.st))
	if m.pendingPerm != nil {
		lbl := m.pendingPerm.label
		if lbl == "" {
			lbl = "[allow?]"
		}
		return "? " + agent + " " + lbl
	}
	if m.busy {
		frame := yellow(bold(spinnerFrames[m.spinnerFrame%len(spinnerFrames)]))
		pulsed := pulse(agentLabel(m.st), m.spinnerFrame)
		if m.activeToolUse != "" {
			return frame + " " + pulsed + dim(" ["+m.activeToolUse+"]")
		}
		return frame + " " + pulsed
	}
	return agent
}

// statusTokens returns the token counter fragment for the status bar.
// While busy: shows live streamed char count as a proxy for in-progress output.
// While idle: "in:X out:Y  last in:X out:Y" — session totals + last turn real tokens.
func (m *model) statusTokens() string {
	role := m.activeTokenRole()

	if !m.busy {
		m.lastTokenRole = role
	}

	var prompt, completion int64
	switch role {
	case "escalation":
		prompt, completion = m.escalationPrompt, m.escalationComp
	default:
		prompt, completion = m.primaryPrompt, m.primaryCompletion
	}

	lastPrompt := m.lastTurnPrompt[role]
	lastCompletion := m.lastTurnCompletion[role]

	var parts []string
	parts = append(parts, fmt.Sprintf("↑%s↓%s", formatTokenCount(prompt), formatTokenCount(completion)))
	if m.busy {
		parts = append(parts, fmt.Sprintf("↓~%s", formatTokenCount(int64(math.Round(float64(m.currentTurnChars)*0.25)))))
	} else if lastPrompt+lastCompletion > 0 {
		parts = append(parts, fmt.Sprintf("(last:↑%s↓%s)", formatTokenCount(lastPrompt), formatTokenCount(lastCompletion)))
	}
	if role == "escalation" {
		cacheTotal := m.escalationCacheRead + m.escalationCacheCreation
		if cacheTotal > 0 {
			hitPct := int(100 * float64(m.escalationCacheRead) / float64(cacheTotal))
			parts = append(parts, fmt.Sprintf("cache:%s/%s(%d%%)",
				formatTokenCount(m.escalationCacheRead),
				formatTokenCount(m.escalationCacheCreation),
				hitPct))
		}
	}
	return "  " + dim(strings.Join(parts, "  "))
}

// activeTokenRole returns "escalation" or "primary" reflecting which agent will
// handle (or is handling) the current turn. Mirrors agentLabel priority order.
func (m *model) activeTokenRole() string {
	st := m.st
	if st.activeFallbackTarget == "escalation" {
		return "escalation"
	}
	if st.activeFallbackTarget == "primary" {
		return "primary"
	}
	if st.stickyEscalate || st.forceEscalate || st.autoStickyEscalate {
		return "escalation"
	}
	if st.stickyPrimary || st.forcePrimary {
		return "primary"
	}
	if st.sess.State == session.StateEscalation || st.sess.State == session.StateEscalationWaiting {
		return "escalation"
	}
	return "primary"
}

// formatTokenCount formats a token count compactly: <1000 → exact, ≥1000 → "1.2k".
func formatTokenCount(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// sessionRole maps session state to the human-readable role shown in the status bar.
func sessionRole(s session.State) string {
	switch s {
	case session.StateLocal:
		return "PRIMARY"
	case session.StateEscalation:
		return "ESCALATION"
	case session.StateEscalationWaiting:
		return "ESCALATION_WAITING"
	default:
		return "ROUTING"
	}
}

func agentLabel(st *interactiveState) string {
	localName := st.cfg.ActiveAgent().Name
	if localName == "" {
		localName = "local"
	}
	escalationName := st.cfg.EscalationAgentConfig().Name
	if escalationName == "" {
		escalationName = "escalation"
	}
	switch {
	case st.activeFallbackTarget == "escalation":
		return escalationName + " (fallback)"
	case st.activeFallbackTarget == "primary":
		return localName + " (fallback)"
	case st.stickyEscalate:
		return escalationName + " (pinned)"
	case st.autoStickyEscalate:
		return escalationName + " (sticky)"
	case st.forceEscalate:
		return escalationName + " (forced)"
	case st.stickyPrimary:
		return localName + " (pinned)"
	case st.forcePrimary:
		return localName + " (forced)"
	case st.sess.State == session.StateEscalation || st.sess.State == session.StateEscalationWaiting:
		return escalationName
	default:
		return localName
	}
}

func (m *model) statusCwd() string {
	cwd := m.st.cwd
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, cwd); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + rel
		}
	}
	return cwd
}
