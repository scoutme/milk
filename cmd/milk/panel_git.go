package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/livebuf"
)

// Git graph panel (issue #206, ADR-0066): the repo's branch/commit graph,
// lanes laid out in code from `git log` plumbing (see gitgraph.go), the
// current branch and dirty count on top. Click a commit to preview its
// summary and diff in the main area.

const gitPanelWidth = 33 // 32 inner + 1 right scrollbar column
const gitPanelInner = 32

// renderGitPanel / renderGitPanelScrollbar — see renderSidePanel.
func (m *model) renderGitPanel(h int) string {
	return m.renderSidePanel(regionGit, h)
}

func (m *model) renderGitPanelScrollbar(h int) string {
	return m.renderSidePanelScrollbar(regionGit, h)
}

// buildGitPanelLines renders the panel: title, branch header, then one graph
// row per commit. Returns lines and one ID per line ("" except commit rows,
// which carry the full hash).
func buildGitPanelLines(rows []gitGraphLine, branch string, dirty int, gitErr string, inner int) (lines, ids []string) {
	lines = append(lines, panelTitleLine("git", regionGit, inner))
	ids = append(ids, "")
	lines = append(lines, "")
	ids = append(ids, "")

	switch {
	case gitErr != "":
		lines = append(lines, dim("  "+gitErr))
		ids = append(ids, "")
		lines = append(lines, dim("  (a git repo is required)"))
		ids = append(ids, "")
		return lines, ids
	case rows == nil:
		lines = append(lines, dim("  (loading…)"))
		ids = append(ids, "")
		return lines, ids
	case len(rows) == 0:
		lines = append(lines, dim("  (no commits)"))
		ids = append(ids, "")
		return lines, ids
	}

	head := "⎇ " + branch
	if dirty > 0 {
		head += red(fmt.Sprintf(" ✎%d", dirty))
	} else {
		head += dim(" clean")
	}
	lines = append(lines, head)
	ids = append(ids, "")
	lines = append(lines, "")
	ids = append(ids, "")

	for _, r := range rows {
		line := gitRowLine(r, inner)
		lines = append(lines, line)
		ids = append(ids, r.Commit.Hash)
	}
	return lines, ids
}

// gitRowLine renders one graph row: the lane prefix, short hash, subject and
// (when it fits) the decorations.
func gitRowLine(r gitGraphLine, inner int) string {
	prefixW := utf8.RuneCountInString(r.Prefix)
	budget := inner - prefixW - 1
	if budget < 8 {
		budget = 8
	}
	subject := r.Commit.Subject
	if decor := strings.TrimSpace(r.Commit.Decor); decor != "" {
		// Decorations only when the subject still fits after them.
		decorW := utf8.RuneCountInString(decor) + 1
		if utf8.RuneCountInString(subject)+decorW <= budget-8 {
			subject = subject + " " + decor
		}
	}
	maxW := budget - 8 // reserve for " " + 7-char hash + " "
	if w := utf8.RuneCountInString(subject); w > maxW {
		subject = string([]rune(subject)[:max(maxW-1, 1)]) + "…"
	}
	return dim(r.Prefix+" ") + dim(r.Commit.Short) + " " + subject
}

// handleGitPanelClick previews the clicked commit's summary and diff in the
// main area (attach view).
func (m *model) handleGitPanelClick(lineIdx int) tea.Cmd {
	if lineIdx < 0 || lineIdx >= len(m.gitIDs) {
		return nil
	}
	hash := m.gitIDs[lineIdx]
	if hash == "" {
		return nil
	}
	out, err := gitShow(m.st.cwd, hash)
	if err != nil {
		m.notify("git show failed: "+err.Error(), "")
		return nil
	}
	buf := livebuf.New(gitShowMaxBytes + 1024)
	buf.Append([]byte(out))
	short := hash
	if len(short) > 7 {
		short = short[:7]
	}
	return m.startAttach(attachPreview, "", "commit "+short, buf)
}

// --- refresh plumbing ---

// projectDataMsg carries a fresh file listing for the project panel.
type projectDataMsg struct {
	files     []string
	truncated bool
}

// gitDataMsg carries a fresh graph for the git panel.
type gitDataMsg struct {
	rows   []gitGraphLine
	branch string
	dirty  int
	err    string
}

// panelDataPollInterval is how often the project/git panels refresh their
// data while open ("live-ish" per the issue — on-tick refresh rather than
// fsnotify).
const panelDataPollInterval = 3 * time.Second

func projectPollTick() tea.Cmd {
	return tea.Tick(panelDataPollInterval, func(time.Time) tea.Msg {
		return projectRefreshMsg{}
	})
}

func gitPollTick() tea.Cmd {
	return tea.Tick(panelDataPollInterval, func(time.Time) tea.Msg {
		return gitRefreshMsg{}
	})
}

type projectRefreshMsg struct{}
type gitRefreshMsg struct{}

// refreshProjectCmd fetches the cwd's file listing off the UI goroutine (the
// first build can take a while; later ones are the index's cached snapshot).
func refreshProjectCmd(cwd string) tea.Cmd {
	return func() tea.Msg {
		snap := indexFor(cwd).get(2 * time.Second)
		if snap == nil {
			return projectDataMsg{}
		}
		var files []string
		for _, e := range snap.entries {
			if !strings.HasSuffix(e.path, "/") {
				files = append(files, e.path)
			}
		}
		return projectDataMsg{files: files, truncated: snap.truncated}
	}
}

// refreshGitCmd fetches the graph off the UI goroutine (git runs a few
// processes per refresh).
func refreshGitCmd(cwd string) tea.Cmd {
	return func() tea.Msg {
		commits, branch, dirty, err := gitLog(cwd)
		msg := gitDataMsg{rows: layoutGraph(commits), branch: branch, dirty: dirty}
		if err != nil {
			msg.err = gitErrText(err)
		}
		return msg
	}
}

// gitErrText maps a git failure to a stable panel line.
func gitErrText(err error) string {
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return "git is not installed"
	}
	return "not a git repository"
}
