package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// SearXNGBackendConfig configures NewSearXNGBackend.
type SearXNGBackendConfig struct {
	// BaseURL is the root of a SearXNG instance, e.g.
	// "https://searx.example.org" (no trailing path). Required.
	BaseURL string

	// HTTPClient is the client used to issue requests. Defaults to a client
	// with a 15s timeout when nil.
	HTTPClient *http.Client
}

// searxngBackend is a WebSearchBackend backed by a SearXNG instance's JSON
// search API.
//
// Concrete-backend assumption (per the task's instruction to document this
// choice rather than silently pick a paid API): SearXNG
// (https://docs.searxng.org/dev/search_api.html) was chosen because it's a
// free, open-source metasearch engine that can be self-hosted with no API
// key, and its JSON output format (?format=json) is simple and stable enough
// to parse without a new dependency. This makes the backend genuinely
// testable in CI against a local httptest.Server that mimics SearXNG's
// response shape, unlike a paid API (Bing, Google Custom Search, Brave
// Search) that would need a live key to exercise. A deployment that wants a
// different provider implements WebSearchBackend directly — this struct is
// one interchangeable implementation, not the only supported one.
type searxngBackend struct {
	baseURL string
	client  *http.Client
}

// NewSearXNGBackend returns a WebSearchBackend that queries a SearXNG
// instance's JSON search API. See the searxngBackend doc comment for why
// SearXNG was picked as the one concrete backend this package ships.
func NewSearXNGBackend(cfg SearXNGBackendConfig) WebSearchBackend {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &searxngBackend{baseURL: cfg.BaseURL, client: client}
}

type searxngResponse struct {
	Results []searxngResult `json:"results"`
}

type searxngResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

func (b *searxngBackend) Search(ctx context.Context, query string, maxResults int) ([]WebSearchResult, error) {
	endpoint := b.baseURL + "/search?" + url.Values{
		"q":      {query},
		"format": {"json"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build search request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to reach search backend: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search backend returned status %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read search response: %w", err)
	}

	var parsed searxngResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse search response: %w", err)
	}

	n := maxResults
	if n <= 0 || n > len(parsed.Results) {
		n = len(parsed.Results)
	}
	results := make([]WebSearchResult, 0, n)
	for i := 0; i < n; i++ {
		r := parsed.Results[i]
		results = append(results, WebSearchResult{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Content,
		})
	}
	return results, nil
}
