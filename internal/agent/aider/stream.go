package aider

import (
	"fmt"
	"io"
	"strings"

	"github.com/scoutme/milk/internal/agent/subprocess"
)

// Parser implements subprocess.StreamParser for the aider CLI.
// aider emits plain text; decoration lines are suppressed.
type Parser struct{}

func (p *Parser) Parse(r io.Reader, out io.Writer, opts subprocess.ParseOpts) (subprocess.ParseResult, error) {
	pt := &subprocess.PlainTextParser{
		SkipLine: isAiderDecoration,
		EventHook: func(line string, out io.Writer, textBuf *strings.Builder) {
			// Detect diff-format edit markers and emit a dim annotation.
			if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
				path := strings.TrimPrefix(strings.TrimPrefix(line, "--- "), "+++ ")
				if path != "/dev/null" && !strings.HasPrefix(path, "a/") && !strings.HasPrefix(path, "b/") {
					block := fmt.Sprintf("\n\033[2m✎ %s\033[0m\n", path)
					fmt.Fprint(out, block) //nolint:errcheck
					textBuf.WriteString(block)
					return
				}
			}
			fmt.Fprintln(out, line) //nolint:errcheck
			textBuf.WriteString(line)
			textBuf.WriteByte('\n')
		},
	}
	return pt.Parse(r, out, opts)
}

// isAiderDecoration returns true for lines aider emits for its own UI that
// carry no content for the milk transcript.
func isAiderDecoration(line string) bool {
	if strings.HasPrefix(line, "─") {
		return true
	}
	if strings.HasPrefix(line, "Aider v") {
		return true
	}
	return false
}
