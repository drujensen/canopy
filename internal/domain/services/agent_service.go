// Package services holds Canopy's orchestration logic (Design §2):
// AgentService ties agentsource-loaded agent definitions, the flat JSON
// provider/model config, the core tool set, and impl/harness's chat-bound
// agent construction together into one entry point a caller (eventually the
// TUI, Plan Phase 6) uses to have a turn with an agent tied to a persisted
// Chat.
//
// # A deliberate deviation from Design §2's package diagram
//
// Design §2 lists domain/interfaces as eventually holding AgentSource,
// SkillSource, and MCPSource interfaces alongside ChatRepository, with the
// implication that domain/services would depend only on those interfaces,
// never on impl/agentsource, impl/skillsource, or impl/mcpsource directly.
// Those three interfaces were never actually introduced in Phase 3.5 — only
// ChatRepository exists in domain/interfaces today, and agentsource/
// skillsource/mcpsource ship as plain "Load(...) (map[string]X, error)"
// functions with no interface indirection. Introducing new domain
// interfaces for them is out of this phase's scope (PLAN.md Phase 4 doesn't
// list it, and doing so unprompted risks guessing at a shape Phase 5/6
// won't actually want). So AgentService pragmatically depends on the
// concrete impl/agentsource.AgentDefinition and impl/skillsource.
// SkillDefinition types directly, the same way impl/providers.New already
// takes entities.ProviderConfig/ModelConfig directly rather than through an
// interface. This can be tightened later without changing AgentService's
// exported behavior.
package services

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/agenttool"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/domain/interfaces"
	"github.com/drujensen/canopy/internal/impl/agentsource"
	"github.com/drujensen/canopy/internal/impl/harness"
	"github.com/drujensen/canopy/internal/impl/mcpsource"
	"github.com/drujensen/canopy/internal/impl/providers"
	"github.com/drujensen/canopy/internal/impl/skillsource"
	"github.com/drujensen/canopy/internal/impl/tools"
)

// coreToolNames lists the seven built-in tools (Design §3.2) in a fixed
// order, used both to build the "inherit everything" tool list (an
// AgentDefinition with no explicit Tools allowlist) deterministically and to
// validate an AgentDefinition's explicit allowlist.
var coreToolNames = []string{"Bash", "FileRead", "FileWrite", "FileSearch", "DirectoryList", "WebFetch", "WebSearch"}

// Definitions bundles the file-format loader output (Design §3.11) an
// AgentService is constructed from: agent and skill definitions loaded once
// at startup by agentsource/skillsource, parsed (not yet client-constructed
// — see ToolsConfig's doc comment) MCP server config from mcpsource, and the
// project's CLAUDE.md/AGENTS.md instructions from projectcontext.
type Definitions struct {
	Agents              map[string]agentsource.AgentDefinition
	Skills              map[string]skillsource.SkillDefinition
	MCPServers          map[string]mcpsource.MCPServerConfig
	ProjectInstructions string
}

// ToolsConfig configures the core tool set every agent is assembled with.
type ToolsConfig struct {
	// WorkingRoot confines file read/write/search, directory listing, and
	// the bash tool's default working directory to this directory tree.
	// Empty means no confinement.
	WorkingRoot string

	// BashTimeout is the per-command deadline for the bash tool. Zero means
	// no timeout.
	BashTimeout time.Duration

	// WebSearchBackend performs web searches. When nil, the web-search tool
	// is omitted from every agent's tool list rather than constructed with
	// no backend (tools.NewWebSearchTool panics on a nil Backend) — a
	// deployment that hasn't configured a search backend simply doesn't get
	// that tool, the same way it wouldn't get an MCP tool it hasn't
	// configured.
	WebSearchBackend tools.WebSearchBackend

	// WebFetchHTTPClient is the HTTP client the web-fetch tool uses. Nil
	// uses tools.NewWebFetchTool's own default.
	WebFetchHTTPClient *http.Client
}

// AgentServiceConfig configures NewAgentService.
type AgentServiceConfig struct {
	Definitions Definitions

	// Providers and Models are the flat JSON provider/model config (Design
	// §4), keyed by ProviderConfig.Name / ModelConfig.Name respectively when
	// resolving an AgentDefinition's provider/model.
	Providers []entities.ProviderConfig
	Models    []entities.ModelConfig

	// DefaultModel is the ModelConfig.Name used for any AgentDefinition that
	// doesn't set its own "model" frontmatter override.
	DefaultModel string

	Repository interfaces.ChatRepository
	Tools      ToolsConfig
	Logger     *slog.Logger
}

// AgentService ties agentsource-loaded agent definitions, provider/model
// config, the core tool set, and impl/harness together (Design §2). It is
// constructed once at startup from loader output and passed to callers
// (Plan Phase 6's TUI) that need to run turns against persisted chats.
type AgentService struct {
	defs         Definitions
	providers    map[string]entities.ProviderConfig
	models       map[string]entities.ModelConfig
	defaultModel string
	repository   interfaces.ChatRepository
	toolsCfg     ToolsConfig
	logger       *slog.Logger
}

// NewAgentService constructs an AgentService from already-loaded
// definitions and config. It performs no I/O itself — cfg.Definitions,
// cfg.Providers, and cfg.Models are expected to already come from the
// Phase 3.5 loaders / config.ProviderStore.
func NewAgentService(cfg AgentServiceConfig) *AgentService {
	providerIndex := make(map[string]entities.ProviderConfig, len(cfg.Providers))
	for _, p := range cfg.Providers {
		providerIndex[p.Name] = p
	}
	modelIndex := make(map[string]entities.ModelConfig, len(cfg.Models))
	for _, m := range cfg.Models {
		modelIndex[m.Name] = m
	}
	return &AgentService{
		defs:         cfg.Definitions,
		providers:    providerIndex,
		models:       modelIndex,
		defaultModel: cfg.DefaultModel,
		repository:   cfg.Repository,
		toolsCfg:     cfg.Tools,
		logger:       cfg.Logger,
	}
}

// StartChat creates and persists a new chat bound to the named agent
// definition. agentName must reference an agent definition this service was
// constructed with.
func (s *AgentService) StartChat(ctx context.Context, chatID, agentName string) (*entities.Chat, error) {
	if _, ok := s.defs.Agents[agentName]; !ok {
		return nil, fmt.Errorf("agent service: unknown agent %q", agentName)
	}
	now := time.Now().UTC()
	chat := &entities.Chat{
		ID:        chatID,
		AgentName: agentName,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repository.Create(ctx, chat); err != nil {
		return nil, fmt.Errorf("agent service: starting chat %q: %w", chatID, err)
	}
	return chat, nil
}

// RunText loads the chat with the given ID, builds the *agent.Agent its
// AgentName resolves to (with dynamic subagent dispatch tools for every
// other loaded agent definition, per FR9/Design §3.4), and runs one turn
// with msg as the new user message, collecting the full response.
func (s *AgentService) RunText(ctx context.Context, chatID, msg string) (*agent.Response, error) {
	chat, err := s.repository.Get(ctx, chatID)
	if err != nil {
		return nil, fmt.Errorf("agent service: loading chat %q: %w", chatID, err)
	}

	a, err := s.buildTopLevelAgent(ctx, chat)
	if err != nil {
		return nil, err
	}

	resp, err := a.RunText(ctx, msg).Collect()
	if err != nil {
		return nil, fmt.Errorf("agent service: running chat %q: %w", chatID, err)
	}
	return resp, nil
}

// buildTopLevelAgent constructs the *agent.Agent for chat.AgentName, bound
// to chat's persisted history via impl/harness, with every other loaded
// agent definition wrapped via tool/agenttool.New and added to its tool
// list as a dynamically-dispatchable subagent (FR9, Design §3.4). Subagents
// are themselves built via buildSubagentAgent, which does NOT recursively
// grant them further subagent-dispatch tools — this both matches Claude
// Code's own Task-tool behavior (a dispatched subagent doesn't itself
// dispatch further subagents) and avoids unbounded construction-time
// recursion when two agent definitions could each wrap the other.
func (s *AgentService) buildTopLevelAgent(ctx context.Context, chat *entities.Chat) (*agent.Agent, error) {
	def, ok := s.defs.Agents[chat.AgentName]
	if !ok {
		return nil, fmt.Errorf("agent service: chat %q references unknown agent %q", chat.ID, chat.AgentName)
	}

	provider, model, err := s.resolveProviderModel(def)
	if err != nil {
		return nil, err
	}

	toolList, err := s.buildTopLevelTools(ctx, def)
	if err != nil {
		return nil, err
	}

	cfg := agent.Config{
		Name:        def.Name,
		Description: def.Description,
		Tools:       toolList,
		RunOptions:  []agent.Option{agent.WithInstructions(s.buildInstructions(def))},
		Logger:      s.logger,
	}

	return harness.Build(ctx, harness.BuildParams{
		Chat:       chat,
		Repository: s.repository,
		Provider:   provider,
		Model:      model,
		Config:     cfg,
	})
}

// buildTopLevelTools assembles def's own core tool list plus, for every
// *other* loaded agent definition, a tool/agenttool-wrapped subagent tool
// (FR9, Design §3.4) — split out from buildTopLevelAgent so the assembly
// logic itself (which agents get wrapped, and that def is never wrapped as
// its own subagent) is directly testable without needing to inspect a
// constructed *agent.Agent's internals, which the framework doesn't expose
// an accessor for.
func (s *AgentService) buildTopLevelTools(ctx context.Context, def agentsource.AgentDefinition) ([]tool.Tool, error) {
	toolList, err := s.buildTools(def)
	if err != nil {
		return nil, err
	}

	for name, subDef := range s.defs.Agents {
		if name == def.Name {
			continue
		}
		subAgent, err := s.buildSubagentAgent(ctx, subDef)
		if err != nil {
			return nil, fmt.Errorf("agent service: building subagent %q for %q: %w", name, def.Name, err)
		}
		toolList = append(toolList, agenttool.New(subAgent, agenttool.Config{}))
	}
	return toolList, nil
}

// buildSubagentAgent constructs a *agent.Agent for def with the core tool
// set but no ChatHistoryProvider and no further subagent-dispatch tools, so
// it runs with the framework's default in-memory history provider — which,
// since tool/agenttool invokes it without a WithSession option, starts a
// brand-new session on every dispatch (Design §3.4's "context isolation is
// automatic" claim). This is what makes subagent dispatch genuinely
// isolated from the parent's conversation rather than sharing its Chat.
func (s *AgentService) buildSubagentAgent(ctx context.Context, def agentsource.AgentDefinition) (*agent.Agent, error) {
	provider, model, err := s.resolveProviderModel(def)
	if err != nil {
		return nil, err
	}
	toolList, err := s.buildTools(def)
	if err != nil {
		return nil, err
	}
	cfg := agent.Config{
		Name:        def.Name,
		Description: def.Description,
		Tools:       toolList,
		RunOptions:  []agent.Option{agent.WithInstructions(s.buildInstructions(def))},
		Logger:      s.logger,
	}
	return providers.New(ctx, provider, model, cfg)
}

// resolveProviderModel resolves def's provider/model against the flat JSON
// config (Design §4): def.Model, when set, names a ModelConfig; otherwise
// s.defaultModel is used. The named ModelConfig's Provider then names a
// ProviderConfig.
func (s *AgentService) resolveProviderModel(def agentsource.AgentDefinition) (entities.ProviderConfig, entities.ModelConfig, error) {
	modelName := def.Model
	if modelName == "" {
		modelName = s.defaultModel
	}
	if modelName == "" {
		return entities.ProviderConfig{}, entities.ModelConfig{}, fmt.Errorf("agent service: agent %q has no model configured and no default model is set", def.Name)
	}
	model, ok := s.models[modelName]
	if !ok {
		return entities.ProviderConfig{}, entities.ModelConfig{}, fmt.Errorf("agent service: agent %q references unknown model %q", def.Name, modelName)
	}
	provider, ok := s.providers[model.Provider]
	if !ok {
		return entities.ProviderConfig{}, entities.ModelConfig{}, fmt.Errorf("agent service: model %q references unknown provider %q", model.Name, model.Provider)
	}
	return provider, model, nil
}

// buildInstructions combines the project's CLAUDE.md/AGENTS.md content
// (Design §3.11, loaded once into Definitions.ProjectInstructions) with
// def's own body instructions, project context first, matching
// projectcontext's own doc comment ordering.
func (s *AgentService) buildInstructions(def agentsource.AgentDefinition) string {
	switch {
	case s.defs.ProjectInstructions == "":
		return def.Instructions
	case def.Instructions == "":
		return s.defs.ProjectInstructions
	default:
		return s.defs.ProjectInstructions + "\n\n" + def.Instructions
	}
}

// buildTools assembles def's tool list from the core built-in set (Design
// §3.2): every core tool when def.Tools is empty ("tools" frontmatter
// omitted means inherit everything, per agentsource's doc comment), or
// exactly the named subset otherwise.
//
// TODO(Design §3.11/PLAN.md Phase 4 scope note): MCP-provided tools
// (tool/mcptool, against s.defs.MCPServers) are deliberately not
// constructed here. mcpsource already parses .mcp.json (Phase 3.5), but
// constructing actual MCP clients from that config is explicitly deferred —
// Design §3.11 defers it in its own phase note, and PLAN.md Phase 4's task
// list only asks for "an empty/no-op path for MCP-provided tools" this
// phase. s.defs.MCPServers is threaded through Definitions so a later phase
// has it ready to use without re-plumbing.
func (s *AgentService) buildTools(def agentsource.AgentDefinition) ([]tool.Tool, error) {
	available, err := s.coreTools()
	if err != nil {
		return nil, err
	}

	if len(def.Tools) == 0 {
		result := make([]tool.Tool, 0, len(coreToolNames))
		for _, name := range coreToolNames {
			if t, ok := available[name]; ok {
				result = append(result, t)
			}
		}
		return result, nil
	}

	result := make([]tool.Tool, 0, len(def.Tools))
	for _, name := range def.Tools {
		t, ok := available[name]
		if !ok {
			return nil, fmt.Errorf("agent service: agent %q references unknown or unavailable tool %q", def.Name, name)
		}
		result = append(result, t)
	}
	return result, nil
}

// coreTools constructs one instance of each of the seven built-in tools
// (Design §3.2), keyed by the name used in an AgentDefinition's "tools"
// frontmatter allowlist. WebSearch is omitted when no WebSearchBackend is
// configured (see ToolsConfig.WebSearchBackend's doc comment).
func (s *AgentService) coreTools() (map[string]tool.Tool, error) {
	bash, err := tools.NewBashTool(tools.BashConfig{
		WorkingDirectory: s.toolsCfg.WorkingRoot,
		Timeout:          s.toolsCfg.BashTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("agent service: constructing bash tool: %w", err)
	}

	result := map[string]tool.Tool{
		"Bash":          bash,
		"FileRead":      tools.NewFileReadTool(tools.FileReadConfig{Root: s.toolsCfg.WorkingRoot}),
		"FileWrite":     tools.NewFileWriteTool(tools.FileWriteConfig{Root: s.toolsCfg.WorkingRoot}),
		"FileSearch":    tools.NewFileSearchTool(tools.FileSearchConfig{Root: s.toolsCfg.WorkingRoot}),
		"DirectoryList": tools.NewDirectoryListTool(tools.DirectoryListConfig{Root: s.toolsCfg.WorkingRoot}),
		"WebFetch":      tools.NewWebFetchTool(tools.WebFetchConfig{HTTPClient: s.toolsCfg.WebFetchHTTPClient}),
	}
	if s.toolsCfg.WebSearchBackend != nil {
		result["WebSearch"] = tools.NewWebSearchTool(tools.WebSearchConfig{Backend: s.toolsCfg.WebSearchBackend})
	}
	return result, nil
}
