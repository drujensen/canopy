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
func mergeUsage(chat *entities.Chat, entries []usageEntry, models map[string]entities.ModelConfig) {
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
		if model, ok := models[e.modelName]; ok {
			mu.CostUSD += float64(e.usage.InputTokenCount) / 1_000_000 * model.InputCostPerMillionTokens
			mu.CostUSD += float64(e.usage.OutputTokenCount) / 1_000_000 * model.OutputCostPerMillionTokens
		}
		chat.Usage[e.modelName] = mu
	}
}
