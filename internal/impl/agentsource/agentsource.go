// Package agentsource loads Claude Code-compatible subagent definitions from
// .claude/agents/**/*.md (project root, recursive), ~/.claude/agents/**/*.md
// (personal, recursive), and ~/.canopy/agents/**/*.md (Canopy-generated
// defaults, recursive). See docs/REQUIREMENTS.md FR17 and docs/DESIGN.md
// §3.11.
//
// Addendum (post-v0.1.0): the third source, ~/.canopy/agents, exists so a
// brand-new Canopy install has zero-config first-run behavior instead of a
// hard error when no agent definitions exist anywhere — see WriteDefault and
// cmd/canopy's run().
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

// Load scans three sources, in ascending precedence order:
//
//  1. homeDir/.canopy/agents/**/*.md (recursive) — Canopy-generated defaults
//     (see WriteDefault).
//  2. homeDir/.claude/agents/**/*.md (recursive) — personal, the user's real
//     Claude Code config.
//  3. projectRoot/.claude/agents/**/*.md (recursive) — project-level.
//
// Each file's frontmatter and body are parsed into an AgentDefinition; the
// result is a map of agent name -> AgentDefinition. On a name conflict, the
// higher-precedence source wins: project beats personal, and personal beats a
// Canopy-generated default — so a user who defines their own agent of the
// same name in either real Claude Code location automatically overrides the
// generated fallback. Missing directories are not an error and simply
// contribute no definitions. A malformed file produces a clear, specific
// error identifying the file and what's wrong (fail loud, per
// docs/DESIGN.md §8) rather than being silently skipped.
func Load(projectRoot, homeDir string) (map[string]AgentDefinition, error) {
	canopyDefaults, err := loadDir(filepath.Join(homeDir, ".canopy", "agents"))
	if err != nil {
		return nil, err
	}

	personal, err := loadDir(filepath.Join(homeDir, ".claude", "agents"))
	if err != nil {
		return nil, err
	}

	project, err := loadDir(filepath.Join(projectRoot, ".claude", "agents"))
	if err != nil {
		return nil, err
	}

	result := make(map[string]AgentDefinition, len(canopyDefaults)+len(personal)+len(project))
	for name, def := range canopyDefaults {
		result[name] = def
	}
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

// defaultAgentFilename is the file WriteDefault creates.
const defaultAgentFilename = "general.md"

// defaultAgentContent is the Canopy-generated fallback agent's full markdown
// source: no "tools" field (omitted = inherit every available tool, matching
// Claude Code's own default agent having no tool restrictions) and no
// "model" field (falls back to AgentServiceConfig.DefaultModel).
const defaultAgentContent = `---
name: general
description: A general-purpose coding and terminal assistant for everyday tasks — reading and editing files, running commands, searching the web — when no more specialized agent applies.
---

You are a general-purpose AI assistant for software engineering and terminal work, running directly in the user's own project directory. You have direct access to file, shell, and web tools — use them to actually read, edit, run, and search things rather than just describing what you would do.

Be concise and direct. Favor taking action over narrating a plan when the request is clear. Confirm with the user before destructive or hard-to-reverse actions (deleting files, force-pushing, overwriting uncommitted work, and the like) when it isn't already clear that's what they want.
`

// WriteDefault writes a single default "general" agent definition file into
// dir (the caller passes homeDir/.canopy/agents), creating dir if it doesn't
// exist. It exists so a brand-new Canopy install has an agent to pick in the
// TUI's picker without requiring the user to hand-write a .claude/agents/*.md
// file first — see cmd/canopy's run() and this package's doc comment
// addendum.
//
// Idempotent: if dir/general.md already exists — from a prior run, possibly
// hand-edited by the user since — WriteDefault leaves it untouched and
// returns nil rather than overwriting it.
func WriteDefault(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create default agent directory %s: %w", dir, err)
	}

	path := filepath.Join(dir, defaultAgentFilename)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to stat default agent file %s: %w", path, err)
	}

	if err := os.WriteFile(path, []byte(defaultAgentContent), 0o644); err != nil {
		return fmt.Errorf("failed to write default agent file %s: %w", path, err)
	}
	return nil
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
