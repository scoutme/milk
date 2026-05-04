package router

import (
	"context"
	"fmt"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/session"
)

type Target string

const (
	TargetLocal  Target = "local"
	TargetClaude Target = "claude"
)

type Decision struct {
	Target     Target
	Reason     string
	Conclusive bool
}

// Router decides which agent should handle a prompt for a given session turn.
type Router struct {
	cfg        config.Config
	localAgent *local.Agent
}

func New(cfg config.Config, localAgent *local.Agent) *Router {
	return &Router{cfg: cfg, localAgent: localAgent}
}

// Route returns the target agent for the given prompt and session state.
// explicitEscalate and explicitLocal reflect the --escalate / --local flags.
func (r *Router) Route(ctx context.Context, sess *session.Session, prompt string, explicitEscalate, explicitLocal bool) (Decision, error) {
	// 1. Explicit flags always win
	if explicitEscalate {
		return Decision{Target: TargetClaude, Conclusive: true, Reason: "--escalate flag"}, nil
	}
	if explicitLocal {
		return Decision{Target: TargetLocal, Conclusive: true, Reason: "--local flag"}, nil
	}

	// 2. Session state: CLAUDE_WAITING means the conversation is mid-flight with Claude
	if sess.State == session.StateClaudeWaiting {
		return Decision{Target: TargetClaude, Conclusive: true, Reason: "session state CLAUDE_WAITING"}, nil
	}

	// 3. Rules layer
	if d := rulesDecision(prompt, r.cfg); d.Conclusive {
		return d, nil
	}

	// 4. Local model classifier (best-effort; fall back to local on error)
	if r.localAgent != nil {
		escalate, err := r.localAgent.Classify(ctx, prompt)
		if err != nil {
			// classifier unavailable — log and default to local
			fmt.Printf("[router] classifier error: %v — defaulting to local\n", err)
		} else if escalate {
			return Decision{Target: TargetClaude, Conclusive: true, Reason: "model classifier"}, nil
		}
	}

	// 5. Default: local
	return Decision{Target: TargetLocal, Conclusive: true, Reason: "default"}, nil
}
