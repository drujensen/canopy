package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
)

// FileEditConfig configures NewFileEditTool.
type FileEditConfig struct {
	// Root confines edits to this directory tree (see resolveSafePath).
	// Empty means no confinement.
	Root string
}

// FileEditInput is the model-facing input for the file-edit tool.
type FileEditInput struct {
	Path       string `json:"path" jsonschema:"Path of the file to edit, relative to the configured working root if one is set."`
	OldString  string `json:"old_string" jsonschema:"Exact text to find and replace. Must match the file's current contents verbatim, including whitespace."`
	NewString  string `json:"new_string" jsonschema:"Text to replace old_string with."`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"Replace every occurrence of old_string instead of requiring it to be unique."`
}

// FileEditOutput is the model-facing output for the file-edit tool.
type FileEditOutput struct {
	Path string `json:"path"`

	// LinesAdded/LinesRemoved mirror FileWriteOutput's line-stat fields (see
	// diffLineCounts) so the TUI renders an edit the same way it renders a
	// full-file write.
	LinesAdded   int `json:"lines_added"`
	LinesRemoved int `json:"lines_removed"`
}

// NewFileEditTool builds Canopy's targeted find-and-replace tool (docs/PLAN.md
// Phase 11) — the Claude-Code-`Edit`-equivalent counterpart to file_write's
// full-file overwrite. Not approval-gated, same posture as every other
// mutating tool (docs/DESIGN.md §3.6).
//
// Unlike file_write, this never creates a file: old_string must already be
// present, so there's always an existing file to anchor the match against.
// old_string must match exactly once unless replace_all is set — an
// ambiguous match is rejected rather than guessing which occurrence the
// caller meant, and a no-op edit (old_string == new_string) is rejected so a
// malformed call fails loudly instead of silently reporting success having
// changed nothing.
func NewFileEditTool(cfg FileEditConfig) tool.FuncTool {
	return functool.MustNew(functool.Config{
		Name:        "file_edit",
		Description: "Replace an exact, unique substring of an existing file's contents. Prefer this over file_write for any change smaller than the whole file.",
	}, func(ctx context.Context, in FileEditInput) (FileEditOutput, error) {
		if in.OldString == "" {
			return FileEditOutput{}, fmt.Errorf("failed to edit file: old_string must not be empty")
		}
		if in.OldString == in.NewString {
			return FileEditOutput{}, fmt.Errorf("failed to edit file: old_string and new_string are identical")
		}

		path, err := resolveSafePath(cfg.Root, in.Path)
		if err != nil {
			return FileEditOutput{}, fmt.Errorf("failed to edit file: %w", err)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return FileEditOutput{}, fmt.Errorf("failed to edit file: %w", err)
		}
		oldContent := string(data)

		occurrences := strings.Count(oldContent, in.OldString)
		if occurrences == 0 {
			return FileEditOutput{}, fmt.Errorf("failed to edit file: old_string not found in %q", in.Path)
		}
		if occurrences > 1 && !in.ReplaceAll {
			return FileEditOutput{}, fmt.Errorf("failed to edit file: old_string appears %d times in %q; pass replace_all or include more surrounding context to make it unique", occurrences, in.Path)
		}

		replaceCount := 1
		if in.ReplaceAll {
			replaceCount = -1
		}
		newContent := strings.Replace(oldContent, in.OldString, in.NewString, replaceCount)

		if err := os.WriteFile(path, []byte(newContent), 0o644); err != nil {
			return FileEditOutput{}, fmt.Errorf("failed to edit file: %w", err)
		}

		added, removed := diffLineCounts(oldContent, newContent)
		return FileEditOutput{Path: in.Path, LinesAdded: added, LinesRemoved: removed}, nil
	})
}
