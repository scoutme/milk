package theme

import (
	"fmt"
	"regexp"
	"strings"
)

// Look is the shape profile a theme renders under — the schema's answer to
// the `th.modern` fork of ADR-0077. It is deliberately an enum, not flags:
// agent/user label layout and the status-bar groups are *shapes*, and only
// two of the shape decisions (tool rows, scrollbar) are per-theme choices.
type Look string

const (
	// LookClassic is the original look: inline agent labels, classic status
	// bar, chrome background tinting, dim recede, the yellow pulse.
	LookClassic Look = "classic"
	// LookSkin is the skin look: block labels, the skin status bar, no
	// chrome tint, muted-ink recede, computed pulse, OSC palette push when
	// the theme carries a terminal block.
	LookSkin Look = "skin"
)

// ToolRows styles tool-activity rows: "accent" (marker accent, name bold
// muted, summary muted) or "plain" (the original dim rows).
type ToolRows string

const (
	ToolRowsAccent ToolRows = "accent"
	ToolRowsPlain  ToolRows = "plain"
)

// Scrollbar picks the scrollbar glyphs: "slim" (hairline track + slim thumb)
// or "classic" (the original dim glyphs).
type Scrollbar string

const (
	ScrollbarSlim    Scrollbar = "slim"
	ScrollbarClassic Scrollbar = "classic"
)

// Palette is the token set every theme draws from. Missing tokens derive
// (see deriveColors) or stay absent — the render layer keeps its own
// fallbacks for absent tokens, which is how "off" stays byte-identical.
type Palette struct {
	Base     Color `json:"base,omitempty"`
	Raised   Color `json:"raised,omitempty"`
	Toast    Color `json:"toast,omitempty"`
	Hairline Color `json:"hairline,omitempty"`
	Thumb    Color `json:"thumb,omitempty"`
	Text     Color `json:"text,omitempty"`
	Muted    Color `json:"muted,omitempty"`
	Accent   Color `json:"accent,omitempty"`
	AccentLo Color `json:"accent_lo,omitempty"`
	Think    Color `json:"think,omitempty"`
	Focus    Color `json:"focus,omitempty"`
}

// Terminal is the emulator palette a skin pushes while active (OSC 10/11/12
// and the 16-colour ramp). Omit the block entirely to leave the terminal's
// own colours alone. Ansi16 entries may be "" to leave one slot alone; the
// slice is padded to 16 on resolve.
type Terminal struct {
	Bg     string   `json:"bg,omitempty"`
	Fg     string   `json:"fg,omitempty"`
	Cursor string   `json:"cursor,omitempty"`
	Ansi16 []string `json:"ansi16,omitempty"`
}

// Theme is one theme as written: the JSON document in a theme file (or the
// builtin definitions in builtin.go). Fields left empty inherit along the
// extends chain and then derive from the palette basis.
type Theme struct {
	Name        string    `json:"name"`
	Extends     string    `json:"extends,omitempty"`
	DerivedFrom string    `json:"derived_from,omitempty"` // metadata only (written by the wizard)
	Look        Look      `json:"look,omitempty"`
	Tagline     string    `json:"tagline,omitempty"`
	ToolRows    ToolRows  `json:"tool_rows,omitempty"`
	Scrollbar   Scrollbar `json:"scrollbar,omitempty"`
	Colors      Palette   `json:"colors,omitempty"`
	Terminal    *Terminal `json:"terminal,omitempty"`
}

// Resolved is a fully merged theme: the extends chain flattened, every
// declared shape decision filled in, derived colours complete. This is what
// cmd/milk converts into its runtime themeDef.
type Resolved struct {
	Name        string
	DerivedFrom string
	Look        Look
	ToolRows    ToolRows
	Scrollbar   Scrollbar
	Tagline     string
	Colors      Palette
	// Terminal is nil when the theme leaves the emulator palette alone.
	Terminal *Terminal
}

// nameSlug is the theme-name rule: lowercase letters, digits, `_`, `-`,
// starting alphanumeric — the same shape as a theme file's basename.
var nameSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Validate checks a written theme for the fields that cannot be defaulted
// away: enum values and hex syntax. Name is validated too when present (a
// file's name may still come from its basename).
func Validate(t *Theme) error {
	if t.Name != "" && !nameSlug.MatchString(t.Name) {
		return fmt.Errorf("theme name %q: want [a-z0-9][a-z0-9_-]*", t.Name)
	}
	switch t.Look {
	case "", LookClassic, LookSkin:
	default:
		return fmt.Errorf("theme %s: look %q: want %q or %q", t.Name, t.Look, LookClassic, LookSkin)
	}
	switch t.ToolRows {
	case "", ToolRowsAccent, ToolRowsPlain:
	default:
		return fmt.Errorf("theme %s: tool_rows %q: want %q or %q", t.Name, t.ToolRows, ToolRowsAccent, ToolRowsPlain)
	}
	switch t.Scrollbar {
	case "", ScrollbarSlim, ScrollbarClassic:
	default:
		return fmt.Errorf("theme %s: scrollbar %q: want %q or %q", t.Name, t.Scrollbar, ScrollbarSlim, ScrollbarClassic)
	}
	var errs []string
	for _, c := range []struct {
		name  string
		color Color
	}{
		{"base", t.Colors.Base}, {"raised", t.Colors.Raised}, {"toast", t.Colors.Toast},
		{"hairline", t.Colors.Hairline}, {"thumb", t.Colors.Thumb}, {"text", t.Colors.Text},
		{"muted", t.Colors.Muted}, {"accent", t.Colors.Accent}, {"accent_lo", t.Colors.AccentLo},
		{"think", t.Colors.Think}, {"focus", t.Colors.Focus},
	} {
		for _, v := range []string{c.color.Dark, c.color.Light} {
			if _, err := ParseHex(v); err != nil {
				errs = append(errs, fmt.Sprintf("colors.%s: %v", c.name, err))
			}
		}
	}
	if t.Terminal != nil {
		for _, c := range []struct {
			name  string
			value string
		}{
			{"bg", t.Terminal.Bg}, {"fg", t.Terminal.Fg}, {"cursor", t.Terminal.Cursor},
		} {
			if _, err := ParseHex(c.value); err != nil {
				errs = append(errs, fmt.Sprintf("terminal.%s: %v", c.name, err))
			}
		}
		if len(t.Terminal.Ansi16) > 16 {
			errs = append(errs, fmt.Sprintf("terminal.ansi16: %d entries, want at most 16", len(t.Terminal.Ansi16)))
		}
		for i, v := range t.Terminal.Ansi16 {
			if _, err := ParseHex(v); err != nil {
				errs = append(errs, fmt.Sprintf("terminal.ansi16[%d]: %v", i, err))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("theme %s: %s", t.Name, strings.Join(errs, "; "))
	}
	return nil
}

// resolve flattens one chain — root ancestor first, the theme itself last —
// into a Resolved: later entries override earlier ones per non-empty field,
// then derivations complete the palette. lookup resolves extends names; it
// is the registry in production. maxDepth bounds chains defensively.
func resolve(chain []*Theme) *Resolved {
	out := &Resolved{}
	for _, t := range chain {
		if t.Look != "" {
			out.Look = t.Look
		}
		if t.ToolRows != "" {
			out.ToolRows = t.ToolRows
		}
		if t.Scrollbar != "" {
			out.Scrollbar = t.Scrollbar
		}
		if t.Tagline != "" {
			out.Tagline = t.Tagline
		}
		if t.Terminal != nil {
			// Clone: resolution must never mutate the registry's records
			// (deriveTerminal pads/fills the ramp in place).
			term := *t.Terminal
			term.Ansi16 = append([]string(nil), t.Terminal.Ansi16...)
			out.Terminal = &term
		}
		mergeColor := func(dst *Color, src Color) {
			if src.Absent() {
				return
			}
			if src.Dark != "" {
				dst.Dark = src.Dark
			}
			if src.Light != "" {
				dst.Light = src.Light
			}
		}
		mergeColor(&out.Colors.Base, t.Colors.Base)
		mergeColor(&out.Colors.Raised, t.Colors.Raised)
		mergeColor(&out.Colors.Toast, t.Colors.Toast)
		mergeColor(&out.Colors.Hairline, t.Colors.Hairline)
		mergeColor(&out.Colors.Thumb, t.Colors.Thumb)
		mergeColor(&out.Colors.Text, t.Colors.Text)
		mergeColor(&out.Colors.Muted, t.Colors.Muted)
		mergeColor(&out.Colors.Accent, t.Colors.Accent)
		mergeColor(&out.Colors.AccentLo, t.Colors.AccentLo)
		mergeColor(&out.Colors.Think, t.Colors.Think)
		mergeColor(&out.Colors.Focus, t.Colors.Focus)
	}
	last := chain[len(chain)-1]
	out.Name = last.Name
	out.DerivedFrom = last.DerivedFrom

	// Shape defaults: inherit-from-look. "off" declares classic explicitly;
	// a theme with no look anywhere in its chain gets the skin render path —
	// the token-driven one — which is what a hand-written palette wants.
	if out.Look == "" {
		out.Look = LookSkin
	}
	if out.ToolRows == "" {
		if out.Look == LookSkin {
			out.ToolRows = ToolRowsAccent
		} else {
			out.ToolRows = ToolRowsPlain
		}
	}
	if out.Scrollbar == "" {
		if out.Look == LookSkin {
			out.Scrollbar = ScrollbarSlim
		} else {
			out.Scrollbar = ScrollbarClassic
		}
	}

	deriveColors(&out.Colors)
	if out.Terminal != nil {
		deriveTerminal(out.Terminal, out.Colors)
	}
	return out
}

// deriveColors completes absent tokens from the declared basis — the "common
// basis" of the schema: a hand-written theme can be six colours and still
// resolve complete. Each rule fires only when its inputs are present, so the
// "off" theme (focus only, no base) resolves to exactly what it declares.
// Order matters: toast reads the derived raised.
func deriveColors(c *Palette) {
	if c.Think.Absent() && !c.Muted.Absent() {
		c.Think = c.Muted
	}
	if c.Raised.Absent() && !c.Base.Absent() {
		c.Raised = Lighten(c.Base, 0.08)
	}
	if c.Toast.Absent() && !c.Base.Absent() && !c.Raised.Absent() {
		c.Toast = Mix(c.Base, c.Raised, 0.5)
	}
	if c.Thumb.Absent() && !c.Muted.Absent() && !c.Hairline.Absent() {
		c.Thumb = Mix(c.Muted, c.Hairline, 0.5)
	}
	if c.AccentLo.Absent() && !c.Accent.Absent() && !c.Base.Absent() {
		c.AccentLo = Mix(c.Accent, c.Base, 0.7)
	}
	if c.Focus.Absent() && !c.Base.Absent() && !c.Accent.Absent() {
		c.Focus = Mix(c.Base, c.Accent, 0.15)
	}
}

// deriveTerminal pads the ANSI ramp to 16 slots and fills a missing ramp
// with DeriveANSI16, so a theme with a terminal block but no ansi16 still
// gets readable code spans and links on its own ground.
func deriveTerminal(t *Terminal, p Palette) {
	if len(t.Ansi16) == 0 {
		ramp := DeriveANSI16(p)
		t.Ansi16 = ramp[:]
		return
	}
	for len(t.Ansi16) < 16 {
		t.Ansi16 = append(t.Ansi16, "")
	}
}
