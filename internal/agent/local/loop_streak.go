package local

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"go.opentelemetry.io/otel/attribute"

	"github.com/scoutme/milk/internal/obs"
)

// Several of the thresholds and detectors in this file are similarly scoped
// to loop-detection mechanisms in MiMo-Code, compared directly during a
// 2026-09-29 prompt/context-management review — see
// docs/prompt-context-management-review.md for the full comparison and the
// reasoning behind each value. These are independent implementations that
// converged on comparable numbers/structure, not ports of MiMo-Code's code.

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
// Point 4 (the escalate hint) is deliberately on the strong tier only, not
// the mild one above — a single nudge is often enough to self-correct, and
// suggesting escalation on the first, often-harmless, recovery would be
// premature.
const recoveryNudgeStrong = `<system-reminder>
WARNING: You are STILL stuck in a loop after a previous recovery attempt.
You MUST:
1. Abandon your current approach entirely.
2. State what you were trying and why it failed.
3. Ask the user for guidance instead of continuing.
4. If you cannot make progress, call escalate(reason) instead of trying again.
If you repeat the same action again the session will be terminated.
</system-reminder>`

// recoveryDuplicateToolMild is injected when the model repeats a tool call
// it already executed with the same arguments — similarly to MiMo-Code's
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
4. If you cannot make progress, call escalate(reason) instead of trying again.
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

// maxIterSummaryReminder is injected in place of the user's turn on the final
// allowed tool-loop iteration, with tools disabled for that call. It forces
// the model to produce a real closing summary in its own words instead of
// silently exhausting the iteration budget and falling through to a
// mechanically-assembled tool-trail dump — similarly to MiMo-Code's
// max-steps behavior (force a text-only wrap-up rather than a bare cutoff).
const maxIterSummaryReminder = `<system-reminder>
MAXIMUM TOOL ITERATIONS REACHED for this turn. Tools are disabled for this
response — you cannot call any more tools. Respond now with text only:
1. What you accomplished this turn.
2. What remains to be done.
3. Any blocker or decision you need from the user.
This overrides all other instructions for this response.
</system-reminder>`

// recoveryNgramReplan is injected on the second n-gram detection.
const recoveryNgramReplan = `<system-reminder>
CRITICAL REPETITION: You are STILL repeating phrases after a recovery attempt.
You MUST completely replan before continuing:
1. Abandon your current approach entirely — it is stuck in repetition
2. Write out a NEW plan with different steps and a different strategy
3. State what you were trying to do, why it failed, and how your new plan differs
4. If you cannot make progress, call escalate(reason) instead of trying again
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

// loopRecoveryAction is the shared escalation mechanics for every intra-turn
// loop detector: crop the looping tail from context, then either inject a
// nudge (mild on the first attempt, strong from the second) or, once
// recoveryCount exceeds maxRecovery, terminate the turn with a diagnostic
// summary in place of a real response.
//
// Consolidating this in one place (rather than each detector re-implementing
// crop+nudge+terminate independently) removes the risk of detectors drifting
// out of sync — see the ngramRecoveryCount history in the caller for a case
// where that already happened.
func (a *Agent) loopRecoveryAction(ctx context.Context, msgs []Message, userMsgIdx int, recoveryCount, maxRecovery int, mildNudge, strongNudge, terminateReason, detectorName, reasoningText, sessionID string, totalRecoveryCount, cropGroups int) (newMsgs []Message, terminated, escalate bool) {
	role := agentRoleForMetrics(a.escalationName)
	// Escalating is strictly better than either continuing to nudge an
	// already-struggling model or giving up on the turn entirely, so this
	// check runs before (and takes priority over) the max-recovery-exceeded
	// terminate check below — the two conditions can coincide (e.g. one
	// detector's own recoveryCount is still low, but the turn's aggregate
	// across all detector types already crossed the escalate threshold).
	if a.escalateAfterRecoveries > 0 && a.canEscalate() && totalRecoveryCount >= a.escalateAfterRecoveries {
		a.logWarn(detectorName+": recovery threshold reached, escalating instead of nudging",
			append([]any{"model", a.model, "agent", role, "total_recoveries", totalRecoveryCount}, sessionLogAttrs(sessionID)...)...)
		obs.Inc(ctx, inferenceScope, "milk.loop.recovery",
			attribute.String("model", a.model),
			attribute.String("agent", role),
			attribute.String("detector", detectorName),
			attribute.String("outcome", "escalated"),
		)
		return msgs, false, true
	}
	if recoveryCount > maxRecovery {
		a.logWarn(detectorName+": max recovery exceeded, terminating turn",
			append([]any{"model", a.model, "agent", role}, sessionLogAttrs(sessionID)...)...)
		obs.Inc(ctx, inferenceScope, "milk.loop.recovery",
			attribute.String("model", a.model),
			attribute.String("agent", role),
			attribute.String("detector", detectorName),
			attribute.String("outcome", "terminated"),
		)
		loopMsg := "[turn terminated: " + terminateReason + "]"
		resp := summarizeToolTrail(msgs, loopMsg)
		if a.onResponseSegment != nil && resp != "" {
			a.onResponseSegment(resp)
		}
		msgs = append(msgs, Message{Role: "assistant", Content: resp, ReasoningContent: reasoningText})
		return msgs, true, false
	}
	cropped := cropLoopingMessages(msgs, userMsgIdx, cropGroups)
	if len(cropped) < len(msgs) {
		a.logWarn(detectorName+": cropped looping messages",
			append([]any{"model", a.model, "agent", role, "before", len(msgs), "after", len(cropped)}, sessionLogAttrs(sessionID)...)...)
		msgs = cropped
	}
	nudge := mildNudge
	if recoveryCount >= 2 {
		nudge = strongNudge
	}
	a.logWarn(detectorName+" detected, injecting recovery nudge",
		append([]any{"model", a.model, "agent", role, "recovery", recoveryCount}, sessionLogAttrs(sessionID)...)...)
	obs.Inc(ctx, inferenceScope, "milk.loop.recovery",
		attribute.String("model", a.model),
		attribute.String("agent", role),
		attribute.String("detector", detectorName),
		attribute.String("outcome", "recovered"),
	)
	msgs = append(msgs, Message{Role: "user", Content: nudge})
	return msgs, false, false
}

// cropLoopingMessages removes the last `groups` tool-calling iterations
// (an assistant message with tool calls plus its tool results) from the tail
// of msgs, never reaching the user message that started the turn (startIdx).
//
// Callers pass exactly the number of iterations the detector saw repeating
// (a streak's trigger count), or 0 when the repetition is not in msgs at all
// (a duplicate call is detected before it is appended; an n-gram loop is in a
// stream that was cut). Cropping more than the loop itself deletes real
// progress and forces the model to rediscover it: a single repeated `go build`
// once wiped ~80K tokens of a turn's reads this way.
func cropLoopingMessages(msgs []Message, startIdx, groups int) []Message {
	cropTo := len(msgs)
	for ; groups > 0; groups-- {
		i := cropTo
		for i > startIdx+1 && msgs[i-1].Role == "tool" {
			i--
		}
		if i <= startIdx+1 || msgs[i-1].Role != "assistant" || len(msgs[i-1].ToolCalls) == 0 {
			break
		}
		cropTo = i - 1
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

// duplicateToolMaxRecovery is the number of consecutive all-duplicate tool
// batches allowed before terminating the turn. Separate from textLoopMaxRecovery
// because duplicate tool calls are a weaker signal — re-reading a file after
// an edit (read-edit-verify pattern) is legitimate and should not terminate
// after just 3 repetitions. MiMo-Code draws a similar distinction between
// text-loop (strong signal, low threshold) and tool-duplicate (weaker
// signal, higher threshold).
const duplicateToolMaxRecovery = 5

// ngramMaxRecovery is the number of n-gram repetition detections allowed
// before terminating the turn. Sized similarly to MiMo-Code's
// TEXT_NGRAM_MAX_RECOVERY.
const ngramMaxRecovery = 2

// doomLoopThreshold is the number of *consecutive* iterations issuing the
// exact same tool-call batch before the hard doom-loop gate fires (see
// runToolLoop's use of toolCallBatchSignature). Similarly named and scoped
// to MiMo-Code's DOOM_LOOP_THRESHOLD. Deliberately separate from duplicateToolMaxRecovery
// above: that detector nudges on any repeat of a call seen anywhere earlier
// in the turn (a soft, self-recovery signal); this one requires the calls to
// be back-to-back and, once reached, treats it as a safety event needing
// confirmation rather than another nudge attempt.
const doomLoopThreshold = 3

// toolCallBatchSignature returns a signature for a whole iteration's tool
// calls (order-sensitive, exact-match) so two iterations can be compared for
// an identical repeat. Empty for an iteration with no tool calls.
func toolCallBatchSignature(toolCalls []toolCall) string {
	if len(toolCalls) == 0 {
		return ""
	}
	var b strings.Builder
	for i, tc := range toolCalls {
		if i > 0 {
			b.WriteByte('\x01')
		}
		b.WriteString(tc.Function.Name)
		b.WriteByte('\x00')
		b.WriteString(tc.Function.Arguments)
	}
	return b.String()
}

// normalizeForTextLoop lowercases, collapses whitespace, strips leading
// phrases, and truncates — similarly to MiMo-Code's normalizeForLoopDetection.
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
