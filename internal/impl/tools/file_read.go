package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
)

// FileReadConfig configures NewFileReadTool.
type FileReadConfig struct {
	// Root confines reads to this directory tree (docs/PLAN.md Phase 7
	// security note — see resolveSafePath for exactly what is and isn't
	// enforced). Empty means no confinement.
	Root string

	// MaxBytes caps how much of a file is returned. Zero means no cap.
	MaxBytes int64
}

// FileReadInput is the model-facing input for the file-read tool.
type FileReadInput struct {
	Path string `json:"path" jsonschema:"Path of the file to read, relative to the configured working root if one is set."`
}

// FileReadOutput is the model-facing output for the file-read tool.
type FileReadOutput struct {
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

// NewFileReadTool builds Canopy's file-read tool (docs/DESIGN.md §3.2 row 2).
// Not approval-gated — reading is non-mutating.
func NewFileReadTool(cfg FileReadConfig) tool.FuncTool {
	return functool.MustNew(functool.Config{
		Name:        "file_read",
		Description: "Read the contents of a file at the given path.",
	}, func(ctx context.Context, in FileReadInput) (FileReadOutput, error) {
		path, err := resolveSafePath(cfg.Root, in.Path)
		if err != nil {
			return FileReadOutput{}, fmt.Errorf("failed to read file: %w", err)
		}

		info, err := os.Stat(path)
		if err != nil {
			return FileReadOutput{}, fmt.Errorf("failed to read file: %w", err)
		}
		if info.IsDir() {
			return FileReadOutput{}, fmt.Errorf("failed to read file: %q is a directory", in.Path)
		}

		f, err := os.Open(path)
		if err != nil {
			return FileReadOutput{}, fmt.Errorf("failed to read file: %w", err)
		}
		defer f.Close()

		if cfg.MaxBytes <= 0 {
			data, err := os.ReadFile(path)
			if err != nil {
				return FileReadOutput{}, fmt.Errorf("failed to read file: %w", err)
			}
			return FileReadOutput{Content: string(data)}, nil
		}

		buf := make([]byte, cfg.MaxBytes)
		n, err := f.Read(buf)
		if err != nil && !errors.Is(err, io.EOF) {
			return FileReadOutput{}, fmt.Errorf("failed to read file: %w", err)
		}
		truncated := info.Size() > int64(n)
		return FileReadOutput{Content: string(buf[:n]), Truncated: truncated}, nil
	})
}
