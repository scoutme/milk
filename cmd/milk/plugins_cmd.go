package main

// The shared plugin-distribution action layer (ADR-0104 Part II): one
// pluginsActionRun behind two faces — the cobra CLI (plugins_cli.go) and
// the TUI /plugins suite (plugins_tui.go) — so the surfaces can never
// drift ("one source for what is executed", the acp_commands.go pattern).
// This file holds the action layer itself, marketplace resolution and
// install/update plumbing.

import (
	"context"
	"errors"
	"fmt"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/plugin"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type pluginsActionOpts struct {
	ref, sha string
}

// pluginsUsage formats a host-neutral usage error ("usage: plugins …") —
// both hosts render it verbatim.

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
			p, warns, err := plugin.InstallPlugin(context.Background(), pluginsHTTP, src, tokens[1],
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
		name := marketplaceDirName(src)
		dest := filepath.Join(ctxPaths.marketplaces, name)
		// Refuse before touching anything (review W5): the old flow
		// RemoveAll'd the destination BEFORE fetching — a collision or a
		// failed fetch destroyed the previous registration outright.
		if _, err := os.Stat(dest); err == nil {
			return "", false, fmt.Errorf("marketplace %s already registered (%s) — remove that directory to re-register", name, dest)
		}
		warns, err := plugin.FetchTo(context.Background(), pluginsHTTP, src, dest)
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
	return plugin.InstallPlugin(context.Background(), pluginsHTTP, ep.Source,
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
	return plugin.UpdatePlugin(context.Background(), pluginsHTTP, name,
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

func (c pluginBrowseChoice) tokens() []string {
	return []string{"install", c.name + "@" + c.mktDir}
}

// pluginBrowseState is the open browse flyover. Deliberately no deadline
// field: browsing a marketplace must never install anything by itself.

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

// pluginsHTTP is the one HTTP client every distribution fetch runs on
// (review W2): bounded so a stalled peer can never wedge the TUI — the
// action goroutine would otherwise hold the pluginsBusy latch forever and
// pluginsActionMsg would never arrive. FetchTo's own 2-minute default only
// covers nil clients; the action layer must pass THIS one.

var pluginsHTTP = &http.Client{Timeout: 2 * time.Minute}

// marketplaceDirName derives the snapshot directory for `marketplace add`
// (review W5): unique per SOURCE, never per basename. The old name —
// filepath.Base of the repo-or-URL — collapsed "a/tools" and "b/tools" onto
// one directory and silently deleted the loser, and gave URL sources names
// like "plugin.zip".

func marketplaceDirName(src plugin.Source) string {
	switch src.Kind {
	case plugin.SourceGitHub:
		return strings.ReplaceAll(src.Repo, "/", "-")
	case plugin.SourceNPM:
		return strings.NewReplacer("/", "-", "@", "").Replace(src.Package)
	}
	base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(
		path.Base(src.URL), ".gz"), ".tgz"), ".zip"), ".tar")
	host := src.URL
	if u, err := url.Parse(src.URL); err == nil {
		host = u.Host
	}
	return strings.ReplaceAll(host, ":", "-") + "-" + base
}
