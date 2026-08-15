// Package mcpsource loads Claude Code-compatible MCP server configuration from
// .mcp.json at a project root. See docs/REQUIREMENTS.md FR18 and
// docs/DESIGN.md §3.11. This package only parses the config file; it does not
// construct MCP clients (that wiring happens in a later phase).
package mcpsource

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Kind identifies which shape an MCPServerConfig entry took in .mcp.json.
type Kind string

const (
	// KindStdio is a locally-spawned MCP server: {"command", "args", "env"}.
	KindStdio Kind = "stdio"
	// KindHTTP is a remote MCP server reached over HTTP: {"type": "http", "url"}.
	KindHTTP Kind = "http"
	// KindSSE is a remote MCP server reached over SSE: {"type": "sse", "url"}.
	KindSSE Kind = "sse"
)

// MCPServerConfig captures one entry from .mcp.json's mcpServers object, in
// either its stdio shape (Command/Args/Env) or its remote shape (Type/URL).
type MCPServerConfig struct {
	Name string
	Kind Kind

	// Stdio fields.
	Command string
	Args    []string
	Env     map[string]string

	// Remote fields.
	URL string
}

// rawServerConfig mirrors the on-disk JSON shape of a single mcpServers entry.
type rawServerConfig struct {
	// stdio
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`

	// remote
	Type string `json:"type"`
	URL  string `json:"url"`
}

// rawMCPFile mirrors the top-level .mcp.json shape.
type rawMCPFile struct {
	MCPServers map[string]rawServerConfig `json:"mcpServers"`
}

// Load reads projectRoot/.mcp.json and returns a map of server name ->
// MCPServerConfig. If the file does not exist, Load returns an empty map and
// no error, since not every project configures MCP servers. A malformed file
// (invalid JSON, or an entry that is neither a valid stdio nor remote shape)
// produces a clear, specific error identifying the file and what's wrong.
func Load(projectRoot string) (map[string]MCPServerConfig, error) {
	path := filepath.Join(projectRoot, ".mcp.json")

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]MCPServerConfig{}, nil
		}
		return nil, fmt.Errorf("failed to read MCP config %s: %w", path, err)
	}

	var raw rawMCPFile
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("MCP config %s: invalid JSON: %w", path, err)
	}

	result := make(map[string]MCPServerConfig, len(raw.MCPServers))
	for name, entry := range raw.MCPServers {
		cfg, err := toServerConfig(name, entry)
		if err != nil {
			return nil, fmt.Errorf("MCP config %s: server %q: %w", path, name, err)
		}
		result[name] = cfg
	}

	return result, nil
}

func toServerConfig(name string, entry rawServerConfig) (MCPServerConfig, error) {
	switch entry.Type {
	case "http":
		if entry.URL == "" {
			return MCPServerConfig{}, fmt.Errorf("type %q requires a non-empty %q", "http", "url")
		}
		return MCPServerConfig{Name: name, Kind: KindHTTP, URL: entry.URL}, nil
	case "sse":
		if entry.URL == "" {
			return MCPServerConfig{}, fmt.Errorf("type %q requires a non-empty %q", "sse", "url")
		}
		return MCPServerConfig{Name: name, Kind: KindSSE, URL: entry.URL}, nil
	case "":
		// No explicit type: infer stdio if a command is present.
		if entry.Command == "" {
			return MCPServerConfig{}, fmt.Errorf("entry has neither a %q (stdio) nor a %q (remote)", "command", "type")
		}
		return MCPServerConfig{
			Name:    name,
			Kind:    KindStdio,
			Command: entry.Command,
			Args:    entry.Args,
			Env:     entry.Env,
		}, nil
	default:
		return MCPServerConfig{}, fmt.Errorf("unrecognized %q value %q (expected %q or %q)", "type", entry.Type, "http", "sse")
	}
}
