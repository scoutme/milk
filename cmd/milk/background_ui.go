package main

import (
	"fmt"
	"strings"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/textbudget"
)

// renderBackgroundJobDoneBlock renders the transcript block written the
// moment a spawn_background_agent job finishes (repl.go's
// backgroundJobDoneMsg). Until now a finished job's result reached only the
// *model* (drainBackgroundJobs' prompt injection on the next turn) while the
// user saw nothing but a transient toast — the result could pass by
// completely unseen if the model chose not to report it (or a glitched turn
// swallowed the follow-up). This block makes the result itself visible in
// the transcript the instant it exists, independent of what any model does
// with it afterwards.
//
// The result text is capped at backgroundJobResultMaxChars — the exact budget
// the same result gets when injected into the model's prompt (see
// dispatch.go's drainBackgroundJobs), so what you see is what the model got —
// with /bg show <id> as the pointer to the full text when it truncated.
// Issue #162's "lifecycle noise stays out of the transcript" still holds for
// the completion/failure notice itself (that remains a toast, ADR-0048); this
// block is the job's *content*, which is turn output the user asked for.
func renderBackgroundJobDoneBlock(j *local.Job) string {
	var b strings.Builder
	text, status, filesTouched := local.ParseBackgroundResult(j.Result)
	var meta strings.Builder
	if status != "" {
		fmt.Fprintf(&meta, " status=%s", status)
	}
	if filesTouched != "" {
		fmt.Fprintf(&meta, " files_touched=%s", filesTouched)
	}
	switch {
	case j.Err != nil:
		fmt.Fprintf(&b, "\n%s background agent %q failed: %v\n", milkTag(), j.Label, j.Err)
		if strings.TrimSpace(j.Result) == "" {
			return b.String()
		}
		// Failed jobs may still carry partial work (RunBackgroundTask
		// preserves the best-effort answer from the tool trajectory up to
		// the failure) — same wording as drainBackgroundJobs' prompt-side
		// notice, but here with the text itself shown.
		fmt.Fprintf(&b, "%s partial result preserved:\n%s\n", milkTag(),
			textbudget.SummarizeLong(j.Result, backgroundJobResultMaxChars))
	default:
		fmt.Fprintf(&b, "\n%s background agent %q completed%s\n", milkTag(), j.Label, meta.String())
		if strings.TrimSpace(text) == "" {
			fmt.Fprintf(&b, "%s (no result text)\n", milkTag())
		} else {
			fmt.Fprintf(&b, "%s\n", textbudget.SummarizeLong(text, backgroundJobResultMaxChars))
		}
	}
	if len(j.Result) > backgroundJobResultMaxChars && j.ID != "" {
		fmt.Fprintf(&b, "%s full result: /bg show %s\n", milkTag(), j.ID)
	}
	return b.String()
}

// renderBgShowShell prints one background shell's state and current output —
// the shell counterpart of renderBgShow (ADR-0064), the manual "what is it
// doing" path for a shell row (/bg show <shell-id>, TUI and ACP alike). The
// output is the live buffer's snapshot, capped like a job result; it keeps
// accumulating after exit until the shell is reaped (ADR-0063).
func renderBgShowShell(s local.ShellInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s background shell %s (%q) — %s", milkTag(), s.ID, s.Command, s.StatusLabel())
	if s.Dir != "" {
		fmt.Fprintf(&b, " dir=%s", s.Dir)
	}
	b.WriteString("\n")
	out := ""
	if buf, ok := local.ShellLive(s.ID); ok {
		out = buf.Snapshot()
	}
	if strings.TrimSpace(out) == "" {
		fmt.Fprintf(&b, "%s (no output)\n", milkTag())
	} else {
		fmt.Fprintf(&b, "%s\n", textbudget.SummarizeLong(out, backgroundJobResultMaxChars))
	}
	return b.String()
}

// renderShellDoneBlock renders the transcript block written the moment a
// background shell's completion is owed to the model (ADR-0100): an awaited
// shell's exit — whatever its status — or an un-awaited shell that failed.
// Same rule as the prompt-side queue (local.ShellWantsNotice is the single
// predicate), so what the user sees is what the model gets; clean un-awaited
// exits stay toast/panel-only (lifecycle noise, ADR-0048). tail is the
// output captured for the notice — empty string reads the live buffer now
// (the TUI path, where the buffer is certainly still alive) and falls back
// to "(no output)".
func renderShellDoneBlock(info local.ShellInfo, tail string) string {
	if !local.ShellWantsNotice(info) {
		return ""
	}
	var b strings.Builder
	label := "background shell"
	if info.Await {
		label = "awaited background shell"
	}
	fmt.Fprintf(&b, "\n%s %s %s (%q) — %s\n", milkTag(), label, info.ID, info.Command, info.StatusLabel())
	if tail == "" {
		if buf, ok := local.ShellLive(info.ID); ok {
			tail = buf.Snapshot()
		}
	}
	tail = strings.TrimRight(tail, "\n")
	if strings.TrimSpace(tail) == "" {
		fmt.Fprintf(&b, "%s (no output)\n", milkTag())
	} else {
		fmt.Fprintf(&b, "%s\n", textbudget.SummarizeLong(tail, backgroundJobResultMaxChars))
	}
	return b.String()
}

// renderShellNotices turns drained completion events (ADR-0100) into the
// prompt/history prefix next to drainBackgroundJobs' results — the events a
// follow-up turn reports on. Capped to the same budget as job results, so
// one chatty failed watcher can't flood the next turn.
func renderShellNotices(events []local.ShellEvent) string {
	if len(events) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range events {
		what := fmt.Sprintf("background shell %s (%q) — %s", e.Info.ID, e.Info.Command, e.Info.StatusLabel())
		if e.Info.Await {
			what = "awaited " + what
		}
		fmt.Fprintf(&b, "[shell-finished] %s\n", what)
		if tail := strings.TrimRight(e.Tail, "\n"); strings.TrimSpace(tail) != "" {
			fmt.Fprintf(&b, "%s\n", textbudget.SummarizeLong(tail, backgroundJobResultMaxChars))
		}
	}
	return b.String()
}

// renderBgShow prints one job's full, uncapped result — the target the
// truncation hint in renderBackgroundJobDoneBlock points at, and the manual
// "take its output" path for any older job (/bg show <id>, TUI and ACP alike).
// Also lists the job's own status metadata so the result is readable without
// a second /bg list lookup.
func renderBgShow(j local.Job) string {
	var b strings.Builder
	text, status, filesTouched := local.ParseBackgroundResult(j.Result)
	state := string(j.Status)
	if j.TimedOut {
		state = "timed-out"
	}
	fmt.Fprintf(&b, "%s background agent %s (%q) — %s", milkTag(), j.ID, j.Label, state)
	if status != "" {
		fmt.Fprintf(&b, " status=%s", status)
	}
	if filesTouched != "" {
		fmt.Fprintf(&b, " files_touched=%s", filesTouched)
	}
	b.WriteString("\n")
	switch {
	case j.Err != nil:
		fmt.Fprintf(&b, "%s error: %v\n", milkTag(), j.Err)
		if strings.TrimSpace(j.Result) != "" {
			fmt.Fprintf(&b, "%s partial result preserved:\n%s\n", milkTag(), j.Result)
		}
	case strings.TrimSpace(text) == "":
		fmt.Fprintf(&b, "%s (no result text)\n", milkTag())
	default:
		fmt.Fprintf(&b, "%s\n", text)
	}
	return b.String()
}
