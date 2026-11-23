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
// minimal classic theme declares (off.json), a fully declared classic look
// with its own palette, chrome block and tagline (space.json), and what a
// fully explicit skin carries (supermilk.json) are all real, loadable files
// in this repo — and /theme view renders any theme's JSON uniformly (plan
// PR 2). Parity with the pre-registry themeDef literals is pinned by
// cmd/milk's theme_parity_test.go for the two themes that predate the
// registry (off, supermilk); space arrived as data and needs no golden.
//
//go:embed themes/*.json
var themesFS embed.FS

// builtinRaw holds each builtin descriptor's original text (/theme view),
// filled by mustLoadBuiltins before builtins initializes.
var builtinRaw = map[string]string{}

// builtins is the registry's seed, in embedded file-name order
// (off.json < space.json < supermilk.json — alphabetical today, which lands
// the shipped "space" look between the other two; revisit if a future
// builtin needs a different listing position).
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
		builtinRaw[t.Name] = string(data)
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

// Builtins by name, for tests and callers that mean exactly one of them
// (resolution always goes through the Registry).
var (
	builtinOff       = builtin("off")
	builtinSuperMilk = builtin("supermilk")
)
