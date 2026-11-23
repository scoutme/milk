package main

// The local-agent wiring, shared by every host (issue #188). Building a
// *local.Agent from its config is already one place (local.NewFromConfig);
// everything a host then adds on top — permission asks, the background ask and
// safety gate, tool hooks, thinking, the task store, the background manager —
// used to be spelled out twice (buildTUIAgents for the TUI,
// acpSession.buildRunners' wireLocal for ACP) and once more, half-wired, for
// agent-as-tool runners (buildToolRunner). Each host now only supplies its
// callbacks as a localAgentWiring value; this file applies them, so the
// wirings cannot drift and every agent a session runs asks the same way.

import (
	"context"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/tasks"
)

// localAgentWiring is the set of host callbacks and session-owned
// collaborators a host attaches to a local-provider agent. Every field is
// optional; wireLocalAgent skips what is nil. Roles differ only in what they
// fill in: the session's primary/escalation agents get the full set, an
// agent-as-tool runner may get only the permission half.
type localAgentWiring struct {
	// perms: the persistent grant store plus the live ask. permStore may be
	// nil (grants just aren't persisted); a nil permAsk falls back to the
	// agent's configured default handling.
	permStore *local.PermStore
	permAsk   func(tool, summary string) bool
	// bgPermAsk is the three-source race for background-job tool asks
	// (ADR-0052). nil = jobs never ask.
	bgPermAsk func(jobID, tool, summary string) bool
	// safetyAsk is the doom-loop gate for unattended contexts; see
	// WithSafetyAsk. nil = no gate.
	safetyAsk func(jobID, tool, summary string) local.SafetyOutcome
	// userAsk answers the ask_user tool (issue #249) — the question race
	// over direct input and remote oversight. nil = ask_user unavailable.
	userAsk func(ctx context.Context, question string, options []string) (string, bool)
	// bgUserAsk is the unattended variant (background jobs, workflow steps),
	// bounded by the timed "no answer". nil = unattended contexts cannot ask.
	bgUserAsk func(ctx context.Context, jobID, question string, options []string) (string, bool)
	// skipPerms is the live counterpart of the config's
	// dangerously_skip_permissions: the host's /skip-permissions toggle.
	skipPerms func() bool
	// permMode is the session allow-all/deny-all mode (issue #302),
	// consulted before the grant store and any prompt. nil = no mode.
	permMode func() (decided bool, allow bool)
	// steerDrain pops input the user queued while this turn runs (issue
	// #441) at every model-request boundary of the running turn. Session
	// turn agents only: buildToolRunner deliberately does not copy it into
	// agent-as-tool runners (a nested tool-agent call must never consume
	// the session's queue), and background clones drop it. nil = the agent
	// never steers — its host's queue keeps flushing one per next turn.
	steerDrain func() []string

	// Hooks. All optional; nil leaves the agent's no-op default.
	onOpenFile          func(path string) error
	onRetract           func(from, to string)
	onToolUse           func(id, name, summary string, rawInput map[string]any)
	onToolResult        func(id, name, result string, isError bool)
	onThinking          func(text string)
	onReasoningPromoted func()

	// Session-owned collaborators. nil = the matching tools stay off.
	taskStore *tasks.Store
	mgr       *local.Manager
}

// wireLocalAgent applies w to a in the one order the host wirings have
// converged on. a itself is never mutated for the caller's copy: the With*
// calls return shallow copies, and a nil agent wires to nil (a role with no
// local provider).
func wireLocalAgent(a *local.Agent, w localAgentWiring) *local.Agent {
	if a == nil {
		return nil
	}
	if w.skipPerms != nil {
		a = a.WithSkipPermissionsFunc(w.skipPerms)
	}
	if w.permMode != nil {
		a = a.WithPermissionModeFunc(w.permMode)
	}
	if w.steerDrain != nil {
		a = a.WithSteerDrain(w.steerDrain)
	}
	if w.permStore != nil || w.permAsk != nil {
		a = a.WithPermissions(w.permStore, w.permAsk)
	}
	if w.bgPermAsk != nil {
		a = a.WithBackgroundPermissionAsk(w.bgPermAsk)
	}
	if w.safetyAsk != nil {
		a = a.WithSafetyAsk(w.safetyAsk)
	}
	if w.userAsk != nil {
		a = a.WithUserAsk(w.userAsk)
	}
	if w.bgUserAsk != nil {
		a = a.WithBackgroundUserAsk(w.bgUserAsk)
	}
	if w.onOpenFile != nil {
		a = a.WithOnOpenFile(w.onOpenFile)
	}
	if w.onRetract != nil {
		a = a.WithOnRetract(w.onRetract)
	}
	if w.onToolUse != nil {
		a = a.WithOnToolUse(w.onToolUse)
	}
	if w.onToolResult != nil {
		a = a.WithOnToolResult(w.onToolResult)
	}
	if w.onThinking != nil {
		a = a.WithOnThinking(w.onThinking)
	}
	if w.onReasoningPromoted != nil {
		a = a.WithOnReasoningPromoted(w.onReasoningPromoted)
	}
	if w.taskStore != nil {
		a = a.WithTaskStore(tasks.NewAdapter(w.taskStore))
	}
	if w.mgr != nil {
		a.SetBackgroundManager(w.mgr)
	}
	return a
}
