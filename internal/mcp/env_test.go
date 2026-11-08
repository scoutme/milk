package mcp

import (
	"strings"
	"testing"
)

func TestStdioEnv_OverlayAndReserved(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/home/u", "EXTRA=1"}
	overlay := map[string]string{"EXTRA": "2", "NEW": "x"}
	out := stdioEnv(base, overlay, "/root", "/data")

	get := func(k string) (string, bool) {
		for _, kv := range out {
			if bk, v, ok := strings.Cut(kv, "="); ok && bk == k {
				return v, true
			}
		}
		return "", false
	}
	if v, _ := get("EXTRA"); v != "2" {
		t.Errorf("overlay must replace same-name: EXTRA=%q", v)
	}
	if v, _ := get("NEW"); v != "x" {
		t.Errorf("overlay must append: NEW=%q", v)
	}
	if v, _ := get("PATH"); v != "/usr/bin" {
		t.Errorf("base preserved: PATH=%q", v)
	}
	// Reserved variables set last, replacing same-name entries (§9.1).
	if v, _ := get("PLUGIN_ROOT"); v != "/root" {
		t.Errorf("PLUGIN_ROOT=%q", v)
	}
	if v, _ := get("PLUGIN_DATA"); v != "/data" {
		t.Errorf("PLUGIN_DATA=%q", v)
	}
}

func TestStdioEnv_ReservedCannotBeOverridden(t *testing.T) {
	overlay := map[string]string{"PLUGIN_ROOT": "/evil"}
	out := stdioEnv(nil, overlay, "/root", "/data")
	for _, kv := range out {
		if strings.HasPrefix(kv, "PLUGIN_ROOT=") && kv != "PLUGIN_ROOT=/root" {
			t.Fatalf("configured PLUGIN_ROOT must lose: %q", kv)
		}
	}
}
