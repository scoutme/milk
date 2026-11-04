// Package showcase defines and drives showcase runs (issue #387): one
// manifest — an ordered list of feature beats, each with the keys to send
// and the pane state to wait for — consumed by two runners:
//
//   - the recorder (cmd/showcase): calibrates every beat's real duration
//     against a live TUI in tmux, emits a vhs tape with *measured* sleeps,
//     and lets vhs render the GIF. State-synchronized, not sleep-driven:
//     which is why the fixed 8s/24s sleeps in the old hand-written tape
//     rotted and these don't.
//   - a headless verifier (planned): same beats, same waits, asserted
//     against a batch run — CI-runnable. The definition converges, the
//     runners stay separate (ADR-0099).
//
// A beat's waits are the contract: everything milk displays while the beat
// runs must appear in them, and nothing else in the system guesses timing.
package showcase

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Manifest is a showcase definition (demo/showcase.yaml).
type Manifest struct {
	// Name labels the run (reporting, comments in the tape).
	Name string `yaml:"name"`
	// Output is the recorded GIF path, relative to the working directory
	// the recorder runs from (the repo root, like the old tape's path).
	Output string `yaml:"output"`
	// Tape is the generated vhs tape path, relative to the manifest's own
	// directory ("demo.tape" beside demo/showcase.yaml).
	Tape string `yaml:"tape"`
	// Launch is the TUI's command-line *arguments* — the recorder owns
	// the shell line around them (cd, MILK_CONFIG_ROOT, the absolute
	// binary path), so the manifest stays machine-independent: "--new".
	Launch string `yaml:"launch"`
	// Workdir is the directory the run happens in, relative to the working
	// directory (".": the repo — the git panel beat needs a real repo).
	Workdir string `yaml:"workdir"`
	// Seed is a list of shell commands run before *every* pass — verify,
	// calibrate, and the recorded replay — with cwd = the run workdir and
	// env WORKDIR / STATE_ROOT (the pass's config root). Every pass must
	// start from the same state: calibration mutates the workdir (the
	// model under test edits files), so seeds reset it, and per-root state
	// (panel-state, …) must exist in each fresh config root. Commands may
	// create the workdir themselves (it is mkdir'ed first). Empty = today's
	// behavior (no seeds).
	Seed []string `yaml:"seed,omitempty"`
	// AllowTools grants the seeded agents dangerously_skip_permissions so
	// tool-mutating beats (edit_file, bash) run unattended — batch runs have
	// no one to answer a permission ask, and a blocked ask hangs the turn
	// (observed: the first fix-typo verify died waiting on edit_file's
	// [Y/n/a/d]). Only the *seeded* config copy is patched — the live config
	// is never modified. Same semantics as eval's sandbox allow_tools.
	AllowTools bool `yaml:"allow_tools,omitempty"`
	// Margin inflates each measured duration for the recording pass
	// (typing speed, second-run latency variance). sleep = elapsed*margin
	// + base. 0 = default 1.5; margin < 1 is rejected (a recording that
	// sleeps less than reality drops frames on the next backend hiccup).
	Margin float64 `yaml:"margin"`
	// Base is added to every sleep (seconds, default 0.4) so even a
	// zero-wait beat gets a visible beat boundary in the recording.
	Base float64 `yaml:"base"`
	// Timeout bounds every single wait (default 60s). A wait that can't be
	// satisfied is a broken beat — the recorder fails loudly with the last
	// pane capture instead of recording garbage.
	Timeout time.Duration `yaml:"timeout"`
	// StripConfigKeys are config.json top-level keys removed for the clean
	// run root (e.g. remote_oversight: recording must not depend on, or
	// announce, live infrastructure). Agents and settings are kept — the
	// showcase runs on the user's own configured agents.
	StripConfigKeys []string `yaml:"strip_config_keys"`
	// Terminal is the recorded terminal's appearance (vhs header) and the
	// tmux cell size the calibration runs at — both declared, because they
	// are deliberately not derived from each other.
	Terminal Terminal `yaml:"terminal"`
	// Beats is the ordered narrative.
	Beats []Beat `yaml:"beats"`
}

// Terminal describes the recorded/calibrated terminal.
type Terminal struct {
	Width         int     `yaml:"width"` // vhs px
	Height        int     `yaml:"height"`
	Cols          int     `yaml:"cols"` // tmux cells (calibration)
	Rows          int     `yaml:"rows"`
	FontSize      int     `yaml:"font_size"`
	Theme         string  `yaml:"theme"`
	TypingSpeed   string  `yaml:"typing_speed"` // vhs duration, e.g. "60ms"
	PlaybackSpeed float64 `yaml:"playback_speed"`
}

// Beat is one narrative unit: send the steps, wait for what they must show.
type Beat struct {
	// ID names the beat (tape comment, calibration errors). Unique.
	ID string `yaml:"id"`
	// Note is the human sentence in the tape comment — the showcase's
	// narration, visible to anyone reading the generated tape.
	Note string `yaml:"note"`
	// SkipIf skips the whole beat when the pane already shows it (the
	// beat proves nothing new — used when a panel/line can come from an
	// earlier event, so the manifest stays honest on any start state).
	SkipIf string `yaml:"skip_if,omitempty"`
	// Steps run in order.
	Steps []Step `yaml:"steps"`
}

// Step is one action plus the state it must produce. Exactly one action per
// step; waits/extract ride the action they follow.
type Step struct {
	// Type is literal text typed into the input (no submit — pair with a
	// key: Enter step; keeping them separate lets a beat open a picker
	// between text and submit, exactly like a human).
	Type string `yaml:"type,omitempty"`
	// Key is a named tmux key ("Enter", "C-j", "F6", ...).
	Key string `yaml:"key,omitempty"`
	// Clear backspaces the input before continuing (recovery step; the
	// generated tape expands it to Repeat N { Backspace }).
	Clear bool `yaml:"clear,omitempty"`
	// Wait blocks until the pane contains the substring.
	Wait string `yaml:"wait,omitempty"`
	// WaitAbsent blocks until the pane no longer contains it (turn ends:
	// wait for busy first, then for the role marker to leave).
	WaitAbsent string `yaml:"wait_absent,omitempty"`
	// Extract captures a regex group from the pane into a variable usable
	// by later {{name}} placeholders (a generated task id, etc.).
	Extract *Extract `yaml:"extract,omitempty"`
}

// Extract stores one regex capture for template interpolation.
type Extract struct {
	Pattern string `yaml:"pattern"`
	Name    string `yaml:"name"`
}

// Defaults applied by Load.
const (
	DefaultMargin  = 1.5
	DefaultBase    = 0.4
	DefaultTimeout = 60 * time.Second
	PollInterval   = 250 * time.Millisecond
	// Startup is two wrap-safe fragments, never a full phrase: tmux wraps
	// at spaces, so "ctrl+o palette" splits across lines when the version
	// string grows ("-dirty") or the terminal narrows — observed live.
	// Each fragment is a single unbreakable token at a different end of
	// the screen: header painted (top) + status line rendered (bottom).
	StartupWaitText = "ctrl+o" // header's palette hint, first paint
	StatusWaitText  = "role:"  // status bar: input area live
)

// Load reads and validates a manifest.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading showcase manifest: %w", err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing showcase manifest %s: %w", path, err)
	}
	if m.Margin == 0 {
		m.Margin = DefaultMargin
	}
	if m.Base == 0 {
		m.Base = DefaultBase
	}
	if m.Timeout == 0 {
		m.Timeout = DefaultTimeout
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("showcase manifest %s: %w", path, err)
	}
	return &m, nil
}

// Validate rejects a manifest that cannot produce a truthful recording.
func (m *Manifest) Validate() error {
	if len(m.Beats) == 0 {
		return fmt.Errorf("no beats")
	}
	if m.Launch == "" {
		return fmt.Errorf("launch is empty")
	}
	if m.Output == "" {
		return fmt.Errorf("output is empty")
	}
	if m.Margin < 1 {
		return fmt.Errorf("margin %v must be >= 1 (a recording must sleep at least as long as reality)", m.Margin)
	}
	if m.Terminal.Cols <= 0 || m.Terminal.Rows <= 0 {
		return fmt.Errorf("terminal.cols/rows must be positive (tmux calibration size)")
	}
	seen := map[string]bool{}
	for i := range m.Beats {
		b := &m.Beats[i]
		if b.ID == "" {
			return fmt.Errorf("beats[%d]: missing id", i)
		}
		if seen[b.ID] {
			return fmt.Errorf("duplicate beat id %q", b.ID)
		}
		seen[b.ID] = true
		if len(b.Steps) == 0 {
			return fmt.Errorf("beat %q: no steps", b.ID)
		}
		for j := range b.Steps {
			if err := b.Steps[j].validate(b.ID, j); err != nil {
				return err
			}
		}
		// Typed text that no later key ever submits is a wait that can
		// never be satisfied — caught here, not sixty seconds into a live
		// calibration (the exact bug this rule was written for: the first
		// version of demo/showcase.yaml put learn's wait on its type step
		// with no Enter anywhere, and every record attempt timed out on a
		// prompt nobody submitted).
		sawType := -1
		sawKey := false
		for j := range b.Steps {
			if b.Steps[j].Type != "" {
				sawType = j
				sawKey = false
			}
			if b.Steps[j].Key != "" && sawType >= 0 {
				sawKey = true
			}
		}
		if sawType >= 0 && !sawKey {
			return fmt.Errorf("beat %q: step %d types text but no later step sends a key — it is never submitted", b.ID, sawType)
		}
	}
	return nil
}

// validate enforces one action per step — a typo'd step (both `type` and
// `key`) would silently reorder the narration.
func (s *Step) validate(beat string, idx int) error {
	actions := 0
	for _, on := range []bool{s.Type != "", s.Key != "", s.Clear, s.Wait != "", s.WaitAbsent != ""} {
		if on {
			actions++
		}
	}
	if s.Extract != nil {
		actions++
	}
	if actions == 0 {
		return fmt.Errorf("beat %q step %d: empty step", beat, idx)
	}
	hasAction := s.Type != "" || s.Key != "" || s.Clear
	hasWait := s.Wait != "" || s.WaitAbsent != "" || s.Extract != nil
	if !hasAction && !hasWait {
		return fmt.Errorf("beat %q step %d: needs an action or a wait", beat, idx)
	}
	if hasAction && (s.Wait != "" || s.WaitAbsent != "" || s.Extract != nil) && s.Type != "" {
		// type + wait in one step is allowed and common (type the command,
		// then wait for what submitting it shows — requires Enter inside
		// the same beat anyway, so pair waits with key steps instead when
		// a submit is involved). Deliberately permitted; no check.
	}
	if s.Extract != nil && (s.Extract.Pattern == "" || s.Extract.Name == "") {
		return fmt.Errorf("beat %q step %d: extract needs pattern and name", beat, idx)
	}
	return nil
}

// FilterBeats narrows the manifest to the named beat ids (comma-separated,
// manifest order kept — a subset still tells its story in narrative order).
// An unknown name is an error: a typo'd `-beats` subset must fail loudly
// rather than silently run nothing — or worse, silently run everything.
func (m *Manifest) FilterBeats(csv string) error {
	want := map[string]bool{}
	for _, id := range strings.Split(csv, ",") {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = true
		}
	}
	if len(want) == 0 {
		return nil
	}
	kept := make([]Beat, 0, len(want))
	for i := range m.Beats {
		if want[m.Beats[i].ID] {
			kept = append(kept, m.Beats[i])
			delete(want, m.Beats[i].ID)
		}
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for id := range want {
			missing = append(missing, id)
		}
		slices.Sort(missing) // map order is random; a config error reads the same everywhere
		return fmt.Errorf("unknown beat id(s) in -beats: %s (manifest has: %s)",
			strings.Join(missing, ", "), m.beatIDs())
	}
	m.Beats = kept
	return nil
}

// beatIDs lists every beat id in order (error messages, reports).
func (m *Manifest) beatIDs() string {
	ids := make([]string, len(m.Beats))
	for i := range m.Beats {
		ids[i] = m.Beats[i].ID
	}
	return strings.Join(ids, ", ")
}

// Interpolate replaces {{name}} placeholders from vars. Unknown names are
// left intact (visible in the failure, not silently emptied).
func Interpolate(text string, vars map[string]string) string {
	for k, v := range vars {
		text = strings.ReplaceAll(text, "{{"+k+"}}", v)
	}
	return text
}
