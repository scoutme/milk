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
	if p.W != 1.0 {
		t.Errorf("expected W=1.0 for user producer, got %v", p.W)
	}
}

func TestDispatchRecordMemory_ClaudeProducer(t *testing.T) {
	s := newToolsStore(t)
	result := DispatchRecordMemory(context.Background(), s, `{"content":"claude fact","producer":"claude"}`)

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
	if s.global.Percepts[0].Producer != ProducerClaude {
		t.Errorf("expected ProducerClaude, got %q", s.global.Percepts[0].Producer)
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

func TestDispatchRecordMemory_DuplicateReturnsSkipped(t *testing.T) {
	s := newToolsStore(t)
	// Record the original.
	DispatchRecordMemory(context.Background(), s, `{"content":"user prefers flat file output over JSON"}`) //nolint:errcheck

	// Near-duplicate — should be skipped.
	result := DispatchRecordMemory(context.Background(), s, `{"content":"user prefers flat file output not JSON"}`)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := out["error"]; ok {
		t.Errorf("duplicate should return ok result with skipped message, got error: %v", out["error"])
	}
	msg, _ := out["output"].(string)
	if !strings.Contains(msg, "skipped") {
		t.Errorf("expected 'skipped' in output, got %q", msg)
	}
	// Store must still contain exactly one percept.
	if len(s.global.Percepts) != 1 {
		t.Errorf("expected 1 percept after duplicate suppression, got %d", len(s.global.Percepts))
	}
}

func TestDispatchRecordMemory_ConsumerHint(t *testing.T) {
	s := newToolsStore(t)
	result := DispatchRecordMemory(context.Background(), s, `{"content":"local-only fact","consumer":"local"}`)

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
	s.Record(context.Background(), "local only fact", ProducerUser, ConsumerLocal, Roles{}, false)   //nolint:errcheck
	s.Record(context.Background(), "claude only fact", ProducerUser, ConsumerClaude, Roles{}, false) //nolint:errcheck

	// Claude caller should only see the claude-targeted fact.
	result := DispatchGetMemory(context.Background(), s, `{"query":"fact"}`, ConsumerClaude)
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
	s.Record(context.Background(), "fact alpha", ProducerUser, ConsumerAll, Roles{}, false)   //nolint:errcheck
	s.Record(context.Background(), "fact beta", ProducerSystem, ConsumerAll, Roles{}, false)  //nolint:errcheck

	result := DispatchListMemory(context.Background(), s, `{}`)
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
	result := DispatchListMemory(context.Background(), s, `{}`)
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !strings.Contains(out["output"].(string), "no percepts") {
		t.Errorf("expected 'no percepts' for empty store, got %q", out["output"])
	}
}
