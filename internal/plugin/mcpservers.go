package plugin

import (
	"fmt"

	"github.com/scoutme/milk/internal/config"
)

// MilkServers maps the plugin's validated MCP entries (§7.2) onto milk's
// native MCPServerConfig values, ready to hand to the MCP runtime:
//
//   - "stdio" → Transport "stdio" with the already-expanded Command/Args,
//     Env, Cwd plus the §9.1 reserved variables (PluginRoot/PluginData, set
//     by the launcher after the env overlay).
//   - "streamable-http" → Transport "http" (milk's Streamable HTTP client).
//   - "sse" → not supported by milk's transport set (it connects Streamable
//     HTTP first); per §11.3 rule 4 the entry is skipped with a report.
//
// Server names are namespaced "plugin.<plugin>.<server>" so a plugin can
// never collide with a user-configured server name.
func (p *Plugin) MilkServers() ([]config.MCPServerConfig, []string) {
	var out []config.MCPServerConfig
	var warnings []string
	if p.MCP == nil || p.MCPDisabled {
		return out, nil
	}
	for _, name := range sortedServerNames(p.MCP.Servers) {
		srv := p.MCP.Servers[name]
		full := "plugin." + p.Manifest.Name + "." + name
		switch {
		case srv.Stdio != nil:
			st := srv.Stdio
			out = append(out, config.MCPServerConfig{
				Name:       full,
				Transport:  "stdio",
				Command:    st.Command,
				Args:       st.Args,
				Env:        st.Env,
				Cwd:        st.Cwd,
				PluginRoot: p.Root,
				PluginData: p.DataDir,
			})
		case srv.Remote != nil && srv.Remote.Transport == "streamable-http":
			rm := srv.Remote
			out = append(out, config.MCPServerConfig{
				Name:      full,
				URL:       rm.URL,
				Transport: "http", // milk connects Streamable HTTP (§7.2.1)
				Headers:   rm.Headers,
			})
		case srv.Remote != nil && srv.Remote.Transport == "sse":
			warnings = append(warnings, fmt.Sprintf("mcp server %q: legacy sse transport is not supported — skipped", name))
		default:
			warnings = append(warnings, fmt.Sprintf("mcp server %q: unknown transport — skipped", name))
		}
	}
	return out, warnings
}
