package harness_test

import (
	"context"
	"testing"
	"time"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/impl/harness"
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
)

// newTestChat creates and persists an empty chat in repo, returning its ID.
func newTestChat(t *testing.T, repo *jsonrepo.ChatRepository) string {
	t.Helper()
	chat := &entities.Chat{
		ID:        "chat-1",
		AgentName: "test-agent",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	require.NoError(t, repo.Create(context.Background(), chat))
	return chat.ID
}

// TestChatHistoryProvider_RoundTrip asserts the core Design §3.9 contract:
// Invoking loads the chat's stored messages prepended to the new turn, and
// Invoked persists the new turn back so a second turn sees the first turn's
// messages — a genuine round trip through a real JSON ChatRepository in a
// temp dir, not a mocked repository.
func TestChatHistoryProvider_RoundTrip(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	chatID := newTestChat(t, repo)

	provider := harness.NewChatHistoryProvider(chatID, repo)
	ctx := context.Background()

	// First turn: no history yet.
	turn1Input := []*message.Message{message.NewText("hello")}
	got, err := provider.Invoking(ctx, agent.InvokingContext{Messages: turn1Input})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "hello", got[0].String())

	turn1Response := []*message.Message{message.NewText("hi there")}
	turn1Response[0].Role = message.RoleAssistant
	err = provider.Invoked(ctx, agent.InvokedContext{
		RequestMessages:  got,
		ResponseMessages: turn1Response,
	})
	require.NoError(t, err)

	// The chat itself now has exactly the first turn's messages persisted.
	chat, err := repo.Get(ctx, chatID)
	require.NoError(t, err)
	require.Len(t, chat.Messages, 2)
	assert.Equal(t, "hello", chat.Messages[0].String())
	assert.Equal(t, "hi there", chat.Messages[1].String())

	// Second turn: Invoking must see the first turn's messages prepended.
	turn2Input := []*message.Message{message.NewText("how are you")}
	got2, err := provider.Invoking(ctx, agent.InvokingContext{Messages: turn2Input})
	require.NoError(t, err)
	require.Len(t, got2, 3)
	assert.Equal(t, "hello", got2[0].String())
	assert.Equal(t, "hi there", got2[1].String())
	assert.Equal(t, "how are you", got2[2].String())
	// Loaded history is source-stamped so Invoked can filter it back out.
	assert.Equal(t, agent.SourceTypeHistoryProvider, got2[0].Source.Type)
	assert.Equal(t, agent.SourceTypeHistoryProvider, got2[1].Source.Type)
	assert.Equal(t, message.SourceTypeExternal, got2[2].Source.Type)

	turn2Response := []*message.Message{message.NewText("doing well")}
	turn2Response[0].Role = message.RoleAssistant
	err = provider.Invoked(ctx, agent.InvokedContext{
		RequestMessages:  got2,
		ResponseMessages: turn2Response,
	})
	require.NoError(t, err)

	// Third read: chat must have exactly four messages, not a duplicated
	// history (this is the regression case chatHistorySourceID filtering
	// exists to prevent).
	chat, err = repo.Get(ctx, chatID)
	require.NoError(t, err)
	require.Len(t, chat.Messages, 4)
	assert.Equal(t, "hello", chat.Messages[0].String())
	assert.Equal(t, "hi there", chat.Messages[1].String())
	assert.Equal(t, "how are you", chat.Messages[2].String())
	assert.Equal(t, "doing well", chat.Messages[3].String())
}

// TestChatHistoryProvider_Invoked_SkipsFailedRun asserts a failed run's
// messages are not persisted, matching the framework's own default history
// provider behavior.
func TestChatHistoryProvider_Invoked_SkipsFailedRun(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	chatID := newTestChat(t, repo)
	provider := harness.NewChatHistoryProvider(chatID, repo)
	ctx := context.Background()

	err = provider.Invoked(ctx, agent.InvokedContext{
		RequestMessages:  []*message.Message{message.NewText("should not be saved")},
		ResponseMessages: nil,
		Err:              assert.AnError,
	})
	require.NoError(t, err)

	chat, err := repo.Get(ctx, chatID)
	require.NoError(t, err)
	assert.Empty(t, chat.Messages)
}

// TestChatHistoryProvider_Invoked_DedupesRepeatedCallsWithinOneTurn is a
// direct unit test for a real reported bug: a chat resumed via --continue
// showed one typed message duplicated up to 8 times. Root cause:
// agent/harness/toolapproval's internal auto-approval loop makes one
// complete, independent invoke() round trip — including its own Invoked
// call — per newly-issued tool call, all within a *single* outer turn, and
// resends the exact same *message.Message objects (the turn's original new
// messages) on every one of those internal rounds. Simulates exactly that:
// the *same* ChatHistoryProvider instance (one outer turn's worth of
// lifetime — see that type's own doc comment) receiving multiple Invoked
// calls that each include the *same* new user message pointer, mixed with
// genuinely different response content each round (mirroring a real
// auto-approval chain: request approval, execute, ask the model again).
// Asserts the repeated message is persisted exactly once, while each
// round's distinct new content still lands.
func TestChatHistoryProvider_Invoked_DedupesRepeatedCallsWithinOneTurn(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	chatID := newTestChat(t, repo)
	provider := harness.NewChatHistoryProvider(chatID, repo)
	ctx := context.Background()

	// The turn's one new message — the exact same *message.Message object
	// toolapproval's internal loop would resend on every round.
	newTurnMsg := message.NewText("try again")

	round1Response := message.NewText("round 1 reply")
	round1Response.Role = message.RoleAssistant
	require.NoError(t, provider.Invoked(ctx, agent.InvokedContext{
		RequestMessages:  []*message.Message{newTurnMsg},
		ResponseMessages: []*message.Message{round1Response},
	}))

	// A second internal round of the *same* outer turn: the framework
	// resends newTurnMsg again (same pointer), with a genuinely new
	// response.
	round2Response := message.NewText("round 2 reply")
	round2Response.Role = message.RoleAssistant
	require.NoError(t, provider.Invoked(ctx, agent.InvokedContext{
		RequestMessages:  []*message.Message{newTurnMsg},
		ResponseMessages: []*message.Message{round2Response},
	}))

	// A third round, same story.
	round3Response := message.NewText("round 3 reply")
	round3Response.Role = message.RoleAssistant
	require.NoError(t, provider.Invoked(ctx, agent.InvokedContext{
		RequestMessages:  []*message.Message{newTurnMsg},
		ResponseMessages: []*message.Message{round3Response},
	}))

	chat, err := repo.Get(ctx, chatID)
	require.NoError(t, err)

	var tryAgainCount int
	var texts []string
	for _, m := range chat.Messages {
		texts = append(texts, m.String())
		if m.String() == "try again" {
			tryAgainCount++
		}
	}
	assert.Equal(t, 1, tryAgainCount, "the repeated new-turn message must be persisted exactly once, not once per internal round: got %v", texts)
	assert.Contains(t, texts, "round 1 reply")
	assert.Contains(t, texts, "round 2 reply")
	assert.Contains(t, texts, "round 3 reply")
}

// TestChatHistoryProvider_Invoking_EmptyHistory asserts a brand-new chat
// with no stored messages just passes the new turn's messages through
// unchanged.
func TestChatHistoryProvider_Invoking_EmptyHistory(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	chatID := newTestChat(t, repo)
	provider := harness.NewChatHistoryProvider(chatID, repo)

	input := []*message.Message{message.NewText("first message")}
	got, err := provider.Invoking(context.Background(), agent.InvokingContext{Messages: input})
	require.NoError(t, err)
	assert.Equal(t, input, got)
}
