package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
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
}

// NewFileWriteTool builds Canopy's file-write tool (docs/DESIGN.md §3.2 row
// 3). Approval-gated via tool.ApprovalRequiredFunc per docs/REQUIREMENTS.md
// FR5 — writing mutates the filesystem. Creates missing parent directories
// so the model doesn't need a separate "make directory" tool call, mirroring
// Claude Code's Write tool behavior.
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

		if err := os.WriteFile(path, []byte(in.Content), 0o644); err != nil {
			return FileWriteOutput{}, fmt.Errorf("failed to write file: %w", err)
		}

		return FileWriteOutput{BytesWritten: len(in.Content), Path: in.Path}, nil
	})
	return tool.ApprovalRequiredFunc(base)
}
