// Package shelldetect heuristically identifies whether a short text prompt
// looks like a direct shell command rather than a natural-language request.
// It is intentionally conservative: false-negatives (routing a command to the
// agent) are safe; false-positives (running arbitrary text as a shell command)
// are not.
package shelldetect

import (
	"strings"
	"unicode"
)

// knownBinaries is the set of first-token names recognised as shell commands.
var knownBinaries = map[string]bool{
	"ls":       true,
	"git":      true,
	"cat":      true,
	"grep":     true,
	"find":     true,
	"cd":       true,
	"echo":     true,
	"curl":     true,
	"make":     true,
	"task":     true,
	"go":       true,
	"python":   true,
	"python3":  true,
	"pip":      true,
	"pip3":     true,
	"npm":      true,
	"yarn":     true,
	"docker":   true,
	"kubectl":  true,
	"helm":     true,
	"cargo":    true,
	"rustc":    true,
	"node":     true,
	"deno":     true,
	"bun":      true,
	"brew":     true,
	"apt":      true,
	"apt-get":  true,
	"yum":      true,
	"dnf":      true,
	"pacman":   true,
	"touch":    true,
	"mkdir":    true,
	"rm":       true,
	"mv":       true,
	"cp":       true,
	"pwd":      true,
	"which":    true,
	"env":      true,
	"export":   true,
	"source":   true,
	"ssh":      true,
	"scp":      true,
	"rsync":    true,
	"tar":      true,
	"zip":      true,
	"unzip":    true,
	"sed":      true,
	"awk":      true,
	"jq":       true,
	"sort":     true,
	"uniq":     true,
	"wc":       true,
	"head":     true,
	"tail":     true,
	"less":     true,
	"more":     true,
	"diff":     true,
	"patch":    true,
	"chmod":    true,
	"chown":    true,
	"sudo":     true,
	"su":       true,
	"kill":     true,
	"ps":       true,
	"top":      true,
	"htop":     true,
	"df":       true,
	"du":       true,
	"free":     true,
	"uname":    true,
	"date":     true,
	"time":     true,
	"sleep":    true,
	"ping":     true,
	"netstat":  true,
	"ifconfig": true,
	"ip":       true,
	"nc":       true,
	"ncat":     true,
	"wget":     true,
	"open":     true,
	"xdg-open": true,
	"code":     true,
	"vim":      true,
	"nvim":     true,
	"nano":     true,
	"emacs":    true,
}

// nlStarters are common natural-language question/sentence openers.
// A prompt whose first token matches one of these is never a shell command.
var nlStarters = map[string]bool{
	"what":     true,
	"how":      true,
	"why":      true,
	"can":      true,
	"does":     true,
	"is":       true,
	"are":      true,
	"do":       true,
	"should":   true,
	"could":    true,
	"would":    true,
	"will":     true,
	"when":     true,
	"where":    true,
	"who":      true,
	"explain":  true,
	"describe": true,
	"tell":     true,
	"show":     true,
	"list":     true,
	"write":    true,
	"create":   true,
	"help":     true,
	"please":   true,
	"i":        true,
	"the":      true,
	"a":        true,
	"an":       true,
}

// IsShellCommand reports whether input looks like a direct shell command and
// returns the command to execute (the original input, passed to sh -c).
//
// Conservative heuristics applied in order:
//  1. Input must be non-empty.
//  2. Input must not end with '?' (natural-language question).
//  3. Input must not contain more than 100 characters (long sentences aren't commands).
//  4. The first whitespace-token of input must be in knownBinaries.
//  5. The first token must not be in nlStarters.
//  6. None of the tokens (excluding flags starting with '-' and path-like tokens)
//     must be a natural-language word longer than 8 alphabetic chars.
//
// Pipes (|) and redirects (>, <, >>) are allowed — the full input is passed to
// sh -c on execution.
func IsShellCommand(input string) (cmd string, ok bool) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", false
	}
	// Ends with '?' → natural language question.
	if strings.HasSuffix(input, "?") {
		return "", false
	}
	// Excessively long → likely a sentence, not a command.
	if len(input) > 150 {
		return "", false
	}

	// Split on whitespace to get tokens, but keep the full input for execution.
	tokens := strings.Fields(input)
	if len(tokens) == 0 {
		return "", false
	}

	first := strings.ToLower(tokens[0])

	// Natural-language openers override everything.
	if nlStarters[first] {
		return "", false
	}

	// First token must be a known binary.
	if !knownBinaries[first] {
		return "", false
	}

	// Scan remaining tokens for natural-language indicators.
	for _, tok := range tokens[1:] {
		if isNLWord(tok) {
			return "", false
		}
	}

	return input, true
}

// isNLWord returns true when tok looks like a natural-language word rather than
// a shell argument. Flags (-v, --verbose), paths (/foo, ./bar), and tokens
// containing digits or special characters are skipped.
func isNLWord(tok string) bool {
	if tok == "" {
		return false
	}
	// Shell operator / redirect characters → not NL.
	if tok == "|" || tok == ">" || tok == "<" || tok == ">>" || tok == "&&" || tok == "||" || tok == ";" {
		return false
	}
	// Flag: starts with '-'.
	if strings.HasPrefix(tok, "-") {
		return false
	}
	// Path-like: starts with '/' or './' or '~'.
	if strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "./") || strings.HasPrefix(tok, "~/") || strings.HasPrefix(tok, "~") {
		return false
	}
	// Token with any digit, '@', ':', '=', '.', '*', '?' → likely argument, not NL.
	for _, r := range tok {
		if unicode.IsDigit(r) || r == '@' || r == ':' || r == '=' || r == '.' || r == '*' || r == '?' {
			return false
		}
	}
	// Pure alphabetic word longer than 8 chars → suspicious NL word.
	allAlpha := true
	for _, r := range tok {
		if !unicode.IsLetter(r) {
			allAlpha = false
			break
		}
	}
	if allAlpha && len(tok) > 8 {
		return true
	}
	return false
}

// FirstToken returns the first whitespace-delimited token of input (lowercased),
// used for checking against a DirectBashAllow list.
func FirstToken(input string) string {
	fields := strings.Fields(strings.TrimSpace(input))
	if len(fields) == 0 {
		return ""
	}
	return strings.ToLower(fields[0])
}
