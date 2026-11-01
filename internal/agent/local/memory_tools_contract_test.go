package local

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/memory"
)

// This file pins the issue #172 contract: the four memory tools
// (record_memory, get_memory, list_memory, forget_memory) are registered for
// every agent type that receives a memory-store handle, they obey
// limits.included_tools/excluded_tools like any other tool, and the
// system-prompt mandate only appears when the tools actually do.

func newContractStore(t *testing.T) *memory.Store {
	t.Helper()
	s, err := memory.NewStore(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

var memoryToolNames = []string{"record_memory", "get_memory", "list_memory", "forget_memory"}

// TestSchemas_MemoryToolsRegisteredWhenStorePresent verifies the four memory
// tools appear whenever a store handle is passed — the stateless tool-agent
// and background-subagent call sites included (issue #172's core fix).
func TestSchemas_MemoryToolsRegisteredWhenStorePresent(t *testing.T) {
	mem := newContractStore(t)
	names := schemaNames(schemas(mem, "", nil, nil, nil, nil))
	for _, want := range memoryToolNames {
		if !containsName(names, want) {
			t.Errorf("expected memory tool %q registered when mem != nil, got %v", want, names)
		}
	}
}

// TestSchemas_MemoryToolsAbsentWhenStoreNil verifies the tools disappear
// (instead of being registered-but-broken) when no store is available.
func TestSchemas_MemoryToolsAbsentWhenStoreNil(t *testing.T) {
	names := schemaNames(schemas(nil, "", nil, nil, nil, nil))
	for _, gone := range memoryToolNames {
		if containsName(names, gone) {
			t.Errorf("memory tool %q must not be registered when mem == nil", gone)
		}
	}
}

// TestSchemas_MemoryToolsRespectLimits pins issue #172 gap 4: memory tools go
// through the same included_tools/excluded_tools filter as every other tool
// instead of bypassing it.
func TestSchemas_MemoryToolsRespectLimits(t *testing.T) {
	mem := newContractStore(t)

	// excluded_tools can scope memory away from small models entirely.
	limits := &config.AgentLimits{ExcludedTools: []string{"get_memory", "record_memory"}}
	names := schemaNames(schemas(mem, "", nil, nil, nil, limits))
	for _, gone := range []string{"get_memory", "record_memory"} {
		if containsName(names, gone) {
			t.Errorf("excluded_tools must remove memory tool %q", gone)
		}
	}
	for _, stay := range []string{"list_memory", "forget_memory"} {
		if !containsName(names, stay) {
			t.Errorf("non-excluded memory tool %q must remain", stay)
		}
	}

	// included_tools whitelists specific memory tools like any other tool.
	limits = &config.AgentLimits{IncludedTools: []string{"bash", "list_memory"}}
	names = schemaNames(schemas(mem, "", nil, nil, nil, limits))
	if !containsName(names, "list_memory") {
		t.Error("included_tools must be able to whitelist a memory tool")
	}
	for _, gone := range []string{"record_memory", "get_memory", "forget_memory"} {
		if containsName(names, gone) {
			t.Errorf("memory tool %q must not survive an included_tools whitelist that omits it", gone)
		}
	}
}

// TestMemoryCaller_RoleDerived pins issue #172 gap 2: visibility follows the
// calling agent's role — the escalation target sees ConsumerEscalation +
// shared percepts, everyone else ConsumerLocal + shared.
func TestMemoryCaller_RoleDerived(t *testing.T) {
	primary := New("http://x", "m")
	if got := primary.memoryCaller(); got != memory.ConsumerLocal {
		t.Errorf("primary-role agent: want %q, got %q", memory.ConsumerLocal, got)
	}

	esc := New("http://x", "m")
	esc.escalationName = "claude"
	if got := esc.memoryCaller(); got != memory.ConsumerEscalation {
		t.Errorf("escalation-role agent: want %q, got %q", memory.ConsumerEscalation, got)
	}
}

// TestMemoryCaller_ExplicitOverride verifies tool-agents/background clones can
// inherit their invoker's visibility — including an explicit ConsumerAll (""),
// which must stick rather than falling back to role derivation.
func TestMemoryCaller_ExplicitOverride(t *testing.T) {
	a := New("http://x", "m")
	a.escalationName = "claude" // role would say escalation

	if got := a.WithMemoryCaller(memory.ConsumerLocal).memoryCaller(); got != memory.ConsumerLocal {
		t.Errorf("override to ConsumerLocal: got %q", got)
	}
	if got := a.WithMemoryCaller(memory.ConsumerAll).memoryCaller(); got != memory.ConsumerAll {
		t.Errorf("explicit ConsumerAll override must stick, got %q", got)
	}
}

// TestMemoryCaller_BackgroundCloneInherits verifies a spawn_background_agent
// clone sees exactly what its spawner saw (issue #172).
func TestMemoryCaller_BackgroundCloneInherits(t *testing.T) {
	a := New("http://x", "m").WithMemoryCaller(memory.ConsumerEscalation)
	bg := a.cloneForBackground()
	if got := bg.memoryCaller(); got != memory.ConsumerEscalation {
		t.Errorf("background clone: want %q, got %q", memory.ConsumerEscalation, got)
	}
}

// TestMemoryToolContract_TransientBoundary pins the percept-content contract
// (2026-11-02): percepts hold durable knowledge only. The schema description,
// the system-prompt mandate and the background paragraph describe one
// contract (see Schemas' doc comment) and must all tell the model not to
// record transient state — task progress, run results, status milestones —
// which belongs to the task tools or the artifacts themselves.
func TestMemoryToolContract_TransientBoundary(t *testing.T) {
	desc := ""
	for _, s := range memory.Schemas() {
		fn, _ := s["function"].(map[string]any)
		if fn["name"] == "record_memory" {
			desc, _ = fn["description"].(string)
		}
	}
	if desc == "" {
		t.Fatal("record_memory schema not found")
	}
	for name, text := range map[string]string{
		"schema description":   desc,
		"memoryToolGuidance":   memoryToolGuidance,
		"background paragraph": backgroundSystemPrompt("/tmp", true),
	} {
		if !strings.Contains(text, "transient") {
			t.Errorf("%s: expected the transient-state boundary wording", name)
		}
	}
	// The memory-vs-tasks boundary must be stated where the tools are named.
	if !strings.Contains(desc, "task tools") {
		t.Error("record_memory description must route task state to the task tools")
	}
	if !strings.Contains(memoryToolGuidance, "update_task/complete_task") {
		t.Error("memoryToolGuidance must route task state to the task tools")
	}
}

// TestHasToolNamed exercises the helper that keeps prompt mandates in
// lockstep with the real tool list.
func TestHasToolNamed(t *testing.T) {
	tools := schemas(newContractStore(t), "", nil, nil, nil, nil)
	if !hasToolNamed(tools, "get_memory") {
		t.Error("expected get_memory found in schema list")
	}
	if hasToolNamed(tools, "no_such_tool") {
		t.Error("unexpected hit for missing tool")
	}
	if hasToolNamed(nil, "get_memory") {
		t.Error("nil schema list must not match")
	}
}

// TestBackgroundSystemPrompt_MemoryParagraphGated verifies the background
// prompt describes the memory tools exactly when the job actually has them —
// never a mandate without a tool (the issue #172 incident).
func TestBackgroundSystemPrompt_MemoryParagraphGated(t *testing.T) {
	with := backgroundSystemPrompt("/tmp", true)
	if !strings.Contains(with, "record_memory") || !strings.Contains(with, "Never edit the memory store's files directly") {
		t.Error("expected memory-tools paragraph when hasMemoryTools is true")
	}
	without := backgroundSystemPrompt("/tmp", false)
	if strings.Contains(without, "record_memory") || strings.Contains(without, "forget_memory") {
		t.Error("background prompt must not mention memory tools when they are absent")
	}
}

// TestRunBackgroundTask_MemoryToolsWired verifies the end-to-end background
// subagent path (issue #172's second call site): a non-nil store registers the
// four memory tools and the matching mandate; a nil store does neither.
func TestRunBackgroundTask_MemoryToolsWired(t *testing.T) {
	for _, tc := range []struct {
		name    string
		withMem bool
	}{
		{"with memory store", true},
		{"without memory store", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var body []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				body = b
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer srv.Close()

			var mem *memory.Store
			if tc.withMem {
				mem = newContractStore(t)
			}
			agent := New(srv.URL, "test-model")
			var out strings.Builder
			if _, _, err := agent.RunBackgroundTask(context.Background(), "job_test", nil, "/tmp", "investigate", "", &out, mem); err != nil {
				t.Fatalf("RunBackgroundTask returned error: %v", err)
			}

			mu.Lock()
			raw := string(body)
			mu.Unlock()

			var req struct {
				Messages []Message        `json:"messages"`
				Tools    []map[string]any `json:"tools"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("unmarshal request: %v", err)
			}
			names := schemaNames(req.Tools)

			var systemText string
			for _, m := range req.Messages {
				if m.Role == "system" {
					systemText += m.Content
				}
			}

			for _, want := range memoryToolNames {
				present := containsName(names, want)
				if present != tc.withMem {
					t.Errorf("tool %q present=%v (body has name: %v), want %v", want, present, strings.Contains(raw, `"`+want+`"`), tc.withMem)
				}
			}
			mandate := strings.Contains(systemText, "MANDATORY — memory tool actions") || strings.Contains(systemText, "record_memory")
			if mandate != tc.withMem {
				t.Errorf("memory mandate present=%v, want %v", mandate, tc.withMem)
			}
		})
	}
}
