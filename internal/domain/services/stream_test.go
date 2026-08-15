package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
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
// fully drained) reports the same Todos/Mode a RunResult from RunText/
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
	assert.Equal(t, "execute", result.Mode)
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
// behavior (Design §3.9, Requirements FR14). It reuses the exact
// approval-gated-tool scenario TestAgentService_Approvals_AlwaysApprove_
// PersistsAcrossRestart (phase5_test.go) proves for the non-streaming path,
// but drives turns 1 and 2 through the *streaming* entry points
// (RunTextStream/RunMessagesStream) instead, then confirms the standing
// "always approve" rule they persisted is visible to a brand-new
// AgentService (simulating a process restart) driven through the
// *non-streaming* RunText — cross-checking that both entry points read and
// write the identical persisted session-state shape.
func TestAgentService_RunMessagesStream_PersistsSessionIdenticallyToRunMessages(t *testing.T) {
	workDir := t.TempDir()
	marker := filepath.Join(workDir, "marker.txt")

	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		if n%2 == 1 {
			args, err := json.Marshal(map[string]string{
				"command": fmt.Sprintf("echo run-%d >> %s", n, marker),
			})
			require.NoError(t, err)
			_ = json.NewEncoder(w).Encode(toolCallChatCompletionResponse(fmt.Sprintf("call-%d", n), "run_shell", string(args)))
			return
		}
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("ok"))
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
			Tools:        ToolsConfig{WorkingRoot: workDir},
		})
	}

	ctx := context.Background()
	svc := newSvc()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	// Turn 1, via the streaming entry point: the model asks to run the
	// marker command. Drain the stream, then finalize.
	stream1, finalize1, err := svc.RunTextStream(ctx, "chat-1", "please run the marker command")
	require.NoError(t, err)
	updates1 := drainStream(t, stream1)
	result1, err := finalize1()
	require.NoError(t, err)

	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr), "bash must not execute before approval is granted")

	var approvalReq *message.ToolApprovalRequestContent
	for _, u := range updates1 {
		for _, c := range u.Contents {
			if req, ok := c.(*message.ToolApprovalRequestContent); ok {
				approvalReq = req
			}
		}
	}
	require.NotNil(t, approvalReq, "an approval request must have been surfaced in the streamed updates")
	// The finalized Response must carry the same approval request the
	// streamed updates did, matching RunMessages' non-streaming contract.
	require.NotNil(t, findApprovalRequest(t, result1.Response.Messages[len(result1.Response.Messages)-1]))

	// Turn 2, also via the streaming entry point: respond with "always
	// approve this tool".
	approvalMsg := &message.Message{
		Role:     message.RoleUser,
		Contents: []message.Content{approvalReq.AlwaysApproveToolResponse()},
	}
	stream2, finalize2, err := svc.RunMessagesStream(ctx, "chat-1", []*message.Message{approvalMsg})
	require.NoError(t, err)
	drainStream(t, stream2)
	_, err = finalize2()
	require.NoError(t, err)

	_, statErr = os.Stat(marker)
	require.NoError(t, statErr, "bash should have executed once approval was granted via the streaming entry point")

	// Simulate a process restart, then drive the follow-up turn through the
	// *non-streaming* RunText, cross-checking that the streaming path's
	// persisted session state is read identically by the non-streaming path.
	require.NoError(t, os.Remove(marker))
	restarted := newSvc()

	result3, err := restarted.RunText(ctx, "chat-1", "please run the marker command again")
	require.NoError(t, err)

	for _, m := range result3.Response.Messages {
		assert.Nil(t, findApprovalRequest(t, m), "a standing 'always approve' rule persisted via the streaming entry point must auto-approve without re-prompting after a restart, even when read back through the non-streaming entry point")
	}
	_, statErr = os.Stat(marker)
	assert.NoError(t, statErr, "the standing approval rule persisted via streaming must have let the tool execute again automatically after the restart")
}
