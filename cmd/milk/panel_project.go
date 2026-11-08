package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/scoutme/milk/internal/diff"
	"github.com/scoutme/milk/internal/livebuf"
	"github.com/scoutme/milk/internal/session"
)

// Project tree panel (issue #206, ADR-0066): the session cwd's file layout,
// backed by the same git-file-list candidate index the @-file completion uses
// (ADR-0057) so it never pays the walk-budget problem. Double-click a
// directory to expand/collapse it, double-click a file to preview it in the
// main area — as a full-file diff when the file has working-tree changes
// (modified/untracked in a git repo), whole file present with its changes in
// the transcript's green/red palette, as raw contents otherwise — triple-
// click to insert an @-mention into the input (single click only focuses:
// the uniform cadence, ADR-0066).

const projectPanelWidth = 33 // 32 inner + 1 right scrollbar column
const projectPanelInner = 32

// fileDiffFullContext is the -U value for single-file previews: large enough
// that git never truncates context, so the preview shows the whole file —
// every unchanged line, with the changes marked in the transcript's green/red
// — instead of only the hunks around them. The 128 KB output cap still bounds
// pathological files.
const fileDiffFullContext = 999999

// projectTreeMaxLines caps the rendered tree; huge repos get a truncation
// note instead of an unbounded panel.
const projectTreeMaxLines = 1500

// renderProjectPanel returns a vertical panel string of exactly h lines and
// projectPanelInner columns (scrollbar rendered separately). Body shared with
// every other side panel — see renderSidePanel.
func (m *model) renderProjectPanel(h int) string {
	return m.renderSidePanel(regionProject, h)
}

// renderProjectPanelScrollbar — see renderSidePanelScrollbar.
func (m *model) renderProjectPanelScrollbar(h int) string {
	return m.renderSidePanelScrollbar(regionProject, h)
}

// treeNode is one node of the lazily-expanded file tree.
type treeNode struct {
	name     string
	dir      bool
	children map[string]*treeNode
}

func (n *treeNode) child(key, name string, dir bool) *treeNode {
	if n.children == nil {
		n.children = map[string]*treeNode{}
	}
	if c, ok := n.children[key]; ok {
		return c
	}
	c := &treeNode{name: name, dir: dir}
	n.children[key] = c
	return c
}

// buildProjectTreeLines renders the tree from a flat file list. A directory
// is expanded when its path (with trailing slash) is explicitly set in
// expanded, and otherwise when it is at depth 0 (top level starts open,
// everything deeper starts collapsed). changes (the session changeset, issue
// #331 — nil for none) adds the summary line, per-file status glyphs and
// ancestor markers; without it the rendering is byte-identical to the flat
// tree. Returns the rendered lines and one ID per line: "" for
// non-interactive rows, "d:<dir>/" for directories and the relative path for
// files.
func buildProjectTreeLines(files []string, truncated bool, expanded map[string]bool, inner int, changes map[string]session.ChangeKind) (lines, ids []string) {
	lines = append(lines, panelTitleLine("files", regionProject, inner))
	ids = append(ids, "")
	lines = append(lines, "")
	ids = append(ids, "")

	if files == nil {
		lines = append(lines, dim("  (loading…)"))
		ids = append(ids, "")
		return lines, ids
	}

	// Ancestor directories of changed files get the muted discoverability
	// marker so changes under collapsed branches stay visible.
	changedDirs := map[string]bool{}
	for p := range changes {
		parts := strings.Split(p, "/")
		acc := ""
		for _, seg := range parts[:max(len(parts)-1, 0)] {
			acc += seg + "/"
			changedDirs[acc] = true
		}
	}
	if len(changes) > 0 {
		lines = append(lines, changesSummaryLine(changes, inner))
		ids = append(ids, changesRowID)
	}

	root := &treeNode{dir: true}
	for _, f := range files {
		if f == "" {
			continue
		}
		parts := strings.Split(f, "/")
		cur := root
		for i, p := range parts {
			isFile := i == len(parts)-1
			cur = cur.child(p, p, !isFile)
		}
	}

	var walk func(n *treeNode, prefix string, path string, depth int)
	walk = func(n *treeNode, prefix string, path string, depth int) {
		type item struct {
			key, name string
			dir       bool
		}
		var items []item
		for k, c := range n.children {
			items = append(items, item{k, c.name, c.dir})
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].dir != items[j].dir {
				return items[i].dir
			}
			return strings.ToLower(items[i].name) < strings.ToLower(items[j].name)
		})
		for _, it := range items {
			if len(lines) >= projectTreeMaxLines {
				lines = append(lines, dim("  … (tree truncated)"))
				ids = append(ids, "")
				return
			}
			p := path + it.name
			if it.dir {
				p += "/"
				open := depth == 0
				if v, ok := expanded[p]; ok {
					open = v
				}
				glyph := "▸ "
				if open {
					glyph = "▾ "
				}
				lines = append(lines, treeRow(prefix, glyph, it.name+"/", inner, true, "", changedDirs[p]))
				ids = append(ids, "d:"+p)
				if open {
					walk(n.children[it.key], prefix+"  ", p, depth+1)
				}
				continue
			}
			lines = append(lines, treeRow(prefix, changeGlyph(changes[p]), it.name, inner, false, changes[p], false))
			ids = append(ids, p)
		}
	}
	walk(root, "", "", 0)

	if len(root.children) == 0 {
		lines = append(lines, dim("  (empty)"))
		ids = append(ids, "")
	}
	if truncated {
		lines = append(lines, dim("  … (index truncated)"))
		ids = append(ids, "")
	}
	return lines, ids
}

// treeRow renders one indented tree row, rune-truncated to fit inner. Dirs
// render in the section style so they read as headings among files. A file
// with a change kind renders its compact status glyph (M/A/D/R) in the
// alignment slot under the dir glyphs — unchanged rows keep their exact
// historical rendering. marker (dirs only) appends the muted changed-descendant
// dot, reserving one column for it.
func treeRow(prefix, glyph, name string, inner int, dir bool, kind session.ChangeKind, marker bool) string {
	budget := inner
	if marker {
		budget--
	}
	plain := prefix + glyph
	if w := utf8.RuneCountInString(plain) + utf8.RuneCountInString(name); w > budget {
		keep := max(budget-utf8.RuneCountInString(plain)-1, 1)
		name = string([]rune(name)[:keep]) + "…"
	}
	if dir {
		row := dim(prefix) + stylePanelSection.Render(glyph+name)
		if marker {
			row += dim("·")
		}
		return row
	}
	if kind != "" {
		return dim(prefix) + changeStyle(kind).Render(glyph+name)
	}
	return dim(prefix) + name
}

// changesSummaryLine renders the at-a-glance changeset (" changes 2M 1A 1D")
// under the panel title — deletions never appear as rows (gone from disk),
// so the counts are where they stay visible.
func changesSummaryLine(changes map[string]session.ChangeKind, inner int) string {
	counts := map[session.ChangeKind]int{}
	for _, k := range changes {
		counts[k]++
	}
	var b strings.Builder
	b.WriteString(dim(" changes "))
	for _, k := range []session.ChangeKind{session.ChangeCreated, session.ChangeModified, session.ChangeDeleted, session.ChangeRenamed} {
		if counts[k] == 0 {
			continue
		}
		b.WriteString(changeStyle(k).Render(fmt.Sprintf("%d%s", counts[k], changeLetter(k))))
		b.WriteString(" ")
	}
	return strings.TrimRight(b.String(), " ")
}

// changeGlyph / changeLetter: the two-character alignment-slot glyph and the
// one-character counter letter for one change kind.
func changeGlyph(k session.ChangeKind) string {
	if k == "" {
		return "  "
	}
	return changeLetter(k) + " "
}

func changeLetter(k session.ChangeKind) string {
	switch k {
	case session.ChangeCreated:
		return "A"
	case session.ChangeModified:
		return "M"
	case session.ChangeDeleted:
		return "D"
	case session.ChangeRenamed:
		return "R"
	}
	return " "
}

// changeStyle is the per-kind files-panel style; rebuildStyles (theme.go)
// fills them for both skins, and the zero value renders plain — so tests and
// early call paths never carry raw escapes.
func changeStyle(k session.ChangeKind) lipgloss.Style {
	switch k {
	case session.ChangeCreated:
		return styleChangeCreated
	case session.ChangeModified:
		return styleChangeModified
	case session.ChangeDeleted:
		return styleChangeDeleted
	case session.ChangeRenamed:
		return styleChangeRenamed
	}
	return lipgloss.NewStyle()
}

// rebuildProjectTree recomputes the cached panel lines from the cached file
// list and the session changeset. Called from Update (never from View, which
// renders a value copy) so the derived state persists.
func (m *model) rebuildProjectTree() {
	m.projectLines, m.projectIDs = buildProjectTreeLines(m.projectFiles, m.projectTruncated, m.projectExpanded, projectPanelInner, m.changesetFiles)
}

// projectDirExpanded reports a directory's effective open state (explicit
// override wins, else top-level dirs are open).
func (m *model) projectDirExpanded(dir string) bool {
	if v, ok := m.projectExpanded[dir]; ok {
		return v
	}
	return strings.Count(dir, "/") == 1
}

// changesRowID makes the project panel's top changes-summary line clickable:
// its action is the working tree's complete local state (status + diff vs
// HEAD) — the ONLY project-wide preview in the panels' vocabulary. Deliberately
// a prefix no real path carries (project file ids are bare relative paths, dir
// ids are "d:…"), so it can't collide with a file row.
const changesRowID = "wt:"

// handleProjectPanelClick acts on a project row directly: a directory row
// toggles its expansion; the changes-summary row previews the working tree;
// a file row previews it in the main area — as a
// diff when the file has working-tree changes (fileDiffBuffer), as contents
// otherwise. The click cadence around it lives in panelClickAction (single
// = deferred focus, this preview on double, @-mention on triple) — a direct
// call is one action, so every test of the action itself stays simple.
func (m *model) handleProjectPanelClick(lineIdx int) tea.Cmd {
	if lineIdx < 0 || lineIdx >= len(m.projectIDs) {
		return nil
	}
	id := m.projectIDs[lineIdx]
	if id == "" {
		return nil
	}
	if id == changesRowID {
		// The changes-summary line owns the project-wide diff — file rows
		// never do (they stay scoped to their own path, below).
		out, err := workingTreePreview(m.st.cwd)
		if err != nil {
			m.notify("git status failed: "+err.Error(), "")
			return nil
		}
		buf := livebuf.New(gitShowMaxBytes + 1024)
		buf.Append([]byte(out))
		return m.startAttach(attachPreview, "", "working tree", buf)
	}
	if dir, ok := strings.CutPrefix(id, "d:"); ok {
		if m.projectExpanded == nil {
			m.projectExpanded = map[string]bool{}
		}
		m.projectExpanded[dir] = !m.projectDirExpanded(dir)
		m.rebuildProjectTree()
		return nil
	}

	path := id

	// Preferred view: the file's diff when it has one (modified or untracked
	// in a git repo). Clean files, non-repo cwds and git failures fall
	// through to the content preview below — the diff is a bonus, never a
	// requirement.
	if dbuf, dlabel := fileDiffBuffer(m.st.cwd, path); dbuf != nil {
		return m.startAttach(attachPreview, "", dlabel, dbuf)
	}
	buf, label := filePreviewBuffer(m.st.cwd, path)
	if buf == nil {
		m.notify("cannot preview "+path, "")
		return nil
	}
	return m.startAttach(attachPreview, "", label, buf)
}

// fileDiffBuffer returns the file's working-tree diff when it has one: a
// modified file previews as its complete diff vs HEAD (staged + unstaged)
// with FULL context — the whole file is shown, unchanged lines dim, changes
// in the transcript's green/red palette (diff.ColorizeUnified); an untracked
// file as a whole-file added diff; and everything else (clean file, no git,
// not a repo, probe failure) returns nil so the caller keeps the raw content
// preview. One status probe decides the branch.
func fileDiffBuffer(cwd, rel string) (*livebuf.Buffer, string) {
	st, err := gitStatusShort(cwd, rel)
	if err != nil || strings.TrimSpace(st) == "" {
		return nil, "" // clean, or git can't speak for this path
	}
	var out string
	if strings.HasPrefix(strings.TrimSpace(st), "??") {
		out, err = gitDiffNoIndex(cwd, rel)
	} else {
		out, err = gitDiffHEAD(cwd, rel, fileDiffFullContext)
	}
	if err != nil || strings.TrimSpace(out) == "" {
		return nil, ""
	}
	out = diff.ColorizeUnified(out)
	buf := livebuf.New(gitShowMaxBytes + 1024)
	buf.Append([]byte(out))
	return buf, "diff " + rel
}

// filePreviewBuffer reads a file (capped) into an attach-able buffer. Returns
// nil when the file cannot be read.
func filePreviewBuffer(cwd, rel string) (*livebuf.Buffer, string) {
	abs := filepath.Join(cwd, rel)
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, ""
	}
	const capBytes = 128 * 1024
	truncated := false
	if len(data) > capBytes {
		data = data[:capBytes]
		truncated = true
	}
	if isBinaryContent(data) {
		return nil, ""
	}
	buf := livebuf.New(capBytes + 256)
	buf.Append(data)
	if truncated {
		buf.Append([]byte("\n… (file truncated at 128 KB)"))
	}
	return buf, "file " + rel
}

// isBinaryContent reports whether data looks like binary content (a NUL byte
// in the first 8 KB) — those files are refused for preview rather than
// dumped as mojibake into the attach view.
func isBinaryContent(data []byte) bool {
	n := min(len(data), 8192)
	for _, b := range data[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}
