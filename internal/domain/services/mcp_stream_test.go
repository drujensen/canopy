package services

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/impl/agentsource"
	"github.com/drujensen/canopy/internal/impl/mcpclient"
	"github.com/drujensen/canopy/internal/impl/mcpsource"
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
)

// newEchoMCPHTTPServer starts a real MCP server (github.com/modelcontextprotocol/go-sdk)
// over a real httptest.Server, exposing one "echo" tool — the same shape
// internal/impl/mcpclient/mcpclient_test.go's own helper server uses, reused
// here (rather than the stdio TestMain re-exec pattern) because Go only
// allows one TestMain per test binary and this package doesn't have the
// re-exec scaffolding; an httptest-backed streamable-HTTP MCP server is
// exactly as real a protocol round trip for this bug's purposes.
func newEchoMCPHTTPServer(t *testing.T) mcpsource.MCPServerConfig {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "canopy-test-echo", Version: "1.0.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "echoes back the given message",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"message": map[string]any{"type": "string"}},
			"required":   []any{"message"},
		},
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &args)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + args.Message}},
		}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return mcpsource.MCPServerConfig{Name: "echo-server", Kind: mcpsource.KindHTTP, URL: httpServer.URL}
}

// newMCPBackedAgentService connects a real MCP tool (see newEchoMCPHTTPServer)
// via mcpclient.Connect, wires it into AgentServiceConfig.MCPTools exactly the
// way cmd/canopy does, and returns the resulting AgentService plus the
// *mcpclient.Client so the test can Close it.
func newMCPBackedAgentService(t *testing.T, provider entities.ProviderConfig, model entities.ModelConfig) (*AgentService, *mcpclient.Client) {
	t.Helper()
	cfg := newEchoMCPHTTPServer(t)

	ctx := context.Background()
	client, err := mcpclient.Connect(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.Len(t, client.Tools, 1)

	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	mcpTools := make(map[string]tool.Tool, len(client.Tools))
	for _, tl := range client.Tools {
		mcpTools[tl.Name()] = tl
	}

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
		MCPTools:     mcpTools,
	})
	return svc, client
}

// runWithTimeout runs fn on its own goroutine and fails the test if it
// doesn't return within d, so a genuine hang (this bug's live-tmux symptom)
// fails fast and deterministically instead of relying on `go test`'s own
// (much longer) default timeout.
func runWithTimeout(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("timed out after %s — this reproduces the live hang", d)
	}
}

// TestRunMessagesStream_MCPTool_ToolCalled reproduces the live-tmux bug: a
// chat whose assembled agent has a real, connected MCP tool in its tool
// list, sent a message that causes the (fake) provider to request that MCP
// tool, driven through AgentService.RunMessagesStream — the exact code path
// internal/tui/stream.go's startTurn uses — must actually complete (drain
// the stream, then finalize) within a short deadline instead of hanging or
// silently swallowing an error. The MCP tool is not approval-gated, so the
// call executes directly within this one turn.
func TestRunMessagesStream_MCPTool_ToolCalled(t *testing.T) {
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			_ = json.NewEncoder(w).Encode(toolCallChatCompletionResponse("call-1", "echo", `{"message":"hi"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("done"))
	}))
	t.Cleanup(server.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gpt-test"}

	svc, _ := newMCPBackedAgentService(t, provider, model)

	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	runWithTimeout(t, 10*time.Second, func() {
		stream, finalize, err := svc.RunMessagesStream(ctx, "chat-1", []*message.Message{message.NewText("please echo hi")})
		require.NoError(t, err)

		var streamErr error
		var updates []*agent.ResponseUpdate
		for update, uErr := range stream {
			if uErr != nil {
				streamErr = uErr
			}
			updates = append(updates, update)
		}
		require.NoError(t, streamErr)
		t.Logf("got %d stream updates", len(updates))

		result, err := finalize()
		require.NoError(t, err)
		require.NotNil(t, result)
		t.Logf("final response: %q", result.Response.String())
		assert.Contains(t, result.Response.String(), "done", "the turn must complete after the tool executes directly, with no approval round trip")
	})
}

// TestRunMessagesStream_MCPTool_NotCalled isolates whether merely *having* an
// MCP tool in the assembled tool list (never calling it) is enough to break
// RunMessagesStream, independent of TestRunMessagesStream_MCPTool_ToolCalled
// above which actually exercises a tool call.
func TestRunMessagesStream_MCPTool_NotCalled(t *testing.T) {
	provider, model := testProviderModel(t, "hello, no tools needed")
	svc, _ := newMCPBackedAgentService(t, provider, model)

	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	runWithTimeout(t, 10*time.Second, func() {
		stream, finalize, err := svc.RunMessagesStream(ctx, "chat-1", []*message.Message{message.NewText("hi")})
		require.NoError(t, err)

		var streamErr error
		for _, uErr := range stream {
			if uErr != nil {
				streamErr = uErr
			}
		}
		require.NoError(t, streamErr)

		result, err := finalize()
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Contains(t, result.Response.String(), "hello, no tools needed")
	})
}

// geminiFunctionCallResponse builds a minimal Gemini generateContent JSON
// response (mirroring agent-framework-go's own
// provider/geminiprovider/agent_test.go minimalTextResponse/TestToolCall_NonStreaming
// helpers) requesting a single function call.
func geminiFunctionCallResponse(callID, funcName string, args map[string]any) map[string]any {
	return map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"role": "model",
					"parts": []any{
						map[string]any{
							"functionCall": map[string]any{
								"id":   callID,
								"name": funcName,
								"args": args,
							},
						},
					},
				},
				"finishReason": "STOP",
			},
		},
	}
}

// geminiTextResponse builds a minimal Gemini generateContent JSON response
// carrying a plain text reply.
func geminiTextResponse(text string) map[string]any {
	return map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"role":  "model",
					"parts": []any{map[string]any{"text": text}},
				},
				"finishReason": "STOP",
			},
		},
	}
}

// TestRunMessagesStream_MCPTool_Gemini_ToolCalled is
// TestRunMessagesStream_MCPTool_ToolCalled's Gemini-backed counterpart —
// the live-tmux bug reproduction used a real Gemini API key specifically,
// so this isolates whether the bug is provider-specific (geminiprovider's
// own tool-schema conversion, per this task's leading hypothesis) rather
// than something in AgentService/mcpclient that would affect every
// provider equally.
func TestRunMessagesStream_MCPTool_Gemini_ToolCalled(t *testing.T) {
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		body, _ := io.ReadAll(r.Body)
		t.Logf("gemini request #%d: %s", callCount, body)
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			_ = json.NewEncoder(w).Encode(geminiFunctionCallResponse("call-1", "echo", map[string]any{"message": "hi"}))
			return
		}
		_ = json.NewEncoder(w).Encode(geminiTextResponse("done"))
	}))
	t.Cleanup(server.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeGemini, APIKey: "test-key", BaseURL: server.URL}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gemini-test"}

	svc, _ := newMCPBackedAgentService(t, provider, model)

	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	runWithTimeout(t, 10*time.Second, func() {
		stream, finalize, err := svc.RunMessagesStream(ctx, "chat-1", []*message.Message{message.NewText("please echo hi")})
		require.NoError(t, err)

		var streamErr error
		var updates []*agent.ResponseUpdate
		for update, uErr := range stream {
			if uErr != nil {
				streamErr = uErr
			}
			updates = append(updates, update)
		}
		t.Logf("got %d stream updates, streamErr=%v", len(updates), streamErr)
		require.NoError(t, streamErr)

		result, err := finalize()
		require.NoError(t, err)
		require.NotNil(t, result)
		t.Logf("final response: %q", result.Response.String())
		for _, m := range result.Response.Messages {
			for _, c := range m.Contents {
				t.Logf("content: %T %+v", c, c)
			}
		}
		assert.Contains(t, result.Response.String(), "done", "the turn must complete after the tool executes directly, with no approval round trip")
	})
}

// TestRunMessagesStream_MCPTool_Gemini_SendsWellFormedFollowUpRequest asserts
// toolautocall's own follow-up request (the second HTTP call it makes after
// actually executing the tool, within this one turn) is well-formed:
// Gemini's provider does its own client-side validation (buildRequestParts's
// callIDToName scan) before ever making a network call, failing immediately
// with "geminiprovider: missing function name for result with call ID ..."
// if the assistant's functionCall part is missing — this guards against that
// regression by inspecting the raw second request body.
func TestRunMessagesStream_MCPTool_Gemini_SendsWellFormedFollowUpRequest(t *testing.T) {
	var callCount int32
	var secondRequestBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			_ = json.NewEncoder(w).Encode(geminiFunctionCallResponse("call-1", "echo", map[string]any{"message": "hi"}))
			return
		}
		body, _ := io.ReadAll(r.Body)
		secondRequestBody = body
		_ = json.NewEncoder(w).Encode(geminiTextResponse("done"))
	}))
	t.Cleanup(server.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeGemini, APIKey: "test-key", BaseURL: server.URL}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gemini-test"}

	svc, _ := newMCPBackedAgentService(t, provider, model)

	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	runWithTimeout(t, 10*time.Second, func() {
		result, err := drainStreamForTest(t, svc, ctx, "chat-1", []*message.Message{message.NewText("please echo hi")})
		require.NoError(t, err, "the tool call and its follow-up completion must not fail")
		require.NotNil(t, result)
		assert.Contains(t, result.Response.String(), "done")

		require.NotEmpty(t, secondRequestBody, "the follow-up request must have actually reached the fake Gemini backend")
		assert.Contains(t, string(secondRequestBody), `"functionCall"`,
			"the follow-up request must include the assistant's function-call part, not just the tool result")
		assert.Contains(t, string(secondRequestBody), `"name":"echo"`,
			"the function call must name the tool, matching what Gemini's own FunctionResponse requires")
	})
}

// drainStreamForTest drains an AgentService.RunMessagesStream call to
// completion and returns its finalized *RunResult, the shared pattern every
// multi-turn test in this file needs.
func drainStreamForTest(t *testing.T, svc *AgentService, ctx context.Context, chatID string, msgs []*message.Message) (*RunResult, error) {
	t.Helper()
	stream, finalize, err := svc.RunMessagesStream(ctx, chatID, msgs)
	if err != nil {
		return nil, err
	}
	for _, uErr := range stream {
		if uErr != nil {
			return nil, uErr
		}
	}
	return finalize()
}

// TestRunMessages_MCPTool_ToolCalled is the non-streaming counterpart to
// TestRunMessagesStream_MCPTool_ToolCalled, isolating whether the bug is
// specific to the streaming path or affects RunMessages too.
func TestRunMessages_MCPTool_ToolCalled(t *testing.T) {
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			_ = json.NewEncoder(w).Encode(toolCallChatCompletionResponse("call-1", "echo", `{"message":"hi"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("done"))
	}))
	t.Cleanup(server.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gpt-test"}

	svc, _ := newMCPBackedAgentService(t, provider, model)

	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	runWithTimeout(t, 10*time.Second, func() {
		result, err := svc.RunText(ctx, "chat-1", "please echo hi")
		require.NoError(t, err)
		assert.Contains(t, result.Response.String(), "done", "the turn must complete after the tool executes directly, with no approval round trip")
	})
}

// TestRunMessagesStream_MCPTool_OpenAI_SendsWellFormedFollowUpRequest asserts
// toolautocall's own follow-up request (the second HTTP call it makes after
// actually executing the tool, within this one turn) is well-formed: it
// must include an assistant message with matching "tool_calls" naming the
// call, immediately preceding the "tool" role result message — exactly what
// a real OpenAI Chat Completions API requires.
func TestRunMessagesStream_MCPTool_OpenAI_SendsWellFormedFollowUpRequest(t *testing.T) {
	var callCount int32
	var secondRequestBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			_ = json.NewEncoder(w).Encode(toolCallChatCompletionResponse("call-1", "echo", `{"message":"hi"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		secondRequestBody = body
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("done"))
	}))
	t.Cleanup(server.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gpt-test"}

	svc, _ := newMCPBackedAgentService(t, provider, model)

	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	runWithTimeout(t, 10*time.Second, func() {
		result, err := drainStreamForTest(t, svc, ctx, "chat-1", []*message.Message{message.NewText("please echo hi")})
		require.NoError(t, err)
		assert.Contains(t, result.Response.String(), "done")

		require.NotEmpty(t, secondRequestBody)
		var req map[string]any
		require.NoError(t, json.Unmarshal(secondRequestBody, &req))
		msgs, ok := req["messages"].([]any)
		require.True(t, ok)

		var toolCallsIdx, toolResultIdx = -1, -1
		for i, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if mm["role"] == "assistant" {
				if _, has := mm["tool_calls"]; has {
					toolCallsIdx = i
				}
			}
			if mm["role"] == "tool" && mm["tool_call_id"] == "call-1" {
				toolResultIdx = i
			}
		}
		require.NotEqual(t, -1, toolCallsIdx, "the follow-up request must include an assistant message with tool_calls")
		require.NotEqual(t, -1, toolResultIdx, "the follow-up request must include the tool-result message")
		assert.Less(t, toolCallsIdx, toolResultIdx, "the assistant tool_calls message must precede the tool result message, matching OpenAI's own wire-protocol requirement")
	})
}
