package main

// Regression tests for the dispatch token-accounting call sites.
//
// runPrimaryWithSession previously called sess.AddTokensFull with CacheCreate
// and CacheRead swapped (AddTokensFull's declared order is
// (prompt, completion, cacheRead, cacheCreation), but the call passed
// res.CacheCreate before res.CacheRead). This bug predates the prompt-caching
// feature itself — it could not surface until some primary-agent path started
// reporting nonzero, *distinct* cache-read and cache-creation values — but it
// is fixed here because this sprint is already modifying/verifying this exact
// call site end-to-end. runEscalationWithSession's call was already correct
// and serves as the reference order these tests pin down for both sites.

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/session"
)

// withTempHome points $HOME at a scratch directory for the duration of the
// test, so session.Save (called by runPrimaryWithSession/runEscalationWithSession)
// writes to a throwaway ~/.milk/sessions instead of the real one.
func withTempHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

// fakeTokenRunner is a minimal TurnRunner that returns a fixed TurnResult,
// used to pin down how its CacheRead/CacheCreate fields land in the session.
type fakeTokenRunner struct {
	name string
	res  TurnResult
}

func (r *fakeTokenRunner) Name() string { return r.name }
func (r *fakeTokenRunner) IsCLI() bool  { return false }
func (r *fakeTokenRunner) Ping() error  { return nil }

func (r *fakeTokenRunner) Execute(
	_ context.Context,
	_ config.Config,
	_ *session.Session,
	_ *memory.Store,
	_ AgentRole,
	_ escalation.ContextMode,
	_ string, _ string,
	_ []string,
	_ bool,
	_ string,
	_ TurnCallbacks,
	_ io.Writer,
) (TurnResult, error) {
	return r.res, nil
}

func (r *fakeTokenRunner) RunToolCall(_ context.Context, _ config.Config, _ string, _ io.Writer) (string, error) {
	return "", nil
}

// TestRunPrimaryWithSession_TokenAccountingOrder pins down that a TurnResult
// with distinct CacheRead and CacheCreate values lands in sess.Tokens with
// the correct field assignment (not swapped). Fails against the pre-fix
// dispatch.go, which passed (CacheCreate, CacheRead) to AddTokensFull instead
// of (CacheRead, CacheCreate).
func TestRunPrimaryWithSession_TokenAccountingOrder(t *testing.T) {
	withTempHome(t)

	runner := &fakeTokenRunner{
		name: "test-primary",
		res: TurnResult{
			Text:         "",
			InputTokens:  1000,
			OutputTokens: 20,
			CacheRead:    300,
			CacheCreate:  50,
		},
	}

	sess := &session.Session{ID: "test-session"}
	var out bytes.Buffer

	// A named, non-external-process agent so ActiveAgent() (and hence the
	// dispatched model name) is non-empty — AddTokensFull is a no-op for an
	// empty model, which would otherwise mask the very bug under test.
	cfg := config.Config{Agents: []config.AgentConfig{{Name: "test-primary", Provider: "local"}}}

	if err := runPrimaryWithSession(context.Background(), cfg, sess, runner, nil, nil,
		"hi", "hi", &out, nil, nil); err != nil {
		t.Fatalf("runPrimaryWithSession returned error: %v", err)
	}

	usage := onlyTokenUsage(t, sess)
	if usage.CacheRead != 300 {
		t.Errorf("want CacheRead=300, got %d", usage.CacheRead)
	}
	if usage.CacheCreation != 50 {
		t.Errorf("want CacheCreation=50, got %d", usage.CacheCreation)
	}
}

// TestRunEscalationWithSession_TokenAccountingOrder is the escalation-side
// counterpart: runEscalationWithSession's call site was already correct
// before this sprint, so this pins that it stays correct.
func TestRunEscalationWithSession_TokenAccountingOrder(t *testing.T) {
	withTempHome(t)

	runner := &fakeTokenRunner{
		name: "test-escalation",
		res: TurnResult{
			Text:         "",
			InputTokens:  2000,
			OutputTokens: 40,
			CacheRead:    900,
			CacheCreate:  70,
		},
	}

	sess := &session.Session{ID: "test-session"}
	var out bytes.Buffer

	if err := runEscalationWithSession(context.Background(), config.Config{}, sess, runner, "brief", nil,
		"hi", "hi", "", &out, nil); err != nil {
		t.Fatalf("runEscalationWithSession returned error: %v", err)
	}

	usage := onlyTokenUsage(t, sess)
	if usage.CacheRead != 900 {
		t.Errorf("want CacheRead=900, got %d", usage.CacheRead)
	}
	if usage.CacheCreation != 70 {
		t.Errorf("want CacheCreation=70, got %d", usage.CacheCreation)
	}
}

// onlyTokenUsage asserts sess.Tokens has exactly one entry and returns it.
func onlyTokenUsage(t *testing.T, sess *session.Session) *session.TokenUsage {
	t.Helper()
	if len(sess.Tokens) != 1 {
		t.Fatalf("want exactly 1 token usage entry, got %d: %+v", len(sess.Tokens), sess.Tokens)
	}
	for _, u := range sess.Tokens {
		return u
	}
	panic("unreachable")
}
