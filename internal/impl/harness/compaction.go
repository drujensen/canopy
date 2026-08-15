package harness

import (
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/agent/compaction"
)

// defaultContextWindowTokens is used when an entities.ModelConfig doesn't set
// ContextWindowTokens (Design §3.5). 128K tokens is a conservative,
// documented default: it matches or undershoots the context window of every
// mainstream model family Canopy targets (GPT-4o/4.1, Claude 3.5/3.7/4,
// Gemini 1.5/2.0's smaller tier), so compaction engages at least as
// eagerly as it should rather than too late for a smaller-context model.
// Follow-up: seed ContextWindowTokens per known model instead of relying on
// this single fallback for everything.
const defaultContextWindowTokens = 128_000

// compactionTokenBudgetFraction is the fraction of the model's context
// window compaction targets staying under. 0.75 leaves headroom for the
// model's own response plus provider-side overhead (system prompt,
// tool schemas, etc.) that isn't part of the tracked message list.
const compactionTokenBudgetFraction = 0.75

// minimumPreservedTurns/minimumPreservedGroups are the floors compaction
// will not compact below, so recent context — in particular a
// task-continuity marker near the end of a long conversation — always
// survives. See compaction_test.go's regression test for what this
// guarantees concretely.
const (
	minimumPreservedTurns  = 2
	minimumPreservedGroups = 4
)

// NewCompactionProvider builds the compaction agent.ContextProvider Design
// §3.5/Requirements FR10 asks every chat-bound agent impl/harness.Build
// constructs to carry: a TokensExceed(N) trigger — N derived from
// contextWindowTokens, or defaultContextWindowTokens when
// contextWindowTokens <= 0 — driving a two-stage PipelineStrategy:
// SlidingWindowStrategy (excludes the oldest turns first, respecting turn
// boundaries) followed by TruncationStrategy (a group-level safety net for
// content that doesn't fall on clean turn boundaries, e.g. tool-call
// groups). Both stages share the same trigger, so the second stage is a
// no-op once the first has already brought the index under budget.
//
// # Deviation from Design §3.5's "sliding-window-then-summarization" pipeline
//
// Design §3.5 suggests summarization as the second stage. This package uses
// TruncationStrategy instead of compaction.SummarizationStrategy: a
// SummarizationStrategy needs a compaction.Summarizer, which makes its own
// LLM call to produce the summary text. impl/harness.Build assembles a
// ContextProvider *before* impl/providers.New constructs the agent's actual
// provider client (the ContextProvider goes into agent.Config, which is an
// input to providers.New, not an output of it) — there is no provider
// client available yet at the point this function needs to return a
// Strategy, and threading one through would mean either constructing a
// second, throwaway client per agent just for summarization or restructuring
// Build/providers.New's call order. Sliding-window+truncation is
// deterministic, adds no extra LLM round-trip or cost, and already satisfies
// FR10's actual bar (reduce what's sent while preserving enough for task
// continuity — see the regression test). LLM-backed summarization as a
// third pipeline stage is a real, flagged follow-up, not a silently dropped
// requirement.
func NewCompactionProvider(contextWindowTokens int) agent.ContextProvider {
	if contextWindowTokens <= 0 {
		contextWindowTokens = defaultContextWindowTokens
	}
	maxTokens := int(float64(contextWindowTokens) * compactionTokenBudgetFraction)
	trigger := compaction.TokensExceed(maxTokens)

	return compaction.NewContextProvider(compaction.ContextProviderConfig{
		Strategy: &compaction.PipelineStrategy{
			Strategies: []compaction.Strategy{
				&compaction.SlidingWindowStrategy{
					Trigger:               trigger,
					MinimumPreservedTurns: minimumPreservedTurns,
				},
				&compaction.TruncationStrategy{
					Trigger:                trigger,
					MinimumPreservedGroups: minimumPreservedGroups,
				},
			},
		},
	})
}
