package theme

import "testing"

// TestResolveDerivations pins the "common basis" maths: a six-colour theme
// resolves complete (golden values — these are the numbers the theme builder's
// preview will show, so they may only change on purpose).
func TestResolveDerivations(t *testing.T) {
	six := &Theme{Name: "six", Colors: Palette{
		Base:     Fixed("#20242c"),
		Text:     Fixed("#e8e6e0"),
		Muted:    Fixed("#8b8b8b"),
		Accent:   Fixed("#d0a93c"),
		Hairline: Fixed("#3a3f49"),
	}}
	out := resolve([]*Theme{six})
	for _, tc := range []struct{ name, got, want string }{
		{"raised", out.Colors.Raised.Dark, "#313744"},
		{"toast", out.Colors.Toast.Dark, "#292e38"},
		{"thumb", out.Colors.Thumb.Dark, "#63656a"},
		{"accent_lo", out.Colors.AccentLo.Dark, "#554c31"},
		{"think", out.Colors.Think.Dark, "#8b8b8b"},
		{"focus", out.Colors.Focus.Dark, "#3a382e"},
	} {
		if tc.got != tc.want {
			t.Errorf("derived %s = %s, want %s", tc.name, tc.got, tc.want)
		}
	}
	// A theme with no explicit shape defaults to the skin render path.
	if out.Look != LookSkin || out.ToolRows != ToolRowsAccent || out.Scrollbar != ScrollbarSlim {
		t.Errorf("defaults: look=%s toolRows=%s scrollbar=%s", out.Look, out.ToolRows, out.Scrollbar)
	}
}

// TestResolveOffIsMinimal: "off" resolves to exactly what it declares — no
// derivation may invent tokens for the classic look (its chrome colours are
// the render layer's own).
func TestResolveOffIsMinimal(t *testing.T) {
	off := resolve([]*Theme{builtinOff})
	if off.Look != LookClassic || off.ToolRows != ToolRowsPlain || off.Scrollbar != ScrollbarClassic {
		t.Errorf("off shape: %s %s %s", off.Look, off.ToolRows, off.Scrollbar)
	}
	if off.Colors.Focus != (Color{Dark: "#161d2e", Light: "#e2e7f3"}) {
		t.Errorf("off focus = %+v", off.Colors.Focus)
	}
	if !off.Colors.Base.Absent() || !off.Colors.Text.Absent() || !off.Colors.Accent.Absent() {
		t.Errorf("off must keep its non-focus tokens absent, got %+v", off.Colors)
	}
	if off.Terminal != nil || off.Tagline != "" {
		t.Errorf("off terminal=%v tagline=%q", off.Terminal, off.Tagline)
	}
}

// TestResolveSuperMilkExplicit: supermilk declares every token, so its
// resolution is a no-op over the declared values (the cmd/milk parity test
// pins the same numbers against the historical themeDef literal).
func TestResolveSuperMilkExplicit(t *testing.T) {
	sm := resolve([]*Theme{builtinSuperMilk})
	if sm.Colors != builtinSuperMilk.Colors {
		t.Errorf("supermilk palette drifted:\n got %+v\nwant %+v", sm.Colors, builtinSuperMilk.Colors)
	}
	if sm.Tagline != "Got models? Got milk." || sm.Look != LookSkin {
		t.Errorf("supermilk meta: %q %s", sm.Tagline, sm.Look)
	}
	if sm.Terminal == nil || len(sm.Terminal.Ansi16) != 16 ||
		sm.Terminal.Ansi16[15] != "#2b1a17" {
		t.Errorf("supermilk terminal: %+v", sm.Terminal)
	}
}

// TestResolveInheritance: a child overrides what it declares and inherits the
// rest; partial palette entries merge per variant.
func TestResolveInheritance(t *testing.T) {
	parent := &Theme{Name: "parent", Look: LookSkin, Tagline: "parent line",
		Colors: Palette{Base: Fixed("#101010"), Text: Fixed("#eeeeee")}}
	child := &Theme{Name: "child", Extends: "parent",
		Colors: Palette{Base: Color{Dark: "#202020"}}}
	out := resolve([]*Theme{parent, child})
	// Per-variant merge: the child overrides dark only; light inherits.
	if out.Colors.Base != (Color{Dark: "#202020", Light: "#101010"}) {
		t.Errorf("child base = %+v", out.Colors.Base)
	}
	if out.Colors.Text != Fixed("#eeeeee") {
		t.Errorf("child text = %+v", out.Colors.Text)
	}
	if out.Tagline != "parent line" {
		t.Errorf("tagline = %q", out.Tagline)
	}
}

func TestDeriveANSI16Golden(t *testing.T) {
	ramp := DeriveANSI16(Palette{
		Base:  Fixed("#101418"),
		Text:  Fixed("#e6e6e6"),
		Muted: Fixed("#9a9a9a"),
	})
	want := [16]string{
		"#101418", "#bd7f7f", "#7fbd7f", "#bdb87f", "#7f8fbd", "#bd7fae", "#7fbdbd", "#e6e6e6",
		"#9a9a9a", "#d6a4a4", "#a4d6a4", "#d6d2a4", "#a4b0d6", "#d6a4c9", "#a4d6d6", "#edebe9",
	}
	if ramp != want {
		t.Errorf("DeriveANSI16:\n got %v\nwant %v", ramp, want)
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(&Theme{Name: "ok_name-1", Look: LookSkin}); err != nil {
		t.Errorf("valid theme rejected: %v", err)
	}
	for _, bad := range []*Theme{
		{Name: "Bad Name"},
		{Name: "x", Look: "neon"},
		{Name: "x", ToolRows: "sparkly"},
		{Name: "x", Scrollbar: "fat"},
		{Name: "x", Colors: Palette{Base: Fixed("#zzz")}},
		{Name: "x", Terminal: &Terminal{Ansi16: make([]string, 17)}},
	} {
		if err := Validate(bad); err == nil {
			t.Errorf("Validate(%+v) accepted, want error", bad)
		}
	}
}
