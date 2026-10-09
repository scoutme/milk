package local

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/scoutme/milk/internal/session"
)

// A turn's tool trail is kept with the assistant turn so the agent's next
// turns don't start blind. Only the final text survived before, so a
// follow-up turn re-read the files the previous one had just read. Storage is
// bounded here at capture; what is replayed is decided by ReplayTrails.
const (
	trailArgsMaxChars   = 2000      // one call's arguments, compacted past this
	trailResultMaxBytes = 8000      // one stored tool result
	trailStoreBudget    = 256 << 10 // args+results kept per turn, newest first
	trailTextMaxChars   = 400       // narration written beside a call
	trailClearedArgs    = 200       // arguments of a call whose result was dropped

	// trailRecentTurns is how many of the agent's latest tool-using turns are
	// replayed call-by-call (the rest become a one-line manifest). OpenCode
	// and MiMo-Code protect the two most recent user turns the same way.
	trailRecentTurns = 2
	// trailReplayProtectBytes is how much tool output, newest first, stays
	// verbatim in replayed history (~40K tokens, their protect window).
	trailReplayProtectBytes = 160 << 10

	clearedResultPlaceholder = "[Old tool result content cleared]"
)

// compactArgs keeps JSON arguments valid while shrinking them: any string
// value longer than the share of limit each field can claim is cut to its
// head with an omission note. Non-JSON input is cut as plain text.
func compactArgs(args string, limit int) string {
	if len(args) <= limit {
		return args
	}
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return runeSafeHead(args, limit)
	}
	per := max(limit/max(len(m), 1), 40)
	for k, v := range m {
		if s, ok := v.(string); ok && len(s) > per {
			m[k] = runeSafeHead(s, per) + fmt.Sprintf("…[%d more chars]", len(s)-per)
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func capTrailResult(r string) string {
	r = capToolResult(r, trailResultMaxBytes)
	if len(r) > trailResultMaxBytes*2 {
		r = truncateHeadAndTail(r, trailResultMaxBytes)
	}
	return r
}

// TrailFromMessages extracts the tool activity of the turn that started at
// userMsgIdx: every tool-calling iteration with its results matched by call
// id. Nudges, summaries and the final answer are not part of the trail.
func TrailFromMessages(msgs []Message, userMsgIdx int) []session.TrailStep {
	if userMsgIdx < 0 || userMsgIdx >= len(msgs) {
		return nil
	}
	tail := msgs[userMsgIdx+1:]
	results := map[string]string{}
	for _, m := range tail {
		if m.Role == "tool" && m.ToolCallID != "" {
			results[m.ToolCallID] = m.Content
		}
	}
	var steps []session.TrailStep
	for _, m := range tail {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		step := session.TrailStep{Text: runeSafeHead(m.Content, trailTextMaxChars)}
		for _, tc := range m.ToolCalls {
			step.Calls = append(step.Calls, session.TrailCall{
				ID:     tc.ID,
				Name:   tc.Function.Name,
				Args:   compactArgs(tc.Function.Arguments, trailArgsMaxChars),
				Result: capTrailResult(results[tc.ID]),
			})
		}
		steps = append(steps, step)
	}
	// Keep the stored trail bounded: newest calls keep their results, older
	// ones are cleared and their arguments shrunk.
	cum := 0
	for si := len(steps) - 1; si >= 0; si-- {
		for ci := len(steps[si].Calls) - 1; ci >= 0; ci-- {
			c := &steps[si].Calls[ci]
			if cum+len(c.Args)+len(c.Result) > trailStoreBudget {
				c.Result, c.Cleared = "", true
				c.Args = compactArgs(c.Args, trailClearedArgs)
			}
			cum += len(c.Args) + len(c.Result)
		}
	}
	return steps
}

// TurnTrail returns the tool trail of the most recent Run on this agent.
func (a *Agent) TurnTrail() []session.TrailStep { return a.turnTrail }

// TrailReplay is how one earlier turn's trail is shown to the agent that made
// it: Messages go before the turn's final assistant message; Note is appended
// to that message's text when only a manifest of the activity is kept.
type TrailReplay struct {
	Messages []Message
	Note     string
}

// ReplayTrails decides, for index-aligned per-turn trails (nil for turns
// without one), what to replay. The newest trailRecentTurns trails are
// expanded into assistant tool-call / tool-result messages; within them tool
// output is kept newest-first up to trailReplayProtectBytes and older results
// become a placeholder (the call and its short arguments stay, so the agent
// still knows what it ran). Older turns shrink to a one-line manifest.
func ReplayTrails(trails [][]session.TrailStep) []TrailReplay {
	out := make([]TrailReplay, len(trails))
	var withTrail []int
	for i, t := range trails {
		if len(t) > 0 {
			withTrail = append(withTrail, i)
		}
	}
	cut := max(len(withTrail)-trailRecentTurns, 0)
	for _, i := range withTrail[:cut] {
		out[i].Note = TrailManifest(trails[i])
	}
	cum := 0
	for k := len(withTrail) - 1; k >= cut; k-- {
		i := withTrail[k]
		keep := make([][]bool, len(trails[i]))
		for si := len(trails[i]) - 1; si >= 0; si-- {
			keep[si] = make([]bool, len(trails[i][si].Calls))
			for ci := len(trails[i][si].Calls) - 1; ci >= 0; ci-- {
				c := trails[i][si].Calls[ci]
				if !c.Cleared && cum+len(c.Result) <= trailReplayProtectBytes {
					keep[si][ci] = true
					cum += len(c.Result)
				}
			}
		}
		out[i].Messages = trailMessages(trails[i], keep)
	}
	return out
}

func trailMessages(steps []session.TrailStep, keep [][]bool) []Message {
	var msgs []Message
	for si, step := range steps {
		am := Message{Role: "assistant", Content: step.Text}
		var tools []Message
		for ci, c := range step.Calls {
			id := c.ID
			if id == "" {
				id = fmt.Sprintf("trail_%d_%d", si, ci)
			}
			args, result := c.Args, c.Result
			if !keep[si][ci] || result == "" {
				args, result = compactArgs(args, trailClearedArgs), clearedResultPlaceholder
			}
			if args == "" {
				args = "{}"
			}
			am.ToolCalls = append(am.ToolCalls, toolCall{ID: id, Type: "function", Function: toolCallFunction{Name: c.Name, Arguments: args}})
			tools = append(tools, Message{Role: "tool", ToolCallID: id, Content: result})
		}
		msgs = append(msgs, am)
		msgs = append(msgs, tools...)
	}
	return msgs
}

// TrailManifest is the one-line stand-in for an old turn's tool activity:
// call counts per tool and the files it read or changed.
func TrailManifest(steps []session.TrailStep) string {
	counts := map[string]int{}
	var calls []namedArgs
	for _, s := range steps {
		for _, c := range s.Calls {
			counts[c.Name]++
			calls = append(calls, namedArgs{c.Name, c.Args})
		}
	}
	if len(counts) == 0 {
		return ""
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] != counts[names[j]] {
			return counts[names[i]] > counts[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = fmt.Sprintf("%s ×%d", n, counts[n])
	}
	note := "\n\n[Tool activity in that turn: " + strings.Join(parts, ", ")
	if files := touchedFiles(calls, 12); len(files) > 0 {
		var fs []string
		for _, f := range files {
			fs = append(fs, f.path+" ("+f.action+")")
		}
		note += "; files: " + strings.Join(fs, ", ")
	}
	return note + "]"
}
