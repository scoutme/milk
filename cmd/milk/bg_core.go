package main

// Background-job construction shared by the TUI and the ACP server (issue
// #188, the /bg-start and manager rows of the duplication table): how the
// session's job manager is built, and how a user-started job (/bg start) is
// planned and forked. Host-specific halves — how jobs announce themselves
// (tea.Msg vs ACP notifications), the follow-up plumbing, and the transcripts
// around the spawn — stay with the hosts.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/session"
)

// newBackgroundManager builds the session's background-job manager (ADR-0043)
// identically for both hosts: the session-rooted context (jobs outlive the
// turn that spawned them), the configured concurrency and per-job timeout,
// and best-effort per-session state-file persistence — a home dir milk can't
// write to just means no persistence, never a failed session. onStart/
// onDone/onBatchDone are the hosts' own (tea.Msg vs ACP notifications) and are
// wired by the callers.
func newBackgroundManager(ctx context.Context, cfg config.Config, sessID string) *local.Manager {
	mgr := local.NewManager(ctx, cfg.EffectiveMaxBackgroundAgents())
	mgr.SetJobTimeout(cfg.EffectiveBackgroundAgentTimeout())
	if dir, err := config.Dir(); err == nil && sessID != "" {
		mgr.SetStateFile(dir + "/jobs/" + sessID + ".json")
	}
	return mgr
}

// userJobUnavailableMsg is the shared "nothing to fork from" report of
// userJobPlan, in both hosts' words.
const userJobUnavailableMsg = "background agents unavailable — no inference-server-backed agent configured"

// userJobPlan picks what a user-started background job (/bg start) runs on:
// the escalation agent's local backend when there is one (generally the more
// capable one), else the primary's — unlike an agent-initiated
// spawn_background_agent, the in-flight turn may well be running on
// claude-cli, which has no Manager at all, so the job just needs any
// inference-server-backed agent to fork from. modelName falls back to the
// agent's configured name; label is the task truncated to 60 chars (the
// panel/summary width both hosts render). ok=false when nothing is forkable —
// the caller reports userJobUnavailableMsg.
func userJobPlan(cfg config.Config, escLocal, primaryLocal *local.Agent, task string) (agent *local.Agent, modelName, label string, ok bool) {
	agent = escLocal
	ac := cfg.EscalationAgentConfig()
	if agent == nil {
		agent = primaryLocal
		ac = cfg.ActiveAgent()
	}
	if agent == nil {
		return nil, "", "", false
	}
	modelName = ac.Model
	if modelName == "" {
		modelName = ac.Name
	}
	return agent, modelName, userJobLabel(task), true
}

// userJobLabel is the task truncated to 60 chars — the panel/summary width
// both hosts render.
func userJobLabel(task string) string {
	if len(task) > 60 {
		return task[:57] + "..."
	}
	return task
}

// errUserJobUnavailable is userJobPlanFor's "nothing to fork from" error.
var errUserJobUnavailable = errors.New(userJobUnavailableMsg)

// bgAgentCandidates lists the configured agents a user-started job can run
// on: the inference-server-backed ones (claude-cli and subprocess agents have
// no Manager to fork from).
func bgAgentCandidates(cfg config.Config) []string {
	var names []string
	for _, ac := range cfg.Agents {
		if !ac.IsExternalProcess() {
			names = append(names, ac.Name)
		}
	}
	return names
}

// userJobDefaultName is the agent a blank answer / no --agent resolves to, for
// the prompt: the configured background_agent, else what userJobPlan picks.
func userJobDefaultName(cfg config.Config, escLocalAvailable bool) string {
	if s := strings.TrimSpace(cfg.BackgroundAgent); s != "" {
		return s
	}
	if escLocalAvailable {
		return cfg.EscalationAgentConfig().Name
	}
	return cfg.ActiveAgent().Name
}

// userJobPlanFor is userJobPlan with an explicit agent choice, the way
// workflow roles take one: spec is an agent name or the aliases "primary" /
// "escalation"; blank falls back to cfg.BackgroundAgent, then to userJobPlan's
// default. The session's own wired primary/escalation agents are reused as-is
// (permission asks, token accounting); any other configured agent comes from
// build, which may be nil when the host can't construct one.
func userJobPlanFor(cfg config.Config, escLocal, primaryLocal *local.Agent, build func(name string) (*local.Agent, error), spec, task string) (agent *local.Agent, modelName, label string, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		spec = strings.TrimSpace(cfg.BackgroundAgent)
	}
	if spec == "" {
		a, m, l, ok := userJobPlan(cfg, escLocal, primaryLocal, task)
		if !ok {
			return nil, "", "", errUserJobUnavailable
		}
		return a, m, l, nil
	}

	var ac config.AgentConfig
	switch {
	case strings.EqualFold(spec, "primary"):
		ac, agent = cfg.ActiveAgent(), primaryLocal
	case strings.EqualFold(spec, "escalation"):
		ac, agent = cfg.EscalationAgentConfig(), escLocal
	default:
		found, ok := findAgentByName(cfg, spec)
		if !ok {
			return nil, "", "", fmt.Errorf("unknown agent %q (available: %s)", spec, strings.Join(bgAgentCandidates(cfg), ", "))
		}
		ac = found
		switch {
		case strings.EqualFold(ac.Name, cfg.ActiveAgent().Name):
			agent = primaryLocal
		case strings.EqualFold(ac.Name, cfg.EscalationAgentConfig().Name):
			agent = escLocal
		}
	}
	if ac.IsExternalProcess() {
		return nil, "", "", fmt.Errorf("agent %q is not inference-server-backed — a background job needs one (available: %s)", ac.Name, strings.Join(bgAgentCandidates(cfg), ", "))
	}
	if agent == nil && build != nil {
		built, berr := build(ac.Name)
		if berr != nil {
			return nil, "", "", fmt.Errorf("agent %q: %w", ac.Name, berr)
		}
		agent = built
	}
	if agent == nil {
		return nil, "", "", errUserJobUnavailable
	}
	modelName = ac.Model
	if modelName == "" {
		modelName = ac.Name
	}
	return agent, modelName, userJobLabel(task), nil
}

// parseBgStartArgs splits "/bg start" arguments into an optional leading
// "--agent <name>" (or "--agent=<name>") and the task text. Only a leading
// flag is recognised, so a task that itself mentions "--agent" is left alone.
func parseBgStartArgs(arg string) (agent, task string, err error) {
	arg = strings.TrimSpace(arg)
	switch {
	case strings.HasPrefix(arg, "--agent="):
		rest := strings.TrimPrefix(arg, "--agent=")
		name, tail, _ := strings.Cut(rest, " ")
		agent, arg = name, strings.TrimSpace(tail)
	case arg == "--agent" || strings.HasPrefix(arg, "--agent "):
		rest := strings.TrimSpace(strings.TrimPrefix(arg, "--agent"))
		name, tail, _ := strings.Cut(rest, " ")
		agent, arg = name, strings.TrimSpace(tail)
	default:
		return "", arg, nil
	}
	if agent == "" {
		return "", "", errors.New("flag --agent requires a value")
	}
	return agent, arg, nil
}

// userJobRunner builds the spawn body both hosts pass to Manager.Spawn for a
// user-started job: fork the planned agent on the task text, persisting
// through the session's memory store. mgr supplies the job's follow-up-input
// channel (send_background_agent — issue #250); it may be nil. The job body
// consolidates memory when it finishes (#375 proposal 3) — background runs
// used to write percepts without ever running a pass of their own.
func userJobRunner(agent *local.Agent, mgr *local.Manager, cfg config.Config, cwd, task string, mem *memory.Store, images []local.ContentPart) func(context.Context, string, io.Writer) (string, session.TokenUsage, error) {
	return func(ctx context.Context, jobID string, out io.Writer) (string, session.TokenUsage, error) {
		defer consolidateAfterTurn(mem, cfg)
		var inbox chan string
		if mgr != nil {
			inbox = mgr.Inbox(jobID)
		}
		// Attachments the user had staged when the job spawned (issue: they
		// used to be silently ignored) ride the job's ctx as per-call image
		// parts, exactly like a normal turn's.
		if len(images) > 0 {
			ctx = local.WithImageParts(ctx, images)
		}
		return agent.RunBackgroundTask(ctx, jobID, inbox, cwd, task, "", out, mem)
	}
}
