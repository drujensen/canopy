package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/impl/agentsource"
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
)

// drainStream ranges over stream, collecting every update, and fails the
// test immediately on any error — the standard "happy path" drain a caller
// (Plan Phase 6's TUI) performs before calling finalize.
func drainStream(t *testing.T, stream agent.ResponseStream) []*agent.ResponseUpdate {
	t.Helper()
	var updates []*agent.ResponseUpdate
	for update, err := range stream {
		require.NoError(t, err)
		updates = append(updates, update)
	}
	return updates
}

// TestAgentService_RunMessagesStream_DeliversUpdatesIncrementally proves the
// core streaming contract (Requirements FR8): ranging over the
// agent.ResponseStream RunMessagesStream returns yields the same content a
// non-streaming RunText call would collect, and finalize (once the stream is
// fully drained) reports the same Todos a RunResult from RunText/
// RunMessages would.
func TestAgentService_RunMessagesStream_DeliversUpdatesIncrementally(t *testing.T) {
	provider, model := testProviderModel(t, "hello from the stream")
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{
				"assistant": {Name: "assistant", Description: "test agent"},
			},
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

	stream, finalize, err := svc.RunTextStream(ctx, "chat-1", "hello")
	require.NoError(t, err)
	require.NotNil(t, stream)
	require.NotNil(t, finalize)

	updates := drainStream(t, stream)
	require.NotEmpty(t, updates, "at least one ResponseUpdate must be yielded")

	var text string
	for _, u := range updates {
		text += u.String()
	}
	assert.Contains(t, text, "hello from the stream", "streamed updates must carry the same content a non-streaming run would collect")

	result, err := finalize()
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Contains(t, result.Response.String(), "hello from the stream")
	assert.Empty(t, result.Todos)
}

// TestAgentService_RunMessagesStream_FinalizeBeforeDrainErrors proves
// finalize refuses to hand back a result before the caller has actually
// drained the stream, since RunMessagesStream's doc comment promises that
// invariant (there is no background goroutine computing the result
// concurrently — it literally doesn't exist yet).
func TestAgentService_RunMessagesStream_FinalizeBeforeDrainErrors(t *testing.T) {
	provider, model := testProviderModel(t, "unused")
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{
				"assistant": {Name: "assistant", Description: "test agent"},
			},
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

	_, finalize, err := svc.RunTextStream(ctx, "chat-1", "hello")
	require.NoError(t, err)

	_, err = finalize()
	assert.Error(t, err, "finalize before the stream is drained must error, not block or return a zero-value result")
}

// TestAgentService_RunMessagesStream_PersistsSessionIdenticallyToRunMessages
// is this phase's key claim about the streaming gap resolution: the
// streaming entry point must not diverge from RunMessages' persistence
// behavior (Design §3.9, Requirements FR14). Drives a turn that adds a todo
// item through the *streaming* entry point (RunTextStream), then confirms
// that todo is visible to a brand-new AgentService (simulating a process
// restart) via GetTodos, which reads directly from persisted
// SessionState — cross-checking that both entry points read and write the
// identical persisted session-state shape, mirroring
// TestAgentService_Todos_PersistAcrossRestart's non-streaming claim
// (phase5_test.go) for the streaming path instead.
func TestAgentService_RunMessagesStream_PersistsSessionIdenticallyToRunMessages(t *testing.T) {
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_ = json.NewEncoder(w).Encode(toolCallChatCompletionResponse("call-1", "todos_add", `{"Arg0":[{"title":"Write the report"}]}`))
			return
		}
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("added it"))
	}))
	t.Cleanup(server.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gpt-test"}

	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	newSvc := func() *AgentService {
		return NewAgentService(AgentServiceConfig{
			Definitions: Definitions{
				Agents: map[string]agentsource.AgentDefinition{
					"assistant": {Name: "assistant", Description: "test agent"},
				},
			},
			Providers:    []entities.ProviderConfig{provider},
			Models:       []entities.ModelConfig{model},
			DefaultModel: model.Name,
			Repository:   repo,
			Tools:        ToolsConfig{WorkingRoot: t.TempDir()},
		})
	}

	ctx := context.Background()
	svc := newSvc()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	// Via the streaming entry point: the model adds a todo item. Drain the
	// stream, then finalize.
	stream, finalize, err := svc.RunTextStream(ctx, "chat-1", "please track this task for me")
	require.NoError(t, err)
	drainStream(t, stream)
	result, err := finalize()
	require.NoError(t, err)
	require.Len(t, result.Todos, 1)
	assert.Equal(t, "Write the report", result.Todos[0].Title)

	// Simulate a process restart, then read the todo back through a
	// brand-new AgentService that only shares the persisted ChatRepository
	// — cross-checking that the streaming path's persisted session state is
	// read identically to the non-streaming path's own persistence.
	restarted := newSvc()
	todos, err := restarted.GetTodos(ctx, "chat-1")
	require.NoError(t, err)
	require.Len(t, todos, 1)
	assert.Equal(t, "Write the report", todos[0].Title, "a todo added via the streaming entry point must persist identically to the non-streaming path")
}
