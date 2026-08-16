package services

import (
	"context"
	"testing"
	"time"

	"github.com/microsoft/agent-framework-go/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/impl/agentsource"
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
)

// ---------------------------------------------------------------------
// ListChatSummaries / GetChat
// ---------------------------------------------------------------------

// TestAgentService_ListChatSummaries_SortedByRecency asserts the ctrl+h
// history browser's/--continue's source list is ordered most-recently-
// updated first, and carries each chat's Title/AgentName through.
func TestAgentService_ListChatSummaries_SortedByRecency(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}}},
		Repository:  repo,
	})

	ctx := context.Background()
	older := &entities.Chat{ID: "older", AgentName: "assistant", Title: "Older chat", CreatedAt: time.Now(), UpdatedAt: time.Now().Add(-time.Hour)}
	newer := &entities.Chat{ID: "newer", AgentName: "assistant", Title: "Newer chat", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	require.NoError(t, repo.Create(ctx, older))
	require.NoError(t, repo.Create(ctx, newer))

	summaries, err := svc.ListChatSummaries(ctx)
	require.NoError(t, err)
	require.Len(t, summaries, 2)
	assert.Equal(t, "newer", summaries[0].ID, "most recently updated chat must come first")
	assert.Equal(t, "Newer chat", summaries[0].Title)
	assert.Equal(t, "older", summaries[1].ID)
}

func TestAgentService_ListChatSummaries_EmptyIsNotAnError(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	svc := NewAgentService(AgentServiceConfig{Repository: repo})

	summaries, err := svc.ListChatSummaries(context.Background())
	require.NoError(t, err)
	assert.Empty(t, summaries)
}

func TestAgentService_GetChat_ReturnsFullChat(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}}},
		Repository:  repo,
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	chat, err := svc.GetChat(ctx, "chat-1")
	require.NoError(t, err)
	assert.Equal(t, "chat-1", chat.ID)
	assert.Equal(t, "assistant", chat.AgentName)
}

func TestAgentService_GetChat_UnknownChat(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	svc := NewAgentService(AgentServiceConfig{Repository: repo})

	_, err = svc.GetChat(context.Background(), "ghost")
	require.Error(t, err)
}

// ---------------------------------------------------------------------
// GenerateChatTitle
// ---------------------------------------------------------------------

// TestAgentService_GenerateChatTitle_Success drives a real turn (so the
// chat has a genuine first exchange), then generates a title against a
// second fake provider response and asserts it's persisted onto the chat.
func TestAgentService_GenerateChatTitle_Success(t *testing.T) {
	provider, model := testProviderModel(t, "Debugging a Go Nil Pointer")
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}},
		},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Repository:   repo,
		Tools:        ToolsConfig{WorkingRoot: t.TempDir()},
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)
	_, err = svc.RunText(ctx, "chat-1", "why is my pointer nil?")
	require.NoError(t, err)

	title, err := svc.GenerateChatTitle(ctx, "chat-1")
	require.NoError(t, err)
	assert.Equal(t, "Debugging a Go Nil Pointer", title)

	chat, err := svc.GetChat(ctx, "chat-1")
	require.NoError(t, err)
	assert.Equal(t, "Debugging a Go Nil Pointer", chat.Title, "the generated title must be persisted")
}

func TestAgentService_GenerateChatTitle_PreservesUpdatedAt(t *testing.T) {
	provider, model := testProviderModel(t, "A Title")
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := NewAgentService(AgentServiceConfig{
		Definitions:  Definitions{Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}}},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Repository:   repo,
		Tools:        ToolsConfig{WorkingRoot: t.TempDir()},
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)
	_, err = svc.RunText(ctx, "chat-1", "hello")
	require.NoError(t, err)

	before, err := svc.GetChat(ctx, "chat-1")
	require.NoError(t, err)

	_, err = svc.GenerateChatTitle(ctx, "chat-1")
	require.NoError(t, err)

	after, err := svc.GetChat(ctx, "chat-1")
	require.NoError(t, err)
	assert.True(t, before.UpdatedAt.Equal(after.UpdatedAt), "generating a title must not bump UpdatedAt (would skew --continue/ctrl+h recency ordering)")
}

func TestAgentService_GenerateChatTitle_NoMessagesYet_Errors(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}}},
		Repository:  repo,
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	_, err = svc.GenerateChatTitle(ctx, "chat-1")
	require.Error(t, err)

	chat, getErr := svc.GetChat(ctx, "chat-1")
	require.NoError(t, getErr)
	assert.Empty(t, chat.Title, "a failed generation must leave Title untouched")
}

func TestAgentService_GenerateChatTitle_EmptyModelResponse_Errors(t *testing.T) {
	provider, model := testProviderModel(t, "   ")
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := NewAgentService(AgentServiceConfig{
		Definitions:  Definitions{Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}}},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Repository:   repo,
		Tools:        ToolsConfig{WorkingRoot: t.TempDir()},
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)
	_, err = svc.RunText(ctx, "chat-1", "hi")
	require.NoError(t, err)

	_, err = svc.GenerateChatTitle(ctx, "chat-1")
	require.Error(t, err)
}

// ---------------------------------------------------------------------
// buildTitlePrompt / sanitizeTitle (pure functions)
// ---------------------------------------------------------------------

func TestBuildTitlePrompt_NoMessages_ReturnsEmpty(t *testing.T) {
	assert.Equal(t, "", buildTitlePrompt(nil))
}

func TestBuildTitlePrompt_NoUserText_ReturnsEmpty(t *testing.T) {
	msgs := []*message.Message{
		{Role: message.RoleTool, Contents: message.Contents{&message.TextContent{Text: "tool output"}}},
	}
	assert.Equal(t, "", buildTitlePrompt(msgs))
}

func TestBuildTitlePrompt_IncludesFirstUserAndAssistantText(t *testing.T) {
	msgs := []*message.Message{
		{Role: message.RoleUser, Contents: message.Contents{&message.TextContent{Text: "how do I sort a slice?"}}},
		{Role: message.RoleAssistant, Contents: message.Contents{&message.TextContent{Text: "use sort.Slice"}}},
	}
	prompt := buildTitlePrompt(msgs)
	assert.Contains(t, prompt, "how do I sort a slice?")
	assert.Contains(t, prompt, "use sort.Slice")
}

func TestBuildTitlePrompt_SkipsEmptyToolMessagesToFindText(t *testing.T) {
	msgs := []*message.Message{
		{Role: message.RoleUser, Contents: message.Contents{&message.TextContent{Text: "read this file"}}},
		{Role: message.RoleAssistant, Contents: message.Contents{&message.FunctionCallContent{Name: "file_read"}}}, // no text
		{Role: message.RoleTool, Contents: message.Contents{&message.TextContent{Text: "file contents..."}}},
		{Role: message.RoleAssistant, Contents: message.Contents{&message.TextContent{Text: "here's the summary"}}},
	}
	prompt := buildTitlePrompt(msgs)
	assert.Contains(t, prompt, "read this file")
	assert.Contains(t, prompt, "here's the summary")
}

func TestSanitizeTitle_TrimsWhitespaceAndQuotes(t *testing.T) {
	assert.Equal(t, "Fixing the Bug", sanitizeTitle(`  "Fixing the Bug"  `))
}

func TestSanitizeTitle_CollapsesEmbeddedNewlines(t *testing.T) {
	assert.Equal(t, "Line one Line two", sanitizeTitle("Line one\nLine two"))
}

func TestSanitizeTitle_EmptyStaysEmpty(t *testing.T) {
	assert.Equal(t, "", sanitizeTitle("   "))
}

func TestSanitizeTitle_TruncatesVeryLongTitles(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "a"
	}
	got := sanitizeTitle(long)
	assert.LessOrEqual(t, len([]rune(got)), maxGeneratedTitleLen)
}
