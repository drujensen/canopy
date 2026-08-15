package tools

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
)

// FileSearchConfig configures NewFileSearchTool.
type FileSearchConfig struct {
	// Root confines the search to this directory tree (see resolveSafePath).
	// Empty means no confinement.
	Root string

	// DefaultMaxResults caps the number of matches returned when the caller
	// doesn't specify one. Zero means 200.
	DefaultMaxResults int
}

// FileSearchInput is the model-facing input for the file-search (grep) tool.
type FileSearchInput struct {
	Pattern    string `json:"pattern" jsonschema:"Regular expression (RE2 syntax) to search for in file contents."`
	Path       string `json:"path,omitempty" jsonschema:"Directory to search, relative to the configured working root if one is set. Defaults to the root itself."`
	Glob       string `json:"glob,omitempty" jsonschema:"Optional filename glob (e.g. *.go) restricting which files are searched."`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"Maximum number of matches to return."`
}

// FileSearchMatch is a single matching line.
type FileSearchMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// FileSearchOutput is the model-facing output for the file-search tool.
type FileSearchOutput struct {
	Matches   []FileSearchMatch `json:"matches"`
	Truncated bool              `json:"truncated"`
}

const defaultSearchMaxResults = 200

// NewFileSearchTool builds Canopy's file-search/grep tool (docs/DESIGN.md
// §3.2 row 4). Not approval-gated — searching is non-mutating. Walks the
// directory tree under Path (or Root, if Path is empty), skips .git
// directories and files that look binary (a NUL byte in the first 8KiB), and
// returns matching lines with their 1-based line number.
func NewFileSearchTool(cfg FileSearchConfig) tool.FuncTool {
	maxDefault := cfg.DefaultMaxResults
	if maxDefault <= 0 {
		maxDefault = defaultSearchMaxResults
	}

	return functool.MustNew(functool.Config{
		Name:        "file_search",
		Description: "Search file contents for a regular expression within a directory tree (like grep -r).",
	}, func(ctx context.Context, in FileSearchInput) (FileSearchOutput, error) {
		re, err := regexp.Compile(in.Pattern)
		if err != nil {
			return FileSearchOutput{}, fmt.Errorf("invalid search pattern: %w", err)
		}

		searchPath := in.Path
		if searchPath == "" {
			searchPath = "."
		}
		root, err := resolveSafePath(cfg.Root, searchPath)
		if err != nil {
			return FileSearchOutput{}, fmt.Errorf("failed to search files: %w", err)
		}

		maxResults := in.MaxResults
		if maxResults <= 0 {
			maxResults = maxDefault
		}

		var out FileSearchOutput
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if len(out.Matches) >= maxResults {
				return fs.SkipAll
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return fs.SkipDir
				}
				return nil
			}
			if in.Glob != "" {
				if ok, _ := filepath.Match(in.Glob, d.Name()); !ok {
					return nil
				}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			return searchFile(path, re, maxResults, &out)
		})
		if walkErr != nil {
			return FileSearchOutput{}, fmt.Errorf("failed to search files: %w", walkErr)
		}

		return out, nil
	})
}

func searchFile(path string, re *regexp.Regexp, maxResults int, out *FileSearchOutput) error {
	f, err := os.Open(path)
	if err != nil {
		// A file that vanished or became unreadable mid-walk shouldn't abort
		// the whole search.
		return nil
	}
	defer f.Close()

	if looksBinary(f) {
		return nil
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if re.MatchString(text) {
			out.Matches = append(out.Matches, FileSearchMatch{Path: path, Line: line, Text: text})
			if len(out.Matches) >= maxResults {
				out.Truncated = true
				return nil
			}
		}
	}
	return nil
}

// looksBinary sniffs the first 8KiB of f for a NUL byte and rewinds f
// afterward. Best-effort heuristic, matching common grep implementations.
func looksBinary(f *os.File) bool {
	buf := make([]byte, 8192)
	n, _ := f.Read(buf)
	_, _ = f.Seek(0, 0)
	return bytes.IndexByte(buf[:n], 0) != -1
}
