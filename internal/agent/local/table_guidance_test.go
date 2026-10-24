package local

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/escalation"
)

func TestSystemPrompt_CarriesTableGuidanceUnlessMinimal(t *testing.T) {
	for _, tier := range []string{"standard", "full"} {
		if p := buildSystemPrompt("", "agent", "", false, tier, true); !strings.Contains(p, escalation.TableGuidance) {
			t.Errorf("%s tier misses the table guidance", tier)
		}
	}
	if p := buildSystemPrompt("", "agent", "", false, "minimal", true); strings.Contains(p, escalation.TableGuidance) {
		t.Error("the minimal tier is role framing only and must stay lean")
	}
}
