package main

// Host-independent rules for running a turn on the implicit Claude CLI. The
// escalation target only counts as "defined" when the user configured one
// (config.EscalationAgentIsImplicit); the built-in Claude default is never
// reached silently — not as a stand-in for a down primary, not through
// /escalate, the router, or the model's own escalate() — because that sends
// the conversation to a cloud model the user did not choose. Every such path
// asks once per session (default No). Shared by the TUI (runTurn), ACP and
// single-prompt mode.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/events"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/router"
)

// errFallbackDeclined marks a turn that was not run because the user declined
// (or nobody could answer) the Claude question. Hosts show it as a notice,
// not as a failure.
var errFallbackDeclined = errors.New("turn not run")

// primaryDisplayName is the configured primary agent's name for messages.
func primaryDisplayName(cfg config.Config) string { return cfg.ActiveAgent().Name }

const claudeConsentTail = "Approving allows Claude Code for the rest of this session (the prompt and conversation context go to Claude)."

// claudeConsentQuestion is the plain-text question (no styling) both hosts and
// remote oversight show. outage says the turn is only going to Claude because
// the primary is unavailable; reason is the model's own escalation reason
// ("" otherwise).
func claudeConsentQuestion(cfg config.Config, outage bool, reason string) string {
	switch {
	case reason != "":
		return fmt.Sprintf("%s asked to escalate (%s), and no escalation agent is configured. Run it on Claude Code, which is available on this host? %s",
			primaryDisplayName(cfg), reason, claudeConsentTail)
	case outage:
		return fmt.Sprintf("Primary agent %s is unavailable and no escalation agent is configured. Claude Code is available on this host — run this turn on it? %s",
			primaryDisplayName(cfg), claudeConsentTail)
	default:
		hint := ""
		if !cfg.HasInferenceAgent() {
			hint = "No agent is set up yet — run /config init for the guided setup. "
		}
		return fmt.Sprintf("%sNo escalation agent is configured. Claude Code is available on this host — run this turn on it? %s", hint, claudeConsentTail)
	}
}

// noAgentError is returned when a turn is headed to the implicit Claude
// default but Claude Code is not installed.
func noAgentError(cfg config.Config) error {
	return fmt.Errorf("no escalation agent is configured and Claude Code was not found on this host — "+
		"the turn cannot run on escalation; use /agent to configure an escalation agent (primary: %s)", primaryDisplayName(cfg))
}

// claudeDeclinedError wraps errFallbackDeclined with the user-facing reason.
func claudeDeclinedError() error {
	return fmt.Errorf("%w: Claude Code was not approved — set escalation_agent (e.g. \"claude\") to make it the configured escalation agent",
		errFallbackDeclined)
}

// confirmClaude asks the question over the shared ask layer and, on approval,
// remembers it for the session. host and n are the surfaces (either may be
// nil); with neither, the answer is no.
func confirmClaude(ctx context.Context, st *interactiveState, question string, host events.Host, n oversight.Notifier) bool {
	var direct func(context.Context) bool
	if host != nil {
		direct = func(c context.Context) bool {
			out, err := host.RequestPermission(c, events.PermissionRequest{
				Prompt:    "\n" + milkTag() + " " + question + " [y/N] ",
				DefaultNo: true,
			})
			return err == nil && out.Allow
		}
	}
	allow, _ := askPermission(ctx, n,
		oversight.PermRequest{ToolName: "claude-escalation", Input: question},
		direct, foregroundAskPolicy)
	if allow {
		st.claudeApproved = true
	}
	return allow
}

// gateClaude applies the consent rules to a routed turn. It returns nil when
// the turn may run, noAgentError when there is nowhere to run it, and
// claudeDeclinedError when the user said no.
func gateClaude(ctx context.Context, st *interactiveState, rt routedTurn, host events.Host, n oversight.Notifier) error {
	if rt.NoAgent {
		return noAgentError(st.cfg)
	}
	if !rt.NeedsClaudeConsent {
		return nil
	}
	if !confirmClaude(ctx, st, claudeConsentQuestion(st.cfg, rt.Fallback == "escalation", ""), host, n) {
		return claudeDeclinedError()
	}
	return nil
}

type claudeConsentKey struct{}

// withClaudeConsent attaches the model-escalation consent hook to a turn's
// context, where runPrimaryWithSession finds it. A context without one (tests,
// headless callers) hands off unasked.
func withClaudeConsent(ctx context.Context, f func(context.Context, string) bool) context.Context {
	return context.WithValue(ctx, claudeConsentKey{}, f)
}

// confirmSelfEscalation asks the hook on ctx, if any, whether the model's
// escalate() may hand the turn to the escalation agent.
func confirmSelfEscalation(ctx context.Context, reason string) bool {
	if f, ok := ctx.Value(claudeConsentKey{}).(func(context.Context, string) bool); ok {
		return f(ctx, reason)
	}
	return true
}

// selfEscalationConsent is the consent hook for a session:
// the model's own escalate() handoff to the implicit Claude default needs the
// same approval as any other route to it.
func selfEscalationConsent(st *interactiveState, host events.Host, n oversight.Notifier) func(context.Context, string) bool {
	return func(ctx context.Context, reason string) bool {
		if !st.cfg.EscalationAgentIsImplicit() || st.claudeApproved {
			return true
		}
		return confirmClaude(ctx, st, claudeConsentQuestion(st.cfg, false, reason), host, n)
	}
}

// singlePromptConsent is the same rule set for single-prompt mode, which has
// no TUI or ACP client: it asks on the terminal when stdin is one, and
// otherwise refuses with the explicit way to proceed.
type singlePromptConsent struct {
	cfg         config.Config
	interactive bool
	in          io.Reader
	errOut      io.Writer
	approved    bool
}

func (c *singlePromptConsent) ask(question string) error {
	if c.approved {
		return nil
	}
	if !c.interactive {
		return fmt.Errorf("no escalation agent is configured, so Claude Code would be used by default; it is not used without approval — " +
			"set escalation_agent (e.g. \"claude\") to use it non-interactively")
	}
	fmt.Fprintf(c.errOut, "%s %s [y/N] ", milkTag(), question)
	line, _ := bufio.NewReader(c.in).ReadString('\n')
	if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
		return claudeDeclinedError()
	}
	c.approved = true
	return nil
}

// gate checks the routed target: decided is the router's choice, target the
// choice after the availability fallback.
func (c *singlePromptConsent) gate(decided, target router.Target) error {
	if target != router.TargetEscalation || !c.cfg.EscalationAgentIsImplicit() {
		return nil
	}
	return c.ask(claudeConsentQuestion(c.cfg, decided == router.TargetLocal, ""))
}

// escalate is the consent hook for the model's escalate().
func (c *singlePromptConsent) escalate(_ context.Context, reason string) bool {
	if !c.cfg.EscalationAgentIsImplicit() {
		return true
	}
	return c.ask(claudeConsentQuestion(c.cfg, false, reason)) == nil
}

// primaryDownWarning is the startup/reload line for an unreachable primary.
// With an implicit Claude default nothing is routed there on its own; the
// user is asked instead.
func primaryDownWarning(cfg config.Config, escalationAvail bool) string {
	name := primaryDisplayName(cfg)
	switch {
	case !cfg.EscalationAgentIsImplicit():
		return "warning: " + name + " primary agent unreachable — routing all to escalation agent"
	case escalationAvail:
		return "warning: " + name + " primary agent unreachable; no escalation agent is configured — you will be asked before using Claude Code"
	default:
		return "warning: " + name + " primary agent unreachable; no escalation agent is configured and Claude Code was not found"
	}
}
