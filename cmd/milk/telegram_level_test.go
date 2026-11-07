package main

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

// telegramBool is a *bool literal helper for NotifyTools fixtures.
func telegramBool(v bool) *bool { return &v }

// telegramLevelCfg returns an enabled config with fake credentials and the
// given starting notify_tools value (nil = unset).
func telegramLevelCfg(tools *bool) config.Config {
	return config.Config{
		RemoteOversight: &config.RemoteOversightConfig{
			Backend:     "telegram",
			Telegram:    &config.TelegramConfig{Token: "123:abc", ChatID: 42},
			NotifyTools: tools,
		},
	}
}

// TestSetTelegramLevelCfg_MapsLevels: the enable level is a preset over the
// config's own knobs — on/lite write notify_tools explicitly (a stale value
// can never contradict the level), off clears the backend and leaves the
// knobs for the next enable.
func TestSetTelegramLevelCfg_MapsLevels(t *testing.T) {
	dir := t.TempDir()
	cfg := telegramLevelCfg(nil)
	if err := setTelegramLevelCfg(&cfg, "on", dir); err != nil {
		t.Fatal(err)
	}
	if cfg.RemoteOversight.Backend != "telegram" || !cfg.RemoteOversight.NotifyToolsEnabled() {
		t.Errorf("on: backend=%q tools=%v, want telegram/full",
			cfg.RemoteOversight.Backend, cfg.RemoteOversight.NotifyToolsEnabled())
	}
	// A stale notify_tools=false must not survive `on`.
	cfg = telegramLevelCfg(telegramBool(false))
	if err := setTelegramLevelCfg(&cfg, "on", dir); err != nil {
		t.Fatal(err)
	}
	if !cfg.RemoteOversight.NotifyToolsEnabled() {
		t.Error("on must force notify_tools=true over a stale false")
	}
	if err := setTelegramLevelCfg(&cfg, "lite", dir); err != nil {
		t.Fatal(err)
	}
	if cfg.RemoteOversight.Backend != "telegram" || cfg.RemoteOversight.NotifyToolsEnabled() {
		t.Errorf("lite: backend=%q tools=%v, want telegram/quiet",
			cfg.RemoteOversight.Backend, cfg.RemoteOversight.NotifyToolsEnabled())
	}
	// off clears the backend only — the knob survives for the next enable.
	if err := setTelegramLevelCfg(&cfg, "off", dir); err != nil {
		t.Fatal(err)
	}
	if cfg.RemoteOversight.Backend != "" {
		t.Errorf("off: backend=%q, want empty", cfg.RemoteOversight.Backend)
	}
	// Unknown levels are an error, not a silent off.
	cfg = telegramLevelCfg(nil)
	if err := setTelegramLevelCfg(&cfg, "maybe", dir); err == nil {
		t.Error("unknown level must error")
	}
}

// TestSetTelegramLevelCfg_RequiresCreds: both enable levels need
// credentials; off works credential-free.
func TestSetTelegramLevelCfg_RequiresCreds(t *testing.T) {
	dir := t.TempDir()
	for _, level := range []string{"on", "lite"} {
		cfg := config.Config{RemoteOversight: &config.RemoteOversightConfig{}}
		err := setTelegramLevelCfg(&cfg, level, dir)
		if err == nil || !strings.Contains(err.Error(), "no Telegram credentials configured") {
			t.Errorf("%s without creds: err = %v, want the credentials error", level, err)
		}
	}
	cfg := config.Config{RemoteOversight: &config.RemoteOversightConfig{Backend: "telegram"}}
	if err := setTelegramLevelCfg(&cfg, "off", dir); err != nil {
		t.Errorf("off without creds: %v", err)
	}
}

// TestTelegramStatusText_LevelDerivation: the status line shows the level —
// on / lite / off — derived from backend + notify_tools, with the knob's own
// detail line kept underneath.
func TestTelegramStatusText_LevelDerivation(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Config
		want string
		also string
	}{
		{"off", config.Config{}, "Telegram oversight: off", ""},
		{"on", telegramLevelCfg(nil), "Telegram oversight: on", "tool notifications: on"},
		{"lite", telegramLevelCfg(telegramBool(false)), "Telegram oversight: lite", "tool notifications: off"},
	}
	for _, c := range cases {
		got := telegramStatusText(c.cfg)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: status = %q, want %q", c.name, got, c.want)
		}
		if c.also != "" && !strings.Contains(got, c.also) {
			t.Errorf("%s: status = %q, want %q", c.name, got, c.also)
		}
	}
	// The enable hint advertises both levels (with creds configured —
	// without them the status returns early on the credentials line).
	offWithCreds := telegramLevelCfg(nil)
	offWithCreds.RemoteOversight.Backend = ""
	if got := telegramStatusText(offWithCreds); !strings.Contains(got, "/setup telegram on|lite") {
		t.Errorf("enable hint = %q, want on|lite advertised", got)
	}
}

// TestHandleSetupCmd_TelegramLite drives the TUI surface end to end: the
// lite level enables the backend, silences tool notifications, and says so.
func TestHandleSetupCmd_TelegramLite(t *testing.T) {
	m := newTestModelForPanels(t)
	m.st.cfg.RemoteOversight = &config.RemoteOversightConfig{
		Telegram: &config.TelegramConfig{Token: "123:abc", ChatID: 42},
	}
	next, _ := m.handleSetupCmd("telegram lite")
	nm, ok := next.(model)
	if !ok {
		t.Fatalf("handleSetupCmd returned %T", next)
	}
	ro := nm.st.cfg.RemoteOversight
	if ro.Backend != "telegram" || ro.NotifyToolsEnabled() {
		t.Errorf("backend=%q tools=%v, want telegram/quiet", ro.Backend, ro.NotifyToolsEnabled())
	}
	if got := nm.transcript.String(); !strings.Contains(got, "lite — tool notifications off") {
		t.Errorf("transcript = %q, want the lite confirmation", got)
	}
}

// TestACPSetup_LiteRouting: /setup telegram lite routes to the level core
// over ACP too — without credentials it fails with the same error as on.
func TestACPSetup_LiteRouting(t *testing.T) {
	server, conn := acpTestServer(t, "model reply")
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/setup telegram lite")

	chunks := acpChunks(conn)
	if len(chunks) != 1 || !strings.Contains(chunks[0], "no Telegram credentials configured") {
		t.Errorf("chunks = %q, want the credentials error", chunks)
	}
}
