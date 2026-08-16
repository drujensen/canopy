package harness_test

import (
	"context"
	"testing"
	"time"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/impl/harness"
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
)

// TestBuild_ConstructsAgentWithChatHistoryProvider asserts Build wires a
// ChatHistoryProvider bound to params.Chat/params.Repository (Design §3.9)
// on top of whatever impl/providers.New produces, for a real (offline)
// provider type. Constructing an openai-go client does not itself make any
// network call, so this exercises Build end to end without needing a live
// provider or a fake HTTP server.
func TestBuild_ConstructsAgentWithChatHistoryProvider(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	chat := &entities.Chat{
		ID:        "chat-1",
		AgentName: "test-agent",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	require.NoError(t, repo.Create(context.Background(), chat))

	a, err := harness.Build(context.Background(), harness.BuildParams{
		Chat:       chat,
		Repository: repo,
		Provider:   entities.ProviderConfig{Name: "openai", Type: entities.ProviderTypeOpenAI, APIKey: "test-key"},
		Model:      entities.ModelConfig{Name: "gpt-4o-mini", Provider: "openai", ModelName: "gpt-4o-mini"},
		Config:     agent.Config{Name: "test-agent"},
	})
	require.NoError(t, err)
	require.NotNil(t, a)
}

// TestBuild_UnrecognizedProviderType asserts Build propagates the error
// impl/providers.New returns for an unrecognized ProviderType, rather than
// swallowing it or panicking.
func TestBuild_UnrecognizedProviderType(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	chat := &entities.Chat{
		ID:        "chat-1",
		AgentName: "test-agent",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	require.NoError(t, repo.Create(context.Background(), chat))

	_, err = harness.Build(context.Background(), harness.BuildParams{
		Chat:       chat,
		Repository: repo,
		Provider:   entities.ProviderConfig{Name: "bogus", Type: entities.ProviderType("bogus")},
		Model:      entities.ModelConfig{Name: "bogus", Provider: "bogus", ModelName: "bogus"},
		Config:     agent.Config{Name: "test-agent"},
	})
	assert.Error(t, err)
}
