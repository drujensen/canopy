package harness_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/impl/harness"
)

// markerText is a distinctive string near the end of the synthetic long
// conversation fixture built by longConversationFixture — the "task
// continuity" content compaction must never drop, no matter how much older
// filler it excludes.
const markerText = "TASK_CONTINUITY_MARKER_7788"

// fillerTurnText is the distinctive early-conversation content compaction is
// expected to exclude once the conversation is long enough to blow a small
// token budget.
func fillerTurnText(i int) string {
	return fmt.Sprintf("FILLER_TURN_%d %s", i, strings.Repeat("x", 500))
}

// longConversationFixture builds turns alternating user/assistant messages,
// each padded with enough filler text that a small compaction token budget
// is blown well before the end of the conversation, followed by one final
// user turn carrying markerText — the message a real caller would need to
// survive compaction to keep working on the task at hand.
func longConversationFixture(turns int) []*message.Message {
	msgs := make([]*message.Message, 0, turns*2+1)
	for i := 0; i < turns; i++ {
		user := message.NewText(fillerTurnText(i))
		user.Role = message.RoleUser
		assistant := message.NewText(fmt.Sprintf("ack turn %d", i))
		assistant.Role = message.RoleAssistant
		msgs = append(msgs, user, assistant)
	}
	marker := message.NewText(markerText)
	marker.Role = message.RoleUser
	msgs = append(msgs, marker)
	return msgs
}

func containsText(msgs []*message.Message, text string) bool {
	for _, m := range msgs {
		if strings.Contains(m.String(), text) {
			return true
		}
	}
	return false
}

// TestNewCompactionProvider_ReducesLongConversation is the regression test
// Requirements FR10/Design §3.5 ask for: a long synthetic conversation, run
// through the exact agent.ContextProvider harness.Build wires into every
// chat-bound agent, must come out smaller (compaction actually reduced what
// would be sent to the model) while the most recent content — the thing a
// real caller needs to keep working on the task — survives.
func TestNewCompactionProvider_ReducesLongConversation(t *testing.T) {
	// A small context window (2000 tokens, so the 0.75 budget in
	// compaction.go triggers at 1500 tokens) forces compaction well before
	// the fixture's ~40 turns of ~500-byte filler each (rough estimate,
	// nil TokenCounter: byteCount/4) would otherwise fit.
	provider := harness.NewCompactionProvider(2000)
	fixture := longConversationFixture(40)

	session := &agent.Session{}
	got, options, err := provider.Invoking(context.Background(), agent.InvokingContext{
		Messages: fixture,
		Options:  []agent.Option{agent.WithSession(session)},
	})
	require.NoError(t, err)
	assert.NotNil(t, options)

	// Compaction actually reduced what gets sent.
	assert.Less(t, len(got), len(fixture), "compaction should have excluded some of the 40-turn fixture")

	// Task continuity: the marker message near the end must survive.
	assert.True(t, containsText(got, markerText), "the most recent message must survive compaction")

	// Older filler must actually have been excluded, not just reordered.
	assert.False(t, containsText(got, fillerTurnText(0)), "the oldest filler turn should have been excluded by compaction")

	// The included set must respect the configured floor — the preserved
	// tail is never compacted away entirely.
	assert.GreaterOrEqual(t, len(got), 1)
}

// TestNewCompactionProvider_ShortConversationUntouched asserts compaction is
// a no-op (not just "not obviously destructive") when the conversation
// doesn't come close to the token budget — a regression guard against a
// trigger that's too aggressive and compacts conversations that don't need
// it.
func TestNewCompactionProvider_ShortConversationUntouched(t *testing.T) {
	provider := harness.NewCompactionProvider(0) // 0 => defaultContextWindowTokens (128K), a huge budget
	fixture := longConversationFixture(3)

	session := &agent.Session{}
	got, _, err := provider.Invoking(context.Background(), agent.InvokingContext{
		Messages: fixture,
		Options:  []agent.Option{agent.WithSession(session)},
	})
	require.NoError(t, err)
	assert.Len(t, got, len(fixture), "a short conversation well under budget should not be compacted at all")
}
