package router

import (
	"context"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/session"
)

const instrumentationScope = "github.com/scoutme/milk"

// routerRule maps a human-readable decision reason to a stable label for the
// milk.router.decisions metric. Labels match the spec: explicit, state,
// hard_threshold, soft_score, classifier, default.
func routerRule(reason string) string {
	switch {
	case strings.Contains(reason, "flag"):
		return "explicit"
	case strings.Contains(reason, "ESCALATION_WAITING"):
		return "state"
	case strings.Contains(reason, "token threshold"), strings.Contains(reason, "keyword"), strings.Contains(reason, "short prompt"):
		return "hard_threshold"
	case strings.Contains(reason, "signals"):
		return "soft_score"
	case strings.Contains(reason, "classifier"), strings.Contains(reason, "model classifier"):
		return "classifier"
	default:
		return "default"
	}
}

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
	if err == nil {
		rule := routerRule(d.Reason)
		obs.Inc(ctx, instrumentationScope, "milk.router.decisions",
			attribute.String("target", string(d.Target)),
			attribute.String("rule", rule),
		)
	}
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
	if d := rulesDecisionCtx(ctx, prompt, r.cfg); d.Conclusive {
		return d, nil
	}

	// 4. Configurable fallback: local LLM classifier or direct escalation
	switch r.cfg.Rules.ClassifierFallback {
	case "claude", "escalation":
		return Decision{Target: TargetEscalation, Conclusive: true, Reason: "classifier-fallback=escalation"}, nil
	default: // "local" or unset
		if r.localAgent != nil {
			classifyStart := time.Now()
			escalate, err := r.localAgent.Classify(ctx, prompt)
			classifyElapsed := time.Since(classifyStart)
			if err != nil {
				obs.Warn("router classifier error, defaulting to local", "err", err)
			} else {
				obs.RecordDuration(ctx, instrumentationScope, "milk.router.classify_latency_ms", classifyElapsed,
					attribute.String("model", r.cfg.ActiveAgent().Model),
				)
				if escalate {
					return Decision{Target: TargetEscalation, Conclusive: true, Reason: "model classifier"}, nil
				}
			}
		}
	}

	// 5. Default: local
	return Decision{Target: TargetLocal, Conclusive: true, Reason: "default"}, nil
}
