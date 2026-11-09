package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/plugin"
)

// seedAliasMarketplace lays down the minicut-shaped catalog: the snapshot
// directory (named after the source repo by `marketplace add`) differs from
// the catalog's own name field — the exact axis mismatch that made
// `install minicut@minicut` fail with "no marketplace.json found".
func seedAliasMarketplace(t *testing.T) {
	t.Helper()
	writeStateFile(t, "marketplaces/snap-alias/marketplace.json",
		`{"name":"friendly","owner":{"name":"t"},"plugins":[
		  {"name":"beta","description":"beta plugin","source":"./beta"}]}`)
	writeStateFile(t, "marketplaces/snap-alias/beta/plugin.json",
		`{"$schema":"`+plugin.SchemaPlugin+`","name":"beta","version":"0.2.0","description":"beta plugin"}`)
}

// TestResolveMarketplace_CanonicalAndAlias pins the two-key resolution: the
// catalog's own name (published docs) and the snapshot directory (the add-time
// alias) both resolve to the same directory; unknown keys list what's
// registered; an empty registry says how to register one.
func TestResolveMarketplace_CanonicalAndAlias(t *testing.T) {
	resetPluginState(t)
	seedAliasMarketplace(t)
	root := paths().marketplaces

	for _, key := range []string{"friendly", "snap-alias"} {
		dir, err := resolveMarketplace(root, key)
		if err != nil {
			t.Fatalf("resolveMarketplace(%q): %v", key, err)
		}
		if dir != filepath.Join(root, "snap-alias") {
			t.Errorf("resolveMarketplace(%q) = %q, want the snap-alias dir", key, dir)
		}
	}

	if _, err := resolveMarketplace(root, "nope"); err == nil || !strings.Contains(err.Error(), "have:") || !strings.Contains(err.Error(), "snap-alias (name friendly)") {
		t.Errorf("unknown key: err = %v, want a listing of registered catalogs", err)
	}

	os.RemoveAll(root)
	if _, err := resolveMarketplace(root, "friendly"); err == nil || !strings.Contains(err.Error(), "marketplace add <owner/repo> first") {
		t.Errorf("empty registry: err = %v, want the register hint", err)
	}
}

// TestResolveMarketplace_Ambiguous pins the collision rule: two snapshots
// with the same catalog name can't both win — the error names both dirs and
// tells the user to key by directory.
func TestResolveMarketplace_Ambiguous(t *testing.T) {
	resetPluginState(t)
	writeStateFile(t, "marketplaces/dupA/marketplace.json",
		`{"name":"dup","owner":{"name":"t"},"plugins":[]}`)
	writeStateFile(t, "marketplaces/dupB/marketplace.json",
		`{"name":"dup","owner":{"name":"t"},"plugins":[]}`)

	_, err := resolveMarketplace(paths().marketplaces, "dup")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") ||
		!strings.Contains(err.Error(), "dupA") || !strings.Contains(err.Error(), "dupB") {
		t.Fatalf("ambiguous key: err = %v, want both directories named", err)
	}
	// The alias still resolves unambiguously.
	if dir, err := resolveMarketplace(paths().marketplaces, "dupB"); err != nil || dir == "" {
		t.Errorf("dir alias of an ambiguous catalog: dir=%q err=%v", dir, err)
	}
}

// TestPluginsInstall_ByCatalogName is the user-reported failure as an action:
// `install <name>@<catalog-name>` through the resolver (it used to join the
// key onto the marketplaces dir directly and miss the snapshot).
func TestPluginsInstall_ByCatalogName(t *testing.T) {
	resetPluginState(t)
	seedAliasMarketplace(t)

	for _, key := range []string{"beta@friendly", "beta@snap-alias"} {
		text, mutated, err := pluginsActionRun([]string{"install", key}, pluginsActionOpts{})
		if err != nil {
			t.Fatalf("install %s: %v", key, err)
		}
		if !mutated || !strings.Contains(text, "installed beta 0.2.0") {
			t.Errorf("install %s = %q mutated=%v", key, text, mutated)
		}
		if _, err := os.Stat(filepath.Join(paths().plugins, "beta")); err != nil {
			t.Errorf("install %s: plugin dir missing: %v", key, err)
		}
	}
}

// TestPluginsAdd_BareNameThroughCatalog pins the bare-name form users reach
// for (`add minicut`): it resolves through registered catalogs — the suffix
// is the only thing missing — while unknown names keep ParseAddRef's error
// and a name in several catalogs stays an explicit name@mkt choice.
func TestPluginsAdd_BareNameThroughCatalog(t *testing.T) {
	resetPluginState(t)
	seedSuiteMarketplace(t) // mp1/catalog "mp1", entry "demo" (relative source)

	text, mutated, err := pluginsActionRun([]string{"add", "demo"}, pluginsActionOpts{})
	if err != nil {
		t.Fatalf("add demo: %v", err)
	}
	if !mutated || !strings.Contains(text, "installed demo 0.1.0") || !strings.Contains(text, "from marketplace") {
		t.Errorf("add demo = %q mutated=%v", text, mutated)
	}
	rec, err := plugin.LoadInstalls(paths().installsFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Records["demo"].Origin; got != "mp1:demo" {
		t.Errorf("origin = %q, want mp1:demo", got)
	}

	// Unknown name → ParseAddRef's own error stands.
	if _, _, err := pluginsActionRun([]string{"add", "nope"}, pluginsActionOpts{}); err == nil ||
		!strings.Contains(err.Error(), "unrecognized plugin reference") {
		t.Errorf("unknown name: err = %v, want the unrecognized-reference error", err)
	}

	// The same entry name in two catalogs → explicit candidates, no guessing.
	writeStateFile(t, "marketplaces/other/marketplace.json",
		`{"name":"other-cat","owner":{"name":"t"},"plugins":[
		  {"name":"demo","description":"same name elsewhere","source":"./demo"}]}`)
	_, _, err = pluginsActionRun([]string{"add", "demo"}, pluginsActionOpts{})
	if err == nil || !strings.Contains(err.Error(), "several marketplaces") ||
		!strings.Contains(err.Error(), "demo@mp1") || !strings.Contains(err.Error(), "demo@other-cat") {
		t.Errorf("ambiguous bare name: err = %v, want both name@mkt candidates", err)
	}
}

// TestInstalledOriginStaysStable guards bookkeeping for update: an install
// keyed by the catalog name records the snapshot directory as origin (what a
// refetch must re-open), not the typed key.
func TestInstalledOriginStaysStable(t *testing.T) {
	resetPluginState(t)
	seedAliasMarketplace(t)
	if _, _, err := pluginsActionRun([]string{"install", "beta@friendly"}, pluginsActionOpts{}); err != nil {
		t.Fatal(err)
	}
	rec, err := plugin.LoadInstalls(paths().installsFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Records["beta"].Origin; got != "friendly:beta" {
		t.Errorf("origin = %q, want friendly:beta (typed key, origin is display)", got)
	}
	// And the snapshot itself still exists for a refetch-based update.
	if _, err := os.Stat(filepath.Join(paths().marketplaces, "snap-alias", "marketplace.json")); err != nil {
		t.Errorf("snapshot vanished: %v", err)
	}
}

// TestPluginsUpdate_RelativeMarketplaceInstall pins the update path for
// catalog installs: re-install from the registered snapshot (FetchTo refuses
// relative sources — `update` used to fail on exactly that), picking up the
// snapshot's new content through the origin's marketplace key.
func TestPluginsUpdate_RelativeMarketplaceInstall(t *testing.T) {
	resetPluginState(t)
	seedAliasMarketplace(t)
	if _, _, err := pluginsActionRun([]string{"install", "beta@friendly"}, pluginsActionOpts{}); err != nil {
		t.Fatal(err)
	}
	// The catalog publishes a new version in its snapshot…
	writeStateFile(t, "marketplaces/snap-alias/beta/plugin.json",
		`{"$schema":"`+plugin.SchemaPlugin+`","name":"beta","version":"0.3.0","description":"beta plugin"}`)
	// …and update picks it up — through the recorded origin's marketplace key.
	text, mutated, err := pluginsActionRun([]string{"update", "beta"}, pluginsActionOpts{})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !mutated || !strings.Contains(text, "updated beta 0.3.0") {
		t.Errorf("update = %q mutated=%v, want the snapshot's new version", text, mutated)
	}
	// An unrecorded name keeps the loud not-found error.
	if _, _, err := pluginsActionRun([]string{"update", "ghost"}, pluginsActionOpts{}); err == nil ||
		!strings.Contains(err.Error(), "no install record") {
		t.Errorf("unknown update: err = %v", err)
	}
}
