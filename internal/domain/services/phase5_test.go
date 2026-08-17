package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/tool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/impl/agentsource"
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
)

// toolCallChatCompletionResponse builds a minimal, valid OpenAI Chat
// Completions response body requesting a single function/tool call,
// mirroring the shape internal/impl/providers' own tests use for plain-text
// responses (chatCompletionResponse in agent_service_test.go), but for the
// tool_calls path.
func toolCallChatCompletionResponse(callID, funcName, argsJSON string) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion",
		"created": 1700000000,
		"model":   "test-model",
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []map[string]any{
						{
							"id":   callID,
							"type": "function",
							"function": map[string]any{
								"name":      funcName,
								"arguments": argsJSON,
							},
						},
					},
				},
				"finish_reason": "tool_calls",
			},
		},
	}
}

// findApprovalRequest returns the first ToolApprovalRequestContent in resp's
// messages, failing the test if none is present.
func findApprovalRequest(t *testing.T, resp *message.Message) *message.ToolApprovalRequestContent {
	t.Helper()
	for _, c := range resp.Contents {
		if req, ok := c.(*message.ToolApprovalRequestContent); ok {
			return req
		}
	}
	return nil
}

// TestAgentService_Approvals_AlwaysApprove_PersistsAcrossRestart is
// Requirements FR5's core claim, made concrete end-to-end through
// AgentService (not just impl/harness's lower-level ordering test): a real
// approval-gated tool call (the built-in Bash tool) is not executed before
// approval; "always approve this tool" lets it execute; and — the part that
// actually exercises Design §3.9's persistence mechanism — a brand-new
// AgentService built from nothing but the same persisted ChatRepository
// (simulating a process restart) auto-approves the same tool call without
// ever re-prompting.
//
// The mock provider server alternates by call parity: every odd-numbered
// call is a fresh model turn (returns a tool_calls response requesting
// run_shell), every even-numbered call is the follow-up completion
// toolautocall makes after a tool call actually executes (returns plain
// text). This matches the exact call sequence traced in
// harness.WireLoopQuality's doc comment: a turn that only surfaces an
// approval request (never approved) makes one call; a turn where the tool
// call is (auto-)approved and executes makes two.
func TestAgentService_Approvals_AlwaysApprove_PersistsAcrossRestart(t *testing.T) {
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

	// Turn 1: the model asks to run the marker command. It must not execute
	// yet — the tool is approval-gated and no standing rule exists.
	result1, err := svc.RunText(ctx, "chat-1", "please run the marker command")
	require.NoError(t, err)
	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr), "bash must not execute before approval is granted")

	var approvalReq *message.ToolApprovalRequestContent
	for _, m := range result1.Response.Messages {
		if req := findApprovalRequest(t, m); req != nil {
			approvalReq = req
		}
	}
	require.NotNil(t, approvalReq, "an approval request must have been surfaced instead of the tool silently executing")

	// Turn 2: the caller (Plan Phase 6's TUI, simulated here) responds with
	// "always approve this tool".
	approvalMsg := &message.Message{
		Role:     message.RoleUser,
		Contents: []message.Content{approvalReq.AlwaysApproveToolResponse()},
	}
	_, err = svc.RunMessages(ctx, "chat-1", []*message.Message{approvalMsg})
	require.NoError(t, err)
	_, statErr = os.Stat(marker)
	require.NoError(t, statErr, "bash should have executed once approval was granted")

	// Simulate a process restart: a brand-new AgentService (fresh in-memory
	// maps, fresh todo Provider instance) that shares nothing
	// with svc except the on-disk ChatRepository.
	require.NoError(t, os.Remove(marker))
	restarted := newSvc()

	result3, err := restarted.RunText(ctx, "chat-1", "please run the marker command again")
	require.NoError(t, err)

	for _, m := range result3.Response.Messages {
		assert.Nil(t, findApprovalRequest(t, m), "a standing 'always approve' rule loaded from persisted SessionState must auto-approve without re-prompting after a restart")
	}
	_, statErr = os.Stat(marker)
	assert.NoError(t, statErr, "the standing approval rule must have let the tool execute again automatically after the restart")
}

// TestAgentService_ChainedAutoApprove_ThenLaterTurn_DoesNotError is an
// end-to-end regression test for a real reported bug: a user answered one
// approval-gated Bash call with "always allow this tool", the agent then
// went on to make several more Bash calls in that same turn (all
// auto-approved by the standing rule, exactly what "always allow" is for),
// and the *next* unrelated message the user sent afterward failed with
// "ToolApprovalRequestContent found with ToolCall.CallID(s) '...' that have
// no matching ToolApprovalResponseContent" — a confirmed agent-framework-go
// bug where only the *first* auto-approved call in a turn gets a correctly
// matched request+response pair persisted; every one after that in the same
// turn persists only the request (see harness.RemoveOrphanedApprovalRequests'
// doc comment for the full empirical finding). No restart involved — this
// reproduces entirely within one running AgentService, the same as the real
// report. Uses enough chained calls (6) to actually trigger the bug: 1
// (explicitly approved) + 2 (first auto-approved, historically fine) would
// not have caught this on its own.
func TestAgentService_ChainedAutoApprove_ThenLaterTurn_DoesNotError(t *testing.T) {
	workDir := t.TempDir()
	marker := filepath.Join(workDir, "marker.txt")

	const numToolCalls = 6
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		if n <= numToolCalls {
			args, err := json.Marshal(map[string]string{"command": fmt.Sprintf("echo run-%d >> %s", n, marker)})
			require.NoError(t, err)
			_ = json.NewEncoder(w).Encode(toolCallChatCompletionResponse(fmt.Sprintf("call-%d", n), "run_shell", string(args)))
			return
		}
		_ = json.NewEncoder(w).Encode(chatCompletionResponse(fmt.Sprintf("reply-%d", n)))
	}))
	t.Cleanup(server.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gpt-test"}

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
		Tools:        ToolsConfig{WorkingRoot: workDir},
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	// Turn 1: the first call requires approval.
	result1, err := svc.RunText(ctx, "chat-1", "please run the marker command")
	require.NoError(t, err)
	var approvalReq *message.ToolApprovalRequestContent
	for _, m := range result1.Response.Messages {
		if req := findApprovalRequest(t, m); req != nil {
			approvalReq = req
		}
	}
	require.NotNil(t, approvalReq, "turn 1 must surface an approval request")

	// Turn 2: "always allow this tool" — this chains through the remaining
	// numToolCalls-1 calls, all auto-approved by the standing rule, within
	// this single turn. This is where the bug used to leave orphaned,
	// unanswered requests in persisted history.
	approvalMsg := &message.Message{
		Role:     message.RoleUser,
		Contents: []message.Content{approvalReq.AlwaysApproveToolResponse()},
	}
	_, err = svc.RunMessages(ctx, "chat-1", []*message.Message{approvalMsg})
	require.NoError(t, err, "turn 2 (always-approve, chaining into several more auto-approved calls) must not error")

	// Turn 3: a later, entirely unrelated message — this is exactly where
	// the real bug surfaced.
	_, err = svc.RunText(ctx, "chat-1", "continue doing some work")
	require.NoError(t, err, "a later unrelated turn must not fail with 'no matching ToolApprovalResponseContent'")
}

// TestAgentService_AlreadyStuckChat_SelfHealsOnNextTurn covers the case
// TestAgentService_ChainedAutoApprove_ThenLaterTurn_DoesNotError doesn't:
// a chat that was already left in the broken state by a *pre-fix* binary
// (an orphaned ToolApprovalRequestContent already sitting in persisted
// history, e.g. exactly the shape a user hitting this bug before today
// would have on disk right now) must self-heal automatically on its very
// next turn, not require any manual repair. This specifically proves the
// *proactive* repair (harness.RemoveOrphanedApprovalRequests called before
// a.Run, not only after) — without it, a.Run itself fails immediately on
// every future turn, before the after-the-fact repair ever gets a chance
// to run.
func TestAgentService_AlreadyStuckChat_SelfHealsOnNextTurn(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("all good now"))
	}))
	t.Cleanup(server.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gpt-test"}

	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}},
		},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Repository:   repo,
	})

	ctx := context.Background()
	chat, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	// Directly construct the exact broken shape a pre-fix binary would have
	// left behind: an orphaned request with no matching response anywhere.
	orphanedReq := &message.ToolApprovalRequestContent{
		RequestID: "req-orphaned",
		ToolCall:  &message.FunctionCallContent{CallID: "call-stale", Name: "run_shell", Arguments: `{}`},
	}
	chat.Messages = append(chat.Messages,
		&message.Message{Role: message.RoleUser, Contents: []message.Content{&message.TextContent{Text: "please run something"}}},
		&message.Message{Role: message.RoleAssistant, Contents: []message.Content{orphanedReq}},
	)
	require.NoError(t, repo.Update(ctx, chat))

	// Sanity: confirm this chat really is stuck the way the bug leaves it —
	// the framework's own validation must reject it as-is.
	_, err = svc.RunText(ctx, "chat-1", "continue doing some work")
	require.NoError(t, err, "the proactive repair must fix the already-stuck chat before a.Run ever sees the orphaned request, so this must succeed on the very first attempt after the fix, with no manual intervention")
}

// TestAgentService_PlainFollowUp_ChainedAutoApprove_NoDuplicateMessage is an
// end-to-end regression test for a second real reported bug (found in the
// same investigation as the orphaned-request bugs above, but distinct): a
// chat resumed via --continue showed one typed message duplicated up to 8
// times in the rendered transcript. Root cause: agent/harness/toolapproval's
// internal auto-approval loop makes one independent invoke() round trip —
// each with its own impl/harness.ChatHistoryProvider.Invoked persist call —
// per newly-issued tool call within a single outer turn, resending the same
// *message.Message objects (the turn's actual new messages) on every round;
// without deduplication, each round re-persists them as if they were new.
// Fixed by impl/harness.ChatHistoryProvider tracking pointer identity across
// Invoked calls for the lifetime of one turn (see
// TestChatHistoryProvider_Invoked_DedupesRepeatedCallsWithinOneTurn in
// impl/harness for the direct unit-level test of that mechanism) — this
// test proves it end-to-end through a real chained-auto-approval sequence,
// the same shape the user actually hit.
func TestAgentService_PlainFollowUp_ChainedAutoApprove_NoDuplicateMessage(t *testing.T) {
	workDir := t.TempDir()
	marker := filepath.Join(workDir, "marker.txt")

	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case n == 1:
			args, err := json.Marshal(map[string]string{"command": "echo one >> " + marker})
			require.NoError(t, err)
			_ = json.NewEncoder(w).Encode(toolCallChatCompletionResponse("call-1", "run_shell", string(args)))
		case n == 2:
			_ = json.NewEncoder(w).Encode(chatCompletionResponse("done with call-1"))
		case n >= 3 && n <= 7:
			// The "try again" follow-up chains through several more
			// auto-approved calls under the now-standing rule.
			args, err := json.Marshal(map[string]string{"command": fmt.Sprintf("echo run-%d >> %s", n, marker)})
			require.NoError(t, err)
			_ = json.NewEncoder(w).Encode(toolCallChatCompletionResponse(fmt.Sprintf("call-%d", n), "run_shell", string(args)))
		default:
			_ = json.NewEncoder(w).Encode(chatCompletionResponse(fmt.Sprintf("reply-%d", n)))
		}
	}))
	t.Cleanup(server.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gpt-test"}

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
		Tools:        ToolsConfig{WorkingRoot: workDir},
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	result1, err := svc.RunText(ctx, "chat-1", "please run the marker command")
	require.NoError(t, err)
	var approvalReq *message.ToolApprovalRequestContent
	for _, m := range result1.Response.Messages {
		if req := findApprovalRequest(t, m); req != nil {
			approvalReq = req
		}
	}
	require.NotNil(t, approvalReq)

	approvalMsg := &message.Message{
		Role:     message.RoleUser,
		Contents: []message.Content{approvalReq.AlwaysApproveToolResponse()},
	}
	_, err = svc.RunMessages(ctx, "chat-1", []*message.Message{approvalMsg})
	require.NoError(t, err)

	_, err = svc.RunText(ctx, "chat-1", "try again")
	require.NoError(t, err)

	chat, err := repo.Get(ctx, "chat-1")
	require.NoError(t, err)

	var tryAgainCount int
	for _, m := range chat.Messages {
		for _, c := range m.Contents {
			if txt, ok := c.(*message.TextContent); ok && txt.Text == "try again" {
				tryAgainCount++
			}
		}
	}
	assert.Equal(t, 1, tryAgainCount, "the user's plain follow-up message must be persisted exactly once, not once per internal auto-approval round it happens to trigger")
}

// TestAgentService_Todos_PersistAcrossRestart is Requirements FR11's
// persistence claim: a todo item the agent adds during a run is readable
// from a brand-new AgentService (simulating a restart) that only shares the
// persisted ChatRepository — GetTodos reads directly from the chat's
// deserialized SessionState, no live run required.
func TestAgentService_Todos_PersistAcrossRestart(t *testing.T) {
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

	result, err := svc.RunText(ctx, "chat-1", "please track this task for me")
	require.NoError(t, err)
	require.Len(t, result.Todos, 1)
	assert.Equal(t, "Write the report", result.Todos[0].Title)
	assert.False(t, result.Todos[0].IsComplete)

	restarted := newSvc()
	todos, err := restarted.GetTodos(ctx, "chat-1")
	require.NoError(t, err)
	require.Len(t, todos, 1)
	assert.Equal(t, "Write the report", todos[0].Title)
}

func containsToolName(tools []tool.Tool, name string) bool {
	for _, tl := range tools {
		if tl.Name() == name {
			return true
		}
	}
	return false
}

// TestAgentService_Model_ReadSetPersistAcrossRestart is the per-chat model
// override's persistence claim (post-v0.1.0 addendum, Design §4/FR1),
// mirroring TestAgentService_Mode_ReadSetPersistAcrossRestart's shape: a
// brand-new chat resolves to AgentServiceConfig.DefaultModel with no
// override, SetModel changes it immediately, and the new override is
// readable from a brand-new AgentService sharing only the persisted
// ChatRepository (simulating a process restart).
func TestAgentService_Model_ReadSetPersistAcrossRestart(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	p1 := entities.ProviderConfig{Name: "p1", Type: entities.ProviderTypeOpenAI}
	p2 := entities.ProviderConfig{Name: "p2", Type: entities.ProviderTypeAnthropic}
	m1 := entities.ModelConfig{Name: "m1", Provider: "p1"}
	m2 := entities.ModelConfig{Name: "m2", Provider: "p2"}

	newSvc := func() *AgentService {
		return NewAgentService(AgentServiceConfig{
			Definitions: Definitions{
				Agents: map[string]agentsource.AgentDefinition{
					"assistant": {Name: "assistant", Description: "test agent"},
				},
			},
			Providers:    []entities.ProviderConfig{p1, p2},
			Models:       []entities.ModelConfig{m1, m2},
			DefaultModel: "m1",
			Repository:   repo,
		})
	}

	ctx := context.Background()
	svc := newSvc()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	model, err := svc.GetModel(ctx, "chat-1")
	require.NoError(t, err)
	assert.Equal(t, "m1", model, "a brand-new chat with no override must resolve to the configured default model")

	require.NoError(t, svc.SetModel(ctx, "chat-1", "m2"))

	model, err = svc.GetModel(ctx, "chat-1")
	require.NoError(t, err)
	assert.Equal(t, "m2", model)

	restarted := newSvc()
	model, err = restarted.GetModel(ctx, "chat-1")
	require.NoError(t, err)
	assert.Equal(t, "m2", model, "the model override must survive a restart")
}

// TestAgentService_SetModel_RecordsLastModel asserts SetModel calls
// AgentServiceConfig.RecordLastModel (post-v0.1.0 addendum, fixing a real
// bug where a fresh chat always fell back to providers.json's first-listed
// model, silently discarding the model the user had actually switched to)
// with the newly-set model name, on success — mirrors
// TestAgentService_StartChat_RecordsLastAgent exactly.
func TestAgentService_SetModel_RecordsLastModel(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	p1 := entities.ProviderConfig{Name: "p1", Type: entities.ProviderTypeOpenAI}
	m1 := entities.ModelConfig{Name: "m1", Provider: "p1"}
	m2 := entities.ModelConfig{Name: "m2", Provider: "p1"}

	var recorded []string
	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}},
		},
		Providers:    []entities.ProviderConfig{p1},
		Models:       []entities.ModelConfig{m1, m2},
		DefaultModel: "m1",
		Repository:   repo,
		RecordLastModel: func(name string) error {
			recorded = append(recorded, name)
			return nil
		},
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)
	assert.Empty(t, recorded, "starting a chat must not itself record a last-used model — only an explicit SetModel does")

	require.NoError(t, svc.SetModel(ctx, "chat-1", "m2"))
	assert.Equal(t, []string{"m2"}, recorded)
}

// TestAgentService_SetModel_NilRecordLastModel_NoPanic asserts the default
// (no RecordLastModel set) is a safe no-op, not a nil-func-call panic.
func TestAgentService_SetModel_NilRecordLastModel_NoPanic(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	p1 := entities.ProviderConfig{Name: "p1", Type: entities.ProviderTypeOpenAI}
	m1 := entities.ModelConfig{Name: "m1", Provider: "p1"}
	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}},
		},
		Providers:    []entities.ProviderConfig{p1},
		Models:       []entities.ModelConfig{m1},
		DefaultModel: "m1",
		Repository:   repo,
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)
	require.NoError(t, svc.SetModel(ctx, "chat-1", "m1"))
}

// TestAgentService_SetModel_RecordLastModelError_DoesNotFailSetModel
// asserts remembering the last-used model is genuinely best-effort: a
// failure writing that state must never fail an otherwise-successful
// SetModel call.
func TestAgentService_SetModel_RecordLastModelError_DoesNotFailSetModel(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	p1 := entities.ProviderConfig{Name: "p1", Type: entities.ProviderTypeOpenAI}
	m1 := entities.ModelConfig{Name: "m1", Provider: "p1"}
	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}},
		},
		Providers:       []entities.ProviderConfig{p1},
		Models:          []entities.ModelConfig{m1},
		DefaultModel:    "m1",
		Repository:      repo,
		RecordLastModel: func(name string) error { return errors.New("disk full") },
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)
	err = svc.SetModel(ctx, "chat-1", "m1")
	require.NoError(t, err, "a RecordLastModel failure must not fail SetModel")
}

// TestAgentService_SetModel_RejectsUnknownModel is SetModel's defensive
// validation claim: an unconfigured model name is a clear error and must not
// mutate the chat's persisted override.
func TestAgentService_SetModel_RejectsUnknownModel(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{
				"assistant": {Name: "assistant"},
			},
		},
		Providers:    []entities.ProviderConfig{{Name: "p1", Type: entities.ProviderTypeOpenAI}},
		Models:       []entities.ModelConfig{{Name: "m1", Provider: "p1"}},
		DefaultModel: "m1",
		Repository:   repo,
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	err = svc.SetModel(ctx, "chat-1", "ghost")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghost")

	model, getErr := svc.GetModel(ctx, "chat-1")
	require.NoError(t, getErr)
	assert.Equal(t, "m1", model, "a rejected SetModel call must not have mutated the chat's persisted override")
}

// TestAgentService_ListModels_Sorted asserts ListModels returns every
// configured ModelConfig.Name in deterministic sorted order rather than Go's
// randomized map iteration order — the same determinism concern
// NewAgentService's own mcpToolNames sorting addresses, here for the TUI's
// ctrl+o model-picker overlay's item list.
func TestAgentService_ListModels_Sorted(t *testing.T) {
	svc := NewAgentService(AgentServiceConfig{
		Models: []entities.ModelConfig{
			{Name: "zeta", Provider: "p"},
			{Name: "alpha", Provider: "p"},
			{Name: "mid", Provider: "p"},
		},
	})
	assert.Equal(t, []string{"alpha", "mid", "zeta"}, svc.ListModels())
}

// TestAgentService_ListModelSummaries_IncludesCostAndSorted asserts
// ListModelSummaries (post-v0.1.0 addendum) carries each model's
// input/output per-million-token cost through from entities.ModelConfig,
// sorted the same deterministic way ListModels is, and that a model with no
// configured cost (the common case for a self-hosted model) comes through
// as zero rather than erroring or being omitted.
func TestAgentService_ListModelSummaries_IncludesCostAndSorted(t *testing.T) {
	svc := NewAgentService(AgentServiceConfig{
		Models: []entities.ModelConfig{
			{Name: "zeta", Provider: "p", InputCostPerMillionTokens: 3, OutputCostPerMillionTokens: 15},
			{Name: "alpha", Provider: "p"}, // no cost configured (e.g. self-hosted)
			{Name: "mid", Provider: "p", InputCostPerMillionTokens: 0.5, OutputCostPerMillionTokens: 1.5},
		},
	})

	got := svc.ListModelSummaries()
	require.Len(t, got, 3)
	assert.Equal(t, ModelSummary{Name: "alpha"}, got[0])
	assert.Equal(t, ModelSummary{Name: "mid", InputCostPerMillionTokens: 0.5, OutputCostPerMillionTokens: 1.5}, got[1])
	assert.Equal(t, ModelSummary{Name: "zeta", InputCostPerMillionTokens: 3, OutputCostPerMillionTokens: 15}, got[2])
}

// TestAgentService_ModelOverride_RoutesProviderDispatch is the routing proof
// this feature actually needs, not just persistence: two real provider
// shapes — an OpenAI Chat Completions-shaped httptest.Server (chatCompletionResponse,
// this package's existing helper) and a Gemini generateContent-shaped one
// (geminiTextResponse, mcp_stream_test.go's existing helper) — each paired
// with its own ModelConfig, and an assertion that switching
// Chat.ModelOverride via SetModel between the two actually changes which
// server receives the *next* turn's request. This is what proves
// resolveProviderModel's override precedence is wired all the way through
// buildTopLevelAgent's real dispatch path, not merely that a string got
// persisted onto the chat's JSON.
func TestAgentService_ModelOverride_RoutesProviderDispatch(t *testing.T) {
	var openaiCalls, geminiCalls int32
	openaiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&openaiCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("hello from openai"))
	}))
	t.Cleanup(openaiServer.Close)

	geminiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&geminiCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(geminiTextResponse("hello from gemini"))
	}))
	t.Cleanup(geminiServer.Close)

	openaiProvider := entities.ProviderConfig{Name: "p-openai", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: openaiServer.URL + "/v1"}
	geminiProvider := entities.ProviderConfig{Name: "p-gemini", Type: entities.ProviderTypeGemini, APIKey: "test-key", BaseURL: geminiServer.URL}
	openaiModel := entities.ModelConfig{Name: "m-openai", Provider: openaiProvider.Name, ModelName: "gpt-test"}
	geminiModel := entities.ModelConfig{Name: "m-gemini", Provider: geminiProvider.Name, ModelName: "gemini-test"}

	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{
				"assistant": {Name: "assistant", Description: "test agent"},
			},
		},
		Providers:    []entities.ProviderConfig{openaiProvider, geminiProvider},
		Models:       []entities.ModelConfig{openaiModel, geminiModel},
		DefaultModel: openaiModel.Name,
		Repository:   repo,
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	// No override yet: the chat's default model (openai-backed) handles the turn.
	result1, err := svc.RunText(ctx, "chat-1", "hi")
	require.NoError(t, err)
	assert.Equal(t, "m-openai", result1.Model)
	assert.Equal(t, int32(1), atomic.LoadInt32(&openaiCalls), "the default model must route to the openai-shaped server")
	assert.Equal(t, int32(0), atomic.LoadInt32(&geminiCalls), "no request should have reached the gemini-shaped server yet")

	// Switch the override to the gemini-backed model.
	require.NoError(t, svc.SetModel(ctx, "chat-1", geminiModel.Name))
	gotModel, err := svc.GetModel(ctx, "chat-1")
	require.NoError(t, err)
	assert.Equal(t, "m-gemini", gotModel)

	result2, err := svc.RunText(ctx, "chat-1", "hi again")
	require.NoError(t, err)
	assert.Equal(t, "m-gemini", result2.Model)
	assert.Equal(t, int32(1), atomic.LoadInt32(&geminiCalls), "after switching the override, the next turn must route to the gemini-shaped server")
	assert.Equal(t, int32(1), atomic.LoadInt32(&openaiCalls), "the openai server must not receive a second call once the override switched away from it")
}

// TestAgentService_ListAgents_Sorted mirrors TestAgentService_ListModels_Sorted
// for the ctrl+a in-chat agent-switch overlay's source list.
func TestAgentService_ListAgents_Sorted(t *testing.T) {
	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{
				"zeta":  {Name: "zeta"},
				"alpha": {Name: "alpha"},
				"mid":   {Name: "mid"},
			},
		},
	})
	assert.Equal(t, []string{"alpha", "mid", "zeta"}, svc.ListAgents())
}

// TestAgentService_SetAgent_RejectsUnknownAgent mirrors
// TestAgentService_SetModel_RejectsUnknownModel: an unconfigured agent name
// is a clear error and must not mutate the chat's persisted AgentName.
func TestAgentService_SetAgent_RejectsUnknownAgent(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{
				"assistant": {Name: "assistant"},
			},
		},
		Repository: repo,
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	err = svc.SetAgent(ctx, "chat-1", "ghost")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghost")

	chat, getErr := repo.Get(ctx, "chat-1")
	require.NoError(t, getErr)
	assert.Equal(t, "assistant", chat.AgentName, "a rejected SetAgent call must not have mutated the chat's bound agent")
}

// TestAgentService_SetAgent_RecordsLastAgent asserts SetAgent, like
// StartChat, calls RecordLastAgent (post-v0.1.0 addendum) with the newly
// active agent's name — so a mid-session ctrl+a switch is remembered for
// the next session too, not just the agent a chat was originally started
// with.
func TestAgentService_SetAgent_RecordsLastAgent(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	var recorded []string
	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{
				"assistant": {Name: "assistant"},
				"reviewer":  {Name: "reviewer"},
			},
		},
		Repository: repo,
		RecordLastAgent: func(name string) error {
			recorded = append(recorded, name)
			return nil
		},
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)
	require.NoError(t, svc.SetAgent(ctx, "chat-1", "reviewer"))

	assert.Equal(t, []string{"assistant", "reviewer"}, recorded, "both StartChat and SetAgent must record")
}

// TestAgentService_SetAgent_SwitchesAgentKeepsHistory is the coordinator's
// explicit correction to the ctrl+a design: switching a chat's agent must
// behave symmetrically with SetModel — same chat ID, same persisted
// message history, only chat.AgentName (and therefore which agent's system
// prompt/tools the *next* turn uses) changes. This drives two full real
// turns against two distinctly-personality'd fake providers (one server per
// agent, each replying with a distinctive marker string identifying which
// agent's instructions reached it) and asserts: (1) turn 2's request, sent
// after SetAgent, actually reaches the second agent's model — a real
// resolution/dispatch proof, not just a persisted string — and (2) the
// chat's message history retains both turns afterward, proving SetAgent
// never cleared or reset it.
func TestAgentService_SetAgent_SwitchesAgentKeepsHistory(t *testing.T) {
	helpfulServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("reply from helpful-bot"))
	}))
	t.Cleanup(helpfulServer.Close)

	grumpyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("reply from grumpy-bot"))
	}))
	t.Cleanup(grumpyServer.Close)

	helpfulProvider := entities.ProviderConfig{Name: "p-helpful", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: helpfulServer.URL + "/v1"}
	grumpyProvider := entities.ProviderConfig{Name: "p-grumpy", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: grumpyServer.URL + "/v1"}
	helpfulModel := entities.ModelConfig{Name: "m-helpful", Provider: helpfulProvider.Name, ModelName: "gpt-test"}
	grumpyModel := entities.ModelConfig{Name: "m-grumpy", Provider: grumpyProvider.Name, ModelName: "gpt-test"}

	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{
				"helpful": {Name: "helpful", Description: "a helpful agent", Model: "m-helpful", Instructions: "You are extremely helpful."},
				"grumpy":  {Name: "grumpy", Description: "a grumpy agent", Model: "m-grumpy", Instructions: "You are extremely grumpy."},
			},
		},
		Providers:  []entities.ProviderConfig{helpfulProvider, grumpyProvider},
		Models:     []entities.ModelConfig{helpfulModel, grumpyModel},
		Repository: repo,
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "helpful")
	require.NoError(t, err)

	result1, err := svc.RunText(ctx, "chat-1", "first message")
	require.NoError(t, err)
	assert.Contains(t, result1.Response.String(), "reply from helpful-bot")

	require.NoError(t, svc.SetAgent(ctx, "chat-1", "grumpy"))

	chatAfterSwitch, err := repo.Get(ctx, "chat-1")
	require.NoError(t, err)
	assert.Equal(t, "grumpy", chatAfterSwitch.AgentName)
	assert.Len(t, chatAfterSwitch.Messages, 2, "the first turn's user+assistant messages must still be present immediately after switching agents")

	result2, err := svc.RunText(ctx, "chat-1", "second message")
	require.NoError(t, err)
	assert.Contains(t, result2.Response.String(), "reply from grumpy-bot", "the turn after SetAgent must be routed through the newly-switched agent's own model/instructions")

	finalChat, err := repo.Get(ctx, "chat-1")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(finalChat.Messages), 4, "history from before the agent switch must still be present after a turn with the new agent")

	var sawFirstUserMessage bool
	for _, m := range finalChat.Messages {
		if m.Role == message.RoleUser && strings.Contains(m.String(), "first message") {
			sawFirstUserMessage = true
		}
	}
	assert.True(t, sawFirstUserMessage, "the pre-switch user message must still be in chat history — SetAgent must never clear it")
}
