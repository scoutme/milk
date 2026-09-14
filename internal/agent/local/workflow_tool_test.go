package local

// Tests for the start_workflow tool (model-callable entry point into milk's
// native /workflow engine). See WorkflowStartSignal's doc comment for why
// this can't act directly — it only validates locally and returns a signal
// for cmd/milk's dispatch layer to act on.

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/workflow"
)

func TestStartWorkflow_ValidNameSignalsWorkflowStart(t *testing.T) {
	a := &Agent{}
	tc := toolCall{ID: "1", Function: toolCallFunction{
		Name:      "start_workflow",
		Arguments: `{"name":"dev","task":"ship the feature"}`,
	}}

	outcome := a.dispatchOneTool(context.Background(), tc, 0, "", "", io.Discard, nil, nil, nil)
	if outcome.workflowStart == nil {
		t.Fatalf("expected a WorkflowStartSignal, got none (msg: %+v)", outcome.msg)
	}
	if outcome.workflowStart.Name != "dev" {
		t.Errorf("want Name=dev, got %q", outcome.workflowStart.Name)
	}
	if outcome.workflowStart.Task != "ship the feature" {
		t.Errorf("want Task=%q, got %q", "ship the feature", outcome.workflowStart.Task)
	}
	// dev.yaml declares roles: [designer, generator, evaluator] — none were
	// specified in Arguments, so every one must default to "escalation".
	want := map[string]string{"designer": "escalation", "generator": "escalation", "evaluator": "escalation"}
	for role, agent := range want {
		if got := outcome.workflowStart.Roles[role]; got != agent {
			t.Errorf("role %q: want %q, got %q", role, agent, got)
		}
	}
	if outcome.msg.ToolCallID != "1" {
		t.Errorf("want tool result ToolCallID=1, got %q", outcome.msg.ToolCallID)
	}
}

func TestStartWorkflow_RoleOverridePreserved(t *testing.T) {
	a := &Agent{}
	tc := toolCall{ID: "1", Function: toolCallFunction{
		Name:      "start_workflow",
		Arguments: `{"name":"dev","task":"ship it","roles":{"generator":"gemma-local"}}`,
	}}

	outcome := a.dispatchOneTool(context.Background(), tc, 0, "", "", io.Discard, nil, nil, nil)
	if outcome.workflowStart == nil {
		t.Fatalf("expected a WorkflowStartSignal, got none")
	}
	if got := outcome.workflowStart.Roles["generator"]; got != "gemma-local" {
		t.Errorf("want overridden role generator=gemma-local, got %q", got)
	}
	if got := outcome.workflowStart.Roles["designer"]; got != "escalation" {
		t.Errorf("want un-overridden role designer defaulted to escalation, got %q", got)
	}
}

func TestStartWorkflow_UnknownNameReturnsToolResultError_NotSignal(t *testing.T) {
	a := &Agent{}
	tc := toolCall{ID: "1", Function: toolCallFunction{
		Name:      "start_workflow",
		Arguments: `{"name":"does-not-exist","task":"ship it"}`,
	}}

	outcome := a.dispatchOneTool(context.Background(), tc, 0, "", "", io.Discard, nil, nil, nil)
	if outcome.workflowStart != nil {
		t.Fatalf("expected no signal for an unknown workflow name, got %+v", outcome.workflowStart)
	}
	if !isToolError(outcome.msg.Content) {
		t.Errorf("expected an ordinary tool-result error the model can react to, got %q", outcome.msg.Content)
	}
	if !strings.Contains(outcome.msg.Content, "does-not-exist") {
		t.Errorf("expected the error to name the bad workflow, got %q", outcome.msg.Content)
	}
}

func TestStartWorkflow_EmptyTaskReturnsToolResultError_NotSignal(t *testing.T) {
	a := &Agent{}
	tc := toolCall{ID: "1", Function: toolCallFunction{
		Name:      "start_workflow",
		Arguments: `{"name":"dev","task":"  "}`,
	}}

	outcome := a.dispatchOneTool(context.Background(), tc, 0, "", "", io.Discard, nil, nil, nil)
	if outcome.workflowStart != nil {
		t.Fatalf("expected no signal for an empty task, got %+v", outcome.workflowStart)
	}
	if !isToolError(outcome.msg.Content) {
		t.Errorf("expected an ordinary tool-result error, got %q", outcome.msg.Content)
	}
}

// TestStartWorkflow_ExcludedForToolAgent verifies start_workflow is filtered
// out of a stateless agent-as-tool call's schema list, mirroring escalate's
// exclusion — there is no session for WorkflowStartSignal's caller to attach
// a launched workflow to.
func TestStartWorkflow_ExcludedForToolAgent(t *testing.T) {
	tools := schemas(nil, "", nil, nil, nil, &config.AgentLimits{ExcludedTools: []string{"escalate", "start_workflow"}})
	for _, s := range tools {
		if toolName(s) == "start_workflow" {
			t.Fatalf("expected start_workflow to be excluded, found it in the tool list")
		}
	}
}

// TestWorkflowRegistryNames_MatchesRegistry verifies the schema's enum is
// built from the live registry rather than a hardcoded list — proof the
// tool schema can't silently drift from what /workflow itself would list.
func TestWorkflowRegistryNames_MatchesRegistry(t *testing.T) {
	reg, _ := workflow.LoadRegistry()
	want := reg.Names()
	got := workflowRegistryNames()
	if len(got) != len(want) {
		t.Fatalf("want %d names, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: want %q, got %q", i, want[i], got[i])
		}
	}
}

// TestSchemas_StartWorkflowEnumIsRegistryDriven verifies the actual schema
// map's "name" enum is populated from the registry, not a static list baked
// into the Go source.
func TestSchemas_StartWorkflowEnumIsRegistryDriven(t *testing.T) {
	tools := schemas(nil, "", nil, nil, nil, nil)
	var found map[string]any
	for _, s := range tools {
		if toolName(s) == "start_workflow" {
			fn, _ := s["function"].(map[string]any)
			params, _ := fn["parameters"].(map[string]any)
			props, _ := params["properties"].(map[string]any)
			found, _ = props["name"].(map[string]any)
			break
		}
	}
	if found == nil {
		t.Fatal("start_workflow tool not found in schemas()")
	}
	enum, _ := found["enum"].([]string)
	reg, _ := workflow.LoadRegistry()
	want := reg.Names()
	if len(enum) != len(want) {
		t.Fatalf("want enum %v, got %v", want, enum)
	}
	b, _ := json.Marshal(enum)
	wb, _ := json.Marshal(want)
	if string(b) != string(wb) {
		t.Errorf("want enum %s, got %s", wb, b)
	}
}
