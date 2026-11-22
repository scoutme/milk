// Package theme defines milk's TUI themes as data: a theme is a JSON overlay
// on a common palette basis, resolved through an `extends` chain, with the
// few shape decisions the render layer needs expressed as small enums — never
// per-theme code (plan: docs/internal/plans/config-themes-carousel-wizard-plan.md;
// supersedes the code-only skins of ADR-0077 in part).
//
// The package is pure logic plus file discovery: no TUI, no lipgloss, no
// terminal. cmd/milk converts a Resolved into its runtime themeDef and owns
// all rendering; the carousel and theme-builder wizard (later steps) build
// candidate Themes here and preview them through the same conversion.
package theme

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Color is one theme token: a dark-ground and a light-ground variant, picked
// by the detected terminal background. Fixed(hex) is a token that reads the
// same on any ground. The zero Color (both variants "") means "absent": the
// resolver derives it or, with no basis to derive from, leaves it empty and
// the render layer falls back to its own defaults (the "off" theme relies on
// this — its hardcoded chrome colours are the design, not a gap).
type Color struct {
	Dark  string
	Light string
}

// Fixed is a token that reads the same on any terminal ground.
func Fixed(hex string) Color { return Color{Dark: hex, Light: hex} }

// Absent reports whether the token carries no colour at all.
func (c Color) Absent() bool { return c.Dark == "" && c.Light == "" }

// UnmarshalJSON accepts both JSON forms of a token: "#rrggbb" (fixed) and
// {"dark":"#rrggbb","light":"#rrggbb"}. Hex forms are normalized to
// lowercase "#rrggbb"; "#rgb" expands.
func (c *Color) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		hex, err := ParseHex(s)
		if err != nil {
			return err
		}
		*c = Fixed(hex)
		return nil
	}
	var pair struct {
		Dark  string `json:"dark"`
		Light string `json:"light"`
	}
	if err := json.Unmarshal(b, &pair); err != nil {
		return fmt.Errorf("theme color: want \"#rrggbb\" or {\"dark\",\"light\"}: %w", err)
	}
	dark, err := ParseHex(pair.Dark)
	if err != nil {
		return fmt.Errorf("theme color dark: %w", err)
	}
	light, err := ParseHex(pair.Light)
	if err != nil {
		return fmt.Errorf("theme color light: %w", err)
	}
	*c = Color{Dark: dark, Light: light}
	return nil
}

// MarshalJSON writes the compact form: a fixed hex string when both variants
// agree, the pair object otherwise.
func (c Color) MarshalJSON() ([]byte, error) {
	if c.Dark == c.Light {
		return json.Marshal(c.Dark)
	}
	return json.Marshal(struct {
		Dark  string `json:"dark"`
		Light string `json:"light"`
	}{c.Dark, c.Light})
}

// ParseHex validates "#rgb"/"#rrggbb" (case-insensitive) and normalizes to
// lowercase "#rrggbb". An empty string parses as "" (the "absent" token).
func ParseHex(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if !strings.HasPrefix(s, "#") {
		return "", fmt.Errorf("theme color %q: want #rgb or #rrggbb", s)
	}
	digits := s[1:]
	switch len(digits) {
	case 3:
		digits = string([]byte{digits[0], digits[0], digits[1], digits[1], digits[2], digits[2]})
	case 6:
	default:
		return "", fmt.Errorf("theme color %q: want #rgb or #rrggbb", s)
	}
	if _, err := strconv.ParseUint(digits, 16, 32); err != nil {
		return "", fmt.Errorf("theme color %q: want #rgb or #rrggbb", s)
	}
	return "#" + strings.ToLower(digits), nil
}

// hexRGB splits a normalized "#rrggbb" into 0–255 components.
func hexRGB(h string) (r, g, b float64) {
	v, _ := strconv.ParseUint(h[1:], 16, 32)
	return float64(v >> 16), float64(v >> 8 & 0xff), float64(v & 0xff)
}

// rgbHex formats components back to "#rrggbb", rounding and clamping.
func rgbHex(r, g, b float64) string {
	clamp := func(v float64) uint32 {
		v = math.Round(v)
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return uint32(v)
	}
	return fmt.Sprintf("#%02x%02x%02x", clamp(r), clamp(g), clamp(b))
}

// Mix blends token a toward token b by t (0 = a, 1 = b) in linear component
// space, per ground variant. Used by the derivations — a hand-written theme
// declares six colours and the resolver completes the rest.
func Mix(a, b Color, t float64) Color {
	mix := func(x, y string) string {
		if x == "" || y == "" {
			return ""
		}
		xr, xg, xb := hexRGB(x)
		yr, yg, yb := hexRGB(y)
		return rgbHex(xr+(yr-xr)*t, xg+(yg-xg)*t, xb+(yb-xb)*t)
	}
	return Color{Dark: mix(a.Dark, b.Dark), Light: mix(a.Light, b.Light)}
}

// Lighten shifts a token's HSL lightness by delta (-1…1, clamped), per ground
// variant. Positive brightens; saturation and hue are preserved.
func Lighten(c Color, delta float64) Color {
	shift := func(x string) string {
		if x == "" {
			return ""
		}
		h, s, l := hexToHSL(x)
		return hslToHex(h, s, clamp01(l+delta))
	}
	return Color{Dark: shift(c.Dark), Light: shift(c.Light)}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// hexToHSL converts "#rrggbb" to hue (0–360), saturation and lightness (0–1).
func hexToHSL(hex string) (h, s, l float64) {
	r, g, b := hexRGB(hex)
	r, g, b = r/255, g/255, b/255
	max := math.Max(r, math.Max(g, b))
	min := math.Min(r, math.Min(g, b))
	l = (max + min) / 2
	if max == min {
		return 0, 0, l
	}
	d := max - min
	if l > 0.5 {
		s = d / (2 - max - min)
	} else {
		s = d / (max + min)
	}
	switch max {
	case r:
		h = (g - b) / d
		if g < b {
			h += 6
		}
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	return h * 60, s, l
}

// hslToHex converts hue (0–360), saturation and lightness (0–1) to "#rrggbb".
func hslToHex(h, s, l float64) string {
	if s == 0 {
		return rgbHex(l*255, l*255, l*255)
	}
	var q float64
	if l < 0.5 {
		q = l * (1 + s)
	} else {
		q = l + s - l*s
	}
	p := 2*l - q
	hk := math.Mod(h, 360) / 360
	channel := func(t float64) float64 {
		if t < 0 {
			t++
		}
		if t > 1 {
			t--
		}
		switch {
		case t < 1.0/6:
			return p + (q-p)*6*t
		case t < 1.0/2:
			return q
		case t < 2.0/3:
			return p + (q-p)*(2.0/3-t)*6
		default:
			return p
		}
	}
	return rgbHex(channel(hk+1.0/3)*255, channel(hk)*255, channel(hk-1.0/3)*255)
}
