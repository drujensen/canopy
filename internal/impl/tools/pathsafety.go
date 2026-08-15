// Package tools implements Canopy's narrow, Claude-Code-comparable built-in
// tool set (docs/DESIGN.md §3.2): bash/shell, file read/write/search,
// directory listing, web search, and web fetch. Each tool is a
// tool.FuncTool; mutating tools (bash, file write) are approval-gated via
// tool.ApprovalRequiredFunc per docs/REQUIREMENTS.md FR5.
package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveSafePath resolves path against root and reports an error if the
// resolved location would fall outside root.
//
// Safety boundary (docs/PLAN.md Phase 7 flags this as a security concern):
// this is a best-effort baseline, not a full sandbox. It:
//   - joins relative paths onto root, and requires absolute paths to already
//     fall under root;
//   - cleans ".." segments via filepath.Clean/Rel;
//   - resolves symlinks (on the target if it exists, otherwise on the
//     nearest existing ancestor) so a symlink inside root that points
//     outside it is also rejected.
//
// It does NOT sandbox the process (no chroot/namespaces), does not stop a
// tool from following a symlink created *during* a run (TOCTOU), and does
// not restrict what a shell command executed by the bash tool can touch —
// shell commands are free-form text, so path confinement for bash is left to
// its own WorkingDirectory setting plus approval-gating, not this function.
// If root is empty, no confinement is enforced at all (the caller explicitly
// opted out of a working root); the path is still Clean-ed.
func resolveSafePath(root, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	if root == "" {
		return filepath.Clean(path), nil
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("failed to resolve root %q: %w", root, err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("failed to resolve root %q: %w", root, err)
	}

	var target string
	if filepath.IsAbs(path) {
		target = filepath.Clean(path)
	} else {
		target = filepath.Join(resolvedRoot, path)
	}

	resolvedTarget, err := resolveExistingOrNearestAncestor(target)
	if err != nil {
		return "", fmt.Errorf("failed to resolve path %q: %w", path, err)
	}

	rel, err := filepath.Rel(resolvedRoot, resolvedTarget)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes configured root %q", path, root)
	}

	return resolvedTarget, nil
}

// resolveExistingOrNearestAncestor evaluates symlinks on target. If target
// does not exist yet (common for a file about to be written), it walks up to
// the nearest existing ancestor, resolves symlinks there, and rejoins the
// non-existent suffix — so a not-yet-created file still gets its parent
// directory's symlink resolution checked.
func resolveExistingOrNearestAncestor(target string) (string, error) {
	resolved, err := filepath.EvalSymlinks(target)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}

	var suffix []string
	dir := target
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached filesystem root without finding an existing ancestor.
			return "", err
		}
		suffix = append([]string{filepath.Base(dir)}, suffix...)
		dir = parent

		resolvedDir, statErr := filepath.EvalSymlinks(dir)
		if statErr == nil {
			return filepath.Join(append([]string{resolvedDir}, suffix...)...), nil
		}
		if !os.IsNotExist(statErr) {
			return "", statErr
		}
	}
}
