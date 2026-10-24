package escalation

import (
	"strings"
	"testing"
)

func TestBuildStaticContext_CarriesTableGuidance(t *testing.T) {
	got := BuildStaticContext("nonce", nil, ContextModeFirst, false, "primary", "claude")
	if !strings.Contains(got, TableGuidance) {
		t.Error("the cached prefix must carry the table guidance")
	}
}
