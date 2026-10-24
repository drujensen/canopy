package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
	"github.com/pmezard/go-difflib/difflib"
)

// FileWriteConfig configures NewFileWriteTool.
type FileWriteConfig struct {
	// Root confines writes to this directory tree (see resolveSafePath).
	// Empty means no confinement.
	Root string
}

// FileWriteInput is the model-facing input for the file-write tool.
type FileWriteInput struct {
	Path    string `json:"path" jsonschema:"Path of the file to write, relative to the configured working root if one is set."`
	Content string `json:"content" jsonschema:"Full content to write. Overwrites any existing file at path."`
}

// FileWriteOutput is the model-facing output for the file-write tool.
type FileWriteOutput struct {
	BytesWritten int    `json:"bytes_written"`
	Path         string `json:"path"`

	// LinesAdded/LinesRemoved summarize how this write changed the file
	// relative to whatever was there before it (every line counts as added,
	// none removed, for a brand-new file — there's nothing to diff against) —
	// a git-diff-style line stat from a real line-level diff (go-difflib's
	// SequenceMatcher), not a naive (new line count - old line count)
	// delta, which would misreport a line that merely moved as some mix of
	// an add and a remove. internal/tui renders these alongside the path
	// so a run of file_write calls is as legible as Claude Code's own
	// "+N -M" Write/Edit summaries, instead of an opaque "wrote a file."
	LinesAdded   int `json:"lines_added"`
	LinesRemoved int `json:"lines_removed"`
}

// diffLineCounts returns how many lines were added/removed going from
// oldContent to newContent.
func diffLineCounts(oldContent, newContent string) (added, removed int) {
	if oldContent == newContent {
		return 0, 0
	}
	matcher := difflib.NewMatcher(difflib.SplitLines(oldContent), difflib.SplitLines(newContent))
	for _, op := range matcher.GetOpCodes() {
		switch op.Tag {
		case 'i':
			added += op.J2 - op.J1
		case 'd':
			removed += op.I2 - op.I1
		case 'r':
			removed += op.I2 - op.I1
			added += op.J2 - op.J1
		}
	}
	return added, removed
}

// NewFileWriteTool builds Canopy's file-write tool (docs/DESIGN.md §3.2 row
// 3). Creates missing parent directories so the model doesn't need a
// separate "make directory" tool call, mirroring Claude Code's Write tool
// behavior. Not approval-gated — approval-gating was removed (it only ever
// auto-approved silently, adding an extra round trip and the risk of
// malformed-history bugs with no actual safety benefit).
func NewFileWriteTool(cfg FileWriteConfig) tool.FuncTool {
	base := functool.MustNew(functool.Config{
		Name:        "file_write",
		Description: "Write (creating or overwriting) a file's full contents at the given path.",
	}, func(ctx context.Context, in FileWriteInput) (FileWriteOutput, error) {
		path, err := resolveSafePath(cfg.Root, in.Path)
		if err != nil {
			return FileWriteOutput{}, fmt.Errorf("failed to write file: %w", err)
		}

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return FileWriteOutput{}, fmt.Errorf("failed to write file: %w", err)
		}

		// Read whatever's there now, before it's overwritten, purely to
		// compute the added/removed line stat below — a missing file
		// (brand-new write) is not an error here, it just means "no prior
		// content to diff against."
		oldContent, _ := os.ReadFile(path)

		if err := os.WriteFile(path, []byte(in.Content), 0o644); err != nil {
			return FileWriteOutput{}, fmt.Errorf("failed to write file: %w", err)
		}

		added, removed := diffLineCounts(string(oldContent), in.Content)
		return FileWriteOutput{BytesWritten: len(in.Content), Path: in.Path, LinesAdded: added, LinesRemoved: removed}, nil
	})
	return base
}
