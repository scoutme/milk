package local

import (
	"fmt"
	"strings"
	"sync"
)

// SkillEntry is one Agent Skill surfaced to the model. Skills follow
// progressive disclosure (Agent Skills spec): the catalog carries name +
// description only; the body (SKILL.md instructions) is loaded on demand
// through the skill tool. Load re-reads the file so an edited skill is
// picked up without a restart.
type SkillEntry struct {
	Name        string
	Description string
	Load        func() (string, error)
}

var (
	skillCatalogMu sync.RWMutex
	skillCatalog   []SkillEntry
)

// SetSkillCatalog replaces the installed skill catalog. The host calls this
// at startup after plugin discovery; nil/empty clears it and the skill tool
// is then omitted from the tool list entirely (no token cost for users
// without skills).
func SetSkillCatalog(entries []SkillEntry) {
	skillCatalogMu.Lock()
	defer skillCatalogMu.Unlock()
	skillCatalog = entries
}

func skillCatalogEntries() []SkillEntry {
	skillCatalogMu.RLock()
	defer skillCatalogMu.RUnlock()
	out := make([]SkillEntry, len(skillCatalog))
	copy(out, skillCatalog)
	return out
}

// SkillCatalog is the exported read-only view of the installed catalog
// (used by the host for /plugins display and by tests).
func SkillCatalog() []SkillEntry { return skillCatalogEntries() }

// skillBodyMax bounds the instruction body returned by the skill tool. The
// Agent Skills spec recommends <5000 tokens of instructions and moving
// detail to references; the cap keeps a pathological SKILL.md from flooding
// the context.
const skillBodyMax = 64 << 10

// skillToolSchema builds the skill tool's schema with the catalog's
// name+description lines inlined (computed fresh per schemas() call, like
// workflowToolDescription — the catalog can change between runs). Returns
// nil when no skills are installed.
func skillToolSchema() map[string]any {
	entries := skillCatalogEntries()
	if len(entries) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("Load a skill by name and receive its instructions. Skills (Agent Skills) are procedures installed via plugins — call one when a task matches, then follow the instructions it returns. Available:\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "- %s: %s\n", e.Name, e.Description)
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "skill",
			"description": b.String(),
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string", "description": "Name of the skill to load (see the available list)"},
				},
				"required": []string{"name"},
			},
		},
	}
}

// runSkill dispatches the skill tool: loads the named skill's body.
func runSkill(args map[string]any) (string, bool) {
	name := strArg(args, "name")
	entries := skillCatalogEntries()
	if name == "" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name)
		}
		return toolResult{Error: "skill: name is required — available: " + strings.Join(names, ", ")}.String(), false
	}
	for _, e := range entries {
		if e.Name != name {
			continue
		}
		body, err := e.Load()
		if err != nil {
			return toolResult{Error: fmt.Sprintf("skill %q: %v", name, err)}.String(), false
		}
		if len(body) > skillBodyMax {
			body = body[:skillBodyMax] + "\n…(skill body truncated)"
		}
		return fmt.Sprintf("[skill: %s]\n%s", name, body), false
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return toolResult{Error: fmt.Sprintf("unknown skill %q — available: %s", name, strings.Join(names, ", "))}.String(), false
}
