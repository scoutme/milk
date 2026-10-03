// Package schema generates the published JSON Schema for milk's batch
// `--output-format stream-json` line format (capability stream_v1, locked by
// ADR-0050) from the typed wire model in internal/transport/streamjson.
//
// The committed artifact is docs/schema/stream-json.schema.json (whole-document
// json.MarshalIndent per design §8.3) — the exact path ADR-0050 and
// docs/machine-readable-output-design.md cite; it may not be renamed without a
// superseding ADR. Regenerate with:
//
//	go generate ./internal/transport/streamjson/schema
//
// schema_test.go fails when the committed artifact drifts from Generate().
package schema

//go:generate go run gen.go

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/scoutme/milk/internal/transport/streamjson"
)

// SchemaID is the published $id of the generated artifact.
const SchemaID = "https://github.com/scoutme/milk/docs/schema/stream-json.schema.json"

// Description is the schema-level contract summary, including the
// cache_read/cache_creation naming rationale demanded by design §6.4.
const Description = `Validates ONE line of milk's batch machine-readable output stream
(milk "prompt" --output-format stream-json), the JSONL wire ratified in
ADR-0049 and locked in detail by docs/machine-readable-output-design.md §6
(event catalog) and §8.3 (batch conventions).

Framing rules the schema cannot express (enforced by the golden-file contract
suite in internal/transport/streamjson): one UTF-8 JSON object per line with
one trailing newline and nothing else on stdout; system/init first; exactly
one terminal result last; monotonic seq per run; never any ANSI escapes;
provider detail confined to the extension/_meta reserved fields (or free-form
payloads such as tool_use.input and tool_use_result).

Conventions: snake_case fields; a "type" discriminator per line plus "subtype"
for the system/result families; open-set enums everywhere — this schema
documents known values in descriptions but does not constrain them, and
consumers must ignore values they don't recognize (result subtype known set:
success | error_during_execution | error_max_turns | interrupted | refused,
with is_error as the boolean contract — a refusal is a completed turn and
reports is_error false). usage.cache_read / usage.cache_creation are milk's
existing snake_case token-cache keys (session store + eval reports), kept
as-is rather than Anthropic's cache_read_input_tokens so one vocabulary spans
session files, eval reports, MilkEvent and this stream.

Evolution is additive-only within capability stream_v1; any shape change
requires a superseding ADR.`

// fieldDocs carries per-field rationale into the schema. Keys are
// "<TypeName>.<FieldName>".
var fieldDocs = map[string]string{
	"Event.Type":                "line discriminator — system | stream_event | assistant | user | result (open set: ignore unknown values)",
	"Event.Subtype":             "system family: init | agent_switch | route | notification | warning | state | task_started | task_progress | task_notification | background_tasks_changed | memory | commands | config_option | permission_denied | error; result family: success | error_during_execution | error_max_turns | interrupted | refused (open sets — ignore unrecognized values)",
	"Event.SessionID":           "milk session id (sess_...)",
	"Event.Seq":                 "monotonic per run (gap-tolerant consumers can detect dropped lines on relays)",
	"Event.TS":                  "RFC 3339 timestamp",
	"Event.ParentToolUseID":     "present exactly when the actor is nested (sub-agent, tool-agent, workflow stage) — makes F3/F4-style tree views reconstructable",
	"Event.Capabilities":        "open-set strings (known: stream_v1, partial_messages_v1, tasks_v1, workflows_v1) — tolerate unknown values",
	"Event.Message":             "dual-form: a string on system/error lines; the Message object on assistant/user lines",
	"Event.Agent":               "dual-form: the AgentInfo object on system/init lines; a role label string (primary | escalation) on assistant/user lines",
	"Event.Task":                "free-form task payload (workflow stage node, live-buffer chunk, ...) — tool/provider detail belongs here or in extension/_meta",
	"Event.ToolUseResult":       "free-form structured tool summary (path, is_error, summary)",
	"Event.IsError":             "boolean contract: true when the run failed (execution error, max turns, interruption); present on every result line",
	"Event.StopReason":          "open set (known: end_turn, max_turns, refusal, cancelled, error)",
	"Event.Usage":               "aggregate per-run token accounting",
	"Event.ModelUsage":          "per-model token rollups, keyed by model name",
	"Event.TotalCostUSD":        "reserved — emitted only once milk has a pricing table",
	"Event.Extension":           "reserved extension point: the ONLY home for provider-specific detail",
	"Event.Meta":                "reserved _meta field: host/implementation metadata (also the only other home for provider detail)",
	"Usage.CacheRead":           "milk's snake_case token-cache key (session store + eval reports) — kept as-is rather than Anthropic's cache_read_input_tokens (design §6.4)",
	"Usage.CacheCreation":       "milk's snake_case token-cache key (session store + eval reports) — kept as-is rather than Anthropic's cache_creation_input_tokens (design §6.4)",
	"StreamPayload.Type":        "content_block_delta | tool_args_delta (open set)",
	"StreamPayload.PartialJSON": "progressive tool-argument fragment (AI SDK-style delta framing; consumers concatenate, no partial-JSON reassembly)",
	"Delta.Type":                "text_delta | thinking_delta (open set)",
	"ContentBlock.Type":         "text | thinking | tool_use | tool_result (open set)",
	"ContentBlock.Thinking":     "reasoning preserved verbatim per ADR-0042",
	"ContentBlock.Input":        "free-form tool arguments (tool_use)",
	"ContentBlock.Content":      "free-form tool result payload (tool_result)",
}

// freeFormDocs describes untyped json.RawMessage fields.
var freeFormDocs = map[string]string{
	"Input":         "free-form JSON payload (tool-defined)",
	"Content":       "free-form JSON payload (tool result)",
	"Task":          "free-form JSON payload (task detail)",
	"ToolUseResult": "free-form JSON payload (structured tool summary)",
	"Extension":     "free-form JSON object (provider detail — never new modeled keys)",
	"Meta":          "free-form JSON object (host/implementation metadata)",
}

// Generate renders the JSON Schema for one `stream-json` line from the typed
// wire model (streamjson.Event and its nested types) and returns the whole
// document in json.MarshalIndent form with one trailing newline.
func Generate() ([]byte, error) {
	defs := map[string]any{}
	if _, ok := schemaFor(reflect.TypeOf(streamjson.Event{}), defs).(map[string]any); !ok {
		return nil, fmt.Errorf("schema: event schema is not an object")
	}
	eventSchema, ok := defs["event"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema: missing event def")
	}

	// Root: the event schema plus per-type discriminator branches (open-set
	// `type`: the known set is documented; the oneOf pins the five catalog
	// families as the contract, additive evolution updates this schema in the
	// same change per ADR-0049).
	root := map[string]any{}
	for k, v := range eventSchema {
		root[k] = v
	}
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["$id"] = SchemaID
	root["title"] = "milk machine-readable output stream v1 — one stream-json line (stream_v1)"
	root["description"] = Description
	root["oneOf"] = []any{
		typeBranch("system", "lifecycle/meta lines: system/init first, then agent_switch | route | notification | warning | state | task_started | task_progress | task_notification | background_tasks_changed | memory | commands | config_option | permission_denied | error (open set)"),
		typeBranch("stream_event", "partial deltas (partial_messages_v1): content_block_delta or tool_args_delta"),
		typeBranch("assistant", "completed assistant message blocks (text | thinking | tool_use)"),
		typeBranch("user", "tool_result blocks + tool_use_result summary"),
		typeBranch("result", "exactly one terminal line per run"),
	}
	root["$defs"] = defs

	// Whole-document MarshalIndent rule (design §8.3).
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func typeBranch(name, doc string) map[string]any {
	return map[string]any{
		"description": doc,
		"required":    []any{"type"},
		"properties": map[string]any{
			"type": map[string]any{"const": name},
		},
	}
}

// schemaFor maps a Go type to an inline schema or a $defs reference, filling
// defs as it recurses.
func schemaFor(t reflect.Type, defs map[string]any) any {
	switch t.Kind() {
	case reflect.Pointer:
		return schemaFor(t.Elem(), defs)
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{} // json.RawMessage: free-form, any JSON value
		}
		return map[string]any{
			"type":  "array",
			"items": schemaFor(t.Elem(), defs),
		}
	case reflect.Map:
		return map[string]any{
			"type":                 "object",
			"additionalProperties": schemaFor(t.Elem(), defs),
		}
	case reflect.Struct:
		name := defName(t)
		if _, done := defs[name]; done {
			return ref(name)
		}
		defs[name] = map[string]any{} // recursion guard
		def := structSchema(t, defs)
		defs[name] = def
		return ref(name)
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	default:
		return map[string]any{}
	}
}

func ref(name string) map[string]any {
	return map[string]any{"$ref": "#/$defs/" + name}
}

// defName is the snake_case $defs key for a named struct type
// (MCPServer → mcp_server, AgentInfo → agent_info).
func defName(t reflect.Type) string {
	n := t.Name()
	var b strings.Builder
	runes := []rune(n)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if 'A' <= r && r <= 'Z' {
			prevLower := i > 0 && 'a' <= runes[i-1] && runes[i-1] <= 'z'
			runUpper := i > 0 && i+1 < len(runes) && 'A' <= runes[i+1] && runes[i+1] <= 'Z'
			nextLower := i+1 < len(runes) && 'a' <= runes[i+1] && runes[i+1] <= 'z'
			if i > 0 && (prevLower || (runUpper && nextLower)) {
				b.WriteByte('_')
			}
			b.WriteRune(r + 'a' - 'A')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// structSchema renders one struct's properties from its JSON tags.
func structSchema(t reflect.Type, defs map[string]any) map[string]any {
	props := map[string]any{}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, opts := jsonName(f)
		if name == "-" {
			continue
		}
		sub := schemaFor(f.Type, defs)
		if doc := fieldDoc(t.Name(), f.Name); doc != "" {
			sub = withDoc(sub, doc)
		} else if f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.Uint8 {
			if doc := freeFormDocs[f.Name]; doc != "" {
				sub = withDoc(map[string]any{}, doc)
			}
		}
		// Dual-form fields (wire-matched by json.RawMessage) get their typed
		// shape documented as oneOf[string, $ref].
		if dual := dualForm(t.Name(), f.Name, defs); dual != nil {
			sub = dual
		}
		props[name] = sub
		if !strings.Contains(opts, "omitempty") {
			required = append(required, name)
		}
	}
	out := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           props,
	}
	if len(required) > 0 {
		out["required"] = stringSlice(required)
	}
	return out
}

func stringSlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// dualForm documents the two wire-shaped fields: `agent` (AgentInfo object |
// role-label string) and `message` (Message object | error string).
func dualForm(owner, field string, defs map[string]any) map[string]any {
	switch owner + "." + field {
	case "Event.Agent":
		schemaFor(reflect.TypeOf(streamjson.AgentInfo{}), defs)
		return map[string]any{
			"description": "dual-form: the AgentInfo object on system/init lines; a role label string (primary | escalation) on assistant/user lines",
			"oneOf":       []any{map[string]any{"type": "string"}, ref("agent_info")},
		}
	case "Event.Message":
		schemaFor(reflect.TypeOf(streamjson.Message{}), defs)
		return map[string]any{
			"description": "dual-form: a string on system/error lines; the Message object on assistant/user lines",
			"oneOf":       []any{map[string]any{"type": "string"}, ref("message")},
		}
	}
	return nil
}

func fieldDoc(owner, field string) string { return fieldDocs[owner+"."+field] }

// withDoc returns sub with description attached (copying non-$ref schemas).
func withDoc(sub any, doc string) any {
	m, ok := sub.(map[string]any)
	if !ok {
		return sub
	}
	if len(m) == 1 && m["$ref"] != nil {
		// $ref siblings: wrap so the description survives strict consumers.
		return map[string]any{
			"description": doc,
			"allOf":       []any{map[string]any{"$ref": m["$ref"]}},
		}
	}
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	out["description"] = doc
	return out
}

func jsonName(f reflect.StructField) (name, opts string) {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name, ""
	}
	parts := strings.SplitN(tag, ",", 2)
	name = parts[0]
	if name == "" {
		name = f.Name
	}
	if len(parts) == 2 {
		opts = parts[1]
	}
	return name, opts
}
