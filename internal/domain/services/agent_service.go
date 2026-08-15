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
	"github.com/microsoft/agent-framework-go/agent/harness/agentmode"
	"github.com/microsoft/agent-framework-go/agent/harness/todo"
	"github.com/microsoft/agent-framework-go/message"
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

// planMode is the agentmode name (Design §3.8, Requirements FR12) under
// which mutating tools are withheld from every agent's tool list — see
// planModeMutatingTools and isPlanModeRestricted below for the mechanism
// this package uses to answer Design §3.8's flagged open point ("plan mode
// should pair with §3.6 ... the exact mechanism is impl/harness's to
// define").
const planMode = "plan"

// planModeMutatingTools are the core tool names (Design §3.2's two
// approval-gated tools — Bash, FileWrite) omitted from every agent's
// assembled tool list while the chat's current mode is planMode, regardless
// of the agent definition's own "tools" allowlist.
//
// # Design decision: omit, not just approval-force
//
// Design §3.8 flagged the exact plan-mode mechanism as open ("mutating
// tools stay approval-required or withheld while planning"). This package
// chooses withheld: plan mode drops Bash/FileWrite from the tool list
// entirely rather than leaving them present-but-approval-gated. Both tools
// are already approval-gated in every mode (Requirements FR5), so
// "approval-required" alone wouldn't actually change anything about plan
// mode — it would just be the status quo with an extra label. Omitting the
// tools instead gives plan mode a real, independent guarantee: even a
// standing "always approve this tool" rule from a prior execute-mode turn
// (Design §3.6) cannot let a mutating call slip through while planning,
// because the model has no way to invoke a tool that was never offered to
// it this turn. execute mode restores the tool's normal (approval-gated)
// availability.
//
// # Known limitation
//
// This restriction is only applied to the top-level, chat-bound agent
// (buildTopLevelAgent). A subagent dispatched via tool/agenttool
// (buildSubagentAgent) is NOT restricted by the parent's mode, since
// subagents run in a fresh, unpersisted session with no agentmode wiring of
// their own (Design §3.4's context-isolation model doesn't thread the
// parent's mode through). A plan-mode top-level agent could still cause a
// mutation indirectly by dispatching a subagent that calls Bash/FileWrite.
// Flagged here as a real follow-up, not silently accepted.
var planModeMutatingTools = map[string]struct{}{
	"Bash":      {},
	"FileWrite": {},
}

// isPlanModeRestricted reports whether toolName should be omitted from the
// assembled tool list because mode is planMode.
func isPlanModeRestricted(mode, toolName string) bool {
	if mode != planMode {
		return false
	}
	_, restricted := planModeMutatingTools[toolName]
	return restricted
}

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

	// todoProvider and modeProvider are the single, shared agent/harness/todo
	// and agent/harness/agentmode instances (Design §3.7/§3.8) used both to
	// wire every chat-bound agent's todos_*/mode_* tools (via
	// harness.BuildParams.TodoProvider/ModeProvider) and to answer
	// GetTodos/GetMode/SetMode directly from a chat's deserialized session,
	// without needing a live *agent.Agent mid-run — see those methods' doc
	// comments. One shared instance per AgentService (not per-agent, per-run)
	// because both providers are stateless configuration; the actual state
	// they read/write always lives on the *agent.Session passed via
	// agent.WithSession, keyed by a fixed package-internal state key, so any
	// same-configuration instance reads/writes the same data.
	todoProvider *todo.Provider
	modeProvider *agentmode.Provider
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
		todoProvider: todo.New(nil),
		// DefaultMode is "execute", not agentmode's own package default of
		// "plan" (defaultModes[0]): a brand-new chat should be immediately
		// able to act, matching Claude Code's default posture — a user opts
		// into plan mode (via the mode_set tool or, in Plan Phase 6's TUI, a
		// keybinding calling SetMode) rather than every chat starting
		// restricted until switched. This is a judgment call, not a
		// framework requirement.
		modeProvider: agentmode.New(agentmode.Config{DefaultMode: "execute"}),
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

// RunResult is AgentService.RunText/RunMessages' return value: the model's
// response plus a post-turn snapshot of the session-scoped state Design
// §3.7/§3.8 track (todos, mode), so a caller (Plan Phase 6's TUI) doesn't
// need a second round-trip just to render the current todo list or mode
// after every turn. Todos/Mode reflect state as of immediately after this
// turn completed (i.e. after session persistence — see persistSession).
type RunResult struct {
	Response *agent.Response
	Todos    []todo.Item
	Mode     string
}

// RunText loads the chat with the given ID, builds the *agent.Agent its
// AgentName resolves to, and runs one turn with msg as a new user text
// message. It's a thin wrapper around RunMessages for the common case.
func (s *AgentService) RunText(ctx context.Context, chatID, msg string) (*RunResult, error) {
	return s.RunMessages(ctx, chatID, []*message.Message{message.NewText(msg)})
}

// RunMessages loads the chat with the given ID, builds the *agent.Agent its
// AgentName resolves to (with dynamic subagent dispatch tools for every
// other loaded agent definition, per FR9/Design §3.4), and runs one turn
// with msgs as the new messages, collecting the full response.
//
// Unlike RunText, callers can supply arbitrary message content here — in
// particular, a message.ToolApprovalResponseContent or
// message.AlwaysApproveToolApprovalResponseContent (Design §3.6, Requirements
// FR5), which is the only way to answer a pending approval request
// surfaced in a prior turn's RunResult.Response. Plan Phase 6's TUI is the
// intended caller for that path (an "approve"/"always allow" prompt);
// RunText alone can't express it since a plain string can't carry
// structured approval content.
//
// # Session-state persistence (Design §3.9, Requirements FR14)
//
// The chat's persisted SessionState is deserialized into a *agent.Session
// before the run, passed to the agent via agent.WithSession, and — this is
// the part worth explaining — re-serialized and persisted via a second,
// explicit interfaces.ChatRepository.Update call *after* the run completes,
// rather than piggybacking on impl/harness.ChatHistoryProvider.Invoked the
// way message history is persisted.
//
// That "piggyback on Invoked" design was the originally recommended
// approach (one write covers messages + session state together) and was
// tried first, but reading agent/harness/toolapproval/toolapproval.go's
// run() function shows it doesn't actually work for toolapproval
// specifically: toolapproval's middleware sits in agent.Config.Middlewares,
// which wraps the *entire* agent invoke lifecycle including
// HistoryProvider.Invoked (see WireLoopQuality's doc comment for why it has
// to sit there). Every code path in toolapproval's run() that persists a
// newly-added standing Rule into the session (session.Set, inside its
// saveState helper) does so *after* it calls next(...) and that call
// returns — and next(...), for a middleware in Config.Middlewares, is (or
// wraps) the agent's whole invoke, which is where HistoryProvider.Invoked
// already fired. So a Rule added by this turn's "always approve" response
// is only written onto the *agent.Session object after
// ChatHistoryProvider.Invoked already read and persisted it — relying
// solely on Invoked would silently persist a chat.SessionState missing the
// very Rule this turn was supposed to record. This was empirically confirmed
// by tracing that call sequence, not assumed.
//
// The fix is this explicit second write: by the time a.RunText(...).Collect()
// returns to this function, the entire outer middleware chain (including
// toolapproval's post-next() saveState) has finished, so the *agent.Session
// object now reflects every mutation any part of the turn made to it —
// compaction's incremental index state, todo/mode's tool-driven state
// (both of which mutate *during* the provider call, well before
// HistoryProvider.Invoked runs, so those would actually have been fine
// either way), and toolapproval's Rule. persistSession reloads the chat
// (rather than reusing the chat variable from the top of this function) so
// it applies the SessionState write on top of whatever Messages
// ChatHistoryProvider.Invoked already persisted, instead of clobbering them
// with a stale in-memory copy. This is a real, deliberate double-write per
// turn (chat.Messages via Invoked, then chat.SessionState via
// persistSession) — noted here explicitly rather than left implicit.
func (s *AgentService) RunMessages(ctx context.Context, chatID string, msgs []*message.Message) (*RunResult, error) {
	chat, err := s.repository.Get(ctx, chatID)
	if err != nil {
		return nil, fmt.Errorf("agent service: loading chat %q: %w", chatID, err)
	}

	session, err := harness.LoadSession(chat)
	if err != nil {
		return nil, fmt.Errorf("agent service: loading session state for chat %q: %w", chatID, err)
	}

	a, err := s.buildTopLevelAgent(ctx, chat, session)
	if err != nil {
		return nil, err
	}

	resp, err := a.Run(ctx, msgs, agent.WithSession(session)).Collect()
	if err != nil {
		return nil, fmt.Errorf("agent service: running chat %q: %w", chatID, err)
	}

	if err := s.persistSession(ctx, chatID, session); err != nil {
		return nil, err
	}

	return &RunResult{
		Response: resp,
		Todos:    s.todoProvider.GetAllItems(agent.WithSession(session)),
		Mode:     s.modeProvider.GetMode(agent.WithSession(session)),
	}, nil
}

// persistSession serializes session and writes it onto the chat's
// SessionState field, reloading the chat first so this write doesn't
// clobber a concurrent Messages update (see RunMessages' doc comment for
// why this is a deliberate second write, not merged into
// ChatHistoryProvider.Invoked's own Update call).
func (s *AgentService) persistSession(ctx context.Context, chatID string, session *agent.Session) error {
	data, err := harness.SerializeSession(session)
	if err != nil {
		return fmt.Errorf("agent service: serializing session state for chat %q: %w", chatID, err)
	}
	chat, err := s.repository.Get(ctx, chatID)
	if err != nil {
		return fmt.Errorf("agent service: reloading chat %q to persist session state: %w", chatID, err)
	}
	chat.SessionState = data
	chat.UpdatedAt = time.Now().UTC()
	if err := s.repository.Update(ctx, chat); err != nil {
		return fmt.Errorf("agent service: persisting session state for chat %q: %w", chatID, err)
	}
	return nil
}

// GetTodos returns chat's current todo list (Design §3.7, Requirements
// FR11) without running a turn: it loads the chat, deserializes its
// persisted SessionState (LoadSession), and reads directly from that
// *agent.Session via todo.Provider.GetAllItems(agent.WithSession(session)) —
// GetAllItems only needs a session, not a live *agent.Agent mid-run, so this
// works standalone (e.g. a TUI polling between turns, or right after a
// simulated restart).
func (s *AgentService) GetTodos(ctx context.Context, chatID string) ([]todo.Item, error) {
	session, err := s.loadChatSession(ctx, chatID)
	if err != nil {
		return nil, err
	}
	return s.todoProvider.GetAllItems(agent.WithSession(session)), nil
}

// GetMode returns chat's current plan/execute mode (Design §3.8, Requirements
// FR12), read the same standalone way GetTodos reads todos.
func (s *AgentService) GetMode(ctx context.Context, chatID string) (string, error) {
	session, err := s.loadChatSession(ctx, chatID)
	if err != nil {
		return "", err
	}
	return s.modeProvider.GetMode(agent.WithSession(session)), nil
}

// SetMode updates chat's current mode and persists it immediately (Design
// §3.8: "the TUI also calls SetMode directly on a user keybinding") — a
// user-initiated mode switch doesn't need to wait for a run to take effect
// or to be saved.
func (s *AgentService) SetMode(ctx context.Context, chatID, mode string) error {
	session, err := s.loadChatSession(ctx, chatID)
	if err != nil {
		return err
	}
	if err := s.modeProvider.SetMode(mode, agent.WithSession(session)); err != nil {
		return fmt.Errorf("agent service: setting mode for chat %q: %w", chatID, err)
	}
	return s.persistSession(ctx, chatID, session)
}

// loadChatSession loads chatID's persisted chat and deserializes its
// SessionState, the common first step GetTodos/GetMode/SetMode all need.
func (s *AgentService) loadChatSession(ctx context.Context, chatID string) (*agent.Session, error) {
	chat, err := s.repository.Get(ctx, chatID)
	if err != nil {
		return nil, fmt.Errorf("agent service: loading chat %q: %w", chatID, err)
	}
	session, err := harness.LoadSession(chat)
	if err != nil {
		return nil, fmt.Errorf("agent service: loading session state for chat %q: %w", chatID, err)
	}
	return session, nil
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
//
// session is chat's already-deserialized session (see LoadSession) — used
// here only to read the chat's current mode (Design §3.8) before the tool
// list is assembled, so planMode's tool restriction (see
// planModeMutatingTools) applies to *this* turn's tool list rather than
// lagging a turn behind.
func (s *AgentService) buildTopLevelAgent(ctx context.Context, chat *entities.Chat, session *agent.Session) (*agent.Agent, error) {
	def, ok := s.defs.Agents[chat.AgentName]
	if !ok {
		return nil, fmt.Errorf("agent service: chat %q references unknown agent %q", chat.ID, chat.AgentName)
	}

	provider, model, err := s.resolveProviderModel(def)
	if err != nil {
		return nil, err
	}

	mode := s.modeProvider.GetMode(agent.WithSession(session))

	toolList, err := s.buildTopLevelTools(ctx, def, mode)
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
		Chat:         chat,
		Repository:   s.repository,
		Provider:     provider,
		Model:        model,
		Config:       cfg,
		TodoProvider: s.todoProvider,
		ModeProvider: s.modeProvider,
	})
}

// buildTopLevelTools assembles def's own core tool list (restricted by mode,
// see buildTools) plus, for every *other* loaded agent definition, a
// tool/agenttool-wrapped subagent tool (FR9, Design §3.4) — split out from
// buildTopLevelAgent so the assembly logic itself (which agents get
// wrapped, and that def is never wrapped as its own subagent) is directly
// testable without needing to inspect a constructed *agent.Agent's
// internals, which the framework doesn't expose an accessor for.
func (s *AgentService) buildTopLevelTools(ctx context.Context, def agentsource.AgentDefinition, mode string) ([]tool.Tool, error) {
	toolList, err := s.buildTools(def, mode)
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
//
// Subagents are NOT restricted by the parent's plan/execute mode (mode="" —
// see buildTools/isPlanModeRestricted) and get none of Design §3.5-§3.8's
// other wiring either (no compaction/todo/agentmode/approvals) — see
// planModeMutatingTools' doc comment for the known limitation this implies,
// and this package's doc comment on why AgentService pragmatically keeps
// subagent construction on the direct impl/providers.New path rather than
// impl/harness.Build.
func (s *AgentService) buildSubagentAgent(ctx context.Context, def agentsource.AgentDefinition) (*agent.Agent, error) {
	provider, model, err := s.resolveProviderModel(def)
	if err != nil {
		return nil, err
	}
	toolList, err := s.buildTools(def, "")
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
func (s *AgentService) buildTools(def agentsource.AgentDefinition, mode string) ([]tool.Tool, error) {
	available, err := s.coreTools()
	if err != nil {
		return nil, err
	}

	if len(def.Tools) == 0 {
		result := make([]tool.Tool, 0, len(coreToolNames))
		for _, name := range coreToolNames {
			if isPlanModeRestricted(mode, name) {
				continue
			}
			if t, ok := available[name]; ok {
				result = append(result, t)
			}
		}
		return result, nil
	}

	result := make([]tool.Tool, 0, len(def.Tools))
	for _, name := range def.Tools {
		if isPlanModeRestricted(mode, name) {
			continue
		}
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
