package telegram

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/oversight"
)

// Delivery-path hardening tests (issues #439, #449): escMD backslash,
// plain-text fallback for parse rejections, honest failure accounting,
// proactive pacing, the unified penalty gate, flattened code spans.

// TestEscMDEscapesBackslash pins the content-corruption fix (issue #439,
// bug 1): MarkdownV2 treats '\' as an escape introducer, so a literal one
// must become '\\' — otherwise C:\Users rendered as C:Users and a bad
// pairing (trailing '\', '\[' …) got the whole message rejected with 400
// "can't parse entities".
func TestEscMDEscapesBackslash(t *testing.T) {
	cases := map[string]string{
		`C:\Users\test\file.txt`: `C:\\Users\\test\\file\.txt`,
		`\d+`:                    `\\d\+`,
		`\`:                      `\\`,
		`trailing \` + ` `:       `trailing \\ `,
		`{"a": "x"}`:             `\{"a": "x"\}`,
	}
	for in, want := range cases {
		if got := escMD(in); got != want {
			t.Errorf("escMD(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEscMDRoundTripsThroughTelegramParser checks the invariant behind the
// fix: Telegram's MarkdownV2 unescaper (backslash + special → that literal
// character) must reproduce the original text for every input shape — i.e.
// escaping is now lossless, not merely different.
func TestEscMDRoundTripsThroughTelegramParser(t *testing.T) {
	unescape := func(s string) string {
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				b.WriteByte(s[i])
				continue
			}
			b.WriteByte(s[i])
		}
		return b.String()
	}
	inputs := []string{
		`C:\Users\test\file.txt`,
		`regex \d+ and \\ double`,
		`back\` + "`tick and _stars_ and [link](url)",
		`mixed \ #hash - dash . dot ! bang > quote`,
		`trailing \`,
		`{a}|b=c~d+e*f`,
	}
	for _, in := range inputs {
		if got := unescape(escMD(in)); got != in {
			t.Errorf("round trip lost data: in %q, escaped %q, unescaped %q", in, escMD(in), got)
		}
	}
}

// parseRejectServer answers sendMessage: MarkdownV2 attempts get 400
// "can't parse entities" (the exact rejection that used to vanish), plain
// attempts succeed. Returns the server plus call counters.
func parseRejectServer(t *testing.T) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var mdCalls, plainCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text      string `json:"text"`
			ParseMode string `json:"parse_mode"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload.ParseMode != "" {
			mdCalls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": false, "error_code": 400,
				"description": "Bad Request: can't parse entities: unclosed code block at byte 3",
			})
			return
		}
		plainCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "result": map[string]any{"message_id": 77},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &mdCalls, &plainCalls
}

// TestSend_Parse400FallsBackToPlainText: a MarkdownV2 parse rejection is
// re-sent once without parse_mode — content is never lost to escaping
// (issue #439, bug 2) — and the fallback is counted (bug 5).
func TestSend_Parse400FallsBackToPlainText(t *testing.T) {
	srv, mdCalls, plainCalls := parseRejectServer(t)
	n := New(Config{Token: "t", ChatID: 1, APIBase: srv.URL})

	id := n.send("back\\slash `content`")
	if id != 77 {
		t.Fatalf("id = %d, want 77 — the plain-text fallback must deliver", id)
	}
	if md, plain := mdCalls.Load(), plainCalls.Load(); md != 1 || plain != 1 {
		t.Fatalf("calls: markdown=%d plain=%d, want 1 and 1 (one parse attempt, one fallback)", md, plain)
	}
	if got := n.Stats()["plain_fallback"]; got != 1 {
		t.Errorf("plain_fallback counter = %d, want 1", got)
	}
	if got := n.Stats()["sent"]; got != 1 {
		t.Errorf("sent counter = %d, want 1", got)
	}
}

// TestSend_OtherFailuresCountedNotSilent: non-parse API failures still fail
// fast (no retry storm) but are now counted and logged — bug #2 was exactly
// the silent swallow.
func TestSend_OtherFailuresCountedNotSilent(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": false, "error_code": 403, "description": "Forbidden: bot was blocked by the user",
		})
	}))
	t.Cleanup(srv.Close)
	n := New(Config{Token: "t", ChatID: 1, APIBase: srv.URL})

	if id := n.send("blocked"); id != 0 {
		t.Fatalf("id = %d, want 0", id)
	}
	if c := calls.Load(); c != 1 {
		t.Fatalf("calls = %d, want 1 — non-retryable failures must not loop", c)
	}
	if got := n.Stats()["send_failed"]; got != 1 {
		t.Errorf("send_failed counter = %d, want 1 (the drop must be accounted)", got)
	}
}

// TestSend_FloodDropCounted: a long penalty's drop lands in the counters
// (issue #449, C — the silent-drop half of bug #2), and the 429 itself is
// recorded as a flood wait.
func TestSend_FloodDropCounted(t *testing.T) {
	srv, _ := sendMessageResponder(t, 3600, 42)
	n := New(Config{Token: "t", ChatID: 1, APIBase: srv.URL})

	if id := n.send("flooded"); id != 0 {
		t.Fatalf("id = %d, want 0", id)
	}
	st := n.Stats()
	if st["flood_wait_429"] != 1 {
		t.Errorf("flood_wait_429 = %d, want 1 (the 429 must be recorded)", st["flood_wait_429"])
	}
	if st["flood_dropped"] != 1 {
		t.Errorf("flood_dropped = %d, want 1 (the drop must never be silent)", st["flood_dropped"])
	}
	if st["send_failed"] != 0 {
		t.Errorf("send_failed = %d, want 0 — a gated drop is a penalty drop, not a failure", st["send_failed"])
	}
}

// TestFloodSummaryWhenGateClears: drops during a penalty episode end with
// ONE summary message on the wire once the gate expires (issue #449, C) —
// the remote side learns why output vanished instead of seeing silence.
func TestFloodSummaryWhenGateClears(t *testing.T) {
	var mu atomic.Int64
	var lastText atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		lastText.Store(payload.Text)
		mu.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "result": map[string]any{"message_id": mu.Load()},
		})
	}))
	t.Cleanup(srv.Close)
	n := New(Config{Token: "t", ChatID: 1, APIBase: srv.URL})

	// Simulate an expired episode: one drop recorded while a (now past)
	// gate held, then a fresh send — flushFloodSummary must fire first.
	n.sendMu.Lock()
	n.dropFlooded("lost message", 30*time.Second)
	n.armGate(time.Now().Add(-time.Second)) // already expired
	n.sendMu.Unlock()

	if id := n.send("after the storm"); id == 0 {
		t.Fatal("post-gate send must deliver")
	}
	// The summary goes out asynchronously (it re-enters send after this
	// call releases sendMu) — give it a moment.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if s, _ := lastText.Load().(string); strings.Contains(s, "dropped during a flood penalty") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("flood summary never sent; last text = %q", lastText.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := n.Stats()["flood_dropped"]; got != 1 {
		t.Errorf("flood_dropped = %d, want 1", got)
	}
}

// TestPaceBucketMath pins the lazy-refill pacing arithmetic (issue #449, A)
// with synthetic clocks — no sleeping: burst messages pass free, the next
// one waits exactly one slot, refill restores the burst over time.
func TestPaceBucketMath(t *testing.T) {
	b := tokenBucket{rate: 1.0, burst: 2}
	t0 := time.Now()
	if w := b.pace(t0); w != 0 {
		t.Errorf("1st token wait = %v, want 0 (burst)", w)
	}
	if w := b.pace(t0); w != 0 {
		t.Errorf("2nd token wait = %v, want 0 (burst)", w)
	}
	if w := b.pace(t0); w != time.Second {
		t.Errorf("3rd token wait = %v, want 1s (rate 1/s, empty bucket)", w)
	}
	// A message paced out at t0+1s: the next one arriving right behind it
	// (0.1s later) waits the remaining 0.9s — one slot per second (±float
	// truncation).
	if w := b.pace(t0.Add(1100 * time.Millisecond)); w < 899*time.Millisecond || w > 901*time.Millisecond {
		t.Errorf("post-wait token wait = %v, want ~900ms", w)
	}
	// Disabled bucket never waits.
	off := tokenBucket{}
	if w := off.pace(t0); w != 0 {
		t.Errorf("zero-rate bucket waited %v", w)
	}
}

// TestPacingOnlyOnTheRealAPI: production (real api.telegram.org) is paced;
// test servers are not — the suites must never sleep between sends, and
// tests exercise pacing by setting the buckets directly (issue #449, A).
func TestPacingOnlyOnTheRealAPI(t *testing.T) {
	prod := New(Config{Token: "t", ChatID: 1}) // APIBase default
	if prod.chatPace.rate != chatPrivateRate || prod.globalPace.rate != globalRate {
		t.Fatalf("production buckets: chat=%v global=%v, want %v/%v",
			prod.chatPace.rate, prod.globalPace.rate, chatPrivateRate, globalRate)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	t.Cleanup(srv.Close)
	fake := New(Config{Token: "t", ChatID: 1, APIBase: srv.URL})
	if fake.chatPace.rate != 0 || fake.globalPace.rate != 0 {
		t.Fatalf("test-server buckets must be disabled, got chat=%v global=%v",
			fake.chatPace.rate, fake.globalPace.rate)
	}
}

// TestSendPacingSpacesSends: with a rate injected, consecutive sends are
// spaced by the bucket (issue #449, A) — burst first, then one slot's wait.
func TestSendPacingSpacesSends(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":9}}`))
	}))
	t.Cleanup(srv.Close)
	n := New(Config{Token: "t", ChatID: 1, APIBase: srv.URL})
	n.chatPace = tokenBucket{rate: 20, burst: 1} // one free token, then 50ms slots
	n.globalPace = tokenBucket{}

	start := time.Now()
	n.send("one")
	n.send("two")
	elapsed := time.Since(start)
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("two sends took %v — the second must wait a pacing slot (~50ms)", elapsed)
	}
	if w := n.Stats()["paced_wait"]; w != 1 {
		t.Errorf("paced_wait = %d, want 1", w)
	}
}

// TestGroupLikeUsesGroupBucket: once the chat is known to be a group, the
// slower 20/min bucket governs instead of the private one (issue #449, A).
func TestGroupLikeUsesGroupBucket(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":9}}`))
	}))
	t.Cleanup(srv.Close)
	n := New(Config{Token: "t", ChatID: 1, APIBase: srv.URL})
	n.chatPace = tokenBucket{rate: 1000, burst: 100} // free — must not be the one gating
	n.chatPaceGroup = tokenBucket{rate: 5, burst: 1} // 200ms slots
	n.globalPace = tokenBucket{}
	n.groupLike.Store(true)

	start := time.Now()
	n.send("g1")
	n.send("g2")
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("group sends took %v — the group bucket (~200ms) must pace them", elapsed)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

// TestTurnStartMessageFlattensMultilinePrompt: the prompt preview is a
// single-line code span (issue #439, bug 4) — newline flattened to ⏎ and
// escaped for code (escCode), never raw escMD inside the span.
func TestTurnStartMessageFlattensMultilinePrompt(t *testing.T) {
	msg := turnStartMessage("agent-x", "local", "line one\nline two with `ticks` and - dashes")
	if strings.Contains(msg, "\n`") && strings.Count(msg, "\n") > 1 {
		t.Errorf("prompt preview spans multiple lines: %q", msg)
	}
	if !strings.Contains(msg, " ⏎ ") {
		t.Errorf("newline must flatten to ⏎: %q", msg)
	}
	if strings.Contains(msg, "`line one\\n") || strings.Contains(msg, "one\nline") {
		t.Errorf("raw newline leaked into the code span: %q", msg)
	}
	// escCode semantics inside the span: backtick escaped, dashes NOT.
	if !strings.Contains(msg, "\\`") {
		t.Errorf("backticks inside the preview must be code-escaped: %q", msg)
	}
}

// TestPermPromptFlattensMultilineInput: permission inputs run through
// codeText + escCode (issue #439, bug 4) — multi-line input collapses, and
// both the input and the blocked path are code-escaped.
func TestPermPromptFlattensMultilineInput(t *testing.T) {
	lines := permPromptLines(oversightPermFixture())
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "first line\nsecond") {
		t.Errorf("multi-line input leaked a raw newline: %q", joined)
	}
	if !strings.Contains(joined, " ⏎ ") {
		t.Errorf("input newline must flatten to ⏎: %q", joined)
	}
	if !strings.Contains(joined, "back\\`slash") {
		t.Errorf("backtick in input must be code-escaped: %q", joined)
	}
}

// oversightPermFixture builds the ask that used to ship raw newlines.
func oversightPermFixture() oversight.PermRequest {
	return oversight.PermRequest{
		ToolName: "Bash",
		Input:    "cat report\nback`slash",
	}
}

// TestPollBackoffDelay pins the getUpdates failure backoff curve (issue
// #449, F): 3s doubling to a 30s ceiling.
func TestPollBackoffDelay(t *testing.T) {
	cases := map[int]time.Duration{
		1: 3 * time.Second,
		2: 6 * time.Second,
		4: 24 * time.Second,
		5: 30 * time.Second,
		9: 30 * time.Second,
	}
	for fails, want := range cases {
		if got := pollBackoffDelay(fails); got != want {
			t.Errorf("pollBackoffDelay(%d) = %v, want %v", fails, got, want)
		}
	}
}

// TestGateArmsOnlyForward (issue #449, B): armGate never moves the shared
// gate backwards — an edit's small retry can't cut ahead of a send's larger
// penalty.
func TestGateArmsOnlyForward(t *testing.T) {
	n := New(Config{Token: "t", ChatID: 1, APIBase: "http://127.0.0.1:0"})
	base := time.Now().Add(30 * time.Second)
	n.armGate(base)
	n.armGate(base.Add(-10 * time.Second)) // older — must be ignored
	if got := n.gateUntil(); !got.Equal(base) {
		t.Errorf("gate after back-dated arm = %v, want %v (only-forward)", got, base)
	}
	n.armGate(base.Add(5 * time.Second))
	if got := n.gateUntil(); !got.Equal(base.Add(5 * time.Second)) {
		t.Errorf("gate after forward arm = %v, want %v", got, base.Add(5*time.Second))
	}
}
