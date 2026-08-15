package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/domain/services"
	"github.com/drujensen/canopy/internal/impl/agentsource"
	"github.com/drujensen/canopy/internal/impl/mcpclient"
	"github.com/drujensen/canopy/internal/impl/mcpsource"
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
)

// newMCPTestService builds a real AgentService wired to a real, connected
// MCP tool (an "echo" tool served over a real httptest-backed MCP HTTP
// server) plus a fake OpenAI-compatible provider that requests it — the
// live-tmux bug's setup, reproduced through the actual chatModel/Model
// wiring (chatModel.startTurnCmd -> stream.go's startTurn ->
// AgentService.RunMessagesStream), not just AgentService directly.
func newMCPTestService(t *testing.T) *services.AgentService {
	t.Helper()

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "canopy-test-echo", Version: "1.0.0"}, nil)
	mcpServer.AddTool(&mcp.Tool{
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
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)
	mcpHTTPServer := httptest.NewServer(handler)
	t.Cleanup(mcpHTTPServer.Close)

	client, err := mcpclient.Connect(context.Background(), mcpsource.MCPServerConfig{
		Name: "echo-server", Kind: mcpsource.KindHTTP, URL: mcpHTTPServer.URL,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	mcpTools := make(map[string]tool.Tool, len(client.Tools))
	for _, tl := range client.Tools {
		mcpTools[tl.Name()] = tl
	}

	var callCount int32
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			_ = json.NewEncoder(w).Encode(toolCallChatCompletionResponse("call-1", "echo", `{"message":"hi"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("done"))
	}))
	t.Cleanup(providerServer.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: providerServer.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gpt-test"}

	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	return services.NewAgentService(services.AgentServiceConfig{
		Definitions: services.Definitions{
			Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant", Description: "test agent"}},
		},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Repository:   repo,
		Tools:        services.ToolsConfig{WorkingRoot: t.TempDir()},
		MCPTools:     mcpTools,
	})
}

// drainCmdWithTimeout is drainCmd (model_test.go) but bails out instead of
// hanging forever if a Cmd never produces a terminal message — reproducing
// the live-tmux bug (if it lives in this package) as a fast test failure
// instead of a real hang.
func drainCmdWithTimeout(t *testing.T, c *chatModel, cmd tea.Cmd, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		drainCmd(t, c, cmd)
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("drainCmd timed out after %s — reproduces the live hang", d)
	}
}

// TestChatModel_MCPTool_ToolApprovalPromptAppears drives the real
// chatModel.startTurnCmd -> startTurn -> AgentService.RunMessagesStream path
// (exactly what a live keypress in the TUI does) with a real, connected MCP
// tool the fake provider requests, asserting the resulting
// ToolApprovalRequestContent actually reaches chatModel's state (a rendered
// approval prompt), not silently lost.
func TestChatModel_MCPTool_ToolApprovalPromptAppears(t *testing.T) {
	svc := newMCPTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "execute", 80, 24)

	drainCmdWithTimeout(t, c, c.startTurnCmd(svc, ctx, []*message.Message{message.NewText("please use the echo tool to say hi")}), 10*time.Second)

	require.NotNil(t, c.pendingApproval, "an MCP tool call must surface a real approval prompt through the full chatModel/stream.go path")
	require.Equal(t, "echo", c.pendingApprovalTool)
	require.Contains(t, c.View(80, 24), "Tool approval requested: echo")
}

// TestChatModel_MCPTool_PlainMessage_NoHang isolates whether merely having
// an MCP tool in the assembled tool list (the provider never actually
// requests it) hangs the real chatModel/stream.go path.
func TestChatModel_MCPTool_PlainMessage_NoHang(t *testing.T) {
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "canopy-test-echo", Version: "1.0.0"}, nil)
	mcpServer.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "echoes back the given message",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"message": map[string]any{"type": "string"}},
			"required":   []any{"message"},
		},
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo: hi"}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)
	mcpHTTPServer := httptest.NewServer(handler)
	t.Cleanup(mcpHTTPServer.Close)

	client, err := mcpclient.Connect(context.Background(), mcpsource.MCPServerConfig{
		Name: "echo-server", Kind: mcpsource.KindHTTP, URL: mcpHTTPServer.URL,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	mcpTools := make(map[string]tool.Tool, len(client.Tools))
	for _, tl := range client.Tools {
		mcpTools[tl.Name()] = tl
	}

	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("hello, no tools needed"))
	}))
	t.Cleanup(providerServer.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: providerServer.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gpt-test"}

	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := services.NewAgentService(services.AgentServiceConfig{
		Definitions: services.Definitions{
			Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant", Description: "test agent"}},
		},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Repository:   repo,
		Tools:        services.ToolsConfig{WorkingRoot: t.TempDir()},
		MCPTools:     mcpTools,
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "execute", 80, 24)
	drainCmdWithTimeout(t, c, c.startTurnCmd(svc, ctx, []*message.Message{message.NewText("hi")}), 10*time.Second)

	require.Len(t, c.transcript, 1)
	require.Contains(t, c.transcript[0].text, "hello, no tools needed")
}

// TestChatModel_MCPTool_ApproveOnce_ExecutesToolAndCompletes drives the
// approval decision itself ("y") through the real chatModel/stream.go path,
// asserting the actual MCP tool Call (a real network round trip to the
// httptest-backed MCP server) executes and the turn completes with the
// follow-up model response folded into the transcript — the second half of
// the live-tmux repro this package's other MCP tests above don't reach.
func TestChatModel_MCPTool_ApproveOnce_ExecutesToolAndCompletes(t *testing.T) {
	svc := newMCPTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "execute", 80, 24)

	drainCmdWithTimeout(t, c, c.startTurnCmd(svc, ctx, []*message.Message{message.NewText("please use the echo tool to say hi")}), 10*time.Second)
	require.NotNil(t, c.pendingApproval)

	drainCmdWithTimeout(t, c, c.handleApprovalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}}, svc, ctx), 10*time.Second)

	require.Nil(t, c.pendingApproval, "approving must clear the pending prompt")
	// Turn 1 (the request that surfaced the approval prompt) never wrote any
	// text into the streaming buffer — only a ToolApprovalRequestContent,
	// which applyUpdate doesn't accumulate as text — so finishStreaming's
	// "only append if non-empty" check means it contributed no transcript
	// entry of its own; turn 2 (post-approval) is the only one, and it
	// contains both the "[calling echo...]" marker and the final reply.
	require.Len(t, c.transcript, 1)
	require.Contains(t, c.transcript[0].text, "done")
}

// TestRealProgram_MCPTool_AsyncRuntime drives the actual tea.Program runtime
// (real goroutines dispatching tea.Cmd, a real message loop — not
// drainCmd's synchronous simulation) against a chat screen wired to a real
// MCP tool, typing "hi" and pressing enter exactly like a live keypress
// would. This is the closest this test suite can get to the live-tmux
// repro's actual execution model, to rule out (or catch) a race that only
// exists under genuine concurrent Cmd/Update scheduling.
func TestRealProgram_MCPTool_AsyncRuntime(t *testing.T) {
	svc := newMCPTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	m := Model{
		svc:    svc,
		ctx:    ctx,
		screen: screenChat,
		chat:   newChatModel("chat-1", "assistant", nil, "execute", 80, 24),
		width:  80, height: 24,
	}

	p := tea.NewProgram(m, tea.WithoutRenderer(), tea.WithInput(nil), tea.WithoutSignalHandler())

	type runResult struct {
		model tea.Model
		err   error
	}
	resultCh := make(chan runResult, 1)
	go func() {
		fm, err := p.Run()
		resultCh <- runResult{model: fm, err: err}
	}()

	for _, r := range "please use the echo tool to say hi" {
		p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	p.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// Give the real async runtime time to run the fake HTTP round trip and
	// process the resulting streamChunkMsg/streamDoneMsg. There's no direct
	// read access to the live model without racing the Update goroutine, so
	// this just waits a fixed window — generous relative to the fake
	// provider's near-instant response, but far short of the live-tmux
	// repro's 35+ second hang, so a genuine reproduction here still fails
	// fast — then Quits and inspects the final model Run() hands back.
	time.Sleep(500 * time.Millisecond)

	p.Quit()

	select {
	case res := <-resultCh:
		require.NoError(t, res.err)
		finalModel, ok := res.model.(Model)
		require.True(t, ok)
		require.NotNil(t, finalModel.chat, "chat must still be set on the final model")
		require.NotNil(t, finalModel.chat.pendingApproval, "the real async tea.Program runtime must have surfaced the MCP tool's approval prompt, matching the synchronous drainCmd tests above")
	case <-time.After(15 * time.Second):
		t.Fatal("tea.Program.Run() never returned after Quit — reproduces the live hang under the real async runtime")
	}
}

// TestRealProgram_MCPTool_Gemini400Error simulates what a real Gemini API
// might do if it rejects a request whose declared function schema (passed
// through unmodified from the MCP server, e.g. containing "$schema" or other
// keywords Gemini's parametersJsonSchema subset doesn't accept) is
// malformed: a 400 INVALID_ARGUMENT response instead of a hang. This isolates
// whether AgentService/genai's error handling for a *failed* provider call
// involving an MCP tool silently swallows the error instead of surfacing it
// as a streamErrMsg.
func TestRealProgram_MCPTool_Gemini400Error(t *testing.T) {
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "canopy-test-echo", Version: "1.0.0"}, nil)
	mcpServer.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "echoes back the given message",
		InputSchema: map[string]any{
			"$schema":    "http://json-schema.org/draft-07/schema#",
			"type":       "object",
			"properties": map[string]any{"message": map[string]any{"type": "string", "description": "Message to echo"}},
			"required":   []any{"message"},
		},
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo: hi"}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)
	mcpHTTPServer := httptest.NewServer(handler)
	t.Cleanup(mcpHTTPServer.Close)

	client, err := mcpclient.Connect(context.Background(), mcpsource.MCPServerConfig{
		Name: "echo-server", Kind: mcpsource.KindHTTP, URL: mcpHTTPServer.URL,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	mcpTools := make(map[string]tool.Tool, len(client.Tools))
	for _, tl := range client.Tools {
		mcpTools[tl.Name()] = tl
	}

	geminiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Invalid JSON payload received. Unknown name \"$schema\"","status":"INVALID_ARGUMENT"}}`))
	}))
	t.Cleanup(geminiServer.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeGemini, APIKey: "test-key", BaseURL: geminiServer.URL}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gemini-test"}

	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := services.NewAgentService(services.AgentServiceConfig{
		Definitions: services.Definitions{
			Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant", Description: "test agent"}},
		},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Repository:   repo,
		Tools:        services.ToolsConfig{WorkingRoot: t.TempDir()},
		MCPTools:     mcpTools,
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "execute", 80, 24)
	drainCmdWithTimeout(t, c, c.startTurnCmd(svc, ctx, []*message.Message{message.NewText("hi")}), 10*time.Second)

	require.NotNil(t, c.statusErr, "a real 400 from the provider must surface as a rendered error, not be silently swallowed")
	t.Logf("statusErr: %v", c.statusErr)
}
