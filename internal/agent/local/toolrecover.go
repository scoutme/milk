package local

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Recovery of tool calls a server failed to deliver. Some servers (observed:
// MiMo) lose a call mid-stream: the finish_reason still says "tool_calls", the
// call's arguments arrive as nameless fragments, and the call itself is
// flushed into content as
//
//	<tool_call>=bash><parameter=command>ls</parameter></function></tool_call>
//
// (also "<=bash>" and "<function=bash>" variants). That text names the tool
// and carries every argument, so it can be turned back into a call — but only
// when all of the checks in recoverLeakedToolCalls agree, since running a
// call the server never formally delivered is otherwise a guess.

var (
	reLeakedBlock = regexp.MustCompile(`(?s)<tool_call>(.*?)</tool_call>`)
	reLeakedName  = regexp.MustCompile(`^\s*(?:<function)?<?=\s*([A-Za-z0-9_.:\-]+)\s*>`)
	reLeakedParam = regexp.MustCompile(`(?s)<parameter=([A-Za-z0-9_.\-]+)>(.*?)</parameter>`)
)

type leakedParam struct{ name, value string }

type leakedCall struct {
	name   string
	params []leakedParam
}

// parseLeakedToolBlocks extracts the XML-style calls from text, ignoring any
// block that does not start with a tool name.
func parseLeakedToolBlocks(text string) []leakedCall {
	var calls []leakedCall
	for _, m := range reLeakedBlock.FindAllStringSubmatch(text, -1) {
		body := m[1]
		nm := reLeakedName.FindStringSubmatch(body)
		if nm == nil {
			continue
		}
		lc := leakedCall{name: nm[1]}
		for _, p := range reLeakedParam.FindAllStringSubmatch(body, -1) {
			v := strings.TrimPrefix(p[2], "\n")
			v = strings.TrimSuffix(v, "\n")
			lc.params = append(lc.params, leakedParam{name: p[1], value: v})
		}
		calls = append(calls, lc)
	}
	return calls
}

type toolSchema struct {
	props    map[string]string // param name -> JSON-schema type ("" when unspecified)
	required []string
}

// toolSchemas indexes the request's tool definitions by function name.
func toolSchemas(tools []map[string]any) map[string]toolSchema {
	out := make(map[string]toolSchema, len(tools))
	for _, t := range tools {
		raw, err := json.Marshal(t)
		if err != nil {
			continue
		}
		var def struct {
			Function struct {
				Name       string `json:"name"`
				Parameters struct {
					Properties map[string]struct {
						Type any `json:"type"`
					} `json:"properties"`
					Required []string `json:"required"`
				} `json:"parameters"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &def) != nil || def.Function.Name == "" {
			continue
		}
		s := toolSchema{props: map[string]string{}, required: def.Function.Parameters.Required}
		for name, p := range def.Function.Parameters.Properties {
			typ, _ := p.Type.(string)
			s.props[name] = typ
		}
		out[def.Function.Name] = s
	}
	return out
}

// coerce validates a leaked call against its tool schema and returns typed
// arguments. Every parameter must be declared, every required one present, and
// each value must parse as its declared type.
func (s toolSchema) coerce(lc leakedCall) (map[string]any, bool) {
	args := make(map[string]any, len(lc.params))
	for _, p := range lc.params {
		typ, declared := s.props[p.name]
		if !declared {
			return nil, false
		}
		if _, dup := args[p.name]; dup {
			return nil, false
		}
		switch typ {
		case "integer":
			n, err := strconv.ParseInt(strings.TrimSpace(p.value), 10, 64)
			if err != nil {
				return nil, false
			}
			args[p.name] = n
		case "number":
			f, err := strconv.ParseFloat(strings.TrimSpace(p.value), 64)
			if err != nil {
				return nil, false
			}
			args[p.name] = f
		case "boolean":
			b, err := strconv.ParseBool(strings.TrimSpace(p.value))
			if err != nil {
				return nil, false
			}
			args[p.name] = b
		case "array", "object":
			var v any
			if json.Unmarshal([]byte(p.value), &v) != nil {
				return nil, false
			}
			args[p.name] = v
		default:
			args[p.name] = p.value
		}
	}
	for _, r := range s.required {
		if _, ok := args[r]; !ok {
			return nil, false
		}
	}
	return args, true
}

// jsonEscaped returns v as it would appear inside a JSON string literal.
func jsonEscaped(v string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return v
	}
	s := strings.TrimSuffix(b.String(), "\n")
	return s[1 : len(s)-1]
}

// corroborated reports whether every argument of a recovered call also
// appears in the nameless argument fragments the server streamed for it — two
// independent copies of the same call that must agree. Strings must match as
// whole quoted JSON literals (a bare substring would let "ls" match inside
// "else"), and each parameter's key must be present too.
func corroborated(args map[string]any, orphanText string) bool {
	if orphanText == "" {
		return false
	}
	for k, v := range args {
		if !strings.Contains(orphanText, `"`+k+`"`) {
			return false
		}
		var needle string
		switch x := v.(type) {
		case string:
			needle = `"` + jsonEscaped(x) + `"`
		case int64:
			needle = strconv.FormatInt(x, 10)
		case float64:
			needle = strconv.FormatFloat(x, 'f', -1, 64)
		case bool:
			needle = strconv.FormatBool(x)
		default:
			continue // arrays/objects: spacing differs between the two copies
		}
		if !strings.Contains(orphanText, needle) {
			return false
		}
	}
	return true
}

// recoverLeakedToolCalls turns tool calls leaked into content back into
// runnable calls. Callers must only invoke it for a response whose
// finish_reason was "tool_calls". A leaked call is recovered only if:
//  1. its tool name exists in this request's tool list,
//  2. its parameters validate against that tool's schema (declared names,
//     required present, values parse as their types),
//  3. its values are corroborated by the orphan argument fragments streamed
//     alongside it, and
//  4. it is not just the text copy of a call already delivered natively.
//
// Anything failing a check is left alone (the caller falls back to retrying).
func recoverLeakedToolCalls(tools []map[string]any, native []toolCall, leakedText, orphanText string) []toolCall {
	schemas := toolSchemas(tools)
	if len(schemas) == 0 {
		return nil
	}
	var out []toolCall
	for _, lc := range parseLeakedToolBlocks(leakedText) {
		schema, ok := schemas[lc.name]
		if !ok {
			continue
		}
		args, ok := schema.coerce(lc)
		if !ok || !corroborated(args, orphanText) || deliveredNatively(native, lc.name, args) {
			continue
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if enc.Encode(args) != nil {
			continue
		}
		out = append(out, toolCall{
			ID:       "recovered_" + uuid.NewString(),
			Type:     "function",
			Function: toolCallFunction{Name: lc.name, Arguments: strings.TrimSuffix(buf.String(), "\n")},
		})
	}
	return out
}

// deliveredNatively reports whether native already contains this call
// (same tool, semantically equal arguments).
func deliveredNatively(native []toolCall, name string, args map[string]any) bool {
	want := normalizeJSONValue(args)
	for _, n := range native {
		if n.Function.Name != name {
			continue
		}
		var got map[string]any
		if json.Unmarshal([]byte(n.Function.Arguments), &got) == nil && reflect.DeepEqual(normalizeJSONValue(got), want) {
			return true
		}
	}
	return false
}

// normalizeJSONValue round-trips v through JSON so int64/float64 and the like
// compare equal to their decoded forms.
func normalizeJSONValue(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

// orphanArgText concatenates the argument fragments the server streamed
// without ever announcing a call (no id/name): those dropped when a later
// header took over their index, plus any left on an index that never got one.
func orphanArgText(partialTools map[int]*toolCall) string {
	indices := make([]int, 0, len(partialTools))
	for i := range partialTools {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	var b strings.Builder
	for _, i := range indices {
		pt := partialTools[i]
		if pt == nil {
			continue
		}
		for _, o := range pt.orphans {
			b.WriteString(o)
		}
		if pt.ID == "" && pt.Function.Name == "" {
			b.WriteString(pt.Function.Arguments)
		}
	}
	return b.String()
}
