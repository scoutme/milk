package escalation

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

// TestBuildStaticContext_OwnerPosture: the hand-off framing sets an owner
// posture ("continue as the owner") instead of priming a reviewer to expect
// being handed someone else's work.
func TestBuildStaticContext_OwnerPosture(t *testing.T) {
	got := BuildStaticContext("n1", nil, ContextModeFirst, true, "mimo", "claude")
	if !strings.Contains(got, "continue as the owner") {
		t.Errorf("identity block must state the owner posture, got %q", got)
	}
	if !strings.Contains(got, "don't recap unless asked") {
		t.Errorf("identity block must state the no-recap expectation, got %q", got)
	}
	if strings.Contains(got, "Expect mid-conversation hand-offs") {
		t.Errorf("takeover framing must be gone, got %q", got)
	}
}

// TestBuildContext_EscalationBrief_Label: the brief is attributed without the
// reviewer-priming "from primary agent" takeover framing.
func TestBuildContext_EscalationBrief_Label(t *testing.T) {
	sess := &session.Session{EscalationBrief: "user wants a parity fix"}
	got := BuildDynamicContext(sess, ContextModeFirst, nil)
	if !strings.Contains(got, "[Escalation brief]") {
		t.Errorf("expected neutral [Escalation brief] label, got %q", got)
	}
	if strings.Contains(got, "[Escalation brief from primary agent]") {
		t.Errorf("takeover label must be gone, got %q", got)
	}
	if !strings.Contains(got, "user wants a parity fix") {
		t.Errorf("brief content must survive, got %q", got)
	}
}
