package services

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/microsoft/agent-framework-go/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// geminiEmptyCallIDFunctionCallResponse mirrors geminiFunctionCallResponse
// (mcp_stream_test.go) but omits the functionCall "id" field entirely,
// matching the real Gemini API — captured from a real canopy binary talking
// to the real Gemini API through a real stdio MCP server: every function
// call it returned had no "id" at all. Every existing fake-Gemini-server
// fixture in this package's tests (geminiFunctionCallResponse included)
// hand-sets a fake "id" real traffic never actually sends, which is exactly
// why none of them caught this.
func geminiEmptyCallIDFunctionCallResponse(funcName string, args map[string]any) map[string]any {
	return map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"role": "model",
					"parts": []any{
						map[string]any{
							"functionCall": map[string]any{
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

// TestGeminiEmptyCallID_ApproveThenContinue reproduces the exact error
// captured live from a real canopy binary, talking to the real Gemini API,
// through a real stdio MCP server, the moment a pending MCP tool approval
// was answered: "geminiprovider: missing function name for result with call
// ID \"\"". It also asserts the second turn's actual request body is
// well-formed (a functionCall part followed by a matching-by-name
// functionResponse part), not just that no error was returned — guarding
// against a regression that "happens not to error" without sending Gemini a
// request it could actually act on.
func TestGeminiEmptyCallID_ApproveThenContinue(t *testing.T) {
	var callCount int32
	var secondRequestBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			_ = json.NewEncoder(w).Encode(geminiEmptyCallIDFunctionCallResponse("echo", map[string]any{"message": "hi"}))
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
		result1, err := drainStreamForTest(t, svc, ctx, "chat-1", []*message.Message{message.NewText("please echo hi")})
		require.NoError(t, err)
		approvalReq := findApprovalRequest(t, lastMessage(t, result1.Response))
		require.NotNil(t, approvalReq)
		require.Equal(t, "", approvalReq.ToolCall.GetCallID(), "sanity check: the fake server must reproduce the real Gemini API's empty CallID")

		approvalMsg := &message.Message{
			Role:     message.RoleUser,
			Contents: []message.Content{approvalReq.CreateResponse(true, "")},
		}
		result2, err := drainStreamForTest(t, svc, ctx, "chat-1", []*message.Message{approvalMsg})
		require.NoError(t, err, "approving a real (empty-CallID) Gemini function call must not fail")
		require.NotNil(t, result2)
		assert.Contains(t, result2.Response.String(), "done")

		require.NotEmpty(t, secondRequestBody, "the second turn must have actually reached the fake Gemini backend")
		assert.Contains(t, string(secondRequestBody), `"functionCall"`,
			"the second request must include the reconstructed assistant function-call part")
		assert.Contains(t, string(secondRequestBody), `"functionResponse"`,
			"the second request must include the function's result")
		assert.Contains(t, string(secondRequestBody), `"name":"echo"`,
			"the reconstructed function call/response must name the tool")
	})
}

// TestGeminiEmptyCallID_RunMessages_ApproveThenContinue is
// TestGeminiEmptyCallID_ApproveThenContinue's non-streaming (RunMessages)
// counterpart — withReconstructedApprovalFunctionCalls's chat-history
// reconciliation is applied in both RunMessages and RunMessagesStream (see
// agent_service.go), so both entry points need their own coverage.
func TestGeminiEmptyCallID_RunMessages_ApproveThenContinue(t *testing.T) {
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			_ = json.NewEncoder(w).Encode(geminiEmptyCallIDFunctionCallResponse("echo", map[string]any{"message": "hi"}))
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
		result1, err := svc.RunText(ctx, "chat-1", "please echo hi")
		require.NoError(t, err)
		approvalReq := findApprovalRequest(t, lastMessage(t, result1.Response))
		require.NotNil(t, approvalReq)

		approvalMsg := &message.Message{
			Role:     message.RoleUser,
			Contents: []message.Content{approvalReq.CreateResponse(true, "")},
		}
		result2, err := svc.RunMessages(ctx, "chat-1", []*message.Message{approvalMsg})
		require.NoError(t, err, "approving a real (empty-CallID) Gemini function call via RunMessages must not fail")
		assert.Contains(t, result2.Response.String(), "done")
	})
}
