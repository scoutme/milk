package theme

// Transform shifts a resolved theme's whole palette by HSL lightness and
// saturation deltas (−1…1 each, clamped) — the darken/lighten variant
// operator behind the theme builder's variant mode. Every present colour
// moves together (palette tokens and the terminal ramp), so a variant stays
// one coherent look. The input is never mutated; the result is a fresh copy.
// Absent tokens stay absent, and "" entries in the ramp are left alone.
func Transform(rt *Resolved, dLight, dSat float64) *Resolved {
	out := *rt
	out.Colors = mapPalette(rt.Colors, dLight, dSat)
	if rt.Terminal != nil {
		t := *rt.Terminal
		t.Bg = shiftHex(t.Bg, dLight, dSat)
		t.Fg = shiftHex(t.Fg, dLight, dSat)
		t.Cursor = shiftHex(t.Cursor, dLight, dSat)
		t.Ansi16 = make([]string, len(rt.Terminal.Ansi16))
		for i, c := range rt.Terminal.Ansi16 {
			t.Ansi16[i] = shiftHex(c, dLight, dSat)
		}
		out.Terminal = &t
	}
	return &out
}

func mapPalette(p Palette, dLight, dSat float64) Palette {
	shift := func(c Color) Color {
		return Color{Dark: shiftHex(c.Dark, dLight, dSat), Light: shiftHex(c.Light, dLight, dSat)}
	}
	return Palette{
		Base:     shift(p.Base),
		Raised:   shift(p.Raised),
		Toast:    shift(p.Toast),
		Hairline: shift(p.Hairline),
		Thumb:    shift(p.Thumb),
		Text:     shift(p.Text),
		Muted:    shift(p.Muted),
		Accent:   shift(p.Accent),
		AccentLo: shift(p.AccentLo),
		Think:    shift(p.Think),
		Focus:    shift(p.Focus),
	}
}

func shiftHex(hex string, dLight, dSat float64) string {
	if hex == "" {
		return ""
	}
	h, s, l := hexToHSL(hex)
	return hslToHex(h, clamp01(s+dSat), clamp01(l+dLight))
}
