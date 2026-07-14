package main

import (
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/session"
)

// TestBuildWorkflowRunners_ScratchSession verifies that each role runner receives
// its own scratch session — not the REPL session passed to buildWorkflowRunners.
// The scratch session must have an empty history regardless of what the REPL
// session contains, so workflow agent turns do not see prior REPL conversation.
func TestBuildWorkflowRunners_ScratchSession(t *testing.T) {
	// Build a REPL session with two history entries.
	replSess := &session.Session{
		ID:    "repl-session",
		State: session.StateRouting,
		History: []session.Turn{
			{Role: session.RoleUser, Content: "hello"},
			{Role: session.RoleAssistant, Content: "world"},
		},
	}

	// Zero-value config and dispatchAgents so all roles resolve via the
	// "matches primaryName (empty string)" branch in buildWorkflowRunners.
	agentNames := map[string]string{
		"designer":  "",
		"generator": "",
		"evaluator": "",
	}
	da := &dispatchAgents{} // primary and escalation both nil → primaryName = ""

	runners, err := buildWorkflowRunners(agentNames, config.Config{}, replSess, nil, da)
	if err != nil {
		t.Fatalf("buildWorkflowRunners returned error: %v", err)
	}

	for role, r := range runners {
		wtr, ok := r.(*workflowTurnRunner)
		if !ok {
			t.Fatalf("role %q: expected *workflowTurnRunner, got %T", role, r)
		}
		if wtr.sess == replSess {
			t.Errorf("role %q: runner shares the REPL session pointer — must use a scratch session", role)
		}
		if wtr.sess == nil {
			t.Errorf("role %q: runner has nil session", role)
		}
		if len(wtr.sess.History) != 0 {
			t.Errorf("role %q: scratch session has %d history entries, want 0", role, len(wtr.sess.History))
		}
	}
}

// TestBuildWorkflowRunners_EachRoleGetsDistinctSession verifies that two roles
// do not share the same scratch session instance (separate per-role isolation).
func TestBuildWorkflowRunners_EachRoleGetsDistinctSession(t *testing.T) {
	replSess := &session.Session{ID: "repl", State: session.StateRouting}
	agentNames := map[string]string{
		"designer":  "",
		"generator": "",
	}
	da := &dispatchAgents{}

	runners, err := buildWorkflowRunners(agentNames, config.Config{}, replSess, nil, da)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sessions := make(map[*session.Session]string)
	for role, r := range runners {
		wtr := r.(*workflowTurnRunner)
		if prev, seen := sessions[wtr.sess]; seen {
			t.Errorf("roles %q and %q share the same scratch session — each role must get its own", prev, role)
		}
		sessions[wtr.sess] = role
	}
}
