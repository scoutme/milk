package main

// Wiring tests for issue #471's cache-warm shape fix: after a settled turn is
// persisted into sess.History, dispatch hands the reshape seam the next-turn
// request shape rebuilt with the same builder the runner uses at the top of
// Execute (see buildTurnHistory / reshapeCacheWarm).

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/session"
)

// stubTurnRunner is a minimal inference-free TurnRunner: one settled text
// reply per turn, no session side effects of its own.
type stubTurnRunner struct{ name string }

func (s *stubTurnRunner) Name() string { return s.name }
func (s *stubTurnRunner) IsCLI() bool  { return false }
func (s *stubTurnRunner) Ping() error  { return nil }
func (s *stubTurnRunner) Execute(
	ctx context.Context,
	cfg config.Config,
	sess *session.Session,
	mem *memory.Store,
	role AgentRole,
	ctxMode escalation.ContextMode,
	sessionID, nonce string,
	percepts []string,
	injectInstructions bool,
	prompt string,
	cbs TurnCallbacks,
	out io.Writer,
) (TurnResult, error) {
	return TurnResult{Text: "stub reply"}, nil
}

func (s *stubTurnRunner) RunToolCall(ctx context.Context, cfg config.Config, prompt string, images []local.ContentPart, out io.Writer) (string, error) {
	return "", nil
}

// reshapeCaptureRunner adds the cacheWarmReshaper seam to the stub, capturing
// what the dispatch layer hands it after a settled turn.
type reshapeCaptureRunner struct {
	stubTurnRunner
	calls int
	got   []local.Message
}

func (s *reshapeCaptureRunner) ReshapeCacheWarm(_ context.Context, _ *session.Session, history []local.Message) {
	s.calls++
	s.got = history
}

var (
	_ TurnRunner        = (*stubTurnRunner)(nil)
	_ cacheWarmReshaper = (*reshapeCaptureRunner)(nil)
)

// TestRunPrimaryReshapeCacheWarm pins the persist-side wiring: after the turn
// is recorded into sess.History, runPrimary hands the reshape seam the
// next-turn request shape — the runner's builder over the persisted session,
// the just-settled user and assistant turns included — before returning to the
// idle window (and so before the replay's idle timer can fire).
func TestRunPrimaryReshapeCacheWarm(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir()) // keep session.Save out of the real home

	cfg := config.Config{
		Agent:           "primary",
		EscalationAgent: "esc",
		Agents: []config.AgentConfig{
			{Name: "primary", URL: "http://unused", Model: "primary-model"},
			{Name: "esc", URL: "http://unused", Model: "esc-model"},
		},
	}
	sess := &session.Session{}
	sess.AddTurn(session.Turn{Role: session.RoleUser, Agent: session.AgentLocal, Content: "earlier question"})
	sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentLocal, Content: "earlier answer"})

	stub := &reshapeCaptureRunner{stubTurnRunner: stubTurnRunner{name: "primary"}}
	var out strings.Builder
	if err := runPrimary(context.Background(), cfg, sess, stub, nil, nil, "do the thing", &out, nil, nil, nil, nil); err != nil {
		t.Fatalf("runPrimary: %v", err)
	}

	if stub.calls != 1 {
		t.Fatalf("reshape seam called %d times, want 1", stub.calls)
	}

	// The reshaped history must be the settled next-turn shape: the runner's
	// own builder over sess.History as persisted after this turn.
	want, _ := sessionToUnifiedMessagesWithBoundary(sess, "primary", cfg.ExperimentalLazyHistoryManagement)
	if !reflect.DeepEqual(stub.got, want) {
		t.Errorf("reshaped history diverges from the runner builder:\ngot:  %v\nwant: %v", stub.got, want)
	}

	// Guard against comparing two equally-stale snapshots: the settled turn
	// itself must be part of the reshaped shape.
	joined := make([]string, 0, len(stub.got))
	for _, m := range stub.got {
		joined = append(joined, m.Content)
	}
	all := strings.Join(joined, "\n")
	if !strings.Contains(all, "do the thing") || !strings.Contains(all, "stub reply") {
		t.Errorf("reshaped history lacks the settled turn: %v", stub.got)
	}
}

// TestBuildTurnHistoryMatchesRunnerBuilder pins the reshape's "same builder"
// contract at the unit level, for both prompt layouts. Classic prepends the
// percepts as a leading system message; the stable layout (default) keeps the
// history untouched and returns the percepts as per-turn context instead.
func TestBuildTurnHistoryMatchesRunnerBuilder(t *testing.T) {
	agent := local.New("http://unused", "m")
	sess := &session.Session{}
	sess.AddTurn(session.Turn{Role: session.RoleUser, Agent: session.AgentLocal, Content: "earlier question"})
	sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentLocal, Content: "earlier answer"})
	percepts := []string{"percept one"}
	base, _ := sessionToUnifiedMessagesWithBoundary(sess, "primary", false)

	t.Run("classic", func(t *testing.T) {
		cfg := config.Config{Agents: []config.AgentConfig{{Name: "primary", URL: "http://unused", Model: "m", PromptLayout: "classic"}}}
		got, turnCtx := buildTurnHistory(context.Background(), agent, cfg, sess, RolePrimary, escalation.ContextModeFirst, percepts, false)
		want := append([]local.Message{{Role: "system", Content: escalation.FormatPercepts(percepts)}}, base...)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("classic history diverges from the runner pipeline:\ngot:  %v\nwant: %v", got, want)
		}
		if turnCtx != "" {
			t.Errorf("classic layout must not return turn context, got %q", turnCtx)
		}
	})

	t.Run("stable", func(t *testing.T) {
		cfg := config.Config{Agents: []config.AgentConfig{{Name: "primary", URL: "http://unused", Model: "m"}}}
		got, turnCtx := buildTurnHistory(context.Background(), agent, cfg, sess, RolePrimary, escalation.ContextModeFirst, percepts, false)
		if !reflect.DeepEqual(got, base) {
			t.Errorf("stable history must be the bare session history:\ngot:  %v\nwant: %v", got, base)
		}
		if !strings.Contains(turnCtx, "percept one") {
			t.Errorf("stable layout must return the percepts as turn context, got %q", turnCtx)
		}
	})
}

// TestReshapeCacheWarmSeam pins the seam's runner coverage: only localRunner —
// the one runner whose next-turn request is rebuilt from sess.History —
// satisfies cacheWarmReshaper; the dispatch-side entry point is a safe no-op
// for everything else (and for a localRunner without an agent).
func TestReshapeCacheWarmSeam(t *testing.T) {
	if _, ok := interface{}(&cliRunner{}).(cacheWarmReshaper); ok {
		t.Error("cliRunner must not satisfy cacheWarmReshaper — its requests are not rebuilt from sess.History")
	}
	if _, ok := interface{}(&subprocessRunner{}).(cacheWarmReshaper); ok {
		t.Error("subprocessRunner must not satisfy cacheWarmReshaper — its requests are not rebuilt from sess.History")
	}
	if _, ok := interface{}(newLocalRunner(nil, "empty")).(cacheWarmReshaper); !ok {
		t.Error("localRunner must satisfy cacheWarmReshaper")
	}

	cfg := config.Config{Agents: []config.AgentConfig{{Name: "primary", URL: "http://unused", Model: "m"}}}
	sess := &session.Session{}
	reshapeCacheWarm(context.Background(), &stubTurnRunner{name: "plain"}, cfg, sess, RolePrimary, escalation.ContextModeFirst, nil)
	reshapeCacheWarm(context.Background(), newLocalRunner(nil, "empty"), cfg, sess, RolePrimary, escalation.ContextModeFirst, nil)
}
