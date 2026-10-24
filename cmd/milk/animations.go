package main

import (
	"math"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/harmonica"
	"github.com/charmbracelet/x/ansi"
)

// Spring animations (harmonica): side panels slide in and out, and the mouse
// wheel scrolls with inertia. Both are pure view effects driven by one 60 fps
// tick that runs only while something moves:
//
//   - panels: Update compares the open panels before and after every message
//     (trackAnimations), so F-keys, /panel, auto-open and every other path
//     animate without touching their call sites. The layout changes at once;
//     the panel's content slides within its slot (open) or as an overlay
//     ghost over the freed columns (close), so no frame re-wraps the
//     transcript.
//   - scroll: wheel ticks move a target, a critically damped spring glides
//     the viewport offset to it.
//
// Off when stdout is not a TTY or config "animations" is false.

const (
	animFPS             = 60
	panelSpringFreq     = 9.0
	scrollSpringFreq    = 12.0
	springDamping       = 1.0 // critically damped: glide, no overshoot past the content
	springSettleDist    = 0.4
	panelSettleDist     = 0.01
	scrollWheelLines    = 3
	panelSlotWidth      = memoryPanelWidth // every side panel has the same slot width
	animTickInterval    = time.Second / animFPS
	scrollSnapVelocity  = 0.4
	panelSnapVelocity   = 0.01
	noScrollAnimOffsets = 0
)

// animationsEnabled is set once at startup from the terminal and config; it
// stays false in tests unless a test turns it on.
var animationsEnabled = false

type animTickMsg struct{}

func animTick() tea.Cmd {
	return tea.Tick(animTickInterval, func(time.Time) tea.Msg { return animTickMsg{} })
}

type scrollAnim struct {
	pos, vel, target float64
	active           bool
	lastSet          int  // the offset this animation last wrote; a different offset means someone else scrolled
	attached         bool // which viewport the animation drives
}

type panelAnim struct {
	pos, vel, target float64
	ghost            []string // closing panels: the last rendered rows, drawn over the freed columns
	ghostX           int
}

// animState is shared by value copies of the model (a pointer) and only
// mutated from Update, never from View.
type animState struct {
	panelSpring  harmonica.Spring
	scrollSpring harmonica.Spring
	scroll       scrollAnim
	panels       map[panelRegion]*panelAnim
	ticking      bool
}

func newAnimState() *animState {
	return &animState{
		panelSpring:  harmonica.NewSpring(harmonica.FPS(animFPS), panelSpringFreq, springDamping),
		scrollSpring: harmonica.NewSpring(harmonica.FPS(animFPS), scrollSpringFreq, springDamping),
		panels:       map[panelRegion]*panelAnim{},
	}
}

func (m *model) animState() *animState {
	if m.anim == nil {
		m.anim = newAnimState()
	}
	return m.anim
}

// startAnimTick arms the shared tick if it is not already running.
func (a *animState) startTick() tea.Cmd {
	if a.ticking {
		return nil
	}
	a.ticking = true
	return animTick()
}

func (a *animState) busy() bool {
	return a.scroll.active || len(a.panels) > 0
}

// --- scroll inertia ---

// smoothScroll moves the viewport by delta lines (negative = up). With
// animations on it only moves the target and lets the spring glide there.
func (m *model) smoothScroll(vp *viewport.Model, delta int) tea.Cmd {
	if !animationsEnabled {
		if delta < 0 {
			vp.ScrollUp(-delta)
		} else {
			vp.ScrollDown(delta)
		}
		return nil
	}
	a := m.animState()
	s := &a.scroll
	attached := m.attachActive()
	if !s.active || s.attached != attached || vp.YOffset() != s.lastSet {
		s.pos, s.vel = float64(vp.YOffset()), 0
		s.target = s.pos
		s.lastSet = vp.YOffset()
		s.attached = attached
	}
	maxOffset := max(vp.TotalLineCount()-vp.Height(), noScrollAnimOffsets)
	s.target = math.Min(math.Max(s.target+float64(delta), 0), float64(maxOffset))
	s.active = true
	return a.startTick()
}

func (m *model) stepScroll(a *animState) {
	s := &a.scroll
	if !s.active {
		return
	}
	vp := m.activeViewport()
	if s.attached != m.attachActive() || vp.YOffset() != s.lastSet {
		s.active = false // the view changed or something else scrolled: stand down
		return
	}
	s.pos, s.vel = a.scrollSpring.Update(s.pos, s.vel, s.target)
	if math.Abs(s.target-s.pos) < springSettleDist && math.Abs(s.vel) < scrollSnapVelocity {
		vp.SetYOffset(int(math.Round(s.target)))
		s.active = false
		return
	}
	vp.SetYOffset(int(math.Round(s.pos)))
	s.lastSet = vp.YOffset()
}

// --- panel slide ---

// trackAnimations starts a slide for every panel whose visibility differs
// between the model before a message and after it.
func (m *model) trackAnimations(prev *model) tea.Cmd {
	if !animationsEnabled || m.height <= 0 {
		return nil
	}
	a := m.animState()
	changed := false
	for _, region := range panelRenderOrder {
		was, is := prev.panelOpen(region), m.panelOpen(region)
		switch {
		case !was && is:
			if pa, ok := a.panels[region]; ok {
				pa.target, pa.ghost = 1, nil // reopened while closing: reverse
			} else {
				a.panels[region] = &panelAnim{target: 1}
			}
			changed = true
		case was && !is && prev.isRightmostPanel(region):
			a.panels[region] = &panelAnim{
				pos: 1, target: 0,
				ghost:  strings.Split(prev.panelBlock(region, prev.viewportHeight()), "\n"),
				ghostX: prev.panelX(region),
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return a.startTick()
}

// panelX is the left column of a panel's slot in the current layout.
func (m *model) panelX(region panelRegion) int {
	x := m.mainWidth()
	for _, r := range panelRenderOrder {
		if r == region {
			break
		}
		if m.panelOpen(r) {
			x += panelSlotWidth
		}
	}
	return x
}

// isRightmostPanel reports whether no open panel sits right of region.
func (m *model) isRightmostPanel(region panelRegion) bool {
	seen := false
	for _, r := range panelRenderOrder {
		if r == region {
			seen = true
			continue
		}
		if seen && m.panelOpen(r) {
			return false
		}
	}
	return true
}

func (m *model) stepPanels(a *animState) {
	for region, pa := range a.panels {
		pa.pos, pa.vel = a.panelSpring.Update(pa.pos, pa.vel, pa.target)
		if math.Abs(pa.target-pa.pos) < panelSettleDist && math.Abs(pa.vel) < panelSnapVelocity {
			delete(a.panels, region)
		}
	}
}

// stepAnimations advances every running spring one frame and re-arms the tick
// while anything still moves.
func (m *model) stepAnimations() tea.Cmd {
	a := m.animState()
	m.stepScroll(a)
	m.stepPanels(a)
	if !a.busy() {
		a.ticking = false
		return nil
	}
	return animTick()
}

// slideShift is how many columns a panel is still displaced from its slot.
func (a *animState) slideShift(region panelRegion) (int, bool) {
	if a == nil {
		return 0, false
	}
	pa, ok := a.panels[region]
	if !ok || pa.ghost != nil {
		return 0, false
	}
	shift := int(math.Round((1 - math.Min(math.Max(pa.pos, 0), 1)) * panelSlotWidth))
	return shift, shift > 0
}

// slideRight displaces a block shift columns to the right inside its own
// width: blank columns on the left, the block's left part on the right.
func slideRight(block string, shift int) string {
	if shift <= 0 {
		return block
	}
	lines := strings.Split(block, "\n")
	pad := strings.Repeat(" ", min(shift, panelSlotWidth))
	for i, line := range lines {
		lines[i] = pad + ansi.Truncate(line, max(panelSlotWidth-shift, 0), "")
	}
	return strings.Join(lines, "\n")
}

// overlayClosingPanels draws the ghosts of panels sliding out to the right.
func (m *model) overlayClosingPanels(block string) string {
	if m.anim == nil {
		return block
	}
	var layers []overlayLayer
	for _, region := range panelRenderOrder {
		pa, ok := m.anim.panels[region]
		if !ok || pa.ghost == nil {
			continue
		}
		shift := int(math.Round((1 - math.Min(math.Max(pa.pos, 0), 1)) * panelSlotWidth))
		if shift >= panelSlotWidth {
			continue
		}
		rows := make([]string, len(pa.ghost))
		for i, line := range pa.ghost {
			rows[i] = ansi.Truncate(line, panelSlotWidth-shift, "")
		}
		layers = append(layers, overlayLayer{content: strings.Join(rows, "\n"), x: pa.ghostX + shift})
	}
	return composeOverlays(block, layers...)
}

// joinPanel appends one side panel (body plus scrollbar) to the main area,
// displaced while its slide-in runs.
func (m *model) joinPanel(mainArea string, region panelRegion, h int) string {
	block := m.panelBlock(region, h)
	if shift, sliding := m.anim.slideShift(region); sliding {
		block = slideRight(block, shift)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, mainArea, block)
}

// panelBlock renders a region's body and scrollbar as one block.
func (m *model) panelBlock(region panelRegion, h int) string {
	var panel, bar string
	switch region {
	case regionMemory:
		panel, bar = m.renderMemoryPanel(h), m.renderPanelScrollbar(h)
	case regionTasks:
		panel, bar = m.renderTasksPanel(h), m.renderTasksPanelScrollbar(h)
	case regionBackground:
		panel, bar = m.renderBackgroundPanel(h), m.renderBackgroundPanelScrollbar(h)
	case regionWorkflow:
		panel, bar = m.renderWorkflowPanel(h), m.renderWorkflowPanelScrollbar(h)
	case regionProject:
		panel, bar = m.renderProjectPanel(h), m.renderProjectPanelScrollbar(h)
	case regionGit:
		panel, bar = m.renderGitPanel(h), m.renderGitPanelScrollbar(h)
	default:
		return ""
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, panel, bar)
}
