package main

import (
	"fmt"
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/tasks"
	"github.com/scoutme/milk/internal/workflow"
)

// Panel content events (issue #347): the notification layer for the side
// panels that lacked one (git, project, tasks, memory — the background
// panel already announces its lifecycle, workflows now do too). Two
// deliberately different semantics live side by side:
//
//   - Notifications are per-event and granular: every *observed* domain
//     occurrence gets its own toast — a dirty-count change, a branch
//     switch, a task status flip, a changeset change — whether the panel
//     is open or closed. Nothing here is edge-deduplicated: like the
//     background panel's spawn/done/fail toasts, a repeat occurrence
//     toasts again. The auto-open / master-switch / tab-slot gates never
//     swallow a toast.
//   - Layout is the edge-only layer: auto-open fires on content arriving
//     from a clean previous state (clean→dirty for git, empty→non-empty
//     for the project changeset), and a manual close's veto is released
//     by the domain's "no contents" state — a clean working tree for git,
//     an empty changeset for the project panel (see panelContentFinalized
//     and resetManualOverrideIfFinalized). That is what makes "close" mean
//     "stay closed over this dirt" instead of "stay closed for a second".
//
// The event source is the git status probe that feeds the session
// changeset (changeset.go — one bounded `git status` per 3s panel poll,
// off the UI goroutine, armed for everyone since this issue) plus the
// session file-trace wakeup for agent-caused writes in non-git workspaces.
// The first probe/sync only establishes a baseline: pre-existing state at
// startup is not news.

const (
	// fileTracePokeDelay coalesces a tool burst: many traced writes in a
	// row collapse into one changeset sync (and thus one event), the same
	// way the changeset probe coalesces repo-side churn onto its tick.
	fileTracePokeDelay = 250 * time.Millisecond
)

// fileTraceChangedMsg is sent by the session file-change listener (wired
// in runREPL) whenever a tool records or drops a traced path — the
// instant, zero-poll wakeup for agent-caused writes (issue #347).
type fileTraceChangedMsg struct{ sessID string }

// fileTracePokeMsg is the coalesced follow-up of a fileTraceChangedMsg
// burst (see fileTracePokeDelay).
type fileTracePokeMsg struct{}

// applyGitStatus folds one `git status` probe (branch + whole-repo dirty
// count) into the panel event layer and returns the data refresh for a git
// panel the event just auto-opened (nil otherwise). The first probe only
// establishes the baseline: dirt that pre-existed at startup is state, not
// an event — the *transition* into dirt from a clean previous state is
// what toasts and may open the panel (issue #347).
func (m *model) applyGitStatus(branch string, dirty int) tea.Cmd {
	first := !m.gitStatusSeen
	prevDirty, prevBranch := m.gitStatusDirty, m.gitStatusBranch
	m.gitStatusSeen, m.gitStatusOK = true, true
	m.gitStatusDirty, m.gitStatusBranch = dirty, branch
	if first || (dirty == prevDirty && branch == prevBranch) {
		// Even a no-change observation finalizes: a manual close made
		// while the state was unknown is released the moment clean is
		// observed (git-world "no contents").
		m.resetManualOverrideIfFinalized(regionGit)
		return nil
	}
	// Recency (ADR-0089): the status changed, so the git panel's content
	// did too — regardless of what the layout layers decide.
	m.stampPanelActivity(regionGit)
	// Layout layer — edges only: content from a clean previous state, or
	// a branch switch, may move the layout; a dirty-count drift while
	// already dirty may not.
	edge := branch != prevBranch || (prevDirty == 0 && dirty > 0)
	var cmd tea.Cmd
	if edge {
		wasOpen := m.panelGit
		m.autoOpenPanel(regionGit)
		if !wasOpen && m.panelGit && m.st != nil {
			cmd = refreshGitCmd(m.st.cwd)
		}
	}
	// Notification layer: every occurrence, panel state irrelevant.
	if branch != prevBranch {
		m.notify("git: branch switched to "+branch, "/panel git")
	}
	if dirty != prevDirty {
		if dirty == 0 {
			m.notify("git: working tree clean", "/panel git")
		} else {
			m.notify(fmt.Sprintf("git: working tree has %d change(s)", dirty), "/panel git")
		}
	}
	// Auto-open first, then the veto release (ADR-0059's ordering): clean
	// is git-world "no contents", so a manual close sticks exactly while
	// dirt is live and the next clean→dirty edge opens the panel again.
	m.resetManualOverrideIfFinalized(regionGit)
	return cmd
}

// taskEventsToast is the tasks panel's per-event notification layer
// (issue #347): the store's SetOnChange fires per mutation and each
// mutation becomes its domain toast — created, status flipped, removed —
// plus the "all done" milestone. The first observation is the baseline
// (tasks restored at startup are state, not events).
func (m *model) taskEventsToast() {
	if m.taskStore == nil {
		return
	}
	all, err := m.taskStore.List(tasks.ListOpts{IncludeGlobal: true})
	if err != nil {
		return
	}
	prev := m.taskEventPrev
	cur := make(map[string]tasks.Task, len(all))
	for _, t := range all {
		cur[t.ID] = t
	}
	m.taskEventPrev = cur
	if prev == nil {
		return // baseline
	}
	prevActive, active := 0, 0
	for _, t := range prev {
		if t.Status != tasks.StatusDone {
			prevActive++
		}
	}
	for _, t := range all {
		if t.Status != tasks.StatusDone {
			active++
		}
	}
	for _, t := range all {
		was, ok := prev[t.ID]
		switch {
		case !ok:
			m.notify("task created: "+quoteTaskTitle(t.Title), "/panel tasks")
		case was.Status != t.Status:
			m.notify(fmt.Sprintf("task %s: %s", quoteTaskTitle(t.Title), t.Status), "/panel tasks")
		}
	}
	if len(prev) > len(cur) {
		var gone []string
		for id, t := range prev {
			if _, ok := cur[id]; !ok {
				gone = append(gone, quoteTaskTitle(t.Title))
			}
		}
		sort.Strings(gone)
		for _, title := range gone {
			m.notify("task removed: "+title, "/panel tasks")
		}
	}
	if prevActive > 0 && active == 0 && len(all) > 0 {
		m.notify("all tasks done", "/panel tasks")
	}
}

// taskEventSnapshot reads the store's current id → task map — the baseline
// the per-event diff runs against (restored startup tasks are state, not
// events). Wired wherever a fresh task store replaces the old one.
func taskEventSnapshot(store *tasks.Store) map[string]tasks.Task {
	if store == nil {
		return nil
	}
	all, err := store.List(tasks.ListOpts{IncludeGlobal: true})
	if err != nil {
		return nil
	}
	out := make(map[string]tasks.Task, len(all))
	for _, t := range all {
		out[t.ID] = t
	}
	return out
}

// quoteTaskTitle bounds a task title for toast use and quotes it.
func quoteTaskTitle(title string) string {
	const maxTitle = 32
	r := []rune(title)
	if len(r) > maxTitle {
		title = string(r[:maxTitle-1]) + "…"
	}
	return fmt.Sprintf("%q", title)
}

// memoryPerceptCount is the store's total percept count (session + global
// scope) — what "memory content arrived/cleared" means for events.
func (m *model) memoryPerceptCount() int {
	if m.mem == nil {
		return 0
	}
	return len(m.mem.List(memory.ListOpts{Scope: "session"})) +
		len(m.mem.List(memory.ListOpts{Scope: "global"}))
}

// memoryPerceptToast is the memory panel's per-event notification layer
// (issue #347): each observed percept-count change is an event. A count
// that did not move (context bricks redrawing) is not — only percepts are
// the store's content lifecycle.
func (m *model) memoryPerceptToast(n int) {
	if n == m.memoryPerceptN {
		return
	}
	m.memoryPerceptN = n
	if n == 0 {
		m.notify("memory: no percepts left", "/panel memory")
		return
	}
	m.notify(fmt.Sprintf("memory: %d percept(s)", n), "/panel memory")
}

// workflowEventToast announces one workflow lifecycle occurrence (issue
// #347's per-event layer for the workflow panel). name is the display name
// of the run the event belongs to.
func (m *model) workflowEventToast(text, name string) {
	m.notify(fmt.Sprintf("workflow %q %s", name, text), "/panel workflow")
}

// workflowName reads the current run's display name ("" outside a run).
func workflowName(st *workflow.State) string {
	if st == nil {
		return ""
	}
	return st.WorkflowName
}
