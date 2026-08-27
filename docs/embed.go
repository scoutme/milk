// Package docs embeds milk's own canonical reference docs into the binary so
// they can be looked up at runtime (see internal/selfdocs) without drifting
// from what's actually shipped.
package docs

import "embed"

//go:embed spec.md providers.md
var FS embed.FS
