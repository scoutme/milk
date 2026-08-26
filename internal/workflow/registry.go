package workflow

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/scoutme/milk/internal/config"
)

//go:embed definitions/*.yaml
var builtinDefinitionsFS embed.FS

// Registry holds every loaded workflow Definition, keyed by name. Built-ins
// are loaded first; a user-supplied file in ~/.milk/workflows/ with the same
// name overrides it, mirroring how ~/.milk/config.json layers over defaults.
type Registry struct {
	byName map[string]Definition
}

// LoadRegistry loads the built-in definitions (embedded in the binary) and
// layers any ~/.milk/workflows/*.yaml or *.json files on top. A user file
// whose Name matches a built-in replaces it; a new Name registers a new
// workflow. Every definition is validated before being added — a malformed
// user override is reported and skipped rather than failing the whole load,
// so one bad file doesn't take down every other workflow.
func LoadRegistry() (*Registry, []error) {
	reg := &Registry{byName: make(map[string]Definition)}
	var errs []error

	entries, err := builtinDefinitionsFS.ReadDir("definitions")
	if err != nil {
		return reg, []error{fmt.Errorf("workflow registry: reading embedded definitions: %w", err)}
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := builtinDefinitionsFS.ReadFile(filepath.Join("definitions", e.Name()))
		if err != nil {
			errs = append(errs, fmt.Errorf("workflow registry: reading built-in %s: %w", e.Name(), err))
			continue
		}
		def, err := decodeDefinition(e.Name(), data)
		if err != nil {
			errs = append(errs, fmt.Errorf("workflow registry: built-in %s: %w", e.Name(), err))
			continue
		}
		reg.byName[strings.ToLower(strings.TrimSpace(def.Name))] = def
	}

	dir, err := userDefinitionsDir()
	if err != nil {
		errs = append(errs, fmt.Errorf("workflow registry: resolving user workflows dir: %w", err))
		return reg, errs
	}
	userEntries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return reg, errs
		}
		errs = append(errs, fmt.Errorf("workflow registry: reading %s: %w", dir, err))
		return reg, errs
	}
	for _, e := range userEntries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("workflow registry: reading %s: %w", path, err))
			continue
		}
		def, err := decodeDefinition(name, data)
		if err != nil {
			errs = append(errs, fmt.Errorf("workflow registry: %s: %w", path, err))
			continue
		}
		reg.byName[strings.ToLower(strings.TrimSpace(def.Name))] = def
	}

	return reg, errs
}

// decodeDefinition unmarshals data as YAML or JSON (by extension) and
// validates the result.
func decodeDefinition(filename string, data []byte) (Definition, error) {
	var def Definition
	var err error
	if strings.HasSuffix(filename, ".json") {
		err = json.Unmarshal(data, &def)
	} else {
		err = yaml.Unmarshal(data, &def)
	}
	if err != nil {
		return Definition{}, fmt.Errorf("parsing: %w", err)
	}
	if err := def.Validate(); err != nil {
		return Definition{}, err
	}
	return def, nil
}

// userDefinitionsDir returns ~/.milk/workflows (not created here — LoadRegistry
// treats it as optional, absent is not an error).
func userDefinitionsDir() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "workflows"), nil
}

// Lookup returns the named Definition, or (Definition{}, false) if no
// built-in or user-defined workflow has that name.
func (r *Registry) Lookup(name string) (Definition, bool) {
	def, ok := r.byName[strings.ToLower(strings.TrimSpace(name))]
	return def, ok
}

// Names returns every registered workflow name, sorted.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
