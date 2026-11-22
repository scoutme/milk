package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/theme"
)

// themeRegistry is the single lookup path for themes (plan step 2, PR 1):
// the builtins ("off", "supermilk") plus every drop-in theme file in
// ~/.milk/themes/*.json (state root) and <project>/.milk/themes/*.json.
// Themes are data (internal/theme) — a new theme is a JSON file, never new
// branches. The runtime render layer keeps its themeDef; conversion happens
// here so the old literals stay readable as parity goldens
// (theme_parity_test.go pins converted builtins against them).
var themeRegistry = theme.New()

// loadThemeRegistry rescans the theme dirs. Called at TUI startup and by
// /theme reload — dropping a file into either dir is enough to make it
// available without a restart. projectDir is the session's CWD ("" for no
// project scope).
func loadThemeRegistry(projectDir string) {
	userDir := ""
	if dir, err := config.Dir(); err == nil {
		userDir = filepath.Join(dir, "themes")
	}
	project := ""
	if projectDir != "" {
		project = filepath.Join(projectDir, config.LocalConfigDir, "themes")
	}
	themeRegistry.Load(userDir, project)
}

// themeDefFor resolves a theme name to the runtime themeDef.
func themeDefFor(name string) (*themeDef, bool) {
	rt, ok := themeRegistry.Get(name)
	if !ok {
		return nil, false
	}
	return themeDefFromResolved(rt), true
}

// mustThemeDef resolves at package init — the builtins always exist.
func mustThemeDef(name string) *themeDef {
	t, _ := themeDefFor(name)
	return t
}

// themeProjectDir is the session CWD for project-scope lookups ("" when
// there is no session — tests build bare models).
func (m model) themeProjectDir() string {
	if m.st == nil || m.st.sess == nil {
		return ""
	}
	return m.st.sess.CWD
}

// themeListText renders the registry as one line per theme — name (active
// one marked), source, look, tagline — followed by any load warnings, so a
// broken drop-in file is visible instead of silently skipped.
func themeListText() string {
	var b strings.Builder
	b.WriteString(milkTag() + " themes:\n")
	for _, name := range themeRegistry.Names() {
		rt, ok := themeRegistry.Get(name)
		if !ok {
			continue
		}
		src, _ := themeRegistry.Source(name)
		mark := " "
		if name == th.name {
			mark = "*"
		}
		tag := rt.Tagline
		if tag == "" {
			tag = defaultTagline
		}
		b.WriteString(fmt.Sprintf("  %s %-11s %-7s %-7s %s\n", mark, name, src, rt.Look, dim(tag)))
	}
	for _, warn := range themeRegistry.Warnings() {
		b.WriteString(milkTag() + " warning: " + warn + "\n")
	}
	return b.String()
}

// themeDefFromResolved converts a resolved theme into the render layer's
// themeDef. `skin` is the resolved `look: skin` — the shape shortcut the
// render call sites branch on; `tool_rows`/`scrollbar` are the schema's two
// per-theme cosmetic knobs and reach their own call sites.
func themeDefFromResolved(rt *theme.Resolved) *themeDef {
	toTC := func(c theme.Color) themeColor { return themeColor{dark: c.Dark, light: c.Light} }
	t := &themeDef{
		name:      rt.Name,
		skin:      rt.Look == theme.LookSkin,
		toolRows:  rt.ToolRows,
		scrollbar: rt.Scrollbar,
		tagline:   rt.Tagline,
		base:      toTC(rt.Colors.Base), raised: toTC(rt.Colors.Raised),
		toast: toTC(rt.Colors.Toast), hairline: toTC(rt.Colors.Hairline),
		thumb: toTC(rt.Colors.Thumb), text: toTC(rt.Colors.Text),
		muted: toTC(rt.Colors.Muted), accent: toTC(rt.Colors.Accent),
		accentLo: toTC(rt.Colors.AccentLo), think: toTC(rt.Colors.Think),
		focus: toTC(rt.Colors.Focus),
	}
	if rt.Terminal != nil {
		t.termBg, t.termFg, t.termCursor = rt.Terminal.Bg, rt.Terminal.Fg, rt.Terminal.Cursor
		for i := range t.termAnsi {
			if i < len(rt.Terminal.Ansi16) {
				t.termAnsi[i] = rt.Terminal.Ansi16[i]
			}
		}
	}
	return t
}
