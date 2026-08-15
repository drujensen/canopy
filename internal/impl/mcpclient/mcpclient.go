// Package mcpclient connects to the MCP servers impl/mcpsource parses out of
// .mcp.json and exposes their tools as tool.Tool instances (docs/DESIGN.md
// §3.11, Requirements FR6/FR18). mcpsource only parses the config file; this
// package does the actual client-side MCP protocol work mcpsource's own doc
// comment defers: dialing a transport (stdio subprocess, HTTP, or SSE),
// listing the server's tools via the framework's tool/mcptool, and managing
// the resulting session's lifecycle so a connected server doesn't leak a
// subprocess or HTTP connection for the life of the process.
//
// # Connection lifecycle: eager, not lazy
//
// ConnectAll is meant to be called once, at startup, before
// domain/services.AgentService is constructed (see cmd/canopy/main.go) —
// not lazily on first tool use. Two reasons:
//
//  1. AgentService's own doc comment states NewAgentService "performs no I/O
//     itself"; connecting to MCP servers is I/O (spawning processes, dialing
//     URLs), so it has to happen before construction, not inside it.
//  2. A single Registry's tools are shared by every agent AgentService
//     builds for the life of the process (top-level agents and subagents
//     alike, per Design §3.11's "added to every agent's tool list"), so one
//     eager connect-and-reuse avoids reconnecting (and re-spawning stdio
//     subprocesses) on every turn. The tradeoff is a slower process start
//     when a configured server is slow to respond, and that every agent
//     shares one live session per server rather than getting its own — an
//     acceptable tradeoff given MCP servers in practice are meant to be
//     long-lived, and mcp.ClientSession itself has no documented
//     restriction against concurrent CallTool invocations from independent
//     goroutines.
//
// Whatever calls ConnectAll owns the returned *Registry's lifetime and must
// call its Close method during shutdown so no subprocess or connection
// outlives the process unnecessarily.
package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/mcptool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/drujensen/canopy/internal/impl/mcpsource"
)

// Client is one connected MCP server: its live session (kept only for
// Close) and the tools it exposed at connect time.
type Client struct {
	// Name is the server's key from .mcp.json's mcpServers object.
	Name string
	// Tools are the tools this server exposed at connect time, each wrapped
	// with tool.ApprovalRequiredFunc — see Connect's doc comment.
	Tools []tool.Tool

	session *mcp.ClientSession
}

// Close terminates the underlying MCP session. For a stdio server this
// closes the subprocess's stdin and waits (up to
// mcp.CommandTransport's TerminateDuration) for it to exit before signaling
// it, so a broken/hung MCP server doesn't leak a subprocess indefinitely;
// for HTTP/SSE it ends the remote session. Safe to call on a Client whose
// Connect never fully established a session, and safe to call more than
// once.
func (c *Client) Close() error {
	if c == nil || c.session == nil {
		return nil
	}
	return c.session.Close()
}

// Connect dials cfg's transport — stdio spawns cfg.Command with cfg.Args and
// cfg.Env (merged on top of the current process's environment, not
// replacing it, matching Claude Code's own .mcp.json "env" semantics of
// adding/overriding specific variables rather than sandboxing the
// subprocess's environment); http/sse connect to cfg.URL — and lists the
// tools the server exposes.
//
// # Every MCP tool is wrapped as approval-required
//
// Design §3.2/Requirements FR5 approval-gate Canopy's own mutating built-ins
// (Bash, FileWrite) individually, because Canopy wrote them and knows which
// ones mutate. An MCP server's tools carry no such classification reaching
// this code — a server can name a tool "list_files" or "delete_database" and
// Canopy has no reliable way to tell which is safe from the name or JSON
// schema alone. Requirements §7 is explicit that this needs a conservative
// default: "MCP server configs are read, not auto-executed with elevated
// trust — approval-gating (FR5) applies to MCP-provided mutating tools too."
// So every MCP-provided tool.FuncTool returned here is unconditionally
// wrapped with tool.ApprovalRequiredFunc, regardless of what the server
// itself might claim about its own safety — treating "potentially mutating"
// as the default assumption rather than trying to guess per tool. See
// domain/services.AgentService's plan-mode handling for the matching
// decision to withhold MCP tools entirely while planning, for the same
// reason Bash/FileWrite are withheld outright rather than left merely
// approval-gated (Design §3.8's mechanism: approval-gating alone doesn't
// change plan mode's guarantee, since these tools are already
// approval-gated in every mode).
func Connect(ctx context.Context, cfg mcpsource.MCPServerConfig) (*Client, error) {
	transport, err := buildTransport(cfg)
	if err != nil {
		return nil, err
	}

	session, err := mcptool.Connect(ctx, transport)
	if err != nil {
		return nil, fmt.Errorf("connecting to MCP server %q: %w", cfg.Name, err)
	}

	rawTools, err := mcptool.ListTools(ctx, session)
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("listing tools from MCP server %q: %w", cfg.Name, err)
	}

	wrapped := make([]tool.Tool, 0, len(rawTools))
	for _, t := range rawTools {
		ft, ok := t.(tool.FuncTool)
		if !ok {
			// mcptool.ListTools always wraps MCP tools in a type that
			// implements tool.FuncTool (verified against the framework
			// source at tool/mcptool/mcp.go — mcpWrapper). This branch
			// should be unreachable; it fails loud rather than silently
			// dropping a tool if that ever changes upstream.
			_ = session.Close()
			return nil, fmt.Errorf("MCP server %q: tool %q does not implement tool.FuncTool", cfg.Name, t.Name())
		}
		wrapped = append(wrapped, tool.ApprovalRequiredFunc(ft))
	}

	return &Client{Name: cfg.Name, Tools: wrapped, session: session}, nil
}

// buildTransport translates cfg into the go-sdk mcp.Transport its Kind
// implies — the one piece of glue mcpsource's own Kind/Command/Args/Env/URL
// shape exists for.
func buildTransport(cfg mcpsource.MCPServerConfig) (mcp.Transport, error) {
	switch cfg.Kind {
	case mcpsource.KindStdio:
		if cfg.Command == "" {
			return nil, fmt.Errorf("MCP server %q: stdio config has no command", cfg.Name)
		}
		cmd := exec.Command(cfg.Command, cfg.Args...)
		if len(cfg.Env) > 0 {
			cmd.Env = os.Environ()
			for k, v := range cfg.Env {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
		}
		return &mcp.CommandTransport{Command: cmd}, nil
	case mcpsource.KindHTTP:
		if cfg.URL == "" {
			return nil, fmt.Errorf("MCP server %q: http config has no url", cfg.Name)
		}
		return &mcp.StreamableClientTransport{Endpoint: cfg.URL}, nil
	case mcpsource.KindSSE:
		if cfg.URL == "" {
			return nil, fmt.Errorf("MCP server %q: sse config has no url", cfg.Name)
		}
		return &mcp.SSEClientTransport{Endpoint: cfg.URL}, nil
	default:
		return nil, fmt.Errorf("MCP server %q: unrecognized kind %q", cfg.Name, cfg.Kind)
	}
}

// ConnectError pairs a configured server's name with why ConnectAll failed
// to connect to it.
type ConnectError struct {
	Server string
	Err    error
}

func (e *ConnectError) Error() string {
	return fmt.Sprintf("MCP server %q: %v", e.Server, e.Err)
}

func (e *ConnectError) Unwrap() error { return e.Err }

// Registry aggregates every successfully-connected MCP server's tools into
// one name-keyed map and owns closing every underlying session together.
type Registry struct {
	clients []*Client
	tools   map[string]tool.Tool
}

// Tools returns the combined tool set across every connected server, keyed
// by tool name. The returned map must not be mutated by callers — it is the
// Registry's own backing map, not a copy.
func (r *Registry) Tools() map[string]tool.Tool {
	if r == nil {
		return nil
	}
	return r.tools
}

// Close closes every connected server's session, collecting rather than
// stopping at the first error so one stuck server doesn't prevent cleanup
// of the others.
func (r *Registry) Close() error {
	if r == nil {
		return nil
	}
	var errs []error
	for _, c := range r.clients {
		if err := c.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing MCP server %q: %w", c.Name, err))
		}
	}
	return errors.Join(errs...)
}

// ConnectAll connects to every server in servers, in deterministic
// (name-sorted) order, and returns a Registry over whichever ones succeeded.
//
// A server that fails to connect (bad command, unreachable URL, a
// handshake/protocol failure) is skipped, not fatal to the call as a whole —
// Requirements §7 and this package's task both require that Canopy still
// starts and functions with its core tools even if one configured MCP
// server is broken. Every failure is logged via logger (when non-nil, at
// Error level so it's not mistaken for routine diagnostics) and also
// returned as a *ConnectError in errs, so a caller (cmd/canopy) can surface
// it directly to the user (e.g. a startup warning on stderr) rather than
// relying solely on the log file.
//
// Tool name collisions between two configured MCP servers are resolved
// first-connected-(i.e. alphabetically-first-name)-wins: the later server's
// colliding tool is dropped and logged as a warning, rather than silently
// overwritten or automatically namespaced. Canopy deliberately does not
// prefix MCP tool names with their server name (unlike some MCP clients),
// because an AgentDefinition.Tools allowlist entry and a user reading
// .mcp.json both refer to a tool by the plain name its server documents;
// namespacing would silently break that mapping. Collisions against
// Canopy's own core tool names (Bash, FileRead, ...) are intentionally not
// handled here — that merge is domain/services.AgentService's job (it
// already owns the core tool list this package knows nothing about).
func ConnectAll(ctx context.Context, servers map[string]mcpsource.MCPServerConfig, logger *slog.Logger) (*Registry, []error) {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	reg := &Registry{tools: make(map[string]tool.Tool)}
	var errs []error
	for _, name := range names {
		cfg := servers[name]
		client, err := Connect(ctx, cfg)
		if err != nil {
			cerr := &ConnectError{Server: name, Err: err}
			errs = append(errs, cerr)
			if logger != nil {
				logger.Error("mcp server failed to connect; continuing without its tools",
					"server", name, "error", err)
			}
			continue
		}
		reg.clients = append(reg.clients, client)
		for _, t := range client.Tools {
			if _, exists := reg.tools[t.Name()]; exists {
				if logger != nil {
					logger.Warn("mcp tool name collision; keeping the first-connected server's tool",
						"tool", t.Name(), "server", name)
				}
				continue
			}
			reg.tools[t.Name()] = t
		}
	}
	return reg, errs
}
