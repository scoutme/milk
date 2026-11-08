package plugin

import "strings"

// Expand performs the placeholder expansion required by §9.2: a single,
// non-recursive textual replacement of every exact occurrence of
// ${PLUGIN_ROOT} and ${PLUGIN_DATA}. It applies to stdio MCP `args` elements,
// `env` values, and `cwd` only — never to `command`, `env` keys, or fixed
// component locations. Unrecognized placeholder-like text stays literal.
//
// The scan is left to right in one pass precisely so text introduced by a
// replacement is never re-scanned (§9.2): a plugin root that itself contains
// the literal text "${PLUGIN_DATA}" must not trigger a second replacement.
func Expand(s, pluginRoot, pluginData string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "${PLUGIN_ROOT}"):
			b.WriteString(pluginRoot)
			i += len("${PLUGIN_ROOT}")
		case strings.HasPrefix(s[i:], "${PLUGIN_DATA}"):
			b.WriteString(pluginData)
			i += len("${PLUGIN_DATA}")
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}
