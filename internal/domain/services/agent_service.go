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
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/agent/harness/todo"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/tool"

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
var coreToolNames = []string{"Bash", "FileRead", "FileWrite", "FileEdit", "FileSearch", "DirectoryList", "WebFetch", "WebSearch", "Skill"}

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

	// AgentModels optionally pins an agent name to a ModelConfig.Name
	// (Phase 11 addendum, config.ProvidersFile.AgentModels) — resolved
	// between chat.ModelOverride and the agent definition's own "model"
	// frontmatter (see resolveActiveModelName). A name not present in
	// Models is not a construction-time error: it's checked, and silently
	// falls through to the next tier (with a Logger warning, if set), the
	// first time that agent actually needs its model resolved.
	AgentModels map[string]string

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
	agentModels  map[string]string
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
		agentModels:     cfg.AgentModels,
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
	workingDir := s.toolsCfg.WorkingRoot
	if workingDir == "" {
		// No confinement configured — fall back to the process's own
		// working directory, the same "empty WorkingRoot means bash's
		// default cwd" convention ProjectFiles already applies. Best
		// effort: an error here just means Chat.WorkingDir stays empty,
		// the same as any chat persisted before this field existed.
		if abs, err := os.Getwd(); err == nil {
			workingDir = abs
		}
	}
	now := time.Now().UTC()
	chat := &entities.Chat{
		ID:         chatID,
		AgentName:  agentName,
		WorkingDir: workingDir,
		CreatedAt:  now,
		UpdatedAt:  now,
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

	// Usage is the chat's resulting entities.Chat.Usage (post-v0.1.0
	// addendum: the TUI sidebar's usage/cost display) immediately after
	// this turn's persistSession call — every model this chat's history
	// has ever used (including subagent dispatches), not just Model
	// above, accumulated across the chat's whole lifetime, not just this
	// one turn.
	Usage map[string]entities.ModelUsage
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
// Unlike RunText, callers can supply arbitrary message content here, not
// just plain text — Plan Phase 6's TUI is the intended caller for that path.
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
// approach (one write covers messages + session state together), but
// HistoryProvider.Invoked only ever persists chat.Messages — it never
// touches chat.SessionState — so an explicit second write is required
// regardless of what else is in the middleware chain: compaction's
// incremental index state and todo's tool-driven state both mutate the
// *agent.Session during the provider call, and nothing else in the request
// path ever flushes that mutated Session back to the persisted Chat.
//
// The fix is this explicit second write: by the time a.RunText(...).Collect()
// returns to this function, the *agent.Session object reflects every
// mutation the turn made to it, so persistSession writes it out.
// persistSession reloads the chat (rather than reusing the chat variable
// from the top of this function) so it applies the SessionState write on
// top of whatever Messages ChatHistoryProvider.Invoked already persisted,
// instead of clobbering them with a stale in-memory copy. This is a real,
// deliberate double-write per turn (chat.Messages via Invoked, then
// chat.SessionState via persistSession) — noted here explicitly rather than
// left implicit.
func (s *AgentService) RunMessages(ctx context.Context, chatID string, msgs []*message.Message) (*RunResult, error) {
	chat, err := s.repository.Get(ctx, chatID)
	if err != nil {
		return nil, fmt.Errorf("agent service: loading chat %q: %w", chatID, err)
	}

	modelName, err := s.activeModelName(chat)
	if err != nil {
		return nil, err
	}

	session, err := harness.LoadSession(chat)
	if err != nil {
		return nil, fmt.Errorf("agent service: loading session state for chat %q: %w", chatID, err)
	}

	collector := &usageCollector{}
	ctx = withUsageCollector(ctx, collector)

	a, err := s.buildTopLevelAgent(ctx, chat)
	if err != nil {
		return nil, err
	}

	resp, err := a.Run(ctx, msgs, agent.WithSession(session)).Collect()
	if err != nil {
		if errors.Is(err, context.Canceled) {
			s.persistCancelledUserMessages(chatID, msgs)
		}
		return nil, fmt.Errorf("agent service: running chat %q: %w", chatID, err)
	}
	collector.record(modelName, resp.Usage())

	usage, err := s.persistSession(ctx, chatID, session, collector)
	if err != nil {
		return nil, err
	}

	return &RunResult{
		Response: resp,
		Todos:    s.todoProvider.GetAllItems(agent.WithSession(session)),
		Model:    modelName,
		Usage:    usage,
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

	modelName, err := s.activeModelName(chat)
	if err != nil {
		return nil, nil, err
	}

	session, err := harness.LoadSession(chat)
	if err != nil {
		return nil, nil, fmt.Errorf("agent service: loading session state for chat %q: %w", chatID, err)
	}

	collector := &usageCollector{}
	ctx = withUsageCollector(ctx, collector)

	a, err := s.buildTopLevelAgent(ctx, chat)
	if err != nil {
		return nil, nil, err
	}

	inner := a.Run(ctx, msgs, agent.WithSession(session))
	state := &streamRunState{}

	stream := agent.ResponseStream(func(yield func(*agent.ResponseUpdate, error) bool) {
		var resp agent.Response
		for update, updateErr := range inner {
			if updateErr != nil {
				state.err = fmt.Errorf("agent service: running chat %q: %w", chatID, updateErr)
				state.done = true
				if errors.Is(updateErr, context.Canceled) {
					s.persistCancelledUserMessages(chatID, msgs)
				}
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
		collector.record(modelName, resp.Usage())

		usage, persistErr := s.persistSession(ctx, chatID, session, collector)
		if persistErr != nil {
			state.err = persistErr
			state.done = true
			return
		}

		state.result = &RunResult{
			Response: &resp,
			Todos:    s.todoProvider.GetAllItems(agent.WithSession(session)),
			Model:    modelName,
			Usage:    usage,
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

// persistCancelledUserMessages best-effort persists msgs — the new user
// message(s) a turn was started with — directly onto chatID's stored
// Messages, bypassing ChatHistoryProvider.Invoked entirely. Called only
// from RunMessages/RunMessagesStream's error path, and only when that
// error is (or wraps) context.Canceled.
//
// Why this exists: ChatHistoryProvider.Invoked is a deliberate no-op
// whenever the run it's reporting on failed (invoked.Err != nil — see that
// method's own doc comment), matching agent-framework-go's own default
// history provider behavior for a genuine failure. A user-initiated
// cancellation (tui's "esc" mid-turn, cancelTurn) surfaces through
// a.Run(...) as exactly that same kind of failure — context.Canceled — so
// without this, the user's own prompt for the cancelled turn was never
// persisted at all: a real report was a chat resumed via --continue
// ("--continue" itself worked, and did reattach the right chat) simply
// missing the message the user had just sent before cancelling, as if it
// had never been typed.
//
// Deliberately narrow in scope: this persists only the user's own new
// message(s), not any partial assistant text or tool calls the turn had
// already produced before cancellation. Persisting those safely would mean
// reconstructing a consistent message sequence out of a deliberately
// interrupted one — e.g. a function call with no matching function result,
// which most provider APIs reject on the next turn — for comparatively
// little benefit (resuming just re-reads whatever files it already read;
// nothing is lost, only repeated). Preserving the question itself, so it
// isn't silently dropped, is the actual bug being fixed here.
func (s *AgentService) persistCancelledUserMessages(chatID string, msgs []*message.Message) {
	if len(msgs) == 0 {
		return
	}
	// context.Background(), not the turn's own ctx: that ctx is the one
	// that just got cancelled, and this is cleanup bookkeeping after the
	// turn has already ended, not part of the cancelled operation itself.
	ctx := context.Background()
	chat, err := s.repository.Get(ctx, chatID)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("failed to load chat to persist a cancelled turn's message", "chatID", chatID, "error", err)
		}
		return
	}
	for _, m := range msgs {
		chat.Messages = append(chat.Messages, m.Clone())
	}
	chat.UpdatedAt = time.Now().UTC()
	if err := s.repository.Update(ctx, chat); err != nil && s.logger != nil {
		s.logger.Warn("failed to persist a cancelled turn's message", "chatID", chatID, "error", err)
	}
}

// persistSession serializes session and writes it onto the chat's
// SessionState field, reloading the chat first so this write doesn't
// clobber a concurrent Messages update (see RunMessages' doc comment for
// why this is a deliberate second write, not merged into
// ChatHistoryProvider.Invoked's own Update call). It also merges collector's
// accumulated token usage (the top-level call this turn made, plus every
// subagent dispatch — see usageTrackingSubagentTool) into the same reload-
// and-write cycle, onto chat.Usage, rather than a separate Update call —
// one combined write per turn for everything this turn needs to persist
// beyond Messages (which ChatHistoryProvider.Invoked already handles on
// its own schedule).
//
// Returns the chat's resulting Usage map (post-merge) so callers can hand
// it straight to the caller in RunResult without a second repository read
// — this function already has the authoritative post-write value in hand.
func (s *AgentService) persistSession(ctx context.Context, chatID string, session *agent.Session, collector *usageCollector) (map[string]entities.ModelUsage, error) {
	data, err := harness.SerializeSession(session)
	if err != nil {
		return nil, fmt.Errorf("agent service: serializing session state for chat %q: %w", chatID, err)
	}
	chat, err := s.repository.Get(ctx, chatID)
	if err != nil {
		return nil, fmt.Errorf("agent service: reloading chat %q to persist session state: %w", chatID, err)
	}
	chat.SessionState = data
	mergeUsage(chat, collector.snapshot(), s.models)
	chat.UpdatedAt = time.Now().UTC()
	if err := s.repository.Update(ctx, chat); err != nil {
		return nil, fmt.Errorf("agent service: persisting session state for chat %q: %w", chatID, err)
	}
	return chat.Usage, nil
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
// resolveProviderModel would fall back to (the project-level AgentModels
// pin, the agent definition's own "model" frontmatter, or s.defaultModel)
// — the caller (the TUI sidebar) wants to know what's actually in effect
// for this chat's next turn, not merely whether an override happens to be
// set.
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
// be saved. Deliberately chat-wide rather than scoped to whichever agent
// is active when ctrl+o is pressed: switching agents afterward (SetAgent)
// keeps using this same override, per explicit user feedback that a
// per-agent scoping scheme (briefly tried) was confusing — "most of the
// time I will want one model" regardless of which agent is active.
// modelName must name an already-configured ModelConfig; this is a real,
// defensive check even though the TUI's model picker (ListModels) only
// ever offers valid choices.
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
// "chat.ModelOverride, else s.agentModels[agent], else the agent
// definition's own model:, else s.defaultModel" precedence (see
// resolveActiveModelName) used by both GetModel's read-only view and
// RunMessages/RunMessagesStream's RunResult.Model, kept as one function so
// the two never drift from resolveProviderModel's own resolution (which
// buildTopLevelAgent actually routes a turn through).
func (s *AgentService) activeModelName(chat *entities.Chat) (string, error) {
	def, ok := s.defs.Agents[chat.AgentName]
	if !ok {
		return "", fmt.Errorf("agent service: chat %q references unknown agent %q", chat.ID, chat.AgentName)
	}
	name := s.resolveActiveModelName(def, chat.ModelOverride)
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
//
// Does NOT touch chat.ModelOverride (an earlier revision briefly made
// this clear it, then an even later revision tried scoping it per agent
// instead — both reworked back to this simpler behavior per explicit user
// feedback): the override is chat-wide, so switching agents keeps using
// whatever model the user last picked via ctrl+o, same as if they'd never
// switched. A chat that has never called SetModel still resolves the
// newly-selected agent's own AgentModels pin / "model" frontmatter /
// s.defaultModel exactly as before, since there's no override to apply in
// that case.
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
// tui.historyPickerItem's job, not this type's; WorkingDir, so a user can
// tell which project a listed chat belongs to, since chats are persisted
// in one global, non-project-scoped store) and to resume the chat (ID)
// without needing the full entities.Chat (message history, session state)
// up front.
type ChatSummary struct {
	ID         string
	Title      string
	AgentName  string
	WorkingDir string
	UpdatedAt  time.Time
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
			ID:         chat.ID,
			Title:      chat.Title,
			AgentName:  chat.AgentName,
			WorkingDir: chat.WorkingDir,
			UpdatedAt:  chat.UpdatedAt,
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

// DeleteChat permanently removes chatID's persisted history (post-v0.1.0
// addendum: "d" in ctrl+h's history browser). It returns an error if
// chatID doesn't exist. The caller is responsible for not deleting a chat
// that's still active (tui.chatModel's own pickerHistory handling guards
// against that) — there's nothing at this layer that's still running a
// turn against chatID to interrupt, so this is a plain, unconditional
// repository delete.
func (s *AgentService) DeleteChat(ctx context.Context, chatID string) error {
	if err := s.repository.Delete(ctx, chatID); err != nil {
		return fmt.Errorf("agent service: deleting chat %q: %w", chatID, err)
	}
	return nil
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
// agent definition wrapped via newUsageTrackingSubagentTool and added to
// its tool list as a dynamically-dispatchable subagent (FR9, Design §3.4).
// Subagents
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
// newUsageTrackingSubagentTool-wrapped subagent tool (FR9, Design §3.4) —
// split out from
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
	return append(toolList, s.buildSubagentDispatchTools(ctx, def)...), nil
}

// buildSubagentDispatchTools returns the subagent-dispatch tool (FR9,
// Design §3.4) for every other loaded agent definition that builds
// successfully — split out of buildTopLevelTools (post-v0.1.0 addendum:
// ListTools/ctrl+t tools browser) so both it and ListTools derive the
// identical set of subagent tools from one place, rather than two copies
// that could silently drift apart and make the ctrl+t browser's tool count
// disagree with the sidebar's own Tools: N (AgentCapabilities.ToolCount),
// which is exactly buildTopLevelTools' own len(toolList). See
// buildTopLevelTools' doc comment for why a failure building one of the
// *other* agents here is non-fatal (logged and skipped) rather than
// propagated.
func (s *AgentService) buildSubagentDispatchTools(ctx context.Context, def agentsource.AgentDefinition) []tool.Tool {
	var result []tool.Tool
	for name, subDef := range s.defs.Agents {
		if name == def.Name {
			continue
		}
		subAgent, subModelName, err := s.buildSubagentAgent(ctx, subDef)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("skipping subagent-dispatch tool for a misconfigured agent",
					"agent", name, "parent", def.Name, "error", err)
			}
			continue
		}
		result = append(result, newUsageTrackingSubagentTool(subAgent, subModelName))
	}
	return result
}

// buildSubagentAgent constructs a *agent.Agent for def with the core tool
// set but no ChatHistoryProvider and no further subagent-dispatch tools, so
// it runs with the framework's default in-memory history provider — which,
// since newUsageTrackingSubagentTool invokes it without a WithSession
// option (same as tool/agenttool, the stock wrapper it replaces), starts a
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
// The returned model name is def's resolved entities.ModelConfig.Name
// (post-v0.1.0 addendum: usageTrackingSubagentTool tags every dispatch's
// token usage with it, since a subagent can resolve to a different model
// than its parent, or than another subagent — this is the only place that
// resolution happens, so callers that need it (buildTopLevelTools) get it
// for free rather than resolving it a second time).
func (s *AgentService) buildSubagentAgent(ctx context.Context, def agentsource.AgentDefinition) (*agent.Agent, string, error) {
	provider, model, err := s.resolveProviderModel(def, "")
	if err != nil {
		return nil, "", err
	}
	toolList, err := s.buildTools(def)
	if err != nil {
		return nil, "", err
	}
	cfg := agent.Config{
		Name:        def.Name,
		Description: def.Description,
		Tools:       toolList,
		RunOptions:  []agent.Option{agent.WithInstructions(s.buildInstructions(def))},
		Logger:      s.logger,
		Middlewares: s.middlewares,
	}
	a, err := providers.New(ctx, provider, model, cfg)
	if err != nil {
		return nil, "", err
	}
	return a, model.Name, nil
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

// resolveActiveModelName resolves def's active model name, layering Phase
// 11's project-level AgentModels pin between override and the rest of
// resolveModelName's precedence: override (chat.ModelOverride) wins outright
// when set; otherwise s.agentModels[def.Name], when it names a model that's
// actually configured, wins next; otherwise def.Model, then s.defaultModel
// (resolveModelName handles that last pair unchanged). A pinned name that
// isn't in s.models is deliberately not an error here — see
// AgentServiceConfig.AgentModels' doc comment on why this must fail soft —
// it's logged (if s.logger is set) and treated the same as an unset pin.
//
// Shared by resolveProviderModel (routing a real turn) and activeModelName
// (GetModel/RunResult.Model's read-only view) so the two never drift apart —
// the same reasoning resolveModelName's own doc comment already states for
// the tiers below this one.
func (s *AgentService) resolveActiveModelName(def agentsource.AgentDefinition, override string) string {
	if override != "" {
		return override
	}
	if pinned, ok := s.agentModels[def.Name]; ok && pinned != "" {
		if _, known := s.models[pinned]; known {
			return pinned
		}
		if s.logger != nil {
			s.logger.Warn("agent_models pins agent to an unconfigured model; falling back to the agent's own model/default", "agent", def.Name, "model", pinned)
		}
	}
	return resolveModelName(def, "", s.defaultModel)
}

// resolveProviderModel resolves def's provider/model against the flat JSON
// config (Design §4): override (chat.ModelOverride, post-v0.1.0 addendum —
// empty for a subagent, which never receives one, see buildSubagentAgent's
// doc comment on why subagents stay isolated from the parent chat's
// override), when set, wins outright; otherwise s.agentModels[def.Name]
// (Phase 11 addendum), then def.Model, then s.defaultModel is used
// (resolveActiveModelName). The named ModelConfig's Provider then names a
// ProviderConfig.
func (s *AgentService) resolveProviderModel(def agentsource.AgentDefinition, override string) (entities.ProviderConfig, entities.ModelConfig, error) {
	modelName := s.resolveActiveModelName(def, override)
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

// AgentCapabilities summarizes what's available to one loaded agent
// definition (post-v0.1.0 addendum: the TUI sidebar's "N tools, M skills"
// line). ToolCount mirrors exactly what a real turn wires in
// (buildTopLevelTools: core tools filtered by the agent's own tools:
// allowlist if it set one, plus MCP tools, plus one subagent-dispatch tool
// per other loaded agent that builds successfully — a misconfigured
// subagent candidate is skipped there and so is skipped here too, for the
// same reason). SkillCount is zero unless ToolCount's list actually
// includes the Skill tool: skillsCatalog's system-prompt appendix is
// unconditional, but the skills it lists are only reachable — and this
// count should only be non-zero — when the model can actually call the
// tool that acts on them.
type AgentCapabilities struct {
	ToolCount  int
	SkillCount int
}

// AgentCapabilities resolves agentName's AgentCapabilities. Returns an
// error for an unknown agent name or a failure building its own (not a
// subagent candidate's — see ToolCount's doc comment) tool list, the same
// failure modes buildTopLevelTools itself can hit.
func (s *AgentService) AgentCapabilities(ctx context.Context, agentName string) (AgentCapabilities, error) {
	def, ok := s.defs.Agents[agentName]
	if !ok {
		return AgentCapabilities{}, fmt.Errorf("agent service: unknown agent %q", agentName)
	}
	toolList, err := s.buildTopLevelTools(ctx, def)
	if err != nil {
		return AgentCapabilities{}, err
	}
	caps := AgentCapabilities{ToolCount: len(toolList)}
	for _, t := range toolList {
		if t.Name() == "Skill" {
			caps.SkillCount = len(s.defs.Skills)
			break
		}
	}
	return caps, nil
}

// ToolSummary is one entry in ListTools' result: a tool's Name/Description
// plus Source, which is "Built-in" for one of Canopy's own tools
// (coreToolNames), "MCP" for one exposed by a connected MCP server, or
// "Subagent" for a dispatch-to-another-agent tool (buildSubagentDispatchTools)
// — used by the TUI's ctrl+t tools-browser overlay (post-v0.1.0 addendum)
// to show a user what's actually available without asking the model.
type ToolSummary struct {
	Name        string
	Description string
	Source      string
}

// ListTools returns agentName's complete effective tool set — built-in,
// MCP, and subagent-dispatch — sorted by name and labeled by
// ToolSummary.Source. This is deliberately the exact same set
// buildTopLevelTools assembles for a real turn (built-in/MCP via
// buildTools/resolveToolNames, subagent-dispatch tools via
// buildSubagentDispatchTools), so len(ListTools(...)) always equals
// AgentCapabilities.ToolCount for the same agentName — a ctrl+t browser
// that listed fewer tools than the sidebar's own "Tools: N" would be
// actively confusing, telling a user something is available that the
// browser then can't show them.
func (s *AgentService) ListTools(ctx context.Context, agentName string) ([]ToolSummary, error) {
	def, ok := s.defs.Agents[agentName]
	if !ok {
		return nil, fmt.Errorf("agent service: unknown agent %q", agentName)
	}

	available, err := s.coreTools()
	if err != nil {
		return nil, err
	}
	for name, t := range s.mcpTools {
		available[name] = t
	}

	names, err := s.resolveToolNames(def, available)
	if err != nil {
		return nil, err
	}

	mcpSet := make(map[string]bool, len(s.mcpToolNames))
	for _, name := range s.mcpToolNames {
		mcpSet[name] = true
	}

	result := make([]ToolSummary, 0, len(names)+len(s.defs.Agents))
	for _, name := range names {
		source := "Built-in"
		if mcpSet[name] {
			source = "MCP"
		}
		result = append(result, ToolSummary{Name: name, Description: available[name].Description(), Source: source})
	}
	for _, t := range s.buildSubagentDispatchTools(ctx, def) {
		result = append(result, ToolSummary{Name: t.Name(), Description: t.Description(), Source: "Subagent"})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
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

// fileListingSizeBound caps how many files ProjectFiles returns. No user has
// ever once typed "@" and then scrolled past a couple screens of candidates —
// on a genuinely huge tree, a bound keeps both the walk and the picker's
// own filter pass proportional to real use, at the same price FileSearch's
// maxDefault already pays for its own result set; the (fuzzy) filter still
// ranks within the bound.
const fileListingSizeBound = 10000

// ProjectFiles lists the project's regular files as project-root-relative
// paths, sorted alphabetically and capped at fileListingSizeBound — the
// source behind the TUI's @-mention file-reference picker (docs/PLAN.md's
// Phase 12 @-mention story): confined to ToolsConfig.WorkingRoot the
// way FileRead/FileSearch/DirectoryList already are (that's what "the
// same project root" is), so the paths it returns are exactly ones the
// FileRead/FileWrite tools will accept. .git is skipped, same as
// FileSearch's own walk; the picker itself (the TUI, not this service)
// does the fuzzy filtering.
func (s *AgentService) ProjectFiles() ([]string, error) {
	root := s.toolsCfg.WorkingRoot
	if root == "" {
		// No confinement configured — list from the process's working
		// directory instead, matching every other root-relative behavior
		// this service has for an empty WorkingRoot (bash's default cwd).
		abs, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("failed to list project files: %w", err)
		}
		root = abs
	}
	var files []string
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return fmt.Errorf("failed to relativize %q: %w", path, relErr)
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("failed to list project files: %w", walkErr)
	}
	sort.Strings(files)
	if len(files) > fileListingSizeBound {
		files = files[:fileListingSizeBound]
	}
	return files, nil
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

	names, err := s.resolveToolNames(def, available)
	if err != nil {
		return nil, err
	}
	result := make([]tool.Tool, 0, len(names))
	for _, name := range names {
		result = append(result, available[name])
	}
	return result, nil
}

// resolveToolNames returns def's ordered tool name keys into available (a
// coreTools()-plus-s.mcpTools map, built identically by both of this
// method's callers, buildTools and ListTools) — every core tool name
// (coreToolNames order) actually present in available, then every MCP
// tool name (s.mcpToolNames order), when def.Tools is empty ("tools"
// frontmatter omitted means inherit everything); or exactly def.Tools
// itself otherwise, erroring on the first name that isn't in available.
// Split out of buildTools (post-v0.1.0 addendum, ListTools/ctrl+t tools
// browser) so both it and ListTools resolve the identical name set from
// one place rather than two copies silently drifting apart — buildTools
// then resolves each name to its actual tool.Tool, and ListTools keeps the
// name itself (the config-facing key a "tools:" allowlist entry and
// Design's own docs use, e.g. "Bash" — not every core tool's
// provider-facing tool.Tool.Name(), e.g. "run_shell" for Bash's underlying
// shelltool.Local, which a user browsing ctrl+t wouldn't recognize).
func (s *AgentService) resolveToolNames(def agentsource.AgentDefinition, available map[string]tool.Tool) ([]string, error) {
	if len(def.Tools) == 0 {
		names := make([]string, 0, len(coreToolNames)+len(s.mcpToolNames))
		for _, name := range coreToolNames {
			if _, ok := available[name]; ok {
				names = append(names, name)
			}
		}
		names = append(names, s.mcpToolNames...)
		return names, nil
	}

	for _, name := range def.Tools {
		if _, ok := available[name]; !ok {
			return nil, fmt.Errorf("agent service: agent %q references unknown or unavailable tool %q", def.Name, name)
		}
	}
	return def.Tools, nil
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
		"FileEdit":      tools.NewFileEditTool(tools.FileEditConfig{Root: s.toolsCfg.WorkingRoot}),
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
