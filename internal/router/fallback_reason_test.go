package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
)

// classifyServer serves pi's/classify's chat-completions shape with a fixed
// classifier answer, or a body that fails to decode when answer is "".
func classifyServer(t *testing.T, answer string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if answer == "" {
			w.Write([]byte("not json"))
			return
		}
		// The decode tag is case-sensitive on "Message" (local.Classify).
		w.Write([]byte(`{"choices":[{"Message":{"content":"` + answer + `"}}]}`))
	}))
}

func localClassifier(t *testing.T, answer string) *Router {
	t.Helper()
	srv := classifyServer(t, answer)
	t.Cleanup(srv.Close)
	return New(config.Config{Rules: config.Rules{ClassifierFallback: "local"}},
		local.NewFromConfig(config.AgentConfig{Name: "t", Model: "m", Provider: "local", URL: srv.URL}))
}

// The route-reason probe (2026-11-25) hit the ambiguity this pins down: a
// classifier verdict of "primary" and a classifier failure both surfaced as
// the bare reason "default", indistinguishable in results.json's
// route_reasons. Each path must now name itself honestly.
func TestFallbackDecision_ReasonVocabulary(t *testing.T) {
	t.Run("classifier says primary", func(t *testing.T) {
		d := localClassifier(t, "primary").fallbackDecision(context.Background(), "task")
		if d.Target != TargetLocal {
			t.Errorf("target = %q, want local", d.Target)
		}
		if d.Reason != "classifier: primary" {
			t.Errorf("reason = %q, want %q", d.Reason, "classifier: primary")
		}
	})

	t.Run("classifier says escalate", func(t *testing.T) {
		d := localClassifier(t, "escalate").fallbackDecision(context.Background(), "task")
		if d.Target != TargetEscalation {
			t.Errorf("target = %q, want escalation", d.Target)
		}
		if d.Reason != "model classifier" {
			t.Errorf("reason = %q, want %q", d.Reason, "model classifier")
		}
	})

	t.Run("classifier errors", func(t *testing.T) {
		d := localClassifier(t, "").fallbackDecision(context.Background(), "task")
		if d.Target != TargetLocal {
			t.Errorf("target = %q, want local", d.Target)
		}
		if d.Reason != "classifier error — defaulted local" {
			t.Errorf("reason = %q, want %q", d.Reason, "classifier error — defaulted local")
		}
	})

	t.Run("no classifier wired", func(t *testing.T) {
		r := New(config.Config{Rules: config.Rules{ClassifierFallback: "local"}}, nil)
		d := r.fallbackDecision(context.Background(), "task")
		if d.Reason != "default" {
			t.Errorf("reason = %q, want %q", d.Reason, "default")
		}
	})

	t.Run("fallback escalates without classifier", func(t *testing.T) {
		r := New(config.Config{Rules: config.Rules{ClassifierFallback: "escalation"}}, nil)
		d := r.fallbackDecision(context.Background(), "task")
		if d.Target != TargetEscalation || d.Reason != "classifier-fallback=escalation" {
			t.Errorf("got %q/%q, want escalation/%q", d.Target, d.Reason, "classifier-fallback=escalation")
		}
	})
}

// routerRule feeds milk.router.decisions: a classifier verdict is a
// classifier decision; a classifier failure that defaulted is not — it
// belongs with the no-decision "default" label.
func TestRouterRule_ClassifierReasonLabels(t *testing.T) {
	for reason, want := range map[string]string{
		"model classifier":                   "classifier",
		"classifier: primary":                "classifier",
		"classifier error — defaulted local": "default",
		"default":                            "default",
	} {
		if got := routerRule(reason); got != want {
			t.Errorf("routerRule(%q) = %q, want %q", reason, got, want)
		}
	}
}
