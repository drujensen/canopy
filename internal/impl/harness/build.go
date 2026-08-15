package harness

import (
	"context"

	"github.com/microsoft/agent-framework-go/agent"

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
	Provider entities.ProviderConfig
	Model    entities.ModelConfig

	// Config is the caller-assembled agent configuration (name, description,
	// tools, instructions via RunOptions, logger, etc — Design §3.1).
	// Config.HistoryProvider is overwritten with a ChatHistoryProvider bound
	// to Chat/Repository regardless of what the caller sets, since a
	// chat-bound agent's history is this package's whole job.
	Config agent.Config
}

// Build constructs a *agent.Agent wired with a ChatHistoryProvider bound to
// params.Chat/params.Repository (Design §3.9) and delegates the rest of
// construction to impl/providers.New, which already wires
// agent/harness/toolautocall via the per-provider constructors (see this
// package's doc comment for why agent/harness/loop is intentionally not
// added here).
func Build(ctx context.Context, params BuildParams) (*agent.Agent, error) {
	cfg := params.Config
	cfg.HistoryProvider = NewChatHistoryProvider(params.Chat.ID, params.Repository)
	return providers.New(ctx, params.Provider, params.Model, cfg)
}
