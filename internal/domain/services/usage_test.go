package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/microsoft/agent-framework-go/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/impl/agentsource"
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
)

// chatCompletionResponseWithUsage is chatCompletionResponse (agent_service_test.go)
// plus a real "usage" object in OpenAI's own wire shape — the thing that
// shared helper deliberately omits (every other test in this package only
// cares about the reply text) and this file's tests need to exercise
// mergeUsage end to end.
func chatCompletionResponseWithUsage(text string, promptTokens, completionTokens int) map[string]any {
	resp := chatCompletionResponse(text)
	resp["usage"] = map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
	}
	return resp
}

// chatCompletionResponseWithCachedUsage is chatCompletionResponseWithUsage
// plus OpenAI's own "prompt_tokens_details.cached_tokens" breakout —
// promptTokens is the inclusive total (cachedTokens is a subset of it, not
// additional), matching the real wire shape OpenCode/OpenAI-compatible
// endpoints send.
func chatCompletionResponseWithCachedUsage(text string, promptTokens, cachedTokens, completionTokens int) map[string]any {
	resp := chatCompletionResponse(text)
	resp["usage"] = map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": cachedTokens,
		},
	}
	return resp
}

// TestAgentService_RunMessages_PersistsUsageAndCost is the end-to-end
// regression test for Chat.Usage (post-v0.1.0 addendum: the TUI sidebar's
// usage/cost display, and the underlying "store it, don't just show it"
// decision — a later project-wide view can sum this across every chat).
// Drives two real turns against a model with published cost rates and
// confirms token counts and USD cost accumulate correctly across both,
// and that RunResult.Usage matches what's actually on disk.
func TestAgentService_RunMessages_PersistsUsageAndCost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponseWithUsage("ok", 100, 50))
	}))
	t.Cleanup(server.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderType("openai-compatible"), APIKey: "sk-test", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{
		Name: "m", Provider: provider.Name, ModelName: "gpt-test",
		InputCostPerMillionTokens: 2, OutputCostPerMillionTokens: 10,
	}

	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant", Description: "test agent"}},
		},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Repository:   repo,
		Tools:        ToolsConfig{WorkingRoot: t.TempDir()},
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	result, err := svc.RunText(ctx, "chat-1", "hi")
	require.NoError(t, err)
	require.Contains(t, result.Usage, "m")
	assert.Equal(t, int64(100), result.Usage["m"].InputTokens)
	assert.Equal(t, int64(50), result.Usage["m"].OutputTokens)
	// 100 tokens @ $2/M + 50 tokens @ $10/M = 0.0002 + 0.0005 = 0.0007
	assert.InDelta(t, 0.0007, result.Usage["m"].CostUSD, 1e-9)

	// A second turn must accumulate, not overwrite.
	result2, err := svc.RunText(ctx, "chat-1", "hi again")
	require.NoError(t, err)
	assert.Equal(t, int64(200), result2.Usage["m"].InputTokens)
	assert.Equal(t, int64(100), result2.Usage["m"].OutputTokens)
	assert.InDelta(t, 0.0014, result2.Usage["m"].CostUSD, 1e-9)

	chat, err := repo.Get(ctx, "chat-1")
	require.NoError(t, err)
	assert.Equal(t, result2.Usage, chat.Usage, "RunResult.Usage must match what's actually persisted")
}

// TestAgentService_RunMessages_BillsCachedTokensAtCacheReadRate is the
// regression test for the bug this addendum fixes: Canopy used to bill every
// reported input token at the full input rate, even the portion a provider
// (OpenCode, via its "x-opencode-session" prompt-caching header) served from
// cache at a steep discount — on a long chat whose growing history is mostly
// cache hits, that overcounted cost by several times what the provider
// actually billed. promptTokens here is the OpenAI-style *inclusive* total
// (900 of the 1000 were cache hits), so the fresh share billed at the full
// input rate is only 100.
func TestAgentService_RunMessages_BillsCachedTokensAtCacheReadRate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponseWithCachedUsage("ok", 1000, 900, 50))
	}))
	t.Cleanup(server.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderType("opencode"), APIKey: "sk-test", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{
		Name: "m", Provider: provider.Name, ModelName: "gpt-test",
		InputCostPerMillionTokens: 10, OutputCostPerMillionTokens: 10, CacheReadCostPerMillionTokens: 1,
	}

	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant", Description: "test agent"}},
		},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Repository:   repo,
		Tools:        ToolsConfig{WorkingRoot: t.TempDir()},
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	result, err := svc.RunText(ctx, "chat-1", "hi")
	require.NoError(t, err)
	require.Contains(t, result.Usage, "m")
	assert.Equal(t, int64(1000), result.Usage["m"].InputTokens)
	assert.Equal(t, int64(900), result.Usage["m"].CachedInputTokens)
	assert.Equal(t, int64(50), result.Usage["m"].OutputTokens)
	// 100 fresh @ $10/M + 900 cached @ $1/M + 50 out @ $10/M
	// = 0.001 + 0.0009 + 0.0005 = 0.0024
	assert.InDelta(t, 0.0024, result.Usage["m"].CostUSD, 1e-9)
}

// TestAgentService_RunMessages_AnthropicCachedTokensAreAdditiveNotInclusive
// asserts the inverse convention for Anthropic: its API reports
// input_tokens *excluding* any cache_read_input_tokens (the two are disjoint
// counters), unlike OpenAI/OpenCode/Gemini's inclusive "prompt tokens"
// total. Billing this the "subtract cached back out of input" way a
// non-Anthropic provider needs would undercount cost; mergeUsage must bill
// the full reported input_tokens at the input rate and add the cached
// portion on top instead.
func TestAgentService_RunMessages_AnthropicCachedTokensAreAdditiveNotInclusive(t *testing.T) {
	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeAnthropic}
	model := entities.ModelConfig{
		Name: "m", Provider: provider.Name, ModelName: "claude-test",
		InputCostPerMillionTokens: 10, OutputCostPerMillionTokens: 10, CacheReadCostPerMillionTokens: 1,
	}
	chat := &entities.Chat{}
	entries := []usageEntry{{
		modelName: "m",
		usage: message.UsageDetails{
			InputTokenCount:       100,
			CachedInputTokenCount: 900,
			OutputTokenCount:      50,
		},
	}}

	mergeUsage(chat, entries, map[string]entities.ModelConfig{"m": model}, map[string]entities.ProviderConfig{"p": provider})

	require.Contains(t, chat.Usage, "m")
	assert.Equal(t, int64(100), chat.Usage["m"].InputTokens)
	assert.Equal(t, int64(900), chat.Usage["m"].CachedInputTokens)
	// 100 input @ $10/M (no subtraction) + 900 cached @ $1/M + 50 out @ $10/M
	// = 0.001 + 0.0009 + 0.0005 = 0.0024
	assert.InDelta(t, 0.0024, chat.Usage["m"].CostUSD, 1e-9)
}

// TestAgentService_RunMessages_PersistsSubagentUsage proves subagent
// dispatch usage is actually captured and billed at the *subagent's own*
// model rate, not silently dropped the way tool/agenttool.New's stock
// Call would (see newUsageTrackingSubagentTool's doc comment) and not
// misattributed to the parent's model. Two separately-provided models back
// two agents ("assistant", the top-level one driving this chat, and
// "helper", loaded only so buildTopLevelTools wires it in as a
// subagent-dispatch tool); the parent's own follow-up reply deliberately
// reports zero usage, so any nonzero usage recorded under the parent's
// model name would prove misattribution, not a passing test.
func TestAgentService_RunMessages_PersistsSubagentUsage(t *testing.T) {
	var parentCalls atomic.Int32
	parentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if parentCalls.Add(1) == 1 {
			// First call: the parent model requests the subagent-dispatch
			// tool (named after the sanitized "helper" agent name).
			_ = json.NewEncoder(w).Encode(toolCallChatCompletionResponse("call-1", "helper", `{"query":"do it"}`))
			return
		}
		// Second call: the parent's own follow-up reply.
		_ = json.NewEncoder(w).Encode(chatCompletionResponseWithUsage("done", 0, 0))
	}))
	t.Cleanup(parentServer.Close)

	subServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponseWithUsage("sub result", 30, 20))
	}))
	t.Cleanup(subServer.Close)

	parentProvider := entities.ProviderConfig{Name: "parent-provider", Type: entities.ProviderType("openai-compatible"), APIKey: "sk-test", BaseURL: parentServer.URL + "/v1"}
	subProvider := entities.ProviderConfig{Name: "sub-provider", Type: entities.ProviderType("openai-compatible"), APIKey: "sk-test", BaseURL: subServer.URL + "/v1"}
	parentModel := entities.ModelConfig{Name: "parent-model", Provider: parentProvider.Name, ModelName: "gpt-parent"}
	subModel := entities.ModelConfig{
		Name: "sub-model", Provider: subProvider.Name, ModelName: "gpt-sub",
		InputCostPerMillionTokens: 1, OutputCostPerMillionTokens: 4,
	}

	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{
				"assistant": {Name: "assistant", Description: "top-level agent", Model: "parent-model"},
				"helper":    {Name: "helper", Description: "a subagent", Model: "sub-model"},
			},
		},
		Providers:    []entities.ProviderConfig{parentProvider, subProvider},
		Models:       []entities.ModelConfig{parentModel, subModel},
		DefaultModel: parentModel.Name,
		Repository:   repo,
		Tools:        ToolsConfig{WorkingRoot: t.TempDir()},
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	result, err := svc.RunText(ctx, "chat-1", "please dispatch the helper")
	require.NoError(t, err)

	assert.NotContains(t, result.Usage, "parent-model", "the parent's own zero-usage reply must not fabricate a nonzero entry")
	require.Contains(t, result.Usage, "sub-model", "the subagent dispatch's usage must be recorded under its own model")
	assert.Equal(t, int64(30), result.Usage["sub-model"].InputTokens)
	assert.Equal(t, int64(20), result.Usage["sub-model"].OutputTokens)
	// 30 tokens @ $1/M + 20 tokens @ $4/M = 0.00003 + 0.00008 = 0.00011
	assert.InDelta(t, 0.00011, result.Usage["sub-model"].CostUSD, 1e-9)
}
