package providers

import (
	"context"
	"strings"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/openai/openai-go/v3/option"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// isOpenCodeProviderType reports whether t is any opencode-family provider
// — the pay-as-you-go catalog entry ("opencode") or the Go-tier one
// ("opencode-go"). Neither is one of Canopy's named entities.ProviderType
// consts; both arrive as the bare provider id string straight from the
// models.dev catalog (config.DetectProviders), so matching by prefix is
// the only way to recognize either without hardcoding two more consts for
// what both dispatch to the same special case below anyway.
func isOpenCodeProviderType(t entities.ProviderType) bool {
	return strings.HasPrefix(string(t), "opencode")
}

// newOpenCode is newOpenAICompatible plus one addition: an
// "x-opencode-session" header carrying chatID, the stable per-conversation
// identifier OpenCode's own docs ask every client to send — "Send a
// stable session ID in x-opencode-session for each conversation so we can
// optimize routing and prompt caching" (opencode.ai/v2/docs/console/go).
// Required, not just optional, for the Go-tier endpoint specifically:
// omitting it there is a hard 400 ("MissingSessionID ... cannot be routed
// efficiently"), confirmed live against
// https://opencode.ai/inference/go/openai/v1. Sent for the plain
// "opencode" endpoint too, even though it tolerates the header's absence
// today, since OpenCode's own guidance isn't scoped to Go specifically and
// there's no downside to a client that's already generating the value for
// the Go case anyway.
//
// chatID is read once at agent-construction time (via chatIDFromContext,
// not re-read per request) because the OpenAI SDK client — and the header
// options baked into it — is built once per turn's agent.Agent and reused
// for every request that turn makes; one agent.Agent is already scoped to
// exactly one chat (AgentService builds a fresh one per RunMessagesStream
// call), so "once per client construction" and "stable per conversation"
// already coincide without needing to re-derive anything per request.
func newOpenCode(cfg entities.ProviderConfig, model entities.ModelConfig, agentCfg agent.Config, chatID string) (*agent.Agent, error) {
	var extra []option.RequestOption
	if chatID != "" {
		extra = append(extra, option.WithHeader("x-opencode-session", chatID))
	}
	return newOpenAICompatibleWithOptions(cfg, model, agentCfg, extra...)
}

// chatIDContextKey is the context.Value key WithChatID/chatIDFromContext
// share — unexported, same pattern as retrynotice.go's retryNotifierKey,
// for the same reason: providers.New's signature is shared by every
// provider family (most of which have no use for a chat ID at all), so
// threading one value only newOpenCode needs through ctx avoids changing
// that signature — and every one of its ~30 existing call sites across
// this package's own tests — just to plumb through one opencode-specific
// header value.
type chatIDContextKey struct{}

// WithChatID attaches chatID to ctx so New can pass it to newOpenCode
// without taking a chatID parameter itself. Callers: harness.Build (the
// chat a turn's agent.Agent is bound to) and AgentService's own two direct
// providers.New call sites (title generation, which has a real chat ID;
// subagent dispatch, which doesn't — see AgentService's own call site for
// what it attaches instead, since a subagent's Design §3.4 context
// isolation means there's no persisted Chat to take an ID from).
func WithChatID(ctx context.Context, chatID string) context.Context {
	return context.WithValue(ctx, chatIDContextKey{}, chatID)
}

// chatIDFromContext retrieves what WithChatID attached, or "" if nothing
// did — e.g. every provider family besides opencode/opencode-go, which
// never needs this at all.
func chatIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(chatIDContextKey{}).(string)
	return id
}
