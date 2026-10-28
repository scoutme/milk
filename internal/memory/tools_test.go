package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func newToolsStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	freezeClock(s)
	return s
}

func TestDispatchRecordMemory_DefaultProducer(t *testing.T) {
	s := newToolsStore(t)
	result := DispatchRecordMemory(context.Background(), s, `{"content":"test fact"}`)

	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := out["error"]; ok {
		t.Fatalf("unexpected error: %v", out["error"])
	}
	if len(s.global.Percepts) != 1 {
		t.Fatalf("expected 1 percept, got %d", len(s.global.Percepts))
	}
	if s.global.Percepts[0].Producer != ProducerLocal {
		t.Errorf("expected ProducerLocal, got %q", s.global.Percepts[0].Producer)
	}
}

func TestDispatchRecordMemory_UserProducer(t *testing.T) {
	s := newToolsStore(t)
	result := DispatchRecordMemory(context.Background(), s, `{"content":"user stated fact","producer":"user"}`)

	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := out["error"]; ok {
		t.Fatalf("unexpected error: %v", out["error"])
	}
	if len(s.global.Percepts) != 1 {
		t.Fatalf("expected 1 percept, got %d", len(s.global.Percepts))
	}
	p := s.global.Percepts[0]
	if p.Producer != ProducerUser {
		t.Errorf("expected ProducerUser, got %q", p.Producer)
	}
	if p.W != 0.9 {
		t.Errorf("expected W=0.9 for user producer, got %v", p.W)
	}
}

func TestDispatchRecordMemory_ClaudeProducer(t *testing.T) {
	s := newToolsStore(t)
	result := DispatchRecordMemory(context.Background(), s, `{"content":"escalation fact","producer":"escalation"}`)

	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := out["error"]; ok {
		t.Fatalf("unexpected error: %v", out["error"])
	}
	if len(s.global.Percepts) != 1 {
		t.Fatalf("expected 1 percept, got %d", len(s.global.Percepts))
	}
	if s.global.Percepts[0].Producer != ProducerEscalation {
		t.Errorf("expected ProducerEscalation, got %q", s.global.Percepts[0].Producer)
	}
}

func TestDispatchRecordMemory_EmptyContent(t *testing.T) {
	s := newToolsStore(t)
	result := DispatchRecordMemory(context.Background(), s, `{"content":""}`)

	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := out["error"]; !ok {
		t.Errorf("expected error key in result for empty content, got: %v", out)
	}
}

func TestDispatchRecordMemory_DuplicateReinforces(t *testing.T) {
	s := newToolsStore(t)
	// Record the original.
	DispatchRecordMemory(context.Background(), s, `{"content":"user prefers flat file output over JSON"}`) //nolint:errcheck

	// Near-duplicate — should reinforce the existing percept.
	result := DispatchRecordMemory(context.Background(), s, `{"content":"user prefers flat file output not JSON"}`)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := out["error"]; ok {
		t.Errorf("duplicate should return ok result with reinforced message, got error: %v", out["error"])
	}
	msg, _ := out["output"].(string)
	if !strings.Contains(msg, "reinforced") {
		t.Errorf("expected 'reinforced' in output, got %q", msg)
	}
	// Store must still contain exactly one percept.
	if len(s.global.Percepts) != 1 {
		t.Errorf("expected 1 percept after reinforcement, got %d", len(s.global.Percepts))
	}
}

func TestDispatchRecordMemory_ConsumerHint(t *testing.T) {
	s := newToolsStore(t)
	result := DispatchRecordMemory(context.Background(), s, `{"content":"primary-only fact","consumer":"primary"}`)

	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := out["error"]; ok {
		t.Fatalf("unexpected error: %v", out["error"])
	}
	if len(s.global.Percepts) != 1 {
		t.Fatalf("expected 1 percept, got %d", len(s.global.Percepts))
	}
	if s.global.Percepts[0].Consumer != ConsumerLocal {
		t.Errorf("expected ConsumerLocal, got %q", s.global.Percepts[0].Consumer)
	}
}

func TestDispatchGetMemory_ReturnsResults(t *testing.T) {
	s := newToolsStore(t)
	s.Record(context.Background(), "user prefers verbose output", ProducerUser, ConsumerAll, Roles{}, false) //nolint:errcheck

	result := DispatchGetMemory(context.Background(), s, `{"query":"verbose"}`, ConsumerAll)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := out["error"]; ok {
		t.Fatalf("unexpected error: %v", out["error"])
	}
	if !strings.Contains(out["output"].(string), "verbose") {
		t.Errorf("expected 'verbose' in output, got %q", out["output"])
	}
}

func TestDispatchGetMemory_EmptyQuery(t *testing.T) {
	s := newToolsStore(t)
	result := DispatchGetMemory(context.Background(), s, `{"query":"nothing here"}`, ConsumerAll)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := out["error"]; ok {
		t.Fatalf("unexpected error: %v", out["error"])
	}
	if !strings.Contains(out["output"].(string), "no relevant") {
		t.Errorf("expected 'no relevant' in output, got %q", out["output"])
	}
}

func TestDispatchGetMemory_ConsumerFilter(t *testing.T) {
	s := newToolsStore(t)
	s.Record(context.Background(), "local only fact", ProducerUser, ConsumerLocal, Roles{}, false)       //nolint:errcheck
	s.Record(context.Background(), "claude only fact", ProducerUser, ConsumerEscalation, Roles{}, false) //nolint:errcheck

	// Claude caller should only see the claude-targeted fact.
	result := DispatchGetMemory(context.Background(), s, `{"query":"fact"}`, ConsumerEscalation)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	msg := out["output"].(string)
	if strings.Contains(msg, "local only") {
		t.Errorf("local-only percept should not be visible to claude caller")
	}
	if !strings.Contains(msg, "claude only") {
		t.Errorf("claude-only percept should be visible to claude caller, got %q", msg)
	}
}

func TestDispatchListMemory_AllPercepts(t *testing.T) {
	s := newToolsStore(t)
	s.Record(context.Background(), "fact alpha", ProducerUser, ConsumerAll, Roles{}, false)  //nolint:errcheck
	s.Record(context.Background(), "fact beta", ProducerSystem, ConsumerAll, Roles{}, false) //nolint:errcheck

	result := DispatchListMemory(context.Background(), s, `{}`, ConsumerAll)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	msg := out["output"].(string)
	if !strings.Contains(msg, "alpha") || !strings.Contains(msg, "beta") {
		t.Errorf("expected both percepts in output, got %q", msg)
	}
}

func TestDispatchListMemory_Empty(t *testing.T) {
	s := newToolsStore(t)
	result := DispatchListMemory(context.Background(), s, `{}`, ConsumerAll)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !strings.Contains(out["output"].(string), "no percepts") {
		t.Errorf("expected 'no percepts' for empty store, got %q", out["output"])
	}
}

func TestDispatchListMemoryFiltered_DropsUnrelated(t *testing.T) {
	s := newToolsStore(t)
	s.Record(context.Background(), "routing session state", ProducerUser, ConsumerAll, Roles{}, false)     //nolint:errcheck
	s.Record(context.Background(), "database migration plan", ProducerSystem, ConsumerAll, Roles{}, false) //nolint:errcheck

	result := DispatchListMemoryFiltered(context.Background(), s, `{}`, "routing", ConsumerAll)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	msg := out["output"].(string)
	if !strings.Contains(msg, "routing") {
		t.Errorf("expected matching percept in output, got %q", msg)
	}
	if strings.Contains(msg, "database") {
		t.Errorf("expected unrelated percept filtered out, got %q", msg)
	}
}

func TestDispatchListMemoryFiltered_EmptyPromptPassesAll(t *testing.T) {
	s := newToolsStore(t)
	s.Record(context.Background(), "fact alpha", ProducerUser, ConsumerAll, Roles{}, false)  //nolint:errcheck
	s.Record(context.Background(), "fact beta", ProducerSystem, ConsumerAll, Roles{}, false) //nolint:errcheck

	result := DispatchListMemoryFiltered(context.Background(), s, `{}`, "", ConsumerAll)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	msg := out["output"].(string)
	if !strings.Contains(msg, "alpha") || !strings.Contains(msg, "beta") {
		t.Errorf("expected all percepts when prompt is empty, got %q", msg)
	}
}

func TestDispatchForgetMemory_DeletesByPrefix(t *testing.T) {
	ctx := context.Background()
	s := newToolsStore(t)
	DispatchRecordMemory(ctx, s, `{"content":"fact to forget"}`)
	id := s.global.Percepts[0].ID

	result := DispatchForgetMemory(ctx, s, `{"id":"`+id[:8]+`"}`)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := out["error"]; ok {
		t.Fatalf("unexpected error: %v", out["error"])
	}
	if len(s.global.Percepts) != 0 {
		t.Errorf("expected percept to be deleted, store still has %d", len(s.global.Percepts))
	}
}

func TestDispatchForgetMemory_DeletesByHashPrefix(t *testing.T) {
	ctx := context.Background()
	s := newToolsStore(t)
	DispatchRecordMemory(ctx, s, `{"content":"fact to forget with hash"}`)
	id := s.global.Percepts[0].ID

	// Pass the ID with a leading '#' — should strip it and delete normally.
	result := DispatchForgetMemory(ctx, s, `{"id":"#`+id[:8]+`"}`)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := out["error"]; ok {
		t.Fatalf("unexpected error: %v", out["error"])
	}
	if len(s.global.Percepts) != 0 {
		t.Errorf("expected percept to be deleted, store still has %d", len(s.global.Percepts))
	}
}

// TestDispatchForgetMemory_NotFound verifies issue #172 gap 5: a not-found
// target returns an error-shaped result (not an ok-shaped "percept X not
// found"), so a tool agent cannot mistake a no-op for a successful deletion.
func TestDispatchForgetMemory_NotFound(t *testing.T) {
	ctx := context.Background()
	s := newToolsStore(t)

	result := DispatchForgetMemory(ctx, s, `{"id":"deadbeef"}`)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	errVal, ok := out["error"]
	if !ok {
		t.Fatalf("expected error-shaped result for not-found target, got %v", out)
	}
	if !strings.Contains(errVal.(string), "not found") {
		t.Errorf("expected 'not found' in error, got %q", errVal)
	}
}

func TestDispatchForgetMemory_AmbiguousPrefix(t *testing.T) {
	ctx := context.Background()
	s := newToolsStore(t)
	// Force two percepts with a known common prefix by recording and then manually
	// checking the ambiguous-match path using the full IDs (which differ).
	// Instead: record two percepts and use a single-char prefix that matches both.
	DispatchRecordMemory(ctx, s, `{"content":"alpha fact"}`)
	DispatchRecordMemory(ctx, s, `{"content":"beta fact"}`)

	// Use empty string as prefix — should match all and trigger ambiguity.
	result := DispatchForgetMemory(ctx, s, `{"id":""}`)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	// Empty id is caught by the "id is required" guard, not the ambiguous path.
	if _, ok := out["error"]; !ok {
		t.Errorf("expected error for empty id, got output: %v", out["output"])
	}
}

func TestDispatchForgetMemory_MissingID(t *testing.T) {
	ctx := context.Background()
	s := newToolsStore(t)

	result := DispatchForgetMemory(ctx, s, `{}`)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := out["error"]; !ok {
		t.Errorf("expected error for missing id")
	}
}

// ── issue #172: forget_memory parity with /forget (id/ids/pattern) ───────────

// TestDispatchForgetMemory_ByIdsBatch verifies several percepts can be removed
// in one call via ids (the batch path from gap 1).
func TestDispatchForgetMemory_ByIdsBatch(t *testing.T) {
	ctx := context.Background()
	s := newToolsStore(t)
	DispatchRecordMemory(ctx, s, `{"content":"first unique fact alpha about bananas"}`)
	DispatchRecordMemory(ctx, s, `{"content":"second unique fact beta about zebras"}`)
	if len(s.global.Percepts) != 2 {
		t.Fatalf("expected 2 percepts, got %d", len(s.global.Percepts))
	}
	id1 := s.global.Percepts[0].ID[:8]
	id2 := s.global.Percepts[1].ID[:8]

	result := DispatchForgetMemory(ctx, s, `{"ids":["`+id1+`","`+id2+`"]}`)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if errVal, ok := out["error"]; ok {
		t.Fatalf("unexpected error: %v", errVal)
	}
	if !strings.Contains(out["output"].(string), "deleted 2 percepts") {
		t.Errorf("expected batch deletion summary, got %q", out["output"])
	}
	if len(s.global.Percepts) != 0 {
		t.Errorf("expected empty store after batch delete, got %d", len(s.global.Percepts))
	}
}

// TestDispatchForgetMemory_ByDescription verifies a non-ID description
// resolves via unique content match — parity with /forget (gap 1).
func TestDispatchForgetMemory_ByDescription(t *testing.T) {
	ctx := context.Background()
	s := newToolsStore(t)
	DispatchRecordMemory(ctx, s, `{"content":"user prefers dark mode in the editor"}`)

	result := DispatchForgetMemory(ctx, s, `{"id":"dark mode in the editor"}`)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if errVal, ok := out["error"]; ok {
		t.Fatalf("unexpected error: %v", errVal)
	}
	if len(s.global.Percepts) != 0 {
		t.Errorf("expected percept deleted by description, store still has %d", len(s.global.Percepts))
	}
}

// TestDispatchForgetMemory_AmbiguousDescription verifies a description hitting
// several percepts is an error listing candidates, not a wild deletion.
func TestDispatchForgetMemory_AmbiguousDescription(t *testing.T) {
	ctx := context.Background()
	s := newToolsStore(t)
	DispatchRecordMemory(ctx, s, `{"content":"project alpha uses go modules"}`)
	DispatchRecordMemory(ctx, s, `{"content":"project alpha needs a release"}`)

	result := DispatchForgetMemory(ctx, s, `{"id":"project alpha"}`)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	errVal, ok := out["error"]
	if !ok {
		t.Fatalf("expected error for ambiguous description, got %v", out)
	}
	if !strings.Contains(errVal.(string), "matches 2 percepts") {
		t.Errorf("expected ambiguity listing in error, got %q", errVal)
	}
	if len(s.global.Percepts) != 2 {
		t.Errorf("expected store untouched, got %d percepts", len(s.global.Percepts))
	}
}

// TestDispatchForgetMemory_PatternDeletesAll verifies the pattern batch path:
// one call deletes every content-matching percept (parity with /forget).
func TestDispatchForgetMemory_PatternDeletesAll(t *testing.T) {
	ctx := context.Background()
	s := newToolsStore(t)
	DispatchRecordMemory(ctx, s, `{"content":"stale note about widget pricing"}`)
	DispatchRecordMemory(ctx, s, `{"content":"stale note about gizmo retirement"}`)
	DispatchRecordMemory(ctx, s, `{"content":"unrelated durable fact"}`)

	result := DispatchForgetMemory(ctx, s, `{"pattern":"stale note"}`)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if errVal, ok := out["error"]; ok {
		t.Fatalf("unexpected error: %v", errVal)
	}
	if !strings.Contains(out["output"].(string), "deleted 2 percepts") {
		t.Errorf("expected 2 deleted in summary, got %q", out["output"])
	}
	if len(s.global.Percepts) != 1 || !strings.Contains(s.global.Percepts[0].Content, "unrelated") {
		t.Errorf("expected only the unrelated percept to survive, got %+v", s.global.Percepts)
	}
}

// TestDispatchForgetMemory_AtomicWhenOneTargetMissing verifies all targets
// resolve before anything is deleted: one bad target leaves the store intact.
func TestDispatchForgetMemory_AtomicWhenOneTargetMissing(t *testing.T) {
	ctx := context.Background()
	s := newToolsStore(t)
	DispatchRecordMemory(ctx, s, `{"content":"keep me around as a durable record"}`)
	DispatchRecordMemory(ctx, s, `{"content":"and keep me too as another entry"}`)
	id1 := s.global.Percepts[0].ID[:8]

	result := DispatchForgetMemory(ctx, s, `{"ids":["`+id1+`","nonexistent-description-xyz"]}`)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if errVal, ok := out["error"]; !ok {
		t.Fatalf("expected error-shaped result, got %v", out)
	} else if !strings.Contains(errVal.(string), "not found") {
		t.Errorf("expected 'not found' in error, got %q", errVal)
	}
	if len(s.global.Percepts) != 2 {
		t.Errorf("expected store untouched on partial failure, got %d percepts", len(s.global.Percepts))
	}
}

// ── issue #172 gap 3: consumer filter on list_memory ─────────────────────────

// TestDispatchListMemory_ConsumerArgFilter verifies the consumer argument
// narrows visibility to the named tag (plus shared percepts).
func TestDispatchListMemory_ConsumerArgFilter(t *testing.T) {
	s := newToolsStore(t)
	s.Record(context.Background(), "primary scoped fact", ProducerUser, ConsumerLocal, Roles{}, false)         //nolint:errcheck
	s.Record(context.Background(), "escalation scoped fact", ProducerUser, ConsumerEscalation, Roles{}, false) //nolint:errcheck
	s.Record(context.Background(), "shared fact", ProducerUser, ConsumerAll, Roles{}, false)                   //nolint:errcheck

	result := DispatchListMemory(context.Background(), s, `{"consumer":"escalation"}`, ConsumerAll)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	msg := out["output"].(string)
	if !strings.Contains(msg, "escalation scoped") {
		t.Errorf("expected escalation-scoped percept visible, got %q", msg)
	}
	if !strings.Contains(msg, "shared fact") {
		t.Errorf("expected shared percept visible, got %q", msg)
	}
	if strings.Contains(msg, "primary scoped") {
		t.Errorf("primary-scoped percept must not appear under consumer=escalation, got %q", msg)
	}
}

// TestDispatchListMemory_CallerVisibility verifies the calling agent's
// visibility (own tag plus shared) applies when no consumer argument is given.
func TestDispatchListMemory_CallerVisibility(t *testing.T) {
	s := newToolsStore(t)
	s.Record(context.Background(), "primary scoped fact", ProducerUser, ConsumerLocal, Roles{}, false)         //nolint:errcheck
	s.Record(context.Background(), "escalation scoped fact", ProducerUser, ConsumerEscalation, Roles{}, false) //nolint:errcheck

	result := DispatchListMemory(context.Background(), s, `{}`, ConsumerEscalation)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	msg := out["output"].(string)
	if !strings.Contains(msg, "escalation scoped") {
		t.Errorf("expected own-tag percept visible to escalation caller, got %q", msg)
	}
	if strings.Contains(msg, "primary scoped") {
		t.Errorf("primary-scoped percept must not be visible to escalation caller, got %q", msg)
	}
}

// ── issue #172: schema/prompt contract parity ────────────────────────────────

// schemaProps extracts the parameter property names of a named tool schema.
func schemaProps(t *testing.T, name string) (props map[string]bool, required []any) {
	t.Helper()
	for _, s := range Schemas() {
		fn, _ := s["function"].(map[string]any)
		if fn == nil || fn["name"] != name {
			continue
		}
		params, _ := fn["parameters"].(map[string]any)
		props = map[string]bool{}
		if raw, ok := params["properties"].(map[string]any); ok {
			for k := range raw {
				props[k] = true
			}
		}
		required, _ = params["required"].([]any)
		return props, required
	}
	t.Fatalf("schema for %q not found", name)
	return nil, nil
}

// TestSchemas_ForgetMemoryParityWithPrompt verifies gap 1: forget_memory's
// schema exposes id/ids/pattern (the same matching as /forget) and requires
// nothing up front — the call shapes the prompt describes are all valid.
func TestSchemas_ForgetMemoryParityWithPrompt(t *testing.T) {
	props, required := schemaProps(t, "forget_memory")
	for _, want := range []string{"id", "ids", "pattern"} {
		if !props[want] {
			t.Errorf("forget_memory schema must expose %q (parity with /forget)", want)
		}
	}
	if len(required) != 0 {
		t.Errorf("forget_memory must not hard-require id (ids/pattern are alternatives), got required=%v", required)
	}
}

// TestSchemas_ListMemoryHasConsumerFilter verifies gap 3: the existing
// ListOpts.Consumer field is exposed in the tool schema.
func TestSchemas_ListMemoryHasConsumerFilter(t *testing.T) {
	props, _ := schemaProps(t, "list_memory")
	if !props["consumer"] {
		t.Error("list_memory schema must expose the consumer filter")
	}
}
