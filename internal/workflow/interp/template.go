package interp

import (
	"bytes"
	"fmt"
	"text/template"
)

// renderTemplate renders body as a Go text/template against vars. Missing
// keys resolve to the zero value (nil) rather than erroring or emitting
// "<no value>" — every prompt in the built-in definitions guards optional
// vars with {{if .x}}, where a nil interface is falsy.
func renderTemplate(name, body string, vars map[string]any) (string, error) {
	tmpl, err := template.New(name).Option("missingkey=zero").Parse(body)
	if err != nil {
		return "", fmt.Errorf("parsing template %q: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", fmt.Errorf("executing template %q: %w", name, err)
	}
	return buf.String(), nil
}
