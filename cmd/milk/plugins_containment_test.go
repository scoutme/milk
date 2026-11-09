package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/plugin"
)

// TestInstallFromEntry_RelativeSourceCannotEscape (review C1): the
// marketplace-relative install path must refuse a source that points outside
// the registered snapshot, whatever the catalog or a bookkeeping record says.
// Before the fix installFromEntry/updateInstalledPlugin joined the raw path
// onto the snapshot dir — a crafted record copied an arbitrary host directory
// into the plugin tree. The parse-level guard (plugin.source) is the first
// line; this pins the install-path containment behind it (defense in depth:
// update joins the stored rec.Path through the same guard).
func TestInstallFromEntry_RelativeSourceCannotEscape(t *testing.T) {
	resetPluginState(t)

	// A valid plugin OUTSIDE the marketplace snapshot (it WOULD install —
	// the only question is containment).
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "plugin.json"),
		[]byte(`{"$schema":"`+plugin.SchemaPlugin+`","name":"outside-plugin"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	mkt := filepath.Join(paths().marketplaces, "evil")
	if err := os.MkdirAll(mkt, 0o755); err != nil {
		t.Fatal(err)
	}
	relOut, err := filepath.Rel(mkt, outside) // joined onto the marketplace ROOT
	if err != nil {
		t.Fatal(err)
	}
	relOut = "./" + filepath.ToSlash(relOut)
	writeStateFile(t, "marketplaces/evil/marketplace.json",
		`{"name":"evil","owner":{"name":"t"},"plugins":[]}`)

	// The poisoned bookkeeping record shape reaching updateInstalledPlugin:
	// a relative source whose path climbs out of the snapshot.
	in := paths().installsMust()
	in.Records["escapee"] = plugin.InstallRecord{
		Name: "escapee", Kind: plugin.SourceRelative.String(),
		Path: relOut, Origin: "evil:escapee",
	}
	if _, _, err := updateInstalledPlugin(paths(), "escapee", in); err == nil {
		t.Fatal("update from a relative record pointing outside the snapshot must be refused")
	} else if !strings.Contains(err.Error(), "escape") {
		t.Fatalf("expected a containment error, got %v", err)
	}
}
