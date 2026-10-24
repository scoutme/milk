package main

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
)

// Toast/notification subsystem tests (issue #162, ADR-0048).
//
// The background_ui_test.go migration tests cover the emitters (job done,
// spawn, panel toggles); these cover the subsystem itself: queue semantics,
// TTL/promotion, dismissal, the generation-guarded expiry tick (the #168
// lesson applied proactively), rendering, and the /notifications history.

// toastMentions reports whether the model has — visible or waiting in the
// queue — a notification whose text contains substr. History is deliberately
// not consulted: an event that was recorded but never shown must not count
// as "the user saw a toast".
func toastMentions(m model, substr string) bool {
	for _, ev := range m.toastVisible {
		if strings.Contains(ev.text, substr) {
			return true
		}
	}
	for _, ev := range m.toastQueue {
		if strings.Contains(ev.text, substr) {
			return true
		}
	}
	return false
}

// newToastTestModel returns a fully wired model for Update/View-level tests.
func newToastTestModel(t *testing.T) model {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	return newModel(context.Background(), st, nil, dispatchAgents{}, nil)
}

func TestNotify_RecordsHistoryTimestampAndVisible(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	m.notify("memory panel opened", "/panel memory")

	if len(m.toastHistory) != 1 {
		t.Fatalf("history length = %d, want 1", len(m.toastHistory))
	}
	ev := m.toastHistory[0]
	if ev.at.IsZero() {
		t.Error("history entry must carry a timestamp")
	}
	if ev.text != "memory panel opened" || ev.hint != "/panel memory" {
		t.Errorf("history entry = %+v, want text/hint recorded", ev)
	}
	if len(m.toastVisible) != 1 {
		t.Fatalf("visible length = %d, want 1", len(m.toastVisible))
	}
	if m.toastVisible[0].expiresAt.IsZero() {
		t.Error("visible toast must have an expiry set at promotion")
	}
	if len(m.toastQueue) != 0 {
		t.Errorf("queue length = %d, want 0 (promoted immediately)", len(m.toastQueue))
	}
}

func TestNotify_CapsVisibleAndQueuesTheRest(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	limit := maxVisibleToastsFor(m.viewportHeight())
	for i := range limit + 2 {
		m.notify(strings.Repeat("event ", 1)+string(rune('a'+i)), "")
	}
	if len(m.toastVisible) != limit {
		t.Errorf("visible = %d, want %d", len(m.toastVisible), limit)
	}
	if len(m.toastQueue) != 2 {
		t.Errorf("queue = %d, want 2", len(m.toastQueue))
	}
	if len(m.toastHistory) != limit+2 {
		t.Errorf("history = %d, want %d (all events recorded)", len(m.toastHistory), limit+2)
	}
}

// TestHandleNotificationsCmd_LastNoteMatchesShownCount pins a real bug: the
// "(last N)" note was built from the history's full length *before* slicing
// down to toastHistoryShow, so a 68-entry history printing only 20 lines
// claimed "(last 68)" instead of "(last 20)".
func TestHandleNotificationsCmd_LastNoteMatchesShownCount(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	for i := range toastHistoryShow + 5 {
		m.notify(strings.Repeat("event ", 1)+string(rune('a'+i%26)), "")
	}
	if len(m.toastHistory) != toastHistoryShow+5 {
		t.Fatalf("setup: history = %d, want %d", len(m.toastHistory), toastHistoryShow+5)
	}

	got := m.handleNotificationsCmd("")
	out := got.transcript.String()

	want := "(last " + strconv.Itoa(toastHistoryShow) + ")"
	if !strings.Contains(out, want) {
		t.Errorf("expected %q in output, got %q", want, out)
	}
	if shown := strings.Count(out, "/notifications"); shown != toastHistoryShow {
		t.Errorf("printed %d entries, want %d (toastHistoryShow)", shown, toastHistoryShow)
	}
}

// TestMaxVisibleToastsFor_FloorsAtMinimum pins the "min 3" floor for short
// terminals and the proportional growth for taller ones.
func TestMaxVisibleToastsFor_FloorsAtMinimum(t *testing.T) {
	if got := maxVisibleToastsFor(0); got != minVisibleToasts {
		t.Errorf("height 0: got %d, want floor %d", got, minVisibleToasts)
	}
	if got := maxVisibleToastsFor(10); got != minVisibleToasts {
		t.Errorf("height 10: got %d, want floor %d", got, minVisibleToasts)
	}
	if got, want := maxVisibleToastsFor(100), 10; got != want {
		t.Errorf("height 100: got %d, want %d", got, want)
	}
}

// TestExpireToasts_PromotesQueuedWithFreshTTL pins the core queue contract:
// a queued event must never expire unseen — promotion stamps a *fresh* TTL,
// so its lifetime starts when it becomes visible, not when it was enqueued.
func TestExpireToasts_PromotesQueuedWithFreshTTL(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	for i := range 4 {
		m.notify(string(rune('a'+i)), "")
	}
	if len(m.toastVisible) != 3 || len(m.toastQueue) != 1 {
		t.Fatalf("visible=%d queue=%d, want 3/1", len(m.toastVisible), len(m.toastQueue))
	}

	now := time.Now()
	for i := range m.toastVisible {
		m.toastVisible[i].expiresAt = now.Add(-time.Second) // all expired
	}
	if !m.expireToasts(now) {
		t.Error("expireToasts must report a change when toasts expired")
	}
	if len(m.toastVisible) != 1 {
		t.Fatalf("visible after expiry = %d, want 1 (promoted from queue)", len(m.toastVisible))
	}
	promoted := m.toastVisible[0]
	if want := now.Add(toastTTL); promoted.expiresAt.Before(now) || promoted.expiresAt.After(want.Add(time.Second)) {
		t.Errorf("promoted expiry = %v, want a fresh TTL around %v", promoted.expiresAt, want)
	}
}

// TestStaleToastTick_Ignored is the #168 lesson applied to the toast tick: a
// tick scheduled before a dismissal/re-arm carries an older generation and
// must not clear the armed flag that now belongs to the newer in-flight tick
// (which would open the door to duplicate tick chains).
func TestStaleToastTick_Ignored(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	m.notify("first", "")
	m.armToastTick()  // gen=1, armed
	m.dismissToasts() // gen=2, armed=false, visible cleared
	m.notify("second", "")
	m.armToastTick() // gen=3, armed=true (live tick in flight)
	if !m.toastTickArmed {
		t.Fatal("expected a tick to be armed after armToastTick")
	}
	liveGen := m.toastGen

	// The stale (first) tick fires: must be ignored entirely — same gen
	// value the dragResetMsg handler failed to check in #168.
	updated, _ := m.updateInner(toastTickMsg{gen: 1})
	got := updated.(model)
	if !got.toastTickArmed {
		t.Error("stale tick cleared the armed flag of the live tick")
	}
	if got.toastGen != liveGen {
		t.Errorf("stale tick changed toastGen: %d -> %d", liveGen, got.toastGen)
	}
	if len(got.toastVisible) != 1 {
		t.Errorf("stale tick mutated the visible set: %d toasts", len(got.toastVisible))
	}

	// The live tick fires: clears the flag and expires the (now past-deadline) toast.
	got.toastVisible[0].expiresAt = time.Now().Add(-time.Second)
	updated, _ = got.updateInner(toastTickMsg{gen: liveGen})
	got = updated.(model)
	if got.toastTickArmed {
		t.Error("live tick must clear the armed flag")
	}
	if len(got.toastVisible) != 0 {
		t.Errorf("live tick must expire past-deadline toasts, got %d", len(got.toastVisible))
	}
}

// TestDismissToasts_KeepsHistory: interactive dismissal (Ctrl+G) clears what
// is on screen but must not erase the record — /notifications still shows it.
func TestDismissToasts_KeepsHistory(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	m.notify("one", "")
	m.notify("two", "")
	genBefore := m.toastGen

	if !m.dismissToasts() {
		t.Fatal("dismissToasts must report success when toasts are open")
	}
	if len(m.toastVisible) != 0 || len(m.toastQueue) != 0 {
		t.Errorf("dismiss must clear visible+queue, got %d/%d", len(m.toastVisible), len(m.toastQueue))
	}
	if len(m.toastHistory) != 2 {
		t.Errorf("history = %d, want 2 (dismissal keeps history)", len(m.toastHistory))
	}
	if m.toastGen == genBefore {
		t.Error("dismiss must bump toastGen to invalidate in-flight ticks")
	}
	if m.dismissToasts() {
		t.Error("dismiss with nothing open must report false")
	}
}

// TestUpdate_CtrlGDismissesToasts exercises the global keybinding path.
func TestUpdate_CtrlGDismissesToasts(t *testing.T) {
	m := newToastTestModel(t)
	m.notify("a toast to dismiss", "")
	if len(m.toastVisible) != 1 {
		t.Fatalf("setup: visible = %d, want 1", len(m.toastVisible))
	}
	updated, _ := m.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	got := updated.(model)
	if len(got.toastVisible) != 0 {
		t.Errorf("Ctrl+G must dismiss open toasts, got %d", len(got.toastVisible))
	}
	if len(got.toastHistory) != 1 {
		t.Errorf("Ctrl+G must keep history, got %d entries", len(got.toastHistory))
	}
}

// TestView_ToastOverlayFloating pins the rendering contract: the toast (with
// its timestamp, message, slash-command hint, and dismiss affordance) shows
// in the rendered view, is NOT part of the transcript, and — being a pure
// render-time overlay — adds zero layout rows (same line count either way).
func TestView_ToastOverlayFloating(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	baseline := m.viewString()
	baselineLines := strings.Count(baseline, "\n")

	// Message kept short: the box's halfway clamp (overlayToasts) truncates
	// the free-form message before dropping hint/dismiss, so a long message
	// is not guaranteed to survive intact — that trade-off has its own
	// dedicated coverage below. This test only pins the overlay mechanics.
	m.notify("panel on", "/panel background")
	view := m.viewString()

	if !strings.Contains(view, "panel on") {
		t.Errorf("view must contain the toast text, got:\n%s", view)
	}
	if !strings.Contains(view, "/panel background") {
		t.Errorf("view must show the slash-command hint next to the toast, got:\n%s", view)
	}
	if !strings.Contains(view, "ctrl+g dismiss") {
		t.Errorf("view must show the dismiss affordance, got:\n%s", view)
	}
	if !regexp.MustCompile(`\d{2}:\d{2}`).MatchString(view) {
		t.Errorf("view must show the current time on the toast, got:\n%s", view)
	}
	if strings.Contains(m.transcript.String(), "panel on") {
		t.Errorf("toast text must never enter the transcript, got %q", m.transcript.String())
	}
	if got := strings.Count(view, "\n"); got != baselineLines {
		t.Errorf("toast overlay changed the view height: %d -> %d lines (must consume zero rows)", baselineLines, got)
	}
}

// TestPadToastRow_StartsContentAtLeftPad pins padToastRow's contract: content
// starts exactly at column leftPad (leading spaces), and the row is padded
// out to width regardless of how short the content is.
func TestPadToastRow_StartsContentAtLeftPad(t *testing.T) {
	content, plainW := toastContent(toastEvent{at: time.Now(), text: "hi"}, 50)
	row := padToastRow(content, plainW, 50, 10)
	plain := ansi.Strip(row)
	if w := utf8.RuneCountInString(plain); w != 50 {
		t.Fatalf("width = %d, want 50", w)
	}
	if !strings.HasPrefix(plain, strings.Repeat(" ", 10)) {
		t.Errorf("expected content to start at column 10, got %q", plain)
	}
	if strings.TrimSpace(plain) == "" {
		t.Fatalf("expected non-blank content, got %q", plain)
	}
}

// firstNonSpace returns the index of the first non-space rune in r, or -1.
func firstNonSpace(r []rune) int {
	for i, c := range r {
		if c != ' ' {
			return i
		}
	}
	return -1
}

// TestOverlayToasts_DismissGetsOwnRow pins requirement: "ctrl+g dismiss"
// renders on its own row right after the last visible toast, not folded
// into that toast's message row — it applies to every open toast, not just
// one message.
func TestOverlayToasts_DismissGetsOwnRow(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	m.notify("short", "")

	view := m.viewString()
	lines := strings.Split(view, "\n")
	// mainArea row i is at line index i+1 (see margin test above); with one
	// visible toast, row 0 is its message and row 1 is the dismiss footer.
	messageRow := lines[1]
	dismissRow := lines[2]

	if strings.Contains(messageRow, "ctrl+g dismiss") {
		t.Errorf("dismiss affordance must not be folded into the message row, got %q", ansi.Strip(messageRow))
	}
	if !strings.Contains(messageRow, "short") {
		t.Errorf("expected the message on its own row, got %q", ansi.Strip(messageRow))
	}
	if !strings.Contains(dismissRow, "ctrl+g dismiss") {
		t.Errorf("expected the dismiss affordance on its own footer row, got %q", ansi.Strip(dismissRow))
	}

	plain := []rune(ansi.Strip(dismissRow))
	if firstNonSpace(plain[toastSideMargin:]) <= 0 {
		t.Errorf("expected leading padding before the dismiss row's content (right-aligned, not flush left), got %q", string(plain))
	}
}

// TestOverlayToasts_PendingCountOnDismissRow pins the "N more pending"
// prefix: when events are still waiting in toastQueue beyond what's
// currently visible, the dismiss footer row names the count, dot-separated
// from the dismiss affordance itself.
func TestOverlayToasts_PendingCountOnDismissRow(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	limit := maxVisibleToastsFor(m.viewportHeight())
	for i := range limit + 2 {
		m.notify(strings.Repeat("event ", 1)+string(rune('a'+i)), "")
	}
	if len(m.toastQueue) != 2 {
		t.Fatalf("setup: queue = %d, want 2", len(m.toastQueue))
	}

	view := m.viewString()
	lines := strings.Split(view, "\n")
	dismissRow := lines[limit+1] // messages occupy rows 1..limit, footer is next

	if !strings.Contains(dismissRow, "2 more pending · ctrl+g dismiss") {
		t.Errorf("expected the pending count dot-separated before the dismiss affordance, got %q", ansi.Strip(dismissRow))
	}
}

// TestOverlayToasts_RowsShareStartColumn pins requirement: every painted
// row — visible toast messages and the dismiss footer alike — starts at the
// same column regardless of each one's own content length, instead of each
// row individually right-aligning to a different column.
//
// Checks the character exactly at the computed bandStart, rather than
// scanning for the first non-space from the margin: a row's *untouched*
// prefix (left of the band, by design — see TestOverlayToasts_BandStartsAtSharedColumn)
// can itself be non-blank underlying content (e.g. the welcome screen's own
// text), which would otherwise read as a false "earlier start".
func TestOverlayToasts_RowsShareStartColumn(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	m.notify("a very long notification message to measure the shared column", "/notifications")
	m.notify("short", "")
	m.notify("mid length message", "")
	if got := maxVisibleToastsFor(m.viewportHeight()); len(m.toastVisible) != got {
		t.Fatalf("setup: visible = %d, want %d (all 3 promoted)", len(m.toastVisible), got)
	}

	width := m.mainWidth()
	boxWidth := width - 2*toastSideMargin
	contentCap := boxWidth - 2*toastContentPadding
	maxLen := 0
	for _, ev := range m.toastVisible {
		if _, l := toastContent(ev, contentCap); l > maxLen {
			maxLen = l
		}
	}
	if _, l := toastDismissLine(len(m.toastQueue), contentCap); l > maxLen {
		maxLen = l
	}
	sharedCol := max(boxWidth/2, boxWidth-(maxLen+2*toastContentPadding))
	// +toastContentPadding: the band's own leading blank column (this
	// change) sits before the content, which starts right after it.
	contentStart := toastSideMargin + sharedCol + toastContentPadding

	view := m.viewString()
	lines := strings.Split(view, "\n")
	// mainArea row i is at line index i+1 (see margin test above); the
	// dismiss footer follows immediately after the last visible toast.
	for i := 0; i <= len(m.toastVisible); i++ {
		row := lines[i+1]
		atContent := ansi.Cut(row, contentStart, contentStart+1)
		if ansi.Strip(atContent) == " " || ansi.Strip(atContent) == "" {
			t.Errorf("row %d: expected content starting exactly at column %d, got blank there in %q", i, contentStart, ansi.Strip(row))
		}
	}
}

// TestOverlayToasts_PreservesSideMargins pins the toastSideMargin inset: the
// toast band must not touch either edge of the confined mainWidth() span —
// the margin columns must still show whatever was underneath, untouched.
func TestOverlayToasts_PreservesSideMargins(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	width := m.mainWidth()

	// View() = headerBar + "\n" + mainArea + "\n" + statusBar: mainArea's own
	// row 0 (where the first toast paints) is line index 1 of the full view.
	before := m.viewString()
	beforeRow := strings.Split(before, "\n")[1]

	m.notify("margin check", "")
	after := m.viewString()
	afterRow := strings.Split(after, "\n")[1]

	beforeLeft := ansi.Strip(ansi.Truncate(beforeRow, toastSideMargin, ""))
	afterLeft := ansi.Strip(ansi.Truncate(afterRow, toastSideMargin, ""))
	if beforeLeft != afterLeft {
		t.Errorf("left margin changed: %q -> %q", beforeLeft, afterLeft)
	}

	beforeRight := ansi.Strip(ansi.Cut(beforeRow, width-toastSideMargin, width))
	afterRight := ansi.Strip(ansi.Cut(afterRow, width-toastSideMargin, width))
	if beforeRight != afterRight {
		t.Errorf("right margin changed: %q -> %q", beforeRight, afterRight)
	}

	if !strings.Contains(afterRow, "margin check") {
		t.Errorf("expected toast text in the overlaid row, got %q", ansi.Strip(afterRow))
	}
}

// TestOverlayToasts_BandStartsAtSharedColumn pins the least-area overlay:
// everything left of the shared column must stay byte-for-byte untouched
// (same as the pre-toast baseline) — the painted band starts at the shared
// column, not at toastSideMargin, instead of painting the whole box width
// (most of it blank filler) regardless of how short the message is.
func TestOverlayToasts_BandStartsAtSharedColumn(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	width := m.mainWidth()
	boxWidth := width - 2*toastSideMargin

	before := m.viewString()
	rowIdx := strings.Count(m.headerBar(), "\n") + 1
	beforeRow := strings.Split(before, "\n")[rowIdx]

	m.notify("hi", "")
	// Mirror overlayToasts' own calculation: the shared column is driven by
	// the longer of the message row and the dismiss footer row, plus the
	// toastContentPadding column reserved on each side.
	contentCap := boxWidth - 2*toastContentPadding
	_, msgLen := toastContent(m.toastVisible[0], contentCap)
	_, dismissLen := toastDismissLine(len(m.toastQueue), contentCap)
	sharedCol := max(boxWidth/2, boxWidth-(max(msgLen, dismissLen)+2*toastContentPadding))
	bandStart := toastSideMargin + sharedCol

	after := m.viewString()
	afterRow := strings.Split(after, "\n")[rowIdx]

	beforePrefix := ansi.Truncate(beforeRow, bandStart, "")
	afterPrefix := ansi.Truncate(afterRow, bandStart, "")
	if beforePrefix != afterPrefix {
		t.Errorf("expected everything before the shared column to stay untouched by the toast:\nbefore=%q\nafter=%q", beforePrefix, afterPrefix)
	}
	if !strings.Contains(afterRow, "hi") {
		t.Errorf("expected the toast text within the painted band, got %q", ansi.Strip(afterRow))
	}
}

// TestOverlayToasts_ContentHasPaddingColumns pins toastContentPadding: the
// band reserves one blank column before the content and (for whichever row
// defines the shared width) exactly one blank column after it, before the
// band's own right edge — content never touches either edge of its band.
func TestOverlayToasts_ContentHasPaddingColumns(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	width := m.mainWidth()
	boxWidth := width - 2*toastSideMargin
	contentCap := boxWidth - 2*toastContentPadding

	m.notify("hi", "")
	_, msgLen := toastContent(m.toastVisible[0], contentCap)
	_, dismissLen := toastDismissLine(len(m.toastQueue), contentCap)
	maxLen := max(msgLen, dismissLen)
	sharedCol := max(boxWidth/2, boxWidth-(maxLen+2*toastContentPadding))
	bandStart := toastSideMargin + sharedCol
	bandEnd := width - toastSideMargin

	rowIdx := strings.Count(m.headerBar(), "\n") + 1
	row := strings.Split(m.viewString(), "\n")[rowIdx]

	leading := ansi.Cut(row, bandStart, bandStart+toastContentPadding)
	if ansi.Strip(leading) != strings.Repeat(" ", toastContentPadding) {
		t.Errorf("expected %d blank column(s) at the start of the band, got %q", toastContentPadding, ansi.Strip(leading))
	}

	// The message row defines maxLen here (longer than the dismiss footer's
	// fixed "ctrl+g dismiss"), so its own trailing gap is exactly one column.
	trailing := ansi.Cut(row, bandEnd-toastContentPadding, bandEnd)
	if ansi.Strip(trailing) != strings.Repeat(" ", toastContentPadding) {
		t.Errorf("expected %d trailing blank column(s) before the band's right edge, got %q", toastContentPadding, ansi.Strip(trailing))
	}
}

// TestView_ToastOverlayPreservesPanelHeader guards against the overlay
// confining itself to mainWidth() for *sizing* the toast but then truncating
// against the full joined-panel line width, which used to blank out an open
// panel's title row (row 0) and leave that one row narrower than the rest.
func TestView_ToastOverlayPreservesPanelHeader(t *testing.T) {
	old := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = old })

	m := layoutTestModel(t, 120, 40)
	m.panelTasks = true
	m.syncLayout()

	before := m.viewString()
	if !strings.Contains(before, "tasks") {
		t.Fatalf("setup: expected tasks panel title in view, got:\n%s", before)
	}

	// Short message: see the comment on TestView_ToastOverlayFloating — the
	// halfway clamp can truncate a long message before dropping hint/dismiss,
	// which isn't what this test is about.
	m.notify("panel open", "/panel tasks")
	after := m.viewString()

	if !strings.Contains(after, "tasks") {
		t.Errorf("toast overlay must not blank out the open panel's title row, got:\n%s", after)
	}
	if !strings.Contains(after, "panel open") {
		t.Errorf("view must still show the toast text, got:\n%s", after)
	}

	beforeLines := strings.Split(before, "\n")
	afterLines := strings.Split(after, "\n")
	if len(beforeLines) != len(afterLines) {
		t.Fatalf("line count changed: %d -> %d", len(beforeLines), len(afterLines))
	}
	for i := range beforeLines {
		bw := utf8.RuneCountInString(ansi.Strip(beforeLines[i]))
		aw := utf8.RuneCountInString(ansi.Strip(afterLines[i]))
		if bw != aw {
			t.Errorf("line %d width changed from %d to %d cells — toast overlay must not shrink a row that has a side panel", i, bw, aw)
		}
	}
}

// TestHandleThink_StateToastNotTranscript: the #162 exemplar emitter —
// state changes are toasts, the bare query stays in the transcript.
func TestHandleThink_StateToastNotTranscript(t *testing.T) {
	m := layoutTestModel(t, 120, 40)

	m2, _ := m.handleThinkCmd("expand")
	if !toastMentions(m2, "reasoning: all blocks expanded") {
		t.Errorf("expected an expand toast, got %#v", m2.toastVisible)
	}
	if strings.Contains(m2.transcript.String(), "all blocks expanded") {
		t.Errorf("state change must not be appended to the transcript, got %q", m2.transcript.String())
	}

	// Bare query: explicit answer → transcript, no toast.
	m3, _ := m2.handleThinkCmd("")
	if !strings.Contains(m3.transcript.String(), "thinking block(s)") {
		t.Errorf("bare /think must answer in the transcript, got %q", m3.transcript.String())
	}
	if len(m3.toastVisible) != len(m2.toastVisible) {
		t.Errorf("bare /think must not toast, visible %d -> %d", len(m2.toastVisible), len(m3.toastVisible))
	}

	// /think on|off are gone — the answer says so instead of hiding anything.
	m4, _ := m3.handleThinkCmd("off")
	if !strings.Contains(m4.transcript.String(), "on|off are gone") {
		t.Errorf("/think off must explain the removal, got %q", m4.transcript.String())
	}
}

// TestAutoOpenPanel_ToastsOnlyOnTransition: ADR-0044 auto-opens fire on every
// workflow chunk — only the closed→open transition may notify, and a panel
// the user already opened manually (panelManualOverride) stays silent.
func TestAutoOpenPanel_ToastsOnlyOnTransition(t *testing.T) {
	m := layoutTestModel(t, 120, 40)

	m.autoOpenPanel(regionBackground)
	if !toastMentions(m, "background panel opened") {
		t.Errorf("expected an auto-open toast, got %#v", m.toastVisible)
	}
	firstCount := len(m.toastHistory)

	// Repeated calls while open (every workflow chunk) stay silent.
	m.autoOpenPanel(regionBackground)
	if len(m.toastHistory) != firstCount {
		t.Errorf("auto-open while already open must not toast, history %d -> %d", firstCount, len(m.toastHistory))
	}

	// A panel the user manages manually is never auto-opened at all.
	m2 := layoutTestModel(t, 120, 40)
	m2.panelBackground = true
	m2.autoOpenPanel(regionBackground)
	if len(m2.toastHistory) != 0 {
		t.Errorf("auto-open must respect manual panel state, got %#v", m2.toastHistory)
	}
}

// TestNotificationsCmd: history view (timestamps + hints), clear, and the
// empty case — the /notifications command required by issue #162.
func TestNotificationsCmd(t *testing.T) {
	m := layoutTestModel(t, 120, 40)

	// Empty history.
	updated, _ := m.handleSlashInput("/notifications", "")
	got := updated.(model)
	if !strings.Contains(got.transcript.String(), "no notifications recorded") {
		t.Errorf("empty history response, got %q", got.transcript.String())
	}

	// Record two events, then view.
	m = got
	m.toastHistory = nil
	m.notify("memory panel opened", "/panel memory")
	m.notify("config reloaded", "/config")
	updated, _ = m.handleSlashInput("/notifications", "")
	got = updated.(model)
	out := got.transcript.String()
	for _, want := range []string{"notification history", "memory panel opened", "/panel memory", "config reloaded", "/config"} {
		if !strings.Contains(out, want) {
			t.Errorf("history output missing %q, got %q", want, out)
		}
	}
	if !regexp.MustCompile(`\d{2}:\d{2}:\d{2}`).MatchString(out) {
		t.Errorf("history output must carry timestamps, got %q", out)
	}

	// Clear.
	updated, _ = got.handleSlashInput("/notifications", "clear")
	got = updated.(model)
	if len(got.toastHistory) != 0 {
		t.Errorf("clear must empty history, got %d entries", len(got.toastHistory))
	}
	if !strings.Contains(got.transcript.String(), "notification history cleared") {
		t.Errorf("clear must confirm in the transcript, got %q", got.transcript.String())
	}
}

// TestNotificationsListCmd covers the `list [count]` subcommand: older
// history than toastHistoryShow is unreachable from the bare command alone,
// so `list` with no count must show everything unfiltered, and `list
// <count>` must show exactly the last <count> entries.
func TestNotificationsListCmd(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	total := toastHistoryShow + 10
	for i := range total {
		m.notify(strings.Repeat("event ", 1)+strconv.Itoa(i), "")
	}
	if len(m.toastHistory) != total {
		t.Fatalf("setup: history = %d, want %d", len(m.toastHistory), total)
	}

	// transcript is a shared *strings.Builder across model copies — reset it
	// before each call below so each check sees only that call's output.
	runList := func(rest string) string {
		m.transcript.Reset()
		updated, _ := m.handleSlashInput("/notifications", rest)
		return updated.(model).transcript.String()
	}

	// Bare "list" (no count): entire history, unfiltered, no "(last N)" note.
	out := runList("list")
	if !strings.Contains(out, "event 0") {
		t.Errorf("list with no count must show the oldest entry too, got %q", out)
	}
	if shown := strings.Count(out, "/notifications"); shown != total {
		t.Errorf("list with no count: printed %d entries, want all %d", shown, total)
	}
	if strings.Contains(out, "(last ") {
		t.Errorf("list with no count must not claim a truncated count, got %q", out)
	}

	// "list <count>": exactly the last <count> entries, with the note.
	out = runList("list 5")
	if !strings.Contains(out, "(last 5)") {
		t.Errorf("list 5 must note the truncated count, got %q", out)
	}
	if shown := strings.Count(out, "/notifications"); shown != 5 {
		t.Errorf("list 5: printed %d entries, want 5", shown)
	}
	if strings.Contains(out, "event 0") {
		t.Errorf("list 5 must only show the most recent entries, got %q", out)
	}

	// Invalid count: a clear error, nothing printed as history.
	out = runList("list abc")
	if !strings.Contains(out, "count must be a positive number") {
		t.Errorf("list abc must report an error, got %q", out)
	}
}

// TestNotificationsRegistration pins the pieces that made #164 (the /clear
// alias) necessary the first time: extraction, completion variants, and help
// text must all know about the command — #162 must not reintroduce the gap.
func TestNotificationsRegistration(t *testing.T) {
	cmd, rest, found := extractSlashCommand("/notifications clear")
	if !found || cmd != "/notifications" || rest != "clear" {
		t.Errorf("extractSlashCommand = (%q, %q, %v), want (/notifications, clear, true)", cmd, rest, found)
	}
	if vs, ok := cmdVariants["/notifications"]; !ok || len(vs) < 2 {
		t.Errorf("cmdVariants[/notifications] = %v, want >= 2 variants (bare + clear)", vs)
	}
	if !strings.Contains(interactiveHelp, "/notifications") {
		t.Error("interactiveHelp must document /notifications")
	}
	if !strings.Contains(interactiveHelp, "Ctrl+G") {
		t.Error("interactiveHelp must document the Ctrl+G dismiss binding")
	}
}

// TestToggleLastThinkBlock_CtrlTPath: the Ctrl+T handler goes through
// toggleLastThinkBlock — a per-block fold toggle, with a toast only where
// there is no visible feedback (nothing to toggle).
func TestToggleLastThinkBlock_CtrlTPath(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	m2 := m.toggleLastThinkBlock()
	if !toastMentions(m2, "no thinking blocks") {
		t.Errorf("empty session must toast, got %#v", m2.toastVisible)
	}
	m3 := m2
	m3.appendThinkingStreamed("a thought\n")
	m4 := m3.toggleLastThinkBlock()
	if len(m4.toastVisible) != len(m3.toastVisible) {
		t.Errorf("toggling a block must not toast, visible %d -> %d", len(m3.toastVisible), len(m4.toastVisible))
	}
	if !m4.thinkBlocks[0].expanded || !m4.thinkBlocks[0].userSet {
		t.Errorf("Ctrl+T must open and pin the folded streaming block, got %+v", m4.thinkBlocks[0])
	}
	if strings.Contains(m4.transcript.String(), "reasoning") {
		t.Errorf("toggle must not write to the transcript, got %q", m4.transcript.String())
	}
}

// TestNewModelToastsOnBackgroundJobDone guards the full Update path including
// the wrapper that arms the expiry tick — a regression here means toasts
// would render but never disappear.
func TestNewModelToastsOnBackgroundJobDone(t *testing.T) {
	m := newToastTestModel(t)
	updated, cmd := m.Update(backgroundJobDoneMsg{job: &local.Job{Label: "x", Result: "y"}})
	got := updated.(model)
	if !toastMentions(got, `background agent "x" completed`) {
		t.Fatalf("expected a completion toast, got %#v", got.toastVisible)
	}
	if !got.toastTickArmed {
		t.Error("Update wrapper must arm the expiry tick while toasts are visible")
	}
	if cmd == nil {
		t.Error("expected a tick Cmd to be batched into the Update result")
	}
	// The lifecycle notice itself stays toast-only (ADR-0048), but the job's
	// result is content and lands in the transcript (renderBackgroundJobDoneBlock).
	if !strings.Contains(got.transcript.String(), `background agent "x" completed`) {
		t.Error("the job result block must be appended to the transcript")
	}
}
