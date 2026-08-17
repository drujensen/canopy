package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// TavilyBackendConfig configures NewTavilyBackend.
type TavilyBackendConfig struct {
	// APIKey is a Tavily API key (https://tavily.com). Required.
	APIKey string

	// HTTPClient is the client used to issue requests. Defaults to a client
	// with a 15s timeout when nil.
	HTTPClient *http.Client
}

// tavilyBackend is a WebSearchBackend backed by the Tavily Search API
// (https://api.tavily.com/search), a paid third-party search API. It's the
// default zero-config backend (wired from the TAVILY_API_KEY environment
// variable in cmd/canopy) alongside the self-hostable NewSearXNGBackend — a
// deployment that wants a different provider implements WebSearchBackend
// directly.
const tavilyEndpoint = "https://api.tavily.com/search"

type tavilyBackend struct {
	apiKey   string
	client   *http.Client
	endpoint string // overridden in tests; always tavilyEndpoint in production
}

// NewTavilyBackend returns a WebSearchBackend that queries the Tavily Search
// API.
func NewTavilyBackend(cfg TavilyBackendConfig) WebSearchBackend {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &tavilyBackend{apiKey: cfg.APIKey, client: client, endpoint: tavilyEndpoint}
}

type tavilyRequest struct {
	Query string `json:"query"`
}

type tavilyResponse struct {
	Results []tavilyResult `json:"results"`
}

type tavilyResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

func (b *tavilyBackend) Search(ctx context.Context, query string, maxResults int) ([]WebSearchResult, error) {
	body, err := json.Marshal(tavilyRequest{Query: query})
	if err != nil {
		return nil, fmt.Errorf("failed to build search request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to build search request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.apiKey)

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to reach search backend: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search backend returned status %s", resp.Status)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read search response: %w", err)
	}

	var parsed tavilyResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
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
