package main

// Tests for the ctx:x/y numerator/window pairing invariant (issue #429):
// the numerator sources (obs's role-keyed last-request slot and the lastTurn*
// token-delta fallback) must only ever be displayed against the context
// window of the agent identity they were measured under. The identity is
// stamped at dispatch (stampTurnCtxIdentity) and compared against the active
// identity at render (statusTokens) — so a /agent switch, --agent, or an
// agent/window config edit suppresses the fragment until the active agent
// serves a request of its own, instead of dividing the previous agent's
// request by the new agent's window.

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/testutil"
)

// stampCtxFixture opens the issue #429 provenance gate for a hand-built
// fixture: it marks the fixture's numerator data as measured under the
// currently active identity — exactly what dispatchAgent does at turn start.
// Tests exercising the gate itself stamp turnCtxIdentity explicitly instead.
func stampCtxFixture(m *model) {
	m.turnCtxIdentity = map[string]ctxIdentity{"primary": ctxIdentityFor(m.st.cfg, "primary")}
}

// issue429Cfg is the /agent-switch repro from issue #429: a large-window
// agent (mimo, 1M) that has served a request, and a small-window one
// (qwen3.5-4B-super-coder, 32k) that becomes active without serving yet.
func issue429Cfg() config.Config {
	return config.Config{
		Agents: []config.AgentConfig{
			{Name: "mimo", Model: "mimo-9b", ContextWindowTokens: 1_000_000},
			{Name: "qwen3.5-4B-super-coder", Model: "qwen3.5-4B", ContextWindowTokens: 32_000},
		},
		Agent: "qwen3.5-4B-super-coder",
	}
}

// TestStatusTokens_ContextPressure_SuppressedAfterAgentSwitch is the issue
// #429 repro: after /agent switch the previous agent's last request must not
// be divided by the new agent's window. Both numerator sources are keyed by
// role alone and survive the switch — the fragment must be suppressed
// entirely (not shown as "148.7k/32.0k") until the new agent serves.
func TestStatusTokens_ContextPressure_SuppressedAfterAgentSwitch(t *testing.T) {
	obs.ResetSessionTokens()
	t.Cleanup(obs.ResetSessionTokens)
	obs.RecordRequestInput("primary", 148_700) // mimo's request — role-keyed, survives the switch

	m := &model{
		width: 120,
		st: &interactiveState{
			sess: &session.Session{ID: "test1234"},
			cfg:  issue429Cfg(),
		},
		// Stale provenance: the last dispatch ran mimo (1M window), the
		// active agent is now qwen (32k) and has not served a request yet.
		turnCtxIdentity: map[string]ctxIdentity{
			"primary": {agent: "mimo", model: "mimo-9b", window: 1_000_000},
		},
		primaryPrompt:      148_700,
		primaryCompletion:  200,
		lastTurnPrompt:     map[string]int64{"primary": 148_700},
		lastTurnCompletion: map[string]int64{"primary": 200},
		lastTokenRole:      "primary",
	}

	bar := ansi.Strip(m.statusTokens())
	if strings.Contains(bar, "ctx:") {
		t.Errorf("want no ctx: fragment while the numerator's identity is stale, got %q", bar)
	}
	if strings.Contains(bar, "32.0k") {
		t.Errorf("the new agent's window must not be paired with the old numerator: %q", bar)
	}
}

// TestStatusTokens_ContextPressure_ShowsWhenIdentityMatches is the flip
// side: once the active agent's own identity is stamped (its first turn has
// been dispatched), the pair renders — the request's size against that same
// agent's window.
func TestStatusTokens_ContextPressure_ShowsWhenIdentityMatches(t *testing.T) {
	obs.ResetSessionTokens()
	t.Cleanup(obs.ResetSessionTokens)
	obs.RecordRequestInput("primary", 10_979) // qwen's own request against its 32k window

	cfg := issue429Cfg()
	m := &model{
		width: 120,
		st: &interactiveState{
			sess: &session.Session{ID: "test1234"},
			cfg:  cfg,
		},
		turnCtxIdentity: map[string]ctxIdentity{
			"primary": ctxIdentityFor(cfg, "primary"),
		},
		lastTurnPrompt:     map[string]int64{"primary": 10_979},
		lastTurnCompletion: map[string]int64{"primary": 200},
		lastTokenRole:      "primary",
	}

	bar := ansi.Strip(m.statusTokens())
	if !strings.Contains(bar, "ctx:11.0k/32.0k") {
		t.Errorf("want ctx:11.0k/32.0k paired under the active agent's own identity, got %q", bar)
	}
}

// TestStatusTokens_ContextPressure_SuppressedAfterWindowEdit: the same agent
// with an edited context_window_tokens must also re-measure first — a request
// trimmed to the old window shown against the new one is the same class of
// mispairing issue #429 reports (identity covers the window, not just the
// agent name).
func TestStatusTokens_ContextPressure_SuppressedAfterWindowEdit(t *testing.T) {
	obs.ResetSessionTokens()
	t.Cleanup(obs.ResetSessionTokens)
	obs.RecordRequestInput("primary", 148_700) // measured before the window shrank

	m := &model{
		width: 120,
		st: &interactiveState{
			sess: &session.Session{ID: "test1234"},
			cfg: config.Config{
				Agents: []config.AgentConfig{{Name: "mimo", Model: "mimo-9b", ContextWindowTokens: 32_000}},
				Agent:  "mimo",
			},
		},
		turnCtxIdentity: map[string]ctxIdentity{
			"primary": {agent: "mimo", model: "mimo-9b", window: 1_000_000}, // pre-edit window
		},
		lastTurnPrompt:     map[string]int64{"primary": 148_700},
		lastTurnCompletion: map[string]int64{"primary": 200},
		lastTokenRole:      "primary",
	}

	bar := ansi.Strip(m.statusTokens())
	if strings.Contains(bar, "ctx:") {
		t.Errorf("want no ctx: fragment after a window edit until the agent re-measures, got %q", bar)
	}
}

// TestDispatchAgent_StampsCtxIdentityAndClearsStaleRequestInput pins the
// dispatch-side half of the invariant: a dispatch under a changed identity
// drops the role-keyed last-request slot (so a first turn that fails before
// reporting usage can't resurface the previous agent's request) and stamps
// the new identity; a dispatch under an unchanged identity keeps the slot.
func TestDispatchAgent_StampsCtxIdentityAndClearsStaleRequestInput(t *testing.T) {
	obs.ResetSessionTokens()
	t.Cleanup(obs.ResetSessionTokens)
	testutil.SandboxHome(t)
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	cfgMimo := config.Config{
		Agents: []config.AgentConfig{{Name: "mimo", Model: "mimo-9b", ContextWindowTokens: 1_000_000}},
		Agent:  "mimo",
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}, cfg: cfgMimo}
	m := newModel(context.Background(), st, nil, dispatchAgents{}, nil)

	// First dispatch of a fresh model counts as an identity change (unknown →
	// mimo), so any pre-existing slot must be dropped.
	obs.RecordRequestInput("primary", 148_700)
	first, _ := m.dispatchAgent("hi")
	m = first.(model)
	if got := obs.SessionLastRequestInput("primary"); got != 0 {
		t.Errorf("stale last-request slot survived the dispatch: got %d, want 0", got)
	}
	if got := m.turnCtxIdentity["primary"]; got != ctxIdentityFor(cfgMimo, "primary") {
		t.Errorf("primary identity stamp = %+v, want %+v", got, ctxIdentityFor(cfgMimo, "primary"))
	}
	if got := m.turnCtxIdentity["escalation"]; got != ctxIdentityFor(cfgMimo, "escalation") {
		t.Errorf("escalation identity stamp = %+v, want %+v", got, ctxIdentityFor(cfgMimo, "escalation"))
	}

	// Same identity again: this dispatch must keep the recorded slot.
	obs.RecordRequestInput("primary", 11_000)
	second, _ := m.dispatchAgent("hi")
	m = second.(model)
	if got := obs.SessionLastRequestInput("primary"); got != 11_000 {
		t.Errorf("unchanged identity cleared the slot: got %d, want 11000", got)
	}

	// /agent switch mid-session: the next dispatch drops the slot again.
	cfgQwen := config.Config{
		Agents: []config.AgentConfig{{Name: "qwen3.5-4B-super-coder", Model: "qwen3.5-4B", ContextWindowTokens: 32_000}},
		Agent:  "qwen3.5-4B-super-coder",
	}
	st.cfg = cfgQwen
	obs.RecordRequestInput("primary", 11_000)
	third, _ := m.dispatchAgent("hi")
	m = third.(model)
	if got := obs.SessionLastRequestInput("primary"); got != 0 {
		t.Errorf("switch left the previous agent's request in place: got %d, want 0", got)
	}
	if got := m.turnCtxIdentity["primary"]; got != ctxIdentityFor(cfgQwen, "primary") {
		t.Errorf("stamp not updated after switch: %+v, want %+v", got, ctxIdentityFor(cfgQwen, "primary"))
	}
}
