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
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/microsoft/agent-framework-go/agent"
	"go.uber.org/zap"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/domain/services"
	"github.com/drujensen/canopy/internal/impl/agentsource"
	"github.com/drujensen/canopy/internal/impl/config"
	"github.com/drujensen/canopy/internal/impl/logging"
	"github.com/drujensen/canopy/internal/impl/mcpclient"
	"github.com/drujensen/canopy/internal/impl/mcpsource"
	"github.com/drujensen/canopy/internal/impl/modelsdev"
	"github.com/drujensen/canopy/internal/impl/projectcontext"
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
	"github.com/drujensen/canopy/internal/impl/skillsource"
	"github.com/drujensen/canopy/internal/impl/tracing"
	"github.com/drujensen/canopy/internal/tui"
)

// modelsDevCacheMaxAge is how long a cached models.dev catalog fetch (Design
// §4 addendum) is considered fresh enough to skip a live network round-trip
// on ordinary startup — matches the predecessor aiagent project's refresh
// interval for the same catalog.
const modelsDevCacheMaxAge = 24 * time.Hour

// version is Canopy's own version string (Requirements FR15's --version
// flag). A package-level var, not a const, so a release build can override
// it at link time via `-ldflags="-X main.version=..."` (Go's -X flag only
// rewrites string variables, not constants) without editing source — see
// the Makefile's `build`/`build-all`/`install` targets (Plan Phase 7).
// Bumped at release time; unset (this default) means a dev build.
var version = "0.1.0-dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "canopy:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		global           bool
		storage          string
		showVersion      bool
		enableOTel       bool
		refreshProviders bool
		continueLatest   bool
	)
	flag.BoolVar(&global, "global", false, "use ~/.canopy config/storage instead of a project-local .canopy directory")
	flag.BoolVar(&global, "g", false, "shorthand for --global")
	flag.StringVar(&storage, "storage", "file", `storage backend; "file" is the only supported value in v1 (Requirements FR15)`)
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.BoolVar(&enableOTel, "otel", false, "enable optional OpenTelemetry tracing (Design §3.10); also enabled implicitly when OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is set")
	flag.BoolVar(&refreshProviders, "refresh-providers", false, "force a live re-fetch of the models.dev catalog, add any newly-detectable providers/models to providers.json, and refresh cost data on existing models (every other field of an existing entry is left untouched — Design §4 addendum)")
	flag.BoolVar(&continueLatest, "continue", false, "resume the most recently updated chat session (across any agent) instead of starting a new one or auto-resuming the last-used agent — Design §5 addendum")
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

	// Zero-config first run (post-v0.1.0, docs/DESIGN.md §3.11 addendum):
	// rather than hard-erroring when no agent definitions exist anywhere,
	// auto-create a sensible default agent under ~/.canopy/agents (Canopy's
	// own directory, deliberately not ~/.claude/agents — see
	// agentsource.WriteDefault's doc comment) and fold it into this run's
	// in-memory result so the picker has something to show without
	// requiring a restart. A failure here (e.g. permission error creating
	// ~/.canopy/agents) is a real startup error worth surfacing clearly,
	// not a reason to silently proceed with zero agents.
	if len(agents) == 0 {
		defaultDir := filepath.Join(homeDir, ".canopy", "agents")
		if err := agentsource.WriteDefault(defaultDir); err != nil {
			return fmt.Errorf("creating default agent: %w", err)
		}
		agents, err = agentsource.Load(projectRoot, homeDir)
		if err != nil {
			return fmt.Errorf("loading agent definitions: %w", err)
		}
		fmt.Fprintf(os.Stderr, "canopy: no agent definitions found; created a default \"general\" agent at %s\n", filepath.Join(defaultDir, "general.md"))
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

	// models.dev catalog cache lives alongside providers.json — the same
	// directory providerStore already resolved to, so this doesn't invent a
	// fourth path-resolution scheme on top of --global/-g.
	modelsCachePath := filepath.Join(filepath.Dir(providerStore.Path()), "models-cache.json")
	detectCtx := context.Background()

	// Auto-resume the last-used agent (post-v0.1.0 addendum, Design §5's
	// addendum): last_agent.json lives alongside providers.json/
	// models-cache.json/chats/, the same directory convention every other
	// per-project state file already follows. Reading it is best-effort —
	// a failure (permissions, corrupt file) falls through to the normal
	// picker-screen startup rather than being a hard error, since this is
	// purely a startup-convenience feature.
	lastAgentPath := filepath.Join(filepath.Dir(providerStore.Path()), "last_agent.json")
	lastUsedAgent, err := config.LoadLastAgent(lastAgentPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "canopy: warning: couldn't read last-used agent:", err)
	}
	startAgent := computeStartAgent(agents, lastUsedAgent)

	// Explicit refresh (post-v0.1.0 addendum, Design §4 addendum):
	// --refresh-providers forces a live re-fetch (bypasses the
	// cache-freshness check entirely — FetchCached's maxAge<=0 contract) and
	// re-runs detection, ADDING any newly-detectable providers/models to the
	// existing file rather than regenerating it — an already-present
	// provider, matched by Name, is never touched even if the catalog's data
	// for it changed, since the user may have hand-edited it. This is the
	// escape hatch for "I added a new API key to my environment after first
	// run and want Canopy to notice without hand-editing JSON."
	//
	// The one deliberate exception to "never touched" is per-model cost
	// (updateExistingModelCosts): every existing model, even one whose
	// provider was already configured and so skipped entirely by
	// mergeNewProviders, gets its cost fields refreshed from the live
	// catalog. See that function's doc comment for why cost specifically is
	// safe to keep fresh where every other field on an existing entry isn't.
	if refreshProviders {
		catalog, _, err := modelsdev.FetchCached(detectCtx, modelsCachePath, 0)
		if err != nil {
			return fmt.Errorf("refreshing models.dev catalog: %w", err)
		}
		detectedFile, _, _ := config.DetectProviders(catalog, os.Environ())

		// Merge into a fresh, UNRESOLVED read of what's actually on disk
		// (LoadRaw, not the already-loaded providersFile above): providersFile
		// went through Load's APIKeyEnv->APIKey resolution, and saving that
		// back out would write a resolved literal secret into providers.json
		// — exactly what APIKeyEnv exists to avoid. See ProviderStore.Load's
		// doc comment.
		raw, err := providerStore.LoadRaw()
		if err != nil {
			return fmt.Errorf("re-reading provider config for --refresh-providers: %w", err)
		}
		added := mergeNewProviders(raw, detectedFile)
		updatedCosts := updateExistingModelCosts(raw, detectedFile)

		if len(added) > 0 || updatedCosts > 0 {
			if err := providerStore.Save(raw); err != nil {
				return fmt.Errorf("saving refreshed provider config: %w", err)
			}
			// Reload (resolved) so this run's AgentService picks up the
			// newly-added provider(s)/refreshed cost immediately, no restart
			// needed — same as the zero-config first-run path below.
			providersFile, err = providerStore.Load()
			if err != nil {
				return fmt.Errorf("reloading provider config after refresh: %w", err)
			}
		}
		switch {
		case len(added) > 0 && updatedCosts > 0:
			fmt.Fprintf(os.Stderr, "canopy: --refresh-providers added %d new provider(s) to %s:\n  %s\ncanopy: --refresh-providers also refreshed cost data for %d existing model(s)\n",
				len(added), providerStore.Path(), strings.Join(describeProviders(providersFile.Providers, added), "\n  "), updatedCosts)
		case len(added) > 0:
			fmt.Fprintf(os.Stderr, "canopy: --refresh-providers added %d new provider(s) to %s:\n  %s\n",
				len(added), providerStore.Path(), strings.Join(describeProviders(providersFile.Providers, added), "\n  "))
		case updatedCosts > 0:
			fmt.Fprintf(os.Stderr, "canopy: --refresh-providers found no new providers, but refreshed cost data for %d existing model(s) in %s\n",
				updatedCosts, providerStore.Path())
		default:
			fmt.Fprintln(os.Stderr, "canopy: --refresh-providers found no new providers or cost changes (already up to date, or no new provider env vars set)")
		}
	}

	// Zero-config first run (post-v0.1.0, Design §4 addendum): rather than
	// hard-erroring when no providers/models are configured, try live
	// auto-detection against whichever provider API-key env vars the user
	// already has set (extremely common — OPENAI_API_KEY, ANTHROPIC_API_KEY,
	// GEMINI_API_KEY, etc.) using the models.dev catalog — the same
	// zero-manual-config spirit as agentsource.WriteDefault's default-agent
	// feature above. A cached catalog fetch (24h freshness, see
	// modelsDevCacheMaxAge) is used so this doesn't add a network
	// round-trip to every ordinary startup once a fetch has happened once.
	if len(providersFile.Providers) == 0 || len(providersFile.Models) == 0 {
		catalog, _, catalogErr := modelsdev.FetchCached(detectCtx, modelsCachePath, modelsDevCacheMaxAge)
		if catalogErr != nil {
			return fmt.Errorf(
				"no providers/models configured, and Canopy tried live auto-detection against "+
					"https://models.dev but couldn't reach it: %v\n\n"+
					"Canopy looked for a provider config file at:\n"+
					"  %s\n\n"+
					"Create it with at least one provider and one model, e.g.:\n\n"+
					"  {\n"+
					"    \"providers\": [{\"name\": \"openai\", \"type\": \"openai\", \"api_key\": \"sk-...\"}],\n"+
					"    \"models\": [{\"name\": \"gpt\", \"provider\": \"openai\", \"model_name\": \"gpt-4o-mini\"}]\n"+
					"  }\n\n"+
					"Pass --global/-g to use ~/.canopy/providers.json instead of a\n"+
					"project-local .canopy/providers.json. See docs/DESIGN.md §4.",
				catalogErr, providerStore.Path(),
			)
		}

		detectedFile, detectedNames, _ := config.DetectProviders(catalog, os.Environ())
		if len(detectedNames) == 0 {
			return fmt.Errorf(
				"no providers/models configured. Canopy checked your environment for known provider "+
					"API keys (using the live models.dev catalog) but found none set.\n\n"+
					"Canopy checked these environment variable names:\n"+
					"  %s\n\n"+
					"Set one of these and run canopy again, or hand-write a provider config file at:\n"+
					"  %s\n\n"+
					"e.g.:\n\n"+
					"  {\n"+
					"    \"providers\": [{\"name\": \"openai\", \"type\": \"openai\", \"api_key\": \"sk-...\"}],\n"+
					"    \"models\": [{\"name\": \"gpt\", \"provider\": \"openai\", \"model_name\": \"gpt-4o-mini\"}]\n"+
					"  }\n\n"+
					"Pass --global/-g to use ~/.canopy/providers.json instead of a\n"+
					"project-local .canopy/providers.json. See docs/DESIGN.md §4.",
				strings.Join(knownEnvVarNames(catalog), ", "), providerStore.Path(),
			)
		}

		// detectedFile is pristine (DetectProviders never sets a literal
		// APIKey, only APIKeyEnv), so saving it directly is safe. Reload
		// afterward (resolved) rather than just assigning providersFile =
		// &detectedFile, so this run's AgentService gets a real, populated
		// APIKey the same way any config loaded from an existing file would
		// — see ProviderStore.Load's doc comment on why Load's result and
		// only Load's result should ever reach AgentService.
		if err := providerStore.Save(&detectedFile); err != nil {
			return fmt.Errorf("saving auto-detected provider config: %w", err)
		}
		providersFile, err = providerStore.Load()
		if err != nil {
			return fmt.Errorf("reloading auto-detected provider config: %w", err)
		}
		fmt.Fprintf(os.Stderr, "canopy: no providers/models configured; auto-detected %d provider(s) from your environment and saved to %s:\n  %s\n",
			len(detectedNames), providerStore.Path(), strings.Join(describeProviders(providersFile.Providers, detectedNames), "\n  "))
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

	// --- Optional OpenTelemetry tracing (Design §3.10, Plan Phase 7) ---
	//
	// Off by default: otelEnabled is false unless -otel was passed or the
	// standard OTel SDK endpoint env vars are set, in which case
	// tracing.Setup is a no-op that returns a nil middleware and a
	// no-op shutdown (see its doc comment) — zero overhead, zero new
	// behavior, matching Design §3.10's "not required for v1 usage."
	// A failure to build the exporter (e.g. a malformed endpoint) is
	// treated as non-fatal, exactly like an MCP server that fails to
	// connect above: tracing is optional, so a misconfigured collector
	// must not stop Canopy from starting.
	otelEnabled := enableOTel || os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
	otelMiddleware, otelShutdown, err := tracing.Setup(ctx, tracing.Config{Enabled: otelEnabled, ServiceName: "canopy"})
	if err != nil {
		fmt.Fprintln(os.Stderr, "canopy: warning: OpenTelemetry tracing disabled:", err)
		otelMiddleware, otelShutdown = nil, func(context.Context) error { return nil }
	}
	defer func() {
		// Bounded, not context.Background(): otelShutdown flushes
		// in-flight spans over the network, and an unreachable collector
		// must not hang process shutdown (see tracing.Setup's doc
		// comment).
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = otelShutdown(shutdownCtx)
	}()
	var middlewares []agent.Middleware
	if otelMiddleware != nil {
		middlewares = append(middlewares, otelMiddleware)
	}

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
		Logger:      slogLogger,
		Middlewares: middlewares,
		RecordLastAgent: func(agentName string) error {
			return config.SaveLastAgent(lastAgentPath, agentName)
		},
	})

	// --continue (post-v0.1.0 addendum, Design §5 addendum): resolved after
	// svc exists, unlike startAgent, since it needs a real AgentService
	// call (ListChatSummaries — disk I/O reading every persisted chat) that
	// computeStartAgent's purely in-memory decision never required. Takes
	// priority over startAgent in tui.Model.Init — see that field's own doc
	// comment. A failure or an empty history is non-fatal: --continue with
	// nothing to resume just falls through to normal startup (the
	// already-resolved startAgent, or the picker).
	var resumeChatID string
	if continueLatest {
		summaries, err := svc.ListChatSummaries(ctx)
		switch {
		case err != nil:
			fmt.Fprintln(os.Stderr, "canopy: warning: couldn't list chat history for --continue:", err)
		case len(summaries) == 0:
			fmt.Fprintln(os.Stderr, "canopy: --continue: no previous chat sessions found; starting a new chat")
		default:
			resumeChatID = summaries[0].ID
		}
	}

	program := tea.NewProgram(tui.NewModel(ctx, svc, agents, startAgent, resumeChatID), tea.WithAltScreen())
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("running the TUI: %w", err)
	}
	return nil
}

// mergeNewProviders adds any provider (and its paired model, matched via
// ModelConfig.Provider) from detected that isn't already present in dst,
// computeStartAgent decides which agent (if any) this run should
// auto-resume into (post-v0.1.0 addendum, Design §5's addendum), rather
// than always showing the top-level agent picker:
//
//  1. lastUsed, if it still names an entry in agents — the common case
//     once at least one prior session has picked an agent.
//  2. Otherwise "general", if that's a configured agent — matching
//     agentsource.WriteDefault's own choice of default agent name, so a
//     brand-new install (which has no last-used record yet) still starts
//     with zero friction rather than a picker showing a single obvious
//     choice.
//  3. Otherwise "" — no sensible single default to guess (lastUsed was
//     never set, or named an agent that's since been removed, and there's
//     no "general" agent either), so the caller falls through to showing
//     the picker exactly as before this feature existed.
func computeStartAgent(agents map[string]agentsource.AgentDefinition, lastUsed string) string {
	if lastUsed != "" {
		if _, ok := agents[lastUsed]; ok {
			return lastUsed
		}
	}
	if _, ok := agents["general"]; ok {
		return "general"
	}
	return ""
}

// mergeNewProviders adds any provider (and its paired model, matched via
// ProviderConfig.Name — --refresh-providers' "additive, never clobbers an
// existing entry" contract (Design §4 addendum). Returns the names of
// providers actually added, for logging.
func mergeNewProviders(dst *config.ProvidersFile, detected config.ProvidersFile) []string {
	existing := make(map[string]bool, len(dst.Providers))
	for _, p := range dst.Providers {
		existing[p.Name] = true
	}

	var added []string
	for _, p := range detected.Providers {
		if existing[p.Name] {
			continue
		}
		dst.Providers = append(dst.Providers, p)
		for _, m := range detected.Models {
			if m.Provider == p.Name {
				dst.Models = append(dst.Models, m)
			}
		}
		added = append(added, p.Name)
	}
	return added
}

// modelKey identifies a model independent of its user-chosen
// entities.ModelConfig.Name (which mergeNewProviders/updateExistingModelCosts
// must never require agreement on — a user is free to rename a model entry
// after Canopy first wrote it) — (Provider, ModelName) is the pair that
// actually ties a dst.Models entry back to one specific models.dev catalog
// model.
type modelKey struct{ provider, modelName string }

// updateExistingModelCosts refreshes InputCostPerMillionTokens/
// OutputCostPerMillionTokens on every dst.Models entry whose (Provider,
// ModelName) matches a model in freshly re-detected catalog data — including
// one whose provider was already configured and so mergeNewProviders skipped
// it entirely as "not new" (this is deliberately the one exception to
// mergeNewProviders' "an already-present entry is never touched" guarantee:
// see that function's own doc comment and Design §4's addendum). Cost is
// catalog metadata a user has no real reason to hand-edit away from whatever
// the provider actually charges — unlike BaseURL, APIKey, or ModelName
// itself, which stay sacrosanct on an existing entry — so keeping it fresh
// on --refresh-providers is safe where touching any other field wouldn't be.
//
// A dst model with no match in detected.Models (a self-hosted model like
// Ollama's, which models.dev has no pricing for at all, or simply a provider
// that isn't currently detectable — its env var unset) is left completely
// untouched, cost included: this only ever refreshes toward fresher catalog
// data Canopy actually has in hand, never clears a value to zero for absence
// of new data. Returns the count of models whose cost genuinely changed, for
// the refresh summary message.
func updateExistingModelCosts(dst *config.ProvidersFile, detected config.ProvidersFile) int {
	type cost struct{ input, output float64 }
	costByKey := make(map[modelKey]cost, len(detected.Models))
	for _, m := range detected.Models {
		costByKey[modelKey{m.Provider, m.ModelName}] = cost{m.InputCostPerMillionTokens, m.OutputCostPerMillionTokens}
	}

	updated := 0
	for i := range dst.Models {
		m := &dst.Models[i]
		c, ok := costByKey[modelKey{m.Provider, m.ModelName}]
		if !ok {
			continue
		}
		if m.InputCostPerMillionTokens == c.input && m.OutputCostPerMillionTokens == c.output {
			continue
		}
		m.InputCostPerMillionTokens = c.input
		m.OutputCostPerMillionTokens = c.output
		updated++
	}
	return updated
}

// describeProviders renders "name (from ENV_VAR)" for each of names, in the
// order given, looking up each provider's APIKeyEnv from providers — used
// to tell the user which env var each auto-configured provider came from
// without ever printing a key value.
func describeProviders(providers []entities.ProviderConfig, names []string) []string {
	byName := make(map[string]entities.ProviderConfig, len(providers))
	for _, p := range providers {
		byName[p.Name] = p
	}
	descriptions := make([]string, 0, len(names))
	for _, name := range names {
		p := byName[name]
		if p.APIKeyEnv != "" {
			descriptions = append(descriptions, fmt.Sprintf("%s (from %s)", p.Name, p.APIKeyEnv))
		} else {
			descriptions = append(descriptions, p.Name)
		}
	}
	return descriptions
}

// knownEnvVarNames collects every env var name the live models.dev catalog
// associates with any provider, deduplicated and sorted — used to tell the
// user exactly what Canopy checked when auto-detection finds nothing to
// configure, pulled live from the catalog rather than a hardcoded/stale
// list.
func knownEnvVarNames(catalog *modelsdev.Catalog) []string {
	seen := make(map[string]bool)
	var names []string
	for _, p := range *catalog {
		for _, name := range p.Env {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
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
