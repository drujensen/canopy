package tui

import (
	"os"
	"path/filepath"
	"strings"
)

// displayWorkingDirectory returns dir with the user's home directory replaced
// by ~ when dir is the home directory or one of its descendants.
func displayWorkingDirectory(dir, home string) string {
	dir = filepath.Clean(dir)
	home = filepath.Clean(home)
	if home == "." || home == "" {
		return dir
	}

	rel, err := filepath.Rel(home, dir)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		if rel == "." {
			return "~"
		}
		return filepath.Join("~", rel)
	}
	return dir
}

// currentDirectoryLabel returns the process working directory in the compact
// form used by the sidebar. Falling back to "." keeps rendering best-effort if
// the working directory has been removed while Canopy is running.
func currentDirectoryLabel() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return dir
	}
	return displayWorkingDirectory(dir, home)
}
