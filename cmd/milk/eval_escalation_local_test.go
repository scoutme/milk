package main

// Eval test: measures the input-token proxy (total message character count)
// sent to a local-provider escalation agent under four session states, and
// compares the result against the pre-optimization baseline behaviour.
//
// Pre-optimization baseline is reproduced inline: escalationLocalHistory
// (full session, no orientation, no percepts) fed to agent.Run with the same
// WithTagCallbacks/WithMemConfig setup as the old runEscalationLocal.
// New behaviour is the live runEscalationLocal call.
//
// Character count across all messages in the HTTP request body is used as a
// token proxy — directionally accurate; real token counts depend on the
// model's BPE vocabulary.
//
// Run with:  go test ./cmd/milk/ -run TestEval_EscalationLocal -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/agent/claude"
	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/session"
)

// evalCaptureServer records every chat-completion request body and replies
// with a minimal SSE stream so the agent completes cleanly.
type evalCaptureServer struct {
	mu       sync.Mutex
	requests [][]evalCapturedMsg
}

type evalCapturedMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (cs *evalCaptureServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []evalCapturedMsg `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			cs.mu.Lock()
			cs.requests = append(cs.requests, req.Messages)
			cs.mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := map[string]any{
			"choices": []map[string]any{
				{"delta": map[string]any{"content": "ok"}, "finish_reason": nil},
			},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	})
}

func (cs *evalCaptureServer) lastChars() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.requests) == 0 {
		return 0
	}
	msgs := cs.requests[len(cs.requests)-1]
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
	}
	return n
}

func (cs *evalCaptureServer) lastSummary() string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.requests) == 0 {
		return "(none)"
	}
	msgs := cs.requests[len(cs.requests)-1]
	var parts []string
	for _, m := range msgs {
		parts = append(parts, fmt.Sprintf("%s:%d", m.Role, len(m.Content)))
	}
	return strings.Join(parts, " ")
}

// hasEscalationTurns reports whether the last captured request contains any
// assistant message whose content matches the known escalation reply.
func (cs *evalCaptureServer) hasEscalationTurns(escalationReply string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.requests) == 0 {
		return false
	}
	for _, m := range cs.requests[len(cs.requests)-1] {
		if strings.Contains(m.Content, escalationReply[:min(40, len(escalationReply))]) {
			return true
		}
	}
	return false
}

// makeEvalCfg returns a minimal config whose escalation agent points at url.
func makeEvalCfg(escURL string) config.Config {
	return config.Config{
		Agent:           "primary",
		EscalationAgent: "esc",
		Agents: []config.AgentConfig{
			{Name: "primary", URL: "http://unused", Model: "primary-model"},
			{Name: "esc", URL: escURL, Model: "esc-model"},
		},
	}
}

// evalTurn builds a deterministic session.Turn.
func evalTurn(role session.Role, agent session.Agent, content string) session.Turn {
	return session.Turn{Role: role, Agent: agent, Content: content,
		Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

// buildBaselineAgent builds a local agent that mirrors the pre-optimization
// runEscalationLocal setup (WithMemConfig + WithTagCallbacks) but without the
// new orientation message or scoped history.
func buildBaselineAgent(escURL string) *local.Agent {
	ac := config.AgentConfig{Name: "esc", URL: escURL, Model: "esc-model"}
	a := local.NewFromConfig(ac).AsEscalationTarget("esc")
	nonce := claude.GenerateNonce()
	a = a.WithMemConfig(local.MemConfig{}).WithTagCallbacks(nonce, "primary", "esc",
		func(string) {}, func(string, string) {})
	return a
}

// scenariosForEval builds session fixtures for the four key routing cases.
// The escalation reply is intentionally large (realistic tool-use response)
// so that excluding it on stale-returning turns saves more chars than the
// orientation overhead adds.
func scenariosForEval() (map[string]*session.Session, string) {
	// A realistic escalation response: refactoring work with code diffs.
	escalationReply := strings.Repeat(
		"Refactored internal/auth/jwt.go: replaced HS256 with RS256, added exp/nbf/aud "+
			"claim validation, thread-safe key cache, and unit tests in auth_test.go. "+
			"The key rotation logic now lives in KeyManager.Rotate() (line 142). "+
			"PR diff follows:\n", 12) // ~700 chars × 12 ≈ 8 400 chars

	const lw1 = "Added unit tests for the JWT validator. Coverage is now 94%."
	const lw2 = "Fixed a nil pointer in the router triggered by missing aud claim."
	const lw3 = "Refactored the session store to use Redis instead of in-memory map."
	const lw4 = "Updated deployment config to set JWT_SECRET from Vault."
	const lw5 = "Wrote migration script for existing sessions (~300 rows)."
	const lw6 = "Reviewed and merged PRs #42 and #44."
	const lw7 = "Added rate-limiting middleware (token bucket, 100 req/s)."
	const lw8 = "Pinned dependency versions in go.mod after audit."
	const lw9 = "Ran benchmarks: 12% latency improvement vs previous store."

	// ── first: no prior escalation ─────────────────────────────────────
	first := &session.Session{
		State: session.StateRouting,
		History: []session.Turn{
			evalTurn(session.RoleUser, session.AgentLocal, "help me refactor the auth module"),
			evalTurn(session.RoleAssistant, session.AgentLocal, "Sure, let me look at the code."),
		},
	}

	// ── returning-stale-need: topic switched after escalation ──────────
	staleNeed := &session.Session{
		State: session.StateRouting,
		// No EscalationSessionID: local providers do not persist session IDs.
		History: []session.Turn{
			evalTurn(session.RoleUser, session.AgentEscalation, "help me refactor the auth module"),
			evalTurn(session.RoleAssistant, session.AgentEscalation, escalationReply),
			evalTurn(session.RoleUser, session.AgentLocal, "add unit tests"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw1),
			evalTurn(session.RoleUser, session.AgentLocal, "now let's work on the deployment pipeline"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw4),
		},
		CurrentNeed:      "set up the deployment pipeline",
		CurrentNeedSetAt: 5, // set after escalation ended at index 1 → stale-need fires
		EscalationBrief:  "refactor auth module",
	}
	staleNeed.RebuildSummaryBricks(12000)

	// ── returning-stale-gap: 9 local turns since escalation (≥ 8) ─────
	staleGap := &session.Session{
		State: session.StateRouting,
		// No EscalationSessionID: local providers do not persist session IDs.
		History: []session.Turn{
			evalTurn(session.RoleUser, session.AgentEscalation, "help me refactor the auth module"),
			evalTurn(session.RoleAssistant, session.AgentEscalation, escalationReply),
			evalTurn(session.RoleUser, session.AgentLocal, "add unit tests"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw1),
			evalTurn(session.RoleUser, session.AgentLocal, "fix nil pointer"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw2),
			evalTurn(session.RoleUser, session.AgentLocal, "refactor session store"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw3),
			evalTurn(session.RoleUser, session.AgentLocal, "update deployment config"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw4),
			evalTurn(session.RoleUser, session.AgentLocal, "write migration script"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw5),
			evalTurn(session.RoleUser, session.AgentLocal, "review PRs"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw6),
			evalTurn(session.RoleUser, session.AgentLocal, "add rate limiting"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw7),
			evalTurn(session.RoleUser, session.AgentLocal, "pin dependencies"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw8),
			evalTurn(session.RoleUser, session.AgentLocal, "run benchmarks"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw9),
		},
		// need set at position 19, last escalation at index 1 → NeedChangedSinceLastEscalation=true
		// AND turn gap (9 local turns) → both conditions fire
		CurrentNeed:      "benchmark the session store",
		CurrentNeedSetAt: 19,
		EscalationBrief:  "refactor auth module",
	}
	staleGap.RebuildSummaryBricks(12000)

	// ── returning-fresh: 2 local turns, same topic, no stale condition ─
	fresh := &session.Session{
		State: session.StateRouting,
		// No EscalationSessionID: local providers do not persist session IDs.
		History: []session.Turn{
			evalTurn(session.RoleUser, session.AgentEscalation, "help me refactor the auth module"),
			evalTurn(session.RoleAssistant, session.AgentEscalation, escalationReply),
			evalTurn(session.RoleUser, session.AgentLocal, "add unit tests"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw1),
			evalTurn(session.RoleUser, session.AgentLocal, "fix nil pointer"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw2),
		},
		// need set at turn 1, escalation came after → NeedChangedSinceLastEscalation=false
		CurrentNeed:      "refactor the auth module",
		CurrentNeedSetAt: 1,
		EscalationBrief:  "refactor auth module",
	}
	fresh.RebuildSummaryBricks(12000)

	return map[string]*session.Session{
		"first":                first,
		"returning-stale-need": staleNeed,
		"returning-stale-gap":  staleGap,
		"returning-fresh":      fresh,
	}, escalationReply
}

func TestEval_EscalationLocal(t *testing.T) {
	srv := &evalCaptureServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	cfg := makeEvalCfg(ts.URL)
	const prompt = "escalate this further please"

	scenarios, escalationReply := scenariosForEval()
	order := []string{"first", "returning-stale-need", "returning-stale-gap", "returning-fresh"}

	type result struct {
		baseChars           int
		newChars            int
		baseSumm            string
		newSumm             string
		newHasEscBlock      bool
		baselineHasEscBlock bool
	}
	results := make(map[string]result, len(order))

	for _, name := range order {
		orig := scenarios[name]

		// ── baseline: pre-optimization path ───────────────────────────────
		// Full history via escalationLocalHistory, WithTagCallbacks mirroring
		// old runEscalationLocal, no orientation, no percept injection.
		baseSess := *orig
		baseSess.History = append([]session.Turn(nil), orig.History...)
		baseAgent := buildBaselineAgent(ts.URL)
		var baseOut strings.Builder
		baseAgent.Run(context.Background(), escalationLocalHistory(&baseSess, prompt), prompt, &baseOut, &baseSess, nil) //nolint:errcheck
		baseChars := srv.lastChars()
		baseSumm := srv.lastSummary()
		baseHasEsc := srv.hasEscalationTurns(escalationReply)

		// ── new path: live runEscalation (via localRunner) ───────────────
		newSess := *orig
		newSess.History = append([]session.Turn(nil), orig.History...)
		newAgent := local.NewFromConfig(cfg.EscalationAgentConfig()).AsEscalationTarget("esc")
		var newOut strings.Builder
		if err := runEscalation(context.Background(), cfg, &newSess, newLocalRunner(newAgent, "esc"), "", nil, prompt, &newOut); err != nil {
			t.Errorf("scenario %q: runEscalation: %v", name, err)
		}
		newChars := srv.lastChars()
		newSumm := srv.lastSummary()
		newHasEsc := srv.hasEscalationTurns(escalationReply)

		results[name] = result{baseChars, newChars, baseSumm, newSumm, newHasEsc, baseHasEsc}
	}

	// ── Report ────────────────────────────────────────────────────────────
	t.Log("\n=== Escalation-local context eval (input-char proxy) ===")
	t.Logf("%-28s  %8s  %8s  %9s  %s", "Scenario", "Baseline", "New", "Delta(%)", "Esc-block in new?")
	t.Logf("%s", strings.Repeat("-", 80))
	for _, name := range order {
		r := results[name]
		delta := r.newChars - r.baseChars
		var pct float64
		if r.baseChars > 0 {
			pct = float64(delta) / float64(r.baseChars) * 100
		}
		escInNew := "yes"
		if !r.newHasEscBlock {
			escInNew = "no (excluded)"
		}
		t.Logf("%-28s  %8d  %8d  %+9.1f%%  %s", name, r.baseChars, r.newChars, pct, escInNew)
		t.Logf("  baseline: %s", r.baseSumm)
		t.Logf("  new:      %s", r.newSumm)
	}
	t.Log(strings.Repeat("-", 80))

	// ── Assertions ────────────────────────────────────────────────────────

	// Stale-returning: the prior escalation block must be absent from the new
	// request (it was scoped out by escalationLocalHistoryFresh).
	for _, name := range []string{"returning-stale-need", "returning-stale-gap"} {
		if r := results[name]; r.newHasEscBlock {
			t.Errorf("scenario %q: prior escalation turns must be excluded from new request but were found", name)
		}
		if r := results[name]; !r.baselineHasEscBlock {
			t.Errorf("scenario %q: baseline should include prior escalation turns but they were absent", name)
		}
	}

	// Stale-returning: net char count must be lower with the new path.
	// This holds when the excluded escalation history is larger than the
	// added orientation overhead — which is the case with a realistic session.
	for _, name := range []string{"returning-stale-need", "returning-stale-gap"} {
		r := results[name]
		if r.newChars >= r.baseChars {
			t.Errorf("scenario %q: new path should send fewer chars; baseline=%d new=%d delta=%+d",
				name, r.baseChars, r.newChars, r.newChars-r.baseChars)
		}
	}

	// Returning-fresh: no stale condition fires; escalation history is preserved.
	if r := results["returning-fresh"]; !r.newHasEscBlock {
		t.Errorf("scenario 'returning-fresh': escalation turns must be present (no stale condition)")
	}

	// Returning-fresh: orientation adds bounded overhead (≤ 50% of baseline).
	if r := results["returning-fresh"]; r.baseChars > 0 {
		ratio := float64(r.newChars) / float64(r.baseChars)
		if ratio > 1.5 {
			t.Errorf("scenario 'returning-fresh': orientation overhead too large (%.1f×); baseline=%d new=%d",
				ratio, r.baseChars, r.newChars)
		}
	}
}
