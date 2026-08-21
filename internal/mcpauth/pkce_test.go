package mcpauth

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestGenerateVerifier(t *testing.T) {
	v, err := GenerateVerifier()
	if err != nil {
		t.Fatalf("GenerateVerifier: %v", err)
	}
	if len(v) < 43 || len(v) > 128 {
		t.Errorf("verifier length %d out of RFC 7636 range [43,128]", len(v))
	}
	for _, r := range v {
		unreserved := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '.' || r == '_' || r == '~'
		if !unreserved {
			t.Errorf("verifier contains non-unreserved char %q", r)
		}
	}

	v2, err := GenerateVerifier()
	if err != nil {
		t.Fatalf("GenerateVerifier (2nd): %v", err)
	}
	if v == v2 {
		t.Error("two successive verifiers were identical")
	}
}

func TestChallenge(t *testing.T) {
	// RFC 7636 Appendix B test vector.
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	want := "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := Challenge(verifier); got != want {
		t.Errorf("Challenge(%q) = %q, want %q", verifier, got, want)
	}

	// Sanity: matches a fresh sha256 computation, not just the fixed vector.
	sum := sha256.Sum256([]byte(verifier))
	if got, wantRaw := Challenge(verifier), base64.RawURLEncoding.EncodeToString(sum[:]); got != wantRaw {
		t.Errorf("Challenge mismatch with direct computation: got %q want %q", got, wantRaw)
	}
}

func TestRandomState(t *testing.T) {
	s1, err := RandomState()
	if err != nil {
		t.Fatalf("RandomState: %v", err)
	}
	s2, err := RandomState()
	if err != nil {
		t.Fatalf("RandomState (2nd): %v", err)
	}
	if s1 == s2 {
		t.Error("two successive states were identical")
	}
	if s1 == "" {
		t.Error("state was empty")
	}
}
