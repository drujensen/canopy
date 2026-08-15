package tools

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
)

// DirectoryListConfig configures NewDirectoryListTool.
type DirectoryListConfig struct {
	// Root confines listings to this directory tree (see resolveSafePath).
	// Empty means no confinement.
	Root string
}

// DirectoryListInput is the model-facing input for the directory-listing
// tool.
type DirectoryListInput struct {
	Path      string `json:"path,omitempty" jsonschema:"Directory to list, relative to the configured working root if one is set. Defaults to the root itself."`
	Recursive bool   `json:"recursive,omitempty" jsonschema:"List nested directories recursively instead of just the top level."`
}

// DirectoryEntry describes one file or directory.
type DirectoryEntry struct {
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// DirectoryListOutput is the model-facing output for the directory-listing
// tool.
type DirectoryListOutput struct {
	Entries []DirectoryEntry `json:"entries"`
}

// NewDirectoryListTool builds Canopy's directory-listing tool
// (docs/DESIGN.md §3.2 row 5). Not approval-gated — listing is non-mutating.
func NewDirectoryListTool(cfg DirectoryListConfig) tool.FuncTool {
	return functool.MustNew(functool.Config{
		Name:        "directory_list",
		Description: "List the files and subdirectories of a directory.",
	}, func(ctx context.Context, in DirectoryListInput) (DirectoryListOutput, error) {
		listPath := in.Path
		if listPath == "" {
			listPath = "."
		}
		dir, err := resolveSafePath(cfg.Root, listPath)
		if err != nil {
			return DirectoryListOutput{}, fmt.Errorf("failed to list directory: %w", err)
		}

		info, err := os.Stat(dir)
		if err != nil {
			return DirectoryListOutput{}, fmt.Errorf("failed to list directory: %w", err)
		}
		if !info.IsDir() {
			return DirectoryListOutput{}, fmt.Errorf("failed to list directory: %q is not a directory", in.Path)
		}

		var out DirectoryListOutput
		if !in.Recursive {
			entries, err := os.ReadDir(dir)
			if err != nil {
				return DirectoryListOutput{}, fmt.Errorf("failed to list directory: %w", err)
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			for _, e := range entries {
				fi, err := e.Info()
				var size int64
				if err == nil {
					size = fi.Size()
				}
				out.Entries = append(out.Entries, DirectoryEntry{
					Path:  filepath.Join(listPath, e.Name()),
					IsDir: e.IsDir(),
					Size:  size,
				})
			}
			return out, nil
		}

		walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == dir {
				return nil
			}
			if d.Name() == ".git" && d.IsDir() {
				return fs.SkipDir
			}
			rel, relErr := filepath.Rel(dir, path)
			if relErr != nil {
				rel = path
			}
			var size int64
			if fi, err := d.Info(); err == nil {
				size = fi.Size()
			}
			out.Entries = append(out.Entries, DirectoryEntry{
				Path:  filepath.Join(listPath, rel),
				IsDir: d.IsDir(),
				Size:  size,
			})
			return nil
		})
		if walkErr != nil {
			return DirectoryListOutput{}, fmt.Errorf("failed to list directory: %w", walkErr)
		}
		return out, nil
	})
}
