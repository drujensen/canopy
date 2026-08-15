// Package skillsource loads Claude Code-compatible Agent Skills from
// .claude/skills/*/SKILL.md (project root) and ~/.claude/skills/*/SKILL.md
// (personal). See docs/REQUIREMENTS.md FR19 and docs/DESIGN.md §3.11.
package skillsource

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillDefinition is a parsed Claude Code Agent Skill: YAML frontmatter plus a
// markdown body, and the skill's directory so supporting files referenced from
// the body can be resolved later.
type SkillDefinition struct {
	Name        string
	Description string
	Body        string
	Dir         string
}

// frontmatter mirrors the YAML fields a SKILL.md file's frontmatter supports.
type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// Load scans projectRoot/.claude/skills/*/SKILL.md and
// homeDir/.claude/skills/*/SKILL.md, parses each file's frontmatter and body,
// and returns a map of skill name -> SkillDefinition. Project-level
// definitions win name conflicts against personal ones. Missing directories
// are not an error and simply contribute no definitions. A malformed file
// produces a clear, specific error identifying the file and what's wrong
// (fail loud, per docs/DESIGN.md §8) rather than being silently skipped.
func Load(projectRoot, homeDir string) (map[string]SkillDefinition, error) {
	personal, err := loadDir(filepath.Join(homeDir, ".claude", "skills"))
	if err != nil {
		return nil, err
	}

	project, err := loadDir(filepath.Join(projectRoot, ".claude", "skills"))
	if err != nil {
		return nil, err
	}

	result := make(map[string]SkillDefinition, len(personal)+len(project))
	for name, def := range personal {
		result[name] = def
	}
	for name, def := range project {
		result[name] = def
	}
	return result, nil
}

// loadDir scans dir for */SKILL.md files (one level of subdirectories, per the
// Agent Skills convention) and parses each into a SkillDefinition, keyed by
// name. A missing dir returns an empty map, not an error.
func loadDir(dir string) (map[string]SkillDefinition, error) {
	result := make(map[string]SkillDefinition)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, fmt.Errorf("failed to read skills directory %s: %w", dir, err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		skillDir := filepath.Join(dir, name)
		skillFile := filepath.Join(skillDir, "SKILL.md")

		if _, err := os.Stat(skillFile); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("failed to stat skill file %s: %w", skillFile, err)
		}

		def, err := parseSkillFile(skillFile, skillDir)
		if err != nil {
			return nil, err
		}
		result[def.Name] = def
	}

	return result, nil
}

func parseSkillFile(path, dir string) (SkillDefinition, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return SkillDefinition{}, fmt.Errorf("failed to read skill file %s: %w", path, err)
	}

	fm, body, err := splitFrontmatter(string(raw))
	if err != nil {
		return SkillDefinition{}, fmt.Errorf("skill file %s: %w", path, err)
	}

	var parsed frontmatter
	if err := yaml.Unmarshal([]byte(fm), &parsed); err != nil {
		return SkillDefinition{}, fmt.Errorf("skill file %s: invalid YAML frontmatter: %w", path, err)
	}

	if strings.TrimSpace(parsed.Name) == "" {
		return SkillDefinition{}, fmt.Errorf("skill file %s: frontmatter missing required field %q", path, "name")
	}
	if strings.TrimSpace(parsed.Description) == "" {
		return SkillDefinition{}, fmt.Errorf("skill file %s: frontmatter missing required field %q", path, "description")
	}

	return SkillDefinition{
		Name:        strings.TrimSpace(parsed.Name),
		Description: strings.TrimSpace(parsed.Description),
		Body:        strings.TrimSpace(body),
		Dir:         dir,
	}, nil
}

// splitFrontmatter separates a file's leading "---\n...\n---" YAML block from
// its markdown body. It returns an error if the file has no valid frontmatter
// fences.
func splitFrontmatter(content string) (frontmatterYAML, body string, err error) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	trimmed := strings.TrimLeft(normalized, "\n")

	if !strings.HasPrefix(trimmed, "---\n") && trimmed != "---" {
		return "", "", fmt.Errorf("missing YAML frontmatter: file must start with a %q fence", "---")
	}

	rest := strings.TrimPrefix(trimmed, "---\n")
	rest = strings.TrimPrefix(rest, "---")

	idx := strings.Index(rest, "\n---")
	if idx == -1 {
		return "", "", fmt.Errorf("missing closing %q fence for YAML frontmatter", "---")
	}

	frontmatterYAML = rest[:idx]

	afterClose := rest[idx+len("\n---"):]
	if nl := strings.Index(afterClose, "\n"); nl != -1 {
		body = afterClose[nl+1:]
	} else {
		body = ""
	}

	return frontmatterYAML, body, nil
}
