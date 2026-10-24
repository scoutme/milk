package main

import (
	"testing"

	"github.com/scoutme/milk/internal/config"
)

func TestDebugLogFormat_SwitchesToPrettyAndBack(t *testing.T) {
	cases := []struct{ before, wantSaved string }{
		{"", "text"},
		{"text", "text"},
		{"json", "json"},
	}
	for _, c := range cases {
		o := config.OtelConfig{LogFormat: c.before}
		savePrettyLogFormat(&o)
		if o.LogFormat != "pretty" || o.PreDebugLogFormat != c.wantSaved {
			t.Errorf("enable from %q: got format %q saved %q", c.before, o.LogFormat, o.PreDebugLogFormat)
		}
		restoreLogFormat(&o)
		if o.LogFormat != c.wantSaved || o.PreDebugLogFormat != "" {
			t.Errorf("disable from %q: got format %q saved %q", c.before, o.LogFormat, o.PreDebugLogFormat)
		}
	}
}

func TestDebugLogFormat_KeepsPrettyAndOff(t *testing.T) {
	for _, f := range []string{"pretty", "off"} {
		o := config.OtelConfig{LogFormat: f}
		savePrettyLogFormat(&o)
		if o.LogFormat != f || o.PreDebugLogFormat != "" {
			t.Errorf("%q must be left alone, got %q / %q", f, o.LogFormat, o.PreDebugLogFormat)
		}
		restoreLogFormat(&o)
		if o.LogFormat != f {
			t.Errorf("%q must survive disable, got %q", f, o.LogFormat)
		}
	}
}
