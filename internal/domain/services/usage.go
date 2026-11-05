package services

import (
	"context"
	"sync"

	"github.com/microsoft/agent-framework-go/message"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// usageEntry is one model call's token usage recorded during a single
// outer turn — the top-level call RunMessages/RunMessagesStream itself
// made, or one subagent dispatch (usageTrackingSubagentTool) — tagged with
// which entities.ModelConfig.Name produced it. A subagent can resolve to a
// different model than its parent (buildSubagentAgent/
// resolveProviderModel), and the parent's own model can differ turn to
// turn after a ctrl+o switch, so every entry carries its own model name
// rather than assuming one model per turn.
type usageEntry struct {
	modelName string
	usage     message.UsageDetails
}

// usageCollector accumulates every usageEntry recorded during one outer
// RunMessages/RunMessagesStream call. One instance is created fresh per
// call (both entry points) and attached to that call's ctx via
// withUsageCollector, so a subagent's wrapped tool — invoked from deep
// inside toolautocall's internal tool-call loop, with no return-value path
// back to AgentService — can report its own usage without every
// intermediate layer (toolautocall, the provider client, agent-framework-go's
// own invoke()) needing to know usage tracking exists. Safe for concurrent
// use: toolautocall can run parallel tool calls within one turn, including
// more than one subagent dispatch.
type usageCollector struct {
	mu      sync.Mutex
	entries []usageEntry
}

// record appends one usageEntry. Safe to call with a nil receiver (a
// defensive no-op) so a caller that forgot to attach a collector to ctx —
// or a code path exercised directly in a unit test with no collector at
// all — doesn't panic.
func (c *usageCollector) record(modelName string, usage message.UsageDetails) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, usageEntry{modelName: modelName, usage: usage})
}

// snapshot returns a copy of every entry recorded so far, safe to read
// without racing a concurrent record call still in flight (toolautocall's
// parallel tool calls). Safe to call with a nil receiver, returning nil.
func (c *usageCollector) snapshot() []usageEntry {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]usageEntry(nil), c.entries...)
}

type usageCollectorKey struct{}

// withUsageCollector attaches c to ctx so usageTrackingSubagentTool.Call —
// reached only through a.Run(ctx, ...)'s own call chain, never a path
// AgentService constructs or holds a reference to directly — can find it
// via usageCollectorFromContext and report into it.
func withUsageCollector(ctx context.Context, c *usageCollector) context.Context {
	return context.WithValue(ctx, usageCollectorKey{}, c)
}

func usageCollectorFromContext(ctx context.Context) *usageCollector {
	c, _ := ctx.Value(usageCollectorKey{}).(*usageCollector)
	return c
}

// mergeUsage folds entries into chat.Usage in place, accumulating token
// counts and USD cost per model (post-v0.1.0 addendum: persisted so a
// chat's running cost survives a restart/--continue, and can later be
// summed across every chat in a project for an aggregate view). Cost is
// computed from models[entry.modelName]'s per-million-token rates; a model
// with no published rate (ModelConfig's zero value, same "unknown means
// free/unpublished" convention ModelSummary already uses) contributes zero
// cost but still accumulates its real token counts. An entry naming a
// model no longer present in models — e.g. removed from providers.json
// since this chat last ran — still accumulates its tokens at zero
// additional cost rather than being dropped, so a historical total never
// silently loses tokens over a config change.
//
// Cache-aware cost (post-v0.1.0 addendum): a provider that serves part of
// the prompt from cache (OpenCode/OpenAI/Gemini via the "x-opencode-session"
// header or equivalent) bills that part at CacheReadCostPerMillionTokens
// instead of the full input rate — e.g. OpenCode's qwen3.8-flash charges
// $0.016/M for a cache hit vs $0.15/M fresh, a ~9x difference. Billing every
// reported input token at the full rate, as this function used to do,
// overcounts cost dramatically on a long chat whose growing history is
// mostly cache hits turn over turn. providers resolves each entry's model
// to its ProviderConfig so the right convention applies: OpenAI/OpenCode/
// Gemini report InputTokenCount *inclusive* of the cached portion (so the
// fresh share is InputTokenCount-CachedInputTokenCount), but Anthropic
// reports them as disjoint counters (InputTokenCount is already fresh-only,
// cached is added on top) — see isCacheInclusive's doc comment for the
// source citations. A model with InputCostPerMillionTokens published but no
// CacheReadCostPerMillionTokens (the catalog hasn't published a cache rate
// for it) bills its cached tokens at the full input rate rather than at a
// misleading $0 — same "don't invent a discount that isn't confirmed"
// reasoning as the overall "unknown means free" convention would otherwise
// invert.
func mergeUsage(chat *entities.Chat, entries []usageEntry, models map[string]entities.ModelConfig, providers map[string]entities.ProviderConfig) {
	if len(entries) == 0 {
		return
	}
	if chat.Usage == nil {
		chat.Usage = make(map[string]entities.ModelUsage)
	}
	for _, e := range entries {
		if e.usage.InputTokenCount == 0 && e.usage.OutputTokenCount == 0 {
			// A provider that genuinely reports no usage metadata at all
			// (some self-hosted/free backends never send a "usage" object)
			// must not fabricate a visible zero-cost line for its model —
			// only a model that has actually reported real tokens at least
			// once ever appears in chat.Usage.
			continue
		}
		mu := chat.Usage[e.modelName]
		mu.InputTokens += e.usage.InputTokenCount
		mu.OutputTokens += e.usage.OutputTokenCount
		mu.CachedInputTokens += e.usage.CachedInputTokenCount
		if model, ok := models[e.modelName]; ok {
			cached := e.usage.CachedInputTokenCount
			fresh := e.usage.InputTokenCount
			if isCacheInclusive(providers, model) {
				fresh -= cached
				if fresh < 0 {
					fresh = 0
				}
			}
			cacheRate := model.CacheReadCostPerMillionTokens
			if cacheRate == 0 {
				cacheRate = model.InputCostPerMillionTokens
			}
			mu.CostUSD += float64(fresh) / 1_000_000 * model.InputCostPerMillionTokens
			mu.CostUSD += float64(cached) / 1_000_000 * cacheRate
			mu.CostUSD += float64(e.usage.OutputTokenCount) / 1_000_000 * model.OutputCostPerMillionTokens
		}
		chat.Usage[e.modelName] = mu
	}
}

// isCacheInclusive reports whether model's provider reports
// UsageDetails.InputTokenCount *inclusive* of CachedInputTokenCount (true —
// the cached share must be subtracted back out to get the fresh share) as
// opposed to the two counters being disjoint (false — InputTokenCount is
// already fresh-only, and cached cost is purely additive on top).
//
// Confirmed directly against agent-framework-go's per-provider usage
// mapping: OpenAI (chat.go/responses.go: InputTokenCount=usage.PromptTokens/
// usage.InputTokens, the API's own prompt-token *total*, which OpenAI's docs
// define as inclusive of any cached portion) and Gemini (agent.go:
// InputTokenCount=PromptTokenCount, likewise documented as the full
// effective prompt size including cached content) are inclusive — and so is
// every OpenAI-Chat-Completions-compatible provider Canopy talks to through
// the same openaiprovider code path (DeepSeek, Groq, Mistral, Together, xAI,
// Ollama, OpenCode, and the generic auto-detected fallback). Anthropic
// (anthropicprovider/agent.go: TotalTokenCount computed in-library as plain
// InputTokens+OutputTokens, with CachedInputTokenCount/
// CacheCreationInputTokens tracked as separate additions never folded into
// InputTokens) is the one exclusive case — confirmed against Anthropic's own
// API docs, where input_tokens never includes cache_read_input_tokens.
// providers[model.Provider] not resolving (e.g. a model whose provider was
// since removed from providers.json) defaults to inclusive, matching every
// real provider Canopy supports except the one named exception.
func isCacheInclusive(providers map[string]entities.ProviderConfig, model entities.ModelConfig) bool {
	provider, ok := providers[model.Provider]
	if !ok {
		return true
	}
	return provider.Type != entities.ProviderTypeAnthropic
}
