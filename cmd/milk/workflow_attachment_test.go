package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
)

// ── attachmentTaskBlock / workflowTaskWithAttachments ─────────────────────────

func TestAttachmentTaskBlock(t *testing.T) {
	atts := []PendingAttachment{
		{Path: "/tmp/milk-paste-123.png", Name: "milk-paste-123.png", MIMEType: "image/png", Data: make([]byte, 145820)},
		{Path: "/home/u/notes.md", Name: "notes.md", MIMEType: "text/markdown", Data: []byte("hi")},
	}
	block := attachmentTaskBlock(atts)
	for _, want := range []string{
		"## Attachments",
		"milk-paste-123.png (image/png, 145820 bytes) — file: /tmp/milk-paste-123.png",
		"notes.md (text/markdown, 2 bytes) — file: /home/u/notes.md",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q:\n%s", want, block)
		}
	}
	// The block must reference files by path only — no inline base64 payloads
	// (they would be mangled by interp's prompt truncation caps).
	if strings.Contains(block, "base64") {
		t.Errorf("block must not inline base64 data:\n%s", block)
	}
	if got := attachmentTaskBlock(nil); got != "" {
		t.Errorf("attachmentTaskBlock(nil) = %q, want empty", got)
	}
}

func TestWorkflowTaskWithAttachments(t *testing.T) {
	if got := workflowTaskWithAttachments("do the thing", nil); got != "do the thing" {
		t.Errorf("without attachments task must be unchanged, got %q", got)
	}
	atts := []PendingAttachment{{Path: "/tmp/a.png", Name: "a.png", MIMEType: "image/png", Data: []byte{1}}}
	got := workflowTaskWithAttachments("do the thing", atts)
	if !strings.HasPrefix(got, "do the thing\n\n## Attachments") {
		t.Errorf("task should keep its text first, then the attachment block, got %q", got)
	}
}

// ── launchGenericWorkflow consumes staged attachments ─────────────────────────

// TestLaunchGenericWorkflow_ConsumesPendingAttachments verifies that launching
// a workflow takes ownership of m.pendingAttachments: the slot is cleared (so
// the attachments cannot silently leak into a later normal REPL turn) and a
// transcript note tells the user where the files went.
func TestLaunchGenericWorkflow_ConsumesPendingAttachments(t *testing.T) {
	sandboxMilkHome(t)
	reg, errs := workflow.LoadRegistry()
	if len(errs) != 0 {
		t.Fatalf("LoadRegistry errors: %v", errs)
	}
	def, ok := reg.Lookup("swarm")
	if !ok {
		t.Fatal("expected built-in \"swarm\" definition")
	}

	m := testModel()
	m.ctx = context.Background()
	m.st = &interactiveState{sess: &session.Session{ID: "test-launch-attachments-session"}}
	m.pendingAttachments = []PendingAttachment{
		{Path: "/tmp/milk-paste-9.png", Name: "milk-paste-9.png", MIMEType: "image/png", Data: []byte{1, 2, 3}},
		{Path: "/tmp/spec.md", Name: "spec.md", MIMEType: "text/markdown", Data: []byte("# spec")},
	}

	roleValues := make(map[string]string, len(def.Roles))
	for _, role := range def.Roles {
		roleValues[role] = workflow.AliasPrimary
	}
	wizard := &workflowWizardState{
		name:       "swarm",
		task:       "build several things from the attached spec",
		def:        def,
		roles:      def.Roles,
		roleValues: roleValues,
	}

	newM, _ := m.launchGenericWorkflow(wizard)
	nm := newM.(model)

	if nm.pendingAttachments != nil {
		t.Errorf("expected pendingAttachments cleared at workflow launch, still %v", nm.pendingAttachments)
	}
	out := nm.transcript.String()
	for _, want := range []string{"attached 2 file(s)", "milk-paste-9.png", "spec.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript missing %q:\n%s", want, out)
		}
	}
}

// ── workflowTurnRunner first-turn attachment injection ────────────────────────

// fakeTurnRunner is a minimal TurnRunner recording the prompts it receives.
// Set isCLI to select the CLI branch (@path injection) or the local branch
// (vision content parts via the imagePartReceiver interface below).
type fakeTurnRunner struct {
	isCLI   bool
	prompts []string
}

func (f *fakeTurnRunner) Name() string { return "fake" }
func (f *fakeTurnRunner) IsCLI() bool  { return f.isCLI }
func (f *fakeTurnRunner) Ping() error  { return nil }
func (f *fakeTurnRunner) Execute(
	_ context.Context,
	_ config.Config,
	_ *session.Session,
	_ *memory.Store,
	_ AgentRole,
	_ escalation.ContextMode,
	_, _ string,
	_ []string,
	_ bool,
	prompt string,
	_ TurnCallbacks,
	_ io.Writer,
) (TurnResult, error) {
	f.prompts = append(f.prompts, prompt)
	return TurnResult{Text: "ok"}, nil
}

func (f *fakeTurnRunner) RunToolCall(context.Context, config.Config, string, []local.ContentPart, io.Writer) (string, error) {
	return "", nil
}

// fakeVisionRunner additionally records SetPendingImageParts calls (satisfying
// the imagePartReceiver interface like localRunner does).
type fakeVisionRunner struct {
	fakeTurnRunner
	imageCalls [][]local.ContentPart
}

func (f *fakeVisionRunner) SetPendingImageParts(parts []local.ContentPart) {
	f.imageCalls = append(f.imageCalls, parts)
}

func TestWorkflowTurnRunner_CLIInjectsAtPathOnFirstTurnOnly(t *testing.T) {
	inner := &fakeTurnRunner{isCLI: true}
	wtr := &workflowTurnRunner{
		inner:       inner,
		role:        RoleWorkflow,
		roleName:    "designer",
		notifier:    oversight.Noop{},
		attachments: []PendingAttachment{{Path: "/tmp/milk-paste-9.png", Name: "milk-paste-9.png", MIMEType: "image/png", Data: []byte{1}}},
	}

	if _, err := wtr.Run(context.Background(), "plan it", nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, err := wtr.Run(context.Background(), "next stage", nil); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(inner.prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(inner.prompts))
	}
	if !strings.Contains(inner.prompts[0], "@/tmp/milk-paste-9.png\n") {
		t.Errorf("first prompt should carry the @path attachment line, got %q", inner.prompts[0])
	}
	if !strings.Contains(inner.prompts[0], "plan it") {
		t.Errorf("first prompt should keep the original task text, got %q", inner.prompts[0])
	}
	if strings.Contains(inner.prompts[1], "@") {
		t.Errorf("second prompt must not re-inject attachments, got %q", inner.prompts[1])
	}
}

func TestWorkflowTurnRunner_LocalInjectsImagePartsOnFirstTurnOnly(t *testing.T) {
	inner := &fakeVisionRunner{}
	wtr := &workflowTurnRunner{
		inner:    inner,
		role:     RoleWorkflow,
		roleName: "designer",
		notifier: oversight.Noop{},
		attachments: []PendingAttachment{
			{Path: "/tmp/milk-paste-9.png", Name: "milk-paste-9.png", MIMEType: "image/png", Data: []byte{1, 2}},
			{Path: "/tmp/spec.md", Name: "spec.md", MIMEType: "text/markdown", Data: []byte("# spec")},
		},
	}

	if _, err := wtr.Run(context.Background(), "plan it", nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, err := wtr.Run(context.Background(), "next stage", nil); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(inner.imageCalls) != 1 {
		t.Fatalf("expected exactly 1 SetPendingImageParts call (first turn only), got %d", len(inner.imageCalls))
	}
	parts := inner.imageCalls[0]
	if len(parts) != 1 {
		t.Fatalf("expected 1 image part (text attachments are path-referenced only), got %d", len(parts))
	}
	if parts[0].Type != "image_url" || parts[0].ImageURL == nil ||
		!strings.HasPrefix(parts[0].ImageURL.URL, "data:image/png;base64,") {
		t.Errorf("image part = %+v, want image_url with a png data URI", parts[0])
	}
	// Non-image attachments must not arrive as image parts.
	for _, p := range inner.prompts {
		if strings.Contains(p, "base64") {
			t.Errorf("prompt must not inline base64, got %q", p)
		}
	}
}

// TestBuildWorkflowRunners_AttachmentsPropagated verifies every role runner
// receives the staged attachments so its first turn can inject them.
func TestBuildWorkflowRunners_AttachmentsPropagated(t *testing.T) {
	atts := []PendingAttachment{{Path: "/tmp/a.png", Name: "a.png", MIMEType: "image/png", Data: []byte{1}}}
	agentNames := map[string]string{"designer": "", "generator": ""}
	da := &dispatchAgents{}

	runners, err := buildWorkflowRunners(agentNames, config.Config{}, &session.Session{ID: "repl"}, nil, da, permContext{}, nil, nil, atts)
	if err != nil {
		t.Fatalf("buildWorkflowRunners: %v", err)
	}
	for role, r := range runners {
		wtr := r.(*workflowTurnRunner)
		if len(wtr.attachments) != 1 || wtr.attachments[0].Name != "a.png" {
			t.Errorf("role %q: attachments = %+v, want the staged list", role, wtr.attachments)
		}
	}
}
