package escalation

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

func TestBuildContext_EmptySession(t *testing.T) {
	sess := &session.Session{}
	got := BuildContext(sess, "testnonce", nil, false, true)
	if !strings.Contains(got, "milk:percept:testnonce") {
		t.Errorf("expected percept nonce in output, got %q", got)
	}
	if !strings.Contains(got, "milk:need:testnonce") {
		t.Errorf("expected need nonce in output, got %q", got)
	}
}

func TestBuildContext_CurrentNeed(t *testing.T) {
	sess := &session.Session{CurrentNeed: "implement JWT auth"}
	got := BuildContext(sess, "n1", nil, false, true)
	if !strings.Contains(got, "implement JWT auth") {
		t.Errorf("expected CurrentNeed in output, got %q", got)
	}
	if !strings.Contains(got, "[Current user goal]") {
		t.Errorf("expected goal header in output, got %q", got)
	}
}

func TestBuildContext_NoNeedWhenEmpty(t *testing.T) {
	sess := &session.Session{}
	got := BuildContext(sess, "n1", nil, false, true)
	if strings.Contains(got, "[Current user goal]") {
		t.Error("empty CurrentNeed should not produce goal block")
	}
}

func TestBuildContext_EscalationBrief_FirstEscalation(t *testing.T) {
	sess := &session.Session{EscalationBrief: "stuck on nil pointer in auth.go"}
	got := BuildContext(sess, "n1", nil, false, true)
	if !strings.Contains(got, "stuck on nil pointer in auth.go") {
		t.Errorf("expected EscalationBrief in first-escalation output, got %q", got)
	}
}

func TestBuildContext_EscalationBrief_SkippedOnResume(t *testing.T) {
	sess := &session.Session{EscalationBrief: "stuck on nil pointer in auth.go"}
	got := BuildContext(sess, "n1", nil, true, false)
	if strings.Contains(got, "stuck on nil pointer in auth.go") {
		t.Error("EscalationBrief should not appear on resume")
	}
}

func TestBuildContext_LastLocalSummary(t *testing.T) {
	sess := &session.Session{LastLocalSummary: "User: fix typo\nAssistant (local): done"}
	got := BuildContext(sess, "n1", nil, false, true)
	if !strings.Contains(got, "fix typo") {
		t.Errorf("expected LastLocalSummary in output, got %q", got)
	}
	if !strings.Contains(got, "[Recent local agent activity]") {
		t.Errorf("expected local activity header, got %q", got)
	}
}

func TestBuildContext_NoLocalSummaryBlock_WhenEmpty(t *testing.T) {
	sess := &session.Session{}
	got := BuildContext(sess, "n1", nil, false, true)
	if strings.Contains(got, "[Recent local agent activity]") {
		t.Error("empty LastLocalSummary should not produce activity block")
	}
}

func TestBuildContext_WithPercepts(t *testing.T) {
	sess := &session.Session{}
	got := BuildContext(sess, "n1", []string{"user prefers Go", "use flat files"}, false, true)
	if !strings.Contains(got, "[Remembered facts]") {
		t.Errorf("expected [Remembered facts] block, got %q", got)
	}
	if !strings.Contains(got, "user prefers Go") {
		t.Errorf("expected percept in output, got %q", got)
	}
}

func TestBuildContext_NilPercepts(t *testing.T) {
	sess := &session.Session{}
	got := BuildContext(sess, "n1", nil, false, true)
	if strings.Contains(got, "[Remembered facts]") {
		t.Error("nil percepts should not produce facts block")
	}
}

func TestBuildContext_ResumeIncludesLocalSummary(t *testing.T) {
	sess := &session.Session{
		LastLocalSummary: "User: run tests",
		CurrentNeed:      "fix failing tests",
	}
	got := BuildContext(sess, "n1", nil, true, false)
	if !strings.Contains(got, "fix failing tests") {
		t.Errorf("expected CurrentNeed on resume, got %q", got)
	}
	if !strings.Contains(got, "run tests") {
		t.Errorf("expected LastLocalSummary on resume, got %q", got)
	}
}

func TestBuildContext_SkipsInstructionsWhenFlagFalse(t *testing.T) {
	sess := &session.Session{}
	got := BuildContext(sess, "n1", []string{"a fact"}, true, false)
	if strings.Contains(got, "milk:percept:n1") {
		t.Error("injectInstructions=false should omit memory instruction")
	}
	if strings.Contains(got, "milk:need:n1") {
		t.Error("injectInstructions=false should omit need instruction")
	}
	if strings.Contains(got, "[Remembered facts]") {
		t.Error("injectInstructions=false should omit percepts block")
	}
}

func TestBuildContext_InjectsInstructionsOnResumeWhenFlagTrue(t *testing.T) {
	sess := &session.Session{CurrentNeed: "build auth"}
	got := BuildContext(sess, "n1", []string{"a fact"}, true, true)
	if !strings.Contains(got, "milk:percept:n1") {
		t.Error("injectInstructions=true on resume should include memory instruction")
	}
	if !strings.Contains(got, "milk:need:n1") {
		t.Error("injectInstructions=true on resume should include need instruction")
	}
	if !strings.Contains(got, "[Remembered facts]") {
		t.Error("injectInstructions=true on resume should include percepts block")
	}
}

func TestMemoryInstruction_NonceInTag(t *testing.T) {
	got := MemoryInstruction("abc123")
	if !strings.Contains(got, "<milk:percept:abc123>") {
		t.Errorf("expected nonce open tag, got %q", got)
	}
	if !strings.Contains(got, "</milk:percept:abc123>") {
		t.Errorf("expected nonce close tag, got %q", got)
	}
}

func TestNeedInstruction_NonceInTag(t *testing.T) {
	got := NeedInstruction("abc123")
	if !strings.Contains(got, "<milk:need:abc123>") {
		t.Errorf("expected need open tag, got %q", got)
	}
	if !strings.Contains(got, "</milk:need:abc123>") {
		t.Errorf("expected need close tag, got %q", got)
	}
}
