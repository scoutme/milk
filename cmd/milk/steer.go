package main

// Mid-turn steering (issue #441): input submitted while a turn runs can be
// delivered into that very turn at its next model-request boundary — the
// Claude Code-style "queue while it works" flow — instead of only waiting
// for the next turn. This file holds the host half: which queued entries may
// steer, and how the running turn pulls them. The agent half lives in
// internal/agent/local (WithSteerDrain + runToolLoop's boundary drain): the
// loop pops through the closure wired here and appends each string as a user
// message; everything the flush path must still own stays queued.

import (
	"strings"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/shelldetect"
)

// steerItem is one queued input cleared for delivery into the running turn.
type steerItem struct {
	text string
	// label renders before the text in the transcript echo — the entry's
	// queue of origin ([queued] / [telegram]), so a steered line looks
	// exactly like its flush would have.
	label string
	// recordHistory: remote inputs enter the input histories at delivery
	// time (they had no queue moment to record at); queued entries were
	// recorded at queue time (issue #368) and must not double-append.
	recordHistory bool
}

// steerDeliveredMsg carries one drained item from the queue pop (the agent
// goroutine, inside the tool loop's boundary drain) to Update, which owns
// the transcript, the loop detector and the input histories.
type steerDeliveredMsg struct{ item steerItem }

// steerablePrompt reports whether queued input may be injected into the
// RUNNING turn. The rule mirrors submitInput exactly: steer only what the
// flush path would dispatch as a plain prompt. Anything the flush must own —
// staged attachments, a slash command, bang direct-exec, direct-bash shell
// text — keeps its old next-turn semantics, because steering bypasses
// submitInput entirely (no command execution, no PTY, no attachment staging).
// Paste provenance is honored like submitInput's: a pasted slash-looking
// line is inert prompt text there, so it steers here too.
func steerablePrompt(text string, leadingPasted bool, attachments int, cfg config.Config) bool {
	if attachments > 0 || strings.TrimSpace(text) == "" {
		return false
	}
	if !leadingPasted {
		if _, _, found := extractSlashCommand(text); found {
			return false
		}
		if _, ok := stripBangPrefix(text); ok {
			return false
		}
		if cfg.DirectBash {
			if _, ok := shelldetect.IsShellCommand(text); ok {
				return false
			}
		}
	}
	return true
}

// popSteer drains queued input for delivery into the RUNNING turn. Order is
// global-FIFO across both queues: the steerable prefix of queued prompts
// first, and remote (Telegram) inputs only once the prompt queue is empty —
// the flush chain (handleAgentDone) always runs queued prompts before remote
// ones, so a remote entry must never jump ahead of a prompt that is still
// waiting. Within a queue the drain is FIFO and STOPS at the first entry that
// must keep the flush path (order cannot skip: a queued slash command stays
// next in line for the next-turn flush, so nothing behind it may jump ahead
// of it either). Thread-safe: the agent's goroutine pops here while the UI
// goroutine queues, drops and reviews — steerMu guards both slices everywhere
// they are touched. The caller (the wiring closure) owns echoing each
// delivered item.
func (st *interactiveState) popSteer() []steerItem {
	st.steerMu.Lock()
	defer st.steerMu.Unlock()

	var items []steerItem
	n := 0
	for _, q := range st.queuedPrompts {
		if !steerablePrompt(q.text, q.leadingPasted, len(q.attachments), st.cfg) {
			break
		}
		items = append(items, steerItem{text: q.text, label: dim("[queued]") + " "})
		n++
	}
	if n > 0 {
		st.queuedPrompts = st.queuedPrompts[n:]
	}

	// Remote inputs wait behind whatever the prompt queue still holds —
	// blocked entries included — to preserve the flush chain's global order.
	if len(st.queuedPrompts) == 0 {
		m := 0
		for _, text := range st.pendingRemoteInputs {
			if !steerablePrompt(text, false, 0, st.cfg) {
				break
			}
			items = append(items, steerItem{
				text:          text,
				label:         dim("[telegram]") + " ",
				recordHistory: true, // remote input never passed through queuePrompt's #368 recording
			})
			m++
		}
		if m > 0 {
			st.pendingRemoteInputs = st.pendingRemoteInputs[m:]
		}
	}
	return items
}
