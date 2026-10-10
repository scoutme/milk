package main

// History bookkeeping for turns that end in an action instead of words, and
// for events that happen between turns. Without it the next turn's model sees
// an unanswered request (or no trace of work it started) and repeats the action.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/textbudget"
	"github.com/scoutme/milk/internal/workflow"
	"github.com/scoutme/milk/internal/workflow/interp"
)

// actionNotes maps tools whose effect outlives the turn to the sentence stored
// in history when the turn ends without closing text. A tool absent here is
// summarized generically ("bash ×2"); presence also marks the tool as a lasting
// action worth recording when its turn is interrupted.
var actionNotes = map[string]func(c session.TrailCall) string{
	"spawn_background_agent": func(c session.TrailCall) string {
		var a struct {
			Label string `json:"label"`
		}
		_ = json.Unmarshal([]byte(c.Args), &a)
		return fmt.Sprintf("Spawned background agent %q; it runs independently and you will be told when it finishes. Don't spawn it again.", a.Label)
	},
}

const (
	workflowResultMaxChars = 800
	recordedEventsMaxChars = 4000
)

// silentTurnContent describes a turn that produced tool activity but no closing
// text. launched reports whether a start_workflow request was actually handed
// to a host that can run it. Returns "" when there is nothing to say.
func silentTurnContent(res TurnResult, launched bool) string {
	var notes []string
	counts := map[string]int{}
	for _, s := range res.Trail {
		for _, c := range s.Calls {
			if res.WorkflowStart != nil && c.Name == "start_workflow" {
				continue
			}
			if note, ok := actionNotes[c.Name]; ok {
				notes = append(notes, note(c))
				continue
			}
			counts[c.Name]++
		}
	}
	if ws := res.WorkflowStart; ws != nil {
		if launched {
			notes = append(notes, fmt.Sprintf("Started workflow %q in the background. milk runs it independently and reports to the user in the transcript; its result will be noted in the conversation when it finishes. Don't start it again for this request.", ws.Name))
		} else {
			notes = append(notes, fmt.Sprintf("Requested workflow %q, but it was not started: start_workflow is only supported in the TUI.", ws.Name))
		}
	}
	if len(counts) > 0 {
		names := make([]string, 0, len(counts))
		for n := range counts {
			names = append(names, n)
		}
		sort.Strings(names)
		parts := make([]string, len(names))
		for i, n := range names {
			parts[i] = fmt.Sprintf("%s ×%d", n, counts[n])
		}
		notes = append(notes, "Tool activity: "+strings.Join(parts, ", ")+".")
	}
	if len(notes) == 0 {
		return ""
	}
	return strings.Join(notes, " ")
}

// recordSilentTurn stores the assistant half of a turn that ended without
// closing text but did something: a start_workflow launch, a spawned job, or
// any other tool calls. Self-escalation is left to the escalation turn that
// follows it, and a turn with real text already has its assistant message.
// Reports whether a turn was added.
func recordSilentTurn(sess *session.Session, agent session.Agent, agentName string, res TurnResult, launched bool) bool {
	if res.Text != "" || res.EscalationReason != "" {
		return false
	}
	content := silentTurnContent(res, launched)
	if content == "" {
		return false
	}
	sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: agent, AgentName: agentName, Content: content, Trail: res.Trail})
	return true
}

// recordInterruptedTurn keeps a failed or cancelled turn in history only when
// it launched something that outlives the turn. Otherwise nothing is recorded,
// so the user can simply retry (and the repeated-prompt check is not tripped
// by the retry).
func recordInterruptedTurn(sess *session.Session, agent session.Agent, agentName, userContent string, nNotices int, res TurnResult, err error) {
	var notes []string
	for _, s := range res.Trail {
		for _, c := range s.Calls {
			if note, ok := actionNotes[c.Name]; ok {
				notes = append(notes, note(c))
			}
		}
	}
	if len(notes) == 0 {
		return
	}
	cause := "failed: " + err.Error()
	if errors.Is(err, context.Canceled) {
		cause = "interrupted by the user"
	}
	recordUserTurn(sess, agent, agentName, userContent, nNotices)
	sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: agent, AgentName: agentName,
		Content: "Turn " + cause + " before finishing. " + strings.Join(notes, " "), Trail: res.Trail})
}

// pendingEventsBlock renders the session's queued notices as a prompt/history
// prefix, returning it with how many notices it covers.
func pendingEventsBlock(sess *session.Session) (string, int) {
	if len(sess.PendingNotices) == 0 {
		return "", 0
	}
	return "[milk] " + strings.Join(sess.PendingNotices, "\n[milk] ") + "\n\n", len(sess.PendingNotices)
}

// recordUserTurn adds the user's turn and consumes the nNotices notices that
// were already shown to the agent in this turn's prompt.
func recordUserTurn(sess *session.Session, agent session.Agent, agentName, content string, nNotices int) {
	sess.AddTurn(session.Turn{Role: session.RoleUser, Agent: agent, AgentName: agentName, Content: content})
	sess.PendingNotices = sess.PendingNotices[min(nNotices, len(sess.PendingNotices)):]
}

// historyUserContent is what history stores for a user turn: the events that
// reached the agent with it (completed background jobs, workflow notices),
// bounded, followed by the user's own text.
func historyUserContent(events, sessionContent string) string {
	if events == "" {
		return sessionContent
	}
	return textbudget.SummarizeLong(strings.TrimRight(events, "\n"), recordedEventsMaxChars) + "\n\n" + sessionContent
}

// workflowCompletionNotice summarizes how a workflow run ended, for the
// session's next turn. cp may be nil (no checkpoint on disk).
func workflowCompletionNotice(name, task string, runErr error, cp *interp.Checkpoint) string {
	var outcome string
	switch {
	case runErr == nil:
		outcome = "completed"
	case errors.Is(runErr, context.Canceled):
		outcome = "was cancelled (/workflow resume continues it)"
	default:
		outcome = "stopped with an error: " + runErr.Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Workflow %q %s. Task: %s", name, outcome, textbudget.SummarizeLong(task, 200))
	if cp != nil && len(cp.Trace) > 0 {
		stages := make([]string, 0, len(cp.Trace))
		var last string
		for _, e := range cp.Trace {
			s := e.StageID
			if e.Outcome != "" {
				s += " (" + e.Outcome + ")"
			}
			stages = append(stages, s)
			if v, ok := e.Value.(string); ok && strings.TrimSpace(v) != "" {
				last = v
			}
		}
		fmt.Fprintf(&b, "\nStages run: %s.", strings.Join(stages, ", "))
		if last != "" {
			fmt.Fprintf(&b, "\nLast stage output: %s", textbudget.SummarizeLong(strings.TrimSpace(last), workflowResultMaxChars))
		}
	}
	return b.String()
}

// noteWorkflowFinished queues the completion notice for the session's next
// turn and persists it.
func noteWorkflowFinished(sess *session.Session, name, task string, workflowID int, runErr error) {
	var cp *interp.Checkpoint
	if dir, err := session.Dir(); err == nil {
		cp, _ = interp.LoadCheckpoint(workflow.InterpCheckpointPath(dir, sess.ID, workflowID))
	}
	sess.AddNotice(workflowCompletionNotice(name, task, runErr, cp))
	_ = session.Save(sess)
}
