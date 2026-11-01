package main

import (
	"context"
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/session"
)

// Session changeset for the files panel (issue #331): two sources merged into
// one cwd-relative map of path → change kind —
//
//  1. the session's traced tool changes (internal/session, recorded at the
//     tool dispatch boundary; the only source in non-git workspaces), and
//  2. `git status --porcelain` (real per-file status, incl. user-side edits),
//     computed off the UI goroutine on the panel's existing 3s poll and
//     skipped entirely for directories known to be outside a repo.
//
// Nothing here runs on the render path: results land on the model, and only
// rebuildProjectTree consults the merged map.

const (
	// changesetGitTimeout bounds each git probe (off the UI goroutine).
	changesetGitTimeout = 3 * time.Second
	// changesetMaxStatusLines caps the parsed porcelain lines (bounded work
	// and memory even on pathological repositories).
	changesetMaxStatusLines = 4096
)

// changesetMsg carries one porcelain snapshot for the files panel plus the
// status header — branch and whole-repo dirty count are the git panel's
// content-event source even while the panel is closed (issue #347).
type changesetMsg struct {
	files  map[string]session.ChangeKind // cwd-relative; empty non-nil when clean
	branch string                        // "##" header branch (whole repo)
	dirty  int                           // porcelain entry count (whole repo)
	gitOK  bool                          // status ran successfully — files is authoritative
	noRepo bool                          // definitively no repo / no git: stop refreshing this cwd
}

// gitTopLevel caches cwd's repository toplevel (positive cache plus the
// definitive-negative "no repo here" entry the model's changesetNoGit flag
// consumes). Only *definitive* probe outcomes are cached (gitErrDefinitive):
// a transient failure — the 3s timeout mid-checkout, lock contention — must
// not be mistaken for "not a repo" and poison either the cache or the
// noRepo skip, so it returns ok=false, definitive=false and caches nothing;
// the next poll re-probes. Returns toplevel, ok, definitive.
func gitTopLevel(cwd string) (string, bool, bool) {
	changesetTopMu.Lock()
	if root, ok := changesetTop[cwd]; ok {
		changesetTopMu.Unlock()
		return root, root != "", true // only definitive results ever reach the cache
	}
	changesetTopMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), changesetGitTimeout)
	defer cancel()
	root := ""
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	switch {
	case err == nil:
		root = strings.TrimSpace(string(out))
	case !gitErrDefinitive(err):
		return "", false, false // transient: cache nothing, retry next tick
	}
	changesetTopMu.Lock()
	changesetTop[cwd] = root
	changesetTopMu.Unlock()
	return root, root != "", true
}

var (
	changesetTopMu sync.Mutex
	changesetTop   = map[string]string{}
)

// refreshChangesetCmd runs git status off the UI goroutine. Uses
// --no-optional-locks so a panel poll never contends with the user's own git
// processes, and -uall so untracked files inside new directories are listed
// per file (the tree styles files, not collapsed folders).
func refreshChangesetCmd(cwd string) tea.Cmd {
	return func() tea.Msg {
		root, ok, definitive := gitTopLevel(cwd)
		if !ok {
			if definitive {
				return changesetMsg{noRepo: true}
			}
			// Transient probe failure: keep the last snapshot (the same rule a
			// failed status below applies) and let the next tick retry.
			return changesetMsg{}
		}
		ctx, cancel := context.WithTimeout(context.Background(), changesetGitTimeout)
		defer cancel()
		// -b adds the "##" branch header: one probe feeds both the file
		// changeset and the git panel's branch/dirty event state (#347).
		out, err := exec.CommandContext(ctx, "git", "--no-optional-locks", "-C", cwd,
			"status", "--porcelain", "-uall", "-b").Output()
		if err != nil {
			// The toplevel probe proved this is a repo, so this is a
			// transient failure (timeout, contention): keep the last
			// snapshot instead of disabling enrichment.
			return changesetMsg{}
		}
		branch, dirty := parseStatusBranch(string(out))
		return changesetMsg{files: parseStatusFiles(string(out), root, cwd), branch: branch, dirty: dirty, gitOK: true}
	}
}

// changesetUnquote undoes porcelain's C-style quoting of special paths.
func changesetUnquote(p string) string {
	if !strings.HasPrefix(p, `"`) {
		return p
	}
	if u, err := strconv.Unquote(p); err == nil {
		return u
	}
	return strings.Trim(p, `"`)
}

// parseStatusFiles reads `git status --porcelain -uall` output and returns
// the cwd-relative changeset. Porcelain paths are repo-root relative (verified
// from subdirectories), so they are rebased onto cwd; paths outside cwd drop
// out (the tree only shows cwd's subtree). Renames/copies carry "old -> new"
// and record both sides. The line count is capped.
func parseStatusFiles(out, root, cwd string) map[string]session.ChangeKind {
	res := map[string]session.ChangeKind{}
	lines := strings.Split(out, "\n")
	if len(lines) > changesetMaxStatusLines {
		lines = lines[:changesetMaxStatusLines]
	}
	add := func(p string, k session.ChangeKind) {
		p = changesetUnquote(strings.TrimSpace(p))
		if p == "" {
			return
		}
		abs := filepath.Join(root, filepath.FromSlash(p))
		rel, err := filepath.Rel(cwd, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return
		}
		res[filepath.ToSlash(rel)] = k
	}
	for _, ln := range lines {
		if strings.HasPrefix(ln, "## ") {
			continue // status header (the probe runs with -b), not a file entry
		}
		if len(ln) < 4 || ln[2] != ' ' {
			continue
		}
		x, y := ln[0], ln[1]
		rest := ln[3:]
		old, newer, arrow := strings.Cut(rest, " -> ")
		switch {
		case x == '?' && y == '?':
			add(rest, session.ChangeCreated)
		case arrow && (x == 'C' || y == 'C'):
			add(newer, session.ChangeCreated) // copy: the source is untouched
		case arrow && (x == 'R' || y == 'R'):
			add(old, session.ChangeDeleted)
			add(newer, session.ChangeRenamed)
		case x == 'R' || y == 'R' || x == 'C' || y == 'C':
			add(rest, session.ChangeCreated) // malformed shape: treat as new
		case y == 'D' && x == 'A':
			add(rest, session.ChangeDeleted) // staged new, removed in worktree → gone
		case x == 'D' && y == 'A':
			add(rest, session.ChangeCreated) // staged delete, re-added → present
		case x == 'A' || y == 'A':
			add(rest, session.ChangeCreated)
		case x == 'D' || y == 'D':
			add(rest, session.ChangeDeleted)
		default:
			add(rest, session.ChangeModified)
		}
	}
	return res
}

// mergeChangeset picks the rendered changeset: with a repo, porcelain is the
// truth and traced entries the index reports clean are stale (reverted or
// committed) and drop out — the issue #331 staleness rule; without a repo the
// session trace is the only source.
func mergeChangeset(trace, git map[string]session.ChangeKind, gitOK bool) map[string]session.ChangeKind {
	if gitOK {
		return git
	}
	return trace
}

// changesetRelativize rekeys the session's absolute traced paths onto cwd,
// dropping paths outside it.
func changesetRelativize(trace map[string]session.ChangeKind, cwd string) map[string]session.ChangeKind {
	if len(trace) == 0 {
		return nil
	}
	out := make(map[string]session.ChangeKind, len(trace))
	for abs, k := range trace {
		rel, err := filepath.Rel(cwd, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		out[filepath.ToSlash(rel)] = k
	}
	return out
}

// changesetRefreshCmd schedules the git enrichment for the files panel —
// unless this cwd is already known to be outside a repo (the skip the issue
// asks for; re-probed on a cwd change, a fresh session build, or toggling the
// panel). Returns nil when there is nothing to schedule.
func (m *model) changesetRefreshCmd() tea.Cmd {
	if m.st == nil || m.st.cwd == "" {
		return nil
	}
	if m.changesetCwd != m.st.cwd {
		m.resetChangesetForCwd()
	}
	if m.changesetNoGit {
		return nil
	}
	return refreshChangesetCmd(m.st.cwd)
}

// resetChangesetForCwd clears every snapshot when the session cwd changes
// (repo membership, porcelain and trace keys are all cwd-scoped). The git
// status event state (issue #347) resets with it: the first probe in the
// new cwd re-establishes the baseline there.
func (m *model) resetChangesetForCwd() {
	m.changesetCwd = m.st.cwd
	m.changesetNoGit = false
	m.changesetGitOK = false
	m.changesetGit = nil
	m.changesetFiles = nil
	m.changesetSeen = false
	m.gitStatusSeen = false
	m.gitStatusOK = false
	m.gitStatusDirty = 0
	m.gitStatusBranch = ""
}

// syncChangeset merges the last porcelain snapshot with the current session
// trace and rebuilds the cached tree when the merged set changed. Called from
// Update only (data messages) — never from View; the tree rebuild consults
// the changeset, the render path does not.
//
// It is also the project panel's content-event source (issue #347): every
// detected change after the first (baseline) observation is a domain event
// — toasted per occurrence whether or not the panel is open — while only
// the empty→non-empty *edge* may auto-open the panel. Returns the data
// refresh for a panel the edge just opened, if any.
func (m *model) syncChangeset() tea.Cmd {
	if m.st == nil || m.st.cwd == "" {
		return nil
	}
	if m.changesetCwd != m.st.cwd {
		m.resetChangesetForCwd()
	}
	var trace map[string]session.ChangeKind
	if m.st.sess != nil {
		trace = changesetRelativize(session.FileChanges(m.st.sess.ID), m.st.cwd)
	}
	next := mergeChangeset(trace, m.changesetGit, m.changesetGitOK)
	if len(next) == 0 {
		next = nil // canonical: an empty set and no set render and compare alike
	}
	// The first *observation* is the baseline (pre-existing state at
	// startup is not news, issue #347) — set before the change check so a
	// clean startup followed by the first change still counts as an event.
	baseline := !m.changesetSeen
	m.changesetSeen = true
	if maps.Equal(m.changesetFiles, next) {
		return nil
	}
	prev := m.changesetFiles
	// Recency stamp (ADR-0089): a change after the first observation
	// means the project tree's rendered content actually changed; the
	// baseline only establishes it so startup panels stay tied (render
	// order).
	if !baseline {
		m.stampPanelActivity(regionProject)
	}
	m.changesetFiles = next
	m.rebuildProjectTree()
	if baseline {
		return nil
	}
	return m.changesetEvent(prev, next)
}

// changesetEvent is the project panel's per-event notification layer
// (issue #347): every observed change of the session changeset is a domain
// event and gets its own toast — granular like the background panel's
// spawn/done/fail notices, never edge-deduplicated — whether the panel is
// open or closed. Layout is the edge-only layer: only new changes from a
// clean previous state may auto-open the panel, and the empty state is
// project-world "no contents", releasing a manual close's veto (see
// panelContentFinalized).
func (m *model) changesetEvent(prev, next map[string]session.ChangeKind) tea.Cmd {
	if len(next) == 0 {
		m.notify("project: session changeset clean", "/panel project")
	} else {
		m.notify("project: "+changesetEventSummary(next)+" changed this session", "/panel project")
	}
	var cmd tea.Cmd
	if len(prev) == 0 && len(next) > 0 {
		// The layout edge: content arriving from a clean previous state.
		wasOpen := m.panelProject
		m.autoOpenPanel(regionProject)
		if !wasOpen && m.panelProject && m.st != nil {
			cmd = refreshProjectCmd(m.st.cwd)
		}
	}
	// Auto-open first, then the veto release (ADR-0059's ordering).
	m.resetManualOverrideIfFinalized(regionProject)
	return cmd
}

// changesetEventSummary renders the compact tally for a changeset toast
// ("3 file(s) (2M 1A)") — the counts-only sibling of the panel's
// changesSummaryLine, without ANSI styling.
func changesetEventSummary(changes map[string]session.ChangeKind) string {
	counts := map[session.ChangeKind]int{}
	for _, k := range changes {
		counts[k]++
	}
	var parts []string
	for _, k := range []session.ChangeKind{session.ChangeCreated, session.ChangeModified, session.ChangeDeleted, session.ChangeRenamed} {
		if counts[k] == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d%s", counts[k], changeLetter(k)))
	}
	return fmt.Sprintf("%d file(s) (%s)", len(changes), strings.Join(parts, " "))
}
