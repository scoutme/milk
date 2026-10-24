package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/livebuf"
)

// Project tree panel (issue #206, ADR-0066): the session cwd's file layout,
// backed by the same git-file-list candidate index the @-file completion uses
// (ADR-0057) so it never pays the walk-budget problem. Click a directory to
// expand/collapse it, click a file to preview it in the main area, double-
// click to insert an @-mention into the input.

const projectPanelWidth = 33 // 32 inner + 1 right scrollbar column
const projectPanelInner = 32

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
// everything deeper starts collapsed). Returns the rendered lines and one ID
// per line: "" for non-interactive rows, "d:<dir>/" for directories and the
// relative path for files.
func buildProjectTreeLines(files []string, truncated bool, expanded map[string]bool, inner int) (lines, ids []string) {
	lines = append(lines, panelTitleLine("project", regionProject, inner))
	ids = append(ids, "")
	lines = append(lines, "")
	ids = append(ids, "")

	if files == nil {
		lines = append(lines, dim("  (loading…)"))
		ids = append(ids, "")
		return lines, ids
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
				lines = append(lines, treeRow(prefix, glyph, it.name+"/", inner, true))
				ids = append(ids, "d:"+p)
				if open {
					walk(n.children[it.key], prefix+"  ", p, depth+1)
				}
				continue
			}
			lines = append(lines, treeRow(prefix, "  ", it.name, inner, false))
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
// render in the section style so they read as headings among files.
func treeRow(prefix, glyph, name string, inner int, dir bool) string {
	plain := prefix + glyph
	if w := utf8.RuneCountInString(plain) + utf8.RuneCountInString(name); w > inner {
		keep := max(inner-utf8.RuneCountInString(plain)-1, 1)
		name = string([]rune(name)[:keep]) + "…"
	}
	if dir {
		return dim(prefix) + stylePanelSection.Render(glyph+name)
	}
	return dim(prefix) + name
}

// rebuildProjectTree recomputes the cached panel lines from the cached file
// list. Called from Update (never from View, which renders a value copy) so
// the derived state persists.
func (m *model) rebuildProjectTree() {
	m.projectLines, m.projectIDs = buildProjectTreeLines(m.projectFiles, m.projectTruncated, m.projectExpanded, projectPanelInner)
}

// projectDirExpanded reports a directory's effective open state (explicit
// override wins, else top-level dirs are open).
func (m *model) projectDirExpanded(dir string) bool {
	if v, ok := m.projectExpanded[dir]; ok {
		return v
	}
	return strings.Count(dir, "/") == 1
}

// handleProjectPanelClick: a click on a directory row toggles its expansion;
// a click on a file row previews the file in the main area, and a second
// click on the same file within 400ms inserts an @-mention into the input
// (the memory panel's arm/act rhythm).
func (m *model) handleProjectPanelClick(lineIdx int) tea.Cmd {
	if lineIdx < 0 || lineIdx >= len(m.projectIDs) {
		return nil
	}
	id := m.projectIDs[lineIdx]
	if id == "" {
		return nil
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
	now := time.Now()
	if path == m.lastTreeClickPath && now.Sub(m.lastTreeClickTime) <= 400*time.Millisecond {
		m.lastTreeClickPath = ""
		m.ta.InsertString("@" + path)
		m.syncLayout()
		return nil
	}
	m.lastTreeClickPath = path
	m.lastTreeClickTime = now

	buf, label := filePreviewBuffer(m.st.cwd, path)
	if buf == nil {
		m.notify("cannot preview "+path, "")
		return nil
	}
	return m.startAttach(attachPreview, "", label, buf)
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
