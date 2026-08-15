package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/impl/mcpsource"
)

// helperServerEnv, when set to "1" in the test binary's own environment,
// makes TestMain run a tiny real MCP server over stdio instead of the normal
// test suite — the standard Go "re-exec the test binary as a subprocess"
// trick (see os/exec's own tests) used here so TestConnect's stdio case
// spawns and speaks to a genuine subprocess via mcpsource.KindStdio's real
// Command/Args/Env path, not a mock.
const helperServerEnv = "CANOPY_MCPCLIENT_TEST_HELPER_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(helperServerEnv) == "1" {
		runHelperMCPServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runHelperMCPServer exposes one trivial "echo" tool over the process's own
// stdin/stdout, matching what mcp.CommandTransport expects on the other end.
func runHelperMCPServer() {
	server := newHelperMCPServer()
	// Run blocks until the parent closes the connection (session.Close on
	// the client side closes this process's stdin).
	_ = server.Run(context.Background(), &mcp.StdioTransport{})
}

// newHelperMCPServer builds the same tiny "echo" tool server used by both
// the stdio (real subprocess) and HTTP/SSE (real httptest.Server) test
// paths below, so every transport is exercised against identical tool
// behavior.
func newHelperMCPServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "canopy-test-helper", Version: "1.0.0"}, nil)
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
		if err := unmarshalArgs(req, &args); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + args.Message}},
		}, nil
	})
	return server
}

func unmarshalArgs(req *mcp.CallToolRequest, v any) error {
	return jsonUnmarshal(req.Params.Arguments, v)
}

// jsonUnmarshal decodes raw JSON tool-call arguments into v. req.Params is a
// *mcp.CallToolParamsRaw (mcp.CallToolRequest = ServerRequest[*CallToolParamsRaw],
// per go-sdk's requests.go), whose Arguments field is a json.RawMessage — the
// server-side handler's raw, not-yet-decoded view of the arguments the client
// sent, as opposed to mcp.CallToolParams.Arguments (any) used on the client
// call path. A plain json.Unmarshal is all that's needed here.
func jsonUnmarshal(data json.RawMessage, v any) error {
	return json.Unmarshal(data, v)
}

// TestConnect_Stdio_RealSubprocess spawns the actual test binary re-executed
// as an MCP server (see TestMain), connects to it exactly the way
// mcpclient.Connect would connect to a real user-configured stdio server,
// lists its tools, and calls the echo tool end-to-end over the real
// subprocess's stdin/stdout — a genuine MCP protocol round trip, not a fake.
func TestConnect_Stdio_RealSubprocess(t *testing.T) {
	cfg := mcpsource.MCPServerConfig{
		Name:    "helper",
		Kind:    mcpsource.KindStdio,
		Command: os.Args[0],
		Env:     map[string]string{helperServerEnv: "1"},
	}

	client, err := Connect(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	require.Len(t, client.Tools, 1)
	echoTool := client.Tools[0]
	assert.Equal(t, "echo", echoTool.Name())

	approval, ok := echoTool.(tool.ApprovalRequiredTool)
	require.True(t, ok, "MCP tools must implement tool.ApprovalRequiredTool")
	assert.True(t, approval.ApprovalRequired(), "MCP tools must default to approval-required")

	funcTool, ok := echoTool.(tool.FuncTool)
	require.True(t, ok)
	result, err := funcTool.Call(context.Background(), `{"message":"hello"}`)
	require.NoError(t, err)
	assert.Contains(t, resultText(t, result), "echo: hello")
}

// TestConnect_Stdio_BadCommandFailsCleanly asserts a misconfigured stdio
// server (a command that doesn't exist) surfaces as a clear error from
// Connect, not a panic or a hang — the failure mode Canopy must tolerate
// per Requirements §7 ("Canopy should still function with its core tools if
// one configured MCP server is broken").
func TestConnect_Stdio_BadCommandFailsCleanly(t *testing.T) {
	cfg := mcpsource.MCPServerConfig{
		Name:    "broken",
		Kind:    mcpsource.KindStdio,
		Command: "this-command-does-not-exist-anywhere-1234",
	}

	client, err := Connect(context.Background(), cfg)
	require.Error(t, err)
	assert.Nil(t, client)
	assert.Contains(t, err.Error(), "broken")
}

// TestConnect_HTTP_RealServer spins up a real net/http server speaking the
// MCP streamable-HTTP transport (mcp.NewStreamableHTTPHandler) and connects
// to it exactly the way an http-kind .mcp.json entry would, verifying tool
// discovery and a real tool call round trip over real HTTP.
func TestConnect_HTTP_RealServer(t *testing.T) {
	server := newHelperMCPServer()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	cfg := mcpsource.MCPServerConfig{Name: "helper-http", Kind: mcpsource.KindHTTP, URL: httpServer.URL}

	client, err := Connect(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	require.Len(t, client.Tools, 1)
	funcTool := client.Tools[0].(tool.FuncTool)
	result, err := funcTool.Call(context.Background(), `{"message":"over http"}`)
	require.NoError(t, err)
	assert.Contains(t, resultText(t, result), "echo: over http")
}

// TestConnect_HTTP_UnreachableURLFailsCleanly mirrors
// TestConnect_Stdio_BadCommandFailsCleanly for the remote-transport case.
func TestConnect_HTTP_UnreachableURLFailsCleanly(t *testing.T) {
	cfg := mcpsource.MCPServerConfig{Name: "unreachable", Kind: mcpsource.KindHTTP, URL: "http://127.0.0.1:1/does-not-exist"}

	client, err := Connect(context.Background(), cfg)
	require.Error(t, err)
	assert.Nil(t, client)
	assert.Contains(t, err.Error(), "unreachable")
}

// TestConnect_SSE_RealServer is TestConnect_HTTP_RealServer's counterpart
// for the sse-kind transport (mcp.NewSSEHandler).
func TestConnect_SSE_RealServer(t *testing.T) {
	server := newHelperMCPServer()
	handler := mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return server }, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	cfg := mcpsource.MCPServerConfig{Name: "helper-sse", Kind: mcpsource.KindSSE, URL: httpServer.URL}

	client, err := Connect(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	require.Len(t, client.Tools, 1)
	funcTool := client.Tools[0].(tool.FuncTool)
	result, err := funcTool.Call(context.Background(), `{"message":"over sse"}`)
	require.NoError(t, err)
	assert.Contains(t, resultText(t, result), "echo: over sse")
}

// TestConnectAll_SkipsBrokenServerButKeepsWorkingOnes asserts the core
// availability guarantee: one broken configured server must not stop the
// others from connecting, and ConnectAll must report the failure rather
// than swallowing it.
func TestConnectAll_SkipsBrokenServerButKeepsWorkingOnes(t *testing.T) {
	server := newHelperMCPServer()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	servers := map[string]mcpsource.MCPServerConfig{
		"working": {Name: "working", Kind: mcpsource.KindHTTP, URL: httpServer.URL},
		"broken":  {Name: "broken", Kind: mcpsource.KindStdio, Command: "this-command-does-not-exist-anywhere-1234"},
	}

	reg, errs := ConnectAll(context.Background(), servers, nil)
	t.Cleanup(func() { _ = reg.Close() })

	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), "broken")

	tools := reg.Tools()
	require.Len(t, tools, 1)
	_, ok := tools["echo"]
	assert.True(t, ok, "the working server's tool must still be registered")
}

// TestConnectAll_ToolNameCollision_FirstWins asserts a name collision
// between two servers' tools doesn't silently overwrite or panic: the
// alphabetically-first server's tool wins and the collision doesn't prevent
// the rest of the registry from being usable.
func TestConnectAll_ToolNameCollision_FirstWins(t *testing.T) {
	serverA := newHelperMCPServer()
	handlerA := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return serverA }, nil)
	httpServerA := httptest.NewServer(handlerA)
	t.Cleanup(httpServerA.Close)

	serverB := newHelperMCPServer()
	handlerB := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return serverB }, nil)
	httpServerB := httptest.NewServer(handlerB)
	t.Cleanup(httpServerB.Close)

	servers := map[string]mcpsource.MCPServerConfig{
		"a-server": {Name: "a-server", Kind: mcpsource.KindHTTP, URL: httpServerA.URL},
		"b-server": {Name: "b-server", Kind: mcpsource.KindHTTP, URL: httpServerB.URL},
	}

	reg, errs := ConnectAll(context.Background(), servers, nil)
	t.Cleanup(func() { _ = reg.Close() })

	assert.Empty(t, errs)
	tools := reg.Tools()
	require.Len(t, tools, 1, "both servers expose an \"echo\" tool; the registry must dedupe by name")
	_, ok := tools["echo"]
	assert.True(t, ok)
}

// resultText extracts the text of a tool.FuncTool.Call result shaped as
// []message.Content, the shape tool/mcptool's Call always returns for a
// successful, non-error MCP result.
func resultText(t *testing.T, result any) string {
	t.Helper()
	contents, ok := result.([]interface {
		String() string
	})
	if ok && len(contents) > 0 {
		return contents[0].String()
	}
	// Fall back to a %v rendering if the exact interface shape above doesn't
	// match (message.Content's concrete types all implement fmt.Stringer in
	// this codebase's framework version, but this keeps the test from
	// panicking if that ever changes).
	return toString(result)
}

// toString renders a tool.FuncTool.Call result for assertion purposes. The
// real return value (see agent-framework-go's tool/mcptool.mcpWrapper.Call)
// is []message.Content, and message.Content itself (message/content.go) does
// not declare a String() method — only its concrete types (e.g.
// *message.TextContent) do — so the typed assertion above in resultText
// never actually matches and every call here goes through this fallback.
// fmt's %v verb already calls String() on each fmt.Stringer element when
// formatting a slice, so a plain Sprintf is enough to surface
// *message.TextContent's text (e.g. "[echo: hello]") without needing to
// import the message package and type-switch by hand.
func toString(v any) string {
	return fmt.Sprintf("%v", v)
}
