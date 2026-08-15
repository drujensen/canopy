package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewWebFetchTool_Contract(t *testing.T) {
	ft := NewWebFetchTool(WebFetchConfig{})

	var _ tool.FuncTool = ft
	assert.Equal(t, "web_fetch", ft.Name())
	assert.NotEmpty(t, ft.Description())

	_, approvalGated := ft.(tool.ApprovalRequiredTool)
	assert.False(t, approvalGated, "web fetch must not be approval-gated")
}

func TestNewWebFetchTool_FetchesRealLocalServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from the web"))
	}))
	defer srv.Close()

	ft := NewWebFetchTool(WebFetchConfig{HTTPClient: srv.Client()})

	args, err := json.Marshal(map[string]string{"url": srv.URL})
	require.NoError(t, err)

	out, err := ft.Call(context.Background(), string(args))
	require.NoError(t, err)

	result, ok := out.(WebFetchOutput)
	require.True(t, ok, "expected WebFetchOutput, got %T", out)
	assert.Equal(t, http.StatusOK, result.StatusCode)
	assert.Equal(t, "hello from the web", result.Content)
	assert.Contains(t, result.ContentType, "text/plain")
	assert.False(t, result.Truncated)
}

func TestNewWebFetchTool_TruncatesLargeBody(t *testing.T) {
	body := strings.Repeat("x", 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	ft := NewWebFetchTool(WebFetchConfig{HTTPClient: srv.Client(), MaxBytes: 10})

	args, err := json.Marshal(map[string]string{"url": srv.URL})
	require.NoError(t, err)

	out, err := ft.Call(context.Background(), string(args))
	require.NoError(t, err)

	result := out.(WebFetchOutput)
	assert.Len(t, result.Content, 10)
	assert.True(t, result.Truncated)
}

func TestNewWebFetchTool_RejectsNonHTTPScheme(t *testing.T) {
	ft := NewWebFetchTool(WebFetchConfig{})

	args, err := json.Marshal(map[string]string{"url": "file:///etc/passwd"})
	require.NoError(t, err)

	_, err = ft.Call(context.Background(), string(args))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported URL scheme")
}

func TestNewWebFetchTool_PropagatesHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	ft := NewWebFetchTool(WebFetchConfig{HTTPClient: srv.Client()})
	args, err := json.Marshal(map[string]string{"url": srv.URL})
	require.NoError(t, err)

	out, err := ft.Call(context.Background(), string(args))
	require.NoError(t, err, "a non-2xx response is not a Go error, just reported via StatusCode")

	result := out.(WebFetchOutput)
	assert.Equal(t, http.StatusNotFound, result.StatusCode)
}
