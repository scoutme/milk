package theme

import (
	"embed"
	"fmt"
	"strings"
)

// The builtin themes are JSON descriptors embedded from themes/*.json — the
// exact format ~/.milk/themes/*.json uses, parsed through the exact same code
// path (parseTheme), so builtin-vs-drop-in differs only in *discovery*
// (embedded dir vs filesystem dirs), never in representation. The descriptors
// double as the working examples to copy when writing your own theme: what a
// minimal classic theme declares (off.json) and what a fully explicit skin
// carries (supermilk.json) are both real, loadable files in this repo — and
// /theme view renders any theme's JSON uniformly (plan PR 2). Parity with
// the pre-registry themeDef literals is pinned by cmd/milk's
// theme_parity_test.go; "off" stays special only in what it declares: shape
// classic, no terminal block, just the focus tint as a token — its hardcoded
// chrome colours are the design (ADR-0077), so its other tokens stay absent
// and the render layer keeps its own fallbacks.
//
//go:embed themes/*.json
var themesFS embed.FS

// builtins is the registry's seed, in embedded file-name order
// (off.json < supermilk.json — alphabetical today; revisit if a third
// builtin ever needs a different listing position).
var builtins = mustLoadBuiltins()

func mustLoadBuiltins() []*Theme {
	entries, err := themesFS.ReadDir("themes")
	if err != nil {
		panic("theme: embedded themes: " + err.Error())
	}
	var out []*Theme
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := themesFS.ReadFile("themes/" + e.Name())
		if err != nil {
			panic("theme: embedded theme " + e.Name() + ": " + err.Error())
		}
		t, err := parseTheme(data, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			panic(fmt.Sprintf("theme: embedded builtin %s: %v", e.Name(), err))
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		panic("theme: no embedded builtin themes")
	}
	return out
}

// builtin returns the builtin theme of that name — the canonical data the
// package tests resolve against.
func builtin(name string) *Theme {
	for _, t := range builtins {
		if t.Name == name {
			return t
		}
	}
	panic("theme: no builtin " + name)
}

// The two builtins by name, for tests and callers that mean exactly one of
// them (resolution always goes through the Registry).
var (
	builtinOff       = builtin("off")
	builtinSuperMilk = builtin("supermilk")
)
