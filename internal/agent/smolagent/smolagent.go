// Package smolagent provides a milk agent backed by the milk-smolagent Python script,
// which wraps HuggingFace smolagents. It implements the milk subprocess protocol
// (NDJSON over stdout) via the subprocess.Runner.
package smolagent

import (
	"context"
	"io"

	"github.com/scoutme/milk/internal/agent/subprocess"
	"github.com/scoutme/milk/internal/config"
)

// Agent is a subprocess-backed agent that runs milk-smolagent per turn.
// All fields are immutable after construction; With* methods return copies.
type Agent struct {
	runner  *subprocess.Runner
	builder *argBuilder
	// callbacks
	onPercept    func(content, consumerHint string)
	perceptNonce string
	agentNames   []string
	onNeed       func(content string)
	needNonce    string
}

// New constructs an Agent from the given AgentConfig.
func New(ac config.AgentConfig) *Agent {
	b := newArgBuilder(ac)
	p := &Parser{}
	return &Agent{
		runner:  subprocess.New(b, p),
		builder: b,
	}
}

// Ping checks whether milk-smolagent is available.
func (a *Agent) Ping() error { return a.runner.Ping() }

// WithDebugLog returns a copy of the agent that writes every raw stdout line to w.
func (a *Agent) WithDebugLog(w io.Writer) *Agent {
	c := *a
	c.runner = a.runner.WithDebugLog(w)
	return &c
}

// WithExtraEnv returns a copy of the agent with extra KEY=VALUE env pairs.
func (a *Agent) WithExtraEnv(pairs ...string) *Agent {
	c := *a
	c.runner = a.runner.WithExtraEnv(pairs...)
	return &c
}

// WithOnPercept wires a percept callback. nonce and agentNames must match the
// values passed to escalation.MemoryInstruction.
func (a *Agent) WithOnPercept(fn func(content, consumerHint string), nonce, primaryName, escalationName string) *Agent {
	c := *a
	c.onPercept = fn
	c.perceptNonce = nonce
	c.agentNames = []string{primaryName, escalationName}
	return &c
}

// WithOnNeed wires a need-tag callback.
func (a *Agent) WithOnNeed(fn func(content string), nonce string) *Agent {
	c := *a
	c.onNeed = fn
	c.needNonce = nonce
	return &c
}

// RunFirst runs the first turn of a new session and returns the session ID.
func (a *Agent) RunFirst(ctx context.Context, staticContext, dynamicContext, prompt string, out io.Writer) (string, subprocess.ParseResult, error) {
	return a.runner.RunFirst(ctx, staticContext, dynamicContext, prompt, a.parseOpts(), out)
}

// RunResume continues an existing session.
func (a *Agent) RunResume(ctx context.Context, sessionID, staticContext, dynamicContext, prompt string, out io.Writer) (subprocess.ParseResult, error) {
	return a.runner.RunResume(ctx, sessionID, staticContext, dynamicContext, prompt, a.parseOpts(), out)
}

func (a *Agent) parseOpts() subprocess.ParseOpts {
	return subprocess.ParseOpts{
		OnPercept:    a.onPercept,
		PerceptNonce: a.perceptNonce,
		AgentNames:   a.agentNames,
		OnNeed:       a.onNeed,
		NeedNonce:    a.needNonce,
	}
}
