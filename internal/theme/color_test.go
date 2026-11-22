package theme

import "testing"

func TestParseHexForms(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"#FFF", "#ffffff"},
		{"#f8f5ee", "#f8f5ee"},
		{"#AbCdEf", "#abcdef"},
		{"", ""},
	} {
		got, err := ParseHex(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseHex(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"#zzz", "#12345", "#1234567", "blue", "#"} {
		if got, err := ParseHex(bad); err == nil {
			t.Errorf("ParseHex(%q) = %q, nil; want error", bad, got)
		}
	}
}

func TestColorJSONForms(t *testing.T) {
	var c Color
	if err := c.UnmarshalJSON([]byte(`"#abc"`)); err != nil || c != Fixed("#aabbcc") {
		t.Fatalf("string form: %+v, %v", c, err)
	}
	if err := c.UnmarshalJSON([]byte(`{"dark":"#111111","light":"#EEEEEE"}`)); err != nil ||
		c != (Color{Dark: "#111111", Light: "#eeeeee"}) {
		t.Fatalf("pair form: %+v, %v", c, err)
	}
	if err := c.UnmarshalJSON([]byte(`{"dark":"nope","light":"#eee"}`)); err == nil {
		t.Fatal("bad pair hex must error")
	}
	// Marshal picks the compact form for fixed tokens.
	if b, err := Fixed("#aabbcc").MarshalJSON(); err != nil || string(b) != `"#aabbcc"` {
		t.Fatalf("marshal fixed: %s, %v", b, err)
	}
	if b, err := (Color{Dark: "#111111", Light: "#eeeeee"}).MarshalJSON(); err != nil ||
		string(b) != `{"dark":"#111111","light":"#eeeeee"}` {
		t.Fatalf("marshal pair: %s, %v", b, err)
	}
}

func TestMixGoldens(t *testing.T) {
	base := Fixed("#f8f5ee")
	accent := Fixed("#6b6140")
	if got := Mix(base, accent, 0.15).Dark; got != "#e3dfd4" {
		t.Errorf("Mix(base, accent, 0.15) = %q, want #e3dfd4", got)
	}
	if got := Mix(accent, base, 0.7).Dark; got != "#cec9ba" {
		t.Errorf("Mix(accent, base, 0.7) = %q, want #cec9ba", got)
	}
	// Per-variant: pairs mix variant-wise.
	p := Mix(Color{Dark: "#000000", Light: "#ffffff"}, Color{Dark: "#444444", Light: "#cccccc"}, 0.5)
	if p.Dark != "#222222" || p.Light != "#e6e6e6" {
		t.Errorf("Mix pair = %+v", p)
	}
	// Absent input keeps the token absent (derivations must not invent halves).
	if got := Mix(Color{}, Fixed("#ffffff"), 0.5); !got.Absent() {
		t.Errorf("Mix with absent input = %+v, want absent", got)
	}
}

func TestLightenGoldens(t *testing.T) {
	if got := Lighten(Fixed("#f8f5ee"), 0.08).Dark; got != "#ffffff" {
		t.Errorf("Lighten(#f8f5ee, 0.08) = %q, want #ffffff (clamped)", got)
	}
	if got := Lighten(Fixed("#3f2a26"), -0.1).Dark; got != "#1f1513" {
		t.Errorf("Lighten(#3f2a26, -0.1) = %q, want #1f1513", got)
	}
	if got := Lighten(Color{}, 0.1); !got.Absent() {
		t.Errorf("Lighten of absent = %+v, want absent", got)
	}
}

func TestHSLRoundTrip(t *testing.T) {
	for _, hex := range []string{"#f8f5ee", "#3f2a26", "#6b6140", "#000000", "#ffffff", "#4a6280"} {
		h, s, l := hexToHSL(hex)
		if got := hslToHex(h, s, l); got != hex {
			t.Errorf("round trip %s → %s (h=%.2f s=%.2f l=%.2f)", hex, got, h, s, l)
		}
	}
}
