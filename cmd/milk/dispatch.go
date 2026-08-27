package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/scoutme/milk/internal/agent/claude"
	"github.com/scoutme/milk/internal/claudesettings"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
)

// executeWithRetry wraps runner.Execute with the same transient network/stream
// error retry (HTTP/2 stream reset or GOAWAY) that workflow turns already get
// via workflow.Turn — without it, a single upstream hiccup ends a standalone
// turn that a workflow turn would have silently retried through.
func executeWithRetry(
	ctx context.Context,
	runner TurnRunner,
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
	for attempt := 0; ; attempt++ {
		res, err := runner.Execute(ctx, cfg, sess, mem, role, ctxMode, sessionID, nonce, percepts, injectInstructions, prompt, cbs, out)
		if err == nil || attempt >= workflow.MaxTurnRetries || !workflow.IsRetryableTurnError(err) {
			return res, err
		}
		fmt.Fprintf(out, "\n[transient error, retrying %d/%d: %v]\n", attempt+1, workflow.MaxTurnRetries, err)
		select {
		case <-time.After(workflow.TurnRetryBackoff(attempt)):
		case <-ctx.Done():
			return TurnResult{}, ctx.Err()
		}
	}
}

// runPrimary executes one primary-agent turn using runner.
// It handles all session bookkeeping: context-mode resolution, nonce management,
// state transitions, turn recording, token accounting, summary rebuild, and
// self-escalation dispatch when runner signals an EscalationReason.
// da is optional (may be nil); when non-nil, tool-agent dispatching is wired
// into local runners so they can call peer agents as tools.
func runPrimary(
	ctx context.Context,
	cfg config.Config,
	sess *session.Session,
	runner TurnRunner,
	escalationRunner TurnRunner, // used only when self-escalation fires; may be nil
	mem *memory.Store,
	prompt string,
	out io.Writer,
	da *dispatchAgents,
	onResponse func(string),
	onSegment func(string),
	prefixOut ...io.Writer,
) error {
	return runPrimaryWithSession(ctx, cfg, sess, runner, escalationRunner, mem, prompt, prompt, out, da, onResponse, onSegment, prefixOut...)
}

// runPrimaryWithSession is like runPrimary but accepts a separate sessionContent
// that is recorded in session history instead of the raw prompt. When
// attachments are present, sessionContent carries compact placeholders while
// prompt carries the full file content for inference.
func runPrimaryWithSession(
	ctx context.Context,
	cfg config.Config,
	sess *session.Session,
	runner TurnRunner,
	escalationRunner TurnRunner,
	mem *memory.Store,
	prompt string,
	sessionContent string,
	out io.Writer,
	da *dispatchAgents,
	onResponse func(string),
	onSegment func(string),
	prefixOut ...io.Writer,
) error {
	ac := cfg.ActiveAgent()
	agentName := runner.Name()

	pw := out
	if len(prefixOut) > 0 && prefixOut[0] != nil {
		pw = prefixOut[0]
	}
	fmt.Fprint(pw, bold(green(agentName+":"))+" ")
	aw := newActivityWriter(out)

	var ctxMode escalation.ContextMode
	if sess.PrimarySessionID != "" {
		ctxMode = escalation.ContextModeReturning
	} else {
		ctxMode = escalation.ContextModeFirst
	}

	logStateTransition(sess, session.StateLocal, "run "+agentName+" primary")
	sess.ForceState(session.StateLocal)

	if sess.PrimaryNonce == "" {
		sess.PrimaryNonce = claude.GenerateNonce()
	}
	nonce := sess.PrimaryNonce

	primaryName := ac.Name
	escalationName := cfg.EscalationAgentConfig().Name

	cbs := TurnCallbacks{
		OnNeed:            func(body string) { sess.RecordNeed(body) },
		OnPercept:         buildPerceptCallback(ctx, mem, primaryName, escalationName, false),
		OnEscalate:        func(reason string) {}, // captured via TurnResult.EscalationReason
		OnResponse:        onResponse,
		OnResponseSegment: onSegment,
	}

	// Wire tool-agent dispatcher into local runners when dispatchAgents is available.
	if da != nil {
		if lr, ok := runner.(*localRunner); ok {
			entries := cfg.EffectiveToolAgents(runner.Name())
			lr.agent = lr.agent.WithToolAgentEntries(entries)
			capturedDA := da
			lr.agent.SetToolAgentDispatcher(func(dctx context.Context, agentName, request string, dout io.Writer) (string, error) {
				tr, err := getOrBuildToolRunner(dctx, agentName, cfg, capturedDA)
				if err != nil {
					return "", err
				}
				return tr.RunToolCall(dctx, cfg, request, dout)
			})
		}
	}

	res, err := executeWithRetry(ctx, runner, cfg, sess, mem, RolePrimary, ctxMode,
		sess.PrimarySessionID, nonce,
		perceptsForAgent(cfg, mem, prompt, false), true,
		prompt, cbs, aw)
	aw.Done()
	if err != nil {
		return err
	}

	sess.AddTurn(session.Turn{Role: session.RoleUser, Agent: session.AgentLocal, AgentName: agentName, Content: sessionContent})

	if res.NewSessionID != "" {
		sess.PrimarySessionID = res.NewSessionID
	}

	model := ac.Model
	if model == "" {
		model = ac.Name
	}
	obs.RecordTokens(ctx, model, "primary", res.InputTokens, res.OutputTokens)
	// AddTokensFull's signature is (prompt, completion, cacheRead, cacheCreation);
	// this call previously passed CacheCreate/CacheRead swapped, predating the
	// prompt-caching feature — fixed here since this sprint already touches this
	// exact call site to verify cache-token flow end-to-end.
	sess.AddTokensFull(model, "primary", res.InputTokens, res.OutputTokens, res.CacheRead, res.CacheCreate)
	obs.Debug("tokens ("+agentName+")", "input", res.InputTokens, "output", res.OutputTokens, "cost_usd", res.CostUSD)

	// For local HTTP runners, text is only set when a real response came back.
	// Only add the assistant turn when there is content: skip both self-escalation
	// (res.EscalationReason != "") and truly empty responses (res.Text == "").
	// The previous condition `res.Text != "" || res.EscalationReason == ""` was
	// incorrect — it evaluated to true when both were empty (false || true), causing
	// a blank assistant turn to be written to session history.
	if res.Text != "" {
		sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentLocal, AgentName: agentName, Content: res.Text})
		sess.RebuildSummaryBricks(cfg.AgentContextBudget(ac))
	}
	if res.Text != "" && cbs.OnResponse != nil {
		cbs.OnResponse(res.Text)
	}

	if res.EscalationReason != "" {
		fmt.Fprintf(out, "\n%s %s requested escalation: %s\n", milkTag(), agentName, res.EscalationReason)
		if sess.CurrentNeed == "" {
			sess.RecordNeed(prompt)
		}
		sess.RebuildSummaryBricks(cfg.AgentContextBudget(cfg.EscalationAgentConfig()))
		logStateTransition(sess, session.StateRouting, agentName+" self-escalation")
		sess.ForceState(session.StateRouting)
		session.Save(sess) //nolint:errcheck

		if escalationRunner != nil {
			return runEscalation(ctx, cfg, sess, escalationRunner, res.EscalationReason, mem, prompt, out, onResponse, onSegment)
		}
		// Fallback: build CLI escalation runner on-demand.
		cliEsc := buildFallbackCLIRunner(cfg)
		return runEscalation(ctx, cfg, sess, cliEsc, res.EscalationReason, mem, prompt, out, onResponse, onSegment)
	}

	logStateTransition(sess, session.StateRouting, agentName+" primary done")
	sess.ForceState(session.StateRouting)
	return session.Save(sess)
}

// runEscalation executes one escalation-agent turn.
// Wrapper around runEscalationWithSession where sessionContent == prompt.
func runEscalation(
	ctx context.Context,
	cfg config.Config,
	sess *session.Session,
	runner TurnRunner,
	brief string,
	mem *memory.Store,
	prompt string,
	out io.Writer,
	onResponse func(string),
	onSegment func(string),
	prefixOut ...io.Writer,
) error {
	return runEscalationWithSession(ctx, cfg, sess, runner, brief, mem, prompt, prompt, "", out, onResponse, onSegment, prefixOut...)
}

// runEscalationWithSession executes one escalation-agent turn using runner.
// sessionContent is the compact version stored in session history (may include
// attachment placeholders instead of raw file data). prompt is the full content
// sent to the agent.
func runEscalationWithSession(
	ctx context.Context,
	cfg config.Config,
	sess *session.Session,
	runner TurnRunner,
	brief string,
	mem *memory.Store,
	prompt string,
	sessionContent string,
	imageContextFile string,
	out io.Writer,
	onResponse func(string),
	onSegment func(string),
	prefixOut ...io.Writer,
) error {
	escAC := cfg.EscalationAgentConfig()
	agentName := runner.Name()

	pw := out
	if len(prefixOut) > 0 && prefixOut[0] != nil {
		pw = prefixOut[0]
	}
	fmt.Fprint(pw, bold(blue(agentName+":"))+" ")
	aw := newActivityWriter(out)

	forceFresh := sess.ForceFreshEscalation
	if forceFresh {
		sess.ForceFreshEscalation = false
		sess.EscalationSessionID = ""
		sess.EscalationNonce = ""
		sess.MemoryInstructionInjectedAt = 0
		sess.CurrentNeed = ""
		sess.CurrentNeedSetAt = 0
		obs.Debug(agentName + " force-fresh-escalation: context reset")
	}

	var ctxMode escalation.ContextMode
	switch {
	case forceFresh:
		// /escalate fresh: always start a new session regardless of history.
		ctxMode = escalation.ContextModeFirst
	case sess.State == session.StateEscalationWaiting && sess.EscalationSessionID != "":
		ctxMode = escalation.ContextModeResume
	case sess.EscalationSessionID != "" && sess.LocalTurnsSinceLastEscalation() == 0:
		// Sticky follow-up: escalation agent is already active and the user typed
		// the next message directly — no local turns have run since last escalation.
		ctxMode = escalation.ContextModeContinuation
	case sess.EscalationSessionID != "" || session.EscalationEverActive(sess):
		// EscalationSessionID is set for CLI/subprocess agents; for local-HTTP agents
		// there is no session ID, so fall back to history scan to detect returning turns.
		ctxMode = escalation.ContextModeReturning
	default:
		ctxMode = escalation.ContextModeFirst
	}

	if ctxMode == escalation.ContextModeReturning {
		freshThreshold := cfg.AgentReturningFreshStartLocalTurns(escAC)
		needStale := sess.NeedChangedSinceLastEscalation()
		turnGap := freshThreshold > 0 && sess.LocalTurnsSinceLastEscalation() >= freshThreshold
		if needStale || turnGap {
			ctxMode = escalation.ContextModeFirst
			obs.Debug(agentName+" returning-fresh-start", "reason_need_stale", needStale, "reason_turn_gap", turnGap)
		}
	}

	resuming := ctxMode == escalation.ContextModeResume || ctxMode == escalation.ContextModeContinuation
	if ctxMode == escalation.ContextModeFirst {
		sess.EscalationBrief = brief
	}

	logStateTransition(sess, session.StateEscalation, "run "+agentName+" escalation")
	sess.ForceState(session.StateEscalation)

	if sess.EscalationNonce == "" {
		sess.EscalationNonce = claude.GenerateNonce()
	}
	nonce := sess.EscalationNonce

	primaryName := cfg.ActiveAgent().Name
	escalationName := escAC.Name

	injectInstructions := shouldInjectMemoryInstructions(cfg, sess, resuming)
	if injectInstructions {
		sess.MemoryInstructionInjectedAt = sess.EscalationTurnCount()
	}

	cbs := TurnCallbacks{
		OnNeed:            func(body string) { sess.RecordNeed(body) },
		OnPercept:         buildPerceptCallback(ctx, mem, primaryName, escalationName, true),
		OnResponse:        onResponse,
		OnResponseSegment: onSegment,
		ImageContextFile:  imageContextFile,
	}

	res, err := executeWithRetry(ctx, runner, cfg, sess, mem, RoleEscalation, ctxMode,
		sess.EscalationSessionID, nonce,
		perceptsForAgent(cfg, mem, prompt, true), injectInstructions,
		prompt, cbs, aw)
	aw.Done()
	if err != nil {
		return err
	}

	sess.AddTurn(session.Turn{Role: session.RoleUser, Agent: session.AgentEscalation, AgentName: agentName, Content: sessionContent})

	if res.NewSessionID != "" {
		sess.EscalationSessionID = res.NewSessionID
	}

	model := escAC.Model
	if model == "" {
		model = escAC.Name
	}
	obs.RecordTokens(ctx, model, "escalation", res.InputTokens, res.OutputTokens)
	obs.AccumulateCacheTokens(model, "escalation", res.CacheRead, res.CacheCreate)
	sess.AddTokensFull(model, "escalation", res.InputTokens, res.OutputTokens, res.CacheRead, res.CacheCreate)
	obs.Debug("tokens ("+agentName+")", "input", res.InputTokens, "output", res.OutputTokens,
		"cache_read", res.CacheRead, "cache_write", res.CacheCreate, "cost_usd", res.CostUSD)

	// Record subagent tokens when present.
	if res.HasSubagentTokens {
		obs.RecordTokens(ctx, model, "escalation:subagent", res.SubagentInputTokens, res.SubagentOutputTokens)
		obs.AccumulateCacheTokens(model, "escalation:subagent", res.SubagentCacheRead, res.SubagentCacheCreate)
		sess.AddTokensFull(model, "escalation:subagent", res.SubagentInputTokens, res.SubagentOutputTokens, res.SubagentCacheRead, res.SubagentCacheCreate)
		obs.Debug("tokens ("+agentName+" subagent)", "input", res.SubagentInputTokens, "output", res.SubagentOutputTokens,
			"cache_read", res.SubagentCacheRead, "cache_write", res.SubagentCacheCreate)
	}
	// Record workflow tokens when present.
	if res.HasWorkflowTokens {
		obs.RecordTokens(ctx, model, "escalation:workflow", res.WorkflowInputTokens, res.WorkflowOutputTokens)
		obs.AccumulateCacheTokens(model, "escalation:workflow", res.WorkflowCacheRead, res.WorkflowCacheCreate)
		sess.AddTokensFull(model, "escalation:workflow", res.WorkflowInputTokens, res.WorkflowOutputTokens, res.WorkflowCacheRead, res.WorkflowCacheCreate)
		obs.Debug("tokens ("+agentName+" workflow)", "input", res.WorkflowInputTokens, "output", res.WorkflowOutputTokens,
			"cache_read", res.WorkflowCacheRead, "cache_write", res.WorkflowCacheCreate)
	}

	// Only persist the assistant turn when there is actual content.
	// A local-HTTP escalation agent with an empty SSE stream returns res.Text == ""
	// with no error; writing a blank turn to history corrupts the session context.
	// For CLI agents: if Claude finished with only tool calls and no closing text,
	// res.Text is also empty — skipping the AddTurn is safe because the escalation
	// agent manages its own conversation state via --resume; milk's local history
	// copy is only used for context handoff back to the primary, where a blank entry
	// is more harmful than a missing one.
	if res.Text != "" {
		sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentEscalation, AgentName: agentName, Content: res.Text})
		sess.RebuildSummaryBricks(cfg.AgentContextBudget(escAC))
		if cbs.OnResponse != nil {
			cbs.OnResponse(res.Text)
		}
	}

	if res.EndsWithQ {
		logStateTransition(sess, session.StateEscalationWaiting, agentName+" escalation ended with question")
		sess.ForceState(session.StateEscalationWaiting)
	} else {
		sess.EscalationBrief = ""
		logStateTransition(sess, session.StateRouting, agentName+" escalation done")
		sess.ForceState(session.StateRouting)
	}
	return session.Save(sess)
}

// buildPerceptCallback returns the OnPercept callback for a turn.
// isEscalation controls which memory producer/consumer assignments are used.
func buildPerceptCallback(
	ctx context.Context,
	mem *memory.Store,
	primaryName, escalationName string,
	isEscalation bool,
) func(body, consumerHint string) {
	if mem == nil {
		return nil
	}
	return func(body, consumerHint string) {
		var consumer memory.Consumer
		if isEscalation {
			switch consumerHint {
			case primaryName:
				consumer = memory.ConsumerLocal
			case escalationName:
				consumer = memory.ConsumerEscalation
			default:
				consumer = memory.ConsumerAll
			}
			_, _ = mem.Record(ctx, body, memory.ProducerEscalation, consumer, memory.Roles{}, false)
		} else {
			switch consumerHint {
			case escalationName:
				consumer = memory.ConsumerEscalation
			case primaryName:
				consumer = memory.ConsumerLocal
			default:
				consumer = memory.ConsumerAll
			}
			_, _ = mem.Record(ctx, body, memory.ProducerLocal, consumer, memory.Roles{}, false)
		}
	}
}

// buildFallbackCLIRunner constructs a cliRunner for the Claude CLI escalation
// agent, used when runPrimary needs to self-escalate but no escalation runner
// was provided (single-shot mode).
func buildFallbackCLIRunner(cfg config.Config) *cliRunner {
	ac := cliAgentConfig(cfg)
	agent := newCLIAgent(ac)
	agent = applyAWSCreds(cfg, agent)
	var cs *claudesettings.Store
	var cwd string
	if d, err := os.Getwd(); err == nil {
		cwd = d
		cs, _ = claudesettings.Open(cwd)
	}
	name := ac.Name
	if name == "" {
		name = "claude"
	}
	return newCLIRunner(agent, name, permContext{cs: cs, cwd: cwd}, func() inputReader { return newStdinInputReader() })
}
