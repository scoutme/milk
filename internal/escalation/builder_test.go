package escalation

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

func TestBuildContext_EmptySession(t *testing.T) {
	sess := &session.Session{}
	got := BuildContext(sess, "testnonce", nil, ContextModeFirst, true, "", "")
	if !strings.Contains(got, "milk:percept:testnonce") {
		t.Errorf("expected percept nonce in output, got %q", got)
	}
	if !strings.Contains(got, "milk:need:testnonce") {
		t.Errorf("expected need nonce in output, got %q", got)
	}
}

func TestBuildContext_CurrentNeed(t *testing.T) {
	sess := &session.Session{CurrentNeed: "implement JWT auth"}
	got := BuildContext(sess, "n1", nil, ContextModeFirst, true, "", "")
	if !strings.Contains(got, "implement JWT auth") {
		t.Errorf("expected CurrentNeed in output, got %q", got)
	}
	if !strings.Contains(got, "[Current user goal]") {
		t.Errorf("expected goal header in output, got %q", got)
	}
}

func TestBuildContext_CurrentNeed_FreshLabel(t *testing.T) {
	sess := &session.Session{
		CurrentNeed:      "implement JWT auth",
		CurrentNeedSetAt: 4, // 1-based: set when len(History)=3 → turnsAgo = 5-(4-1) = 2 → fresh
		History:          make([]session.Turn, 5),
	}
	got := BuildContext(sess, "n1", nil, ContextModeFirst, true, "", "")
	if !strings.Contains(got, "[Current user goal]") {
		t.Errorf("recent need should use fresh header, got %q", got)
	}
	if strings.Contains(got, "may already be fulfilled") {
		t.Errorf("recent need should not carry stale warning, got %q", got)
	}
}

func TestBuildContext_CurrentNeed_StaleLabel(t *testing.T) {
	sess := &session.Session{
		CurrentNeed:      "implement JWT auth",
		CurrentNeedSetAt: 2, // 1-based: set when len(History)=1 → turnsAgo = 6-(2-1) = 5 → stale
		History:          make([]session.Turn, 6),
	}
	got := BuildContext(sess, "n1", nil, ContextModeFirst, true, "", "")
	if strings.Contains(got, "[Current user goal]") {
		t.Errorf("stale need should not use fresh header, got %q", got)
	}
	if !strings.Contains(got, "may already be fulfilled") {
		t.Errorf("stale need should carry stale warning, got %q", got)
	}
	if !strings.Contains(got, "implement JWT auth") {
		t.Errorf("stale need content should still be present, got %q", got)
	}
}

func TestBuildContext_NoNeedWhenEmpty(t *testing.T) {
	sess := &session.Session{}
	got := BuildContext(sess, "n1", nil, ContextModeFirst, true, "", "")
	if strings.Contains(got, "[Current user goal]") {
		t.Error("empty CurrentNeed should not produce goal block")
	}
}

func TestBuildContext_EscalationBrief_FirstEscalation(t *testing.T) {
	sess := &session.Session{EscalationBrief: "stuck on nil pointer in auth.go"}
	got := BuildContext(sess, "n1", nil, ContextModeFirst, true, "", "")
	if !strings.Contains(got, "stuck on nil pointer in auth.go") {
		t.Errorf("expected EscalationBrief in first-escalation output, got %q", got)
	}
}

func TestBuildContext_EscalationBrief_SkippedOnResume(t *testing.T) {
	sess := &session.Session{EscalationBrief: "stuck on nil pointer in auth.go"}
	got := BuildContext(sess, "n1", nil, ContextModeResume, false, "", "")
	if strings.Contains(got, "stuck on nil pointer in auth.go") {
		t.Error("EscalationBrief should not appear on resume")
	}
}

func TestBuildContext_EscalationBrief_IncludedOnReturning(t *testing.T) {
	sess := &session.Session{EscalationBrief: "stuck on nil pointer in auth.go"}
	got := BuildContext(sess, "n1", nil, ContextModeReturning, true, "", "")
	if !strings.Contains(got, "stuck on nil pointer in auth.go") {
		t.Errorf("expected EscalationBrief on returning, got %q", got)
	}
}

func TestBuildContext_LastLocalSummary(t *testing.T) {
	sess := &session.Session{LastLocalSummary: "User: fix typo\nAssistant (primary): done"}
	got := BuildContext(sess, "n1", nil, ContextModeFirst, true, "", "")
	if !strings.Contains(got, "fix typo") {
		t.Errorf("expected LastLocalSummary in output, got %q", got)
	}
	if !strings.Contains(got, "[Recent primary agent activity]") {
		t.Errorf("expected primary activity header, got %q", got)
	}
}

func TestBuildContext_NoLocalSummaryBlock_WhenEmpty(t *testing.T) {
	sess := &session.Session{}
	got := BuildContext(sess, "n1", nil, ContextModeFirst, true, "", "")
	if strings.Contains(got, "[Recent primary agent activity]") {
		t.Error("empty LastLocalSummary should not produce activity block")
	}
}

func TestBuildContext_WithPercepts(t *testing.T) {
	sess := &session.Session{}
	got := BuildContext(sess, "n1", []string{"user prefers Go", "use flat files"}, ContextModeFirst, true, "", "")
	if !strings.Contains(got, "[Remembered facts]") {
		t.Errorf("expected [Remembered facts] block, got %q", got)
	}
	if !strings.Contains(got, "user prefers Go") {
		t.Errorf("expected percept in output, got %q", got)
	}
}

func TestBuildContext_NilPercepts(t *testing.T) {
	sess := &session.Session{}
	got := BuildContext(sess, "n1", nil, ContextModeFirst, true, "", "")
	if strings.Contains(got, "[Remembered facts]") {
		t.Error("nil percepts should not produce facts block")
	}
}

func TestBuildContext_ResumeIncludesLocalSummary(t *testing.T) {
	// Resume omits identity/need/instructions — only the changed primary summary is sent.
	sess := &session.Session{
		LastLocalSummary: "User: run tests",
		CurrentNeed:      "fix failing tests",
	}
	got := BuildContext(sess, "n1", nil, ContextModeResume, false, "", "")
	if strings.Contains(got, "fix failing tests") {
		t.Error("resume should not include CurrentNeed (already cached in Claude's context)")
	}
	if strings.Contains(got, identityBlock) {
		t.Error("resume should not include identity block (already cached)")
	}
	if !strings.Contains(got, "run tests") {
		t.Errorf("expected LastLocalSummary on resume, got %q", got)
	}
}

func TestBuildContext_ReturningDoesNotIncludeEscalationSummary(t *testing.T) {
	// ContextModeReturning uses --resume so Claude already has its history in context.
	// Injecting the escalation summary would be redundant and bust the prompt cache.
	sess := &session.Session{
		LastEscalationSummary: "User: implement feature\nAssistant (escalation): done",
		CurrentNeed:           "polish the UI",
	}
	got := BuildContext(sess, "n1", nil, ContextModeReturning, false, "", "")
	if strings.Contains(got, "[Recent escalation agent activity]") {
		t.Error("returning mode should not include escalation summary block (uses --resume)")
	}
	if strings.Contains(got, "implement feature") {
		t.Error("returning mode should not inject escalation summary content (uses --resume)")
	}
	if !strings.Contains(got, "polish the UI") {
		t.Errorf("expected CurrentNeed on returning, got %q", got)
	}
}

func TestBuildContext_FirstDoesNotIncludeEscalationSummary(t *testing.T) {
	sess := &session.Session{
		LastEscalationSummary: "some prior escalation work",
	}
	got := BuildContext(sess, "n1", nil, ContextModeFirst, true, "", "")
	if strings.Contains(got, "[Recent escalation agent activity]") {
		t.Error("first escalation should not include escalation summary block")
	}
}

func TestBuildContext_SkipsInstructionsWhenFlagFalse(t *testing.T) {
	sess := &session.Session{}
	got := BuildContext(sess, "n1", []string{"a fact"}, ContextModeResume, false, "", "")
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
	// Resume ignores injectInstructions — Claude already has them cached.
	sess := &session.Session{CurrentNeed: "build auth"}
	got := BuildContext(sess, "n1", []string{"a fact"}, ContextModeResume, true, "", "")
	if strings.Contains(got, "milk:percept:n1") {
		t.Error("resume should not include memory instruction (already cached)")
	}
	if strings.Contains(got, "milk:need:n1") {
		t.Error("resume should not include need instruction (already cached)")
	}
	if strings.Contains(got, "[Remembered facts]") {
		t.Error("resume should not include percepts block (already cached)")
	}
}

// --- BuildStaticContext / BuildDynamicContext split tests ---

func TestBuildStaticContext_ContainsInstructions(t *testing.T) {
	got := BuildStaticContext("n1", []string{"a fact"}, ContextModeFirst, true, "primary", "claude")
	if !strings.Contains(got, "milk:percept:n1") {
		t.Error("static context should contain memory instruction")
	}
	if !strings.Contains(got, "milk:need:n1") {
		t.Error("static context should contain need instruction")
	}
	if !strings.Contains(got, "[Remembered facts]") {
		t.Error("static context should contain percepts")
	}
}

func TestBuildStaticContext_EmptyOnResumeNoReinjection(t *testing.T) {
	got := BuildStaticContext("n1", []string{"a fact"}, ContextModeResume, false, "primary", "claude")
	if got != "" {
		t.Errorf("static context should be empty on resume when injectInstructions=false, got %q", got)
	}
}

func TestBuildStaticContext_ReinjectedOnResumeWhenThresholdCrossed(t *testing.T) {
	got := BuildStaticContext("n1", []string{"a fact"}, ContextModeResume, true, "primary", "claude")
	if got == "" {
		t.Error("static context should be re-injected on resume when injectInstructions=true")
	}
	if !strings.Contains(got, "milk:percept") {
		t.Error("re-injected static context should contain memory instruction")
	}
}

func TestBuildStaticContext_EmptyWhenNoInject(t *testing.T) {
	got := BuildStaticContext("n1", []string{"a fact"}, ContextModeFirst, false, "primary", "claude")
	if got != "" {
		t.Errorf("static context should be empty when injectInstructions=false, got %q", got)
	}
}

func TestBuildDynamicContext_ContainsIdentityAndNeed(t *testing.T) {
	sess := &session.Session{
		CurrentNeed:      "fix the bug",
		EscalationBrief:  "nil pointer in auth.go",
		LastLocalSummary: "User: run tests\nAssistant: done",
	}
	got := BuildDynamicContext(sess, ContextModeFirst)
	if !strings.Contains(got, identityBlock) {
		t.Error("dynamic context should contain identity block on first")
	}
	if !strings.Contains(got, "fix the bug") {
		t.Error("dynamic context should contain CurrentNeed")
	}
	if !strings.Contains(got, "nil pointer in auth.go") {
		t.Error("dynamic context should contain EscalationBrief")
	}
	if !strings.Contains(got, "run tests") {
		t.Error("dynamic context should contain LastLocalSummary")
	}
}

func TestBuildDynamicContext_ResumeOnlyChangedSummary(t *testing.T) {
	sess := &session.Session{
		CurrentNeed:      "fix the bug",
		LastLocalSummary: "User: run tests",
	}
	got := BuildDynamicContext(sess, ContextModeResume)
	if strings.Contains(got, identityBlock) {
		t.Error("dynamic context on resume should not contain identity block")
	}
	if strings.Contains(got, "fix the bug") {
		t.Error("dynamic context on resume should not contain CurrentNeed")
	}
	if !strings.Contains(got, "run tests") {
		t.Error("dynamic context on resume should contain LastLocalSummary")
	}
}

func TestBuildDynamicContext_ResumeEmptyWhenSummaryUnchanged(t *testing.T) {
	sess := &session.Session{
		LastLocalSummary:         "User: run tests",
		LastLocalSummaryInjected: "User: run tests",
	}
	got := BuildDynamicContext(sess, ContextModeResume)
	if got != "" {
		t.Errorf("dynamic context on resume should be empty when summary unchanged, got %q", got)
	}
}

func TestBuildStaticContext_DoesNotContainDynamicParts(t *testing.T) {
	got := BuildStaticContext("n1", nil, ContextModeFirst, true, "primary", "claude")
	if strings.Contains(got, identityBlock) {
		t.Error("static context should not contain identity block")
	}
}

func TestBuildDynamicContext_DoesNotContainInstructions(t *testing.T) {
	sess := &session.Session{}
	got := BuildDynamicContext(sess, ContextModeFirst)
	if strings.Contains(got, "milk:percept:") {
		t.Error("dynamic context should not contain memory instruction")
	}
	if strings.Contains(got, "milk:need:") {
		t.Error("dynamic context should not contain need instruction")
	}
}

func TestMemoryInstruction_NonceInTag(t *testing.T) {
	got := MemoryInstruction("abc123", "primary", "escalation")
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

func TestFormatPercepts_NonEmpty(t *testing.T) {
	got := FormatPercepts([]string{"user prefers Go", "flat files only"})
	if !strings.Contains(got, "[Remembered facts]") {
		t.Error("expected [Remembered facts] header")
	}
	if !strings.Contains(got, "user prefers Go") || !strings.Contains(got, "flat files only") {
		t.Error("expected percept content")
	}
}

func TestFormatPercepts_Empty(t *testing.T) {
	if got := FormatPercepts(nil); got != "" {
		t.Errorf("expected empty string for nil percepts, got %q", got)
	}
	if got := FormatPercepts([]string{}); got != "" {
		t.Errorf("expected empty string for empty percepts, got %q", got)
	}
}
