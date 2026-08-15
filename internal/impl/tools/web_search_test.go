package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockBackend is a hand-rolled WebSearchBackend used to test the tool layer
// (schema, defaulting, error propagation) independently of any real HTTP
// backend — exactly the seam WebSearchBackend exists to provide.
type mockBackend struct {
	results []WebSearchResult
	err     error

	lastQuery      string
	lastMaxResults int
}

func (m *mockBackend) Search(ctx context.Context, query string, maxResults int) ([]WebSearchResult, error) {
	m.lastQuery = query
	m.lastMaxResults = maxResults
	if m.err != nil {
		return nil, m.err
	}
	return m.results, nil
}

func TestNewWebSearchTool_Contract(t *testing.T) {
	st := NewWebSearchTool(WebSearchConfig{Backend: &mockBackend{}})

	var _ tool.FuncTool = st
	assert.Equal(t, "web_search", st.Name())
	assert.NotEmpty(t, st.Description())

	_, approvalGated := st.(tool.ApprovalRequiredTool)
	assert.False(t, approvalGated, "web search must not be approval-gated")
}

func TestNewWebSearchTool_NilBackendPanics(t *testing.T) {
	assert.Panics(t, func() {
		NewWebSearchTool(WebSearchConfig{})
	})
}

func TestNewWebSearchTool_DelegatesToBackend(t *testing.T) {
	mock := &mockBackend{results: []WebSearchResult{
		{Title: "Go", URL: "https://go.dev", Snippet: "The Go programming language"},
	}}
	st := NewWebSearchTool(WebSearchConfig{Backend: mock})

	args, err := json.Marshal(map[string]any{"query": "golang"})
	require.NoError(t, err)

	out, err := st.Call(context.Background(), string(args))
	require.NoError(t, err)

	result, ok := out.(WebSearchOutput)
	require.True(t, ok, "expected WebSearchOutput, got %T", out)
	require.Len(t, result.Results, 1)
	assert.Equal(t, "Go", result.Results[0].Title)
	assert.Equal(t, "golang", mock.lastQuery)
	assert.Equal(t, defaultWebSearchMaxResults, mock.lastMaxResults)
}

func TestNewWebSearchTool_EmptyQueryRejected(t *testing.T) {
	st := NewWebSearchTool(WebSearchConfig{Backend: &mockBackend{}})

	args, err := json.Marshal(map[string]any{"query": ""})
	require.NoError(t, err)

	_, err = st.Call(context.Background(), string(args))
	require.Error(t, err)
}

func TestNewWebSearchTool_BackendErrorPropagates(t *testing.T) {
	mock := &mockBackend{err: assert.AnError}
	st := NewWebSearchTool(WebSearchConfig{Backend: mock})

	args, err := json.Marshal(map[string]any{"query": "golang"})
	require.NoError(t, err)

	_, err = st.Call(context.Background(), string(args))
	require.Error(t, err)
}

// --- SearXNG HTTP backend, exercised against a real local test server ---

func TestSearXNGBackend_ParsesRealHTTPResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/search", r.URL.Path)
		assert.Equal(t, "golang", r.URL.Query().Get("q"))
		assert.Equal(t, "json", r.URL.Query().Get("format"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"results": [
				{"title": "The Go Programming Language", "url": "https://go.dev", "content": "Go is an open source language"},
				{"title": "Go Wikipedia", "url": "https://en.wikipedia.org/wiki/Go", "content": "Go is a statically typed language"}
			]
		}`))
	}))
	defer srv.Close()

	backend := NewSearXNGBackend(SearXNGBackendConfig{BaseURL: srv.URL, HTTPClient: srv.Client()})

	results, err := backend.Search(context.Background(), "golang", 10)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "The Go Programming Language", results[0].Title)
	assert.Equal(t, "https://go.dev", results[0].URL)
	assert.Equal(t, "Go is an open source language", results[0].Snippet)
}

func TestSearXNGBackend_RespectsMaxResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results": [
			{"title": "a", "url": "https://a", "content": "a"},
			{"title": "b", "url": "https://b", "content": "b"},
			{"title": "c", "url": "https://c", "content": "c"}
		]}`))
	}))
	defer srv.Close()

	backend := NewSearXNGBackend(SearXNGBackendConfig{BaseURL: srv.URL, HTTPClient: srv.Client()})

	results, err := backend.Search(context.Background(), "q", 2)
	require.NoError(t, err)
	assert.Len(t, results, 2)
}

func TestSearXNGBackend_NonOKStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	backend := NewSearXNGBackend(SearXNGBackendConfig{BaseURL: srv.URL, HTTPClient: srv.Client()})

	_, err := backend.Search(context.Background(), "q", 10)
	require.Error(t, err)
}

func TestSearXNGBackend_SatisfiesWebSearchBackend(t *testing.T) {
	var _ WebSearchBackend = NewSearXNGBackend(SearXNGBackendConfig{BaseURL: "https://example.org"})
}
