package main

// Post-setup smoke check for the /config init wizard: once the config is
// written, prove the chosen primary actually answers — reachability first,
// then one tiny completion — and turn the outcome into the closing message
// every host shows ("you're ready" + first commands, or an actionable error).
// Shared by the TUI (async, initSmokeCmd) and ACP (inline in commitInitWizard)
// so both say the same thing.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
)

// initSmokeTimeout bounds the whole check: a cold local model can take a
// while to answer its first request, but the wizard must not hang on it.
const initSmokeTimeout = 45 * time.Second

// initSmokeCheckFn is the smoke check the hosts run; a var so host tests with
// a stub backend don't depend on a real completion round trip.
var initSmokeCheckFn = initWizardSmokeCheck

// claudeLoginDoneMsg reports that the Claude Code browser login the wizard
// handed the terminal to has returned.
type claudeLoginDoneMsg struct{ err error }

// initSmokeMsg carries the closing text of the smoke check back to the TUI.
type initSmokeMsg struct{ text string }

// initSmokeCmd runs the smoke check off the UI goroutine.
func initSmokeCmd(cfg config.Config) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), initSmokeTimeout)
		defer cancel()
		return initSmokeMsg{text: initSmokeCheckFn(ctx, cfg)}
	}
}

// initWizardSmokeCheck verifies the configured primary and returns the closing
// message. Failure never undoes the saved config — it says what to fix.
func initWizardSmokeCheck(ctx context.Context, cfg config.Config) string {
	ac := cfg.ActiveAgent()
	if err := smokePrimary(ctx, ac); err != nil {
		return fmt.Sprintf("\n%s couldn't get an answer from %s: %v\n%s\n",
			milkTag(), bold(ac.Name), err,
			dim("  Your settings are saved. Fix the cause and re-run /config init, or edit them with /config open."))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n%s ✓ %s answered — you're ready.\n", milkTag(), bold(ac.Name))
	sb.WriteString(dim("  try: /help (all commands) · /sessions (past conversations) · /permissions (what the agent may do)") + "\n")
	if cfg.EscalationAgent != "" {
		sb.WriteString(dim("  hard task? /escalate hands the next prompt to "+cfg.EscalationAgent) + "\n")
	}
	return sb.String()
}

// smokePrimary pings the primary and asks it for one word. Providers without
// a cheap completion path (subprocess agents, Bedrock's native API) are
// accepted on reachability alone.
func smokePrimary(ctx context.Context, ac config.AgentConfig) error {
	switch {
	case ac.IsCLI():
		if err := newCLIAgent(ac).Ping(); err != nil {
			return fmt.Errorf("the claude CLI isn't usable (%v) — install it and sign in with `claude`", err)
		}
		return nil
	case ac.IsExternalProcess():
		return nil
	}
	agent := local.NewFromConfig(ac)
	if err := agent.Ping(ctx); err != nil {
		return fmt.Errorf("%v — is the server running and the URL right?", err)
	}
	_, _, err := agent.Summarize(ctx, "Reply with the single word: ok")
	switch {
	case err == nil, errors.Is(err, local.ErrCompactionUnsupported):
		return nil
	case strings.Contains(err.Error(), "no choices"), strings.Contains(err.Error(), "decode"), strings.Contains(err.Error(), "cannot unmarshal"):
		return fmt.Errorf("the server answered but rejected the request — check the model name and API key")
	}
	return err
}
