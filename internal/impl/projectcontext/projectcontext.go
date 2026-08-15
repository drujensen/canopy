// Package projectcontext loads Claude Code-compatible project instructions
// from CLAUDE.md and/or AGENTS.md at a project root. See docs/REQUIREMENTS.md
// FR20 and docs/DESIGN.md §3.11.
package projectcontext

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Load reads projectRoot/CLAUDE.md and projectRoot/AGENTS.md. If both exist,
// their contents are concatenated with CLAUDE.md first, separated by a blank
// line. If only one exists, its (trimmed) content is returned. If neither
// exists, Load returns an empty string and no error.
func Load(projectRoot string) (string, error) {
	claudeMD, claudeOK, err := readIfExists(filepath.Join(projectRoot, "CLAUDE.md"))
	if err != nil {
		return "", err
	}

	agentsMD, agentsOK, err := readIfExists(filepath.Join(projectRoot, "AGENTS.md"))
	if err != nil {
		return "", err
	}

	switch {
	case claudeOK && agentsOK:
		return claudeMD + "\n\n" + agentsMD, nil
	case claudeOK:
		return claudeMD, nil
	case agentsOK:
		return agentsMD, nil
	default:
		return "", nil
	}
}

func readIfExists(path string) (content string, ok bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("failed to read project instructions %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), true, nil
}
