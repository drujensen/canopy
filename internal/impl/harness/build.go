package harness

import (
	"context"
	"slices"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/agent/harness/todo"

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

	// TodoProvider is the shared agent/harness/todo instance to wire into
	// Config.ContextProviders (Design §3.7). The caller
	// (domain/services.AgentService) owns this instance so the exact same one
	// backs both the agent's own todos_* tools and any read-only,
	// no-live-agent-needed session inspection the caller does itself (see
	// AgentService.GetTodos). Nil skips wiring it in — mainly useful for
	// tests that don't need the full loop-quality surface.
	TodoProvider *todo.Provider
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

// WireLoopQuality attaches the loop-quality context providers to
// params.Config and returns the resulting agent.Config, without touching
// history or constructing anything provider-specific — split out from Build
// so the exact context-provider composition it produces is directly
// testable against a fake agent.ProviderConfig.Run, without needing a real
// provider client.
//
//   - Compaction (Design §3.5/FR10): agent/compaction.NewContextProvider,
//     appended to Config.ContextProviders.
//   - Todo (Design §3.7/FR11): params.TodoProvider, appended to
//     Config.ContextProviders after compaction, so compaction acts on the
//     raw conversation before todo injects its own per-turn instructional
//     messages — otherwise those freshly-injected messages could be
//     immediately compacted away on the same turn they were added.
//
// Approval-gating (agent/harness/toolapproval) was removed entirely: it only
// ever auto-approved every request silently (no human ever saw a prompt),
// so it added an extra provider round trip and the risk of malformed-history
// bugs for no actual safety benefit. No tool in this codebase implements
// tool.ApprovalRequiredTool anymore, so toolautocall never generates an
// approval request in the first place.
func WireLoopQuality(params BuildParams) agent.Config {
	cfg := params.Config

	cfg.ContextProviders = append(slices.Clone(cfg.ContextProviders), NewCompactionProvider(params.Model.ContextWindowTokens))
	if params.TodoProvider != nil {
		cfg.ContextProviders = append(cfg.ContextProviders, params.TodoProvider)
	}

	return cfg
}
