package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Skill is one discovered Agent Skill (§7.1; format per
// https://agentskills.io/specification): a skills/<dir>/SKILL.md with YAML
// frontmatter. Body is loaded lazily — the catalog surfaces only
// name+description (progressive disclosure).
type Skill struct {
	Name          string
	Description   string
	License       string
	Compatibility string
	Metadata      map[string]string
	AllowedTools  string

	// Dir is the skill directory (inside the plugin root); Body is the
	// markdown after frontmatter.
	Dir  string
	Body string

	Plugin string // owning plugin name (set by loadSkills)
}

// loadSkills discovers skills/ (§7.1): each immediate child directory with a
// regular SKILL.md file is one skill — no recursion. A skill that fails
// containment (§4.1 boundary 3) or frontmatter validation is skipped with a
// warning; siblings and other components load normally (§11.3).
func (p *Plugin) loadSkills() {
	skillsDir, err := resolveWithin(p.Root, "./skills")
	if err != nil {
		p.warnf("skills/: %v", err)
		return
	}
	fi, err := os.Stat(skillsDir)
	if err != nil {
		return // missing location is fine (§6.2)
	}
	if !fi.IsDir() {
		p.warnf("skills: not a directory — skills disabled") // §6.2
		return
	}
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		p.warnf("skills: %v", err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillDir := filepath.Join(skillsDir, e.Name())
		skillMD := filepath.Join(skillDir, "SKILL.md")
		// Containment: the discovered SKILL.md must resolve within the root
		// (§4.1 boundary 3), symlinks may point inside the root only.
		if _, err := resolveWithin(p.Root, "./skills/"+e.Name()+"/SKILL.md"); err != nil {
			p.warnf("skill %q: %v — skipped", e.Name(), err)
			continue
		}
		fi, err := os.Stat(skillMD)
		if err != nil || !fi.Mode().IsRegular() {
			continue // a dir without SKILL.md is not a skill (§7.1)
		}
		raw, err := os.ReadFile(skillMD)
		if err != nil {
			p.warnf("skill %q: %v — skipped", e.Name(), err)
			continue
		}
		sk, err := ParseSkill(raw)
		if err != nil {
			p.warnf("skill %q: %v — skipped", e.Name(), err) // §7.1 SHOULD report
			continue
		}
		sk.Dir = skillDir
		sk.Plugin = p.Manifest.Name
		p.Skills = append(p.Skills, sk)
	}
}

// skillFrontmatter is the declared Agent Skills frontmatter shape.
type skillFrontmatter struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license"`
	Compatibility string            `yaml:"compatibility"`
	Metadata      map[string]string `yaml:"metadata"`
	AllowedTools  string            `yaml:"allowed-tools"`
}

// ParseSkill validates one SKILL.md payload per the Agent Skills spec: YAML
// frontmatter (name and description required and constrained) followed by an
// unrestricted markdown body. Unknown frontmatter keys are tolerated (the
// skill format is not a closed schema); defined keys are type-checked.
func ParseSkill(raw []byte) (Skill, error) {
	var sk Skill
	text := string(raw)
	fmRaw, body, ok := splitFrontmatter(text)
	if !ok {
		return sk, fmt.Errorf("missing YAML frontmatter")
	}
	var fm skillFrontmatter
	if err := yaml.Unmarshal([]byte(fmRaw), &fm); err != nil {
		return sk, fmt.Errorf("frontmatter: %w", err)
	}
	// name: ≤64 chars, lowercase letters/numbers/hyphens, no leading or
	// trailing hyphen, no consecutive hyphens.
	if fm.Name == "" {
		return sk, fmt.Errorf("frontmatter: required field \"name\" missing")
	}
	if len(fm.Name) > 64 {
		return sk, fmt.Errorf("frontmatter: name must be at most 64 characters")
	}
	for i := 0; i < len(fm.Name); i++ {
		c := fm.Name[i]
		if !('a' <= c && c <= 'z') && !('0' <= c && c <= '9') && c != '-' {
			return sk, fmt.Errorf("frontmatter: name %q may contain only lowercase letters, numbers and hyphens", fm.Name)
		}
	}
	if fm.Name[0] == '-' || fm.Name[len(fm.Name)-1] == '-' {
		return sk, fmt.Errorf("frontmatter: name must not start or end with a hyphen")
	}
	if strings.Contains(fm.Name, "--") {
		return sk, fmt.Errorf("frontmatter: name must not contain consecutive hyphens")
	}
	if strings.TrimSpace(fm.Description) == "" {
		return sk, fmt.Errorf("frontmatter: required field \"description\" missing or empty")
	}
	if len(fm.Description) > 1024 {
		return sk, fmt.Errorf("frontmatter: description must be at most 1024 characters")
	}
	if len(fm.Compatibility) > 500 {
		return sk, fmt.Errorf("frontmatter: compatibility must be at most 500 characters")
	}
	sk.Name = fm.Name
	sk.Description = fm.Description
	sk.License = fm.License
	sk.Compatibility = fm.Compatibility
	sk.Metadata = fm.Metadata
	sk.AllowedTools = fm.AllowedTools
	sk.Body = body
	return sk, nil
}

// splitFrontmatter separates a "---\n…\n---\n" block from the body.
func splitFrontmatter(text string) (frontmatter, body string, ok bool) {
	trimmed := strings.TrimPrefix(text, "\ufeff")
	if !strings.HasPrefix(trimmed, "---\n") && trimmed != "---" && !strings.HasPrefix(trimmed, "---\r\n") {
		return "", "", false
	}
	rest := trimmed[3:]
	if strings.HasPrefix(rest, "\r\n") {
		rest = rest[2:]
	} else {
		rest = strings.TrimPrefix(rest, "\n")
	}
	// Find the closing delimiter line.
	idx := -1
	for offset := 0; offset <= len(rest); {
		rel := strings.Index(rest[offset:], "\n---")
		if rel < 0 {
			break
		}
		at := offset + rel
		after := rest[at+1:] // starts with ---
		if after == "---" || strings.HasPrefix(after, "---\n") || strings.HasPrefix(after, "---\r\n") {
			idx = at
			break
		}
		offset = at + 1
	}
	if idx < 0 {
		return "", "", false
	}
	front := rest[:idx]
	body = rest[idx+4:]
	body = strings.TrimPrefix(body, "\r\n")
	body = strings.TrimPrefix(body, "\n")
	return front, body, true
}
