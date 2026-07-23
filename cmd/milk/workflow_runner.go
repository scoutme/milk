package main

import (
	"context"
	"fmt"
	"io"

	"github.com/google/uuid"

	"github.com/scoutme/milk/internal/agent/claude"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
)

// workflowTurnRunner adapts a TurnRunner (cmd/milk interface) to
// workflow.TurnRunner, capturing the session/config/memory context needed
// by Execute so workflow code stays clean of those dependencies.
//
// Each role in a workflow gets its own workflowTurnRunner instance with its own
// sessionID string — resume only applies within the same role across passes,
// never across roles.
type workflowTurnRunner struct {
	inner     TurnRunner
	cfg       config.Config
	sess      *session.Session // scratch session for this role (isolated from REPL)
	replSess  *session.Session // REPL session for token accounting only
	mem       *memory.Store
	role      AgentRole
	roleName  string // human-readable role (designer, generator, evaluator)
	nonce     string
	sessionID string // persists across passes for this role; set after first Execute
}

func (r *workflowTurnRunner) Name() string { return r.inner.Name() }

func (r *workflowTurnRunner) Run(ctx context.Context, prompt string, out io.Writer) (string, error) {
	var ctxMode escalation.ContextMode
	if r.sessionID != "" {
		ctxMode = escalation.ContextModeResume
	} else {
		ctxMode = escalation.ContextModeFirst
	}

	if r.nonce == "" {
		r.nonce = claude.GenerateNonce()
	}

	res, err := r.inner.Execute(
		ctx,
		r.cfg,
		r.sess,
		r.mem,
		r.role,
		ctxMode,
		r.sessionID,
		r.nonce,
		nil,   // percepts: not injected for workflow turns
		false, // injectInstructions: not needed for workflow turns
		prompt,
		TurnCallbacks{},
		out,
	)
	if err != nil {
		return "", err
	}
	if res.EscalationReason != "" {
		// Local agent fired an escalation signal — no escalation target exists in a
		// workflow role. Treat it as an error so the workflow halts with a clear message.
		return "", fmt.Errorf("workflow role %q escalated unexpectedly: %s", r.roleName, res.EscalationReason)
	}
	if res.NewSessionID != "" {
		r.sessionID = res.NewSessionID
	}
	// Accumulate token usage into the REPL session so /usage shows workflow costs.
	if r.replSess != nil && (res.InputTokens > 0 || res.OutputTokens > 0) {
		model := r.inner.Name()
		r.replSess.AddTokensFull(model, "workflow:"+r.roleName,
			res.InputTokens, res.OutputTokens, res.CacheRead, res.CacheCreate)
	}
	return res.Text, nil
}

// newWorkflowSession creates a lightweight scratch session for a workflow role.
// It is isolated from the REPL session: empty history, no cwd binding, never
// persisted to disk. Each role gets its own instance so cross-pass resume within
// a role works independently of the live REPL conversation.
func newWorkflowSession() *session.Session {
	return &session.Session{
		ID:      "wf-" + uuid.New().String(),
		State:   session.StateRouting,
		History: []session.Turn{},
	}
}

// buildWorkflowRunners constructs a workflow.TurnRunner adapter for each role
// using the resolved agent name map.
//
// da must be the TUI-wired agents from buildTUIAgents — the primary and escalation
// runners it contains already carry the correct permission handlers, tool-use
// callbacks, and skip-permissions setting for the current turn.
//
// cliPC and newInput are forwarded to any cliRunner built for roles that target
// a claude-cli agent by name.
//
// Each role receives its own scratch session (not the REPL session) so workflow
// turns do not contaminate the live conversation history.
func buildWorkflowRunners(
	agentNames map[string]string,
	cfg config.Config,
	replSess *session.Session,
	mem *memory.Store,
	da *dispatchAgents,
	cliPC permContext,
	newInput func() inputReader,
) (map[string]workflow.TurnRunner, error) {
	primaryName := ""
	if da.primary != nil {
		primaryName = da.primary.Name()
	}
	escalationName := ""
	if da.escalation != nil {
		escalationName = da.escalation.Name()
	}

	out := make(map[string]workflow.TurnRunner, len(agentNames))
	for role, name := range agentNames {
		var inner TurnRunner
		switch name {
		case primaryName:
			inner = da.primary
		case escalationName:
			inner = da.escalation
		default:
			ac, ok := findAgentByName(cfg, name)
			if !ok {
				return nil, fmt.Errorf("workflow role %q: agent %q not found in config", role, name)
			}
			if ac.IsCLI() {
				cliAg := newCLIAgent(ac)
				cr := newCLIRunner(cliAg, name, cliPC, newInput)
				if servers := cfg.EffectiveMCPServers(name); len(servers) > 0 {
					cr = cr.withMCPServers(servers)
				}
				inner = cr
			} else {
				var err error
				inner, err = getOrBuildToolRunner(context.Background(), name, cfg, da)
				if err != nil {
					return nil, fmt.Errorf("workflow role %q: %w", role, err)
				}
			}
		}

		out[role] = &workflowTurnRunner{
			inner:    inner,
			cfg:      cfg,
			sess:     newWorkflowSession(),
			replSess: replSess,
			mem:      mem,
			role:     RoleWorkflow,
			roleName: role,
		}
	}
	return out, nil
}
