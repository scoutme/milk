package session

import "time"

type State string

const (
	StateRouting       State = "ROUTING"
	StateLocal         State = "LOCAL"
	StateClaude        State = "CLAUDE"
	StateClaudeWaiting State = "CLAUDE_WAITING"
)

type Agent string

const (
	AgentLocal  Agent = "local"
	AgentClaude Agent = "claude"
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
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Timestamp time.Time  `json:"timestamp"`
}

type Session struct {
	ID              string    `json:"id"`
	Name            string    `json:"name,omitempty"`
	CWD             string    `json:"cwd"`
	CreatedAt       time.Time `json:"created_at"`
	LastUsed        time.Time `json:"last_used"`
	State           State     `json:"state"`
	ClaudeSessionID string    `json:"claude_session_id,omitempty"`
	History         []Turn    `json:"history"`
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
		return next == StateLocal || next == StateClaude
	case StateLocal:
		return next == StateClaude || next == StateRouting
	case StateClaude:
		return next == StateClaudeWaiting || next == StateRouting
	case StateClaudeWaiting:
		return next == StateClaude || next == StateRouting
	}
	return false
}

func (s *Session) ForceState(st State) {
	s.State = st
}
