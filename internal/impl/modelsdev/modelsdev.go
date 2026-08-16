// Package modelsdev fetches and caches the models.dev provider/model catalog
// (https://models.dev/api.json) — a free, unauthenticated, community-
// maintained JSON listing of LLM providers, their models, pricing, and
// context/output limits. Canopy uses it purely for zero-config provider
// auto-detection (see internal/impl/config.DetectProviders); nothing here
// decides *what* to do with the catalog, only how to get and cache it. This
// package deliberately doesn't know about "~/.canopy" or any other Canopy
// directory convention — callers pass an explicit cache file path.
//
// Shape verified directly against a live fetch of the real endpoint (not
// assumed from the predecessor aiagent project's version, which turned out
// to differ in a few ways — see Catalog's doc comment for specifics).
package modelsdev

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// catalogURL is the live models.dev catalog endpoint. A var (not a const)
// solely so tests can point it at an httptest.Server instead of the real
// network — see modelsdev_test.go's overrideCatalogURLForTest.
var catalogURL = "https://models.dev/api.json"

// requestTimeout bounds a single live fetch.
const requestTimeout = 30 * time.Second

// Catalog is the models.dev catalog response, keyed by provider ID (e.g.
// "openai", "anthropic", "google" — note "google", not "gemini"; and
// "togetherai", not "together" — both confirmed against a live fetch).
type Catalog map[string]ProviderData

// ProviderData describes one provider's entry in the models.dev catalog.
//
// Differences confirmed against a live fetch vs. the predecessor aiagent
// project's version of this struct: the real catalog has no "type" field at
// all (aiagent's ProviderData.Type is always empty against real data, so
// this struct drops it). "api" (BaseURL here) is present on only about 160
// of 185 providers as of this writing; several major, well-known providers —
// including groq, togetherai, mistral, and xai — omit it entirely (their
// consuming SDKs presumably bake in a default base URL from the "npm"
// field's package rather than needing this field). Callers that need a
// BaseURL to make a request (see config.DetectProviders) must treat an empty
// BaseURL as "unusable via the generic dispatch path," not as a bug in this
// package.
type ProviderData struct {
	ID      string               `json:"id"`
	Name    string               `json:"name"`
	BaseURL string               `json:"api,omitempty"`
	Env     []string             `json:"env"`
	Models  map[string]ModelData `json:"models"`
}

// ModelData describes one model's entry in the models.dev catalog. Only the
// fields Canopy's auto-detection heuristic and provider config actually use
// are kept (tool-call capability, release date for recency ranking, context
// window size, and — post-v0.1.0 addendum, for the TUI's ctrl+o model
// picker — per-token cost).
type ModelData struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Family      string    `json:"family"`
	Attachment  bool      `json:"attachment"`
	Reasoning   bool      `json:"reasoning"`
	ToolCall    bool      `json:"tool_call"`
	Temperature bool      `json:"temperature"`
	Limit       LimitData `json:"limit"`
	// ReleaseDate is "YYYY-MM-DD" for the large majority of models, but not
	// universally — about 195 of ~6600 models in a live fetch use a coarser
	// "YYYY-MM" (no day). Parse defensively (time.Parse("2006-01-02", ...))
	// and skip rather than error on a model whose date doesn't match.
	ReleaseDate string `json:"release_date"`
	// Cost is omitted entirely by the catalog for some models (e.g. several
	// open-weights entries) rather than sent as zeros — CostData's own zero
	// value already means "unknown," so no special-casing is needed here.
	Cost CostData `json:"cost"`
}

// LimitData describes a model's context/output token limits.
type LimitData struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

// CostData describes a model's price in US dollars per million tokens
// (post-v0.1.0 addendum), confirmed directly against a live fetch: the
// catalog's "cost" object also carries "cache_read"/"cache_write", which
// Canopy doesn't currently surface anywhere and so isn't captured here.
type CostData struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

// Fetch performs a live, unauthenticated HTTP GET against the real
// models.dev catalog endpoint. Callers that want caching/staleness handling
// should use FetchCached instead of calling this directly on every startup.
func Fetch(ctx context.Context) (*Catalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, catalogURL, nil)
	if err != nil {
		return nil, fmt.Errorf("modelsdev: failed to build request: %w", err)
	}
	req.Header.Set("User-Agent", "canopy")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("modelsdev: failed to fetch %s: %w", catalogURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("modelsdev: %s returned status %d: %s", catalogURL, resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("modelsdev: failed to read response body: %w", err)
	}

	var catalog Catalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("modelsdev: failed to decode response: %w", err)
	}
	return &catalog, nil
}

// cacheFile is the on-disk shape of the local cache written by Save and read
// by Load — the catalog plus the time it was fetched, so freshness can be
// judged without relying on the cache file's own mtime.
type cacheFile struct {
	FetchedAt time.Time `json:"fetched_at"`
	Catalog   Catalog   `json:"catalog"`
}

// Load reads a previously Save-d catalog from path. A missing file is not an
// error: it returns (nil, zero time.Time, nil) as a clear "not cached yet"
// signal, matching ProviderStore.Load's "missing file is not an error"
// convention elsewhere in Canopy's config handling.
func Load(path string) (*Catalog, time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, time.Time{}, nil
		}
		return nil, time.Time{}, fmt.Errorf("modelsdev: failed to read cache %q: %w", path, err)
	}

	var cf cacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, time.Time{}, fmt.Errorf("modelsdev: failed to decode cache %q: %w", path, err)
	}
	return &cf.Catalog, cf.FetchedAt, nil
}

// Save writes catalog to path (creating parent directories as needed),
// stamped with the current time as its fetch time.
func Save(path string, catalog *Catalog) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("modelsdev: failed to create cache directory %q: %w", dir, err)
	}

	data, err := json.MarshalIndent(cacheFile{FetchedAt: time.Now(), Catalog: *catalog}, "", "  ")
	if err != nil {
		return fmt.Errorf("modelsdev: failed to marshal cache: %w", err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("modelsdev: failed to write cache %q: %w", path, err)
	}
	return nil
}

// FetchCached returns the models.dev catalog, hitting the network only when
// the local cache at path is missing or older than maxAge — so a normal
// startup doesn't do a network round-trip on every single launch. refreshed
// reports whether a live fetch actually happened (true) or the cache was
// used as-is (false). On a successful live fetch, the result is also saved
// back to path so the next call within maxAge is cache-only.
//
// maxAge <= 0 forces a live fetch unconditionally (used by
// --refresh-providers, Requirements/DESIGN addendum) regardless of the
// existing cache's age.
func FetchCached(ctx context.Context, path string, maxAge time.Duration) (catalog *Catalog, refreshed bool, err error) {
	if maxAge > 0 {
		cached, fetchedAt, loadErr := Load(path)
		if loadErr != nil {
			return nil, false, loadErr
		}
		if cached != nil && time.Since(fetchedAt) < maxAge {
			return cached, false, nil
		}
	}

	fetched, err := Fetch(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := Save(path, fetched); err != nil {
		return nil, false, err
	}
	return fetched, true, nil
}
