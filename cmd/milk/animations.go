package main

import (
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/harmonica"
	"github.com/charmbracelet/x/ansi"
	viewport "github.com/scoutme/milk/internal/vp"
)

// Spring animations (harmonica): side panels slide in and out, and the mouse
// wheel scrolls with inertia. Both are pure view effects driven by one 60 fps
// tick that runs only while something moves:
//
//   - panels: Update compares open state and columns before and after every
//     message (trackAnimations), so F-keys, /panel, auto-open and every other
//     path animate without touching their call sites. The layout changes at
//     once; the panel's content slides within its slot (open), shrinks out as
//     an overlay ghost over the freed columns (close — any panel, not just the
//     right-most), and the panels left of a toggled panel glide one column
//     with a full-width ghost (right on close, left on open) instead of
//     teleporting, so no frame re-wraps the transcript.
//   - scroll: wheel ticks move a target, a critically damped spring glides
//     the viewport offset to it.
//
// Off when stdout is not a TTY or config "animations" is false.

const (
	animFPS          = 60
	panelSpringFreq  = 24.0 // frequency the springs treat as their rate: 24 lands an exit in ~250-300 ms, still critically damped
	scrollSpringFreq = 12.0
	springDamping    = 1.0 // critically damped: glide, no overshoot past the content
	springSettleDist = 0.4
	// Panel springs settle fast: keep the tail tiny so the last columns don't
	// crawl (the old 0.01/0.01 thresholds made the exit read as sluggish).
	panelSettleDist     = 0.04
	scrollWheelLines    = 3
	panelSlotWidth      = memoryPanelWidth // every side panel has the same slot width
	animTickInterval    = time.Second / animFPS
	scrollSnapVelocity  = 0.4
	panelSnapVelocity   = 0.12 // ~3 columns/s of drift at the snap — invisible, and it ends the crawling tail
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
	ghost            []string // closing: last frame over the freed columns; moving: frame at the old column
	ghostX           int
	move             bool // gliding to a new column (a panel closed elsewhere): full-width translate instead of a shrink-out
	travel           int  // columns between the old and the new column
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

// trackAnimations starts a slide for every panel whose visibility or column
// differs between the model before a message and after it:
//
//   - opening: content slides in inside the reserved slot;
//   - closing: a ghost of the last frame shrinks out — wherever the panel is,
//     not only the right-most one (its right edge never crosses the next
//     panel's left edge, so neighbours are never overwritten);
//   - column changed: the layout is right-anchored, so a toggle shifts every
//     panel left of it by one column (right on close, left on open). Those
//     panels glide to their new column as full-width ghosts while their
//     static slot renders blank — without this they teleport at t=0 and the
//     toggle reads as a jump.
//
// A tabbed-theme tab switch is none of these: the panels share one slot and
// only the slot's content changes, so it animates nothing (ADR-0079).
func (m *model) trackAnimations(prev *model) tea.Cmd {
	if !animationsEnabled || m.height <= 0 {
		return nil
	}
	// A tabbed-theme tab switch swaps panels inside one shared slot (ADR-0079):
	// the slot stays occupied, only its content changes. Swap in place —
	// instant, like browser tabs — instead of stacking a close ghost and a
	// slide-in on top of each other in the same columns.
	if panelTabsActive() && prev.anyPanelVisible() && m.anyPanelVisible() {
		return nil
	}
	a := m.animState()
	changed := false
	for _, region := range panelRenderOrder {
		was, is := prev.panelVisible(region), m.panelVisible(region)
		switch {
		case !was && is:
			if pa, ok := a.panels[region]; ok {
				pa.target, pa.ghost = 1, nil  // reopened while closing: reverse
				pa.move, pa.travel = false, 0 // …and drop any stale glide
			} else {
				a.panels[region] = &panelAnim{target: 1}
			}
			changed = true
		case was && !is:
			a.panels[region] = &panelAnim{
				pos: 1, target: 0,
				ghost:  strings.Split(prev.panelBlock(region, prev.viewportHeight()), "\n"),
				ghostX: prev.panelX(region),
			}
			changed = true
		case was && is:
			oldX, newX := prev.panelX(region), m.panelX(region)
			if oldX != newX {
				a.panels[region] = &panelAnim{
					pos: 0, target: 1,
					ghost:  strings.Split(prev.panelBlock(region, prev.viewportHeight()), "\n"),
					ghostX: oldX,
					move:   true,
					travel: newX - oldX,
				}
				changed = true
			}
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
		if m.panelVisible(r) {
			x += panelSlotWidth
		}
	}
	return x
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

// isMoving reports whether a panel is mid-glide to its new column.
func (a *animState) isMoving(region panelRegion) bool {
	if a == nil {
		return false
	}
	pa, ok := a.panels[region]
	return ok && pa.move
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

// overlayPanelTransitions draws the ghosts of panels in flight: closers
// shrinking out toward their right edge, and movers translating full-width
// from their old column to the new one in either direction. Entries created
// on the same frame share one spring; a leftward glide measures its offset
// from the destination with the same rounding shape the opening slot's
// slide-in uses, so the two layers always fall on the same seam column.
func (m *model) overlayPanelTransitions(block string) string {
	if m.anim == nil {
		return block
	}
	var layers []overlayLayer
	for _, region := range panelRenderOrder {
		pa, ok := m.anim.panels[region]
		if !ok || pa.ghost == nil {
			continue
		}
		if pa.move {
			pos := math.Min(math.Max(pa.pos, 0), 1)
			var x int
			if pa.travel >= 0 {
				x = pa.ghostX + int(math.Round(pos*float64(pa.travel)))
			} else {
				x = pa.ghostX + pa.travel + int(math.Round((1-pos)*float64(-pa.travel)))
			}
			layers = append(layers, overlayLayer{content: strings.Join(pa.ghost, "\n"), x: x})
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
// displaced while its slide-in runs. A panel mid-glide renders its slot
// blank: overlayPanelTransitions paints its ghost at the old column, and a
// static copy alongside would show the panel twice.
func (m *model) joinPanel(mainArea string, region panelRegion, h int) string {
	var block string
	if m.anim.isMoving(region) {
		block = blankPanelBlock(h)
	} else {
		block = m.panelBlock(region, h)
		if shift, sliding := m.anim.slideShift(region); sliding {
			block = slideRight(block, shift)
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, mainArea, block)
}

// blankPanelBlock is an empty panel-sized block (h rows of panelSlotWidth
// spaces), so a gliding panel's slot keeps its width in the layout.
func blankPanelBlock(h int) string {
	rows := make([]string, h)
	for i := range rows {
		rows[i] = strings.Repeat(" ", panelSlotWidth)
	}
	return strings.Join(rows, "\n")
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
