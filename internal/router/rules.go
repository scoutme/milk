package router

import (
	"strings"
	"unicode/utf8"

	"github.com/scoutme/milk/internal/config"
)

// rulesDecision returns a routing decision if the rules layer is conclusive,
// or Decision{} with Conclusive=false if the model classifier should be consulted.
func rulesDecision(prompt string, cfg config.Config) Decision {
	// Token-length heuristic (rough: 1 token ≈ 4 chars)
	approxTokens := utf8.RuneCountInString(prompt) / 4
	if approxTokens > cfg.Rules.EscalateAboveTokens {
		return Decision{Target: TargetClaude, Conclusive: true, Reason: "prompt exceeds token threshold"}
	}

	lower := strings.ToLower(prompt)
	for _, kw := range cfg.Rules.EscalateKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return Decision{Target: TargetClaude, Conclusive: true, Reason: "keyword match: " + kw}
		}
	}

	return Decision{Conclusive: false}
}
