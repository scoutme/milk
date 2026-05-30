// Package telegram implements the oversight.Notifier interface using the
// Telegram Bot API. Messages are sent via plain HTTPS to the sendMessage
// endpoint; updates are polled via getUpdates to receive replies.
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
	pollInterval       = 2 * time.Second
)

// Config holds the Telegram-specific configuration.
type Config struct {
	Token          string
	ChatID         int64
	PermTimeout    time.Duration // how long AskPermission waits for a reply
	TimeoutAction  string        // "allow" or "deny" when PermTimeout expires
	APIBase        string        // override for testing; defaults to https://api.telegram.org
}

// Notifier sends notifications and forwards permission prompts via Telegram.
type Notifier struct {
	cfg        Config
	client     *http.Client
	mu         sync.Mutex
	lastUpdate int64 // offset for getUpdates long-polling
}

// New creates a Notifier. Callers should verify connectivity with Ping.
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
	} else {
		n.sendAsync(fmt.Sprintf("✅ *%s* done", escMD(agent)))
	}
}

// AskPermission sends a formatted permission prompt and polls for a y/n reply.
// Returns PermTimeout when cfg.PermTimeout elapses without a reply.
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

	deadline := time.Now().Add(n.cfg.PermTimeout)
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		pollCtx, cancel := context.WithTimeout(ctx, min(pollInterval+defaultPollTimeout, remaining+time.Second))
		reply, ok := n.pollForReply(pollCtx)
		cancel()
		if ok {
			r := strings.TrimSpace(strings.ToLower(reply))
			if r == "y" || r == "yes" {
				n.sendAsync("✅ Allowed")
				return oversight.PermAllow
			}
			n.sendAsync("🚫 Denied")
			return oversight.PermDeny
		}
		if ctx.Err() != nil {
			break
		}
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

// pollForReply calls getUpdates once and returns the first text message
// directed to this chat, or ("", false) when nothing arrived.
func (n *Notifier) pollForReply(ctx context.Context) (string, bool) {
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
	if err := n.apiGet(ctx, "getUpdates", params, &result); err != nil || !result.OK {
		return "", false
	}

	for _, upd := range result.Result {
		n.mu.Lock()
		if upd.UpdateID > n.lastUpdate {
			n.lastUpdate = upd.UpdateID
		}
		n.mu.Unlock()

		if upd.Message == nil || upd.Message.Chat.ID != n.cfg.ChatID {
			continue
		}
		if upd.Message.Text != "" {
			return upd.Message.Text, true
		}
	}
	return "", false
}

// send sends a Markdown-formatted message synchronously, ignoring errors.
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

