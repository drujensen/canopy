package harness

import (
	"context"
	"slices"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/agent/harness/agentmode"
	"github.com/microsoft/agent-framework-go/agent/harness/todo"
	"github.com/microsoft/agent-framework-go/agent/harness/toolapproval"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/domain/interfaces"
	"github.com/drujensen/canopy/internal/impl/providers"
)

// BuildParams supplies everything Build needs to construct a *agent.Agent
// bound to one persisted Chat.
type BuildParams struct {
	// Chat is the persisted conversation this agent's history round-trips
	// through. Chat.ID must already exist in Repository (see
	// ChatHistoryProvider's doc comment).
	Chat *entities.Chat

	// Repository backs the ChatHistoryProvider this function wires in.
	Repository interfaces.ChatRepository

	// Provider and Model select which provider/model construct the agent
	// (Design §4); passed through to impl/providers.New unchanged.
	// Model.ContextWindowTokens (when set) sizes the compaction trigger
	// (Design §3.5) this function attaches.
	Provider entities.ProviderConfig
	Model    entities.ModelConfig

	// Config is the caller-assembled agent configuration (name, description,
	// tools, instructions via RunOptions, logger, etc — Design §3.1).
	// Config.HistoryProvider is overwritten with a ChatHistoryProvider bound
	// to Chat/Repository regardless of what the caller sets, since a
	// chat-bound agent's history is this package's whole job.
	// Config.ContextProviders/Config.Middlewares are extended (not
	// overwritten) with the Design §3.5-§3.8 loop-quality wiring — see
	// WireLoopQuality.
	Config agent.Config

	// TodoProvider and ModeProvider are the shared agent/harness/todo and
	// agent/harness/agentmode instances to wire into Config.ContextProviders
	// (Design §3.7/§3.8). The caller (domain/services.AgentService) owns
	// these instances so the exact same ones back both the agent's own
	// todos_*/mode_* tools and any read-only, no-live-agent-needed session
	// inspection the caller does itself (see AgentService.GetTodos/GetMode).
	// Nil skips wiring that provider in — mainly useful for tests that don't
	// need the full four-feature surface.
	TodoProvider *todo.Provider
	ModeProvider *agentmode.Provider
}

// Build constructs a *agent.Agent wired with a ChatHistoryProvider bound to
// params.Chat/params.Repository (Design §3.9), the Design §3.5-§3.8
// loop-quality providers/middleware (WireLoopQuality), and delegates the
// rest of construction to impl/providers.New, which already wires
// agent/harness/toolautocall via the per-provider constructors (see this
// package's doc comment for why agent/harness/loop is intentionally not
// added here).
func Build(ctx context.Context, params BuildParams) (*agent.Agent, error) {
	cfg := WireLoopQuality(params)
	cfg.HistoryProvider = NewChatHistoryProvider(params.Chat.ID, params.Repository)
	return providers.New(ctx, params.Provider, params.Model, cfg)
}

// WireLoopQuality attaches Phase 5's four loop-quality features to
// params.Config and returns the resulting agent.Config, without touching
// history or constructing anything provider-specific — split out from Build
// so the exact middleware/context-provider composition it produces is
// directly testable (see toolapproval_ordering_test.go) against a fake
// agent.ProviderConfig.Run, without needing a real provider client.
//
//   - Compaction (Design §3.5/FR10): agent/compaction.NewContextProvider,
//     appended to Config.ContextProviders.
//   - Todo (Design §3.7/FR11) and mode (Design §3.8/FR12): params.TodoProvider
//     and params.ModeProvider, appended to Config.ContextProviders in that
//     order (after compaction, so compaction acts on the raw conversation
//     before todo/mode inject their own per-turn instructional messages —
//     otherwise those freshly-injected messages could be immediately
//     compacted away on the same turn they were added).
//   - Approvals (Design §3.6/FR5): agent/harness/toolapproval.New, prepended
//     to Config.Middlewares. It must land in Config.Middlewares (which wraps
//     an agent's entire invoke lifecycle, including history/context
//     providers) rather than ProviderConfig.Middlewares (which wraps only
//     the provider Run call, and is where every per-provider constructor
//     already places agent/harness/toolautocall per Design §3.3's own
//     finding): toolautocall lives inside ProviderConfig.Middlewares, so
//     placing toolapproval in Config.Middlewares makes it the outer
//     wrapper — it sees (and can intercept) a tool-approval request
//     toolautocall surfaces before that request ever reaches the caller,
//     and it sees inbound approval-response messages before toolautocall
//     gets a chance to process them itself. Getting this backwards (or
//     placing toolapproval alongside toolautocall in
//     ProviderConfig.Middlewares, which Canopy has no seam to do anyway —
//     ProviderConfig.Middlewares is assembled inside each per-provider
//     constructor, not caller-supplied) would mean toolapproval never
//     actually gates anything. toolapproval_ordering_test.go proves this
//     ordering empirically: a stub ApprovalRequiredTool's underlying action
//     is asserted to NOT have run immediately after a turn that would
//     otherwise have called it, then confirmed to run once approval is
//     simulated.
func WireLoopQuality(params BuildParams) agent.Config {
	cfg := params.Config

	cfg.ContextProviders = append(slices.Clone(cfg.ContextProviders), NewCompactionProvider(params.Model.ContextWindowTokens))
	if params.TodoProvider != nil {
		cfg.ContextProviders = append(cfg.ContextProviders, params.TodoProvider)
	}
	if params.ModeProvider != nil {
		cfg.ContextProviders = append(cfg.ContextProviders, params.ModeProvider)
	}

	cfg.Middlewares = append([]agent.Middleware{toolapproval.New(toolapproval.Config{})}, slices.Clone(cfg.Middlewares)...)

	return cfg
}
