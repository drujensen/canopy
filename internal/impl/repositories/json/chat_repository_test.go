package json

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/microsoft/agent-framework-go/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
)

func newTestRepo(t *testing.T) *ChatRepository {
	t.Helper()
	root := filepath.Join(t.TempDir(), "chats")
	repo, err := NewChatRepository(root)
	require.NoError(t, err)
	return repo
}

func sampleChat(id string) *entities.Chat {
	now := time.Now().UTC().Truncate(time.Second)
	return &entities.Chat{
		ID:        id,
		AgentName: "coder",
		Messages: []*message.Message{
			message.NewText("hello there"),
		},
		SessionState: []byte{0x00, 0x01, 0xFF, 0x7F, 0x80, 'x', '{', '}'},
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

func TestChatRepository_CreateGetRoundTrip(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	chat := sampleChat("chat-1")
	require.NoError(t, repo.Create(ctx, chat))

	got, err := repo.Get(ctx, "chat-1")
	require.NoError(t, err)

	assert.Equal(t, chat.ID, got.ID)
	assert.Equal(t, chat.AgentName, got.AgentName)
	assert.True(t, chat.CreatedAt.Equal(got.CreatedAt))
	assert.True(t, chat.UpdatedAt.Equal(got.UpdatedAt))
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "hello there", got.Messages[0].String())

	// The SessionState blob must round-trip byte-for-byte as raw bytes —
	// this is the field that will later carry a serialized *agent.Session
	// (Design §3.9); Phase 1 only needs the plumbing to be lossless.
	assert.Equal(t, chat.SessionState, got.SessionState)
}

func TestChatRepository_CreateDuplicateFails(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	chat := sampleChat("dup")
	require.NoError(t, repo.Create(ctx, chat))

	err := repo.Create(ctx, sampleChat("dup"))
	assert.ErrorIs(t, err, ErrChatAlreadyExists)
}

func TestChatRepository_GetMissingFails(t *testing.T) {
	repo := newTestRepo(t)
	_, err := repo.Get(context.Background(), "does-not-exist")
	assert.ErrorIs(t, err, ErrChatNotFound)
}

func TestChatRepository_CreateEmptyIDFails(t *testing.T) {
	repo := newTestRepo(t)
	err := repo.Create(context.Background(), &entities.Chat{})
	assert.Error(t, err)
}

func TestChatRepository_Update(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	chat := sampleChat("chat-2")
	require.NoError(t, repo.Create(ctx, chat))

	chat.AgentName = "reviewer"
	chat.Messages = append(chat.Messages, message.NewText("second message"))
	chat.SessionState = []byte("updated-session-state")
	chat.UpdatedAt = chat.UpdatedAt.Add(time.Minute)
	require.NoError(t, repo.Update(ctx, chat))

	got, err := repo.Get(ctx, "chat-2")
	require.NoError(t, err)
	assert.Equal(t, "reviewer", got.AgentName)
	assert.Len(t, got.Messages, 2)
	assert.Equal(t, []byte("updated-session-state"), got.SessionState)
}

func TestChatRepository_UpdateMissingFails(t *testing.T) {
	repo := newTestRepo(t)
	err := repo.Update(context.Background(), sampleChat("ghost"))
	assert.ErrorIs(t, err, ErrChatNotFound)
}

func TestChatRepository_Delete(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	chat := sampleChat("chat-3")
	require.NoError(t, repo.Create(ctx, chat))
	require.NoError(t, repo.Delete(ctx, "chat-3"))

	_, err := repo.Get(ctx, "chat-3")
	assert.ErrorIs(t, err, ErrChatNotFound)
}

func TestChatRepository_DeleteMissingFails(t *testing.T) {
	repo := newTestRepo(t)
	err := repo.Delete(context.Background(), "ghost")
	assert.ErrorIs(t, err, ErrChatNotFound)
}

func TestChatRepository_List(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	require.NoError(t, repo.Create(ctx, sampleChat("a")))
	require.NoError(t, repo.Create(ctx, sampleChat("b")))
	require.NoError(t, repo.Create(ctx, sampleChat("c")))

	chats, err := repo.List(ctx)
	require.NoError(t, err)
	require.Len(t, chats, 3)

	ids := make(map[string]bool, 3)
	for _, c := range chats {
		ids[c.ID] = true
	}
	assert.True(t, ids["a"])
	assert.True(t, ids["b"])
	assert.True(t, ids["c"])
}

func TestChatRepository_ListEmpty(t *testing.T) {
	repo := newTestRepo(t)
	chats, err := repo.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, chats)
}
