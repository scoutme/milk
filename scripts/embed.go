// Package scripts exposes scripts bundled with the milk binary.
package scripts

import _ "embed"

// SmolagentScript is the embedded milk-smolagent Python adapter script.
//
//go:embed milk-smolagent
var SmolagentScript []byte
