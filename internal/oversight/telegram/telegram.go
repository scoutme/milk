// Package telegram implements the oversight.Notifier interface using the
// Telegram Bot API. Messages are sent via plain HTTPS to the sendMessage
// endpoint; updates are routed by a single background polling goroutine.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/scoutme/milk/internal/oversight"
)

const (
	defaultAPIBase     = "https://api.telegram.org"
	defaultPollTimeout = 20 * time.Second
	// maxMessageUnits is the per-message budget in UTF-16 code units (the
	// unit Telegram counts for its 4096-character sendMessage limit), kept a
	// margin below 4096 for entity-parsing subtleties. Messages longer than
	// this are split into ordered chunks — never truncated (issue #253).
	maxMessageUnits = 4000
	// sendQueueSize bounds the outbound queue; the single worker goroutine
	// drains it in FIFO order so multi-chunk messages arrive in order.
	sendQueueSize = 256
	// defaultStatusInterval is how often the status line on the turn-start
	// message re-renders (elapsed counters tick; state changes render
	// immediately instead of waiting for the next tick).
	defaultStatusInterval = 2 * time.Second
	// defaultChatActionInterval re-sends sendChatAction("typing") while a
	// response streams. Telegram expires chat actions after ~5s and limits
	// them to roughly one per 5s per chat — 5.2s sits just above both so
	// refreshes don't 429 themselves (the pre-status-line 4.5s cadence did).
	defaultChatActionInterval = 5200 * time.Millisecond
	// maxBusyBackoff caps the exponential send-failure backoff so a broken
	// endpoint is retried at most once a minute.
	maxBusyBackoff = time.Minute
)

// Busy-status states for the indicator line (issue #286) — what the agent is
// actually doing, not a blanket "typing…".
const (
	statusThinking = iota // turn running, no response text streaming yet
	statusTyping          // response segments are streaming out
	statusWaiting         // a remote ask is waiting for the human
)

// Config holds the Telegram-specific configuration.
type Config struct {
	Token         string
	ChatID        int64
	PermTimeout   time.Duration // how long AskPermission waits for a reply
	TimeoutAction string        // "allow" or "deny" when PermTimeout expires
	APIBase       string        // override for testing; defaults to https://api.telegram.org
	// BusyIndicator shows the agent's business status while a turn runs
	// (issue #286): a status line (💭 thinking… / ⌨️ typing… / ⏳ waiting
	// for you…) edited in place on the turn-start message, plus the native
	// "typing…" chat action while response text streams. Production wires
	// it from remote_oversight.busy_indicator (default on); the zero value
	// here is off so tests that don't ask for it see no extra requests.
	BusyIndicator bool
	// StatusInterval is the status-line re-render period (0 →
	// defaultStatusInterval). Tests shrink it to keep suites fast.
	StatusInterval time.Duration
	// ChatActionInterval is the typing-chat-action refresh period (0 →
	// defaultChatActionInterval).
	ChatActionInterval time.Duration
}

// Notifier sends notifications and forwards permission prompts via Telegram.
// A single background goroutine (started by StartPolling) receives all
// incoming messages and routes them to either a pending permission wait or
// the registered OnInput callback.
type Notifier struct {
	cfg    Config
	client *http.Client

	mu         sync.Mutex
	lastUpdate int64        // getUpdates offset
	permCh     chan string  // non-nil when AskPermission is waiting
	onInput    func(string) // called for non-permission messages when set

	outCh   chan outItem // outbound FIFO, drained by one worker goroutine
	outOnce sync.Once

	// sendMu serializes outbound sendMessage calls and guards the 429
	// cooldown gate below. The queue's single worker already serializes
	// FIFO sends; this lock additionally covers the queue-full overflow
	// path and every sender waiting out a cooldown (issue #325).
	sendMu sync.Mutex
	// notBefore is the earliest time any sendMessage may hit the wire:
	// set from the server's 429 retry_after. While it is in the future
	// send() makes no API calls at all, so an active turn can no longer
	// refresh a flood penalty with failed attempts.
	notBefore time.Time

	// Busy-status state (issue #286): a stack of active turns' status
	// entries — top is the most recent — with one render loop for all of
	// them. Both hosts (TUI runTurn, ACP dispatch) and the workflow runner
	// pair NotifyTurnStart/NotifyTurnDone on every path; overlapping turns
	// restore in LIFO order (worst case for a rare out-of-order end: an
	// early pop silences a still-running turn's line — never a wrong
	// restore, since each entry restores only its own message).
	statusMu     sync.Mutex
	statuses     []*turnStatus
	statusCancel context.CancelFunc
}

// New creates a Notifier. Call StartPolling to begin receiving messages.
func New(cfg Config) *Notifier {
	if cfg.APIBase == "" {
		cfg.APIBase = defaultAPIBase
	}
	if cfg.PermTimeout <= 0 {
		cfg.PermTimeout = 2 * time.Minute
	}
	if cfg.TimeoutAction == "" {
		cfg.TimeoutAction = "deny"
	}
	if cfg.StatusInterval <= 0 {
		cfg.StatusInterval = defaultStatusInterval
	}
	if cfg.ChatActionInterval <= 0 {
		cfg.ChatActionInterval = defaultChatActionInterval
	}
	return &Notifier{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
		outCh:  make(chan outItem, sendQueueSize),
	}
}

// SetOnInput registers a callback invoked for every incoming message that is
// not consumed by a pending AskPermission call. Safe to call before or after
// StartPolling. Pass nil to unregister.
func (n *Notifier) SetOnInput(cb func(string)) {
	n.mu.Lock()
	n.onInput = cb
	n.mu.Unlock()
}

// StartPolling launches the background update-polling loop. It runs until ctx
// is cancelled. Safe to call once after New.
func (n *Notifier) StartPolling(ctx context.Context) {
	go n.pollLoop(ctx)
}

// pollLoop is the single getUpdates consumer. It routes each message to either
// the pending permission channel or the onInput callback.
func (n *Notifier) pollLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		n.mu.Lock()
		offset := n.lastUpdate + 1
		n.mu.Unlock()

		params := url.Values{
			"offset":  []string{strconv.FormatInt(offset, 10)},
			"timeout": []string{strconv.Itoa(int(defaultPollTimeout.Seconds()))},
			"limit":   []string{"10"},
		}

		var result struct {
			OK     bool `json:"ok"`
			Result []struct {
				UpdateID int64 `json:"update_id"`
				Message  *struct {
					Chat struct {
						ID int64 `json:"id"`
					} `json:"chat"`
					Text string `json:"text"`
				} `json:"message"`
			} `json:"result"`
		}

		pollCtx, cancel := context.WithTimeout(ctx, defaultPollTimeout+5*time.Second)
		err := n.apiGet(pollCtx, "getUpdates", params, &result)
		cancel()

		if err != nil || !result.OK {
			// Back off briefly on error to avoid hammering the API.
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}

		for _, upd := range result.Result {
			n.mu.Lock()
			if upd.UpdateID > n.lastUpdate {
				n.lastUpdate = upd.UpdateID
			}
			permCh := n.permCh
			cb := n.onInput
			n.mu.Unlock()

			if upd.Message == nil || upd.Message.Chat.ID != n.cfg.ChatID || upd.Message.Text == "" {
				continue
			}
			text := upd.Message.Text

			if permCh != nil {
				// Non-blocking send — if the channel is full the permission
				// waiter already received a reply; treat as input.
				select {
				case permCh <- text:
					continue
				default:
				}
			}
			if cb != nil {
				go cb(text)
			}
		}
	}
}

// Ping sends a getMe request and returns an error if unreachable or token invalid.
func (n *Notifier) Ping(ctx context.Context) error {
	var result struct {
		OK bool `json:"ok"`
	}
	if err := n.apiGet(ctx, "getMe", nil, &result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("telegram: getMe returned ok=false")
	}
	return nil
}

// turnStartMessage renders the shared 🔄 turn-start notice.
func turnStartMessage(agent, target, prompt string) string {
	short := trimPreview(prompt, 117, "...")
	return fmt.Sprintf("🔄 *%s* → %s\n`%s`", escMD(agent), escMD(target), escMD(short))
}

// NotifyTurnStart relays the turn-start notice and engages the busy
// indicator (issue #284): a "typing…" chat action re-sent every ~4.5s until
// NotifyTurnDone. The indicator stops on its own when the turn context is
// cancelled as well.
func (n *Notifier) NotifyTurnStart(_ context.Context, agent, target, prompt string) {
	base := turnStartMessage(agent, target, prompt)
	ts := n.startStatus(base)
	if ts == nil {
		n.sendAsync(base)
		return
	}
	// The status line lives on this message — bind its id once the send
	// queue reports it (attach renders the first 💭 thinking… immediately).
	n.sendAsyncWithID(base, func(msgID int64) { n.attachStatus(ts, msgID) })
}

// Announce sends a turn-start-style notice for events that are not turns
// (startup, /setup confirmations): the same 🔄 message, without engaging
// the busy indicator — no NotifyTurnDone ever follows these, so pairing
// busy state with them would leave "typing…" running forever. cmd/milk
// routes non-turn notices through notifyAnnouncement's optional interface.
func (n *Notifier) Announce(agent, target, prompt string) {
	n.sendAsync(turnStartMessage(agent, target, prompt))
}

func (n *Notifier) NotifyToolUse(_ context.Context, toolName, summary string) {
	// Tool traffic means the agent is working between outputs. The state
	// flips even when notify_tools gates the message itself (the gate sits
	// in cmd/milk's callers), so the status line stays truthful either way.
	n.setStatus(statusThinking)
	n.sendAsync(toolUseMessage(toolName, summary))
}

func (n *Notifier) NotifyToolResult(_ context.Context, toolName, summary string, isError bool) {
	// State first: an empty summary still means a tool ran.
	n.setStatus(statusThinking)
	if summary == "" {
		return
	}
	n.sendAsync(toolResultMessage(toolName, summary, isError))
}

func (n *Notifier) NotifyTurnDone(_ context.Context, agent string, err error) {
	n.stopStatus()
	switch {
	case err == nil:
		// Successful turns end silently, as before.
	case errors.Is(err, context.Canceled):
		// Interrupted by the user (Ctrl+C, /interrupt, session/cancel) —
		// not a failure; say so plainly instead of ❌ error: …
		n.sendAsync("⏹ *" + escMD(agent) + "* turn interrupted")
	default:
		n.sendAsync(fmt.Sprintf("❌ *%s* error: `%s`", escMD(agent), escMD(err.Error())))
	}
}

// --- busy indicator (issue #286) ---
//
// While a turn runs, the turn-start message carries a status line —
// 💭 thinking… / ⌨️ typing… / ⏳ waiting for you… with an elapsed counter —
// updated in place by editMessageText. No extra chat rows: the line lives
// on the existing 🔄 message and is stripped again when the turn ends.
// The native sendChatAction("typing") runs only while response text
// streams, where its fixed label is accurate (and works even if the
// turn-start message id never arrives).

// turnStatus is one active turn's indicator state.
type turnStatus struct {
	mu       sync.Mutex
	msgID    int64     // 0 until the send queue reports the message id (forever, if the send failed)
	base     string    // turn-start message without the status line; restore target
	state    int       // statusThinking | statusTyping | statusWaiting
	prev     int       // state to return to after a wait
	waitFrom time.Time // when the current wait began (waiting-line elapsed)
	t0       time.Time // turn start (thinking/typing elapsed)
	last     string    // last rendered full text (dedupe + restore guard)
	nextEdit time.Time // 429 backoff: no edits before this
	dead     bool      // edit failed irrecoverably (message deleted) — stop touching it
	done     bool      // turn ended — renders are refused, restore wins
}

// sendTyping outcomes.
const (
	typingOK          = iota // accepted
	typingRateLimited        // 429: retry after the server's retry_after
	typingFailed             // transport or API error: exponential backoff
)

// startStatus pushes a turn's status entry and spawns the render loop for
// the first one. Returns nil when the indicator is disabled.
func (n *Notifier) startStatus(base string) *turnStatus {
	if !n.cfg.BusyIndicator {
		return nil
	}
	ts := &turnStatus{base: base, t0: time.Now(), waitFrom: time.Now()}
	n.statusMu.Lock()
	n.statuses = append(n.statuses, ts)
	if len(n.statuses) == 1 {
		ctx, cancel := context.WithCancel(context.Background())
		n.statusCancel = cancel
		go n.statusLoop(ctx)
	}
	n.statusMu.Unlock()
	return ts
}

// stopStatus pops the most recent turn's entry (LIFO — see the field
// comment), refuses its further renders, strips its status line, and stops
// the render loop with the last entry.
func (n *Notifier) stopStatus() {
	n.statusMu.Lock()
	var done *turnStatus
	if last := len(n.statuses) - 1; last >= 0 {
		done = n.statuses[last]
		n.statuses = n.statuses[:last]
	}
	if len(n.statuses) == 0 && n.statusCancel != nil {
		n.statusCancel()
		n.statusCancel = nil
	}
	n.statusMu.Unlock()
	if done == nil {
		return
	}
	done.mu.Lock()
	done.done = true
	msgID, base, last := done.msgID, done.base, done.last
	need := msgID != 0 && !done.dead && last != "" && last != base
	if need {
		done.last = base
	}
	done.mu.Unlock()
	if need {
		// Async: a slow Telegram must not stall turn end. done already
		// blocks every later render, so no status line can reappear.
		go n.editRaw(msgID, base)
	}
}

// topStatus returns the most recent active turn's entry, or nil.
func (n *Notifier) topStatus() *turnStatus {
	n.statusMu.Lock()
	defer n.statusMu.Unlock()
	if len(n.statuses) == 0 {
		return nil
	}
	return n.statuses[len(n.statuses)-1]
}

// setStatus moves the active turn's indicator to a new business state and
// renders it immediately (no waiting for the next tick). No-op with no
// turn in flight (e.g. background-job asks, which have no turn-start
// message) or when the indicator is disabled.
func (n *Notifier) setStatus(state int) {
	ts := n.topStatus()
	if ts == nil {
		return
	}
	ts.mu.Lock()
	ts.state = state
	ts.mu.Unlock()
	n.renderStatus(ts)
}

// attachStatus binds the turn-start message id (reported by the send
// queue) and renders the first status line right away.
func (n *Notifier) attachStatus(ts *turnStatus, msgID int64) {
	ts.mu.Lock()
	ts.msgID = msgID
	ts.mu.Unlock()
	n.renderStatus(ts)
}

// beginWait/endWait bracket a remote ask: the status line switches to
// "⏳ waiting for you…" while milk waits for the human — the agent isn't
// working then — and returns to the previous state when the answer
// arrives (or the ask times out).
func (ts *turnStatus) beginWait(n *Notifier) {
	ts.mu.Lock()
	ts.prev = ts.state
	ts.state = statusWaiting
	ts.waitFrom = time.Now()
	ts.mu.Unlock()
	n.renderStatus(ts)
}

func (ts *turnStatus) endWait(n *Notifier) {
	ts.mu.Lock()
	ts.state = ts.prev
	ts.mu.Unlock()
	n.renderStatus(ts)
}

// currentState reads the entry's state for the loop's chat-action gate.
func (ts *turnStatus) currentState() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.state
}

// renderStatus renders the entry's status line now, skipping entries that
// are unattached, dead, done, in 429 backoff, or unchanged.
func (n *Notifier) renderStatus(ts *turnStatus) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	n.renderLocked(ts)
}

// renderLocked renders the status line; caller holds ts.mu.
func (n *Notifier) renderLocked(ts *turnStatus) {
	if ts.done || ts.dead || ts.msgID == 0 {
		return
	}
	now := time.Now()
	if now.Before(ts.nextEdit) {
		return
	}
	since := ts.t0
	if ts.state == statusWaiting {
		since = ts.waitFrom
	}
	elapsed := int(now.Sub(since).Seconds())
	if elapsed < 0 {
		elapsed = 0
	}
	var line string
	switch ts.state {
	case statusTyping:
		line = fmt.Sprintf("⌨️ typing… %d:%02d", elapsed/60, elapsed%60)
	case statusWaiting:
		line = fmt.Sprintf("⏳ waiting for you… %d:%02d", elapsed/60, elapsed%60)
	default:
		line = fmt.Sprintf("💭 thinking… %d:%02d", elapsed/60, elapsed%60)
	}
	full := ts.base + "\n" + line
	if full == ts.last {
		return
	}
	if n.editStatus(ts, ts.msgID, full) {
		ts.last = full
	}
}

// editStatus edits the turn-start message to text. Mutates the entry's
// backoff/dead state on failure; caller holds ts.mu.
func (n *Notifier) editStatus(ts *turnStatus, msgID int64, text string) bool {
	ok, retry, dead := n.editRawResult(msgID, text)
	if dead {
		ts.dead = true
		return false
	}
	if retry > 0 {
		ts.nextEdit = time.Now().Add(retry)
		return false
	}
	return ok
}

// editRaw edits without any entry state (fire-and-forget restore path).
func (n *Notifier) editRaw(msgID int64, text string) {
	_, _, _ = n.editRawResult(msgID, text)
}

// editRawResult performs editMessageText. retry > 0 requests backoff
// (429 retry_after + slack); dead marks the message uneditable (deleted,
// wrong chat, …). "message is not modified" is benign and counts as ok.
func (n *Notifier) editRawResult(msgID int64, text string) (ok bool, retry time.Duration, dead bool) {
	params := url.Values{
		"chat_id":    []string{strconv.FormatInt(n.cfg.ChatID, 10)},
		"message_id": []string{strconv.FormatInt(msgID, 10)},
		"text":       []string{text},
		"parse_mode": []string{"MarkdownV2"},
	}
	var res struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := n.apiGet(ctx, "editMessageText", params, &res); err != nil {
		return false, 0, false // transport: the next tick retries
	}
	if res.OK {
		return true, 0, false
	}
	if res.ErrorCode == 429 {
		r := time.Duration(res.Parameters.RetryAfter)*time.Second + 200*time.Millisecond
		if res.Parameters.RetryAfter <= 0 {
			r = time.Second
		}
		return false, r, false
	}
	if res.ErrorCode == 400 && strings.Contains(res.Description, "not modified") {
		return true, 0, false
	}
	return false, 0, true
}

// statusLoop is the single render/chat-action goroutine for all active
// turns: re-renders the top entry's status line every StatusInterval and,
// while that entry streams response text, refreshes the native typing
// chat action on its own (slower) cadence with429/error backoff.
func (n *Notifier) statusLoop(ctx context.Context) {
	next := time.Duration(0) // first pass immediately
	actionWait := n.cfg.ChatActionInterval
	lastAction := time.Time{}
	fails := 0
	for {
		if next > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(next):
			}
		} else if ctx.Err() != nil {
			return
		}
		next = n.cfg.StatusInterval
		entry := n.topStatus()
		if entry == nil {
			continue
		}
		n.renderStatus(entry)
		if entry.currentState() != statusTyping || time.Since(lastAction) < actionWait {
			continue
		}
		lastAction = time.Now()
		switch kind, retry := n.sendTyping(ctx); kind {
		case typingOK:
			fails = 0
			actionWait = n.cfg.ChatActionInterval
		case typingRateLimited:
			fails = 0
			actionWait = retry + 200*time.Millisecond
			if actionWait < n.cfg.ChatActionInterval {
				actionWait = n.cfg.ChatActionInterval
			}
		case typingFailed:
			fails++
			backoff := n.cfg.ChatActionInterval
			for i := 1; i < fails && backoff < maxBusyBackoff; i++ {
				backoff *= 2
			}
			if backoff > maxBusyBackoff {
				backoff = maxBusyBackoff
			}
			actionWait = backoff
		}
	}
}

// sendTyping issues one sendChatAction("typing"). Returns the outcome plus,
// for a rate limit, how long Telegram asked us to wait (≥ 0).
func (n *Notifier) sendTyping(ctx context.Context) (int, time.Duration) {
	params := url.Values{
		"chat_id": []string{strconv.FormatInt(n.cfg.ChatID, 10)},
		"action":  []string{"typing"},
	}
	var res struct {
		OK         bool `json:"ok"`
		ErrorCode  int  `json:"error_code"`
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := n.apiGet(ctx, "sendChatAction", params, &res); err != nil {
		return typingFailed, 0
	}
	if res.OK {
		return typingOK, 0
	}
	if res.ErrorCode == 429 && res.Parameters.RetryAfter > 0 {
		return typingRateLimited, time.Duration(res.Parameters.RetryAfter) * time.Second
	}
	return typingFailed, 0
}

// Quiet tool-line formatting (issue #290, ADR-0070): the TUI dims tool
// hints (ADR-0026) and Telegram has no dim, so the message uses the
// affordances it does have — italic tool name + inline-code summary, no
// bold, single line. Error results stay bold: a failure is never quietly
// de-emphasized. User-facing messages (🔄 turn start, 💬 response,
// 🔐/❓ asks, the busy-status line) keep full weight.
const (
	// toolUseSummaryCap bounds the tool-use argument preview: it ships
	// unchunked, so it must stay far below Telegram's 4096-unit message
	// limit (pre-#290 an oversized preview made the send silently fail).
	toolUseSummaryCap = 200
	// toolResultSummaryCap is the long-standing result cap (issue #253).
	toolResultSummaryCap = 497
)

// codeText flattens a summary for an inline code span: newlines become
// " ⏎ " (multi-line MarkdownV2 code spans parse unreliably), then trim to
// the rune cap — trimPreview never splits a multi-byte character.
func codeText(s string, cap int) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	s = strings.TrimSpace(s)
	return trimPreview(s, cap, "…")
}

// toolUseMessage builds the quiet tool-use line: » _Bash_ · `summary`
// (bare » _Bash_ with no summary — as before, but never bold).
func toolUseMessage(toolName, summary string) string {
	if summary == "" {
		return "» _" + escMD(toolName) + "_"
	}
	return fmt.Sprintf("» _%s_ · `%s`", escMD(toolName), escCode.Replace(codeText(summary, toolUseSummaryCap)))
}

// toolResultMessage builds the tool-result line: 📤 _Bash_ · `summary`,
// or — for a failure — the bold ❌ *Bash* error · `summary`. Callers keep
// the pre-existing rule that an empty result summary sends nothing.
func toolResultMessage(toolName, summary string, isError bool) string {
	if isError {
		if summary == "" {
			return "❌ *" + escMD(toolName) + "* error"
		}
		return fmt.Sprintf("❌ *%s* error · `%s`", escMD(toolName), escCode.Replace(codeText(summary, toolResultSummaryCap)))
	}
	if summary == "" {
		return "📤 _" + escMD(toolName) + "_"
	}
	return fmt.Sprintf("📤 _%s_ · `%s`", escMD(toolName), escCode.Replace(codeText(summary, toolResultSummaryCap)))
}

// NotifyResponse relays the agent's response in full. Text longer than one
// Telegram message is split into ordered chunks (each its own sendMessage),
// never trimmed — see issue #253. Chunks go through the FIFO send queue, so
// they arrive in order, as do back-to-back notifications.
func (n *Notifier) NotifyResponse(_ context.Context, agent, text string) {
	if text == "" {
		return
	}
	// Response text is streaming out — the status line says so (and only
	// then does the native "typing…" chat action run).
	n.setStatus(statusTyping)
	header := "💬 *" + escMD(agent) + "*"
	// Reserve room for the worst-case header suffix " (n/m)" on chunked sends
	// so that header + escaped chunk always fits the Telegram limit.
	budget := maxMessageUnits - utf16Len(header) - 10
	if budget < 256 {
		budget = 256
	}
	chunks := chunkMessage(text, budget)
	for i, chunk := range chunks {
		h := header
		if len(chunks) > 1 {
			h = fmt.Sprintf("💬 *%s* (%d/%d)", escMD(agent), i+1, len(chunks))
		}
		n.sendAsync(h + "\n" + escMD(chunk))
	}
}

// AskPermission sends a formatted permission prompt and waits for a y/n reply
// via the background poll loop. answered is true only for a real reply; a
// timeout (cfg.PermTimeout or ctx) is silence — the returned decision then
// reflects cfg.TimeoutAction, and it is the caller (cmd/milk's ask layer) that
// decides whether that resolves the ask (foreground) or keeps it open until
// its own deadline (background asks — see ADR-0052).
func (n *Notifier) AskPermission(ctx context.Context, req oversight.PermRequest) (oversight.PermDecision, bool) {
	// ⏳ waiting for the human — the agent isn't working while the ask is
	// open, so the status line says that instead of "typing…".
	if ts := n.topStatus(); ts != nil {
		ts.beginWait(n)
		defer ts.endWait(n)
	}

	lines := []string{fmt.Sprintf("🔐 Permission request — *%s*", escMD(req.ToolName))}
	if req.Input != "" {
		lines = append(lines, fmt.Sprintf("`%s`", escMD(req.Input)))
	}
	if req.BlockedPath != "" {
		lines = append(lines, fmt.Sprintf("path: `%s`", escMD(req.BlockedPath)))
	}
	if req.Description != "" {
		lines = append(lines, escMD(req.Description))
	}
	if req.AllowAlways {
		lines = append(lines, "\nReply *y* to allow or *n* to deny (or *a*/*d* to answer all remaining tool asks that way)\\.")
	} else {
		lines = append(lines, "\nReply *y* to allow or *n* to deny\\.")
	}

	// Register the reply channel before the question goes out — same
	// drop-the-fast-reply race as AskInput.
	ch := make(chan string, 1)
	n.mu.Lock()
	n.permCh = ch
	n.mu.Unlock()

	defer func() {
		n.mu.Lock()
		n.permCh = nil
		n.mu.Unlock()
	}()

	n.send(strings.Join(lines, "\n"))

	// finish maps a real reply onto its decision and posts the matching
	// chat confirmation.
	finish := func(reply string) (oversight.PermDecision, bool) {
		dec := parsePermReply(reply, req.AllowAlways)
		switch dec {
		case oversight.PermAllow:
			n.sendAsync("✅ Allowed")
		case oversight.PermAllowAlways:
			n.sendAsync("✅ Allowed (all remaining tool asks)")
		case oversight.PermDenyAlways:
			n.sendAsync("🚫 Denied (all remaining tool asks)")
		default:
			n.sendAsync("🚫 Denied")
		}
		return dec, true
	}
	// quiet withdraws on cancellation — the ask resolved on another surface
	// (the TUI or ACP prompt was answered): that surface shows its own
	// outcome, so posting the timeout notice here would contradict it (it
	// used to fire on every TUI-side answer).
	quiet := func() (oversight.PermDecision, bool) { return oversight.PermTimeout, false }

	if !req.SilenceResolves {
		// Silence is not an answer for this ask (ADR-0052): hold the single
		// prompt slot until a reply or the ask's end — no internal timeout,
		// no timeout notice, and a late reply still answers the ask instead
		// of falling through to the chat-input callback.
		select {
		case reply := <-ch:
			return finish(reply)
		case <-ctx.Done():
			return quiet()
		}
	}
	select {
	case reply := <-ch:
		return finish(reply)
	case <-ctx.Done():
		return quiet()
	case <-time.After(n.cfg.PermTimeout):
		action := "Denied (timeout)"
		if n.cfg.TimeoutAction == "allow" {
			action = "Allowed (timeout)"
		}
		n.sendAsync(fmt.Sprintf("⏱ %s", action))
		if n.cfg.TimeoutAction == "allow" {
			return oversight.PermAllow, false
		}
		return oversight.PermTimeout, false
	}
}

// parsePermReply maps a permission reply onto its decision (issue #302 for
// the sticky variants). Anything not y/yes denies, as ever; a/d are the
// sticky answers and only exist where the ask offered them (allowAlways),
// elsewhere they keep their old meaning (anything but y = deny).
func parsePermReply(reply string, allowAlways bool) oversight.PermDecision {
	r := strings.TrimSpace(strings.ToLower(reply))
	switch {
	case r == "y" || r == "yes":
		return oversight.PermAllow
	case allowAlways && (r == "a" || r == "allow" || r == "always"):
		return oversight.PermAllowAlways
	case allowAlways && (r == "d" || r == "deny" || r == "never"):
		return oversight.PermDenyAlways
	}
	return oversight.PermDeny
}

// AskInput sends an ask_user question and waits for a free-text reply via the
// background poll loop — the same single prompt slot as AskPermission (the
// caller serializes remote asks; see cmd/milk's oversight ask mutex). The
// reply is the answer exactly as typed. Silence (cfg.PermTimeout or ctx) is
// not an answer: answered is false and the caller keeps waiting on its other
// surfaces or the timed answer — unlike permissions, questions have no
// timeout action.
func (n *Notifier) AskInput(ctx context.Context, req oversight.InputRequest) (string, bool) {
	// Same as AskPermission: the indicator must say "waiting for you", not
	// "typing…", for the whole ask.
	if ts := n.topStatus(); ts != nil {
		ts.beginWait(n)
		defer ts.endWait(n)
	}

	lines := []string{fmt.Sprintf("❓ *%s*", escMD(req.Question))}
	if len(req.Options) > 0 {
		opts := make([]string, len(req.Options))
		for i, o := range req.Options {
			opts[i] = escMD(o)
		}
		lines = append(lines, "Options: "+strings.Join(opts, " · "))
	}
	lines = append(lines, "\nReply with your answer\\.")

	// Register the reply channel before the question goes out: a reply
	// that arrives between send and registration would otherwise be
	// dropped by the poll loop (race that loses fast answers — and a
	// deterministic loss against a fast bot/relay).
	ch := make(chan string, 1)
	n.mu.Lock()
	n.permCh = ch
	n.mu.Unlock()

	defer func() {
		n.mu.Lock()
		n.permCh = nil
		n.mu.Unlock()
	}()

	n.send(strings.Join(lines, "\n"))

	// Hold until a reply or the ask's end: questions have no timeout action
	// and silence is never an answer (ADR-0052). An internal timeout here
	// would release the single prompt slot while the question is still
	// open, so a later reply would fall through to the chat-input callback
	// and be injected as a typed message.
	select {
	case reply := <-ch:
		return reply, true
	case <-ctx.Done():
		return "", false
	}
}

// send sends a MarkdownV2-formatted message synchronously, ignoring errors.
// outItem is one queued outbound message. onID, when set, receives the
// new message's id after a successful send — the busy indicator binds it
// to edit the turn-start message later (issue #286).
type outItem struct {
	text string
	onID func(int64)
}

// Outbound-send 429 policy (issue #325): maxInlineSendWait bounds how long
// send() will sleep inside the queue worker before giving up on a message —
// Telegram's small penalties are a few seconds (wait + retry, deliver), while
// flood penalties run to hours (drop, gate the wire). maxSendRetries caps
// repeated short 429s on one message so a hostile server can't pin the queue.
const (
	maxInlineSendWait = 10 * time.Second
	maxSendRetries    = 3
)

// send delivers one message synchronously and returns its message_id (0 on
// failure). Every call passes the 429 cooldown gate first (issue #325):
// short penalties are waited out and the send retried, long ones drop the
// message without touching the wire — before this, a 429 silently dropped
// the message and the next queued send fired immediately, escalating the
// flood penalty for as long as a turn kept producing notifications.
func (n *Notifier) send(text string) int64 {
	n.sendMu.Lock()
	defer n.sendMu.Unlock()

	for attempt := 0; attempt <= maxSendRetries; attempt++ {
		if wait := time.Until(n.notBefore); wait > 0 {
			if wait > maxInlineSendWait {
				// A multi-hour flood penalty: waiting would stall the whole
				// outbound queue, but the gate keeps later sends off the wire
				// until it expires (which is what stops the penalty renewing).
				return 0
			}
			time.Sleep(wait)
		}
		id, retryAfter := n.sendOnce(text)
		if retryAfter <= 0 {
			return id
		}
		n.notBefore = time.Now().Add(retryAfter)
	}
	return 0
}

// sendOnce performs one sendMessage attempt. A 429 carrying retry_after
// returns a non-zero cooldown instead of an id; the caller arms the gate
// and retries (or drops, for penalties longer than maxInlineSendWait).
// Non-429 failures keep the historical behavior: id 0, no retry.
func (n *Notifier) sendOnce(text string) (id int64, retryAfter time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	payload := map[string]any{
		"chat_id":    n.cfg.ChatID,
		"text":       text,
		"parse_mode": "MarkdownV2",
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		n.cfg.APIBase+"/bot"+n.cfg.Token+"/sendMessage",
		bytes.NewReader(body))
	if err != nil {
		return 0, 0
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return 0, 0
	}
	defer resp.Body.Close()
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
		Parameters *struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, 0
	}
	if !out.OK {
		if out.Parameters != nil && out.Parameters.RetryAfter > 0 {
			// Same slack editRawResult applies: hit the wire marginally
			// after the server's deadline, never before it.
			return 0, time.Duration(out.Parameters.RetryAfter)*time.Second + 200*time.Millisecond
		}
		return 0, 0
	}
	return out.Result.MessageID, 0
}

// sendAsync enqueues text on the FIFO outbound queue, started lazily. A single
// worker goroutine drains it, so multi-chunk messages and back-to-back
// notifications always arrive in send order (a goroutine per message — the
// pre-#253 behavior — could reorder chunks).
func (n *Notifier) sendAsync(text string) {
	n.enqueue(outItem{text: text})
}

// sendAsyncWithID is sendAsync, reporting the sent message's id to onID
// (from the queue worker; onID is not called when the send fails).
func (n *Notifier) sendAsyncWithID(text string, onID func(int64)) {
	n.enqueue(outItem{text: text, onID: onID})
}

func (n *Notifier) enqueue(item outItem) {
	n.outOnce.Do(func() {
		go func() {
			for msg := range n.outCh {
				id := n.send(msg.text)
				if msg.onID != nil && id != 0 {
					msg.onID(id)
				}
			}
		}()
	})
	select {
	case n.outCh <- item:
	default:
		// Queue full: deliver best-effort out of band rather than dropping
		// content (order may interleave only in this overflow case).
		go func() {
			id := n.send(item.text)
			if item.onID != nil && id != 0 {
				item.onID(id)
			}
		}()
	}
}

// apiGet calls a Telegram Bot API method with query params and decodes the JSON response.
func (n *Notifier) apiGet(ctx context.Context, method string, params url.Values, out any) error {
	u := n.cfg.APIBase + "/bot" + n.cfg.Token + "/" + method
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// escCode escapes text for the inside of a MarkdownV2 inline code span:
// per Telegram's spec only '\\' and '`' are special there. Full escMD
// would over-escape separators like '-' and '.' inside code with no
// purpose — and every tool summary now lives inside code (issue #290).
var escCode = strings.NewReplacer(`\`, `\\`, "`", "\\`")

// escMD escapes special characters for Telegram MarkdownV2.
func escMD(s string) string {
	replacer := strings.NewReplacer(
		`_`, `\_`, `*`, `\*`, `[`, `\[`, `]`, `\]`,
		`(`, `\(`, `)`, `\)`, `~`, `\~`, "`", "\\`",
		`>`, `\>`, `#`, `\#`, `+`, `\+`, `-`, `\-`,
		`=`, `\=`, `|`, `\|`, `{`, `\{`, `}`, `\}`,
		`.`, `\.`, `!`, `\!`,
	)
	return replacer.Replace(s)
}

// utf16Len returns the length of s in UTF-16 code units — the unit Telegram
// counts against its 4096-character sendMessage limit (astral runes are 2).
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// trimPreview cuts s to at most max runes — rune-safe, so it never splits a
// multi-byte character — and appends suffix to mark the cut.
func trimPreview(s string, max int, suffix string) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + suffix
}

// chunkMessage splits text into chunks whose escaped size fits maxUnits UTF-16
// code units, so header + escaped chunk always fits one Telegram message. It
// breaks at line boundaries when possible, then at whitespace, and only hard
// splits at a rune boundary when an unbroken run exceeds the budget. No chunk
// ever splits a character; the concatenation of chunks is exactly text.
func chunkMessage(text string, maxUnits int) []string {
	if text == "" {
		return nil
	}
	if utf16Len(escMD(text)) <= maxUnits {
		return []string{text}
	}
	var chunks []string
	cur := ""
	curUnits := 0
	flush := func() {
		if cur != "" {
			chunks = append(chunks, cur)
			cur = ""
			curUnits = 0
		}
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		if line == "" {
			continue
		}
		le := utf16Len(escMD(line))
		if cur == "" && le <= maxUnits {
			cur, curUnits = line, le
			continue
		}
		if cur != "" && curUnits+le <= maxUnits {
			cur += line
			curUnits += le
			continue
		}
		flush()
		if le <= maxUnits {
			cur, curUnits = line, le
			continue
		}
		// A single line exceeds the budget on its own.
		for _, piece := range splitLongLine(line, maxUnits) {
			chunks = append(chunks, piece)
		}
	}
	flush()
	return chunks
}

// splitLongLine splits one over-long line into pieces each fitting maxUnits
// (escaped). It prefers cutting at whitespace (when at least half the fitted
// run is kept) and falls back to hard rune-boundary splits.
func splitLongLine(line string, maxUnits int) []string {
	var pieces []string
	r := []rune(line)
	for len(r) > 0 {
		if utf16Len(escMD(string(r))) <= maxUnits {
			pieces = append(pieces, string(r))
			break
		}
		acc, fit, lastSpace := 0, 0, 0
		for i, ch := range r {
			w := utf16Len(escMD(string(ch)))
			if acc+w > maxUnits {
				fit = i
				break
			}
			acc += w
			fit = i + 1
			if ch == ' ' || ch == '\t' {
				lastSpace = i + 1
			}
		}
		if fit == 0 {
			fit = 1 // always make progress
		}
		if lastSpace > 0 && lastSpace >= fit/2 {
			fit = lastSpace
		}
		pieces = append(pieces, string(r[:fit]))
		r = r[fit:]
	}
	return pieces
}
