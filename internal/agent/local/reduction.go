package local

import (
	"encoding/json"
	"sync"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/contextreduce"
)

// Layered context reduction (issue #236).
//
// For small-context models most of every request is static overhead: the
// tools JSON array and the system prompt/instructions dominate, while the
// conversation history — the only layer milk used to shrink — is the small,
// high-value part. This file implements the missing layers of the ladder,
// applied progressively as context pressure rises (prompt tokens over the
// window-minus-output-reserve denominator):
//
//	LevelInstructions  tier stepped down one level, optional per-turn
//	                   guidance blocks dropped
//	LevelToolsCompact  tier forced to minimal, the project-instructions
//	                   (AGENTS.md) block head+tail-trimmed, tool
//	                   descriptions one-lined
//	LevelToolsPrune    tool set pruned to the tools observed in use this
//	                   session plus the protected core
//
// History compaction and hard drop-oldest remain the deeper, unchanged
// layers reached only when the static reductions are not enough. The
// reduction layers are strictly request-scoped: they rebuild the outgoing
// request from the pristine system prompt and full tool list on every tool-
// loop iteration, never mutating msgs or tools — so a layer eases back by
// itself as soon as compaction or another change frees room, and a later
// turn in the same session starts from full fidelity again.

// toolUseTracker records which tool names the model has actually asked for.
// A pointer field on Agent (never copied by value), so every shallow copy
// via With* and cloneForBackground shares one session-wide set — exactly the
// "tools observed in use this session" the prune layer needs.
type toolUseTracker struct {
	mu   sync.Mutex
	used map[string]bool
}

func newToolUseTracker() *toolUseTracker {
	return &toolUseTracker{used: map[string]bool{}}
}

// note records a tool the model called. Nil-safe: agents built without a
// tracker (tests, struct literals) simply don't track.
func (t *toolUseTracker) note(name string) {
	if t == nil || name == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.used[name] = true
}

// snapshot returns a copy of the names observed so far, safe to read
// concurrently while later tool calls keep recording.
func (t *toolUseTracker) snapshot() map[string]bool {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]bool, len(t.used))
	for n := range t.used {
		out[n] = true
	}
	return out
}

// sysPromptBuilder holds the components of the system prompt so the
// reduction ladder can rebuild it at any fidelity on demand. render at
// LevelNone reproduces exactly the pre-ladder concatenation order
// (buildSystemPrompt + guidance blocks + customPrompt prepend).
type sysPromptBuilder struct {
	cwd                        string
	selfName, escalationName   string
	workflowRole               bool
	tier                       string
	disableProjectInstructions bool
	custom                     string
	guidance                   []string
	// ownedFromStart selects the escalation role's owned-conversation framing
	// (no hand-off narrative) — see buildSystemPromptOpts.
	ownedFromStart bool
}

// render assembles the system prompt for the given reduction level.
func (b *sysPromptBuilder) render(level contextreduce.Level, p contextreduce.Policy) string {
	tier := b.tier
	trim := 0
	switch {
	case level >= contextreduce.LevelToolsCompact:
		tier, trim = "minimal", p.InstructionsTrimChars
	case level >= contextreduce.LevelInstructions:
		tier = contextreduce.StepDownTier(b.tier)
	}
	s := buildSystemPromptOpts(b.cwd, b.selfName, b.escalationName, b.workflowRole, tier, b.disableProjectInstructions, trim, b.ownedFromStart)
	if level < contextreduce.LevelInstructions {
		for _, g := range b.guidance {
			s += "\n\n" + g
		}
	}
	if b.custom != "" {
		s = b.custom + "\n\n" + s
	}
	return s
}

// WithContextReduction configures the layered context-reduction ladder
// (issue #236). c is the agent's resolved context_reduction config block;
// denominator is the pressure denominator — context window minus output
// reserve (config.AgentContextPressureDenominator). A denominator of 0 (or
// mode "off") disables the ladder entirely.
func (a *Agent) WithContextReduction(c config.ContextReductionConfig, denominator int) *Agent {
	cp := *a
	cp.reductionPolicy = contextreduce.Policy{
		Enabled:                 c.Mode != "off",
		InstructionThresholdPct: c.InstructionThresholdPct,
		ToolCompactThresholdPct: c.ToolCompactThresholdPct,
		ToolPruneThresholdPct:   c.ToolPruneThresholdPct,
		InstructionsTrimChars:   c.InstructionsTrimChars,
		ProtectedTools:          c.ProtectedTools,
	}
	cp.pressureDenominator = denominator
	if cp.usedTools == nil {
		cp.usedTools = newToolUseTracker()
	}
	return &cp
}

// noteToolUsed records that the model asked for the named tool (feeds the
// prune layer — see toolUseTracker).
func (a *Agent) noteToolUsed(name string) { a.usedTools.note(name) }

// reductionLevel picks the ladder level for the next request. It prefers
// the provider-reported prompt size of the previous request; before the
// provider has reported anything (first iteration, or right after a
// compaction reset) it falls back to a chars/4 token estimate.
func (a *Agent) reductionLevel(msgs []Message, tools []map[string]any) contextreduce.Level {
	if !a.reductionPolicy.Enabled || a.pressureDenominator <= 0 {
		return contextreduce.LevelNone
	}
	promptTokens := a.lastPromptTokens
	if promptTokens <= 0 {
		promptTokens = int64((messagesEstimateBytes(msgs) + toolsContentBytes(tools)) / 4)
	}
	if promptTokens <= 0 {
		return contextreduce.LevelNone
	}
	return contextreduce.LevelFor(int(promptTokens), a.pressureDenominator, a.reductionPolicy)
}

// imageEstimateBytes is what one image counts as in token estimates (bytes,
// ÷4 → ~4K tokens, the order of what vision backends charge for a ≤2000 px
// image). Its base64 length is wire size, not context: counting it as text
// made a single screenshot read as hundreds of thousands of tokens and
// engaged the ADR-0058 reduction ladder (tool pruning) on the first request.
const imageEstimateBytes = 16_000

// messagesEstimateBytes is messagesContentBytes for token estimation: text
// at its length, each image at imageEstimateBytes.
func messagesEstimateBytes(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
		for _, p := range m.ContentParts {
			n += len(p.Text)
			if p.ImageURL != nil {
				n += imageEstimateBytes
			}
		}
	}
	return n
}

// toolsContentBytes estimates the marshaled size of the tool schema list.
func toolsContentBytes(tools []map[string]any) int {
	n := 0
	for _, t := range tools {
		if b, err := json.Marshal(t); err == nil {
			n += len(b)
		}
	}
	return n
}

// reduceTools applies the tool-side layers for the given level and returns
// the request-scoped tool list. The input slice is never mutated; at
// LevelNone (or below the compact threshold) it is returned unchanged.
func (a *Agent) reduceTools(tools []map[string]any, level contextreduce.Level) []map[string]any {
	if level < contextreduce.LevelToolsCompact || len(tools) == 0 {
		return tools
	}
	out := contextreduce.CompactTools(tools)
	if level >= contextreduce.LevelToolsPrune {
		protected := a.reductionPolicy.ProtectedTools
		if len(protected) == 0 {
			protected = contextreduce.DefaultPolicy().ProtectedTools
		}
		out = contextreduce.PruneTools(out, a.usedTools.snapshot(), protected)
	}
	return out
}

// levelName maps a reduction level to its stable metric/log label.
func levelName(l contextreduce.Level) string {
	switch l {
	case contextreduce.LevelInstructions:
		return "instructions"
	case contextreduce.LevelToolsCompact:
		return "tools_compact"
	case contextreduce.LevelToolsPrune:
		return "tools_prune"
	default:
		return "none"
	}
}
