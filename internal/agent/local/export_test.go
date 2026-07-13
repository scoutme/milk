package local

// IsRepeatedPromptExported exposes isRepeatedPrompt for cross-package tests.
func IsRepeatedPromptExported(history []Message, userPrompt string) bool {
	return isRepeatedPrompt(history, userPrompt)
}
