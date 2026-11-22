package theme

import "testing"

// TestTransformGolden pins the darken/lighten operator the theme builder's
// variant mode steps through (−25%…+25% lightness): the whole palette moves
// together, tokens and terminal ramp alike.
func TestTransformGolden(t *testing.T) {
	sm := resolve([]*Theme{builtinSuperMilk})
	got := Transform(sm, -0.10, 0.05)
	for _, tc := range []struct{ name, got, want string }{
		{"base", got.Colors.Base.Dark, "#ebe1c8"},
		{"text", got.Colors.Text.Dark, "#201412"},
		{"accent", got.Colors.Accent.Dark, "#4e462a"},
		{"ansi[0]", got.Terminal.Ansi16[0], "#ebe1c8"},
		{"ansi[1]", got.Terminal.Ansi16[1], "#6a3529"},
	} {
		if tc.got != tc.want {
			t.Errorf("transform %s = %s, want %s", tc.name, tc.got, tc.want)
		}
	}
	// Input is never mutated.
	if sm.Colors.Base.Dark != "#f8f5ee" || sm.Terminal.Ansi16[1] != "#8a4b3c" {
		t.Error("Transform must not mutate its input")
	}
}

// TestResolveChromeExtends: the chrome block merges along the extends chain
// like the palette; a partial declaration leaves the rest absent (the render
// layer falls back per surface), and a short pulse ramp pads with its last
// entry.
func TestResolveChromeExtends(t *testing.T) {
	base := &Theme{Name: "base", Colors: Palette{Base: Fixed("#111111")},
		Chrome: Chrome{PanelTitle: Fixed("#888888"), Pulse: []string{"#111111", "#222222"}}}
	child := &Theme{Name: "child", Extends: "base",
		Chrome: Chrome{PanelTitle: Fixed("#999999")}}
	out := resolve([]*Theme{base, child})
	if out.Chrome.PanelTitle != (Color{Dark: "#999999", Light: "#999999"}) {
		t.Errorf("child must override the chrome surface, got %+v", out.Chrome.PanelTitle)
	}
	if !out.Chrome.Border.Absent() || !out.Chrome.StaleMid.Absent() {
		t.Errorf("undeclared chrome must stay absent, got %+v", out.Chrome)
	}
	if len(out.Chrome.Pulse) != 16 || out.Chrome.Pulse[0] != "#111111" || out.Chrome.Pulse[15] != "#222222" {
		t.Errorf("short pulse must pad with its last entry, got %v", out.Chrome.Pulse)
	}
}

// TestTransformChrome: the chrome block moves with the palette — the point
// of declaring the classic surfaces as data. Undeclared surfaces stay absent.
func TestTransformChrome(t *testing.T) {
	off := resolve([]*Theme{builtinOff})
	got := Transform(off, -0.10, 0)
	if got.Chrome.PanelTitle == off.Chrome.PanelTitle {
		t.Error("panel title must shift")
	}
	if got.Chrome.StaleMid == off.Chrome.StaleMid {
		t.Error("staleness ramp must shift")
	}
	if got.Chrome.Pulse[8] == off.Chrome.Pulse[8] || len(got.Chrome.Pulse) != 16 {
		t.Error("pulse ramp must shift in full")
	}
	mini := resolve([]*Theme{{Name: "mini", Colors: Palette{Base: Fixed("#111111")}}})
	got2 := Transform(mini, -0.10, 0)
	if !got2.Chrome.PanelTitle.Absent() || got2.Chrome.Pulse != nil {
		t.Errorf("undeclared chrome must stay absent, got %+v", got2.Chrome)
	}
}

func TestTransformAbsentStaysAbsent(t *testing.T) {
	off := resolve([]*Theme{{Name: "mini", Colors: Palette{Focus: Color{Dark: "#161d2e", Light: "#e2e7f3"}}}})
	got := Transform(off, -0.2, 0)
	if !got.Colors.Base.Absent() || !got.Colors.Text.Absent() {
		t.Errorf("absent tokens must stay absent, got %+v", got.Colors)
	}
	if got.Colors.Focus == off.Colors.Focus {
		t.Error("present tokens must shift")
	}
	// Ramp "" entries survive.
	sm := resolve([]*Theme{builtinSuperMilk})
	sm.Terminal.Ansi16[5] = ""
	got2 := Transform(sm, -0.05, 0)
	if got2.Terminal.Ansi16[5] != "" {
		t.Errorf("empty ramp slot shifted to %q", got2.Terminal.Ansi16[5])
	}
}

func TestTransformClamps(t *testing.T) {
	sm := resolve([]*Theme{builtinSuperMilk})
	got := Transform(sm, 1, 1)
	if got.Colors.Base.Dark != "#ffffff" {
		t.Errorf("clamp lightness: %s", got.Colors.Base.Dark)
	}
	got = Transform(sm, -1, -1)
	if l := hexToHSLLightness(got.Colors.Base.Dark); l != 0 {
		t.Errorf("clamp to black: %s (l=%v)", got.Colors.Base.Dark, l)
	}
}

func hexToHSLLightness(hex string) float64 {
	_, _, l := hexToHSL(hex)
	return l
}
