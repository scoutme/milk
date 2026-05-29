package session

import (
	"fmt"
	"strings"
	"time"
)

type State string

const (
	StateRouting           State = "ROUTING"
	StateLocal             State = "LOCAL"
	StateEscalation        State = "CLAUDE"
	StateEscalationWaiting State = "CLAUDE_WAITING"
)

type Agent string

const (
	AgentLocal      Agent = "local"
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
	CurrentNeed string `json:"current_need,omitempty"`
	// EscalationBrief is set when the local model calls escalate(reason).
	// It is tactical and ephemeral: overwritten on each agent-triggered escalation.
	EscalationBrief string `json:"escalation_brief,omitempty"`
	// LastLocalSummary is a pre-rendered, sanitized, budget-capped summary of
	// local-agent turns since the last escalation turn. Injected when escalating to Claude.
	LastLocalSummary string `json:"last_local_summary,omitempty"`
	// LastEscalationSummary is a pre-rendered, sanitized, budget-capped summary of
	// Escalation turns. Reserved for future demotion back to local.
	LastEscalationSummary string `json:"last_claude_summary,omitempty"`
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
// Claude session ends (i.e. the first local or user turn after the last Claude
// assistant turn). Returns 0 when there are no Claude turns.
func lastEscalationBoundary(history []Turn) int {
	last := -1
	for i, t := range history {
		if t.Agent == AgentEscalation {
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
				fmt.Fprintf(&b, "Assistant (%s): %s\n", t.Agent, t.Content)
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
	// Collect turns for this agent (user turns are shared context, included once).
	var turns []Turn
	for _, t := range history {
		switch t.Role {
		case RoleUser:
			turns = append(turns, t)
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
// LastEscalationSummary covers all escalation agent turns (+ budget cap).
// An empty escalation session (0 tool calls, < 200 chars) does not update LastEscalationSummary.
func (s *Session) RebuildSummaryBricks(budgetChars int) {
	// Local brick: only turns since the last Claude boundary.
	boundary := lastEscalationBoundary(s.History)
	s.LastLocalSummary = buildBrick(s.History[boundary:], AgentLocal, budgetChars)

	// Escalation brick: all escalation agent turns, but skip if the session was empty.
	escalationTurns := s.History
	if !emptyEscalationSession(escalationTurns, 200) {
		s.LastEscalationSummary = buildBrick(escalationTurns, AgentEscalation, budgetChars)
	}
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
