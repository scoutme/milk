package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/events"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/session"
)

// askHost answers permission requests with a fixed outcome and records them.
type askHost struct {
	events.Host
	allow bool
	reqs  []events.PermissionRequest
}

func (h *askHost) RequestPermission(_ context.Context, r events.PermissionRequest) (events.PermissionOutcome, error) {
	h.reqs = append(h.reqs, r)
	return events.PermissionOutcome{Allow: h.allow}, nil
}

// primaryDownRoute routes a turn with the primary unavailable and Claude
// installed, the case that used to fall back silently.
func primaryDownRoute(t *testing.T, st *interactiveState) routedTurn {
	t.Helper()
	st.forcePrimary = true // the router must pick the primary for this to be a fallback
	rt, err := routeTurn(context.Background(), st, router.New(st.cfg, nil), "x", false, true)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestRouteTurn_ImplicitClaudeNeedsConsent(t *testing.T) {
	st := routingState(t)
	rt := primaryDownRoute(t, st)
	if rt.Fallback != "escalation" || !rt.NeedsClaudeConsent {
		t.Errorf("implicit Claude fallback must need consent: %+v", rt)
	}

	// An explicit /escalate does not make the implicit default acceptable.
	st.forceEscalate = true
	rt, _ = routeTurn(context.Background(), st, router.New(st.cfg, nil), "x", true, true)
	if rt.Target != router.TargetEscalation || !rt.NeedsClaudeConsent {
		t.Errorf("/escalate must ask while no escalation agent is configured: %+v", rt)
	}

	// Pinned and auto-sticky escalation reach it the same way.
	st.stickyEscalate = true
	if rt, _ = routeTurn(context.Background(), st, router.New(st.cfg, nil), "x", true, true); !rt.NeedsClaudeConsent {
		t.Errorf("a pin must ask too: %+v", rt)
	}
	st.stickyEscalate = false

	// A primary-bound turn on a healthy primary needs nothing.
	st.forcePrimary = true
	if rt, _ = routeTurn(context.Background(), st, router.New(st.cfg, nil), "x", true, true); rt.NeedsClaudeConsent {
		t.Errorf("a primary turn must not ask: %+v", rt)
	}

	for name, cfg := range map[string]config.Config{
		"escalation_agent claude": {EscalationAgent: "claude"},
		"named inference agent":   {Agents: []config.AgentConfig{{Name: "big", URL: "http://x", Model: "m"}}, EscalationAgent: "big"},
	} {
		st.cfg = cfg
		st.forceEscalate = true
		if rt, _ = routeTurn(context.Background(), st, router.New(st.cfg, nil), "x", true, true); rt.NeedsClaudeConsent {
			t.Errorf("%s is a configured escalation agent: %+v", name, rt)
		}
	}
}

// A failed health ping on every agent (a proxy hiccup hits every agent on the
// same host) must not refuse the turn or redirect it: the router's own choice
// runs, and the real request decides.
func TestRouteTurn_BothReadingDownStillAttemptsTheRoutedTarget(t *testing.T) {
	for _, cfg := range []config.Config{
		{},
		{Agents: []config.AgentConfig{{Name: "p", URL: "http://x", Model: "m"}, {Name: "big", URL: "http://x", Model: "m"}}, Agent: "p", EscalationAgent: "big"},
	} {
		st := routingState(t)
		st.cfg = cfg
		st.forcePrimary = true
		rt, _ := routeTurn(context.Background(), st, router.New(st.cfg, nil), "x", false, false)
		if rt.Target != router.TargetLocal || rt.Fallback != "" || rt.NoAgent || rt.NeedsClaudeConsent {
			t.Errorf("both unavailable, primary-bound turn must still try the primary: %+v", rt)
		}
		if err := gateClaude(context.Background(), st, rt, nil, nil); err != nil {
			t.Errorf("the turn must not be refused up front: %v", err)
		}

		st.forceEscalate = true
		rt, _ = routeTurn(context.Background(), st, router.New(st.cfg, nil), "x", false, false)
		if rt.Target != router.TargetEscalation || rt.NoAgent && !cfg.EscalationAgentIsImplicit() {
			t.Errorf("an escalation-bound turn keeps its target: %+v", rt)
		}
	}
}

// The one up-front refusal left is deterministic: the target is the implicit
// Claude default and Claude Code is not installed.
func TestGateClaude_ImplicitClaudeNotInstalled(t *testing.T) {
	st := routingState(t)
	st.forceEscalate = true
	rt, _ := routeTurn(context.Background(), st, router.New(st.cfg, nil), "x", false, false)
	if !rt.NoAgent {
		t.Fatalf("escalating to a missing implicit Claude must be NoAgent: %+v", rt)
	}
	err := gateClaude(context.Background(), st, rt, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "Claude Code was not found") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestGateClaude_AsksOncePerSession(t *testing.T) {
	st := routingState(t)
	host := &askHost{allow: true}

	rt := primaryDownRoute(t, st)
	if err := gateClaude(context.Background(), st, rt, host, oversight.Noop{}); err != nil {
		t.Fatalf("approved fallback must run: %v", err)
	}
	if len(host.reqs) != 1 || !host.reqs[0].DefaultNo || !strings.Contains(host.reqs[0].Prompt, "no escalation agent is configured") ||
		!strings.Contains(host.reqs[0].Prompt, "rest of this session") {
		t.Errorf("unexpected question: %+v", host.reqs)
	}

	// Later turns of the session — fallback, /escalate, the router — don't ask again.
	if rt = primaryDownRoute(t, st); rt.NeedsClaudeConsent {
		t.Error("an approved session must not ask again on a fallback")
	}
	st.forceEscalate = true
	if rt, _ = routeTurn(context.Background(), st, router.New(st.cfg, nil), "x", true, true); rt.NeedsClaudeConsent {
		t.Error("an approved session must not ask again on /escalate")
	}
}

func TestGateClaude_ExplicitEscalateQuestionIsNotAboutAnOutage(t *testing.T) {
	st := routingState(t)
	st.forceEscalate = true
	rt, _ := routeTurn(context.Background(), st, router.New(st.cfg, nil), "x", true, true)
	host := &askHost{allow: false}
	_ = gateClaude(context.Background(), st, rt, host, oversight.Noop{})
	if len(host.reqs) != 1 || strings.Contains(host.reqs[0].Prompt, "unavailable") {
		t.Errorf("an /escalate with a healthy primary must not claim an outage: %+v", host.reqs)
	}
}

func TestGateClaude_DeclineDoesNotRunOrRemember(t *testing.T) {
	st := routingState(t)
	host := &askHost{allow: false}
	err := gateClaude(context.Background(), st, primaryDownRoute(t, st), host, oversight.Noop{})
	if !errors.Is(err, errFallbackDeclined) {
		t.Fatalf("want errFallbackDeclined, got %v", err)
	}
	if st.claudeApproved {
		t.Error("a decline must not be remembered as approval")
	}
}

func TestGateClaude_NoSurfaceMeansNo(t *testing.T) {
	st := routingState(t)
	if err := gateClaude(context.Background(), st, primaryDownRoute(t, st), nil, nil); !errors.Is(err, errFallbackDeclined) {
		t.Errorf("with nobody to ask the answer is no, got %v", err)
	}
}

func TestSelfEscalationConsent(t *testing.T) {
	st := routingState(t)
	host := &askHost{allow: true}
	consent := selfEscalationConsent(st, host, oversight.Noop{})

	if !consent(context.Background(), "needs deeper analysis") {
		t.Fatal("approved self-escalation must go ahead")
	}
	if len(host.reqs) != 1 || !strings.Contains(host.reqs[0].Prompt, "needs deeper analysis") || !host.reqs[0].DefaultNo {
		t.Errorf("the question must carry the model's reason and default to No: %+v", host.reqs)
	}
	consent(context.Background(), "again")
	if len(host.reqs) != 1 {
		t.Error("approved once means approved for the session")
	}

	st2 := routingState(t)
	if selfEscalationConsent(st2, &askHost{allow: false}, oversight.Noop{})(context.Background(), "r") {
		t.Error("a declined self-escalation must not go ahead")
	}
	st2.cfg = config.Config{EscalationAgent: "claude"}
	if !selfEscalationConsent(st2, &askHost{allow: false}, oversight.Noop{})(context.Background(), "r") {
		t.Error("a configured escalation agent needs no consent")
	}
}

func TestConfirmSelfEscalation_NoHookMeansAllowed(t *testing.T) {
	if !confirmSelfEscalation(context.Background(), "r") {
		t.Error("headless contexts keep the old behavior")
	}
	denied := withClaudeConsent(context.Background(), func(context.Context, string) bool { return false })
	if confirmSelfEscalation(denied, "r") {
		t.Error("the hook's refusal must win")
	}
}

func TestSinglePromptConsent(t *testing.T) {
	cfg := config.Config{}
	run := func(cfg config.Config, interactive bool, in string, decided, target router.Target) (error, string) {
		var errOut bytes.Buffer
		c := &singlePromptConsent{cfg: cfg, interactive: interactive, in: strings.NewReader(in), errOut: &errOut}
		return c.gate(decided, target), errOut.String()
	}

	if err, _ := run(cfg, true, "", router.TargetLocal, router.TargetLocal); err != nil {
		t.Errorf("a primary turn needs nothing: %v", err)
	}
	if err, _ := run(config.Config{EscalationAgent: "claude"}, false, "", router.TargetLocal, router.TargetEscalation); err != nil {
		t.Errorf("a configured escalation agent needs no consent: %v", err)
	}

	// --escalate (decided == target == escalation) asks too.
	err, _ := run(cfg, false, "", router.TargetEscalation, router.TargetEscalation)
	if err == nil || !strings.Contains(err.Error(), "escalation_agent") {
		t.Errorf("non-interactive must refuse and say how to proceed: %v", err)
	}

	err, out := run(cfg, true, "y\n", router.TargetLocal, router.TargetEscalation)
	if err != nil || !strings.Contains(out, "[y/N]") || !strings.Contains(out, "unavailable") {
		t.Errorf("approved outage: err=%v out=%q", err, out)
	}
	for _, in := range []string{"\n", "n\n", ""} {
		if err, _ := run(cfg, true, in, router.TargetEscalation, router.TargetEscalation); !errors.Is(err, errFallbackDeclined) {
			t.Errorf("answer %q must decline, got %v", in, err)
		}
	}

	// Approval carries over to the model's own escalate().
	var errOut bytes.Buffer
	c := &singlePromptConsent{cfg: cfg, interactive: true, in: strings.NewReader("y\n"), errOut: &errOut}
	if !c.escalate(context.Background(), "needs more") || !c.escalate(context.Background(), "again") {
		t.Error("approved escalate() should go ahead, and only ask once")
	}
}

// An approved fallback covers the outage, not the session: it must not make
// the escalation agent sticky, or Claude would keep getting turns after the
// primary is back.
func TestNoteTurnSucceeded_ImplicitFallbackIsNotSticky(t *testing.T) {
	st := routingState(t)
	rt := primaryDownRoute(t, st)
	noteTurnSucceeded(st, rt)
	if st.autoStickyEscalate {
		t.Error("implicit Claude fallback became auto-sticky")
	}

	st.cfg = config.Config{EscalationAgent: "claude"}
	rt = primaryDownRoute(t, st)
	noteTurnSucceeded(st, rt)
	if !st.autoStickyEscalate {
		t.Error("a configured escalation agent keeps the existing sticky behavior")
	}
}

// escalateRunner asks to escalate; execRunner records whether it was run.
type countRunner struct {
	*flakyExecRunner
	res   TurnResult
	calls int
}

func (r *countRunner) Execute(_ context.Context, _ config.Config, _ *session.Session, _ *memory.Store, _ AgentRole,
	_ escalation.ContextMode, _, _ string, _ []string, _ bool, _ string, _ TurnCallbacks, _ io.Writer) (TurnResult, error) {
	r.calls++
	return r.res, nil
}

// A declined escalate() must not run the escalation agent, and the primary's
// request must not be left looking unanswered in history.
func TestRunPrimary_DeclinedSelfEscalationDoesNotRunEscalation(t *testing.T) {
	cfg := config.Config{Agents: []config.AgentConfig{{Name: "p", Provider: "local"}}}
	run := func(consent func(context.Context, string) bool) (*session.Session, *countRunner, *countRunner) {
		sess := &session.Session{ID: "s"}
		primary := &countRunner{flakyExecRunner: &flakyExecRunner{name: "p"}, res: TurnResult{EscalationReason: "too hard"}}
		esc := &countRunner{flakyExecRunner: &flakyExecRunner{name: "claude"}, res: TurnResult{Text: "claude says hi"}}
		ctx := withClaudeConsent(context.Background(), consent)
		var out bytes.Buffer
		if err := runPrimaryWithSession(ctx, cfg, sess, primary, esc, nil, "do it", "do it", &out, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		return sess, primary, esc
	}

	sess, _, esc := run(func(context.Context, string) bool { return false })
	if esc.calls != 0 {
		t.Error("the escalation agent ran although Claude was declined")
	}
	last := sess.History[len(sess.History)-1]
	if last.Role != session.RoleAssistant || !strings.Contains(last.Content, "did not approve Claude Code") {
		t.Errorf("history must record the declined escalation, got %+v", last)
	}

	if _, _, esc = run(func(context.Context, string) bool { return true }); esc.calls != 1 {
		t.Errorf("an approved escalation must run, ran %d times", esc.calls)
	}
}

// TestClaudeConsentQuestion_PointsFirstRunAtSetup: with no agent configured the
// consent question names the guided setup; with one it stays unchanged.
func TestClaudeConsentQuestion_PointsFirstRunAtSetup(t *testing.T) {
	if q := claudeConsentQuestion(config.Config{}, false, ""); !strings.Contains(q, "/config init") {
		t.Errorf("empty config: %q, want the /config init pointer", q)
	}
	cfg := config.Config{Agent: "a", Agents: []config.AgentConfig{{Name: "a", URL: "http://localhost:1", Model: "m", Provider: "local"}}}
	if q := claudeConsentQuestion(cfg, false, ""); strings.Contains(q, "/config init") {
		t.Errorf("configured primary: %q, want no setup pointer", q)
	}
}
