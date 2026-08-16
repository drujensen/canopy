package modelsdev

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtureBody is a trimmed real response shape, captured from a live fetch
// of https://models.dev/api.json (three providers, a couple of models each)
// — not hand-invented, so field names/nesting match production exactly,
// including the surprises noted in modelsdev.go's doc comments: no "type"
// field, "api" present for deepseek but absent for openai/anthropic/google.
// overrideCatalogURLForTest points catalogURL at an httptest.Server's URL
// for the duration of a test and restores it automatically via t.Cleanup.
// The returned func is a no-op kept only so call sites reading `defer
// restore()` or `_ = origURL` read naturally; t.Cleanup already guarantees
// restoration.
func overrideCatalogURLForTest(t *testing.T, url string) func() {
	t.Helper()
	original := catalogURL
	catalogURL = url
	t.Cleanup(func() { catalogURL = original })
	return func() {}
}

const fixtureBody = `{
  "openai": {
    "id": "openai",
    "name": "OpenAI",
    "env": ["OPENAI_API_KEY"],
    "models": {
      "gpt-4o": {
        "id": "gpt-4o",
        "name": "GPT-4o",
        "family": "gpt",
        "attachment": true,
        "reasoning": false,
        "tool_call": true,
        "temperature": true,
        "release_date": "2024-05-13",
        "limit": {"context": 128000, "output": 16384},
        "cost": {"input": 2.5, "output": 10, "cache_read": 1.25, "cache_write": 3.75}
      }
    }
  },
  "anthropic": {
    "id": "anthropic",
    "name": "Anthropic",
    "env": ["ANTHROPIC_API_KEY"],
    "models": {
      "claude-opus-4-7": {
        "id": "claude-opus-4-7",
        "name": "Claude Opus 4.7",
        "family": "claude-opus",
        "attachment": true,
        "reasoning": true,
        "tool_call": true,
        "temperature": false,
        "release_date": "2026-04-14",
        "limit": {"context": 1000000, "output": 128000}
      }
    }
  },
  "deepseek": {
    "id": "deepseek",
    "name": "DeepSeek",
    "env": ["DEEPSEEK_API_KEY"],
    "api": "https://api.deepseek.com",
    "models": {
      "deepseek-v4-flash": {
        "id": "deepseek-v4-flash",
        "name": "DeepSeek V4 Flash",
        "family": "deepseek-flash",
        "attachment": false,
        "reasoning": true,
        "tool_call": true,
        "temperature": true,
        "release_date": "2026-07-31",
        "limit": {"context": 1000000, "output": 384000}
      }
    }
  }
}`

func TestFetch_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtureBody))
	}))
	defer server.Close()

	overrideCatalogURLForTest(t, server.URL)

	catalog, err := Fetch(context.Background())
	require.NoError(t, err)
	require.NotNil(t, catalog)

	require.Contains(t, *catalog, "openai")
	require.Contains(t, *catalog, "deepseek")
	assert.Equal(t, []string{"OPENAI_API_KEY"}, (*catalog)["openai"].Env)
	assert.Equal(t, "https://api.deepseek.com", (*catalog)["deepseek"].BaseURL)
	assert.Empty(t, (*catalog)["openai"].BaseURL)

	gpt4o, ok := (*catalog)["openai"].Models["gpt-4o"]
	require.True(t, ok)
	assert.True(t, gpt4o.ToolCall)
	assert.Equal(t, "2024-05-13", gpt4o.ReleaseDate)
	assert.Equal(t, 128000, gpt4o.Limit.Context)
	assert.Equal(t, 2.5, gpt4o.Cost.Input, "cost.input must decode into ModelData.Cost.Input")
	assert.Equal(t, 10.0, gpt4o.Cost.Output, "cost.output must decode into ModelData.Cost.Output")

	// A model whose fixture entry has no "cost" object at all (claude-opus-4-7
	// below) must decode to CostData's zero value rather than erroring.
	opus, ok := (*catalog)["anthropic"].Models["claude-opus-4-7"]
	require.True(t, ok)
	assert.Zero(t, opus.Cost.Input)
	assert.Zero(t, opus.Cost.Output)
}

func TestFetch_NetworkError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close() // closed before use: guarantees a connection error

	restore := overrideCatalogURLForTest(t, url)
	defer restore()

	catalog, err := Fetch(context.Background())
	require.Error(t, err)
	assert.Nil(t, catalog)
}

func TestFetch_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("down for maintenance"))
	}))
	defer server.Close()

	restore := overrideCatalogURLForTest(t, server.URL)
	defer restore()

	catalog, err := Fetch(context.Background())
	require.Error(t, err)
	assert.Nil(t, catalog)
	assert.Contains(t, err.Error(), "503")
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models-cache.json")

	var catalog Catalog
	require.NoError(t, json.Unmarshal([]byte(fixtureBody), &catalog))

	require.NoError(t, Save(path, &catalog))

	loaded, fetchedAt, err := Load(path)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.WithinDuration(t, time.Now(), fetchedAt, 5*time.Second)
	assert.Equal(t, catalog["openai"].Env, (*loaded)["openai"].Env)
	assert.Equal(t, catalog["deepseek"].BaseURL, (*loaded)["deepseek"].BaseURL)
}

func TestLoad_MissingFileIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")

	catalog, fetchedAt, err := Load(path)
	require.NoError(t, err)
	assert.Nil(t, catalog)
	assert.True(t, fetchedAt.IsZero())
}

func TestFetchCached_MissCallsNetwork(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtureBody))
	}))
	defer server.Close()

	restore := overrideCatalogURLForTest(t, server.URL)
	defer restore()

	path := filepath.Join(t.TempDir(), "models-cache.json")

	catalog, refreshed, err := FetchCached(context.Background(), path, 24*time.Hour)
	require.NoError(t, err)
	assert.True(t, refreshed)
	assert.Equal(t, 1, hits)
	require.NotNil(t, catalog)
	assert.Contains(t, *catalog, "openai")

	// The cache file should now exist on disk.
	cached, _, err := Load(path)
	require.NoError(t, err)
	require.NotNil(t, cached)
}

func TestFetchCached_HitDoesNotCallNetwork(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("network should not be called on a cache hit")
	}))
	defer server.Close()

	restore := overrideCatalogURLForTest(t, server.URL)
	defer restore()

	path := filepath.Join(t.TempDir(), "models-cache.json")
	var seed Catalog
	require.NoError(t, json.Unmarshal([]byte(fixtureBody), &seed))
	require.NoError(t, Save(path, &seed))

	catalog, refreshed, err := FetchCached(context.Background(), path, 24*time.Hour)
	require.NoError(t, err)
	assert.False(t, refreshed)
	require.NotNil(t, catalog)
	assert.Contains(t, *catalog, "openai")
}

func TestFetchCached_StaleCacheCallsNetwork(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtureBody))
	}))
	defer server.Close()

	restore := overrideCatalogURLForTest(t, server.URL)
	defer restore()

	path := filepath.Join(t.TempDir(), "models-cache.json")
	var seed Catalog
	require.NoError(t, json.Unmarshal([]byte(fixtureBody), &seed))
	require.NoError(t, Save(path, &seed))

	// maxAge of 0 (or negative) means "always refresh" per FetchCached's doc
	// comment — used by --refresh-providers.
	catalog, refreshed, err := FetchCached(context.Background(), path, 0)
	require.NoError(t, err)
	assert.True(t, refreshed)
	assert.Equal(t, 1, hits)
	require.NotNil(t, catalog)
}

func TestFetchCached_MaxAgeElapsedCallsNetwork(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtureBody))
	}))
	defer server.Close()

	restore := overrideCatalogURLForTest(t, server.URL)
	defer restore()

	path := filepath.Join(t.TempDir(), "models-cache.json")
	var seed Catalog
	require.NoError(t, json.Unmarshal([]byte(fixtureBody), &seed))
	require.NoError(t, Save(path, &seed))

	catalog, refreshed, err := FetchCached(context.Background(), path, 1*time.Nanosecond)
	require.NoError(t, err)
	assert.True(t, refreshed)
	assert.Equal(t, 1, hits)
	require.NotNil(t, catalog)
}
