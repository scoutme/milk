package main

import (
	"context"
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

// changesetMsg carries one porcelain snapshot for the files panel.
type changesetMsg struct {
	files  map[string]session.ChangeKind // cwd-relative; empty non-nil when clean
	gitOK  bool                          // status ran successfully — files is authoritative
	noRepo bool                          // definitively no repo / no git: stop refreshing this cwd
}

// gitTopLevel caches cwd's repository toplevel (positive cache only — the
// model's changesetNoGit flag is what skips further probes in non-repos).
func gitTopLevel(cwd string) (string, bool) {
	changesetTopMu.Lock()
	if root, ok := changesetTop[cwd]; ok {
		changesetTopMu.Unlock()
		return root, root != ""
	}
	changesetTopMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), changesetGitTimeout)
	defer cancel()
	root := ""
	if out, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel").Output(); err == nil {
		root = strings.TrimSpace(string(out))
	}
	changesetTopMu.Lock()
	changesetTop[cwd] = root
	changesetTopMu.Unlock()
	return root, root != ""
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
		root, ok := gitTopLevel(cwd)
		if !ok {
			return changesetMsg{noRepo: true}
		}
		ctx, cancel := context.WithTimeout(context.Background(), changesetGitTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, "git", "--no-optional-locks", "-C", cwd,
			"status", "--porcelain", "-uall").Output()
		if err != nil {
			// The toplevel probe proved this is a repo, so this is a
			// transient failure (timeout, contention): keep the last
			// snapshot instead of disabling enrichment.
			return changesetMsg{}
		}
		return changesetMsg{files: parseStatusFiles(string(out), root, cwd), gitOK: true}
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
// (repo membership, porcelain and trace keys are all cwd-scoped).
func (m *model) resetChangesetForCwd() {
	m.changesetCwd = m.st.cwd
	m.changesetNoGit = false
	m.changesetGitOK = false
	m.changesetGit = nil
	m.changesetFiles = nil
}

// syncChangeset merges the last porcelain snapshot with the current session
// trace and rebuilds the cached tree when the merged set changed. Called from
// Update only (data messages) — never from View; the tree rebuild consults
// the changeset, the render path does not.
func (m *model) syncChangeset() {
	if m.st == nil || m.st.cwd == "" {
		return
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
	if maps.Equal(m.changesetFiles, next) {
		return
	}
	// Recency stamp (ADR-0089): a change after the first load means the
	// project tree's rendered content actually changed; the first load only
	// establishes the baseline so startup panels stay tied (render order).
	if m.changesetFiles != nil {
		m.stampPanelActivity(regionProject)
	}
	m.changesetFiles = next
	m.rebuildProjectTree()
}
