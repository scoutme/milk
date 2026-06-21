package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/scoutme/milk/internal/agent/claude"
	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/agent/subprocess"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/session"
)

// AgentRole distinguishes primary from escalation so runners can apply
// role-specific behaviour (e.g. history scoping, context builder choice).
type AgentRole int

const (
	RolePrimary    AgentRole = iota
	RoleEscalation AgentRole = iota
)

// TurnResult is the normalised output of a single agent inference turn.
type TurnResult struct {
	Text             string
	NewSessionID     string // non-empty when runner opened a new session; caller must persist
	EscalationReason string // non-empty when subprocess primary requested hand-off
	EndsWithQ        bool
	InputTokens      int64
	OutputTokens     int64
	CacheRead        int64
	CacheCreate      int64
	CostUSD          float64
}

// TurnCallbacks carries the tag-intercept callbacks wired per-turn by the dispatcher.
type TurnCallbacks struct {
	OnNeed     func(body string)
	OnPercept  func(body, consumerHint string)
	OnEscalate func(reason string) // nil for escalation runners (they never self-escalate)
}

// TurnRunner abstracts provider-specific inference for one agent turn.
// It is responsible only for invoking the underlying agent and returning a
// TurnResult. All session bookkeeping (state transitions, turn recording,
// token accounting) is done by the dispatch layer (runPrimary / runEscalation).
type TurnRunner interface {
	// Name returns the display name used in logs and status lines.
	Name() string
	// Ping checks whether the underlying agent binary or endpoint is reachable.
	Ping() error
	// Execute runs one inference turn. sessionID is the existing session to
	// resume (empty → first turn). ctxMode, nonce, percepts, and injectInstructions
	// are pre-computed by the dispatcher. cbs carries tag callbacks wired for this turn.
	Execute(
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
	) (TurnResult, error)
}

// ── localRunner ──────────────────────────────────────────────────────────────

type localRunner struct {
	agent *local.Agent
	name  string
}

func newLocalRunner(agent *local.Agent, name string) *localRunner {
	return &localRunner{agent: agent, name: name}
}

func (r *localRunner) Name() string { return r.name }
func (r *localRunner) Ping() error  { return r.agent.Ping(context.Background()) }

func (r *localRunner) Execute(
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
	ac := agentConfigForRole(cfg, role)

	agent := r.agent.WithMemConfig(local.MemConfig{
		ResultMaxBytes:       cfg.AgentMemoryResultMaxByteCount(ac),
		ReinjectionTurns:     cfg.AgentMemoryReinjectionTurnThreshold(ac, role == RolePrimary),
		ReinjectionBytes:     cfg.AgentMemoryReinjectionByteThreshold(ac, role == RolePrimary),
		RelevanceGateEnabled: cfg.AgentPerceptRelevanceGateEnabled(ac),
		MaxToolIterations:    cfg.AgentMaxToolIterations(ac),
	})

	primaryName := cfg.ActiveAgent().Name
	escalationName := cfg.EscalationAgentConfig().Name

	agent = agent.WithTagCallbacks(nonce, primaryName, escalationName,
		cbs.OnNeed,
		func(body, hint string) {
			if cbs.OnPercept != nil {
				cbs.OnPercept(body, hint)
			}
		},
	)

	// Build history depending on role and context mode.
	var history []local.Message
	switch role {
	case RoleEscalation:
		// Inject orientation as a system message, build appropriately-scoped history.
		orientationText := escalation.BuildDynamicContext(sess, ctxMode)
		perceptsText := escalation.FormatPercepts(percepts)

		isFirst := !session.EscalationEverActive(sess) || ctxMode == escalation.ContextModeFirst
		if isFirst && session.EscalationEverActive(sess) {
			history = escalationLocalHistoryFresh(sess, prompt)
		} else {
			history = escalationLocalHistory(sess, prompt)
		}
		if perceptsText != "" {
			history = append([]local.Message{{Role: "system", Content: perceptsText}}, history...)
		}
		if orientationText != "" {
			history = append([]local.Message{{Role: "system", Content: orientationText}}, history...)
		}

		msgBudget := cfg.AgentMessageBudget(ac)
		if msgBudget > 0 {
			overhead := agent.SystemOverheadChars(sess) + len(orientationText)
			msgBudget = max(1, msgBudget-overhead)
		}
		if trimmed, ok := trimLocalMessages(history, msgBudget); ok {
			history = trimmed
		}

	default: // RolePrimary
		history = sessionToMessages(sess)

		msgBudget := cfg.AgentMessageBudget(ac)
		if msgBudget > 0 {
			overhead := agent.SystemOverheadChars(sess)
			msgBudget = max(1, msgBudget-overhead)
		}
		if trimmed, ok := trimLocalMessages(history, msgBudget); ok {
			history = trimmed
		}
	}

	updatedHistory, err := agent.Run(ctx, history, prompt, out, sess, mem)
	if err != nil {
		if esc, ok := err.(*local.EscalationSignal); ok {
			return TurnResult{EscalationReason: esc.Reason}, nil
		}
		return TurnResult{}, err
	}

	text := ""
	if len(updatedHistory) > 0 {
		last := updatedHistory[len(updatedHistory)-1]
		if last.Role == "assistant" {
			text = last.Content
		}
	}
	return TurnResult{Text: text}, nil
}

// ── cliRunner ─────────────────────────────────────────────────────────────────

type cliRunner struct {
	agent    *claude.Agent
	name     string
	pc       permContext
	newInput func() inputReader // produces an inputReader for permission prompts
}

func newCLIRunner(agent *claude.Agent, name string, pc permContext, newInput func() inputReader) *cliRunner {
	return &cliRunner{agent: agent, name: name, pc: pc, newInput: newInput}
}

func (r *cliRunner) Name() string { return r.name }
func (r *cliRunner) Ping() error  { return r.agent.Ping() }

func (r *cliRunner) Execute(
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
	primaryName := cfg.ActiveAgent().Name
	escalationName := cfg.EscalationAgentConfig().Name

	agent := applyPersistedGrants(r.agent, r.pc)
	if r.pc.cs != nil && r.pc.toolFutures == nil {
		agent = agent.WithPermissionHandler(makePermissionHandler(r.newInput(), out, r.pc.cs))
	}
	if mem != nil && cbs.OnPercept != nil {
		agent = agent.WithOnPercept(cbs.OnPercept, nonce, primaryName, escalationName)
	}
	if cbs.OnNeed != nil {
		agent = agent.WithOnNeed(cbs.OnNeed, nonce)
	}

	staticCtx := escalation.BuildStaticContext(nonce, percepts, ctxMode, injectInstructions, primaryName, escalationName)
	dynamicCtx := escalation.BuildDynamicContext(sess, ctxMode)

	// Suppress duplicate dynamic context on resume turns (cache preservation).
	// Re-sending an identical file still shifts the cache suffix and causes a miss.
	if r.pc.contextHash != nil {
		h := fmt.Sprintf("%x", sha256.Sum256([]byte(dynamicCtx)))[:16]
		if ctxMode == escalation.ContextModeResume && h == *r.pc.contextHash {
			dynamicCtx = ""
		} else {
			*r.pc.contextHash = h
		}
	}

	// Buffer the first run's streaming output so we can discard it when
	// AskUserQuestion is in the denials — the question text and Claude's
	// "stream closed" fallback should not appear in the transcript. We only
	// flush the buffer to the real out when there are no AskUserQuestion denials.
	firstBuf := &bytes.Buffer{}
	var (
		res    claude.ParseResult
		runErr error
	)
	if ctxMode == escalation.ContextModeResume || (ctxMode == escalation.ContextModeReturning && sessionID != "") {
		res, runErr = agent.RunResume(ctx, sessionID, staticCtx, dynamicCtx, prompt, firstBuf)
		if runErr != nil && claude.IsInvalidSession(runErr) {
			// Stale session ID — Claude's store no longer has this session (evicted,
			// CLI upgrade, machine restart, etc.). Discard the buffer, notify the
			// user inline, and fall back to a fresh RunFirst with full context.
			firstBuf.Reset()
			fmt.Fprintf(out, "\n\033[2m[Claude session refreshed — previous session no longer available]\033[0m\n\n")
			staticCtx = escalation.BuildStaticContext(nonce, percepts, escalation.ContextModeFirst, injectInstructions, primaryName, escalationName)
			dynamicCtx = escalation.BuildDynamicContext(sess, escalation.ContextModeFirst)
			var newID string
			newID, res, runErr = agent.RunFirst(ctx, staticCtx, dynamicCtx, prompt, firstBuf)
			if runErr == nil {
				sessionID = newID
			}
		}
	} else {
		var newID string
		newID, res, runErr = agent.RunFirst(ctx, staticCtx, dynamicCtx, prompt, firstBuf)
		if runErr == nil {
			sessionID = newID
		}
	}
	if runErr != nil {
		return TurnResult{}, runErr
	}

	hasAskDenial := false
	for _, d := range res.PermissionDenials {
		if d.ToolName == "AskUserQuestion" {
			hasAskDenial = true
			break
		}
	}
	if !hasAskDenial {
		io.Copy(out, firstBuf) //nolint:errcheck
	}

	// Permission-denial retry (Claude CLI only). handlePermissionDenials reads
	// sess.EscalationSessionID to call RunResume; set it to the live session ID
	// so it works whether this is a first turn (new ID) or a resume.
	if sessionID != "" {
		orig := sess.EscalationSessionID
		sess.EscalationSessionID = sessionID
		res = handlePermissionDenials(ctx, sess, agent, res, r.newInput(), out, r.pc, nonce, primaryName, escalationName)
		sess.EscalationSessionID = orig
	}

	return TurnResult{
		Text:         res.Text,
		NewSessionID: sessionID,
		EndsWithQ:    res.EndsWithQ,
		InputTokens:  res.InputTokens,
		OutputTokens: res.OutputTokens,
		CacheRead:    res.CacheReadInputTokens,
		CacheCreate:  res.CacheCreationInputTokens,
		CostUSD:      res.TotalCostUSD,
	}, nil
}

// ── subprocessRunner ─────────────────────────────────────────────────────────

type subprocessRunner struct {
	agent *subprocess.Agent
	name  string
}

func newSubprocessRunner(agent *subprocess.Agent, name string) *subprocessRunner {
	return &subprocessRunner{agent: agent, name: name}
}

func (r *subprocessRunner) Name() string { return r.name }
func (r *subprocessRunner) Ping() error  { return r.agent.Ping() }

func (r *subprocessRunner) Execute(
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
	primaryName := cfg.ActiveAgent().Name
	escalationName := cfg.EscalationAgentConfig().Name

	agent := r.agent
	if mem != nil && cbs.OnPercept != nil {
		agent = agent.WithOnPercept(cbs.OnPercept, nonce, primaryName, escalationName)
	}
	if cbs.OnNeed != nil {
		agent = agent.WithOnNeed(cbs.OnNeed, nonce)
	}

	var escalationReason string
	if cbs.OnEscalate != nil {
		agent = agent.WithOnEscalate(func(reason string) {
			escalationReason = reason
			cbs.OnEscalate(reason)
		}, nonce)
	}

	var staticCtx string
	if role == RolePrimary {
		staticCtx = escalation.BuildPrimaryStaticContext(nonce, percepts, ctxMode, injectInstructions, primaryName, escalationName)
	} else {
		staticCtx = escalation.BuildStaticContext(nonce, percepts, ctxMode, injectInstructions, primaryName, escalationName)
	}
	dynamicCtx := escalation.BuildPrimaryDynamicContext(sess, ctxMode)
	if role == RoleEscalation {
		dynamicCtx = escalation.BuildDynamicContext(sess, ctxMode)
	}

	var (
		res    subprocess.ParseResult
		runErr error
	)
	if ctxMode == escalation.ContextModeResume || (ctxMode == escalation.ContextModeReturning && sessionID != "") {
		res, runErr = agent.RunResume(ctx, sessionID, staticCtx, dynamicCtx, prompt, out)
	} else {
		var newID string
		newID, res, runErr = agent.RunFirst(ctx, staticCtx, dynamicCtx, prompt, out)
		if runErr == nil {
			sessionID = newID
		}
	}
	if runErr != nil {
		return TurnResult{}, runErr
	}

	return TurnResult{
		Text:             res.Text,
		NewSessionID:     sessionID,
		EscalationReason: escalationReason,
		EndsWithQ:        res.EndsWithQ,
		InputTokens:      res.InputTokens,
		OutputTokens:     res.OutputTokens,
		CacheRead:        res.CacheReadInputTokens,
		CacheCreate:      res.CacheCreationInputTokens,
		CostUSD:          res.TotalCostUSD,
	}, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func agentConfigForRole(cfg config.Config, role AgentRole) config.AgentConfig {
	if role == RolePrimary {
		return cfg.ActiveAgent()
	}
	return cfg.EscalationAgentConfig()
}
