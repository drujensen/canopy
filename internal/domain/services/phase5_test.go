package services

import (
	"context"
	"encoding/json"
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
	// maps, fresh todo/agentmode Provider instances) that shares nothing
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

// TestAgentService_Mode_ReadSetPersistAcrossRestart is Requirements FR12's
// persistence claim, plus the readable/settable half PLAN.md Phase 5 asks
// for: a new chat starts in AgentService's configured default mode
// ("execute", not agentmode's own package default of "plan" — see
// NewAgentService's doc comment), SetMode changes it immediately, and the
// new mode is readable from a brand-new AgentService sharing only the
// persisted ChatRepository.
func TestAgentService_Mode_ReadSetPersistAcrossRestart(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	newSvc := func() *AgentService {
		return NewAgentService(AgentServiceConfig{
			Definitions: Definitions{
				Agents: map[string]agentsource.AgentDefinition{
					"assistant": {Name: "assistant", Description: "test agent"},
				},
			},
			Repository: repo,
		})
	}

	ctx := context.Background()
	svc := newSvc()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	mode, err := svc.GetMode(ctx, "chat-1")
	require.NoError(t, err)
	assert.Equal(t, "execute", mode)

	require.NoError(t, svc.SetMode(ctx, "chat-1", "plan"))

	mode, err = svc.GetMode(ctx, "chat-1")
	require.NoError(t, err)
	assert.Equal(t, "plan", mode)

	restarted := newSvc()
	mode, err = restarted.GetMode(ctx, "chat-1")
	require.NoError(t, err)
	assert.Equal(t, "plan", mode, "the mode switch must survive a restart")
}

// TestAgentService_PlanMode_RestrictsMutatingTools is Design §3.8's flagged
// open point, made concrete: building an agent's tool list under plan mode
// omits the two approval-gated, mutating core tools (Bash/run_shell,
// FileWrite/file_write) regardless of the agent definition's own allowlist,
// while leaving non-mutating tools (file_read) available; execute mode (and
// the zero-value "no mode" used elsewhere in this package's tests) is
// unrestricted.
func TestAgentService_PlanMode_RestrictsMutatingTools(t *testing.T) {
	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{
				"assistant": {Name: "assistant"},
			},
		},
		Tools: ToolsConfig{WorkingRoot: t.TempDir()},
	})
	ctx := context.Background()

	execTools, err := svc.buildTopLevelTools(ctx, agentsource.AgentDefinition{Name: "assistant"}, "execute")
	require.NoError(t, err)
	planTools, err := svc.buildTopLevelTools(ctx, agentsource.AgentDefinition{Name: "assistant"}, "plan")
	require.NoError(t, err)

	assert.True(t, containsToolName(execTools, "run_shell"), "execute mode must offer the bash tool")
	assert.True(t, containsToolName(execTools, "file_write"), "execute mode must offer the file-write tool")

	assert.False(t, containsToolName(planTools, "run_shell"), "plan mode must withhold the bash tool")
	assert.False(t, containsToolName(planTools, "file_write"), "plan mode must withhold the file-write tool")
	assert.True(t, containsToolName(planTools, "file_read"), "plan mode must still offer non-mutating tools")

	// Plan mode overrides even an explicit agent allowlist that names a
	// mutating tool — see planModeMutatingTools' doc comment for why this
	// is a deliberate "withheld, not just approval-gated" design.
	explicit, err := svc.buildTools(agentsource.AgentDefinition{Name: "assistant", Tools: []string{"Bash", "FileRead"}}, "plan")
	require.NoError(t, err)
	assert.False(t, containsToolName(explicit, "run_shell"))
	assert.True(t, containsToolName(explicit, "file_read"))
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
	assert.Equal(t, "m2", model, "the model override must survive a restart, the same way SetMode's mode switch does")
}

// TestAgentService_SetModel_RejectsUnknownModel is SetModel's defensive
// validation claim: an unconfigured model name is a clear error and must not
// mutate the chat's persisted override, mirroring SetMode's own validation
// against s.modeProvider's known modes.
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
