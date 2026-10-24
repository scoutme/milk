package main

import tea "charm.land/bubbletea/v2"

// mouseAction is the phase of a mouse event. Bubble Tea v2 splits it across
// message types (click, release, motion, wheel); the handlers here reason in
// one flat event, so msgs are normalized once at the Update boundary.
type mouseAction int

const (
	mousePress mouseAction = iota
	mouseRelease
	mouseMotion
)

// mouseEvent is a flat view of a tea.MouseMsg. Wheel ticks count as presses.
type mouseEvent struct {
	X, Y             int
	Action           mouseAction
	Button           tea.MouseButton
	Shift, Alt, Ctrl bool
}

func newMouseEvent(msg tea.MouseMsg) mouseEvent {
	mouse := msg.Mouse()
	ev := mouseEvent{
		X:      mouse.X,
		Y:      mouse.Y,
		Button: mouse.Button,
		Shift:  mouse.Mod.Contains(tea.ModShift),
		Alt:    mouse.Mod.Contains(tea.ModAlt),
		Ctrl:   mouse.Mod.Contains(tea.ModCtrl),
	}
	switch msg.(type) {
	case tea.MouseReleaseMsg:
		ev.Action = mouseRelease
	case tea.MouseMotionMsg:
		ev.Action = mouseMotion
	default:
		ev.Action = mousePress
	}
	return ev
}

// msg converts the flat event back into the matching Bubble Tea message.
func (ev mouseEvent) msg() tea.MouseMsg {
	var mod tea.KeyMod
	if ev.Shift {
		mod |= tea.ModShift
	}
	if ev.Alt {
		mod |= tea.ModAlt
	}
	if ev.Ctrl {
		mod |= tea.ModCtrl
	}
	mouse := tea.Mouse{X: ev.X, Y: ev.Y, Button: ev.Button, Mod: mod}
	switch {
	case ev.Action == mouseRelease:
		return tea.MouseReleaseMsg(mouse)
	case ev.Action == mouseMotion:
		return tea.MouseMotionMsg(mouse)
	case ev.Button >= tea.MouseWheelUp && ev.Button <= tea.MouseWheelRight:
		return tea.MouseWheelMsg(mouse)
	default:
		return tea.MouseClickMsg(mouse)
	}
}
