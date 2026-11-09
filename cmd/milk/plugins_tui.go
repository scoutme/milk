package main

// The TUI /plugins suite face (ADR-0104 Part II): transcript reporting, the
// hot reload after mutations, and the bare-/plugins marketplace browse
// flyover — all dispatching into the same pluginsActionRun layer the CLI
// uses.

import (
	tea "charm.land/bubbletea/v2"
	"fmt"
	"github.com/scoutme/milk/internal/plugin"
	"os"
	"path/filepath"
	"strings"
)

type pluginsActionMsg struct {
	text    string
	mutated bool
	err     error
}

// handlePluginsCmd dispatches the TUI /plugins suite. Bare /plugins keeps
// the synchronous loader view and — when registered catalogs hold entries —
// opens the marketplace browse flyover (navigate + install, no timed
// default: never auto-install). A subcommand runs the shared action layer
// off the update loop and reports through pluginsActionMsg, which hot-
// reloads the plugin set after a mutation — no restart.

func (m model) handlePluginsCmd(args string) (tea.Model, tea.Cmd) {
	args = strings.TrimSpace(args)
	if args == "" {
		m.appendTranscript(renderPluginsView())
		choices := browsePluginChoices()
		if len(choices) == 0 {
			m.appendTranscript(milkTag() + " plugins: no marketplace entries to browse — /plugins marketplace add <owner/repo>\n")
			return m, nil
		}
		m.pendingPluginBrowse = &pluginBrowseState{choices: choices}
		m.appendTranscript(milkTag() + fmt.Sprintf(" plugins: %d available to install — ↑↓ choose · enter install · esc cancel\n", len(choices)))
		m.refreshPrompt()
		m.syncLayout()
		return m, nil
	}
	if m.pluginsBusy {
		m.appendTranscript(milkTag() + " plugins: another operation is in flight\n")
		return m, nil
	}
	tokens, opts, err := parsePluginsTUIArgs(args)
	if err != nil {
		m.appendTranscript(milkPluginsErr(err))
		return m, nil
	}
	if pluginsActionNetwork(tokens) {
		m.appendTranscript(milkTag() + " plugins: working…\n")
	}
	m.pluginsBusy = true
	return m, func() tea.Msg {
		text, mutated, err := pluginsActionRun(tokens, opts)
		return pluginsActionMsg{text: text, mutated: mutated, err: err}
	}
}

// pluginBrowseChoice is one row of the bare /plugins flyover: a catalog
// entry plus the marketplace it lives in, reduced to the suite tokens that
// install it. mktDir — the registered snapshot directory — is the install
// key because it is unique by construction (catalog names may collide).

type pluginBrowseChoice struct {
	label  string // row text, unique across every choice
	mktDir string
	name   string
}

type pluginBrowseState struct {
	choices []pluginBrowseChoice
	sel     int
}

// browsePluginChoices reads every registered catalog for its entries. A
// catalog entry name that appears in several marketplaces is disambiguated
// to name@dir so rows (and their exact-typed answers) stay unique; entries
// from unreadable catalogs are skipped (registered ≠ loadable).

func browsePluginChoices() []pluginBrowseChoice {
	mkt := paths().marketplaces
	dirs, err := os.ReadDir(mkt)
	if err != nil {
		return nil
	}
	type raw struct{ name, dir string }
	var raws []raw
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		mp, _, err := plugin.LoadMarketplaceDir(filepath.Join(mkt, d.Name()))
		if err != nil {
			continue
		}
		for _, ep := range mp.Plugins {
			raws = append(raws, raw{name: ep.Name, dir: d.Name()})
		}
	}
	count := map[string]int{}
	for _, r := range raws {
		count[r.name]++
	}
	out := make([]pluginBrowseChoice, 0, len(raws))
	for _, r := range raws {
		label := r.name
		if count[r.name] > 1 {
			label = r.name + "@" + r.dir
		}
		out = append(out, pluginBrowseChoice{label: label, mktDir: r.dir, name: r.name})
	}
	return out
}

// browseEntries is the flyover's palette rows — hint shows the marketplace.

func browseEntries(choices []pluginBrowseChoice) []paletteEntry {
	entries := make([]paletteEntry, 0, len(choices))
	for _, c := range choices {
		entries = append(entries, paletteEntry{name: c.label, hint: c.mktDir})
	}
	return entries
}

// browseAnswer resolves Enter: an exact typed label answers it, a narrowing
// filter answers the highlighted row, text matching nothing stays raw for
// the handler to reject. No blank-equivalent exists here — every row
// installs (unlike agentPickerAnswer, whose "(default)" hint would blank
// the first hinted row).

func browseAnswer(entries []paletteEntry, typed string, sel int) string {
	typed = strings.TrimSpace(typed)
	if typed != "" {
		for _, e := range entries {
			if strings.EqualFold(e.name, typed) {
				return e.name
			}
		}
	}
	if a := agentPickerHighlight(entries, typed, sel); a != "" {
		return a
	}
	return typed
}

// handlePluginBrowseKey mirrors handleBgAgentWizardKey's contract minus the
// timed default: ↑↓/Tab (filter, palette semantics), Enter installs through
// the same suite dispatcher the hand-typed command would hit (busy latch,
// working-notice, report, hot reload — all reused), Esc cancels. Unknown
// typed text keeps the flyover open with a hint at the direct-ref path.

func (m model) handlePluginBrowseKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	w := m.pendingPluginBrowse
	if w == nil {
		return m, nil
	}
	switch msg.String() {
	case "ctrl+c", "esc":
		m.pendingPluginBrowse = nil
		m.ta.Reset()
		m.appendTranscript("\n" + milkTag() + " plugins: browse cancelled\n")
		m.refreshPrompt()
		m.syncLayout()
		return m, nil
	case "up", "ctrl+p", "shift+tab", "down", "ctrl+n", "tab":
		filtered := pickerFilter(browseEntries(w.choices), m.ta.Value())
		delta := 1
		if msg.String() == "up" || msg.String() == "ctrl+p" || msg.String() == "shift+tab" {
			delta = -1
		}
		w.sel = movePickerSel(w.sel, delta, len(filtered))
		return m, nil
	case "enter", "ctrl+m":
		entries := browseEntries(w.choices)
		typed := strings.TrimSpace(m.ta.Value())
		answer := browseAnswer(entries, typed, w.sel)
		m.ta.Reset()
		idx := -1
		for i, c := range w.choices {
			if strings.EqualFold(c.label, answer) {
				idx = i
				break
			}
		}
		if idx < 0 {
			m.appendTranscript(milkTag() + fmt.Sprintf(" unknown plugin %q — /plugins add <ref> installs by direct reference\n", answer))
			return m, nil
		}
		tokens := w.choices[idx].tokens()
		m.pendingPluginBrowse = nil
		m.refreshPrompt()
		m.syncLayout()
		m.appendTranscript(promptLabel(m.st) + "/plugins " + strings.Join(tokens, " ") + "\n")
		return m.handlePluginsCmd(strings.Join(tokens, " "))
	}
	cmd := m.updateTA(msg)
	if msg.Text != "" || msg.String() == "backspace" {
		w.sel = 0
	}
	m.syncLayout()
	return m, cmd
}

// handlePluginsActionMsg prints the action's report and, for a mutation,
// re-runs plugin discovery live (skill catalog + MCP seam + registry swap).
// Reload happens even when a partial update errored — the names that DID
// update must take effect.

func (m model) handlePluginsActionMsg(msg pluginsActionMsg) (tea.Model, tea.Cmd) {
	m.pluginsBusy = false
	if msg.text != "" {
		m.appendTranscript("\n" + milkTag() + " " + msg.text + "\n")
	}
	if msg.err != nil {
		m.appendTranscript(milkPluginsErr(msg.err))
	}
	if msg.mutated {
		initPlugins(m.st.cfg)
		m.appendTranscript(fmt.Sprintf("%s plugins: reloaded — %d loaded\n", milkTag(), pluginsLoadedCount()))
	}
	return m, nil
}

// pluginsLoadedCount reads the live registry for the reload note.

func pluginsLoadedCount() int {
	pluginsMu.RLock()
	defer pluginsMu.RUnlock()
	return len(plugins)
}

// milkPluginsErr renders an action error with the [milk] tag; usage errors
// are printed verbatim (they already carry their own "usage:" lead).

func milkPluginsErr(err error) string {
	s := err.Error()
	if strings.HasPrefix(s, "usage:") {
		return milkTag() + " " + s + "\n"
	}
	return milkTag() + " plugins: " + s + "\n"
}

// parsePluginsTUIArgs splits a TUI /plugins argument line into runner
// tokens plus the pin flags (the CLI carries those as cobra flags).

func parsePluginsTUIArgs(args string) ([]string, pluginsActionOpts, error) {
	var opts pluginsActionOpts
	var tokens []string
	fields := strings.Fields(args)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch f {
		case "--ref", "--sha":
			if i+1 >= len(fields) {
				return nil, opts, fmt.Errorf("%s needs a value", f)
			}
			i++
			if f == "--ref" {
				opts.ref = fields[i]
			} else {
				opts.sha = fields[i]
			}
		default:
			if strings.HasPrefix(f, "--") {
				return nil, opts, fmt.Errorf("unknown flag %s (supported: --ref, --sha)", f)
			}
			tokens = append(tokens, f)
		}
	}
	if len(tokens) == 0 {
		return nil, opts, pluginsUsage("plugins <add|marketplace|install|list|details|update|remove|shim>")
	}
	return tokens, opts, nil
}

// ---------------------------------------------------------------------------
// cobra face: milk plugins …
// ---------------------------------------------------------------------------

// newPluginsCmd is the plugin distribution CLI (ADR-0104 Part II):
// install-by-slug over the ecosystem's marketplace interchange, no clone.
// The loader (what a plugin directory means) is internal/plugin; this is
// only acquisition and bookkeeping. Every RunE is a thin print wrapper
// around pluginsActionRun — the TUI runs the same function.
