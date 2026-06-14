package session

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var ansiEscRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

type State string

const (
	StateRouting           State = "ROUTING"
	StateLocal             State = "LOCAL"
	StateEscalation        State = "ESCALATION"
	StateEscalationWaiting State = "ESCALATION_WAITING"
)

type Agent string

const (
	AgentLocal      Agent = "primary"
	AgentEscalation Agent = "escalation"
)

type Role string

const (
	RoleUser       Role = "user"
	RoleAssistant  Role = "assistant"
	RoleToolResult Role = "tool_result"
)

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Turn struct {
	Role      Role       `json:"role"`
	Agent     Agent      `json:"agent,omitempty"`
	Content   string     `json:"content"`
	Thinking  string     `json:"thinking,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Timestamp time.Time  `json:"timestamp"`
}

type Session struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name,omitempty"`
	CWD                 string    `json:"cwd"`
	CreatedAt           time.Time `json:"created_at"`
	LastUsed            time.Time `json:"last_used"`
	State               State     `json:"state"`
	EscalationSessionID string    `json:"claude_session_id,omitempty"`
	History             []Turn    `json:"history"`

	// Summary bricks — maintained eagerly after each turn.
	// CurrentNeed tracks what the user is trying to accomplish; updated by
	// <milk:need:NONCE> tags emitted by either agent when context switches.
	// CurrentNeedSetAt is len(History)+1 at the time CurrentNeed was last written
	// (1-based: 0 is the unset sentinel, ≥1 encodes a real position).
	// Used to detect stale needs that may already have been fulfilled.
	CurrentNeed      string `json:"current_need,omitempty"`
	CurrentNeedSetAt int    `json:"current_need_set_at,omitempty"`
	// EscalationBrief is set when the local model calls escalate(reason).
	// It is tactical and ephemeral: overwritten on each agent-triggered escalation.
	EscalationBrief string `json:"escalation_brief,omitempty"`
	// LastLocalSummary is a pre-rendered, sanitized, budget-capped summary of
	// local-agent turns since the last escalation turn. Injected when escalating to Claude.
	LastLocalSummary string `json:"last_local_summary,omitempty"`
	// LastLocalSummaryInjected is the value of LastLocalSummary at the time it was last
	// sent to the escalation agent. Used to detect when re-injection is necessary.
	LastLocalSummaryInjected string `json:"-"` // transient, not persisted
	// LastEscalationSummary is a pre-rendered, sanitized, budget-capped summary of
	// Escalation turns. Reserved for future demotion back to local.
	LastEscalationSummary string `json:"last_claude_summary,omitempty"`

	// EscalationNonce is generated once at the first escalation of this session
	// and reused on every subsequent turn. Keeping it stable lets the static
	// instruction block (which embeds the nonce in tag patterns) remain
	// byte-identical across turns, preserving Claude's prompt-cache prefix.
	EscalationNonce string `json:"escalation_nonce,omitempty"`

	// PrimarySessionID is the subprocess session ID for a subprocess primary agent
	// (subprocess, aider-cli). Mirrors EscalationSessionID for the primary slot.
	PrimarySessionID string `json:"primary_session_id,omitempty"`
	// PrimaryNonce is the stable nonce for the primary subprocess agent's instruction
	// blocks. Generated once on the first primary-subprocess turn, then reused.
	PrimaryNonce string `json:"primary_nonce,omitempty"`

	// MemoryInstructionInjectedAt is the escalation turn count at which the
	// memory/need instruction block was last injected. Zero means never injected.
	// Used to skip redundant re-injection on resume turns.
	MemoryInstructionInjectedAt int `json:"memory_instruction_injected_at,omitempty"`

	// LocalMemoryInstructionInjectedAt is LocalTurnCount()+1 at the time the
	// memory/need instruction was last appended to the local agent's messages.
	// 1-based: 0 is the unset sentinel (never injected); ≥1 encodes a real position.
	// Mirrors the CurrentNeedSetAt encoding to avoid the turn-0 collision.
	LocalMemoryInstructionInjectedAt int `json:"local_memory_instruction_injected_at,omitempty"`

	// ForceFreshEscalation is a transient flag set by /escalate fresh to force
	// ContextModeFirst on the next escalation turn, bypassing the normal
	// Returning/Resume detection. Cleared immediately after use. Not persisted.
	ForceFreshEscalation bool `json:"-"`

	// Tokens holds cumulative token usage for this session, keyed by "model\x00role".
	// Persisted so /usage can show totals from prior runs of the same session.
	Tokens map[string]*TokenUsage `json:"tokens,omitempty"`
}

// TokenUsage holds cumulative token counts for one (model, agent-role) pair.
type TokenUsage struct {
	Model         string `json:"model"`
	Agent         string `json:"agent"`
	Prompt        int64  `json:"prompt"`
	Completion    int64  `json:"completion"`
	CacheRead     int64  `json:"cache_read,omitempty"`
	CacheCreation int64  `json:"cache_creation,omitempty"`
}

// AddTokens accumulates token counts for a (model, role) pair into the session.
func (s *Session) AddTokens(model, role string, prompt, completion int64) {
	s.AddTokensFull(model, role, prompt, completion, 0, 0)
}

// AddTokensFull accumulates prompt, completion, and cache token counts.
func (s *Session) AddTokensFull(model, role string, prompt, completion, cacheRead, cacheCreation int64) {
	if model == "" || role == "" || (prompt == 0 && completion == 0 && cacheRead == 0 && cacheCreation == 0) {
		return
	}
	if s.Tokens == nil {
		s.Tokens = map[string]*TokenUsage{}
	}
	key := model + "\x00" + role
	e, ok := s.Tokens[key]
	if !ok {
		e = &TokenUsage{Model: model, Agent: role}
		s.Tokens[key] = e
	}
	e.Prompt += prompt
	e.Completion += completion
	e.CacheRead += cacheRead
	e.CacheCreation += cacheCreation
}

// emptyEscalationSession returns true when a Claude session produced no real work:
// zero tool calls and response text under the character threshold.
func emptyEscalationSession(turns []Turn, charThreshold int) bool {
	var totalChars int
	for _, t := range turns {
		if t.Role == RoleAssistant && t.Agent == AgentEscalation {
			totalChars += len(t.Content)
		}
		if t.Role == RoleUser && t.Agent == AgentEscalation && len(t.ToolCalls) > 0 {
			return false // had tool calls
		}
	}
	return totalChars < charThreshold
}

// lastEscalationBoundary returns the index of the first turn after the most recent
// escalation assistant turn. Returns 0 when there are no escalation assistant turns.
// Only assistant turns are considered — user turns with Agent==AgentEscalation are
// conversation scaffolding and must not shift the boundary.
func lastEscalationBoundary(history []Turn) int {
	last := -1
	for i, t := range history {
		if t.Role == RoleAssistant && t.Agent == AgentEscalation {
			last = i
		}
	}
	if last < 0 {
		return 0
	}
	return last + 1
}

// lastLocalBoundary returns the index of the first turn after the most recent
// local assistant turn. Returns 0 when there are no local assistant turns.
// Only assistant turns are considered — user turns with Agent==AgentLocal are
// conversation scaffolding and must not shift the boundary.
func lastLocalBoundary(history []Turn) int {
	last := -1
	for i, t := range history {
		if t.Role == RoleAssistant && t.Agent == AgentLocal {
			last = i
		}
	}
	if last < 0 {
		return 0
	}
	return last + 1
}

// renderTurns serialises a slice of turns to text, collapsing consecutive
// identical tool calls and truncating long tool results.
func renderTurns(turns []Turn) string {
	var b strings.Builder
	prevToolName := ""
	prevToolCount := 0

	flushTool := func() {
		if prevToolCount > 1 {
			fmt.Fprintf(&b, "  [%d more %s calls collapsed]\n", prevToolCount-1, prevToolName)
		}
		prevToolName = ""
		prevToolCount = 0
	}

	for _, t := range turns {
		switch t.Role {
		case RoleUser:
			flushTool()
			fmt.Fprintf(&b, "User: %s\n", t.Content)
		case RoleAssistant:
			if len(t.ToolCalls) > 0 {
				// Each Turn contains exactly one tool call in practice.
				tc := t.ToolCalls[0]
				if prevToolName == tc.Name {
					prevToolCount++
				} else {
					flushTool()
					fmt.Fprintf(&b, "[Tool: %s] %s\n", tc.Name, tc.Arguments)
					prevToolName = tc.Name
					prevToolCount = 1
				}
				// Additional tool calls within the same turn (rare).
				for _, tc2 := range t.ToolCalls[1:] {
					if prevToolName == tc2.Name {
						prevToolCount++
					} else {
						flushTool()
						fmt.Fprintf(&b, "[Tool: %s] %s\n", tc2.Name, tc2.Arguments)
						prevToolName = tc2.Name
						prevToolCount = 1
					}
				}
			} else {
				flushTool()
				fmt.Fprintf(&b, "Assistant (%s): %s\n", t.Agent, ansiEscRe.ReplaceAllString(t.Content, ""))
			}
		case RoleToolResult:
			// Don't flush on tool results — they belong to the preceding tool call run.
			content := t.Content
			if len(content) > 500 {
				content = content[:500] + "\n... (truncated)"
			}
			fmt.Fprintf(&b, "[Tool result] %s\n", content)
		}
	}
	flushTool()
	return b.String()
}

// buildBrick selects turns belonging to agent, applies sanitization, then
// trims from the oldest end until the rendered output is within budgetChars.
func buildBrick(history []Turn, agent Agent, budgetChars int) string {
	// Collect turns for this agent. User turns are shared context, but only
	// include a user turn if it belongs to this agent or has no agent tag (legacy).
	var turns []Turn
	for _, t := range history {
		switch t.Role {
		case RoleUser:
			if t.Agent == "" || t.Agent == agent {
				turns = append(turns, t)
			}
		case RoleAssistant:
			if t.Agent == agent {
				turns = append(turns, t)
			}
		case RoleToolResult:
			// Include tool results only when the preceding assistant turn belongs to this agent.
			if len(turns) > 0 {
				last := turns[len(turns)-1]
				if last.Role == RoleAssistant && last.Agent == agent {
					turns = append(turns, t)
				}
			}
		}
	}
	if len(turns) == 0 {
		return ""
	}
	// Trim from oldest end until within budget.
	for len(turns) > 0 {
		rendered := renderTurns(turns)
		if len(rendered) <= budgetChars {
			return rendered
		}
		turns = turns[1:]
	}
	return ""
}

// RebuildSummaryBricks recomputes LastLocalSummary and LastEscalationSummary from
// History. Call after every turn completes. budgetChars is the per-brick limit.
// LastLocalSummary covers local turns since the last escalation turn (+ budget cap).
// LastEscalationSummary covers escalation turns since the last local turn (+ budget cap).
// An empty escalation session (0 tool calls, < 200 chars) does not update LastEscalationSummary.
func (s *Session) RebuildSummaryBricks(budgetChars int) {
	// Local brick: only turns since the last escalation boundary.
	escBoundary := lastEscalationBoundary(s.History)
	s.LastLocalSummary = buildBrick(s.History[escBoundary:], AgentLocal, budgetChars)

	// Escalation brick: only turns since the last local boundary (symmetric with local brick).
	localBoundary := lastLocalBoundary(s.History)
	escalationTurns := s.History[localBoundary:]
	if !emptyEscalationSession(escalationTurns, 200) {
		s.LastEscalationSummary = buildBrick(escalationTurns, AgentEscalation, budgetChars)
	}
}

// EscalationTurnCount returns the number of assistant turns produced by the
// escalation agent in the session history.
func (s *Session) EscalationTurnCount() int {
	count := 0
	for _, t := range s.History {
		if t.Role == RoleAssistant && t.Agent == AgentEscalation {
			count++
		}
	}
	return count
}

// LocalTurnCount returns the number of assistant turns produced by the local
// agent in the session history.
func (s *Session) LocalTurnCount() int {
	count := 0
	for _, t := range s.History {
		if t.Role == RoleAssistant && t.Agent == AgentLocal {
			count++
		}
	}
	return count
}

// LocalOutputBytesSince returns the total byte length of local agent assistant
// turns that occurred after the given turn index (exclusive).
func (s *Session) LocalOutputBytesSince(afterTurnIndex int) int {
	total := 0
	count := 0
	for _, t := range s.History {
		if t.Role == RoleAssistant && t.Agent == AgentLocal {
			if count >= afterTurnIndex {
				total += len(t.Content)
			}
			count++
		}
	}
	return total
}

// EscalationOutputBytesSince returns the total byte length of escalation agent
// assistant turns that occurred after the given turn index (exclusive). Used to
// measure output volume since the last memory instruction injection.
func (s *Session) EscalationOutputBytesSince(afterTurnIndex int) int {
	total := 0
	count := 0
	for _, t := range s.History {
		if t.Role == RoleAssistant && t.Agent == AgentEscalation {
			if count >= afterTurnIndex {
				total += len(t.Content)
			}
			count++
		}
	}
	return total
}

// LocalTurnsSinceLastEscalation returns the number of local-agent assistant turns
// that have occurred after the most recent escalation assistant turn.
// Returns 0 when there is no prior escalation history (ContextModeFirst case).
func (s *Session) LocalTurnsSinceLastEscalation() int {
	// lastEscalationBoundary returns 0 when there are no escalation turns,
	// which would count all turns from the start — guard that case explicitly.
	hasEscalation := false
	for _, t := range s.History {
		if t.Agent == AgentEscalation {
			hasEscalation = true
			break
		}
	}
	if !hasEscalation {
		return 0
	}
	boundary := lastEscalationBoundary(s.History)
	count := 0
	for _, t := range s.History[boundary:] {
		if t.Role == RoleAssistant && t.Agent == AgentLocal {
			count++
		}
	}
	return count
}

// NeedChangedSinceLastEscalation reports whether CurrentNeed was updated after
// the most recent escalation assistant turn. Returns false when no need has been
// set or there is no prior escalation history.
func (s *Session) NeedChangedSinceLastEscalation() bool {
	if s.CurrentNeedSetAt == 0 || !EscalationEverActive(s) {
		return false
	}
	// CurrentNeedSetAt is 1-based (len(History)+1 at write time).
	// Count escalation assistant turns up to that point — if fewer turns had
	// occurred than now, the need was set after the last escalation turn.
	needHistoryPos := s.CurrentNeedSetAt - 1 // convert to 0-based history index
	for i := needHistoryPos; i < len(s.History); i++ {
		if s.History[i].Role == RoleAssistant && s.History[i].Agent == AgentEscalation {
			return false // an escalation turn happened after the need was set
		}
	}
	return true
}

// EscalationEverActive reports whether the escalation agent has ever produced a
// turn in this session. Used by non-CLI escalation paths that do not persist an
// EscalationSessionID to distinguish first escalation from a return.
func EscalationEverActive(s *Session) bool {
	for _, t := range s.History {
		if t.Agent == AgentEscalation {
			return true
		}
	}
	return false
}

// LastEscalationBoundary returns the history index of the first turn after the
// most recent escalation assistant turn. Returns 0 when there are no escalation
// turns. Exported for use by non-CLI escalation paths that need to scope history
// to turns since the last escalation boundary.
func LastEscalationBoundary(s *Session) int {
	return lastEscalationBoundary(s.History)
}

// EscalationMostRecent reports whether the escalation agent was the most recently
// active agent — i.e. there is at least one escalation assistant turn after the
// last local assistant turn. Used to decide whether to inject LastEscalationSummary
// into local context (only relevant immediately after returning from escalation).
func EscalationMostRecent(s *Session) bool {
	lastLocal := -1
	lastEsc := -1
	for i, t := range s.History {
		if t.Role != RoleAssistant {
			continue
		}
		if t.Agent == AgentLocal {
			lastLocal = i
		} else if t.Agent == AgentEscalation {
			lastEsc = i
		}
	}
	return lastEsc > lastLocal
}

func (s *Session) AddTurn(t Turn) {
	t.Timestamp = time.Now()
	s.History = append(s.History, t)
	s.LastUsed = t.Timestamp
}

// Transition applies a state transition, returning false if the transition is not valid.
func (s *Session) Transition(next State) bool {
	switch s.State {
	case StateRouting:
		return next == StateLocal || next == StateEscalation
	case StateLocal:
		return next == StateEscalation || next == StateRouting
	case StateEscalation:
		return next == StateEscalationWaiting || next == StateRouting
	case StateEscalationWaiting:
		return next == StateEscalation || next == StateRouting
	}
	return false
}

func (s *Session) ForceState(st State) {
	s.State = st
}
