// Command canopy is Canopy's real entry point (Plan Phase 6, Requirements
// FR15): it discovers project configuration the same way Claude Code does
// (.claude/agents, .claude/skills, .mcp.json, CLAUDE.md/AGENTS.md — Design
// §3.11), loads Canopy's own provider/model config (Design §4), wires up an
// AgentService (Design §2), and launches the Bubble Tea TUI (Design §5)
// against it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"go.uber.org/zap"

	"github.com/drujensen/canopy/internal/domain/services"
	"github.com/drujensen/canopy/internal/impl/agentsource"
	"github.com/drujensen/canopy/internal/impl/config"
	"github.com/drujensen/canopy/internal/impl/logging"
	"github.com/drujensen/canopy/internal/impl/mcpclient"
	"github.com/drujensen/canopy/internal/impl/mcpsource"
	"github.com/drujensen/canopy/internal/impl/projectcontext"
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
	"github.com/drujensen/canopy/internal/impl/skillsource"
	"github.com/drujensen/canopy/internal/tui"
)

// version is Canopy's own version string (Requirements FR15's --version
// flag). Bumped at release time (Plan Phase 8) — not tied to any dependency
// version.
const version = "0.1.0-dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "canopy:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		global      bool
		storage     string
		showVersion bool
	)
	flag.BoolVar(&global, "global", false, "use ~/.canopy config/storage instead of a project-local .canopy directory")
	flag.BoolVar(&global, "g", false, "shorthand for --global")
	flag.StringVar(&storage, "storage", "file", `storage backend; "file" is the only supported value in v1 (Requirements FR15)`)
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.Parse()

	if showVersion {
		fmt.Println("canopy", version)
		return nil
	}
	if storage != "file" {
		return fmt.Errorf(`unsupported --storage value %q: only "file" is supported in v1`, storage)
	}

	projectRoot, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to resolve the current directory: %w", err)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to resolve your home directory: %w", err)
	}

	// --- Claude Code file-format compatibility layer (Design §3.11) ---
	agents, err := agentsource.Load(projectRoot, homeDir)
	if err != nil {
		return fmt.Errorf("loading agent definitions: %w", err)
	}
	skills, err := skillsource.Load(projectRoot, homeDir)
	if err != nil {
		return fmt.Errorf("loading skills: %w", err)
	}
	mcpServers, err := mcpsource.Load(projectRoot)
	if err != nil {
		return fmt.Errorf("loading MCP server config: %w", err)
	}
	projectInstructions, err := projectcontext.Load(projectRoot)
	if err != nil {
		return fmt.Errorf("loading project instructions: %w", err)
	}

	if len(agents) == 0 {
		return fmt.Errorf(
			"no agent definitions found.\n\n"+
				"Canopy looked for *.md files in:\n"+
				"  - %s\n"+
				"  - %s\n\n"+
				"Add at least one agent definition (YAML frontmatter with \"name\" and\n"+
				"\"description\", followed by the agent's instructions as the file body) to\n"+
				"either location and run canopy again. See docs/DESIGN.md §3.11 / Claude\n"+
				"Code's subagent format for the exact shape.",
			filepath.Join(projectRoot, ".claude", "agents"),
			filepath.Join(homeDir, ".claude", "agents"),
		)
	}

	// --- Canopy's own provider/model config (Design §4) ---
	providerStore, err := config.NewProviderStoreForProject(projectRoot, global)
	if err != nil {
		return fmt.Errorf("resolving provider config location: %w", err)
	}
	providersFile, err := providerStore.Load()
	if err != nil {
		return fmt.Errorf("loading provider config: %w", err)
	}
	if len(providersFile.Providers) == 0 || len(providersFile.Models) == 0 {
		return fmt.Errorf(
			"no providers/models configured.\n\n"+
				"Canopy looked for a provider config file at:\n"+
				"  %s\n\n"+
				"Create it with at least one provider and one model, e.g.:\n\n"+
				"  {\n"+
				"    \"providers\": [{\"name\": \"openai\", \"type\": \"openai\", \"api_key\": \"sk-...\"}],\n"+
				"    \"models\": [{\"name\": \"gpt\", \"provider\": \"openai\", \"model_name\": \"gpt-4o-mini\"}]\n"+
				"  }\n\n"+
				"Pass --global/-g to use ~/.canopy/providers.json instead of a\n"+
				"project-local .canopy/providers.json. See docs/DESIGN.md §4.",
			providerStore.Path(),
		)
	}
	// Judgment call: an AgentDefinition with no "model" frontmatter override
	// falls back to the first configured model (ProvidersFile.Models is a
	// JSON array, so "first" is whatever order the user's config file lists
	// them in) rather than requiring every deployment to additionally
	// designate one model as "default" in a currently-nonexistent config
	// field. Not documented anywhere in Design/Requirements as *the* answer
	// — flagged here as this phase's specific choice, easy to revisit if a
	// real default-model config field gets added later.
	defaultModel := providersFile.Models[0].Name

	// --- Chat storage (Design §6/§2's "repositories/json") ---
	//
	// Judgment call: FR15 only specifies --global/-g for provider config
	// (Design §4); chat storage location isn't spelled out. Chat storage
	// co-locates with wherever providerStore actually resolved to
	// (<dir of providerStore.Path()>/chats), rather than independently
	// re-deriving local-vs-global by checking whether a chats/ directory
	// already exists: that check is a chicken-and-egg bug on a fresh
	// project — .canopy/chats can never exist before the very first chat,
	// so it would always fall back to global on a project's first run even
	// when its .canopy/providers.json is correctly project-local. Deriving
	// chatRoot from providerStore.Path() instead guarantees the two always
	// agree, and removes the redundant os.Stat check entirely.
	chatRoot := filepath.Join(filepath.Dir(providerStore.Path()), "chats")
	repo, err := jsonrepo.NewChatRepository(chatRoot)
	if err != nil {
		return fmt.Errorf("initializing chat storage at %s: %w", chatRoot, err)
	}

	// --- Structured logging (Design §3.10, Requirements FR16) ---
	//
	// Judgment call: the TUI takes over the terminal (tea.WithAltScreen),
	// so run/middleware/provider diagnostics can't go to stderr the way
	// cmd/hello's spike or a future non-interactive mode would send them —
	// that would corrupt the alt-screen display. They're written to a log
	// file under the same directory chat storage uses instead.
	logPath := filepath.Join(filepath.Dir(chatRoot), "canopy.log")
	zapLogger, err := newFileLogger(logPath)
	if err != nil {
		return fmt.Errorf("initializing logging at %s: %w", logPath, err)
	}
	defer func() { _ = zapLogger.Sync() }()
	slogLogger := logging.NewSlogLogger(zapLogger)

	ctx := context.Background()

	// --- MCP client connections (Design §3.11, Requirements FR6/FR18) ---
	//
	// Connecting to every configured MCP server has to happen here, eagerly,
	// before AgentService is constructed — mcpclient's own doc comment
	// ("Connection lifecycle: eager, not lazy") explains why this I/O can't
	// live inside NewAgentService itself. A server that fails to connect is
	// not fatal to startup (Requirements §7: Canopy must still start and
	// function with its core tools even if one configured MCP server is
	// broken) — every failure is already logged to the log file by
	// ConnectAll via slogLogger, and additionally surfaced here as a
	// non-fatal warning directly on stderr, which only works while stderr is
	// still the terminal — i.e. strictly before tea.WithAltScreen's program
	// takes it over below. mcpRegistry.Close is deferred immediately so a
	// connected server's subprocess/connection can never outlive this
	// process, however run() returns (including an early error return before
	// program.Run is ever reached).
	mcpRegistry, mcpErrs := mcpclient.ConnectAll(ctx, mcpServers, slogLogger)
	defer func() { _ = mcpRegistry.Close() }()
	for _, mcpErr := range mcpErrs {
		fmt.Fprintln(os.Stderr, "canopy: warning:", mcpErr)
	}

	svc := services.NewAgentService(services.AgentServiceConfig{
		Definitions: services.Definitions{
			Agents:              agents,
			Skills:              skills,
			MCPServers:          mcpServers,
			ProjectInstructions: projectInstructions,
		},
		Providers:    providersFile.Providers,
		Models:       providersFile.Models,
		DefaultModel: defaultModel,
		Repository:   repo,
		MCPTools:     mcpRegistry.Tools(),
		Tools: services.ToolsConfig{
			WorkingRoot: projectRoot,
			BashTimeout: 2 * time.Minute,
			// WebSearchBackend is intentionally left nil: Canopy ships no
			// default search backend (see ToolsConfig.WebSearchBackend's
			// doc comment and tools.NewSearXNGBackend, which needs a
			// self-hosted SearXNG instance's URL this command has no way
			// to discover on its own). A deployment that wants the
			// web-search tool configures one via a later flag/config
			// field — a real gap, not an oversight, and flagged as such
			// rather than silently wired to nothing.
		},
		Logger: slogLogger,
	})

	program := tea.NewProgram(tui.NewModel(ctx, svc, agents), tea.WithAltScreen())
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("running the TUI: %w", err)
	}
	return nil
}

// newFileLogger builds a production (structured JSON) zap.Logger writing to
// path instead of stderr — see run's "Structured logging" comment for why.
func newFileLogger(path string) (*zap.Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating log directory: %w", err)
	}
	cfg := zap.NewProductionConfig()
	cfg.OutputPaths = []string{path}
	cfg.ErrorOutputPaths = []string{path}
	logger, err := cfg.Build()
	if err != nil {
		return nil, fmt.Errorf("building logger: %w", err)
	}
	return logger, nil
}
