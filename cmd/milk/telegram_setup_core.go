package main

// Host-independent core of /setup telegram: the config mutations, status
// text and validation seams shared by the TUI's handleSetupCmd and ACP's
// acpSetup, so both hosts write the same config shape and word the same
// replies. The interactive shells around it stay per-host — tea keypresses
// (commands.go) vs ACP elicitation dialogs and choice prompts
// (acp_setup.go). The step machine itself (telegramSetupState:
// token → waitMsg) is interactive.go's, shared verbatim.

import (
	"fmt"
	"strings"

	"github.com/scoutme/milk/internal/config"
)

// Test seams: the wizard's two Telegram API calls go through these vars so
// tests can drive the flows without the network (and without a bot).
var (
	telegramGetMe         = resolveTelegramBotName
	telegramResolveChatID = resolveTelegramChatID
)

// setTelegramEnabledCfg toggles the oversight backend without touching the
// stored credentials, saving the config. Enabling without credentials is an
// error; the error text is what both hosts show to the user.
func setTelegramEnabledCfg(cfg *config.Config, on bool) error {
	ro := cfg.RemoteOversight
	if ro == nil {
		ro = &config.RemoteOversightConfig{}
		cfg.RemoteOversight = ro
	}
	if on {
		if ro.Telegram == nil || ro.Telegram.Token == "" || ro.Telegram.ChatID == 0 {
			return fmt.Errorf("no Telegram credentials configured — run /setup telegram first")
		}
		ro.Backend = "telegram"
	} else {
		ro.Backend = ""
	}
	if err := saveLocalOrGlobal(*cfg); err != nil {
		return fmt.Errorf("error saving config: %w", err)
	}
	return nil
}

// commitTelegramCfg stores freshly validated credentials and enables the
// backend — the setup wizard's final step. Error text matches the TUI's
// historic wording.
func commitTelegramCfg(cfg *config.Config, token string, chatID int64) error {
	ro := cfg.RemoteOversight
	if ro == nil {
		ro = &config.RemoteOversightConfig{}
	}
	ro.Backend = "telegram"
	if ro.Telegram == nil {
		ro.Telegram = &config.TelegramConfig{}
	}
	ro.Telegram.Token = token
	ro.Telegram.ChatID = chatID
	cfg.RemoteOversight = ro
	if err := saveLocalOrGlobal(*cfg); err != nil {
		return fmt.Errorf("error saving config: %w", err)
	}
	return nil
}

// telegramMaskToken hides all but the last 6 characters of a bot token for
// display (tokens are secrets — nothing ever echoes them in full).
func telegramMaskToken(token string) string {
	if len(token) <= 6 {
		return token
	}
	return strings.Repeat("*", len(token)-6) + token[len(token)-6:]
}

// telegramStatusText renders /setup telegram status for either host.
func telegramStatusText(cfg config.Config) string {
	ro := cfg.RemoteOversight
	var b strings.Builder
	enabled := ro != nil && ro.Backend == "telegram"
	state := "off"
	if enabled {
		state = "on"
	}
	fmt.Fprintf(&b, "%s Telegram oversight: %s\n", milkTag(), state)
	if ro == nil || ro.Telegram == nil || ro.Telegram.Token == "" || ro.Telegram.ChatID == 0 {
		b.WriteString("  credentials: not configured — run /setup telegram\n")
		return strings.TrimRight(b.String(), "\n")
	}
	fmt.Fprintf(&b, "  credentials: configured (chat_id: %d, token: %s)\n",
		ro.Telegram.ChatID, telegramMaskToken(ro.Telegram.Token))
	if !enabled {
		b.WriteString("  enable with: /setup telegram on\n")
	}
	tools := "on"
	if !ro.NotifyToolsEnabled() {
		tools = "off"
	}
	fmt.Fprintf(&b, "  tool notifications: %s\n", tools)
	fmt.Fprintf(&b, "  permission wait: %ds → %s\n",
		int(ro.PermTimeoutDuration().Seconds()), ro.TimeoutActionValue())
	return strings.TrimRight(b.String(), "\n")
}

// telegramDoneWord reports whether a typed answer means "I've sent the
// message to the bot" — the waitMsg step's typed-input confirmation (the
// form dialog and the clickable choice prompt are the nicer surfaces; this
// is the universal floor).
func telegramDoneWord(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "done", "ok", "ready", "yes", "sent", "continue":
		return true
	}
	return false
}
