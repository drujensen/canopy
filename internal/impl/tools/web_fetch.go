package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
)

const (
	defaultFetchMaxBytes = 512 * 1024
	defaultFetchTimeout  = 30 * time.Second
)

// WebFetchConfig configures NewWebFetchTool.
type WebFetchConfig struct {
	// HTTPClient is the client used to issue requests. Defaults to a client
	// with DefaultFetchTimeout when nil, so tests can inject one pointed at
	// an httptest server (or one that fails, to test error handling).
	HTTPClient *http.Client

	// MaxBytes caps how much of the response body is read. Zero uses
	// defaultFetchMaxBytes (512 KiB).
	MaxBytes int64
}

// WebFetchInput is the model-facing input for the web-fetch tool.
type WebFetchInput struct {
	URL string `json:"url" jsonschema:"The http(s) URL to fetch."`
}

// WebFetchOutput is the model-facing output for the web-fetch tool.
type WebFetchOutput struct {
	StatusCode  int    `json:"status_code"`
	ContentType string `json:"content_type"`
	Content     string `json:"content"`
	Truncated   bool   `json:"truncated"`
}

// NewWebFetchTool builds Canopy's web-fetch tool (docs/DESIGN.md §3.2 row
// 7): given a URL, returns its raw response body (capped at MaxBytes),
// status code, and content type, so the model can read a webpage. Not
// approval-gated — fetching is non-mutating (it's a GET, not a form submit).
//
// Deliberately scoped down: only http/https schemes are allowed (no file://,
// no reaching into the local network stack indirection like gopher:// etc.);
// there is no HTML-to-text/markdown conversion (unlike Claude Code's own
// WebFetch) — that's judged out of scope for a narrow v1 tool and can be
// layered on later without changing the interface.
func NewWebFetchTool(cfg WebFetchConfig) tool.FuncTool {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultFetchTimeout}
	}
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultFetchMaxBytes
	}

	return functool.MustNew(functool.Config{
		Name:        "web_fetch",
		Description: "Fetch the contents of a web page or other HTTP(S) resource at a given URL.",
	}, func(ctx context.Context, in WebFetchInput) (WebFetchOutput, error) {
		parsed, err := url.Parse(in.URL)
		if err != nil {
			return WebFetchOutput{}, fmt.Errorf("failed to fetch %q: invalid URL: %w", in.URL, err)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return WebFetchOutput{}, fmt.Errorf("failed to fetch %q: unsupported URL scheme %q (only http/https allowed)", in.URL, parsed.Scheme)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL, nil)
		if err != nil {
			return WebFetchOutput{}, fmt.Errorf("failed to fetch %q: %w", in.URL, err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return WebFetchOutput{}, fmt.Errorf("failed to fetch %q: %w", in.URL, err)
		}
		defer resp.Body.Close()

		limited := io.LimitReader(resp.Body, maxBytes+1)
		body, err := io.ReadAll(limited)
		if err != nil {
			return WebFetchOutput{}, fmt.Errorf("failed to fetch %q: %w", in.URL, err)
		}

		truncated := int64(len(body)) > maxBytes
		if truncated {
			body = body[:maxBytes]
		}

		return WebFetchOutput{
			StatusCode:  resp.StatusCode,
			ContentType: resp.Header.Get("Content-Type"),
			Content:     string(body),
			Truncated:   truncated,
		}, nil
	})
}
