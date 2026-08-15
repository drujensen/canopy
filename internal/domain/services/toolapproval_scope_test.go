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

	"github.com/microsoft/agent-framework-go/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/impl/agentsource"
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
)

// This file is Phase 7's security-pass regression coverage for Design
// §3.6's standing-approval-rule matching: a "always allow tool X" rule must
// not accidentally widen to cover a *different* tool, and — per
// agent-framework-go's agent/harness/toolapproval.Rule.matches (module
// cache: github.com/microsoft/agent-framework-go/agent/harness/toolapproval/
// toolapproval.go) — Canopy's own "always approve" UX
// (AlwaysApproveToolResponse, see internal/tui/chat.go and
// domain/services.AgentService's use of it) produces a Rule with a nil
// Arguments map, which matches *any* invocation of that tool name
// regardless of arguments. That second behavior is the framework's own
// design (Rule.matches: "If Arguments is nil, all invocations of the named
// tool are auto-approved"), not a bug Canopy introduced or can silently
// change without forking the framework — Canopy never calls the
// argument-scoped AlwaysApproveToolWithArgumentsResponse, so this is
// documented here as an accepted tradeoff, proven by
// TestAgentService_AlwaysApprove_CoversDifferentArguments_AcceptedTradeoff
// below, rather than asserted-and-then-worked-around.

// TestAgentService_AlwaysApprove_DoesNotCoverDifferentTool defeats the
// "broader than approved" failure mode across *tools*: after "always
// approve" is granted for the Bash tool (framework tool name "run_shell"),
// a later call to the unrelated FileWrite tool ("file_write") must still
// surface its own approval request rather than being silently auto-approved
// by the Bash rule — proving Rule.ToolName is checked exactly (toolapproval.
// Rule.matches: "if r.ToolName != toolName { return false }") rather than,
// say, "any standing rule exists" being treated as blanket trust.
func TestAgentService_AlwaysApprove_DoesNotCoverDifferentTool(t *testing.T) {
	workDir := t.TempDir()
	bashMarker := filepath.Join(workDir, "bash-marker.txt")
	writtenFile := filepath.Join(workDir, "written-by-model.txt")

	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			// Turn 1: model asks to run a bash command.
			args, err := json.Marshal(map[string]string{
				"command": fmt.Sprintf("echo run-1 >> %s", bashMarker),
			})
			require.NoError(t, err)
			_ = json.NewEncoder(w).Encode(toolCallChatCompletionResponse("call-1", "run_shell", string(args)))
		case 2:
			// Turn 2 (after "always approve" the Bash call): completion
			// following actual tool execution.
			_ = json.NewEncoder(w).Encode(chatCompletionResponse("ran it"))
		case 3:
			// Turn 3: model asks to write a file via the unrelated
			// FileWrite tool. This must NOT be silently auto-approved by
			// the standing Bash rule.
			args, err := json.Marshal(map[string]any{
				"path":    "written-by-model.txt",
				"content": "should require its own approval",
			})
			require.NoError(t, err)
			_ = json.NewEncoder(w).Encode(toolCallChatCompletionResponse("call-3", "file_write", string(args)))
		default:
			_ = json.NewEncoder(w).Encode(chatCompletionResponse("ok"))
		}
	}))
	t.Cleanup(server.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gpt-test"}

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
		Tools:        ToolsConfig{WorkingRoot: workDir},
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	// Turn 1: bash call surfaces an approval request.
	result1, err := svc.RunText(ctx, "chat-1", "please run a bash command")
	require.NoError(t, err)
	bashApproval := findApprovalRequest(t, lastMessage(t, result1.Response))
	require.NotNil(t, bashApproval, "the Bash call must surface an approval request")

	// Turn 2: grant "always approve" for the Bash tool. It executes.
	approvalMsg := &message.Message{
		Role:     message.RoleUser,
		Contents: []message.Content{bashApproval.AlwaysApproveToolResponse()},
	}
	_, err = svc.RunMessages(ctx, "chat-1", []*message.Message{approvalMsg})
	require.NoError(t, err)
	_, statErr := os.Stat(bashMarker)
	require.NoError(t, statErr, "bash should have executed once its standing rule was granted")

	// Turn 3: the model now asks for the unrelated FileWrite tool. This
	// must NOT be auto-approved by the Bash standing rule — it must surface
	// its own approval request, and the file must not have been written.
	result3, err := svc.RunText(ctx, "chat-1", "please write a file too")
	require.NoError(t, err)

	fileApproval := findApprovalRequest(t, lastMessage(t, result3.Response))
	assert.NotNil(t, fileApproval, "a standing 'always approve' rule for Bash must not silently cover the unrelated FileWrite tool")
	_, statErr = os.Stat(writtenFile)
	assert.True(t, os.IsNotExist(statErr), "file_write must not have executed without its own approval")
}

// TestAgentService_AlwaysApprove_CoversDifferentArguments_AcceptedTradeoff
// documents (via a passing test, not a workaround) that Canopy's "always
// approve" UX produces a tool-scoped standing rule, not an
// argument-scoped one: once granted for the Bash tool, a later Bash call
// with completely different arguments is auto-approved too, without a new
// prompt. This is agent-framework-go's own Rule.matches behavior when
// Arguments is nil (see toolapproval.go in the module cache) — Canopy's
// AgentService/TUI only ever calls AlwaysApproveToolResponse (nil
// Arguments), never AlwaysApproveToolWithArgumentsResponse, so this is the
// actual, intended behavior of Canopy's "always allow this tool" affordance
// (matching Claude Code's own "don't ask again for this tool" semantics),
// not a bug — recorded here as an accepted, documented tradeoff per Phase
// 7's security-pass instructions rather than silently assumed.
func TestAgentService_AlwaysApprove_CoversDifferentArguments_AcceptedTradeoff(t *testing.T) {
	workDir := t.TempDir()
	markerA := filepath.Join(workDir, "marker-a.txt")
	markerB := filepath.Join(workDir, "marker-b.txt")

	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			args, err := json.Marshal(map[string]string{
				"command": fmt.Sprintf("echo run-a >> %s", markerA),
			})
			require.NoError(t, err)
			_ = json.NewEncoder(w).Encode(toolCallChatCompletionResponse("call-1", "run_shell", string(args)))
		case 2:
			_ = json.NewEncoder(w).Encode(chatCompletionResponse("ran it"))
		case 3:
			// A materially different command than call-1 — different
			// argument value entirely, not just a cosmetic tweak.
			args, err := json.Marshal(map[string]string{
				"command": fmt.Sprintf("echo run-b >> %s", markerB),
			})
			require.NoError(t, err)
			_ = json.NewEncoder(w).Encode(toolCallChatCompletionResponse("call-3", "run_shell", string(args)))
		default:
			_ = json.NewEncoder(w).Encode(chatCompletionResponse("ran the second one too"))
		}
	}))
	t.Cleanup(server.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gpt-test"}

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
		Tools:        ToolsConfig{WorkingRoot: workDir},
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	result1, err := svc.RunText(ctx, "chat-1", "please run a bash command")
	require.NoError(t, err)
	approval := findApprovalRequest(t, lastMessage(t, result1.Response))
	require.NotNil(t, approval)

	approvalMsg := &message.Message{
		Role:     message.RoleUser,
		Contents: []message.Content{approval.AlwaysApproveToolResponse()},
	}
	_, err = svc.RunMessages(ctx, "chat-1", []*message.Message{approvalMsg})
	require.NoError(t, err)
	_, statErr := os.Stat(markerA)
	require.NoError(t, statErr)

	// A second, argument-different Bash call must now run without any new
	// approval prompt — the accepted tradeoff this test documents.
	result3, err := svc.RunText(ctx, "chat-1", "please run a different bash command")
	require.NoError(t, err)
	for _, m := range result3.Response.Messages {
		assert.Nil(t, findApprovalRequest(t, m), "a tool-scoped standing rule auto-approves a same-tool call with different arguments too — this is agent-framework-go's own Rule.matches semantics, not a Canopy bug")
	}
	_, statErr = os.Stat(markerB)
	assert.NoError(t, statErr, "the differently-argued bash call should have executed automatically under the standing rule")
}
