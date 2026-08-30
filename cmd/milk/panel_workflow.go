package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/scoutme/milk/internal/workflow"
)

const workflowPanelWidth = 31        // total incl. 1 scrollbar column (used to size the main area)
const workflowPanelContentWidth = 30 // border + padding + inner content, excl. scrollbar

var styleWorkflowPanel = lipgloss.NewStyle().
	BorderStyle(lipgloss.NormalBorder()).
	BorderLeft(true).
	BorderForeground(lipgloss.AdaptiveColor{Light: "#AAA", Dark: "#555"}).
	PaddingLeft(1)

// buildWorkflowPanelLines returns the full (unwindowed, unpadded) content
// lines for the workflow panel. Scrolling and highlighting both operate on
// this slice before it is windowed down to the visible h rows.
func buildWorkflowPanelLines(st *workflow.State, inner int) []string {
	var lines []string
	add := func(ss ...string) { lines = append(lines, ss...) }

	// Title row (matches memory panel style)
	add(stylePanelTitle.Render(truncatePanel(" workflow", inner)))
	add("")

	switch {
	case st == nil || st.WorkflowName == "":
		add(dim("no active workflow"))
	default:
		add(buildGenericWorkflowPanelLines(st, inner)...)
	}

	return lines
}

// renderStageTree walks the full StageNode tree, producing indented lines and
// overlaying active/completed dynamic branches (for example fanout items).
func renderStageTree(node *workflow.StageNode, active *workflow.StageNode, completed *workflow.StageNode, depth int, inner int, add func(string)) {
	if node == nil {
		return
	}
	indent := strings.Repeat("  ", depth)
	activeNode := matchingActiveNode(node.Label, active)
	completedNode := matchingActiveNode(node.Label, completed)
	displayNode := node
	if activeNode != nil && stageLabelBase(activeNode.Label) == stageLabelBase(node.Label) {
		displayNode = activeNode
	} else if completedNode != nil && stageLabelBase(completedNode.Label) == stageLabelBase(node.Label) {
		displayNode = completedNode
	}
	label := indent + displayStageLabel(displayNode.Label)
	if activeNode != nil {
		add(truncatePanel(bold("▸ "+label), inner))
	} else if completedNode != nil {
		add(truncatePanel(dim("✓ "+label), inner))
	} else {
		add(truncatePanel(dim("  "+label), inner))
	}
	dynamicChildren := mergedDynamicChildren(node.Children, activeNode, completedNode)
	for _, child := range dynamicChildren {
		marker := "✓"
		if child.active {
			marker = "▸"
		}
		renderDynamicStageTree(child.activeNode, child.completedNode, depth+1, marker, inner, add)
	}
	if len(dynamicChildren) > 0 {
		return
	}
	for _, child := range node.Children {
		renderStageTree(child, active, completed, depth+1, inner, add)
	}
}

type dynamicStageChild struct {
	activeNode    *workflow.StageNode
	completedNode *workflow.StageNode
	active        bool
}

func mergedDynamicChildren(staticChildren []*workflow.StageNode, activeNode, completedNode *workflow.StageNode) []dynamicStageChild {
	seen := map[string]int{}
	var out []dynamicStageChild
	appendChildren := func(parent *workflow.StageNode, isActive bool) {
		if parent == nil {
			return
		}
		for _, child := range parent.Children {
			base := stageLabelBase(child.Label)
			if staticChildren != nil && !isDynamicStageLabel(child.Label) && stageChildrenContainBase(staticChildren, base) {
				continue
			}
			key := dynamicStageKey(child.Label)
			if idx, ok := seen[key]; ok {
				if isActive {
					out[idx].activeNode = child
					out[idx].active = true
				} else {
					out[idx].completedNode = child
				}
				continue
			}
			seen[key] = len(out)
			entry := dynamicStageChild{active: isActive}
			if isActive {
				entry.activeNode = child
			} else {
				entry.completedNode = child
			}
			out = append(out, entry)
		}
	}
	appendChildren(completedNode, false)
	appendChildren(activeNode, true)
	return out
}

func renderDynamicStageTree(active, completed *workflow.StageNode, depth int, marker string, inner int, add func(string)) {
	displayNode := active
	if displayNode == nil {
		displayNode = completed
	}
	if displayNode == nil {
		return
	}
	indent := strings.Repeat("  ", depth)
	style := dim
	if marker == "▸" {
		style = bold
	}
	add(truncatePanel(style(marker+" "+indent+displayStageLabel(displayNode.Label)), inner))
	children := mergedDynamicChildren(nil, active, completed)
	for _, child := range children {
		childMarker := "✓"
		if child.active {
			childMarker = "▸"
		}
		renderDynamicStageTree(child.activeNode, child.completedNode, depth+1, childMarker, inner, add)
	}
}

func displayStageLabel(label string) string {
	base := stageLabelBase(label)
	switch {
	case strings.Contains(label, " item["):
		if idx := strings.Index(label, " item["); idx >= 0 {
			return "item " + strings.TrimSuffix(strings.TrimPrefix(label[idx+len(" item["):], ""), "]")
		}
	case base == "sprint_loop":
		if idx := strings.Index(label, "["); idx >= 0 {
			return "sprint " + strings.TrimSuffix(label[idx+1:], "]")
		}
		return "sprint loop"
	case strings.HasSuffix(base, "_pass_loop") || base == "pass_loop":
		if idx := strings.Index(label, "["); idx >= 0 {
			return "pass " + strings.TrimSuffix(label[idx+1:], "]")
		}
		return "pass loop"
	}
	switch base {
	case "workflow":
		return "workflow"
	case "designer", "design":
		return "design"
	case "worker_fanout", "fanout":
		return "fanout"
	case "worker":
		return "work"
	case "worker_eval", "review":
		return "eval"
	case "final_evaluation":
		return "final eval"
	case "final_implementation":
		return "final impl"
	case "implement", "implementation":
		return "impl"
	}
	return strings.ReplaceAll(base, "_", " ")
}

func isDynamicStageLabel(label string) bool {
	return strings.Contains(label, "[") || strings.Contains(label, " item[") || strings.Contains(label, " items)")
}

func dynamicStageKey(label string) string {
	if isDynamicStageLabel(label) {
		return strings.TrimSpace(label)
	}
	return stageLabelBase(label)
}

func matchingActiveNode(label string, active *workflow.StageNode) *workflow.StageNode {
	if active == nil {
		return nil
	}
	targetLabel := label
	var walk func(*workflow.StageNode) *workflow.StageNode
	walk = func(node *workflow.StageNode) *workflow.StageNode {
		if node == nil {
			return nil
		}
		if stageLabelMatches(targetLabel, stageLabelBase(node.Label)) {
			return node
		}
		for _, child := range node.Children {
			if match := walk(child); match != nil {
				return match
			}
		}
		return nil
	}
	return walk(active)
}

func stageLabelMatches(label, activeBase string) bool {
	activeBase = normalizeStageMatchKey(activeBase)
	if normalizeStageMatchKey(stageLabelBase(label)) == activeBase {
		return true
	}
	_, role, ok := stageLabelParts(label)
	return ok && normalizeStageMatchKey(role) == activeBase
}

func normalizeStageMatchKey(s string) string {
	s = strings.TrimSpace(s)
	switch s {
	case "implementer", "implementation":
		return "implement"
	case "reviewer":
		return "review"
	}
	return s
}

func stageChildrenContainBase(children []*workflow.StageNode, base string) bool {
	for _, child := range children {
		if stageLabelBase(child.Label) == base {
			return true
		}
	}
	return false
}

func stageLabelParts(label string) (id, role string, ok bool) {
	if idx := strings.Index(label, "  "); idx >= 0 {
		return strings.TrimSpace(label[:idx]), strings.TrimSpace(label[idx+2:]), true
	}
	return strings.TrimSpace(label), "", false
}

func stageLabelBase(label string) string {
	if idx := strings.Index(label, "  "); idx >= 0 {
		return strings.TrimSpace(label[:idx])
	}
	if idx := strings.Index(label, " ("); idx >= 0 {
		return strings.TrimSpace(label[:idx])
	}
	if strings.Contains(label, " item[") {
		return strings.TrimSpace(label)
	}
	if idx := strings.Index(label, "["); idx >= 0 {
		return strings.TrimSpace(label[:idx])
	}
	return strings.TrimSpace(label)
}

func definitionStageTree(stages []workflow.Stage) *workflow.StageNode {
	root := &workflow.StageNode{Label: "workflow"}
	for _, stage := range stages {
		root.Children = append(root.Children, definitionStageNode(stage))
	}
	if len(root.Children) == 1 {
		return root.Children[0]
	}
	return root
}

func definitionStageNode(stage workflow.Stage) *workflow.StageNode {
	label := stage.ID
	if stage.Role != "" {
		label += "  " + stage.Role
	}
	node := &workflow.StageNode{Label: label}
	for _, child := range stage.Body {
		node.Children = append(node.Children, definitionStageNode(child))
	}
	return node
}

// buildGenericWorkflowPanelLines renders an interpreter-driven run's
// progress (workflow.ProgressMsg): StageTree instead of Sprint/Pass, no
// verdict history (the checkpoint's trace is the record of what happened,
// not surfaced in this panel).
func buildGenericWorkflowPanelLines(st *workflow.State, inner int) []string {
	var lines []string
	add := func(s string) { lines = append(lines, s) }

	if st.Task != "" {
		for _, line := range wordWrapPanel(st.Task, inner) {
			add(dim(line))
		}
		add("")
	}

	add(truncatePanel(bold(st.WorkflowName), inner))
	if st.Role != "" {
		add(dim("role: " + st.Role))
	}
	add("")

	if st.StageTree != nil {
		active := st.ActiveStageTree
		if active == nil && st.Role != "" && st.Role != "done" {
			active = &workflow.StageNode{Label: st.Role}
		}
		renderStageTree(st.StageTree, active, st.CompletedStageTree, 0, inner, add)
	}

	return lines
}

// renderWorkflowPanel renders the workflow progress panel into exactly h lines,
// scrolled to m.workflowPanelOffset (clamped so it never scrolls past the last
// screenful) and with any active panel-text selection highlighted.
func (m *model) renderWorkflowPanel(h int) string {
	inner := workflowPanelContentWidth - 2 // left border + padding

	all := buildWorkflowPanelLines(m.workflowState, inner)
	total := len(all)

	maxOffset := max(total-h, 0)
	if m.workflowPanelOffset > maxOffset {
		m.workflowPanelOffset = maxOffset
	}

	if m.panelSelRegion == regionWorkflow {
		all = applyPanelSelectionHighlight(all, m.panelSelAnchorLine, m.panelSelAnchorCol, m.panelSelEndLine, m.panelSelEndCol)
	}

	// Pad or trim to exactly h lines.
	lines := all[m.workflowPanelOffset:]
	for len(lines) < h {
		lines = append(lines, "")
	}
	lines = lines[:h]

	// Pre-pad each line to exactly inner cols so lipgloss does not re-wrap when
	// the style is rendered — Width() on a bordered style triggers cellbuf.Wrap,
	// which adds lines and makes the panel taller than h.
	var rows []string
	for _, line := range lines {
		lineW := len([]rune(stripANSI(line)))
		if lineW < inner {
			line += strings.Repeat(" ", inner-lineW)
		}
		rows = append(rows, line)
	}
	content := strings.Join(rows, "\n")
	return styleWorkflowPanel.Render(content)
}

// renderWorkflowPanelScrollbar returns a 1-column string of h lines: a dim │
// track with a ▌ thumb when the panel content overflows, or a blank column
// otherwise. Mirrors renderPanelScrollbar for the memory panel.
func (m *model) renderWorkflowPanelScrollbar(h int) string {
	inner := workflowPanelContentWidth - 2
	total := len(buildWorkflowPanelLines(m.workflowState, inner))
	needsBar := total > h

	var rows []string
	if !needsBar {
		for range h {
			rows = append(rows, " ")
		}
		return strings.Join(rows, "\n")
	}

	thumbTop, thumbBot := scrollThumb(h, total, m.workflowPanelOffset)
	for i := range h {
		if i >= thumbTop && i <= thumbBot {
			rows = append(rows, dim("▌"))
		} else {
			rows = append(rows, dim("│"))
		}
	}
	return strings.Join(rows, "\n")
}

// wordWrapPanel wraps s into lines of at most maxWidth visible characters.
func wordWrapPanel(s string, maxWidth int) []string {
	if maxWidth <= 0 {
		return []string{s}
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var out []string
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) <= maxWidth {
			line += " " + w
		} else {
			out = append(out, line)
			line = w
		}
	}
	return append(out, line)
}

// workflowPanelLineCount returns the number of content lines the panel would occupy.
func workflowPanelLineCount(st *workflow.State) int {
	return len(buildWorkflowPanelLines(st, workflowPanelContentWidth-2))
}

func truncatePanel(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return s
	}
	stripped := []rune(stripANSI(s))
	if len(stripped) <= maxWidth {
		return s
	}
	return string(stripped[:maxWidth-1]) + "…"
}
