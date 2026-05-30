// Package telegram implements the oversight.Notifier interface using the
// Telegram Bot API. Messages are sent via plain HTTPS to the sendMessage
// endpoint; updates are routed by a single background polling goroutine.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
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
)

// Config holds the Telegram-specific configuration.
type Config struct {
	Token         string
	ChatID        int64
	PermTimeout   time.Duration // how long AskPermission waits for a reply
	TimeoutAction string        // "allow" or "deny" when PermTimeout expires
	APIBase       string        // override for testing; defaults to https://api.telegram.org
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
	return &Notifier{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
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

func (n *Notifier) NotifyTurnStart(_ context.Context, agent, target, prompt string) {
	short := prompt
	if len(short) > 120 {
		short = short[:117] + "..."
	}
	msg := fmt.Sprintf("🔄 *%s* → %s\n`%s`", escMD(agent), escMD(target), escMD(short))
	n.sendAsync(msg)
}

func (n *Notifier) NotifyToolUse(_ context.Context, toolName, summary string) {
	var msg string
	if summary != "" {
		msg = fmt.Sprintf("⚙ *%s*: `%s`", escMD(toolName), escMD(summary))
	} else {
		msg = fmt.Sprintf("⚙ *%s*", escMD(toolName))
	}
	n.sendAsync(msg)
}

func (n *Notifier) NotifyTurnDone(_ context.Context, agent string, err error) {
	if err != nil {
		n.sendAsync(fmt.Sprintf("❌ *%s* error: `%s`", escMD(agent), escMD(err.Error())))
	}
}

func (n *Notifier) NotifyResponse(_ context.Context, agent, text string) {
	if text == "" {
		return
	}
	const maxLen = 3000
	if len(text) > maxLen {
		text = text[:maxLen] + "\n…"
	}
	n.sendAsync(fmt.Sprintf("💬 *%s*\n%s", escMD(agent), escMD(text)))
}

// AskPermission sends a formatted permission prompt and waits for a y/n reply
// via the background poll loop. Returns PermTimeout when cfg.PermTimeout elapses.
func (n *Notifier) AskPermission(ctx context.Context, req oversight.PermRequest) oversight.PermDecision {
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
	lines = append(lines, "\nReply *y* to allow or *n* to deny\\.")
	n.send(strings.Join(lines, "\n"))

	ch := make(chan string, 1)
	n.mu.Lock()
	n.permCh = ch
	n.mu.Unlock()

	defer func() {
		n.mu.Lock()
		n.permCh = nil
		n.mu.Unlock()
	}()

	select {
	case reply := <-ch:
		r := strings.TrimSpace(strings.ToLower(reply))
		if r == "y" || r == "yes" {
			n.sendAsync("✅ Allowed")
			return oversight.PermAllow
		}
		n.sendAsync("🚫 Denied")
		return oversight.PermDeny
	case <-time.After(n.cfg.PermTimeout):
	case <-ctx.Done():
	}

	action := "Denied (timeout)"
	if n.cfg.TimeoutAction == "allow" {
		action = "Allowed (timeout)"
	}
	n.sendAsync(fmt.Sprintf("⏱ %s", action))
	if n.cfg.TimeoutAction == "allow" {
		return oversight.PermAllow
	}
	return oversight.PermTimeout
}

// send sends a MarkdownV2-formatted message synchronously, ignoring errors.
func (n *Notifier) send(text string) {
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
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// sendAsync fires send in a goroutine so callers are never blocked.
func (n *Notifier) sendAsync(text string) {
	go n.send(text)
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
