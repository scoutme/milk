package local

import (
	"encoding/json"

	"github.com/scoutme/milk/internal/obs"
)

// PayloadInfo is the first-request payload composition of a session turn —
// issue #462's meter. All counts are UTF-8 bytes of the JSON-relevant
// content: SystemBytes is the rendered system prompt, ToolsBytes the
// serialized tool-schema array, MessagesBytes the sum of every message's
// text content (system included — so total ≈ MessagesBytes + ToolsBytes +
// per-message JSON framing). Byte counts are tokenizer-agnostic on purpose;
// consumers divide by ~4 for a token estimate.
type PayloadInfo struct {
	SystemBytes   int64  `json:"system_bytes"`
	ToolsBytes    int64  `json:"tools_bytes"`
	MessagesBytes int64  `json:"messages_bytes"`
	Diet          string `json:"diet"` // effective tier: full | compact | lean
}

// noteFirstPayload records the first assembled request of this agent and
// emits the per-request composition at DEBUG. Later calls are no-ops: the
// stable prefix is what the diet moves, and the tool loop re-sends it every
// iteration. Safe for concurrent use (first writer wins).
func (a *Agent) noteFirstPayload(systemPrompt string, msgs []Message, tools []map[string]any) {
	toolsBytes := int64(0)
	if n, err := jsonMarshalLen(tools); err == nil {
		toolsBytes = int64(n)
	}
	info := &PayloadInfo{
		SystemBytes:   int64(len(systemPrompt)),
		ToolsBytes:    toolsBytes,
		MessagesBytes: messagesBytes(msgs),
		Diet:          dietLabel(a.toolDescTier),
	}
	if a.firstPayload != nil {
		a.firstPayload.CompareAndSwap(nil, info)
	}
	obs.Debug("payload", "system_bytes", info.SystemBytes, "tools_bytes", info.ToolsBytes,
		"messages_bytes", info.MessagesBytes, "diet", info.Diet)
}

// FirstPayload returns the recorded first-request composition, or ok=false
// when this agent has not assembled a request yet (escalation-only runs,
// non-HTTP providers, bare test agents).
func (a *Agent) FirstPayload() (PayloadInfo, bool) {
	if a.firstPayload == nil {
		return PayloadInfo{}, false
	}
	if p := a.firstPayload.Load(); p != nil {
		return *p, true
	}
	return PayloadInfo{}, false
}

// dietLabel normalizes a configured tier for reporting: empty means full.
func dietLabel(tier string) string {
	switch {
	case tier == "":
		return "full"
	default:
		return tier
	}
}

// messagesBytes sums the text content of every message (Content or the
// text parts of a ContentParts message; image parts count their alt text
// nowhere — they are not payload text).
func messagesBytes(msgs []Message) int64 {
	var n int64
	for _, m := range msgs {
		if len(m.ContentParts) > 0 {
			for _, p := range m.ContentParts {
				n += int64(len(p.Text))
			}
			continue
		}
		n += int64(len(m.Content))
	}
	return n
}

// jsonMarshalLen returns the marshaled length of v without keeping it.
func jsonMarshalLen(v any) (int, error) {
	b, err := json.Marshal(v)
	return len(b), err
}
