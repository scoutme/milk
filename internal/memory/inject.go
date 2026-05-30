package memory

// FilterByRelevance returns the subset of percepts that share at least one
// non-stop token with the prompt. When the prompt tokenizes to nothing (empty
// or all stop-words), all percepts pass through unchanged.
func FilterByRelevance(percepts []Percept, prompt string) []Percept {
	promptTokens := tokenize(prompt)
	if len(promptTokens) == 0 {
		return percepts
	}
	out := percepts[:0:0]
	for _, p := range percepts {
		for t := range tokenize(p.Content) {
			if _, ok := promptTokens[t]; ok {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// LimitInjection trims percepts to fit within maxCount and maxBytes.
// percepts must already be sorted by weight descending (highest first).
// A limit of 0 means no restriction for that dimension.
func LimitInjection(percepts []Percept, maxCount, maxBytes int) []Percept {
	var out []Percept
	totalBytes := 0
	for _, p := range percepts {
		if maxCount > 0 && len(out) >= maxCount {
			break
		}
		b := len(p.Content)
		if maxBytes > 0 && totalBytes+b > maxBytes {
			break
		}
		out = append(out, p)
		totalBytes += b
	}
	return out
}
