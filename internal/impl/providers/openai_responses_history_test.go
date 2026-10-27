package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
)

func inputTypes(t *testing.T, body []byte) []string {
	t.Helper()
	var payload struct {
		Input []map[string]any `json:"input"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))
	var out []string
	for _, it := range payload.Input {
		typ, _ := it["type"].(string)
		if typ == "" {
			typ, _ = it["role"].(string)
		}
		out = append(out, typ)
	}
	return out
}

func TestPruneUnreplayableReasoning(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			"keeps reasoning with id before assistant output",
			`{"input":[{"role":"user","content":"hi"},{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"x"},{"type":"function_call","call_id":"c","name":"n","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"ok"}]}`,
			[]string{"user", "reasoning", "function_call", "function_call_output"},
		},
		{
			"drops reasoning without id",
			`{"input":[{"role":"user","content":"hi"},{"type":"reasoning","id":"","summary":[],"encrypted_content":"x"},{"type":"function_call","call_id":"c","name":"n","arguments":"{}"}]}`,
			[]string{"user", "function_call"},
		},
		{
			"drops reasoning orphaned by removed output",
			`{"input":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"x"},{"role":"user","content":"next"}]}`,
			[]string{"user"},
		},
		{
			"drops trailing reasoning",
			`{"input":[{"role":"user","content":"hi"},{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"x"}]}`,
			[]string{"user"},
		},
		{"no reasoning leaves body untouched", `{"input":[{"role":"user","content":"hi"}]}`, []string{"user"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, inputTypes(t, pruneUnreplayableReasoning([]byte(tt.in))))
		})
	}
	t.Run("non json untouched", func(t *testing.T) {
		assert.Equal(t, "nope", string(pruneUnreplayableReasoning([]byte("nope"))))
	})
}

// A history reloaded from disk has no RawRepresentation, so the reasoning
// item's id is gone; the request must still be sendable.
func TestNew_OpenAI_ReloadedReasoningHistoryIsSendable(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(responsesAPIResponse("ok"))
	}))
	defer server.Close()

	cfg := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk", BaseURL: server.URL + "/v1"}
	a, err := New(context.Background(), cfg, entities.ModelConfig{Name: "m", Provider: "p", ModelName: "gpt-test"}, agent.Config{Name: "a"})
	require.NoError(t, err)

	history := []*message.Message{
		message.NewText("first"),
		{Role: message.RoleAssistant, Contents: []message.Content{
			&message.TextReasoningContent{ProtectedData: "enc", Text: ""},
			&message.TextContent{Text: "answer"},
		}},
		message.NewText("second"),
	}
	_, err = a.Run(context.Background(), history).Collect()
	require.NoError(t, err)

	assert.NotContains(t, inputTypes(t, body), "reasoning")
	assert.Contains(t, string(body), `"store":false`)
	assert.NotContains(t, string(body), "previous_response_id")
	assert.Contains(t, string(body), "first")
	assert.Contains(t, string(body), "answer")
}
