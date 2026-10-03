// Package streamjson is milk's batch machine-readable wire contract: the
// canonical typed model and codec for the `--output-format stream-json` JSONL
// event stream.
//
// The contract is ratified in ADR-0049 and normatively specified in
// docs/machine-readable-output-design.md §6 (event catalog) and §8.3 (batch
// conventions). The locked rules this package encodes:
//
//   - one UTF-8 JSON object per line, one trailing \n, nothing else on stdout;
//   - snake_case fields (input_tokens, cache_read, is_error, session_id, …);
//   - a `type` discriminator per line (`system`, `stream_event`, `assistant`,
//     `user`, `result`) plus `subtype` for the `system`/`result` families;
//   - monotonic `seq` per run and an RFC 3339 `ts` on every line;
//   - optional `parent_tool_use_id` on every event whose actor is nested
//     (sub-agents, tool-agents, workflow stages);
//   - `system/init` first, exactly one terminal `result` last;
//   - open-set enums everywhere — consumers must ignore values they don't
//     recognize (result `subtype`: success | error_during_execution |
//     error_max_turns | interrupted | refused, with `is_error` as the boolean
//     contract; `system/init.capabilities` is a set of open-set strings);
//   - never ANSI: machine transports emit no escape sequences;
//   - provider detail may ride ONLY in the `extension` / `_meta` reserved
//     fields (or inside free-form payload fields such as `tool_use.input`) —
//     never as new top-level or structurally modeled keys.
//
// Evolution is additive-only within capability `stream_v1`; any shape change
// requires a superseding ADR. The golden-file contract suite
// (golden_test.go over testdata/events/*.jsonl) and the generated JSON Schema
// under docs/schema/ keep the contract checkable, not folklore.
package streamjson

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// Wire `type` discriminators. Open set: consumers must ignore unknown types.
const (
	TypeSystem      = "system"
	TypeStreamEvent = "stream_event"
	TypeAssistant   = "assistant"
	TypeUser        = "user"
	TypeResult      = "result"
)

// `system` family subtypes (open set).
const (
	SubtypeInit                   = "init"
	SubtypeAgentSwitch            = "agent_switch"
	SubtypeRoute                  = "route"
	SubtypeNotification           = "notification"
	SubtypeWarning                = "warning"
	SubtypeState                  = "state"
	SubtypeTaskStarted            = "task_started"
	SubtypeTaskProgress           = "task_progress"
	SubtypeTaskNotification       = "task_notification"
	SubtypeBackgroundTasksChanged = "background_tasks_changed"
	SubtypeMemory                 = "memory"
	SubtypeCommands               = "commands"
	SubtypeConfigOption           = "config_option"
	SubtypePermissionDenied       = "permission_denied"
	SubtypeError                  = "error"
)

// `result` family subtypes — the known values of the open enum (Claude Code
// vocabulary). `is_error` remains the boolean contract; consumers must ignore
// unrecognized subtypes.
const (
	ResultSuccess              = "success"
	ResultErrorDuringExecution = "error_during_execution"
	ResultErrorMaxTurns        = "error_max_turns"
	ResultInterrupted          = "interrupted"
	ResultRefused              = "refused"
)

// Known `system/init.capabilities` strings (open set: emitters may advertise
// more, consumers must tolerate unknown values).
const (
	CapabilityStream          = "stream_v1"
	CapabilityPartialMessages = "partial_messages_v1"
	CapabilityTasks           = "tasks_v1"
	CapabilityWorkflows       = "workflows_v1"
)

// Event is one `stream-json` line. The wire is deliberately flat (Claude
// Code-shaped): each event family populates a subset of these fields, and the
// golden-file suite pins which subset appears where. Fields typed
// json.RawMessage are free-form on the wire (tool inputs, tool results, task
// payloads, dual-form `agent`/`message`) and are preserved verbatim on
// round-trip.
type Event struct {
	// Envelope — present on every line.
	Type            string `json:"type"`
	Subtype         string `json:"subtype,omitempty"`
	SessionID       string `json:"session_id"`
	Seq             int64  `json:"seq"`
	TS              string `json:"ts"`
	ParentToolUseID string `json:"parent_tool_use_id,omitempty"`

	// system/init meta payload (design §6.1).
	CWD             string          `json:"cwd,omitempty"`
	MilkVersion     string          `json:"milk_version,omitempty"`
	Agent           json.RawMessage `json:"agent,omitempty"` // dual-form: init = AgentInfo object; assistant/user = role string (AsAgent/AgentLabel)
	EscalationAgent *AgentInfo      `json:"escalation_agent,omitempty"`
	Tools           []string        `json:"tools,omitempty"`
	MCPServers      []MCPServer     `json:"mcp_servers,omitempty"`
	Route           *RouteDecision  `json:"route,omitempty"`
	SessionState    string          `json:"session_state,omitempty"` // init: state machine incl. sticky-escalation; state events: running | idle | requires_action (design §6.3, ADR-0050)
	Warnings        []string        `json:"warnings,omitempty"`
	Capabilities    []string        `json:"capabilities,omitempty"`

	// system lifecycle payloads (design §6.3), keyed by subtype.
	From        string          `json:"from,omitempty"`         // agent_switch
	To          string          `json:"to,omitempty"`           // agent_switch
	Reason      string          `json:"reason,omitempty"`       // agent_switch / permission_denied / route
	ID          string          `json:"id,omitempty"`           // notification
	Severity    string          `json:"severity,omitempty"`     // notification
	CommandHint string          `json:"command_hint,omitempty"` // notification
	Body        string          `json:"body,omitempty"`         // notification
	Category    string          `json:"category,omitempty"`     // warning
	Count       int             `json:"count,omitempty"`        // warning
	Limit       int             `json:"limit,omitempty"`        // warning
	Message     json.RawMessage `json:"message,omitempty"`      // error: string; assistant/user: Message object
	Recoverable *bool           `json:"recoverable,omitempty"`  // error (explicit boolean — false is meaningful and must round-trip)
	Tool        string          `json:"tool,omitempty"`         // permission_denied
	TaskID      string          `json:"task_id,omitempty"`      // task_*
	Kind        string          `json:"kind,omitempty"`         // task_* (background_agent|workflow|tool_agent)
	Status      string          `json:"status,omitempty"`       // task_*
	RunningIDs  []string        `json:"running_ids,omitempty"`  // background_tasks_changed
	FinishedIDs []string        `json:"finished_ids,omitempty"` // background_tasks_changed
	Op          string          `json:"op,omitempty"`           // memory
	PerceptID   string          `json:"percept_id,omitempty"`   // memory
	Subject     string          `json:"subject,omitempty"`      // memory
	Commands    []Command       `json:"commands,omitempty"`     // commands
	Option      string          `json:"option,omitempty"`       // config_option
	Value       string          `json:"value,omitempty"`        // config_option
	StopReason  string          `json:"stop_reason,omitempty"`  // state (on idle) / result
	Task        json.RawMessage `json:"task,omitempty"`         // task_* free-form payload (stage node, buffer chunk, …)
	IsError     *bool           `json:"is_error,omitempty"`     // error / result

	// Content (design §6.2).
	Event         *StreamPayload  `json:"event,omitempty"`           // stream_event
	ToolUseResult json.RawMessage `json:"tool_use_result,omitempty"` // user

	// Terminal result payload (design §6.4).
	NumTurns     int64             `json:"num_turns,omitempty"`
	DurationMS   int64             `json:"duration_ms,omitempty"`
	Result       string            `json:"result,omitempty"`
	RouteHistory []RouteHop        `json:"route_history,omitempty"`
	Usage        *Usage            `json:"usage,omitempty"`
	ModelUsage   map[string]*Usage `json:"model_usage,omitempty"`
	TotalCostUSD *float64          `json:"total_cost_usd,omitempty"` // reserved; emitted only with a pricing table

	// Reserved extension points: the ONLY home for provider-specific detail.
	Extension json.RawMessage `json:"extension,omitempty"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}

// AgentInfo describes the primary or escalation agent (system/init).
type AgentInfo struct {
	Name                string `json:"name"`
	Provider            string `json:"provider"`
	Model               string `json:"model,omitempty"`
	Role                string `json:"role,omitempty"`
	ContextWindowTokens int64  `json:"context_window_tokens,omitempty"`
}

// MCPServer is one connected MCP server (system/init).
type MCPServer struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// RouteDecision is a routing decision (system/init.route, system/route).
type RouteDecision struct {
	Target     string `json:"target"`
	Reason     string `json:"reason,omitempty"`
	Conclusive bool   `json:"conclusive,omitempty"`
}

// RouteHop records which agent handled one turn of the run (result.route_history).
type RouteHop struct {
	Turn   int64  `json:"turn"`
	Target string `json:"target"`
}

// Command is one available slash command (system/commands).
type Command struct {
	Name        string `json:"name"`
	Hint        string `json:"hint,omitempty"`
	Description string `json:"description,omitempty"`
}

// Usage is one token-accounting bucket. cache_read / cache_creation are milk's
// existing snake_case token-cache keys (session store + eval reports) — kept
// as-is rather than Anthropic's cache_read_input_tokens, per design §6.4.
// All four counters are always emitted so consumers see explicit zeros.
type Usage struct {
	InputTokens   int64 `json:"input_tokens"`
	OutputTokens  int64 `json:"output_tokens"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

// StreamPayload is the typed body of a `stream_event` line: a single streaming
// fragment. `tool_args_delta` uses the AI SDK's explicit delta framing rather
// than Claude's raw input_json_delta, so partial-JSON reassembly is not the
// consumer's job (design §6.2).
type StreamPayload struct {
	Type        string          `json:"type"` // content_block_delta | tool_args_delta (open set)
	Index       *int64          `json:"index,omitempty"`
	Delta       *Delta          `json:"delta,omitempty"`
	ToolUseID   string          `json:"tool_use_id,omitempty"`
	PartialJSON string          `json:"partial_json,omitempty"`
	Extension   json.RawMessage `json:"extension,omitempty"`
	Meta        json.RawMessage `json:"_meta,omitempty"`
}

// Delta is a content_block_delta fragment (text_delta | thinking_delta; open set).
type Delta struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
}

// Message is the assistant/user message object carried in `message`.
// On the wire `message` is dual-form: a string on system/error lines, this
// object on assistant/user lines (see Event.Message and AsMessage).
type Message struct {
	ID      string         `json:"id,omitempty"`
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ContentBlock is one message content block (text | thinking | tool_use |
// tool_result; open set).
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`          // tool_use
	Name      string          `json:"name,omitempty"`        // tool_use
	Input     json.RawMessage `json:"input,omitempty"`       // tool_use (free-form)
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result
	Content   json.RawMessage `json:"content,omitempty"`     // tool_result (free-form)
	IsError   *bool           `json:"is_error,omitempty"`    // tool_result
	Extension json.RawMessage `json:"extension,omitempty"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}

// AsAgent parses the dual-form `agent` field into its object form
// (system/init). Returns (nil, nil) when absent or in the assistant/user
// string (role-label) form.
func (e *Event) AsAgent() (*AgentInfo, error) {
	if len(e.Agent) == 0 {
		return nil, nil
	}
	var probe any
	if err := json.Unmarshal(e.Agent, &probe); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	if _, ok := probe.(string); ok {
		return nil, nil
	}
	var a AgentInfo
	if err := json.Unmarshal(e.Agent, &a); err != nil {
		return nil, fmt.Errorf("agent object: %w", err)
	}
	return &a, nil
}

// AgentLabel returns the string (role-label) form of `agent` on assistant/user
// lines ("primary", "escalation", …).
func (e *Event) AgentLabel() (string, error) {
	if len(e.Agent) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(e.Agent, &s); err != nil {
		return "", fmt.Errorf("agent string form: %w", err)
	}
	return s, nil
}

// AsMessage parses the dual-form `message` field into its object form.
// Returns (nil, nil) when the field is absent or is the system/error string
// form; returns an error when the object form is malformed.
func (e *Event) AsMessage() (*Message, error) {
	if len(e.Message) == 0 {
		return nil, nil
	}
	var probe any
	if err := json.Unmarshal(e.Message, &probe); err != nil {
		return nil, fmt.Errorf("message: %w", err)
	}
	if _, ok := probe.(string); ok {
		return nil, nil // system/error string form
	}
	var m Message
	if err := json.Unmarshal(e.Message, &m); err != nil {
		return nil, fmt.Errorf("message object: %w", err)
	}
	return &m, nil
}

// MessageText returns the string form of `message` (system/error lines).
func (e *Event) MessageText() (string, error) {
	if len(e.Message) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(e.Message, &s); err != nil {
		return "", fmt.Errorf("message string form: %w", err)
	}
	return s, nil
}

// SetMessageObject sets `message` to the assistant/user object form.
func (e *Event) SetMessageObject(m *Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	e.Message = b
	return nil
}

// SetMessageText sets `message` to the system/error string form.
func (e *Event) SetMessageText(s string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	e.Message = b
	return nil
}

// EncodeLine serializes one event in canonical batch form: compact JSON (key
// order fixed by Event's field order) plus one trailing newline. This is the
// only streaming serialization in milk; whole-document exports stay
// json.MarshalIndent (design §8.3).
func EncodeLine(ev *Event) ([]byte, error) {
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Decode parses one `stream-json` line (without trailing newline) into the
// typed event model. Unknown fields outside the reserved extension points are
// rejected by the golden contract, not here — but any data they carry would not
// survive an encode round-trip, which the golden suite asserts never happens.
func Decode(line []byte) (*Event, error) {
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("streamjson: decode: %w", err)
	}
	if ev.Type == "" {
		return nil, fmt.Errorf("streamjson: decode: missing type discriminator")
	}
	return &ev, nil
}

// Encoder emits events with monotonic seq stamping (design §6: seq is
// monotonic per run, starting at 0). Producer-side helper for the
// `--output-format stream-json` transport.
type Encoder struct {
	w   io.Writer
	seq int64
}

// NewEncoder returns an Encoder writing line-framed events to w.
func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

// Encode stamps ev.Seq monotonically and writes one canonical line.
func (e *Encoder) Encode(ev *Event) error {
	ev.Seq = e.seq
	e.seq++
	line, err := EncodeLine(ev)
	if err != nil {
		return err
	}
	_, err = e.w.Write(line)
	return err
}

// Decoder reads line-framed events. Consumer-side helper for the eval adapter
// and other stream-json consumers. Open-set tolerant: unrecognized fields are
// ignored (they should not exist outside extension/_meta).
type Decoder struct {
	s *bufio.Scanner
}

// NewDecoder returns a Decoder reading from r.
func NewDecoder(r io.Reader) *Decoder {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)
	return &Decoder{s: s}
}

// Next returns the next event, or io.EOF at end of stream. Blank lines are
// skipped; malformed lines return an error.
func (d *Decoder) Next() (*Event, error) {
	for d.s.Scan() {
		line := d.s.Bytes()
		if len(line) == 0 {
			continue
		}
		return Decode(line)
	}
	if err := d.s.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}
