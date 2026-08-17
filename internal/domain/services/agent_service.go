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
	"sort"
	"strings"
	"time"

	"github.com/microsoft/agent-framework-go/agent"
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

// coreToolNames lists Canopy's built-in tools (Design §3.2) in a fixed
// order, used both to build the "inherit everything" tool list (an
// AgentDefinition with no explicit Tools allowlist) deterministically and to
// validate an AgentDefinition's explicit allowlist. Eight names total: the
// original seven core tools, plus "Skill" (post-v0.1.0 addendum, Design
// §3.11/FR19). Like "WebSearch" (only constructed when a WebSearchBackend is
// configured), "Skill" is only actually constructed into coreTools'
// available map under a condition of its own — at least one skill loaded
// (see coreTools) — so its presence in this fixed name list matters for
// allowlist validation and MCP-name-collision avoidance (isCoreToolName)
// even on a deployment where it isn't currently offered.
var coreToolNames = []string{"Bash", "FileRead", "FileWrite", "FileSearch", "DirectoryList", "WebFetch", "WebSearch", "Skill"}

// isCoreToolName reports whether name is one of the seven core built-in tool
// names, used at NewAgentService construction time to keep a same-named MCP
// tool from shadowing a core tool (see AgentServiceConfig.MCPTools' doc
// comment).
func isCoreToolName(name string) bool {
	for _, coreName := range coreToolNames {
		if coreName == name {
			return true
		}
	}
	return false
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

	// Middlewares are appended to every constructed agent's
	// agent.Config.Middlewares — both the top-level, chat-bound agent
	// (buildTopLevelAgent) and every dynamically-dispatched subagent
	// (buildSubagentAgent). Nil/empty (the default) means no extra
	// middleware is attached, matching Design §3.10's "not required for v1
	// usage": AgentService has no opinion on what a middleware here does,
	// but the motivating case is cmd/canopy wiring in the optional
	// impl/tracing.Setup OpenTelemetry middleware (Plan Phase 7) only when
	// a caller has actually asked for tracing.
	Middlewares []agent.Middleware

	// MCPTools are the tools exposed by every successfully-connected MCP
	// server (Design §3.11, Requirements FR6/FR18), keyed by tool name —
	// typically the map a *mcpclient.Registry's Tools() method returns,
	// built via mcpclient.ConnectAll before AgentServiceConfig is
	// constructed. NewAgentService performs no I/O itself (see its own doc
	// comment), so connecting to MCP servers has to already be done by the
	// caller (cmd/canopy) before this config is built; AgentService only
	// reads this map at construction time and does not own the underlying
	// Registry's lifetime — the caller must still call Registry.Close
	// itself during shutdown. Nil or empty means no MCP tools are available
	// to any agent. A tool name colliding with one of Canopy's own core
	// tool names (coreToolNames) is dropped in favor of the core tool and
	// logged via Logger, since domain/services — not mcpclient — is where
	// that merge is meant to happen (see mcpclient.ConnectAll's own doc
	// comment).
	MCPTools map[string]tool.Tool

	// RecordLastAgent, when set (post-v0.1.0 addendum), is called with an
	// agent name every time StartChat or SetAgent successfully makes it the
	// active agent for some chat — the single choke point covering every
	// UI-level place "which agent is in use" can change (the top-level
	// picker, ctrl+n, ctrl+a), so cmd/canopy only needs to wire one callback
	// rather than hooking three separate TUI call sites. The real
	// implementation (impl/config.SaveLastAgent) persists it to a small
	// per-project state file so the next session can auto-resume the same
	// agent (cmd/canopy reads it back via impl/config.LoadLastAgent before
	// constructing the TUI's Model). Best-effort: a nil callback (the
	// default — every existing caller/test) is simply not invoked, and an
	// error it returns is logged (Logger, if set) rather than propagated —
	// remembering the last-used agent is a convenience, not something that
	// should ever fail an otherwise-successful StartChat/SetAgent call.
	RecordLastAgent func(agentName string) error

	// RecordLastModel, when set (post-v0.1.0 addendum), is called with a
	// ModelConfig.Name every time SetModel successfully makes it a chat's
	// model override — mirrors RecordLastAgent exactly, including its
	// best-effort contract (nil callback is simply not invoked; an error it
	// returns is logged, never propagated). The real implementation
	// (impl/config.SaveLastModel) persists it so a brand-new chat's default
	// model resolution (resolveModelName's defaultModel fallback) can pick
	// up the last model the user actually chose instead of always falling
	// back to providers.json's first-listed model — see cmd/canopy's
	// computeDefaultModel, which reads it back via impl/config.LoadLastModel
	// before constructing AgentServiceConfig.DefaultModel.
	RecordLastModel func(modelName string) error
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

	// recordLastAgent is AgentServiceConfig.RecordLastAgent, possibly nil
	// (see that field's doc comment).
	recordLastAgent func(agentName string) error

	// recordLastModel is AgentServiceConfig.RecordLastModel, possibly nil
	// (see that field's doc comment).
	recordLastModel func(modelName string) error

	// todoProvider is the single, shared agent/harness/todo instance (Design
	// §3.7) used both to wire every chat-bound agent's todos_* tools (via
	// harness.BuildParams.TodoProvider) and to answer GetTodos directly from
	// a chat's deserialized session, without needing a live *agent.Agent
	// mid-run — see that method's doc comment. One shared instance per
	// AgentService (not per-agent, per-run) because the provider is stateless
	// configuration; the actual state it reads/writes always lives on the
	// *agent.Session passed via agent.WithSession, keyed by a fixed
	// package-internal state key, so any same-configuration instance
	// reads/writes the same data.
	todoProvider *todo.Provider

	// mcpTools and mcpToolNames are AgentServiceConfig.MCPTools, filtered at
	// construction time to drop any name colliding with coreToolNames (see
	// AgentServiceConfig.MCPTools' doc comment) and, for mcpToolNames, sorted
	// once so buildTools appends MCP tools to an "inherit everything" list in
	// a deterministic order rather than Go's randomized map iteration order —
	// the same determinism concern mcpclient.ConnectAll's own doc comment
	// raises for its Registry.
	mcpTools     map[string]tool.Tool
	mcpToolNames []string

	// middlewares is AgentServiceConfig.Middlewares, attached to every
	// constructed agent.Config.Middlewares — see that field's doc comment.
	middlewares []agent.Middleware
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

	mcpTools := make(map[string]tool.Tool, len(cfg.MCPTools))
	mcpToolNames := make([]string, 0, len(cfg.MCPTools))
	for name, t := range cfg.MCPTools {
		if isCoreToolName(name) {
			if cfg.Logger != nil {
				cfg.Logger.Warn("mcp tool name collides with a core built-in tool name; the core tool wins", "tool", name)
			}
			continue
		}
		mcpTools[name] = t
		mcpToolNames = append(mcpToolNames, name)
	}
	sort.Strings(mcpToolNames)

	return &AgentService{
		defs:            cfg.Definitions,
		providers:       providerIndex,
		models:          modelIndex,
		defaultModel:    cfg.DefaultModel,
		repository:      cfg.Repository,
		toolsCfg:        cfg.Tools,
		logger:          cfg.Logger,
		recordLastAgent: cfg.RecordLastAgent,
		recordLastModel: cfg.RecordLastModel,
		middlewares:     cfg.Middlewares,
		mcpTools:        mcpTools,
		mcpToolNames:    mcpToolNames,
		todoProvider:    todo.New(nil),
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
	s.tryRecordLastAgent(agentName)
	return chat, nil
}

// tryRecordLastAgent calls recordLastAgent (AgentServiceConfig.
// RecordLastAgent, post-v0.1.0 addendum), if set, logging rather than
// propagating any error — see that field's doc comment for why this must
// never fail an otherwise-successful StartChat/SetAgent call.
func (s *AgentService) tryRecordLastAgent(agentName string) {
	if s.recordLastAgent == nil {
		return
	}
	if err := s.recordLastAgent(agentName); err != nil && s.logger != nil {
		s.logger.Warn("failed to record last-used agent", "agent", agentName, "error", err)
	}
}

// tryRecordLastModel calls recordLastModel (AgentServiceConfig.
// RecordLastModel, post-v0.1.0 addendum), if set, logging rather than
// propagating any error — mirrors tryRecordLastAgent exactly.
func (s *AgentService) tryRecordLastModel(modelName string) {
	if s.recordLastModel == nil {
		return
	}
	if err := s.recordLastModel(modelName); err != nil && s.logger != nil {
		s.logger.Warn("failed to record last-used model", "model", modelName, "error", err)
	}
}

// RunResult is AgentService.RunText/RunMessages' return value: the model's
// response plus a post-turn snapshot of the session-scoped state Design
// §3.7 tracks (todos) and the chat-scoped model routing state
// (post-v0.1.0 addendum, Design §4/FR1 — Chat.ModelOverride), so a caller
// (Plan Phase 6's TUI) doesn't need a second round-trip just to render the
// current todo list or active model name after every turn.
// Todos/Model reflect state as of immediately after this turn completed
// (i.e. after session persistence — see persistSession).
type RunResult struct {
	Response *agent.Response
	Todos    []todo.Item

	// Model is the ModelConfig.Name this turn was actually routed through —
	// chat.ModelOverride if set, otherwise the agent definition's own
	// "model" frontmatter or s.defaultModel (see resolveModelName/
	// activeModelName), the same value GetModel returns for this chat
	// immediately after the turn.
	Model string
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
// compaction's incremental index state, todo's tool-driven state
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

	a, err := s.buildTopLevelAgent(ctx, chat)
	if err != nil {
		return nil, err
	}

	var chatPatched bool
	msgs, chatPatched = withReconstructedApprovalFunctionCalls(chat, msgs)
	// Proactive repair (see removeOrphanedApprovalRequests' doc comment): a
	// chat already carrying an orphaned ToolApprovalRequestContent from a
	// *previous* turn's chained-auto-approval bug would otherwise fail
	// inside a.Run below on every single future turn — toolautocall's own
	// request/response validation runs before this function gets a chance
	// to repair anything after the fact. Repairing here too, before the
	// call, means an already-stuck chat self-heals on its very next turn
	// instead of staying permanently broken.
	if harness.RemoveOrphanedApprovalRequests(chat) {
		chatPatched = true
	}
	if chatPatched {
		if err := s.repository.Update(ctx, chat); err != nil {
			return nil, fmt.Errorf("agent service: persisting reconciled approval state for chat %q: %w", chatID, err)
		}
	}
	resp, err := a.Run(ctx, msgs, agent.WithSession(session)).Collect()
	if err != nil {
		return nil, fmt.Errorf("agent service: running chat %q: %w", chatID, err)
	}

	if err := s.persistSession(ctx, chatID, session); err != nil {
		return nil, err
	}

	if err := s.repairOrphanedApprovalRequests(ctx, chatID); err != nil {
		return nil, err
	}

	modelName, err := s.activeModelName(chat)
	if err != nil {
		return nil, err
	}

	return &RunResult{
		Response: resp,
		Todos:    s.todoProvider.GetAllItems(agent.WithSession(session)),
		Model:    modelName,
	}, nil
}

// RunTextStream is RunText's streaming counterpart — a thin wrapper around
// RunMessagesStream for the common "one new user text message" case. See
// RunMessagesStream's doc comment for the full streaming contract.
func (s *AgentService) RunTextStream(ctx context.Context, chatID, msg string) (agent.ResponseStream, func() (*RunResult, error), error) {
	return s.RunMessagesStream(ctx, chatID, []*message.Message{message.NewText(msg)})
}

// RunMessagesStream is RunMessages' streaming counterpart (Requirements FR8,
// Design §5): instead of blocking until the turn is over and handing back a
// fully-materialized *RunResult, it hands the caller (Plan Phase 6's TUI) the
// underlying agent.ResponseStream itself, so *agent.ResponseUpdate values can
// be rendered incrementally as they arrive — while still performing exactly
// the same post-run session persistence RunMessages does (persistSession,
// unchanged and shared, not reimplemented — see RunMessages' own doc comment
// for why that persistence has to be a deliberate second write) and still
// making the same final Todos/Response available once the turn is over.
//
// # Why this returns a (stream, finalize) pair instead of just a stream
//
// RunMessages can return *RunResult synchronously because
// a.Run(...).Collect() already blocks until the whole run — including
// persistSession's own network/disk I/O, since that happens after Collect
// returns — is complete. A streaming caller can't have that: the entire
// point is that the caller drives when updates get consumed, by ranging over
// the returned agent.ResponseStream itself (`for update, err := range
// stream`). Because agent.ResponseStream is a push-style iter.Seq2 (Design
// §3.1 — Go's range-over-func compiles that loop into a single call to the
// stream function, passing it the loop body as a callback), everything the
// wrapping stream function below does — including forwarding each update
// and, once the underlying provider run is exhausted, calling
// persistSession — runs synchronously nested inside the caller's own range
// loop, on the caller's own goroutine. There is no separate goroutine here
// for a second return value to block on; the *RunResult a full turn produces
// simply doesn't exist yet at the moment RunMessagesStream returns. So:
//
//   - stream: forwards every (*agent.ResponseUpdate, error) pair from
//     a.Run(...) to the caller completely unchanged, while also folding each
//     one into a local *agent.Response using that type's own exported
//     Update method — the same method agent.ResponseStream.Collect() itself
//     calls per update (see agent/response.go). This is not a reimplemented,
//     divergent copy of Collect's accumulation logic; it's the identical
//     Update/Coalesce calls Collect makes, just interleaved with forwarding
//     to the caller instead of hidden entirely inside Collect. Once the
//     inner stream is exhausted (the underlying range loop ends with no
//     error), it Coalesce()s the accumulated Response and calls
//     s.persistSession — the exact function RunMessages calls, so both
//     entry points persist session state identically and neither can drift
//     from the other.
//   - finalize: a func the caller calls once, after fully draining stream
//     (a `for range` that runs to completion rather than breaking early),
//     which returns the same *RunResult shape RunMessages returns. Calling
//     finalize before stream has been fully drained returns an error rather
//     than blocking, since — again — there's no background goroutine to
//     wait on; the result plainly doesn't exist yet.
//
// If the caller's range loop exits early — either because an update carried
// a non-nil error and the caller stopped, or because the caller simply
// `break`s out for its own reasons — persistSession is never reached, and
// finalize returns an error. This mirrors RunMessages, which likewise never
// reaches persistSession if a.Run(...).Collect() itself returns an error.
// The one new edge case streaming introduces is a caller-initiated early
// break for a reason *other* than an error (e.g. a cancelled turn): that
// silently skips persistence too, so a caller that wants a partial turn's
// tool-approval/todo state persisted must still drain the stream fully
// rather than abandoning it partway through.
func (s *AgentService) RunMessagesStream(ctx context.Context, chatID string, msgs []*message.Message) (agent.ResponseStream, func() (*RunResult, error), error) {
	chat, err := s.repository.Get(ctx, chatID)
	if err != nil {
		return nil, nil, fmt.Errorf("agent service: loading chat %q: %w", chatID, err)
	}

	session, err := harness.LoadSession(chat)
	if err != nil {
		return nil, nil, fmt.Errorf("agent service: loading session state for chat %q: %w", chatID, err)
	}

	a, err := s.buildTopLevelAgent(ctx, chat)
	if err != nil {
		return nil, nil, err
	}

	msgs, chatPatched := withReconstructedApprovalFunctionCalls(chat, msgs)
	// Proactive repair — see the identical block in RunMessages for why
	// this has to run before a.Run, not only after, to let an already-stuck
	// chat self-heal on its next turn (removeOrphanedApprovalRequests' doc
	// comment has the full empirical finding).
	if harness.RemoveOrphanedApprovalRequests(chat) {
		chatPatched = true
	}
	if chatPatched {
		if err := s.repository.Update(ctx, chat); err != nil {
			return nil, nil, fmt.Errorf("agent service: persisting reconciled approval state for chat %q: %w", chatID, err)
		}
	}
	inner := a.Run(ctx, msgs, agent.WithSession(session))
	state := &streamRunState{}

	stream := agent.ResponseStream(func(yield func(*agent.ResponseUpdate, error) bool) {
		var resp agent.Response
		for update, updateErr := range inner {
			if updateErr != nil {
				state.err = fmt.Errorf("agent service: running chat %q: %w", chatID, updateErr)
				state.done = true
				yield(update, updateErr)
				return
			}
			resp.Update(update)
			if !yield(update, nil) {
				// Caller stopped early (see doc comment above): no
				// persistence happens, matching RunMessages' behavior when
				// a.Run(...).Collect() never returns a result.
				return
			}
		}
		resp.Coalesce()

		if persistErr := s.persistSession(ctx, chatID, session); persistErr != nil {
			state.err = persistErr
			state.done = true
			return
		}

		if repairErr := s.repairOrphanedApprovalRequests(ctx, chatID); repairErr != nil {
			state.err = repairErr
			state.done = true
			return
		}

		modelName, modelErr := s.activeModelName(chat)
		if modelErr != nil {
			state.err = modelErr
			state.done = true
			return
		}

		state.result = &RunResult{
			Response: &resp,
			Todos:    s.todoProvider.GetAllItems(agent.WithSession(session)),
			Model:    modelName,
		}
		state.done = true
	})

	finalize := func() (*RunResult, error) {
		if !state.done {
			return nil, fmt.Errorf("agent service: finalize called for chat %q before its stream was fully drained", chatID)
		}
		if state.err != nil {
			return nil, state.err
		}
		return state.result, nil
	}

	return stream, finalize, nil
}

// withReconstructedApprovalFunctionCalls works around a confirmed bug in
// agent-framework-go's agent/harness/toolautocall package: when a turn's new
// msgs carry a *message.ToolApprovalResponseContent or
// *message.AlwaysApproveToolApprovalResponseContent (i.e. this is a
// follow-up turn answering an approval request surfaced by a *previous*
// RunMessages/RunMessagesStream call — Design §3.6), toolautocall.Run
// reconstructs a *message.FunctionResultContent for the approved/rejected
// call and appends it to the messages sent to the underlying provider, but
// never re-adds a matching *message.FunctionCallContent alongside it — see
// toolautocall.go's processToolApprovalResponses/invokeApprovedToolApprovalResponses:
// the reconstructed assistant message (built by convertToToolCallContentMessages)
// is only used for preDownstreamCallHistory (yielded to the caller for
// display) and is never merged back into the messages slice actually passed
// to next() (the provider). This was confirmed empirically by dumping the
// raw HTTP request body a real approval-then-continue turn produces: for
// both the OpenAI and Gemini providers, the second turn's request contains a
// "tool" role message with the function's result but no preceding
// "assistant" message naming the function call it answers — a malformed
// request by both APIs' own contract (a tool-result message must follow a
// matching assistant tool-call message).
//
// OpenAI's/Anthropic's own client libraries are lenient enough (or Canopy's
// test doubles didn't validate strictly enough) that this went unnoticed;
// google.golang.org/genai's provider does its own client-side validation
// before ever making a network call (provider/geminiprovider/agent.go's
// buildRequestParts scans history for a *message.FunctionCallContent whose
// CallID matches each *message.FunctionResultContent, since Gemini's
// FunctionResponse wire format requires the function *name*, which
// FunctionResultContent alone doesn't carry) — so with Gemini specifically,
// every approval-gated tool call (every MCP tool, unconditionally, since
// mcpclient.Connect wraps all of them with tool.ApprovalRequiredFunc; Bash
// and FileWrite too) fails immediately with "geminiprovider: missing
// function name for result with call ID ..." the moment a pending approval
// is answered, whether by an explicit user decision or by a standing
// "always approve" rule auto-replaying it on a later turn.
//
// The fix: before handing msgs to a.Run, scan them for any approval-response
// content and, for each one whose snapshotted ToolCall is a
// *message.FunctionCallContent, synthesize a plain assistant message
// carrying that FunctionCallContent (marked InformationalOnly since it's
// replayed context, not a new call to invoke) and prepend it. This new
// message is not itself a ToolApprovalRequestContent/ResponseContent, so
// toolautocall's own extraction leaves it untouched (its default case just
// keeps unrecognized content types as-is) — it simply becomes ordinary
// history a downstream provider's own callID/name reconciliation (Gemini's
// callIDToName scan, in particular) can find, restoring the assistant
// tool-call message every provider's wire protocol expects to precede a
// tool-result message.
//
// A *message.ToolApprovalResponseContent already snapshots its originating
// *message.FunctionCallContent onto its own ToolCall field at creation time
// (see ToolApprovalRequestContent.CreateResponse in agent-framework-go's
// message/content.go), so this reconstruction needs no separate lookup
// against chat history — the data is already on the response content itself.
//
// # A second, real-Gemini-specific wrinkle: empty CallIDs
//
// google.golang.org/genai's FunctionCall.ID is documented Optional, and the
// real Gemini API routinely omits it entirely — unlike OpenAI's
// tool_call_id, which is always present, and unlike every fake-Gemini-server
// fixture in this package's tests (before this fix), which always hand-set a
// fake "id" real traffic never actually sends. agent-framework-go's
// geminiprovider parses that missing field as
// *message.FunctionCallContent.CallID == "", and its own buildRequestParts
// unconditionally errors on a *message.FunctionResultContent whose CallID is
// "" — captured live: a real canopy binary, talking to the real Gemini API,
// through a real stdio MCP server, the moment a pending MCP tool approval
// was answered ("geminiprovider: missing function name for result with call
// ID \"\""; see TestGeminiEmptyCallID_ApproveThenContinue for the fast,
// deterministic repro this evidence produced). Not MCP-specific — any
// approval-gated tool (Bash/FileWrite too) hits the same error the moment a
// real Gemini response omits the call ID.
//
// Naively minting a synthetic CallID only for the *response* side here (an
// earlier version of this fix did exactly that) doesn't work: toolautocall's
// own request/response reconciliation
// (extractAndRemoveToolApprovalRequestsAndResponses in agent-framework-go)
// separately compares the *ToolApprovalRequestContent* from turn N-1
// (already persisted to chat history with CallID == "" by the time turn N
// runs — chat storage round-trips through JSON, so there is no shared Go
// object to mutate in place across turns) against turn N's response, and
// fails loudly ("ToolApprovalRequestContent found with ToolCall.CallID(s)
// ” that have no matching ToolApprovalResponseContent") the instant the two
// sides disagree — empirically confirmed against
// TestGeminiEmptyCallID_ApproveThenContinue. So the empty CallID has to be
// repaired on *both* sides, in lockstep: this turn's response AND the
// matching historical request sitting in chat.Messages. chat's
// ToolApprovalRequestContent entries are matched to this turn's responses by
// RequestID (a plain string, stable across the JSON round-trip, unlike
// CallID-based identity) rather than object identity, and are mutated
// in-place on the *entities.Chat the caller already loaded — the caller
// (RunMessages/RunMessagesStream) must persist chat via
// interfaces.ChatRepository.Update *before* calling a.Run when this function
// reports chatPatched, so ChatHistoryProvider.Invoking's own independent
// reload of the chat sees the repaired CallID rather than a stale
// still-empty one.
func withReconstructedApprovalFunctionCalls(chat *entities.Chat, msgs []*message.Message) (out []*message.Message, chatPatched bool) {
	historicalRequestsByID := make(map[string]*message.FunctionCallContent)
	if chat != nil {
		for _, m := range chat.Messages {
			if m == nil {
				continue
			}
			for _, c := range m.Contents {
				req, ok := c.(*message.ToolApprovalRequestContent)
				if !ok || req == nil {
					continue
				}
				if fcc, ok := req.ToolCall.(*message.FunctionCallContent); ok && fcc != nil {
					historicalRequestsByID[req.RequestID] = fcc
				}
			}
		}
	}

	var calls []message.Content
	seen := make(map[*message.FunctionCallContent]bool)
	var syntheticCallIDCounter int
	addCall := func(toolCall message.ToolCallContent, requestID string) {
		fcc, ok := toolCall.(*message.FunctionCallContent)
		if !ok || fcc == nil || seen[fcc] {
			return
		}
		seen[fcc] = true
		if fcc.CallID == "" {
			// Post gemini_transport.go (internal/impl/providers): this
			// branch is no longer reachable for any *newly received* Gemini
			// function call — the transport now patches a synthetic,
			// non-empty id into the raw HTTP response before genai's
			// decoder (and therefore this code) ever sees it, so a
			// ToolApprovalRequestContent built from a fresh response always
			// already has fcc.CallID != "" by the time it would reach here.
			// It remains reachable, and is still needed, for exactly one
			// real case: a chat persisted to disk *before* the transport
			// fix existed can already contain a ToolApprovalRequestContent
			// snapshot with CallID == "" (chat storage round-trips through
			// JSON, so there's no way to retroactively repair
			// already-persisted history just by shipping a new binary) — if
			// a user resumes that chat and answers the still-pending
			// approval today, CreateResponse clones that same empty CallID
			// onto the new response, and this branch is what repairs it.
			// See TestWithReconstructedApprovalFunctionCalls_EmptyCallID_PreFixPersistedChat
			// (reconstructed_approval_calls_test.go) for a fast,
			// transport-independent repro of that exact scenario, and
			// TestWithReconstructedApprovalFunctionCalls_NonEmptyCallID_BranchNotTriggered
			// confirming this branch stays inert once a CallID is already
			// present (the post-fix steady state). Kept intentionally, not
			// dead code: mint one synthetic ID and apply it to both this
			// response's own snapshot and the matching historical request's
			// snapshot in chat.Messages (if found), so toolautocall's
			// request/response reconciliation still agrees — see this
			// function's doc comment for why both sides need it.
			if hist, found := historicalRequestsByID[requestID]; found && hist.CallID != "" {
				fcc.CallID = hist.CallID
			} else {
				syntheticCallIDCounter++
				fcc.CallID = fmt.Sprintf("canopy-synth-%d", syntheticCallIDCounter)
				if found {
					hist.CallID = fcc.CallID
					chatPatched = true
				}
			}
		}
		clone := *fcc
		clone.InformationalOnly = true
		calls = append(calls, &clone)
	}

	for _, m := range msgs {
		if m == nil {
			continue
		}
		for _, c := range m.Contents {
			switch resp := c.(type) {
			case *message.ToolApprovalResponseContent:
				if resp != nil {
					addCall(resp.ToolCall, resp.RequestID)
				}
			case *message.AlwaysApproveToolApprovalResponseContent:
				if resp != nil && resp.InnerResponse != nil {
					addCall(resp.InnerResponse.ToolCall, resp.InnerResponse.RequestID)
				}
			}
		}
	}

	if len(calls) == 0 {
		return msgs, chatPatched
	}
	synthetic := &message.Message{Role: message.RoleAssistant, Contents: calls}
	return append([]*message.Message{synthetic}, msgs...), chatPatched
}

// repairOrphanedApprovalRequests reloads chatID fresh from the repository
// (the caller's own in-memory *entities.Chat is stale by this point — see
// harness.RemoveOrphanedApprovalRequests' doc comment for the full
// empirical finding behind this repair, including why
// impl/harness.ChatHistoryProvider.Invoked *also* runs the same repair on
// every individual persist, not only here) and persists the repair if
// harness.RemoveOrphanedApprovalRequests actually finds and drops
// anything. A no-op, not an error, when there's nothing to repair — this
// runs after every single turn, so the common case (no orphaned request at
// all) must stay
// cheap and silent.
func (s *AgentService) repairOrphanedApprovalRequests(ctx context.Context, chatID string) error {
	chat, err := s.repository.Get(ctx, chatID)
	if err != nil {
		return fmt.Errorf("agent service: loading chat %q: %w", chatID, err)
	}
	if !harness.RemoveOrphanedApprovalRequests(chat) {
		return nil
	}
	if err := s.repository.Update(ctx, chat); err != nil {
		return fmt.Errorf("agent service: persisting orphaned-approval-request repair for chat %q: %w", chatID, err)
	}
	return nil
}

// streamRunState carries RunMessagesStream's post-run outcome from its
// wrapping stream closure to the paired finalize func it returns alongside
// that stream — see RunMessagesStream's doc comment. Not safe for concurrent
// use: both the returned stream and finalize are meant to be driven from the
// same goroutine, in order (range the stream fully, then call finalize
// exactly once).
type streamRunState struct {
	done   bool
	result *RunResult
	err    error
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

// ListModels returns every configured ModelConfig.Name, sorted (post-v0.1.0
// addendum, Design §4/FR1) — a caller (Plan Phase 6's TUI's ctrl+o
// model-picker overlay) uses this to present the set of valid choices
// SetModel will accept. Sorted for the same "deterministic over Go's
// randomized map iteration order" reason NewAgentService sorts mcpToolNames.
func (s *AgentService) ListModels() []string {
	names := make([]string, 0, len(s.models))
	for name := range s.models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ModelSummary is one entry in ListModelSummaries' result: a configured
// model's Name plus its per-million-token request/response cost
// (post-v0.1.0 addendum), mirroring SkillSummary's role for the ctrl+s
// skills browser — used by the TUI's ctrl+o model-picker overlay so a user
// can compare cost before switching, without needing the full
// entities.ModelConfig (Parameters, provider linkage, etc.) that isn't
// relevant to that picker.
type ModelSummary struct {
	Name                       string
	InputCostPerMillionTokens  float64
	OutputCostPerMillionTokens float64
}

// ListModelSummaries returns every configured model's ModelSummary, sorted
// by name (post-v0.1.0 addendum) — the richer counterpart to ListModels for
// callers (the ctrl+o picker) that want to display cost alongside the name,
// not just the name alone that SetModel's validation needs.
func (s *AgentService) ListModelSummaries() []ModelSummary {
	names := s.ListModels()
	result := make([]ModelSummary, 0, len(names))
	for _, name := range names {
		model := s.models[name]
		result = append(result, ModelSummary{
			Name:                       model.Name,
			InputCostPerMillionTokens:  model.InputCostPerMillionTokens,
			OutputCostPerMillionTokens: model.OutputCostPerMillionTokens,
		})
	}
	return result
}

// GetModel returns chat's currently *active* model name (post-v0.1.0
// addendum, Design §4/FR1): chat.ModelOverride if set, otherwise whatever
// resolveProviderModel would fall back to (the agent definition's own
// "model" frontmatter, or s.defaultModel) — the caller (the TUI sidebar)
// wants to know what's actually in effect for this chat's next turn, not
// merely whether an override happens to be set.
func (s *AgentService) GetModel(ctx context.Context, chatID string) (string, error) {
	chat, err := s.repository.Get(ctx, chatID)
	if err != nil {
		return "", fmt.Errorf("agent service: loading chat %q: %w", chatID, err)
	}
	return s.activeModelName(chat)
}

// SetModel sets chat's per-chat model override (Chat.ModelOverride,
// post-v0.1.0 addendum, Design §4/FR1) and persists it immediately — a
// user-initiated switch doesn't need to wait for a run to take effect or to
// be saved. modelName must name an already-configured ModelConfig; this is
// a real, defensive check even though the TUI's model picker (ListModels)
// only ever offers valid choices.
func (s *AgentService) SetModel(ctx context.Context, chatID, modelName string) error {
	if _, ok := s.models[modelName]; !ok {
		return fmt.Errorf("agent service: unknown model %q", modelName)
	}
	chat, err := s.repository.Get(ctx, chatID)
	if err != nil {
		return fmt.Errorf("agent service: loading chat %q: %w", chatID, err)
	}
	chat.ModelOverride = modelName
	chat.UpdatedAt = time.Now().UTC()
	if err := s.repository.Update(ctx, chat); err != nil {
		return fmt.Errorf("agent service: setting model override for chat %q: %w", chatID, err)
	}
	s.tryRecordLastModel(modelName)
	return nil
}

// activeModelName resolves chat's currently active model name — the shared
// "chat.ModelOverride, else the agent definition's own model:, else
// s.defaultModel" precedence (see resolveModelName) used by both GetModel's
// read-only view and RunMessages/RunMessagesStream's RunResult.Model, kept
// as one function so the two never drift from resolveProviderModel's own
// resolution (which buildTopLevelAgent actually routes a turn through).
func (s *AgentService) activeModelName(chat *entities.Chat) (string, error) {
	def, ok := s.defs.Agents[chat.AgentName]
	if !ok {
		return "", fmt.Errorf("agent service: chat %q references unknown agent %q", chat.ID, chat.AgentName)
	}
	name := resolveModelName(def, chat.ModelOverride, s.defaultModel)
	if name == "" {
		return "", fmt.Errorf("agent service: agent %q has no model configured and no default model is set", def.Name)
	}
	return name, nil
}

// ListAgents returns every loaded agent definition's Name, sorted via
// agentsource.SortNames (post-v0.1.0 addendum, Design §3.4/§5: Canopy's own
// default agents first, in SDLC pipeline order, then everything else
// alphabetically) — mirrors ListModels for the same "a TUI picker needs a
// deterministic list of valid choices" reason, here for the ctrl+a in-chat
// agent-switch overlay rather than the top-level picker screen (which
// sources its list directly from the agentsource-loaded map it's
// constructed with, not through AgentService — see NewModel/
// Model.agentNames, which calls the same agentsource.SortNames).
func (s *AgentService) ListAgents() []string {
	names := make([]string, 0, len(s.defs.Agents))
	for name := range s.defs.Agents {
		names = append(names, name)
	}
	agentsource.SortNames(names)
	return names
}

// SetAgent switches chat's bound agent definition (post-v0.1.0 addendum,
// Design §3.4/§5) without starting a new chat: unlike StartChat, this does
// NOT touch chat.Messages or chat.SessionState — the same chat ID and its
// entire conversation history carry over unchanged, only chat.AgentName
// changes. The *next* turn is driven by the new agent's system
// prompt/tools because buildTopLevelAgent resolves chat.AgentName fresh
// from the reloaded chat on every RunMessages/RunMessagesStream call — there
// is no cached *agent.Agent this needs to invalidate. agentName must
// reference an already-loaded agent definition, the same validation
// StartChat itself already performs, and the same defensive posture
// SetModel applies against s.models.
func (s *AgentService) SetAgent(ctx context.Context, chatID, agentName string) error {
	if _, ok := s.defs.Agents[agentName]; !ok {
		return fmt.Errorf("agent service: unknown agent %q", agentName)
	}
	chat, err := s.repository.Get(ctx, chatID)
	if err != nil {
		return fmt.Errorf("agent service: loading chat %q: %w", chatID, err)
	}
	chat.AgentName = agentName
	chat.UpdatedAt = time.Now().UTC()
	if err := s.repository.Update(ctx, chat); err != nil {
		return fmt.Errorf("agent service: switching agent for chat %q: %w", chatID, err)
	}
	s.tryRecordLastAgent(agentName)
	return nil
}

// ChatSummary is one entry in ListChatSummaries' result (post-v0.1.0
// addendum, Design §5's addendum — the ctrl+h history browser): enough to
// render a picker row (Title, falling back to a formatted date when empty —
// tui.historyPickerItem's job, not this type's) and to resume the chat (ID)
// without needing the full entities.Chat (message history, session state)
// up front.
type ChatSummary struct {
	ID        string
	Title     string
	AgentName string
	UpdatedAt time.Time
}

// ListChatSummaries returns every persisted chat as a ChatSummary, most
// recently updated first (post-v0.1.0 addendum) — the ctrl+h history
// browser's source list, and also what cmd/canopy's --continue flag
// consults (summaries[0].ID) to find the single most recent chat to resume.
func (s *AgentService) ListChatSummaries(ctx context.Context) ([]ChatSummary, error) {
	chats, err := s.repository.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent service: listing chats: %w", err)
	}
	summaries := make([]ChatSummary, 0, len(chats))
	for _, chat := range chats {
		summaries = append(summaries, ChatSummary{
			ID:        chat.ID,
			Title:     chat.Title,
			AgentName: chat.AgentName,
			UpdatedAt: chat.UpdatedAt,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].UpdatedAt.After(summaries[j].UpdatedAt)
	})
	return summaries, nil
}

// GetChat returns the full persisted entities.Chat for chatID (post-v0.1.0
// addendum). Unlike every other AgentService accessor (GetTodos/
// GetModel), which deliberately expose one derived piece of state each,
// this is the one place the TUI needs the raw Messages too: resuming a chat
// (ctrl+h/--continue, tui.resumeChatCmd) has to reconstruct a transcript
// from prior history, which no existing accessor returns.
func (s *AgentService) GetChat(ctx context.Context, chatID string) (*entities.Chat, error) {
	chat, err := s.repository.Get(ctx, chatID)
	if err != nil {
		return nil, fmt.Errorf("agent service: loading chat %q: %w", chatID, err)
	}
	return chat, nil
}

// titleGenerationTextLimit caps how much of each message's text is fed into
// the title-generation prompt (buildTitlePrompt), so an unusually long
// first exchange doesn't balloon token usage for what's meant to be a
// cheap, throwaway call.
const titleGenerationTextLimit = 2000

// GenerateChatTitle generates a short, human-readable title for chatID from
// its first exchange (post-v0.1.0 addendum, Design §5's addendum — the
// ctrl+h history browser), using the same provider/model the chat itself is
// configured to use (resolveProviderModel, honoring chat.ModelOverride —
// the generated title's cost/quality tracks whatever the user is actually
// paying for and using), and persists it onto the chat's Title field. The
// TUI is expected to call this once, shortly after a chat's first turn
// completes (chatModel's titleAttempted guard) — but this method itself is
// idempotent-safe to call again: it always makes a fresh generation call
// and overwrites whatever Title (if any) is already stored, rather than
// skipping when one exists.
//
// Deliberately not built via buildTopLevelAgent/harness.Build: a title-
// generation call needs no tools, no ChatHistoryProvider, no compaction/
// todo wiring — providers.New directly (the same minimal-construction
// path buildSubagentAgent already uses) is enough, and skips all of that
// overhead for a single throwaway completion.
//
// On any failure — no messages yet, provider error, or an empty/unusable
// response — no partial state is written (chat.Title is left exactly as it
// was) and the error is returned for the caller to log; the history
// browser's own date fallback (services.ChatSummary/tui.historyPickerItem)
// covers this case, so a title-generation failure is never user-facing as
// an error.
func (s *AgentService) GenerateChatTitle(ctx context.Context, chatID string) (string, error) {
	title, err := s.generateChatTitle(ctx, chatID)
	if err != nil && s.logger != nil {
		s.logger.Warn("failed to generate chat title", "chat", chatID, "error", err)
	}
	return title, err
}

func (s *AgentService) generateChatTitle(ctx context.Context, chatID string) (string, error) {
	chat, err := s.repository.Get(ctx, chatID)
	if err != nil {
		return "", fmt.Errorf("agent service: loading chat %q: %w", chatID, err)
	}
	def, ok := s.defs.Agents[chat.AgentName]
	if !ok {
		return "", fmt.Errorf("agent service: chat %q references unknown agent %q", chatID, chat.AgentName)
	}
	provider, model, err := s.resolveProviderModel(def, chat.ModelOverride)
	if err != nil {
		return "", err
	}

	prompt := buildTitlePrompt(chat.Messages)
	if prompt == "" {
		return "", fmt.Errorf("agent service: chat %q has no messages to generate a title from yet", chatID)
	}

	titleAgent, err := providers.New(ctx, provider, model, agent.Config{Name: "canopy-title-generator", Logger: s.logger})
	if err != nil {
		return "", fmt.Errorf("agent service: building title-generation agent for chat %q: %w", chatID, err)
	}
	resp, err := titleAgent.RunText(ctx, prompt).Collect()
	if err != nil {
		return "", fmt.Errorf("agent service: generating title for chat %q: %w", chatID, err)
	}

	title := sanitizeTitle(resp.String())
	if title == "" {
		return "", fmt.Errorf("agent service: model returned an empty title for chat %q", chatID)
	}

	// Reload rather than reusing chat from the top of this function, same
	// reasoning as persistSession: apply the Title write on top of whatever
	// the actual turn (which may well have completed after this function
	// started resolving the title, in a real caller) has since persisted,
	// rather than clobbering it with a stale in-memory copy. UpdatedAt is
	// deliberately left untouched — generating a title is not user
	// activity, and touching it here would skew --continue/ctrl+h's
	// "most recently updated" ordering by however long generation took.
	fresh, err := s.repository.Get(ctx, chatID)
	if err != nil {
		return "", fmt.Errorf("agent service: reloading chat %q to persist its title: %w", chatID, err)
	}
	fresh.Title = title
	if err := s.repository.Update(ctx, fresh); err != nil {
		return "", fmt.Errorf("agent service: persisting title for chat %q: %w", chatID, err)
	}
	return title, nil
}

// buildTitlePrompt builds GenerateChatTitle's prompt from messages' first
// user message and first assistant reply (each independently — a message
// list can interleave tool-call/tool-result messages with no usable text in
// between, so this doesn't assume the first two messages are the pair).
// Returns "" when there's no user message with any text yet (nothing to
// title), the one condition GenerateChatTitle treats as a real error rather
// than attempting a call.
func buildTitlePrompt(messages []*message.Message) string {
	var userText, assistantText string
	for _, m := range messages {
		if m == nil {
			continue
		}
		text := strings.TrimSpace(m.String())
		if text == "" {
			continue
		}
		switch m.Role {
		case message.RoleUser:
			if userText == "" {
				userText = truncateForTitlePrompt(text)
			}
		case message.RoleAssistant:
			if assistantText == "" {
				assistantText = truncateForTitlePrompt(text)
			}
		}
		if userText != "" && assistantText != "" {
			break
		}
	}
	if userText == "" {
		return ""
	}

	var b strings.Builder
	b.WriteString("Generate a short, plain-text title (4-6 words, no quotes, no trailing punctuation) summarizing what this conversation is about. Reply with ONLY the title, nothing else.\n\n")
	b.WriteString("User: " + userText + "\n")
	if assistantText != "" {
		b.WriteString("Assistant: " + assistantText + "\n")
	}
	return b.String()
}

// truncateForTitlePrompt caps s at titleGenerationTextLimit runes.
func truncateForTitlePrompt(s string) string {
	r := []rune(s)
	if len(r) <= titleGenerationTextLimit {
		return s
	}
	return string(r[:titleGenerationTextLimit]) + "…"
}

// maxGeneratedTitleLen bounds a generated title's length as a safety net
// against a model ignoring buildTitlePrompt's "4-6 words" instruction —
// truncated, not rejected, since a too-long-but-otherwise-good title is
// still far more useful in the history browser than falling back to a bare
// date.
const maxGeneratedTitleLen = 80

// sanitizeTitle cleans a raw model response into a display-ready title:
// trims whitespace and any wrapping quote characters a model might add
// despite being asked not to, collapses embedded newlines/repeated
// whitespace into single spaces (a picker row is one line), and caps
// length. Returns "" if nothing usable remains, which GenerateChatTitle
// treats as a failure.
func sanitizeTitle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'“”‘’")
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxGeneratedTitleLen {
		s = string(r[:maxGeneratedTitleLen])
	}
	return s
}

// loadChatSession loads chatID's persisted chat and deserializes its
// SessionState, the common first step GetTodos needs.
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
func (s *AgentService) buildTopLevelAgent(ctx context.Context, chat *entities.Chat) (*agent.Agent, error) {
	def, ok := s.defs.Agents[chat.AgentName]
	if !ok {
		return nil, fmt.Errorf("agent service: chat %q references unknown agent %q", chat.ID, chat.AgentName)
	}

	provider, model, err := s.resolveProviderModel(def, chat.ModelOverride)
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
		Middlewares: s.middlewares,
	}

	return harness.Build(ctx, harness.BuildParams{
		Chat:         chat,
		Repository:   s.repository,
		Provider:     provider,
		Model:        model,
		Config:       cfg,
		TodoProvider: s.todoProvider,
	})
}

// buildTopLevelTools assembles def's own core tool list (see buildTools)
// plus, for every *other* loaded agent definition, a
// tool/agenttool-wrapped subagent tool (FR9, Design §3.4) — split out from
// buildTopLevelAgent so the assembly logic itself (which agents get
// wrapped, and that def is never wrapped as its own subagent) is directly
// testable without needing to inspect a constructed *agent.Agent's
// internals, which the framework doesn't expose an accessor for.
//
// def's own tool list still fails loudly (the buildTools call above): a
// user directly selecting an agent whose own tools: allowlist references
// something unavailable deserves an immediate, clear error. But a failure
// building one of the *other* agents as a subagent-dispatch tool is
// deliberately non-fatal here — logged as a warning and skipped, not
// propagated — for the same reason a broken MCP server doesn't stop Canopy
// from starting (Requirements §7): one misconfigured or under-configured
// agent definition (e.g. one whose tools: names WebSearch/Skill without
// the corresponding backend/skills actually available — see coreTools'
// conditional construction) must not make every *other* agent unusable
// just because this loop eagerly tries to wrap it. Before this fix, it
// did: any agent's construction — including "general"'s — would fail
// outright the moment any other loaded agent had an unavailable tool in
// its allowlist, which is a much larger blast radius than the one broken
// agent actually deserves.
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
			if s.logger != nil {
				s.logger.Warn("skipping subagent-dispatch tool for a misconfigured agent",
					"agent", name, "parent", def.Name, "error", err)
			}
			continue
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
// Subagents get none of Design §3.5-§3.7's other harness wiring either (no
// compaction/todo/approvals) — see this package's doc comment on why
// AgentService pragmatically keeps subagent construction on the direct
// impl/providers.New path rather than impl/harness.Build.
//
// Subagents also do NOT honor the parent chat's Chat.ModelOverride
// (post-v0.1.0 addendum, Design §4/FR1): resolveProviderModel is called here
// with an empty override, the same "no override" resolution a top-level
// agent gets when its chat has never called SetModel. This is a deliberate
// choice, not an oversight — Design §3.4's whole point is subagent context
// isolation (a subagent runs in its own fresh, unpersisted session with no
// Chat of its own, see above), and a parent chat's model switch is exactly
// the kind of chat-scoped state that isolation says shouldn't leak into a
// dispatched subagent's own construction.
func (s *AgentService) buildSubagentAgent(ctx context.Context, def agentsource.AgentDefinition) (*agent.Agent, error) {
	provider, model, err := s.resolveProviderModel(def, "")
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
		Middlewares: s.middlewares,
	}
	return providers.New(ctx, provider, model, cfg)
}

// resolveModelName applies the shared "which model" precedence (Design §4,
// post-v0.1.0 per-chat override addendum, FR1): override (a chat's
// Chat.ModelOverride), when non-empty, wins outright; otherwise def.Model
// (the agent definition's own "model" frontmatter); otherwise defaultModel
// (AgentServiceConfig.DefaultModel). An empty return means none of the three
// resolved to anything — the caller decides how to report that (see
// resolveProviderModel/activeModelName, both of which turn it into an
// error). Split out as a pure function (no AgentService method receiver,
// nothing but string comparisons) so resolveProviderModel (routing a real
// turn) and activeModelName (GetModel/RunResult.Model's read-only view) can
// share the exact same precedence without one silently drifting from the
// other.
func resolveModelName(def agentsource.AgentDefinition, override, defaultModel string) string {
	if override != "" {
		return override
	}
	if def.Model != "" {
		return def.Model
	}
	return defaultModel
}

// resolveProviderModel resolves def's provider/model against the flat JSON
// config (Design §4): override (chat.ModelOverride, post-v0.1.0 addendum —
// empty for a subagent, which never receives one, see buildSubagentAgent's
// doc comment on why subagents stay isolated from the parent chat's
// override), when set, wins outright; otherwise def.Model, when set, names a
// ModelConfig; otherwise s.defaultModel is used (resolveModelName). The
// named ModelConfig's Provider then names a ProviderConfig.
func (s *AgentService) resolveProviderModel(def agentsource.AgentDefinition, override string) (entities.ProviderConfig, entities.ModelConfig, error) {
	modelName := resolveModelName(def, override, s.defaultModel)
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
// projectcontext's own doc comment ordering — and, post-v0.1.0 addendum,
// appends the skills catalog (skillsCatalog) last: level 1 of §3.11/FR19's
// progressive disclosure, every loaded skill's name+description (never its
// full Body — see the Skill tool, NewSkillTool, for levels 2/3), so the
// model always knows what's available without spending context on a body it
// may never need. Ordering — project instructions, then agent instructions,
// then the skills catalog — puts the catalog where it reads naturally as an
// appendix rather than interrupting either instruction block. When neither
// ProjectInstructions nor def.Instructions nor the skills catalog has
// anything to contribute, this returns "", matching this function's
// pre-addendum behavior exactly (see agent_service_test.go's "returns just
// the agent's instructions when project instructions are empty" case).
func (s *AgentService) buildInstructions(def agentsource.AgentDefinition) string {
	parts := make([]string, 0, 3)
	if s.defs.ProjectInstructions != "" {
		parts = append(parts, s.defs.ProjectInstructions)
	}
	if def.Instructions != "" {
		parts = append(parts, def.Instructions)
	}
	if catalog := s.skillsCatalog(); catalog != "" {
		parts = append(parts, catalog)
	}
	return strings.Join(parts, "\n\n")
}

// skillsCatalog renders §3.11/FR19's level-1 progressive-disclosure list:
// every loaded skill's name+description, one per line, under a
// "## Available Skills" heading — never the Body (see NewSkillTool for
// levels 2/3, which return a specific skill's Body/supporting file only
// when the model actually asks for it by name). Returns "" when no skills
// are loaded, so buildInstructions never adds an empty, useless
// "## Available Skills" header to an agent's prompt (Design's own
// instruction: "only add the section when skills exist").
func (s *AgentService) skillsCatalog() string {
	if len(s.defs.Skills) == 0 {
		return ""
	}
	names := make([]string, 0, len(s.defs.Skills))
	for name := range s.defs.Skills {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("## Available Skills\n\n")
	for _, name := range names {
		skill := s.defs.Skills[name]
		fmt.Fprintf(&b, "- %s: %s\n", skill.Name, skill.Description)
	}
	return strings.TrimRight(b.String(), "\n")
}

// SkillSummary is one entry in ListSkills' result: a loaded skill's
// Name/Description (level 1 of Design §3.11/FR19's progressive disclosure —
// the same two fields skillsCatalog renders into the system prompt), used by
// the TUI's ctrl+s skills-browser overlay (post-v0.1.0 addendum) to list
// what's available without needing every skill's full Body up front.
type SkillSummary struct {
	Name        string
	Description string
}

// ListSkills returns every loaded skill's Name/Description, sorted by name
// (post-v0.1.0 addendum, Design §3.11/FR19) — mirrors ListAgents/ListModels
// for the same "a TUI picker needs a deterministic list of valid choices"
// reason, here for the ctrl+s skills-browser overlay rather than an
// in-place chat switch.
func (s *AgentService) ListSkills() []SkillSummary {
	names := make([]string, 0, len(s.defs.Skills))
	for name := range s.defs.Skills {
		names = append(names, name)
	}
	sort.Strings(names)

	result := make([]SkillSummary, 0, len(names))
	for _, name := range names {
		skill := s.defs.Skills[name]
		result = append(result, SkillSummary{Name: skill.Name, Description: skill.Description})
	}
	return result
}

// GetSkillBody returns the full SKILL.md body for the named skill
// (post-v0.1.0 addendum, Design §3.11/FR19) — the exact same content the
// model-facing Skill tool (level 2, NewSkillTool) returns, exposed here
// read-only so the TUI's ctrl+s browser can show a user what a skill
// actually does without going through the model at all. Returns a clear
// error for an unknown name, the same defensive posture SetAgent/SetModel
// apply against their own name-lookup maps.
func (s *AgentService) GetSkillBody(name string) (string, error) {
	skill, ok := s.defs.Skills[name]
	if !ok {
		return "", fmt.Errorf("agent service: unknown skill %q", name)
	}
	return skill.Body, nil
}

// buildTools assembles def's tool list from the core built-in set (Design
// §3.2) plus every connected MCP server's tools (Design §3.11, Requirements
// FR6/FR18, s.mcpTools — populated at NewAgentService construction time from
// AgentServiceConfig.MCPTools, itself built by the caller via
// mcpclient.ConnectAll before construction; see that field's doc comment):
// every core and MCP tool when def.Tools is empty ("tools" frontmatter
// omitted means inherit everything, per agentsource's doc comment), or
// exactly the named subset otherwise — an MCP tool's plain name (as its
// server documents it, not namespaced; see mcpclient.ConnectAll's doc
// comment) is a valid allowlist entry exactly like a core tool's name.
func (s *AgentService) buildTools(def agentsource.AgentDefinition) ([]tool.Tool, error) {
	available, err := s.coreTools()
	if err != nil {
		return nil, err
	}
	for name, t := range s.mcpTools {
		available[name] = t
	}

	if len(def.Tools) == 0 {
		result := make([]tool.Tool, 0, len(coreToolNames)+len(s.mcpToolNames))
		for _, name := range coreToolNames {
			if t, ok := available[name]; ok {
				result = append(result, t)
			}
		}
		for _, name := range s.mcpToolNames {
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

// coreTools constructs one instance of each configured built-in tool
// (Design §3.2), keyed by the name used in an AgentDefinition's "tools"
// frontmatter allowlist. WebSearch is omitted when no WebSearchBackend is
// configured (see ToolsConfig.WebSearchBackend's doc comment); Skill is
// omitted when no skills are loaded (post-v0.1.0 addendum, Design
// §3.11/FR19) — an agent/project with no skills shouldn't get a tool that
// offers nothing to look up, the same reasoning WebSearch already applies.
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
	if len(s.defs.Skills) > 0 {
		result["Skill"] = tools.NewSkillTool(s.defs.Skills)
	}
	return result, nil
}
