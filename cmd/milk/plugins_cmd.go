package main

// Plugin distribution with two faces over ONE action layer:
//
//   - `milk plugins …` — the cobra CLI: prints the report, returns errors;
//   - `/plugins …`     — the TUI suite: transcript output, and a mutation
//     hot-reloads the plugin set (initPlugins) so skills/MCP assignment take
//     effect without a restart.
//
// Both dispatch into pluginsActionRun, so the surfaces can never drift —
// the "one source for what is executed" pattern acp_commands.go uses for
// slash commands. Distribution spec: ADR-0104 Part II.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/plugin"
	"github.com/spf13/cobra"
)

// pluginsActionOpts carries the pin flags shared by every fetching action
// (--ref/--sha on the CLI, --ref/--sha tokens in the TUI).
type pluginsActionOpts struct {
	ref, sha string
}

// pluginsUsage formats a host-neutral usage error ("usage: plugins …") —
// both hosts render it verbatim.
func pluginsUsage(format string, args ...any) error {
	return fmt.Errorf("usage: "+format, args...)
}

// pluginsActionRun executes one distribution subcommand and returns its
// report text (no trailing newline), whether the installed set changed
// (the TUI reloads after that), and the first error. Text may be non-empty
// alongside an error (a multi-name update that partially succeeded).
func pluginsActionRun(tokens []string, opts pluginsActionOpts) (string, bool, error) {
	if len(tokens) == 0 {
		return "", false, pluginsUsage("plugins <add|marketplace|install|list|details|update|remove|shim>")
	}
	switch tokens[0] {
	case "add":
		if len(tokens) != 2 {
			return "", false, pluginsUsage("plugins add <owner/repo | https://… | npm:pkg> [--ref <branch/tag> --sha <commit>]")
		}
		ctxPaths := paths()
		if src, aerr := plugin.ParseAddRef(tokens[1]); aerr == nil {
			src.Ref, src.SHA = opts.ref, opts.sha
			p, warns, err := plugin.InstallPlugin(context.Background(), http.DefaultClient, src, tokens[1],
				ctxPaths.plugins, ctxPaths.data, ctxPaths.installs())
			if err != nil {
				return "", false, err
			}
			return reportInstalled("installed", p, warns), true, nil
		} else {
			// A bare plugin name resolves through registered catalogs — the
			// `install <name>@<marketplace>` flow without the suffix, which is
			// where users arrive from other tools. No catalog match → the
			// owner/URL/npm error stands as-is.
			ep, snap, origin, lerr := registeredEntry(ctxPaths.marketplaces, tokens[1])
			switch {
			case errors.Is(lerr, errNotRegistered):
				return "", false, aerr
			case lerr != nil:
				return "", false, lerr
			}
			p, warns, err := installFromEntry(ep, snap, origin, ctxPaths)
			if err != nil {
				return "", false, err
			}
			return reportInstalled("installed", p, warns) + "\n  from marketplace " + snap, true, nil
		}

	case "marketplace":
		return runMarketplaceAction(tokens, opts)

	case "install":
		if len(tokens) != 2 || !strings.Contains(tokens[1], "@") {
			return "", false, pluginsUsage("plugins install <name>@<marketplace>")
		}
		name, mktName, _ := strings.Cut(tokens[1], "@")
		ctxPaths := paths()
		dir, err := resolveMarketplace(ctxPaths.marketplaces, mktName)
		if err != nil {
			return "", false, err
		}
		mp, warns, err := plugin.LoadMarketplaceDir(dir)
		if err != nil {
			return "", false, err
		}
		ep, ok := mp.Entry(name)
		if !ok {
			return "", false, fmt.Errorf("marketplace %s has no plugin %q (has: %s)", mktName, name, entryNames(mp))
		}
		p, installWarns, err := installFromEntry(ep, dir, mktName+":"+name, ctxPaths)
		if err != nil {
			return "", false, err
		}
		warns = append(warns, installWarns...)
		return reportInstalled("installed", p, warns), true, nil

	case "list":
		ctxPaths := paths()
		in := ctxPaths.installsMust()
		var b strings.Builder
		names := make([]string, 0, len(in.Records))
		for name := range in.Records {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			rec := in.Records[name]
			fmt.Fprintf(&b, "%-20s %-8s %s%s\n", name, rec.Version, rec.Origin, map[bool]string{true: "  [shimmed]", false: ""}[rec.Shimmed])
		}
		// Hand-installed dirs (the drop-in flow) are shown too.
		if entries, err := os.ReadDir(ctxPaths.plugins); err == nil {
			for _, e := range entries {
				if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				if _, ok := in.Records[e.Name()]; !ok {
					fmt.Fprintf(&b, "%-20s %-8s (drop-in, no install record)\n", e.Name(), "")
				}
			}
		}
		if b.Len() == 0 {
			return "nothing installed (drop a plugin in " + ctxPaths.plugins + " or install one)", false, nil
		}
		return strings.TrimRight(b.String(), "\n"), false, nil

	case "details":
		if len(tokens) != 2 {
			return "", false, pluginsUsage("plugins details <name>")
		}
		ctxPaths := paths()
		in := ctxPaths.installsMust()
		var b strings.Builder
		if rec, ok := in.Records[tokens[1]]; ok {
			fmt.Fprintf(&b, "origin:       %s\nkind:         %s\ninstalled at: %s\nshimmed:      %v\n",
				rec.Origin, rec.Kind, rec.InstalledAt.Format("2006-01-02 15:04"), rec.Shimmed)
		} else {
			b.WriteString("no install record (drop-in)\n")
		}
		p, err := plugin.Load(filepath.Join(ctxPaths.plugins, tokens[1]), ctxPaths.data)
		if err != nil {
			return strings.TrimRight(b.String(), "\n"), false, err
		}
		servers, w := p.MilkServers()
		fmt.Fprintf(&b, "skills:       %d\nmcp servers:  %d\nagents:       %d\ncommands:     %d", len(p.Skills), len(servers), len(p.Agents), len(p.Commands))
		for _, warn := range append(append([]string{}, p.Warnings...), w...) {
			fmt.Fprintf(&b, "\nwarning:      %s", warn)
		}
		return b.String(), false, nil

	case "update":
		ctxPaths := paths()
		in := ctxPaths.installsMust()
		names := append([]string(nil), tokens[1:]...)
		if len(names) == 0 {
			for name := range in.Records {
				names = append(names, name)
			}
			sort.Strings(names)
		}
		if len(names) == 0 {
			return "nothing to update (no install records)", false, nil
		}
		var b strings.Builder
		mutated := false
		for _, name := range names {
			p, warns, err := updateInstalledPlugin(ctxPaths, name, in)
			if err != nil {
				// Partial success (earlier names updated) still returns its text.
				return strings.TrimRight(b.String(), "\n"), mutated, err
			}
			mutated = true
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(reportInstalled("updated", p, warns))
		}
		return b.String(), mutated, nil

	case "remove":
		if len(tokens) != 2 {
			return "", false, pluginsUsage("plugins remove <name>")
		}
		ctxPaths := paths()
		if err := plugin.RemovePlugin(tokens[1], ctxPaths.plugins, ctxPaths.installsMust()); err != nil {
			return "", false, err
		}
		return "removed " + tokens[1], true, nil

	case "shim":
		if len(tokens) != 2 {
			return "", false, pluginsUsage("plugins shim <dir>")
		}
		created, warns, err := plugin.WriteShim(tokens[1])
		if err != nil {
			return "", false, err
		}
		var b strings.Builder
		if len(created) == 0 {
			b.WriteString("nothing to do (portable manifest already present)")
		} else {
			fmt.Fprintf(&b, "shimmed: %s", strings.Join(created, ", "))
		}
		for _, w := range warns {
			fmt.Fprintf(&b, "\n  warning: %s", w)
		}
		// Not a mutation of the installed set: the shim target is an
		// arbitrary directory (usually a package about to be installed,
		// which mutates on its own). A shim written directly into a live
		// drop-in takes effect at the next start.
		return b.String(), false, nil
	}
	return "", false, pluginsUsage("plugins <add|marketplace|install|list|details|update|remove|shim>")
}

// runMarketplaceAction is the `plugins marketplace <add|list>` branch.
func runMarketplaceAction(tokens []string, opts pluginsActionOpts) (string, bool, error) {
	if len(tokens) < 2 {
		return "", false, pluginsUsage("plugins marketplace <add <owner/repo | https://…> | list>")
	}
	switch tokens[1] {
	case "add":
		if len(tokens) != 3 {
			return "", false, pluginsUsage("plugins marketplace add <owner/repo | https://…> [--ref <branch/tag> --sha <commit>]")
		}
		src, err := plugin.ParseAddRef(tokens[2])
		if err != nil {
			return "", false, err
		}
		src.Ref, src.SHA = opts.ref, opts.sha
		ctxPaths := paths()
		if err := os.MkdirAll(ctxPaths.marketplaces, 0o755); err != nil {
			return "", false, err
		}
		name := strings.TrimSuffix(filepath.Base(src.Repo+src.URL), ".git")
		if src.Repo == "" && src.URL == "" && src.Package != "" {
			name = src.Package
		}
		dest := filepath.Join(ctxPaths.marketplaces, name)
		_ = os.RemoveAll(dest)
		warns, err := plugin.FetchTo(context.Background(), http.DefaultClient, src, dest)
		if err != nil {
			return "", false, err
		}
		mp, mkWarns, err := plugin.LoadMarketplaceDir(dest)
		warns = append(warns, mkWarns...)
		if err != nil {
			return "", false, err
		}
		text := fmt.Sprintf("marketplace %s registered (%d plugins)", name, len(mp.Plugins))
		for _, w := range warns {
			text += "\n  warning: " + w
		}
		// A registered catalog is not part of the loaded plugin set.
		return text, false, nil

	case "list":
		ctxPaths := paths()
		entries, err := os.ReadDir(ctxPaths.marketplaces)
		if err != nil {
			if os.IsNotExist(err) {
				return "no marketplaces registered", false, nil
			}
			return "", false, err
		}
		var b strings.Builder
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if mp, _, err := plugin.LoadMarketplaceDir(filepath.Join(ctxPaths.marketplaces, e.Name())); err == nil {
				fmt.Fprintf(&b, "%-20s %s\n", e.Name(), mp.String())
			}
		}
		if b.Len() == 0 {
			return "no marketplaces registered", false, nil
		}
		return strings.TrimRight(b.String(), "\n"), false, nil
	}
	return "", false, pluginsUsage("plugins marketplace <add <owner/repo | https://…> | list>")
}

// reportInstalled renders one install/update result: the verb line, its
// components, and every warning (caller-provided skips, MilkServers skips,
// manifest warnings — same set printReport showed).
func reportInstalled(verb string, p *plugin.Plugin, warns []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s", verb, p.Manifest.Name, p.Manifest.Version)
	for _, s := range p.Skills {
		fmt.Fprintf(&b, "\n  skill %s", s.Name)
	}
	servers, w := p.MilkServers()
	for _, s := range servers {
		fmt.Fprintf(&b, "\n  mcp  %s", s.Name)
	}
	for _, warn := range append(append([]string{}, warns...), w...) {
		fmt.Fprintf(&b, "\n  warning: %s", warn)
	}
	for _, warn := range p.Warnings {
		fmt.Fprintf(&b, "\n  warning: %s", warn)
	}
	return b.String()
}

func entryNames(mp plugin.Marketplace) string {
	var names []string
	for _, ep := range mp.Plugins {
		names = append(names, ep.Name)
	}
	return strings.Join(names, ", ")
}

// resolveMarketplace maps a marketplace key onto its registered snapshot
// directory. `marketplace add` names the directory after the source (the
// owner/repo basename: add lliu56/minicut-skills → marketplaces/minicut-skills)
// while published install docs use the catalog's own name field
// ("name@marketplace", the ecosystem convention: minicut@minicut) — both
// spellings resolve, the catalog name winning on collision. The two halves
// used to disagree on the key (dir vs catalog name) and installs of exactly
// that shape failed with "no marketplace.json found".
func resolveMarketplace(marketplaces, key string) (string, error) {
	reg := registeredCatalogs(marketplaces)
	var byName, byDir []regCatalog
	for _, r := range reg {
		if r.name == key {
			byName = append(byName, r)
		}
		if r.dir == key {
			byDir = append(byDir, r)
		}
	}
	switch {
	case len(byName) > 1:
		return "", fmt.Errorf("marketplace %q is ambiguous (%s) — use its snapshot directory name", key, regCatalogList(byName))
	case len(byName) == 1:
		return filepath.Join(marketplaces, byName[0].dir), nil
	case len(byDir) > 0:
		return filepath.Join(marketplaces, byDir[0].dir), nil
	}
	if len(reg) == 0 {
		return "", fmt.Errorf("marketplace %q not registered (milk plugins marketplace add <owner/repo> first)", key)
	}
	return "", fmt.Errorf("marketplace %q not registered (have: %s)", key, regCatalogList(reg))
}

// regCatalog is one registered snapshot: its directory (the add-time alias)
// and the catalog's own name field (the install-time key).
type regCatalog struct{ dir, name string }

func regCatalogList(rs []regCatalog) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.dir+" (name "+r.name+")")
	}
	return strings.Join(s, ", ")
}

// registeredCatalogs reads every loadable snapshot under marketplaces/.
// A snapshot whose marketplace.json no longer parses is not registered for
// install (same skip rule marketplace list applies).
func registeredCatalogs(marketplaces string) []regCatalog {
	entries, err := os.ReadDir(marketplaces)
	if err != nil {
		return nil
	}
	var out []regCatalog
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mp, _, err := plugin.LoadMarketplaceDir(filepath.Join(marketplaces, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, regCatalog{dir: e.Name(), name: mp.Name})
	}
	return out
}

// errNotRegistered: no registered catalog carries the requested entry — the
// caller falls through to its own error (e.g. ParseAddRef's).
var errNotRegistered = errors.New("not found in any registered marketplace")

// registeredEntry finds entryName in the registered catalogs — the bare-name
// form of `plugins add`. Unique hit → the entry, its snapshot directory and
// a bookkeeping origin; none → errNotRegistered; several → an error naming
// every name@mkt candidate so the choice stays explicit.
func registeredEntry(marketplaces, entryName string) (plugin.MarketPlugin, string, string, error) {
	entries, err := os.ReadDir(marketplaces)
	if err != nil {
		if os.IsNotExist(err) {
			return plugin.MarketPlugin{}, "", "", errNotRegistered
		}
		return plugin.MarketPlugin{}, "", "", err
	}
	type hit struct {
		ep   plugin.MarketPlugin
		dir  string
		name string // catalog name for the name@mkt candidate
	}
	var hits []hit
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mp, _, err := plugin.LoadMarketplaceDir(filepath.Join(marketplaces, e.Name()))
		if err != nil {
			continue
		}
		if ep, ok := mp.Entry(entryName); ok {
			hits = append(hits, hit{ep: ep, dir: e.Name(), name: mp.Name})
		}
	}
	switch len(hits) {
	case 0:
		return plugin.MarketPlugin{}, "", "", errNotRegistered
	case 1:
		h := hits[0]
		return h.ep, filepath.Join(marketplaces, h.dir), h.dir + ":" + entryName, nil
	default:
		var cands []string
		for _, h := range hits {
			cands = append(cands, entryName+"@"+h.name)
		}
		return plugin.MarketPlugin{}, "", "", fmt.Errorf("%q exists in several marketplaces — install explicitly: %s", entryName, strings.Join(cands, ", "))
	}
}

// installFromEntry is the single install path for catalog entries: relative
// sources install straight from the registered snapshot (no fetch), every
// other source fetches from its own spec. Warnings (shim/load — the install
// path previously dropped them) come back for the report.
func installFromEntry(ep plugin.MarketPlugin, snapDir, origin string, ctxPaths pluginPaths) (*plugin.Plugin, []string, error) {
	if ep.Source.Kind == plugin.SourceRelative {
		srcDir, err := plugin.ResolveRelative(snapDir, ep.Source.Path)
		if err != nil {
			return nil, nil, err
		}
		return plugin.InstallFromDir(srcDir, origin, ep.Source, ctxPaths.plugins, ctxPaths.data, ctxPaths.installs())
	}
	return plugin.InstallPlugin(context.Background(), http.DefaultClient, ep.Source,
		origin, ctxPaths.plugins, ctxPaths.data, ctxPaths.installs())
}

// updateInstall refetches one recorded install. Relative marketplace sources
// re-install from their registered snapshot — FetchTo refuses them (a relative
// source was never fetchable), so `update` on a catalog install used to fail
// with exactly that rejection; the origin's marketplace key resolves through
// the same two-key rule as `install`. Everything else refetches from its
// structured source as before.
func updateInstalledPlugin(ctxPaths pluginPaths, name string, in *plugin.Installs) (*plugin.Plugin, []string, error) {
	rec, ok := in.Records[name]
	if !ok {
		return nil, nil, fmt.Errorf("no install record for %q (installed by hand?)", name)
	}
	if rec.Kind == plugin.SourceRelative.String() {
		mktKey, _, _ := strings.Cut(rec.Origin, ":")
		dir, err := resolveMarketplace(ctxPaths.marketplaces, mktKey)
		if err != nil {
			return nil, nil, fmt.Errorf("update %s: %w", name, err)
		}
		src := plugin.Source{Kind: plugin.SourceRelative, Path: rec.Path}
		srcDir, err := plugin.ResolveRelative(dir, rec.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("update %s: %w", name, err)
		}
		return plugin.InstallFromDir(srcDir, rec.Origin, src, ctxPaths.plugins, ctxPaths.data, in)
	}
	return plugin.UpdatePlugin(context.Background(), http.DefaultClient, name,
		ctxPaths.plugins, ctxPaths.data, in)
}

// pluginsActionNetwork marks actions that may take seconds (HTTP fetch or
// refetch) — the TUI announces them before going quiet.
func pluginsActionNetwork(tokens []string) bool {
	if len(tokens) == 0 {
		return false
	}
	switch tokens[0] {
	case "add", "install", "update":
		return true
	case "marketplace":
		return len(tokens) > 1 && tokens[1] == "add"
	}
	return false
}

// ---------------------------------------------------------------------------
// TUI face: /plugins suite
// ---------------------------------------------------------------------------

// pluginsActionMsg delivers a finished /plugins action back to the UI loop
// (the action ran on a goroutine — fetches are network-bound and must not
// block Update).
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
		m.appendTranscript(handlePluginsCmd())
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

func (c pluginBrowseChoice) tokens() []string {
	return []string{"install", c.name + "@" + c.mktDir}
}

// pluginBrowseState is the open browse flyover. Deliberately no deadline
// field: browsing a marketplace must never install anything by itself.
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
func newPluginsCmd() *cobra.Command {
	var ref, sha string
	opts := func() pluginsActionOpts { return pluginsActionOpts{ref: ref, sha: sha} }
	run := func(prefix []string) func(*cobra.Command, []string) error {
		return func(_ *cobra.Command, args []string) error {
			text, _, err := pluginsActionRun(append(append([]string{}, prefix...), args...), opts())
			if text != "" {
				fmt.Println(text)
			}
			return err
		}
	}

	cmd := &cobra.Command{Use: "plugins", Short: "Install and manage Agent Plugins"}

	add := &cobra.Command{
		Use:   "add <owner/repo | https://… | npm:pkg>",
		Short: "Install a plugin by reference (no clone)",
		RunE:  run([]string{"add"}),
	}
	add.Flags().StringVar(&ref, "ref", "", "branch or tag to fetch")
	add.Flags().StringVar(&sha, "sha", "", "commit pin to fetch")

	marketplace := &cobra.Command{Use: "marketplace", Short: "Plugin catalogs (marketplace.json interchange)"}
	mkAdd := &cobra.Command{
		Use:   "add <owner/repo | https://…>",
		Short: "Register a marketplace catalog",
		RunE:  run([]string{"marketplace", "add"}),
	}
	mkAdd.Flags().StringVar(&ref, "ref", "", "branch or tag to fetch")
	mkAdd.Flags().StringVar(&sha, "sha", "", "commit pin to fetch")
	marketplace.AddCommand(mkAdd, &cobra.Command{
		Use:   "list",
		Short: "List registered marketplaces",
		RunE:  run([]string{"marketplace", "list"}),
	})

	cmd.AddCommand(add, marketplace)
	cmd.AddCommand(
		&cobra.Command{
			Use:   "install <name>@<marketplace>",
			Short: "Install a plugin from a registered marketplace",
			RunE:  run([]string{"install"}),
		},
		&cobra.Command{
			Use:   "list",
			Short: "List installed plugins (bookkeeping + hand-installed)",
			RunE:  run([]string{"list"}),
		},
		&cobra.Command{
			Use:   "details <name>",
			Short: "Show an installed plugin's record and loaded components",
			RunE:  run([]string{"details"}),
		},
		&cobra.Command{
			Use:   "update [name]",
			Short: "Refetch installed plugins from their recorded sources",
			RunE:  run([]string{"update"}),
		},
		&cobra.Command{
			Use:   "remove <name>",
			Short: "Uninstall a recorded plugin (PLUGIN_DATA is preserved)",
			RunE:  run([]string{"remove"}),
		},
		&cobra.Command{
			Use:   "shim <dir>",
			Short: "Write a portable manifest into a .claude-plugin/ package (create-only)",
			RunE:  run([]string{"shim"}),
		},
	)
	return cmd
}

// pluginPaths are the state-root locations the distribution layer owns.
type pluginPaths struct {
	plugins      string
	data         string
	marketplaces string
	installsFile string
}

func paths() pluginPaths {
	stateRoot, _ := config.Dir()
	return pluginPaths{
		plugins:      filepath.Join(stateRoot, "plugins"),
		data:         filepath.Join(stateRoot, "plugin-data"),
		marketplaces: filepath.Join(stateRoot, "marketplaces"),
		installsFile: filepath.Join(stateRoot, "plugin-installs.json"),
	}
}

func (p pluginPaths) installs() *plugin.Installs {
	in, err := plugin.LoadInstalls(p.installsFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v (starting fresh bookkeeping)\n", err)
		in = &plugin.Installs{Path: p.installsFile, Records: map[string]plugin.InstallRecord{}}
	}
	return in
}

func (p pluginPaths) installsMust() *plugin.Installs { return p.installs() }
