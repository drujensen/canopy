package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTavilyBackend_ParsesRealHTTPResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/search", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		var body tavilyRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "golang", body.Query)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"results": [
				{"title": "The Go Programming Language", "url": "https://go.dev", "content": "Go is an open source language"},
				{"title": "Go Wikipedia", "url": "https://en.wikipedia.org/wiki/Go", "content": "Go is a statically typed language"}
			]
		}`))
	}))
	defer srv.Close()

	backend := newTavilyBackendForTest(t, srv)

	results, err := backend.Search(context.Background(), "golang", 10)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "The Go Programming Language", results[0].Title)
	assert.Equal(t, "https://go.dev", results[0].URL)
	assert.Equal(t, "Go is an open source language", results[0].Snippet)
}

func TestTavilyBackend_RespectsMaxResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results": [
			{"title": "a", "url": "https://a", "content": "a"},
			{"title": "b", "url": "https://b", "content": "b"},
			{"title": "c", "url": "https://c", "content": "c"}
		]}`))
	}))
	defer srv.Close()

	backend := newTavilyBackendForTest(t, srv)

	results, err := backend.Search(context.Background(), "q", 2)
	require.NoError(t, err)
	assert.Len(t, results, 2)
}

func TestTavilyBackend_NonOKStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	backend := newTavilyBackendForTest(t, srv)

	_, err := backend.Search(context.Background(), "q", 10)
	require.Error(t, err)
}

func TestTavilyBackend_SatisfiesWebSearchBackend(t *testing.T) {
	var _ WebSearchBackend = NewTavilyBackend(TavilyBackendConfig{APIKey: "test-key"})
}

// newTavilyBackendForTest builds a tavilyBackend pointed at srv instead of
// the real api.tavily.com, since NewTavilyBackend hardcodes the endpoint.
func newTavilyBackendForTest(t *testing.T, srv *httptest.Server) WebSearchBackend {
	t.Helper()
	return &tavilyBackend{apiKey: "test-key", client: srv.Client(), endpoint: srv.URL + "/search"}
}
