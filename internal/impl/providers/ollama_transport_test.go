package providers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// realisticOllamaToolCallSSEBody reproduces (condensed to two content
// tokens instead of dozens) the exact shape captured live from
// ai.drujensen.com (ollama 0.32.13) during this bug's investigation: a
// reasoning-only content delta, followed by the buggy tool-call delta that
// carries both "content":"" and "tool_calls" in the same object, followed by
// the terminating delta and finish chunk. See ollama_transport.go's package
// comment for the full analysis of why the co-present empty "content" key
// breaks openai-go's ChatCompletionAccumulator.
const realisticOllamaToolCallSSEBody = `data: {"id":"chatcmpl-819","object":"chat.completion.chunk","created":1786892958,"model":"ornith:35b","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning":"The"},"finish_reason":null}]}

data: {"id":"chatcmpl-819","object":"chat.completion.chunk","created":1786892958,"model":"ornith:35b","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"id":"call_gntyca0y","index":0,"type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"Paris\"}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-819","object":"chat.completion.chunk","created":1786892958,"model":"ornith:35b","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`

// ---------------------------------------------------------------------
// stripEmptyContentAlongsideToolCalls
// ---------------------------------------------------------------------

func TestStripEmptyContentAlongsideToolCalls(t *testing.T) {
	t.Run("removes content when empty alongside tool_calls", func(t *testing.T) {
		parsed := map[string]any{
			"choices": []any{
				map[string]any{
					"delta": map[string]any{
						"content":    "",
						"tool_calls": []any{map[string]any{"id": "call_1"}},
					},
				},
			},
		}
		changed := stripEmptyContentAlongsideToolCalls(parsed)
		assert.True(t, changed)
		delta := parsed["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		_, hasContent := delta["content"]
		assert.False(t, hasContent)
		assert.NotNil(t, delta["tool_calls"])
	})

	t.Run("leaves non-empty content alongside tool_calls untouched", func(t *testing.T) {
		parsed := map[string]any{
			"choices": []any{
				map[string]any{
					"delta": map[string]any{
						"content":    "some real text",
						"tool_calls": []any{map[string]any{"id": "call_1"}},
					},
				},
			},
		}
		changed := stripEmptyContentAlongsideToolCalls(parsed)
		assert.False(t, changed)
		delta := parsed["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		assert.Equal(t, "some real text", delta["content"])
	})

	t.Run("leaves content-only delta untouched", func(t *testing.T) {
		parsed := map[string]any{
			"choices": []any{
				map[string]any{"delta": map[string]any{"content": ""}},
			},
		}
		changed := stripEmptyContentAlongsideToolCalls(parsed)
		assert.False(t, changed)
	})

	t.Run("leaves empty tool_calls array untouched", func(t *testing.T) {
		parsed := map[string]any{
			"choices": []any{
				map[string]any{
					"delta": map[string]any{"content": "", "tool_calls": []any{}},
				},
			},
		}
		changed := stripEmptyContentAlongsideToolCalls(parsed)
		assert.False(t, changed)
	})

	t.Run("no choices key", func(t *testing.T) {
		assert.False(t, stripEmptyContentAlongsideToolCalls(map[string]any{}))
	})

	t.Run("multiple choices, only matching one patched", func(t *testing.T) {
		parsed := map[string]any{
			"choices": []any{
				map[string]any{"delta": map[string]any{"content": "keep me"}},
				map[string]any{
					"delta": map[string]any{
						"content":    "",
						"tool_calls": []any{map[string]any{"id": "call_1"}},
					},
				},
			},
		}
		changed := stripEmptyContentAlongsideToolCalls(parsed)
		assert.True(t, changed)
		choices := parsed["choices"].([]any)
		d0 := choices[0].(map[string]any)["delta"].(map[string]any)
		assert.Equal(t, "keep me", d0["content"])
		d1 := choices[1].(map[string]any)["delta"].(map[string]any)
		_, hasContent := d1["content"]
		assert.False(t, hasContent)
	})
}

// ---------------------------------------------------------------------
// splitOllamaSSERaw
// ---------------------------------------------------------------------

func TestSplitOllamaSSERaw(t *testing.T) {
	t.Run("LF-delimited event, delimiter retained", func(t *testing.T) {
		data := []byte("data: {\"a\":1}\n\nmore")
		advance, token, err := splitOllamaSSERaw(data, false)
		require.NoError(t, err)
		assert.Equal(t, "data: {\"a\":1}\n\n", string(token))
		assert.Equal(t, len(token), advance)
	})

	t.Run("CRLF-delimited event, delimiter retained", func(t *testing.T) {
		data := []byte("data: {\"a\":1}\r\n\r\nmore")
		advance, token, err := splitOllamaSSERaw(data, false)
		require.NoError(t, err)
		assert.Equal(t, "data: {\"a\":1}\r\n\r\n", string(token))
		assert.Equal(t, len(token), advance)
	})

	t.Run("incomplete event requests more data", func(t *testing.T) {
		advance, token, err := splitOllamaSSERaw([]byte("data: {\"a\":1"), false)
		require.NoError(t, err)
		assert.Nil(t, token)
		assert.Equal(t, 0, advance)
	})

	t.Run("undelimited final fragment at EOF", func(t *testing.T) {
		data := []byte("data: {\"a\":1}")
		advance, token, err := splitOllamaSSERaw(data, true)
		require.NoError(t, err)
		assert.Equal(t, string(data), string(token))
		assert.Equal(t, len(data), advance)
	})

	t.Run("empty data at EOF signals done", func(t *testing.T) {
		advance, token, err := splitOllamaSSERaw(nil, true)
		require.NoError(t, err)
		assert.Nil(t, token)
		assert.Equal(t, 0, advance)
	})
}

// ---------------------------------------------------------------------
// patchOllamaSSEToken
// ---------------------------------------------------------------------

func TestPatchOllamaSSEToken_StripsEmptyContent(t *testing.T) {
	raw := []byte(`data: {"choices":[{"index":0,"delta":{"content":"","tool_calls":[{"id":"call_1","function":{"name":"f","arguments":"{}"}}]}}]}` + "\n\n")
	out := patchOllamaSSEToken(raw)
	require.True(t, bytes.HasSuffix(out, []byte("\n\n")))
	require.True(t, bytes.HasPrefix(out, []byte("data:")))
	assert.NotContains(t, string(out), `"content":""`)
	assert.Contains(t, string(out), `"tool_calls"`)
}

func TestPatchOllamaSSEToken_NoToolCalls_Passthrough(t *testing.T) {
	raw := []byte(`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n")
	out := patchOllamaSSEToken(raw)
	assert.Equal(t, raw, out)
}

func TestPatchOllamaSSEToken_Done_Passthrough(t *testing.T) {
	raw := []byte("data: [DONE]\n\n")
	out := patchOllamaSSEToken(raw)
	assert.Equal(t, raw, out)
}

func TestPatchOllamaSSEToken_MalformedJSON_Passthrough(t *testing.T) {
	raw := []byte(`data: {"tool_calls": not valid json` + "\n\n")
	out := patchOllamaSSEToken(raw)
	assert.Equal(t, raw, out)
}

func TestPatchOllamaSSEToken_NonDataField_Passthrough(t *testing.T) {
	raw := []byte("event: tool_calls-marker\n\n")
	out := patchOllamaSSEToken(raw)
	assert.Equal(t, raw, out)
}

func TestPatchOllamaSSEToken_BlankTokenPassthrough(t *testing.T) {
	raw := []byte("\n\n")
	out := patchOllamaSSEToken(raw)
	assert.Equal(t, raw, out)
}

// ---------------------------------------------------------------------
// RoundTrip: fail-open behavior
//
// roundTripFunc and mustRequest are shared with gemini_transport_test.go
// (same package) — not redefined here.
// ---------------------------------------------------------------------

func newSSEResponse(status int, body string) *http.Response {
	h := make(http.Header)
	h.Set("Content-Type", "text/event-stream")
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: h}
}

func newJSONResponsePlain(status int, body string) *http.Response {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: h}
}

func TestOllamaRoundTrip_Streaming_StripsBuggyContent(t *testing.T) {
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return newSSEResponse(200, realisticOllamaToolCallSSEBody), nil
	})
	transport := newOllamaToolCallContentStrippingTransport(base)

	req := mustRequest(t, "http://example.invalid/v1/chat/completions")
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)

	var buf bytes.Buffer
	_, err = buf.ReadFrom(resp.Body)
	require.NoError(t, err)
	out := buf.String()

	// The tool_calls event's own delta must have had "content" removed...
	events := strings.Split(strings.TrimRight(out, "\n"), "\n\n")
	require.GreaterOrEqual(t, len(events), 2)
	toolCallEvent := events[1]
	assert.Contains(t, toolCallEvent, `"tool_calls"`)
	assert.NotContains(t, toolCallEvent, `"content"`)

	// ...but the earlier, unrelated content-only (reasoning) event must be
	// left completely untouched, empty "content" and all — this transport
	// only strips "content" when it co-occurs with "tool_calls" in the same
	// delta, never on a plain content/reasoning-only chunk.
	reasoningEvent := events[0]
	assert.Contains(t, reasoningEvent, `"content":""`)
	assert.Contains(t, reasoningEvent, `"reasoning":"The"`)
}

func TestOllamaRoundTrip_NonEventStream_PassesThroughUnmodified(t *testing.T) {
	original := `{"choices":[{"message":{"content":"hi","tool_calls":null}}]}`
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return newJSONResponsePlain(200, original), nil
	})
	transport := newOllamaToolCallContentStrippingTransport(base)

	req := mustRequest(t, "http://example.invalid/v1/chat/completions")
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)

	var buf bytes.Buffer
	_, err = buf.ReadFrom(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, original, buf.String())
}

func TestOllamaRoundTrip_NonSuccessStatus_PassesThroughUnmodified(t *testing.T) {
	errorBody := `{"error":"boom, tool_calls unrelated"}`
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return newSSEResponse(500, errorBody), nil
	})
	transport := newOllamaToolCallContentStrippingTransport(base)

	req := mustRequest(t, "http://example.invalid/v1/chat/completions")
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 500, resp.StatusCode)

	var buf bytes.Buffer
	_, err = buf.ReadFrom(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, errorBody, buf.String())
}

func TestOllamaRoundTrip_TransportError_PropagatedUnchanged(t *testing.T) {
	wantErr := errors.New("boom: connection refused")
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, wantErr
	})
	transport := newOllamaToolCallContentStrippingTransport(base)

	req := mustRequest(t, "http://example.invalid/v1/chat/completions")
	resp, respErr := transport.RoundTrip(req)
	assert.Nil(t, resp)
	assert.ErrorIs(t, respErr, wantErr)
}

func TestNewOllamaToolCallContentStrippingTransport_NilBaseClonesDefaultTransport(t *testing.T) {
	transport := newOllamaToolCallContentStrippingTransport(nil)
	require.NotNil(t, transport)
	concrete, ok := transport.(*ollamaToolCallContentStrippingTransport)
	require.True(t, ok)
	assert.NotNil(t, concrete.base)
}

// ---------------------------------------------------------------------
// End-to-end regression test: proves the actual SDK bug is fixed.
//
// This runs the exact same openai-go call pattern and
// ChatCompletionAccumulator API that agent-framework-go's
// provider/openaiprovider (chat.go) uses internally. Without the
// content-stripping transport installed, replaying this exact response
// shape against openai-go's accumulator never calls
// JustFinishedToolCall() with ok=true (confirmed during this bug's
// investigation, both against the live ai.drujensen.com server and this
// synthetic reproduction) — meaning agent-framework-go would silently drop
// the tool call. With the transport installed, it must fire exactly once
// with the correct name/arguments/id.
// ---------------------------------------------------------------------

func TestOllamaTransport_FixesAccumulatorToolCallDetection_EndToEnd(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(realisticOllamaToolCallSSEBody))
	}))
	defer server.Close()

	client := openai.NewClient(
		option.WithBaseURL(server.URL),
		option.WithAPIKey("test-key"),
		option.WithHTTPClient(newOllamaHTTPClient(nil)),
	)

	params := openai.ChatCompletionNewParams{
		Model: "ornith:35b",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("What is the weather in Paris?"),
		},
	}

	stream := client.Chat.Completions.NewStreaming(context.Background(), params)
	defer func() { _ = stream.Close() }()

	var acc openai.ChatCompletionAccumulator
	var finishedCalls []openai.FinishedChatCompletionToolCall
	for stream.Next() {
		chunk := stream.Current()
		if !acc.AddChunk(chunk) {
			continue
		}
		if tc, ok := acc.JustFinishedToolCall(); ok {
			finishedCalls = append(finishedCalls, tc)
		}
	}
	require.NoError(t, stream.Err())

	require.Len(t, finishedCalls, 1, "the tool call must be surfaced exactly once via JustFinishedToolCall")
	assert.Equal(t, "get_weather", finishedCalls[0].Name)
	assert.Equal(t, `{"location":"Paris"}`, finishedCalls[0].Arguments)
	assert.Equal(t, "call_gntyca0y", finishedCalls[0].ID)
}
