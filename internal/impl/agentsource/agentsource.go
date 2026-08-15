// Package agentsource loads Claude Code-compatible subagent definitions from
// .claude/agents/**/*.md (project root, recursive) and ~/.claude/agents/**/*.md
// (personal, recursive). See docs/REQUIREMENTS.md FR17 and docs/DESIGN.md §3.11.
package agentsource

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// AgentDefinition is a parsed Claude Code subagent definition: YAML frontmatter
// plus a markdown body used verbatim as the agent's system-prompt instructions.
type AgentDefinition struct {
	Name         string
	Description  string
	Tools        []string
	Model        string
	Instructions string
	SourcePath   string
}

// frontmatter mirrors the YAML fields Claude Code's agent files support.
type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Tools       string `yaml:"tools"`
	Model       string `yaml:"model"`
}

// Load scans projectRoot/.claude/agents/**/*.md (recursive) and
// homeDir/.claude/agents/**/*.md (recursive), parses each file's frontmatter and
// body, and returns a map of agent name -> AgentDefinition. Project-level
// definitions win name conflicts against personal ones. Missing directories are
// not an error and simply contribute no definitions. A malformed file produces a
// clear, specific error identifying the file and what's wrong (fail loud, per
// docs/DESIGN.md §8) rather than being silently skipped.
func Load(projectRoot, homeDir string) (map[string]AgentDefinition, error) {
	personal, err := loadDir(filepath.Join(homeDir, ".claude", "agents"))
	if err != nil {
		return nil, err
	}

	project, err := loadDir(filepath.Join(projectRoot, ".claude", "agents"))
	if err != nil {
		return nil, err
	}

	result := make(map[string]AgentDefinition, len(personal)+len(project))
	for name, def := range personal {
		result[name] = def
	}
	for name, def := range project {
		result[name] = def
	}
	return result, nil
}

// loadDir scans dir recursively for *.md files and parses each into an
// AgentDefinition, keyed by name. A missing dir returns an empty map, not an
// error.
func loadDir(dir string) (map[string]AgentDefinition, error) {
	result := make(map[string]AgentDefinition)

	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, fmt.Errorf("failed to stat agent directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return result, nil
	}

	var paths []string
	walkErr := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(fi.Name()), ".md") {
			paths = append(paths, path)
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("failed to walk agent directory %s: %w", dir, walkErr)
	}

	// Deterministic order for reproducible errors/behavior.
	sort.Strings(paths)

	for _, path := range paths {
		def, err := parseAgentFile(path)
		if err != nil {
			return nil, err
		}
		result[def.Name] = def
	}

	return result, nil
}

func parseAgentFile(path string) (AgentDefinition, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return AgentDefinition{}, fmt.Errorf("failed to read agent file %s: %w", path, err)
	}

	fm, body, err := splitFrontmatter(string(raw))
	if err != nil {
		return AgentDefinition{}, fmt.Errorf("agent file %s: %w", path, err)
	}

	var parsed frontmatter
	if err := yaml.Unmarshal([]byte(fm), &parsed); err != nil {
		return AgentDefinition{}, fmt.Errorf("agent file %s: invalid YAML frontmatter: %w", path, err)
	}

	if strings.TrimSpace(parsed.Name) == "" {
		return AgentDefinition{}, fmt.Errorf("agent file %s: frontmatter missing required field %q", path, "name")
	}
	if strings.TrimSpace(parsed.Description) == "" {
		return AgentDefinition{}, fmt.Errorf("agent file %s: frontmatter missing required field %q", path, "description")
	}

	var tools []string
	if strings.TrimSpace(parsed.Tools) != "" {
		for _, t := range strings.Split(parsed.Tools, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tools = append(tools, t)
			}
		}
	}

	return AgentDefinition{
		Name:         strings.TrimSpace(parsed.Name),
		Description:  strings.TrimSpace(parsed.Description),
		Tools:        tools,
		Model:        strings.TrimSpace(parsed.Model),
		Instructions: strings.TrimSpace(body),
		SourcePath:   path,
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
	// Consume the rest of the closing fence's line.
	if nl := strings.Index(afterClose, "\n"); nl != -1 {
		body = afterClose[nl+1:]
	} else {
		body = ""
	}

	return frontmatterYAML, body, nil
}
