package plugin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// MCPFile is the mcp.json configuration (§7.2.1): a closed top-level object
// with exactly $schema and mcpServers.
type MCPFile struct {
	Schema  string
	Servers map[string]MCPServer
}

// MCPServer is one validated server entry — exactly one of the closed
// variants (§7.2.1). Stdio is set for type "stdio"; Remote is set for
// "streamable-http" and "sse".
type MCPServer struct {
	Name   string
	Stdio  *StdioServer
	Remote *RemoteServer
}

// StdioServer is the stdio variant (§7.2.1 stdio table).
type StdioServer struct {
	Command string            // single executable token: bare name or "./" path
	Args    []string          // expanded (§9.2)
	Env     map[string]string // expanded values (§9.2); keys are not
	Cwd     string            // expanded and containment-checked
}

// RemoteServer is the streamable-http / legacy sse variant (§7.2.1 remote
// table). No placeholder expansion applies to url or headers.
type RemoteServer struct {
	Transport string // "streamable-http" | "sse"
	URL       string
	Headers   map[string]string
}

// loadMCP reads and validates plugin-root mcp.json (§7.2). Whole-file
// problems disable MCP for the plugin (p.MCPDisabled + warning) and never
// affect other components; per-entry problems skip just that entry (§7.2.2
// rules 3-5) with a warning each.
func (p *Plugin) loadMCP() {
	mcpPath, err := resolveWithin(p.Root, "./mcp.json")
	if err != nil {
		p.warnf("mcp.json: %v", err)
		p.MCPDisabled = true
		return
	}
	raw, err := os.ReadFile(mcpPath)
	if err != nil {
		if os.IsNotExist(err) {
			return // missing location is fine (§6.2)
		}
		p.warnf("mcp.json: %v", err)
		p.MCPDisabled = true
		return
	}
	if fi, err := os.Stat(mcpPath); err == nil && !fi.Mode().IsRegular() {
		p.warnf("mcp.json: not a regular file — MCP disabled")
		p.MCPDisabled = true
		return
	}

	file, entryWarnings, err := ParseMCPFile(raw, p.Manifest.Schema)
	if err != nil {
		p.warnf("mcp.json: %v — MCP disabled", err)
		p.MCPDisabled = true
		return
	}
	for _, w := range entryWarnings {
		p.warnf("mcp.json: %s", w)
	}
	// Keep only entries that pass the runtime-shaped rules (§7.2.1).
	accepted := make(map[string]MCPServer, len(file.Servers))
	for _, name := range sortedServerNames(file.Servers) {
		srv := file.Servers[name]
		if err := p.validateServer(&srv); err != nil {
			p.warnf("mcp server %q: %v — skipped", name, err)
			continue
		}
		accepted[name] = srv
	}
	file.Servers = accepted
	p.MCP = &file
}

// ParseMCPFile validates the closed mcp.json structure and every entry's
// variant shape (§7.2.1). Whole-file violations are the returned error (the
// caller disables MCP per §7.2.2 rule 2); per-entry shape violations land in
// the returned warnings and drop only that entry (§7.2.2 rule 3).
// pluginSchema is the $schema declared by plugin.json — a version mismatch
// between the two documents invalidates the MCP configuration (§10.1).
func ParseMCPFile(raw []byte, pluginSchema string) (MCPFile, []string, error) {
	var f MCPFile
	top, err := decodeObject(raw, "mcp.json")
	if err != nil {
		return f, nil, err
	}
	for k := range top {
		if k != "$schema" && k != "mcpServers" {
			return f, nil, fmt.Errorf("unknown top-level field %q", k) // §7.2.1: closed top level
		}
	}
	sch, err := requireString(top, "$schema")
	if err != nil {
		return f, nil, err
	}
	if sch != SchemaMCP {
		return f, nil, fmt.Errorf("unsupported $schema %q (this client supports Agent Plugins 1.0.0)", sch)
	}
	if pluginSchema != "" && specVersion(pluginSchema) != specVersion(sch) {
		return f, nil, fmt.Errorf("$schema targets a different Agent Plugins version than plugin.json") // §10.1
	}
	f.Schema = sch

	rawServers, ok := top["mcpServers"]
	if !ok {
		return f, nil, fmt.Errorf("required field \"mcpServers\" missing")
	}
	var members map[string]json.RawMessage
	if err := decodeInto(rawServers, &members); err != nil || members == nil {
		return f, nil, fmt.Errorf("mcpServers: must be an object")
	}
	f.Servers = make(map[string]MCPServer, len(members))
	var warnings []string
	for _, name := range sortedRawNames(members) {
		srv, err := parseServer(name, members[name])
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("server %q: %v — skipped", name, err))
			continue
		}
		f.Servers[name] = srv
	}
	return f, warnings, nil
}

// parseServer enforces the closed per-entry union (§7.2.1): unknown fields,
// unknown type values and cross-variant fields make the entry invalid.
func parseServer(name string, raw []byte) (MCPServer, error) {
	srv := MCPServer{Name: name}
	obj, err := decodeObject(raw, "server "+name)
	if err != nil {
		return srv, err
	}
	typ, err := requireString(obj, "type")
	if err != nil {
		return srv, err
	}
	allowed := map[string]bool{}
	switch typ {
	case "stdio":
		for _, k := range []string{"type", "command", "args", "env", "cwd"} {
			allowed[k] = true
		}
		st := &StdioServer{}
		if st.Command, err = requireString(obj, "command"); err != nil {
			return srv, err
		}
		if rawArgs, ok := obj["args"]; ok {
			if err := decodeInto(rawArgs, &st.Args); err != nil {
				return srv, fmt.Errorf("args: must be an array of strings")
			}
		}
		if rawEnv, ok := obj["env"]; ok {
			var env map[string]string
			if err := decodeInto(rawEnv, &env); err != nil {
				return srv, fmt.Errorf("env: must be an object of strings")
			}
			for k := range env {
				if k == "PLUGIN_ROOT" || k == "PLUGIN_DATA" {
					return srv, fmt.Errorf("env: %q is a reserved variable", k) // §9.2
				}
			}
			st.Env = env
		}
		if rawCwd, ok := obj["cwd"]; ok {
			if st.Cwd, err = wantString(rawCwd, "cwd"); err != nil {
				return srv, err
			}
		}
		srv.Stdio = st
	case "streamable-http", "sse":
		for _, k := range []string{"type", "url", "headers"} {
			allowed[k] = true
		}
		rm := &RemoteServer{Transport: typ}
		if rm.URL, err = requireString(obj, "url"); err != nil {
			return srv, err
		}
		if rawHdr, ok := obj["headers"]; ok {
			var hdr map[string]string
			if err := decodeInto(rawHdr, &hdr); err != nil {
				return srv, fmt.Errorf("headers: must be an object of strings")
			}
			rm.Headers = hdr
		}
		srv.Remote = rm
	default:
		return srv, fmt.Errorf("unknown type %q", typ)
	}
	for k := range obj {
		if !allowed[k] {
			return srv, fmt.Errorf("unknown field %q for type %q", k, typ)
		}
	}
	return srv, nil
}

// validateServer applies the runtime-shaped rules (§7.2.1, §9): command token
// and containment, placeholder expansion in args/env/cwd, cwd forms, remote
// URL and header rules. On success the entry's strings carry the expanded,
// resolved values ready to hand to a launcher.
func (p *Plugin) validateServer(s *MCPServer) error {
	if s.Stdio == nil && s.Remote == nil {
		return fmt.Errorf("invalid entry")
	}
	if s.Stdio != nil {
		st := s.Stdio
		// command: one executable token (§7.2.1). Bare name → platform
		// search at launch; "./"-path → resolved against the plugin root,
		// contained (§4.1 boundary 4).
		if strings.TrimSpace(st.Command) != st.Command || strings.ContainsAny(st.Command, " \t\n\"'\\") {
			return fmt.Errorf("command must be a single executable token")
		}
		if strings.HasPrefix(st.Command, "./") {
			resolved, err := resolveWithin(p.Root, st.Command)
			if err != nil {
				return fmt.Errorf("command: %w", err)
			}
			st.Command = resolved
		} else if strings.Contains(st.Command, "/") {
			return fmt.Errorf("command %q must be a bare name or start with ./", st.Command)
		}
		// args / env values / cwd: placeholder expansion (§9.2), single pass.
		for i := range st.Args {
			st.Args[i] = Expand(st.Args[i], p.Root, p.DataDir)
		}
		for k, v := range st.Env {
			st.Env[k] = Expand(v, p.Root, p.DataDir)
		}
		if st.Cwd != "" {
			if err := p.resolveCwd(st); err != nil {
				return err
			}
		}
		return nil
	}

	rm := s.Remote
	// url: absolute http(s), no userinfo, no fragment; non-loopback must be
	// https. No expansion (§7.2.1).
	u, err := url.Parse(rm.URL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("url must be an absolute HTTP(S) URL")
	}
	if u.User != nil || u.Fragment != "" {
		return fmt.Errorf("url must not contain user information or a fragment")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url scheme must be http or https")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("non-loopback endpoints must use https")
	}
	// headers: valid fields, no duplicate case-insensitive names; no
	// expansion (§7.2.1).
	seen := map[string]bool{}
	for k, v := range rm.Headers {
		if !validHeaderName(k) || strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("invalid header %q", k)
		}
		lk := strings.ToLower(k)
		if seen[lk] {
			return fmt.Errorf("duplicate header %q (case-insensitive)", k)
		}
		seen[lk] = true
	}
	return nil
}

// resolveCwd enforces the cwd forms (§7.2.1): "./" plugin-relative,
// ${PLUGIN_ROOT}-rooted, or ${PLUGIN_DATA}-rooted — expanded first (§9.2),
// contained afterwards in the matching root (§4.1).
func (p *Plugin) resolveCwd(st *StdioServer) error {
	raw := st.Cwd
	switch {
	case strings.HasPrefix(raw, "./"):
		resolved, err := resolveWithin(p.Root, Expand(raw, p.Root, p.DataDir))
		if err != nil {
			return fmt.Errorf("cwd: %w", err)
		}
		st.Cwd = resolved
	case raw == "${PLUGIN_ROOT}" || strings.HasPrefix(raw, "${PLUGIN_ROOT}/"):
		expanded := Expand(raw, p.Root, p.DataDir)
		if !contains(p.Root, expanded) {
			return fmt.Errorf("cwd escapes the plugin root")
		}
		st.Cwd = expanded
	case raw == "${PLUGIN_DATA}" || strings.HasPrefix(raw, "${PLUGIN_DATA}/"):
		expanded := Expand(raw, p.Root, p.DataDir)
		if !contains(p.DataDir, expanded) {
			return fmt.Errorf("cwd escapes the plugin data directory")
		}
		st.Cwd = expanded
	default:
		return fmt.Errorf("cwd %q must start with ./ or be rooted at ${PLUGIN_ROOT} or ${PLUGIN_DATA}", raw)
	}
	return nil
}

// specVersion extracts the version segment of a canonical schema identifier
// (…/schemas/<version>/<file>.schema.json), e.g. "1.0.0" — the §10.1 match
// rule compares versions, not identifiers (plugin.json and mcp.json target
// different schema files of the same release).
func specVersion(schema string) string {
	parts := strings.Split(strings.TrimSuffix(schema, "/"), "/")
	for i := range parts {
		if parts[i] == "schemas" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func sortedServerNames(m map[string]MCPServer) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sortStrings(names)
	return names
}

func sortedRawNames(m map[string]json.RawMessage) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sortStrings(names)
	return names
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validHeaderName(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !strings.ContainsRune("!#$%&'*+-.^_`|~0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ", rune(c)) {
			return false
		}
	}
	return true
}
