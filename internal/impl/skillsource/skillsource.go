// Package skillsource loads Claude Code-compatible Agent Skills from
// .claude/skills/*/SKILL.md (project root), ~/.claude/skills/*/SKILL.md
// (personal), and ~/.canopy/skills/*/SKILL.md (Canopy-generated defaults).
// See docs/REQUIREMENTS.md FR19 and docs/DESIGN.md §3.11.
//
// Addendum (post-v0.1.0): the third source, ~/.canopy/skills, mirrors
// agentsource's own ~/.canopy/agents tier — see WriteDefaults and
// cmd/canopy's run().
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

// Load scans three sources, in ascending precedence order:
//
//  1. homeDir/.canopy/skills/*/SKILL.md — Canopy-generated defaults (see
//     WriteDefaults).
//  2. homeDir/.claude/skills/*/SKILL.md — personal, the user's real Claude
//     Code config.
//  3. projectRoot/.claude/skills/*/SKILL.md — project-level.
//
// Each file's frontmatter and body are parsed into a SkillDefinition; the
// result is a map of skill name -> SkillDefinition. On a name conflict, the
// higher-precedence source wins: project beats personal, and personal beats
// a Canopy-generated default. Missing directories are not an error and
// simply contribute no definitions. A malformed file produces a clear,
// specific error identifying the file and what's wrong (fail loud, per
// docs/DESIGN.md §8) rather than being silently skipped.
func Load(projectRoot, homeDir string) (map[string]SkillDefinition, error) {
	canopyDefaults, err := loadDir(filepath.Join(homeDir, ".canopy", "skills"))
	if err != nil {
		return nil, err
	}

	personal, err := loadDir(filepath.Join(homeDir, ".claude", "skills"))
	if err != nil {
		return nil, err
	}

	project, err := loadDir(filepath.Join(projectRoot, ".claude", "skills"))
	if err != nil {
		return nil, err
	}

	result := make(map[string]SkillDefinition, len(canopyDefaults)+len(personal)+len(project))
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

// mcpServerSetupSkillContent is the Canopy-generated default skill's full
// markdown source (post-v0.1.0 addendum): finding and configuring MCP
// servers from public registries is useful to essentially any project with
// zero project-specific setup, the same reasoning that motivates
// agentsource's default agents.
const mcpServerSetupSkillContent = `---
name: mcp-server-setup
description: Finds and configures Model Context Protocol (MCP) servers for this project — searches public MCP server registries/directories, helps pick one that fits what the user needs, and writes the resulting entry into .mcp.json. Use when the user wants to add a new tool integration and doesn't already have a specific MCP server in mind.
---

You are helping the user add a new MCP (Model Context Protocol) server to this project's .mcp.json, giving every agent access to that server's tools automatically (Canopy loads .mcp.json at startup — restart after editing it for the new tools to become available).

Find candidates. Use web search/fetch against public MCP registries and directories rather than guessing from memory — the ecosystem changes fast and a specific package/command that worked last month may already be renamed or deprecated:
- The official MCP servers repository: github.com/modelcontextprotocol/servers
- Community directories: mcp.so, Smithery (smithery.ai), Glama (glama.ai/mcp/servers), PulseMCP (pulsemcp.com)
- A targeted web search for "<the user's need> MCP server" often surfaces the right one directly

Ask the user what they're trying to connect to (a specific service/API, or a category like "database access" or "browser automation") if it isn't already clear, rather than picking the first plausible-looking result.

Evaluate before recommending. A server's README/package listing should tell you: what it actually needs to run (a package to install, a Docker image, an API key/credential), whether it's actively maintained (check the last commit/release date), and whether it runs as a local stdio process or a remote HTTP/SSE endpoint — that distinction determines which .mcp.json shape it needs (see below). Prefer official/first-party servers over unofficial reimplementations when both exist for the same service.

Write .mcp.json. Canopy reads .mcp.json at the project root ({"mcpServers": {"<name>": {...}}}). Two shapes, matching what the server actually is:

- Local/stdio server: {"command": "npx", "args": ["-y", "@some/mcp-server-package"], "env": {"API_KEY": "..."}}
- Remote HTTP/SSE server: {"type": "http", "url": "https://..."} (or "type": "sse")

If the file already exists, add your new entry under the existing mcpServers object rather than overwriting the file — other servers may already be configured there. If it doesn't exist yet, create it with just your new entry.

Credentials. Never hardcode a real API key/secret directly into .mcp.json's env block if you can avoid it — check whether the server supports reading the credential from an already-set environment variable instead, and if the user needs to obtain a key from a provider's dashboard, tell them where to get it rather than asking them to paste a secret into the chat.

Confirm, then tell them to restart. Show the user the exact .mcp.json entry before writing it (FileWrite is approval-gated either way, but a one-line summary of what you're about to add helps them catch a wrong choice before approving). After writing, tell them plainly that Canopy needs a restart to connect to the new server and load its tools — this doesn't happen automatically mid-session.
`

// defaultSkillFiles lists every skill WriteDefaults writes: one directory
// name (skillsource's one-subdirectory-per-skill convention, matching the
// Agent Skills spec) mapped to its SKILL.md content.
var defaultSkillFiles = []struct {
	dirName string
	content string
}{
	{"mcp-server-setup", mcpServerSetupSkillContent},
}

// WriteDefaults writes Canopy's default skills into dir (the caller passes
// homeDir/.canopy/skills), creating dir if it doesn't exist — mirrors
// agentsource.WriteDefaults exactly, including its per-entry idempotency:
// for each entry in defaultSkillFiles, if dir/<dirName>/SKILL.md already
// exists, it's left untouched; a missing one is (re)written.
func WriteDefaults(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create default skills directory %s: %w", dir, err)
	}

	for _, f := range defaultSkillFiles {
		skillDir := filepath.Join(dir, f.dirName)
		path := filepath.Join(skillDir, "SKILL.md")
		if _, err := os.Stat(path); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("failed to stat default skill file %s: %w", path, err)
		}

		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			return fmt.Errorf("failed to create default skill directory %s: %w", skillDir, err)
		}
		if err := os.WriteFile(path, []byte(f.content), 0o644); err != nil {
			return fmt.Errorf("failed to write default skill file %s: %w", path, err)
		}
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
	if nl := strings.Index(afterClose, "\n"); nl != -1 {
		body = afterClose[nl+1:]
	} else {
		body = ""
	}

	return frontmatterYAML, body, nil
}
