// Package harness wires a *agent.Agent for one persisted Chat (Design §2,
// §3.3, §3.9). It supplies the ChatHistoryProvider so conversation history
// round-trips through interfaces.ChatRepository, then delegates
// provider/model construction to impl/providers.
//
// # Loop and tool-call wiring (Design §3.3, PLAN.md Phase 4)
//
// This package deliberately does NOT add agent/harness/toolautocall
// middleware itself. Reading the framework source
// (provider/openaiprovider/chat.go, provider/openaiprovider/responses.go,
// provider/anthropicprovider/agent.go, provider/geminiprovider/agent.go —
// all four constructors) confirms every one of them already appends
// toolautocall.New(...) to its provider middlewares unless
// Config.DisableFuncAutoCall is set. impl/providers.New (which this package
// calls) routes every configured provider type through one of those four
// constructors, so toolautocall is wired in for free; adding it again here
// would double up automatic tool-call handling.
//
// agent/harness/loop is also deliberately NOT wired in here. Reading
// agent/harness/loop/loop.go shows it is a distinct, opt-in middleware that
// re-invokes an agent across multiple internal iterations until its
// Evaluators decide no more work is needed (e.g. a completion-marker
// "keep working until done" pattern) — nothing else in the framework wires
// it automatically, and nothing in Phase 4's exit criteria (a multi-turn
// conversation with tool calls and persisted history; a parent agent
// dispatching to a child agent as a tool call) requires it:
// toolautocall's own internal loop (see its package doc: "the middleware
// looks up the matching tool, invokes it, sends the generated function
// result back to the provider, and repeats this loop until there are no
// more function calls") already provides the run loop an ordinary
// tool-call/response turn needs. Attaching agent/harness/loop is a
// reasonable extension point for a later phase that wants autonomous
// multi-iteration turns, not something to cargo-cult in now.
package harness

import (
	"context"
	"fmt"
	"time"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"

	"github.com/drujensen/canopy/internal/domain/interfaces"
)

// chatHistorySourceID marks messages ChatHistoryProvider.Invoking loads from
// storage, so Invoked can tell them apart from the genuinely new
// request/response messages produced by the current run (Design §3.9). This
// mirrors the same source-stamping pattern agent.NewHistoryProvider itself
// uses internally (agent/history.go's notSourceTypes(SourceTypeHistoryProvider)
// filter) — necessary because, per agent.go's invoke(), the RequestMessages
// an agent.InvokedContext carries are the *post*-Invoking messages (loaded
// history + the new turn), not just the new turn; without this filter,
// every call to Invoked would re-append the entire loaded history back onto
// the chat, duplicating it on every turn.
const chatHistorySourceID = "canopy-chat-history"

// ChatHistoryProvider implements agent.HistoryProvider (Design §3.9,
// Requirements FR13) backed by interfaces.ChatRepository: Invoking loads the
// named chat's stored Messages and prepends them to whatever new messages
// the caller supplies; Invoked appends the turn's new request/response
// messages back onto the chat and persists it via ChatRepository.Update.
//
// The Chat identified by ChatID is expected to already exist in Repository
// (created by the caller, e.g. domain/services, before the first Run) —
// ChatHistoryProvider does not create chats itself, only reads and updates
// one that's already there.
type ChatHistoryProvider struct {
	chatID     string
	repository interfaces.ChatRepository
}

var _ agent.HistoryProvider = (*ChatHistoryProvider)(nil)

// NewChatHistoryProvider returns a ChatHistoryProvider bound to one chat ID
// and repository.
func NewChatHistoryProvider(chatID string, repository interfaces.ChatRepository) *ChatHistoryProvider {
	return &ChatHistoryProvider{chatID: chatID, repository: repository}
}

// Invoking loads the bound chat's stored messages and returns them prepended
// to invoking.Messages, per agent.HistoryProvider's contract of returning
// messages in chronological order.
func (p *ChatHistoryProvider) Invoking(ctx context.Context, invoking agent.InvokingContext) ([]*message.Message, error) {
	chat, err := p.repository.Get(ctx, p.chatID)
	if err != nil {
		return nil, fmt.Errorf("chat history provider: loading chat %q: %w", p.chatID, err)
	}
	if len(chat.Messages) == 0 {
		return invoking.Messages, nil
	}

	source := message.Source{Type: agent.SourceTypeHistoryProvider, ID: chatHistorySourceID}
	history := make([]*message.Message, len(chat.Messages))
	for i, m := range chat.Messages {
		clone := m.Clone()
		clone.Source = source
		history[i] = clone
	}
	return append(history, invoking.Messages...), nil
}

// Invoked appends the turn's new request and response messages to the bound
// chat and persists it. It is a no-op when the run failed (invoked.Err !=
// nil), matching the framework's own default history provider's behavior of
// not storing a failed turn.
func (p *ChatHistoryProvider) Invoked(ctx context.Context, invoked agent.InvokedContext) error {
	if invoked.Err != nil {
		return nil
	}

	chat, err := p.repository.Get(ctx, p.chatID)
	if err != nil {
		return fmt.Errorf("chat history provider: loading chat %q: %w", p.chatID, err)
	}

	// invoked.RequestMessages is the *post*-Invoking message set: loaded
	// history (see the chatHistorySourceID doc comment above for why that
	// must be filtered out here rather than re-appended) plus the new turn's
	// messages plus, as of Phase 5's compaction/todo/agentmode wiring
	// (impl/harness.WireLoopQuality), whatever those context providers
	// injected this turn — a todo-list summary message, a mode-change
	// notification, a compaction summary group, etc. (agent/context.go's
	// invoke() captures requestMessages *after* the context-provider Invoking
	// loop runs, so those injected messages land in RequestMessages too).
	// Those are transient, regenerated-per-turn scaffolding, not real
	// conversation content, so they're filtered out here the same way
	// loaded-history messages are: persisting them would permanently bloat
	// chat.Messages with a fresh "current todo list" message (etc.) on every
	// single turn. Compaction in particular needs the *raw*, uncompacted
	// chat.Messages to keep working correctly on reload — it keeps its own
	// view (which groups are excluded) in session state, not by mutating
	// what's persisted here.
	for _, m := range invoked.RequestMessages {
		if m == nil || m.Source.Type == agent.SourceTypeHistoryProvider || m.Source.Type == agent.SourceTypeContextProvider {
			continue
		}
		chat.Messages = append(chat.Messages, m)
	}
	chat.Messages = append(chat.Messages, invoked.ResponseMessages...)
	chat.UpdatedAt = time.Now().UTC()

	if err := p.repository.Update(ctx, chat); err != nil {
		return fmt.Errorf("chat history provider: updating chat %q: %w", p.chatID, err)
	}
	return nil
}
