package local

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
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

// recoveryDuplicateToolMild is injected when the model repeats a tool call
// it already executed with the same arguments.  Matching MiMo-Code's
// repeated-step nudge approach: nudge first, don't terminate.
const recoveryDuplicateToolMild = `<system-reminder>
Your last step repeated a tool call you already executed with the same arguments.
You appear to be repeating the same action without making progress. Stop and
reconsider: the current approach is not working. Try a different strategy, use a
different tool, or if you are blocked, explain the blocker to the user instead
of repeating the same step again.
</system-reminder>`

// recoveryDuplicateToolStrong is injected on the second duplicate detection.
const recoveryDuplicateToolStrong = `<system-reminder>
WARNING: You are STILL repeating the same tool call after a previous recovery
attempt. You MUST:
1. Abandon your current approach entirely.
2. State what you were trying and why it failed.
3. Ask the user for guidance instead of continuing.
If you repeat the same action again the session will be terminated.
</system-reminder>`

// recoveryNgramRemind is injected when streaming n-gram detection catches
// periodic reasoning repetition.  The wording emphasises varying output
// rather than changing tools, since the loop is in the reasoning, not the
// tool calls.
const recoveryNgramRemind = `<system-reminder>
REPETITION DETECTED: Your recent reasoning contains repeated phrases.
STOP repeating yourself and retry with a different approach:
- Vary your wording and reasoning — do not reuse the same phrases
- If you were about to call a tool, try a different tool or different arguments
- If you are blocked, explain what is blocking you instead of looping
Do NOT output the same phrases again.
</system-reminder>`

// recoveryNgramReplan is injected on the second n-gram detection.
const recoveryNgramReplan = `<system-reminder>
CRITICAL REPETITION: You are STILL repeating phrases after a recovery attempt.
You MUST completely replan before continuing:
1. Abandon your current approach entirely — it is stuck in repetition
2. Write out a NEW plan with different steps and a different strategy
3. State what you were trying to do, why it failed, and how your new plan differs
Do NOT continue the same line of reasoning or reuse the same wording.
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

// leadingPhraseRe strips common reasoning-model opening phrases so that
// "Let me check the file" and "I'll check the file" hash identically.
var leadingPhraseRe = regexp.MustCompile(`(?i)^(let me |i'll |i will |let's )`)

// streakHashPrefixLen is the maximum number of normalised characters fed to
// the hash.  The full reasoning text of a stuck model can be 200K+ chars with
// each loop iteration adding slightly different details — hashing the whole
// thing means every iteration gets a unique key.  Truncating to a prefix
// catches the pattern because the first N chars are identical across loop
// cycles ("I'm done. Let me write up my findings. Actually, wait…").
const streakHashPrefixLen = 500

// normalisedHash returns the first 16 hex chars of SHA-256 of the normalised
// text (trim, lowercase, collapse whitespace, strip leading phrases, truncate).
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
	normalised := b.String()
	normalised = leadingPhraseRe.ReplaceAllString(normalised, "")
	if len(normalised) > streakHashPrefixLen {
		normalised = normalised[:streakHashPrefixLen]
	}
	h := sha256.Sum256([]byte(normalised))
	return hex.EncodeToString(h[:8]) // 16 hex chars
}

// textLoopTracker detects when the model produces the same output text
// (not reasoning) across consecutive steps.  This catches a different
// failure mode than the reasoning streak tracker: the model repeating its
// visible answer rather than its thinking.
type textLoopTracker struct {
	recentTexts []string
}

const (
	textLoopTriggerCount = 3
	textLoopMaxRecovery  = 2
	textLoopPrefixLen    = 200
)

// normalizeForTextLoop lowercases, collapses whitespace, strips leading
// phrases, and truncates — matching MiMo-Code's normalizeForLoopDetection.
func normalizeForTextLoop(text string) string {
	text = strings.TrimSpace(text)
	text = strings.ToLower(text)
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
	normalised := b.String()
	normalised = leadingPhraseRe.ReplaceAllString(normalised, "")
	if len(normalised) > textLoopPrefixLen {
		normalised = normalised[:textLoopPrefixLen]
	}
	return normalised
}

// recordStep appends the normalised text and returns true if the last
// textLoopTriggerCount entries are identical (non-empty).
func (t *textLoopTracker) recordStep(text string) bool {
	normalised := normalizeForTextLoop(text)
	if normalised == "" {
		return false
	}
	t.recentTexts = append(t.recentTexts, normalised)
	if len(t.recentTexts) > textLoopTriggerCount {
		t.recentTexts = t.recentTexts[len(t.recentTexts)-textLoopTriggerCount:]
	}
	if len(t.recentTexts) < textLoopTriggerCount {
		return false
	}
	for _, s := range t.recentTexts {
		if s != t.recentTexts[0] {
			return false
		}
	}
	return true
}

func (t *textLoopTracker) reset() {
	t.recentTexts = nil
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
