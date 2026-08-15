package tools

import (
	"context"
	"fmt"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
)

// WebSearchResult is one result from a web search backend.
type WebSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// WebSearchBackend performs the actual search request against whatever
// search provider is configured. The tool itself (NewWebSearchTool) is
// backend-agnostic; production code, tests, and future providers all
// implement this same small interface.
//
// There is no MAF-Go-provided hosted web-search tool suitable for a
// non-hosted/local build (docs/DESIGN.md's whitelist doesn't include one),
// so Canopy defines its own pluggable seam here instead of hardcoding a
// specific paid search API that couldn't be exercised in CI without a live
// key. See NewSearXNGBackend for the one concrete HTTP backend this package
// ships, and its doc comment for why that backend was picked.
type WebSearchBackend interface {
	Search(ctx context.Context, query string, maxResults int) ([]WebSearchResult, error)
}

// WebSearchConfig configures NewWebSearchTool.
type WebSearchConfig struct {
	// Backend performs the actual search. Required — NewWebSearchTool
	// panics if it is nil, since a search tool with no backend can't do
	// anything useful and that's a wiring bug worth catching immediately.
	Backend WebSearchBackend

	// DefaultMaxResults caps the number of results returned when the caller
	// doesn't specify one. Zero means 10.
	DefaultMaxResults int
}

// WebSearchInput is the model-facing input for the web-search tool.
type WebSearchInput struct {
	Query      string `json:"query" jsonschema:"The search query."`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"Maximum number of results to return."`
}

// WebSearchOutput is the model-facing output for the web-search tool.
type WebSearchOutput struct {
	Results []WebSearchResult `json:"results"`
}

const defaultWebSearchMaxResults = 10

// NewWebSearchTool builds Canopy's web-search tool (docs/DESIGN.md §3.2 row
// 6). Not approval-gated — searching is non-mutating. The actual search
// request is delegated to cfg.Backend, so the concrete provider is swappable
// (and mockable in tests) without touching this tool's schema or wiring.
func NewWebSearchTool(cfg WebSearchConfig) tool.FuncTool {
	if cfg.Backend == nil {
		panic("tools: NewWebSearchTool requires a non-nil Backend")
	}
	maxDefault := cfg.DefaultMaxResults
	if maxDefault <= 0 {
		maxDefault = defaultWebSearchMaxResults
	}

	return functool.MustNew(functool.Config{
		Name:        "web_search",
		Description: "Search the web for a query and return matching page titles, URLs, and snippets.",
	}, func(ctx context.Context, in WebSearchInput) (WebSearchOutput, error) {
		if in.Query == "" {
			return WebSearchOutput{}, fmt.Errorf("query must not be empty")
		}
		maxResults := in.MaxResults
		if maxResults <= 0 {
			maxResults = maxDefault
		}

		results, err := cfg.Backend.Search(ctx, in.Query, maxResults)
		if err != nil {
			return WebSearchOutput{}, fmt.Errorf("web search failed: %w", err)
		}
		return WebSearchOutput{Results: results}, nil
	})
}
