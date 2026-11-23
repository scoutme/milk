package theme

// DeriveANSI16 generates a readable same-family ANSI-16 ramp for a palette:
// the ground in slot 0, ink in 7/8, the six standard hues at contrast
// lightness for the ground (1 red … 6 cyan), their bright variants in 9–14,
// and a bright neutral in 15. supermilk carries its hand-tuned ramp
// explicitly; this derivation is the fallback for themes that declare a
// terminal block without an ansi16 — hand-tuned ramps stay a JSON feature.
func DeriveANSI16(p Palette) [16]string {
	groundL := 0.9 // assume a light ground when no base is declared
	base := p.Base.Dark
	if base == "" {
		base = p.Base.Light
	}
	if base != "" {
		_, _, groundL = hexToHSL(base)
	}
	lightGround := groundL >= 0.5
	inkL := 0.62
	if lightGround {
		inkL = 0.42
	}
	// ANSI hue for slots 1–6 (red, green, yellow, blue, magenta, cyan).
	hues := [6]float64{0, 120, 55, 225, 315, 180}

	var out [16]string
	out[0] = p.Base.Dark
	out[7] = p.Text.Dark
	out[8] = p.Muted.Dark
	for i, h := range hues {
		out[1+i] = hslToHex(h, 0.32, inkL)
		out[9+i] = hslToHex(h, 0.38, clamp01(inkL+0.12))
	}
	if lightGround {
		out[15] = hslToHex(20, 0.20, 0.10) // bright neutral on light ground: near-black warm
	} else {
		out[15] = hslToHex(40, 0.10, 0.92)
	}
	return out
}
