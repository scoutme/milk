package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/scoutme/milk/internal/agent/claude"
	"github.com/scoutme/milk/internal/claudesettings"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/session"
)

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

	sess.AddTurn(session.Turn{Role: session.RoleUser, Agent: session.AgentLocal, Content: prompt})
	logStateTransition(sess, session.StateLocal, "run "+agentName+" primary")
	sess.ForceState(session.StateLocal)

	if sess.PrimaryNonce == "" {
		sess.PrimaryNonce = claude.GenerateNonce()
	}
	nonce := sess.PrimaryNonce

	primaryName := ac.Name
	escalationName := cfg.EscalationAgentConfig().Name

	cbs := TurnCallbacks{
		OnNeed:     func(body string) { sess.RecordNeed(body) },
		OnPercept:  buildPerceptCallback(ctx, mem, primaryName, escalationName, false),
		OnEscalate: func(reason string) {}, // captured via TurnResult.EscalationReason
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

	res, err := runner.Execute(ctx, cfg, sess, mem, RolePrimary, ctxMode,
		sess.PrimarySessionID, nonce,
		perceptsForEscalation(cfg, mem, prompt), true,
		prompt, cbs, aw)
	aw.Done()
	if err != nil {
		return err
	}

	if res.NewSessionID != "" {
		sess.PrimarySessionID = res.NewSessionID
	}

	model := ac.Model
	if model == "" {
		model = ac.Name
	}
	obs.RecordTokens(ctx, model, "primary", res.InputTokens, res.OutputTokens)
	sess.AddTokensFull(model, "primary", res.InputTokens, res.OutputTokens, res.CacheCreate, res.CacheRead)
	obs.Debug("tokens ("+agentName+")", "input", res.InputTokens, "output", res.OutputTokens, "cost_usd", res.CostUSD)

	// For local HTTP runners, text is only set when a real response came back.
	// Only add the assistant turn when there is content (mirrors runLocal behaviour).
	if res.Text != "" || res.EscalationReason == "" {
		sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentLocal, Content: res.Text})
		sess.RebuildSummaryBricks(cfg.AgentContextBudget(ac))
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
			return runEscalation(ctx, cfg, sess, escalationRunner, res.EscalationReason, mem, prompt, out)
		}
		// Fallback: build CLI escalation runner on-demand.
		cliEsc := buildFallbackCLIRunner(cfg)
		return runEscalation(ctx, cfg, sess, cliEsc, res.EscalationReason, mem, prompt, out)
	}

	logStateTransition(sess, session.StateRouting, agentName+" primary done")
	sess.ForceState(session.StateRouting)
	return session.Save(sess)
}

// runEscalation executes one escalation-agent turn using runner.
// It handles context-mode resolution (First/Returning/Resume), fresh-start check,
// state transitions, nonce management, turn recording, token accounting, and
// summary rebuild.
func runEscalation(
	ctx context.Context,
	cfg config.Config,
	sess *session.Session,
	runner TurnRunner,
	brief string,
	mem *memory.Store,
	prompt string,
	out io.Writer,
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

	resuming := ctxMode == escalation.ContextModeResume
	if ctxMode == escalation.ContextModeFirst {
		sess.EscalationBrief = brief
	}

	sess.AddTurn(session.Turn{Role: session.RoleUser, Agent: session.AgentEscalation, Content: prompt})
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
		OnNeed:    func(body string) { sess.RecordNeed(body) },
		OnPercept: buildPerceptCallback(ctx, mem, primaryName, escalationName, true),
	}

	res, err := runner.Execute(ctx, cfg, sess, mem, RoleEscalation, ctxMode,
		sess.EscalationSessionID, nonce,
		perceptsForEscalation(cfg, mem, prompt), injectInstructions,
		prompt, cbs, aw)
	aw.Done()
	if err != nil {
		return err
	}

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

	sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentEscalation, Content: res.Text})
	sess.RebuildSummaryBricks(cfg.AgentContextBudget(escAC))

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
	if cwd, err := os.Getwd(); err == nil {
		cs, _ = claudesettings.Open(cwd)
	}
	name := ac.Name
	if name == "" {
		name = "claude"
	}
	return newCLIRunner(agent, name, permContext{cs: cs}, func() inputReader { return newStdinInputReader() })
}
