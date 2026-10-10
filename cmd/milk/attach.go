package main

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/livebuf"
	"github.com/scoutme/milk/internal/obs"
)

// attachKind identifies what an attachState is showing.
type attachKind int

const (
	attachBackground attachKind = iota
	attachWorkflow
)

func (k attachKind) String() string {
	if k == attachWorkflow {
		return "workflow"
	}
	return "background"
}

// attachState is non-nil while the TUI is showing a live-attach view over a
// background job's or workflow's live buffer instead of the main transcript
// viewport (ADR-0047; issue #154) — the same View()/handleResize()
// substitution shape as ptyPane, generalized to a plain streamed-text buffer
// rather than a full VT100 emulation, since this is a read-only view, not an
// interactive terminal.
type attachState struct {
	kind attachKind
	// jobID identifies which Job this is (kind == attachBackground); empty
	// for a workflow, since the TUI shows at most one active workflow at a
	// time (m.workflowState) and so needs no per-instance ID to disambiguate.
	jobID string
	label string
	buf   *livebuf.Buffer
	vp    viewport.Model
}

// attachPollInterval is how often the attach view's content is refreshed
// while attached. A background job's Live buffer is written to directly by
// its own goroutine with no accompanying tea.Msg (see internal/agent/local's
// Job.Live) — so, like the memory panel's periodic poll, the attach view
// needs its own tick to notice new content rather than waiting for some
// other message to trigger a redraw. Workflow chunks additionally get an
// immediate resync from their own WorkflowChunkMsg handler, so this tick is
// belt-and-suspenders for the workflow case but load-bearing for background
// jobs.
const attachPollInterval = 500 * time.Millisecond

type attachRefreshMsg struct{}

func attachRefreshTick() tea.Cmd {
	return tea.Tick(attachPollInterval, func(time.Time) tea.Msg {
		return attachRefreshMsg{}
	})
}

// startAttach begins showing buf in place of the main transcript. A second
// call while already attached (e.g. double-clicking a different background
// job row) simply re-targets the existing attach view at the new buffer,
// rather than requiring a detach first.
func (m *model) startAttach(kind attachKind, jobID, label string, buf *livebuf.Buffer) tea.Cmd {
	if buf == nil {
		return nil
	}
	m.attached = &attachState{
		kind:  kind,
		jobID: jobID,
		label: label,
		buf:   buf,
		vp:    viewport.New(m.vpWidth(), m.viewportHeight()),
	}
	m.syncAttachedContent()
	obs.Debug("attach.start", "kind", kind.String(), "job", jobID, "label", label)
	return attachRefreshTick()
}

// detachAttach stops showing the attach view and returns to the normal
// transcript. The underlying buffer is untouched and keeps accumulating
// (for a still-running job/workflow) even while nothing is attached to it.
func (m *model) detachAttach() {
	if m.attached == nil {
		return
	}
	obs.Debug("attach.stop", "kind", m.attached.kind.String(), "job", m.attached.jobID, "label", m.attached.label)
	m.attached = nil
}

// syncAttachedContent rebuilds the attach viewport's content from the live
// buffer's current snapshot. Sticky-bottom, matching the main transcript's
// own convention: only auto-scrolls when already at the bottom, so a user
// who has scrolled up to read earlier output isn't yanked back down by new
// content arriving.
func (m *model) syncAttachedContent() {
	if m.attached == nil {
		return
	}
	vw := m.vpWidth()
	vpH := m.viewportHeight()
	if m.attached.vp.Width != vw {
		m.attached.vp.Width = vw
	}
	if m.attached.vp.Height != vpH {
		m.attached.vp.Height = vpH
	}
	atBottom := m.attached.vp.AtBottom()
	header := dim(fmt.Sprintf("── attached: %s — Esc to detach ──", m.attached.label))
	body := m.attached.buf.Snapshot()
	if vw > 0 {
		body = ansi.Wrap(expandTabsForWrap(body), vw, "")
	}
	if m.attached.kind == attachWorkflow && m.workflowState != nil && m.workflowState.ParallelExec {
		header += "\n" + dim("parallel workflow — worker output is not streamed live; per-item start/finish lines appear instead")
	}
	content := header + "\n\n" + body
	// Tint every row of the attach view with the same subtle background used
	// to set an alternating side panel apart from the main transcript —
	// applied unconditionally here (not alternating with anything) so the
	// whole area reads as visually distinct from the live conversation
	// underneath it while attached.
	if bg := panelAltBackgroundCode(); bg != "" {
		content = tintBlock(content, vw, bg)
	}
	m.attached.vp.SetContent(content)
	if atBottom {
		m.attached.vp.GotoBottom()
	}
}

// tintBlock pads every line of content to width (so the background tint
// spans the full row, not just the underlying text) and applies bg via
// withPanelBackground, mirroring renderSidePanel's per-line treatment.
func tintBlock(content string, width int, bg string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if lineW := utf8.RuneCountInString(stripANSI(line)); width > 0 && lineW < width {
			line += strings.Repeat(" ", width-lineW)
		}
		lines[i] = withPanelBackground(line, bg)
	}
	return strings.Join(lines, "\n")
}

// handleAttachKey handles every key while attached: Esc detaches; everything
// else is swallowed — this is a read-only view, not an input surface, and
// arrow keys in particular stay reserved for input-history navigation
// elsewhere in the TUI, never repurposed here for scrolling.
func (m model) handleAttachKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" {
		m.detachAttach()
		m.syncLayout()
	}
	return m, nil
}

// handleBackgroundPanelClick runs the background panel's click-for-attach
// behavior (ADR-0047): a single click arms the row's job ID, and a second
// click on the same job within 400ms attaches the live view to it — the
// background-panel analogue of handleMemoryPanelClick, but attaching instead
// of printing to the transcript. The "bg:" prefix keeps this click's arm/
// trigger state from colliding with the memory panel's own IDs, since both
// share the same m.lastPanelClickID field.
func (m *model) handleBackgroundPanelClick(lineIdx int) tea.Cmd {
	if m.agents.backgroundMgr == nil {
		return nil
	}
	jobs := m.agents.backgroundMgr.Jobs()
	// buildBackgroundPanelLines: line 0 is the title, line 1 is blank, and
	// line 2+i is job i in the same order Jobs() returns them — recomputed
	// here rather than cached, so a job that started or finished between
	// the last render and this click can't shift the mapping out of sync.
	jobIdx := lineIdx - 2
	if jobIdx < 0 || jobIdx >= len(jobs) {
		return nil
	}
	job := jobs[jobIdx]
	key := "bg:" + job.ID
	now := time.Now()
	if key == m.lastPanelClickID && now.Sub(m.lastPanelClickTime) <= 400*time.Millisecond {
		m.lastPanelClickID = ""
		return m.startAttach(attachBackground, job.ID, fmt.Sprintf("%s (%s)", job.Label, job.Status), job.Live)
	}
	m.lastPanelClickID = key
	m.lastPanelClickTime = now
	return nil
}

// handleWorkflowPanelClick is handleBackgroundPanelClick's workflow
// counterpart. No per-row ID is needed since the TUI runs at most one active
// workflow at a time (m.workflowState).
func (m *model) handleWorkflowPanelClick() tea.Cmd {
	if m.workflowState == nil {
		return nil
	}
	const key = "wf"
	now := time.Now()
	if key == m.lastPanelClickID && now.Sub(m.lastPanelClickTime) <= 400*time.Millisecond {
		m.lastPanelClickID = ""
		label := m.workflowState.WorkflowName
		if label == "" {
			label = m.workflowState.Task
		}
		return m.startAttach(attachWorkflow, "", label, m.workflowState.LiveBuffer())
	}
	m.lastPanelClickID = key
	m.lastPanelClickTime = now
	return nil
}
