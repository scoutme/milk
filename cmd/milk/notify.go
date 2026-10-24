package main

import (
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/sgr"
)

// Toast/notification subsystem (issue #162, ADR-0048).
//
// milk's own informational events (thinking-visibility switches, panel
// open/close, background-job lifecycle, credential refresh, config reload…)
// used to be appended straight into the transcript, where they pollute the
// conversation view, carry no timestamp, can't be dismissed, and scroll
// away. They now render as a floating toast overlay pinned to the top-right
// of the main area (viewport or PTY pane) — visually displaced, never part
// of the transcript content, so nothing changes for scrollback, selection,
// or the escalation context builder.
//
// Scope boundary (ADR-0048): only turn-unrelated *informational* events move
// here. Tool calls, turn-bound output, warnings, and errors stay in the
// transcript where they persist until the user scrolls past them.

const (
	// toastTTL is how long a toast stays visible once promoted from the
	// queue to the visible set.
	toastTTL = 6 * time.Second
	// minVisibleToasts is the floor on how many toasts render simultaneously
	// (stacked top-right) — see maxVisibleToastsFor, which scales this up
	// for taller terminals; further events wait in toastQueue.
	minVisibleToasts = 3
	// toastVisibleRowDivisor controls how fast the visible cap grows with
	// chat height: one extra slot per this many rows, see maxVisibleToastsFor.
	toastVisibleRowDivisor = 10
	// toastSideMargin keeps the toast band off both edges of the confined
	// transcript+separator span (m.mainWidth()) — a floating card, not a
	// band flush against the viewport's own edges.
	toastSideMargin = 2
	// toastContentPadding reserves one blank column before and after each
	// row's content, inside the painted band itself — breathing room so
	// text never touches the band's own left/right edge.
	toastContentPadding = 1
	// toastQueueCap bounds the waiting queue — beyond it the oldest *waiting*
	// toast is dropped (it is still recorded in history either way).
	toastQueueCap = 50
	// toastHistoryCap bounds the /notifications history ring.
	toastHistoryCap = 500
	// toastHistoryShow is how many entries a bare /notifications prints.
	toastHistoryShow = 20
)

// maxVisibleToastsFor returns how many toasts may render at once for a chat
// area of the given height (m.viewportHeight()): proportional to the
// available space so a tall terminal can surface more at once without a
// short one being swamped, floored at minVisibleToasts.
func maxVisibleToastsFor(chatHeight int) int {
	return max(minVisibleToasts, chatHeight/toastVisibleRowDivisor)
}

// toastEvent is one notification: either queued, visible (expiresAt set), or
// historical (expiresAt zeroed once recorded).
type toastEvent struct {
	at        time.Time
	text      string
	hint      string    // related slash command; empty → "/notifications" fallback
	expiresAt time.Time // set when the toast is promoted to visible
}

// hintOrFallback returns the event's related slash command, defaulting to
// the history command — per issue #162 the hint is always "the command you
// would use to see the full history or act on it".
func (ev toastEvent) hintOrFallback() string {
	if ev.hint != "" {
		return ev.hint
	}
	return "/notifications"
}

// toastTickMsg schedules toast expiry. gen must match model.toastGen —
// stale ticks (from before a dismissal or a re-arm) are ignored rather than
// mutating tick state: the exact class of bug issue #168 caught in
// dragResetMsg, where the generation existed but was never checked.
type toastTickMsg struct{ gen int }

// notify enqueues an informational event: recorded in history, pushed onto
// the toast queue, and promoted into the visible set while there is room.
//
// It deliberately returns nothing: emitters are value-receiver helpers
// (handleThinkCmd, autoOpenPanel, refreshMCPToolSets…) that predate toasts.
// The Update wrapper (repl.go) arms the expiry tick after every message, so
// firing-and-forgetting here is safe as long as the caller returns the
// mutated model — which every emitter call site does.
func (m *model) notify(text, hint string) {
	now := time.Now()
	ev := toastEvent{at: now, text: text, hint: hint}

	m.toastHistory = append(m.toastHistory, ev)
	if over := len(m.toastHistory) - toastHistoryCap; over > 0 {
		m.toastHistory = append(m.toastHistory[:0], m.toastHistory[over:]...)
	}

	m.toastQueue = append(m.toastQueue, ev)
	if over := len(m.toastQueue) - toastQueueCap; over > 0 {
		// Drop the oldest *waiting* toasts; the visible set sits at the
		// front of the queue so it keeps its slots, and every dropped event
		// survives in history either way.
		m.toastQueue = append(m.toastQueue[:0], m.toastQueue[over:]...)
	}

	m.promoteToasts(now)
}

// promoteToasts fills the visible window from the queue, stamping each newly
// visible toast with a fresh TTL so queued events never expire unseen.
func (m *model) promoteToasts(now time.Time) {
	limit := maxVisibleToastsFor(m.viewportHeight())
	for len(m.toastVisible) < limit && len(m.toastQueue) > 0 {
		ev := m.toastQueue[0]
		m.toastQueue = m.toastQueue[1:]
		ev.expiresAt = now.Add(toastTTL)
		m.toastVisible = append(m.toastVisible, ev)
	}
}

// expireToasts drops visible toasts past their deadline and promotes queued
// ones into the freed slots. Reports whether the visible set changed.
func (m *model) expireToasts(now time.Time) bool {
	kept := m.toastVisible[:0]
	for _, ev := range m.toastVisible {
		if ev.expiresAt.After(now) {
			kept = append(kept, ev)
		}
	}
	changed := len(kept) != len(m.toastVisible)
	m.toastVisible = kept

	before := len(m.toastVisible)
	m.promoteToasts(now)
	return changed || len(m.toastVisible) != before
}

// dismissToasts clears the visible toasts AND the waiting queue (history is
// kept — /notifications still shows everything). Returns false when there
// was nothing to dismiss.
//
// Bumping toastGen invalidates any expiry tick already in flight; without
// that, a stale tick firing after a re-arm would clear the "armed" flag that
// now belongs to a newer tick and open the door to duplicate tick chains —
// the #168 lesson applied proactively.
func (m *model) dismissToasts() bool {
	if len(m.toastVisible) == 0 && len(m.toastQueue) == 0 {
		return false
	}
	m.toastVisible = nil
	m.toastQueue = nil
	m.toastGen++
	m.toastTickArmed = false
	return true
}

// armToastTick returns a Tick cmd for the earliest visible expiry, or nil
// when a tick is already in flight or nothing is visible. Each arm bumps
// toastGen so only the newest tick is considered live (see toastTickMsg).
func (m *model) armToastTick() tea.Cmd {
	if m.toastTickArmed || len(m.toastVisible) == 0 {
		return nil
	}
	earliest := m.toastVisible[0].expiresAt
	for _, ev := range m.toastVisible[1:] {
		if ev.expiresAt.Before(earliest) {
			earliest = ev.expiresAt
		}
	}
	delay := time.Until(earliest)
	if delay < 0 {
		delay = 0
	}
	m.toastTickArmed = true
	m.toastGen++
	gen := m.toastGen
	return tea.Tick(delay, func(time.Time) tea.Msg { return toastTickMsg{gen: gen} })
}

// toastContent builds the styled time + message + slash-command hint for one
// toast, truncating the free-form message so the whole thing fits within
// width. Returns the styled string and its plain-text cell width — the
// latter drives both the shared start-column calculation and the per-row
// padding in overlayToasts.
func toastContent(ev toastEvent, width int) (string, int) {
	timeStr := ev.at.Format("15:04")
	hint := ev.hintOrFallback()
	hintPart := "  " + hint

	// Budget for the free-form message: everything else is fixed width.
	budget := width - (len(timeStr) + 1 + len([]rune(hintPart)))
	if budget < 6 && hintPart != "" {
		hintPart = ""
		budget = width - (len(timeStr) + 1)
	}
	if budget < 4 {
		budget = 4
	}
	text := truncate(ev.text, budget)

	line := dim(timeStr) + " " + text
	if hintPart != "" {
		line += hintPart[:2] + blue(hint)
	}
	plainW := len(timeStr) + 1 + len([]rune(text)) + len([]rune(hintPart))
	return line, plainW
}

// toastDismissLine builds the standalone dismiss-row content: "N more
// pending · ctrl+g dismiss" (dot separated) when the waiting queue is
// non-empty, or just "ctrl+g dismiss" when nothing is queued. Degrades by
// dropping the pending count before ever touching the dismiss affordance
// itself, since that's this row's entire purpose.
func toastDismissLine(pending, width int) (string, int) {
	const dismiss = "ctrl+g dismiss"
	full := dismiss
	if pending > 0 {
		full = strconv.Itoa(pending) + " more pending · " + dismiss
	}
	if len([]rune(full)) > width {
		full = dismiss
	}
	if w := len([]rune(full)); w > width {
		full = string([]rune(full)[:max(width, 0)])
	}
	return dim(full), len([]rune(full))
}

// padToastRow pads pre-built toast content (plainW cells wide) to exactly
// width cells, starting at column leftPad, and applies the toast band
// background. Leading reset first: the transcript line underneath may have
// been cut mid-SGR during truncation, and its state would otherwise bleed
// into the toast text.
func padToastRow(content string, plainW, width, leftPad int) string {
	trailing := max(width-leftPad-plainW, 0)
	line := strings.Repeat(" ", leftPad) + content + strings.Repeat(" ", trailing)

	bg := toastBackgroundCode()
	if bg == "" {
		return line
	}
	return ansiReset + sgr.WithBackground(line, bg)
}

// overlayToasts paints the visible toasts over the top-right corner of the
// transcript+separator column of the already-rendered main-area block
// (viewport+panels, attach view, or PTY screen). Because it operates purely
// at render time it consumes no layout rows: viewportHeight, panel geometry,
// PTY sizing, and the attach view all stay untouched — the toast genuinely
// floats over whatever is underneath.
//
// The overlay is confined to m.mainWidth() — the viewport+separator span —
// rather than the full block width. Any side panel joined to the right of
// that (memory/tasks/background/workflow) lives beyond mainWidth and is cut
// off intact as `tail` below, then reattached untouched: row 0 of an open
// panel is its title line (panelTitleLine), and sizing/truncating against
// the full block width here used to blank that title out from under a toast
// instead of floating only over the transcript.
//
// Within that span, the painted toast band is inset by toastSideMargin
// columns from the right edge, and its *left* edge sits at the shared start
// column (sharedCol below) rather than toastSideMargin: everything left of
// that — including what would otherwise be blank, background-tinted filler —
// is left as the original underlying content untouched, so the overlay
// covers the least area needed instead of painting the whole box width.
//
// The dismiss affordance gets its own row, right after the last visible
// toast, instead of being folded into that toast's message row: it applies
// to every open toast, not just one message, so it reads better as a
// footer. That row shows "N more pending · ctrl+g dismiss" when toastQueue
// is non-empty (events waiting beyond what's currently visible), or just
// "ctrl+g dismiss" otherwise. Every row — the messages and the dismiss
// footer alike — shares one start column: computed just far enough left to
// fit the longest of them (plus one toastContentPadding column reserved on
// each side), but never left of the box's halfway point, so the block
// reads as a consistent ragged-right list instead of each row individually
// right-aligning to its own, different, start column.
func (m *model) overlayToasts(block string) string {
	if len(m.toastVisible) == 0 {
		return block
	}
	width := m.mainWidth()
	if width < 20 {
		return block
	}
	boxWidth := width - 2*toastSideMargin
	n := len(m.toastVisible)
	contentCap := max(boxWidth-2*toastContentPadding, 0)

	contents := make([]string, n)
	plainLens := make([]int, n)
	maxLen := 0
	for i, ev := range m.toastVisible {
		contents[i], plainLens[i] = toastContent(ev, contentCap)
		if plainLens[i] > maxLen {
			maxLen = plainLens[i]
		}
	}
	dismissContent, dismissLen := toastDismissLine(len(m.toastQueue), contentCap)
	if dismissLen > maxLen {
		maxLen = dismissLen
	}
	sharedCol := max(boxWidth/2, boxWidth-(maxLen+2*toastContentPadding))
	avail := boxWidth - sharedCol // == the painted band's width
	availCap := max(avail-2*toastContentPadding, 0)

	lines := strings.Split(block, "\n")
	paint := func(row int, content string, plainW int) {
		if row >= len(lines) {
			return
		}
		toast := padToastRow(content, plainW, avail, toastContentPadding)
		lines[row] = overlayRowAt(lines[row], toastSideMargin+sharedCol, toast)
	}

	for i, ev := range m.toastVisible {
		content, plainW := contents[i], plainLens[i]
		if plainW > availCap {
			// The halfway clamp left less room than this row's natural
			// (full-boxWidth-budgeted) length: re-budget against the
			// smaller avail width so the message truncates further rather
			// than the already-built string getting blindly chopped.
			content, plainW = toastContent(ev, availCap)
		}
		paint(i, content, plainW)
	}
	if dismissLen > availCap {
		dismissContent, dismissLen = toastDismissLine(len(m.toastQueue), availCap)
	}
	paint(n, dismissContent, dismissLen)

	return strings.Join(lines, "\n")
}

// handleNotificationsCmd implements `/notifications [list [count] | clear]` —
// the history view required by issue #162, extended with an explicit `list`
// subcommand so history older than toastHistoryShow stays reachable: a bare
// `/notifications` is exactly `list toastHistoryShow`, `list` with no count
// shows the entire history unfiltered, and `list <count>` shows the last
// <count> entries. Output goes to the transcript like every other explicit
// query (/history, /export…): the user asked for it.
func (m model) handleNotificationsCmd(sub string) model {
	args := strings.Fields(sub)
	if len(args) == 0 {
		return m.listNotifications(strconv.Itoa(toastHistoryShow))
	}
	switch args[0] {
	case "clear":
		m.toastHistory = nil
		m.appendTranscript(milkTag() + " notification history cleared\n")
		return m
	case "list":
		if len(args) > 1 {
			return m.listNotifications(args[1])
		}
		return m.listNotifications("")
	}
	m.appendTranscript(milkTag() + " unknown /notifications subcommand: " + args[0] + " (try: list [count], clear)\n")
	return m
}

// listNotifications prints the last count entries of toastHistory, or the
// entire history unfiltered when count is "". Older-than-toastHistoryShow
// events would otherwise be unreachable once rotation drops them from the
// default view.
func (m model) listNotifications(count string) model {
	if len(m.toastHistory) == 0 {
		m.appendTranscript(milkTag() + " no notifications recorded\n")
		return m
	}
	entries := m.toastHistory
	note := ""
	if count != "" {
		n, err := strconv.Atoi(count)
		if err != nil || n <= 0 {
			m.appendTranscript(milkTag() + " /notifications list: count must be a positive number, got " + count + "\n")
			return m
		}
		if n < len(entries) {
			entries = entries[len(entries)-n:]
			note = " (last " + strconv.Itoa(len(entries)) + ")"
		}
	}
	var b strings.Builder
	b.WriteString(milkTag() + " notification history" + note + ":\n")
	for _, ev := range entries {
		b.WriteString("  " + dim(ev.at.Format("15:04:05")) + "  " + ev.text)
		b.WriteString("  " + blue(ev.hintOrFallback()))
		b.WriteString("\n")
	}
	m.appendTranscript(b.String())
	return m
}
