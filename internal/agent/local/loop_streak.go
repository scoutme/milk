package local

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// loopStreakTracker detects when the model repeats the same reasoning or
// tool-call pattern across consecutive assistant steps. mimo-v2.5 and other
// reasoning models can get stuck producing near-identical reasoning that
// drives slightly-varying tool calls — exact duplicate detection misses this.
//
// The tracker works in two tiers:
//  1. Reasoning-hash: if the assistant produced reasoning content, SHA-256
//     the normalised text. This is the primary signal — even when tool args
//     drift, the reasoning hash stays stable.
//  2. Tool-signature fallback: when there is no reasoning, build a
//     deterministic string from the tool calls (name + sorted-key JSON of
//     arguments).
//
// When streakTriggerCount consecutive steps share the same key the tracker
// reports a streak. The caller can then inject a recovery nudge or crop the
// looping messages from context.
type loopStreakTracker struct {
	recentKeys []string
}

const (
	streakTriggerCount = 3
	streakMaxSpan      = 16 // max messages to consider for cropping
)

// recoveryNudgeMild is injected as a user message when a streak is first
// detected. It tells the model to change strategy.
const recoveryNudgeMild = `<system-reminder>
Your last few steps have been identical — you appear to be repeating the same
action without making progress. Stop and reconsider: the current approach is
not working. Try a different strategy, use a different tool, or if you are
blocked, explain the blocker to the user instead of repeating the same step.
</system-reminder>`

// recoveryNudgeStrong is injected on the second consecutive streak detection.
const recoveryNudgeStrong = `<system-reminder>
WARNING: You are STILL stuck in a loop after a previous recovery attempt.
You MUST:
1. Abandon your current approach entirely.
2. State what you were trying and why it failed.
3. Ask the user for guidance instead of continuing.
If you repeat the same action again the session will be terminated.
</system-reminder>`

// stepKeyFromIteration computes a deterministic key for the current tool-call
// iteration. It prefers the reasoning content hash (the strongest loop signal
// for reasoning models like mimo-v2.5); falls back to a tool-call signature.
func stepKeyFromIteration(reasoningText string, toolCalls []toolCall) string {
	if reasoningText != "" {
		return "reason:" + normalisedHash(reasoningText)
	}
	var parts []string
	for _, tc := range toolCalls {
		parts = append(parts, "tool:"+tc.Function.Name+":"+stableStringify(tc.Function.Arguments))
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return "tool:" + strings.Join(parts, "|")
}

// recordStep appends the key to the recent buffer and returns true if the
// last streakTriggerCount keys are all identical (non-empty).
func (t *loopStreakTracker) recordStep(key string) bool {
	if key == "" {
		return false
	}
	t.recentKeys = append(t.recentKeys, key)
	if len(t.recentKeys) > streakTriggerCount {
		t.recentKeys = t.recentKeys[len(t.recentKeys)-streakTriggerCount:]
	}
	if len(t.recentKeys) < streakTriggerCount {
		return false
	}
	for _, k := range t.recentKeys {
		if k != t.recentKeys[0] {
			return false
		}
	}
	return true
}

// reset clears the tracker state (e.g. after a recovery nudge is injected).
func (t *loopStreakTracker) reset() {
	t.recentKeys = nil
}

// recoveryCount tracks how many recovery nudges have been injected so the
// caller can escalate from mild to strong.
type streakState struct {
	tracker       loopStreakTracker
	recoveryCount int
}

// cropLoopingMessages removes consecutive assistant+tool-result message
// groups from the tail of msgs that match the given step key, preserving the
// user message that started the turn (at startIdx). Returns the cropped
// slice.
func cropLoopingMessages(msgs []Message, startIdx int) []Message {
	// Walk backwards from the end, removing assistant messages and their
	// trailing tool results as long as they belong to the looping streak.
	// We keep at least the system prompt (index 0), the original user
	// message (startIdx), and any pre-loop history.
	cropTo := len(msgs)
	for cropTo > startIdx+1 {
		// Check if the message at cropTo-1 is a tool result or assistant msg
		// that's part of the loop.
		prev := msgs[cropTo-1]
		if prev.Role == "tool" {
			cropTo--
			continue
		}
		if prev.Role == "assistant" && len(prev.ToolCalls) > 0 {
			cropTo--
			continue
		}
		break
	}
	if cropTo == len(msgs) {
		return msgs // nothing was cropped
	}
	return msgs[:cropTo]
}

// normalisedHash returns the first 16 hex chars of SHA-256 of the normalised
// text (trim, lowercase, collapse whitespace).
func normalisedHash(text string) string {
	text = strings.TrimSpace(text)
	text = strings.ToLower(text)
	// Collapse whitespace.
	var b strings.Builder
	prevSpace := false
	for _, r := range text {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !prevSpace {
				b.WriteRune(' ')
				prevSpace = true
			}
		} else {
			b.WriteRune(r)
			prevSpace = false
		}
	}
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:8]) // 16 hex chars
}

// stableStringify produces a deterministic JSON string for a JSON arguments
// string by unmarshalling and re-marshalling with sorted keys.
func stableStringify(args string) string {
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return args // not valid JSON; use as-is
	}
	return marshalSorted(v)
}

func marshalSorted(v any) string {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kj, _ := json.Marshal(k)
			b.Write(kj)
			b.WriteByte(':')
			b.WriteString(marshalSorted(val[k]))
		}
		b.WriteByte('}')
		return b.String()
	case []any:
		var b strings.Builder
		b.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(marshalSorted(item))
		}
		b.WriteByte(']')
		return b.String()
	default:
		j, _ := json.Marshal(val)
		return string(j)
	}
}
