package theme

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Source records where a registry entry came from — `/theme list` shows it,
// and shadowing (project beats user beats builtin) is visible rather than
// magical.
type Source string

const (
	SourceBuiltin Source = "builtin"
	SourceUser    Source = "user"    // ~/.milk/themes/*.json (state root)
	SourceProject Source = "project" // <cwd>/.milk/themes/*.json
)

// Registry is the theme catalogue: builtin themes plus every *.json theme
// file in the user and project theme dirs. Lookup is by name (case-
// insensitive), resolution flattens the extends chain, and invalid files are
// skipped with a warning — a broken theme can never crash the TUI.
type Registry struct {
	mu       sync.RWMutex
	themes   map[string]*Theme
	sources  map[string]Source
	order    []string // first-seen order: builtins, user A–Z, project A–Z
	resolved map[string]*Resolved
	warnings []string
}

// New returns a registry seeded with the builtins.
func New() *Registry {
	r := &Registry{
		themes:   map[string]*Theme{},
		sources:  map[string]Source{},
		resolved: map[string]*Resolved{},
	}
	for _, t := range builtins {
		r.add(t, SourceBuiltin)
	}
	return r
}

// Load rescans the user and project theme dirs, replacing every previously
// loaded file theme (the builtins stay). A theme file's `name` wins over its
// basename; a later source shadows an earlier one on a name collision. Every
// skipped file leaves a warning retrievable via Warnings.
func (r *Registry) Load(userDir, projectDir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.themes = map[string]*Theme{}
	r.sources = map[string]Source{}
	r.order = nil
	r.resolved = map[string]*Resolved{}
	r.warnings = nil
	for _, t := range builtins {
		r.add(t, SourceBuiltin)
	}
	for _, dir := range []struct {
		path string
		src  Source
	}{{userDir, SourceUser}, {projectDir, SourceProject}} {
		if dir.path == "" {
			continue
		}
		r.loadDir(dir.path, dir.src)
	}
}

func (r *Registry) loadDir(dir string, src Source) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // absent dir is the common case; not a warning
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		t, err := readThemeFile(path)
		if err != nil {
			r.warnings = append(r.warnings, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		r.add(t, src)
	}
}

// readThemeFile parses and validates one theme file. Unknown keys are an
// error (the schema is small and spelled out in docs/reference/spec.md — a
// typo'd key must not silently do nothing).
// parseTheme decodes one theme descriptor — the shared path for the embedded
// builtins and filesystem drop-ins alike: discovery differs, parsing and
// validation never do. baseName names the theme when the descriptor omits it.
func parseTheme(data []byte, baseName string) (*Theme, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var t Theme
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("invalid theme: %w", err)
	}
	if t.Name == "" {
		t.Name = strings.ToLower(baseName)
	}
	if err := Validate(&t); err != nil {
		return nil, err
	}
	return &t, nil
}

func readThemeFile(path string) (*Theme, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from a themedir listing
	if err != nil {
		return nil, err
	}
	return parseTheme(data, strings.TrimSuffix(filepath.Base(path), ".json"))
}

// add registers a theme under its lowercased name. Source order is load
// order: a later add shadows an earlier one but keeps the earlier listing
// position, so the carousel order is stable while a project shadow works.
func (r *Registry) add(t *Theme, src Source) {
	key := strings.ToLower(strings.TrimSpace(t.Name))
	if _, seen := r.themes[key]; !seen {
		r.order = append(r.order, key)
	}
	r.themes[key] = t
	r.sources[key] = src
	delete(r.resolved, key) // invalidate any cached resolution
}

// Get resolves a theme by name (case-insensitive). Reports false for an
// unknown name or a broken extends chain.
func (r *Registry) Get(name string) (*Resolved, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return nil, false
	}
	if _, ok := r.themes[key]; !ok {
		return nil, false // a plain lookup miss is not a load problem
	}
	if rt, ok := r.resolved[key]; ok {
		return rt, true
	}
	chain, err := r.chain(key, map[string]bool{})
	if err != nil {
		r.warnings = append(r.warnings, err.Error())
		return nil, false
	}
	rt := resolve(chain)
	r.resolved[key] = rt
	return rt, true
}

// chain walks `extends` from name up to its root ancestor and returns the
// chain root-first. Cycles and unknown parents are errors.
func (r *Registry) chain(name string, seen map[string]bool) ([]*Theme, error) {
	if seen[name] {
		return nil, fmt.Errorf("theme %s: extends cycle", name)
	}
	seen[name] = true
	t, ok := r.themes[name]
	if !ok {
		return nil, fmt.Errorf("theme %s: unknown theme", name)
	}
	if t.Extends == "" {
		return []*Theme{t}, nil
	}
	parent := strings.ToLower(strings.TrimSpace(t.Extends))
	up, err := r.chain(parent, seen)
	if err != nil {
		return nil, err
	}
	return append(up, t), nil
}

// Names lists every registry name in listing order (builtins, then user
// themes A–Z, then project themes A–Z; a shadow keeps its position).
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	sort.SliceStable(out, func(i, j int) bool {
		rank := func(n string) int {
			switch r.sources[n] {
			case SourceBuiltin:
				return 0
			case SourceUser:
				return 1
			default:
				return 2
			}
		}
		if rank(out[i]) != rank(out[j]) {
			return rank(out[i]) < rank(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// Source reports where a name's definition comes from.
func (r *Registry) Source(name string) (Source, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src, ok := r.sources[strings.ToLower(strings.TrimSpace(name))]
	return src, ok
}

// Warnings reports the load problems of the last Load — invalid files, broken
// chains seen at lookup. `/theme` surfaces them once; startup stays silent.
func (r *Registry) Warnings() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.warnings))
	copy(out, r.warnings)
	return out
}
