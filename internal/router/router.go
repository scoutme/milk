package router

import (
	"context"
	"fmt"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/session"
)

type Target string

const (
	TargetLocal      Target = "local"
	TargetEscalation Target = "escalation"
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
	d, err := r.route(ctx, sess, prompt, explicitEscalate, explicitLocal)
	obs.Debug("router", "target", d.Target, "reason", d.Reason, "conclusive", d.Conclusive)
	return d, err
}

func (r *Router) route(ctx context.Context, sess *session.Session, prompt string, explicitEscalate, explicitLocal bool) (Decision, error) {
	// 1. Explicit flags always win
	if explicitEscalate {
		return Decision{Target: TargetEscalation, Conclusive: true, Reason: "--escalate flag"}, nil
	}
	if explicitLocal {
		return Decision{Target: TargetLocal, Conclusive: true, Reason: "--local flag"}, nil
	}

	// 2. Session state: ESCALATION_WAITING means the conversation is mid-flight with the escalation agent
	if sess.State == session.StateEscalationWaiting {
		return Decision{Target: TargetEscalation, Conclusive: true, Reason: "session state ESCALATION_WAITING"}, nil
	}

	// 3. Rules layer
	if d := rulesDecision(prompt, r.cfg); d.Conclusive {
		return d, nil
	}

	// 4. Configurable fallback: local LLM classifier or direct escalation
	switch r.cfg.Rules.ClassifierFallback {
	case "claude", "escalation":
		return Decision{Target: TargetEscalation, Conclusive: true, Reason: "classifier-fallback=escalation"}, nil
	default: // "local" or unset
		if r.localAgent != nil {
			escalate, err := r.localAgent.Classify(ctx, prompt)
			if err != nil {
				fmt.Printf("[router] classifier error: %v — defaulting to local\n", err)
			} else if escalate {
				return Decision{Target: TargetEscalation, Conclusive: true, Reason: "model classifier"}, nil
			}
		}
	}

	// 5. Default: local
	return Decision{Target: TargetLocal, Conclusive: true, Reason: "default"}, nil
}
