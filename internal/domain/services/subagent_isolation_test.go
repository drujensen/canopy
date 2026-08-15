package services_test

import (
	"context"
	"encoding/json"
	"iter"
	"testing"
	"time"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/agenttool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/impl/harness"
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
)

// parentContextMarker is a distinctive string only present in the parent
// chat's stored history. If it turns up anywhere in the messages the child
// agent's Run receives, the child is not actually isolated from the
// parent's conversation.
const parentContextMarker = "SECRET_PARENT_CONTEXT_1701"

// childContextMarker is the mirror-image check: a distinctive string the
// child stamps into its own run/session, which must never appear back in
// the parent's own state after the dispatch returns.
const childContextMarker = "SECRET_CHILD_MARKER_2404"

// TestSubagentDispatch_SessionIsolation is Design §3.4's core claim, made
// concrete: tool/agenttool.New wraps a child *agent.Agent as a tool.FuncTool
// on a parent's tool list, and Config §3.4 asserts that invoking it runs the
// child in a brand-new, isolated Session — the parent's conversation history
// does not leak into the child's context, and nothing the child does to its
// own session leaks back into the parent's.
//
// This is verified empirically, not by reading the framework source: a real
// impl/harness.ChatHistoryProvider is wired onto the parent, backed by a
// real chat pre-seeded with parentContextMarker in its stored history. The
// parent's (stubbed) provider Run function, when invoked, stamps a marker
// into its own *agent.Session and then calls the child tool directly. The
// child's (stubbed) provider Run function captures the exact messages and
// Session it was invoked with. The test then asserts, using those captured
// values:
//
//  1. parentContextMarker (only ever written to the parent's persisted chat
//     history) does not appear in any message the child's Run received.
//  2. The Session object the child's Run received is not the same Session
//     object the parent's Run received (agenttool invokes RunText with no
//     WithSession option, so Agent.prepareRun creates a fresh one).
//  3. A key the parent's Run set on its own session is NOT readable from the
//     child's session (proves state doesn't propagate parent -> child).
//  4. A key the child's Run set on its own session is NOT readable from the
//     parent's session after the dispatch returns (proves state doesn't
//     propagate child -> parent, the "vice versa" half of Design §3.4's
//     claim).
func TestSubagentDispatch_SessionIsolation(t *testing.T) {
	ctx := context.Background()

	// Real ChatRepository + a parent chat pre-seeded with history containing
	// parentContextMarker, so the isolation check has real prior
	// conversation content to leak, not just a hypothetical.
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	parentChat := &entities.Chat{
		ID:        "parent-chat",
		AgentName: "parent",
		Messages: []*message.Message{
			message.NewText(parentContextMarker),
		},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	require.NoError(t, repo.Create(ctx, parentChat))

	// --- child agent: captures what it was actually invoked with ---
	var childReceivedMessages []*message.Message
	var childReceivedSession *agent.Session
	var childSawParentSession bool

	childRun := func(_ context.Context, messages []*message.Message, options ...agent.Option) iter.Seq2[*agent.ResponseUpdate, error] {
		childReceivedMessages = messages
		sess, _ := agent.GetOption(options, agent.WithSession)
		childReceivedSession = sess

		// Check #3: a key the parent stamped onto its own session must not
		// be visible here.
		var v string
		if ok, _ := sess.Get(parentSessionKey, &v); ok {
			childSawParentSession = true
		}
		// Stamp the child's own marker so the parent-side check (#4) below
		// has something concrete to look for.
		sess.Set(childSessionKey, childContextMarker)

		return func(yield func(*agent.ResponseUpdate, error) bool) {
			yield(&agent.ResponseUpdate{Contents: message.Contents{&message.TextContent{Text: "child-response"}}}, nil)
		}
	}
	child := agent.New(agent.ProviderConfig{ProviderName: "stub-child", Run: childRun}, agent.Config{
		Name:        "Child Agent",
		Description: "test subagent",
	})
	childTool := agenttool.New(child, agenttool.Config{})

	// --- parent agent: stamps its own session marker, then calls the child
	// tool directly (bypassing toolautocall's function-call protocol, which
	// is the framework's own already-tested concern — what this test
	// verifies is agenttool's session isolation, not toolautocall's wire
	// format) ---
	var parentSession *agent.Session
	var parentSawChildMarkerAfterDispatch bool

	parentRun := func(ctx context.Context, messages []*message.Message, options ...agent.Option) iter.Seq2[*agent.ResponseUpdate, error] {
		sess, _ := agent.GetOption(options, agent.WithSession)
		parentSession = sess
		sess.Set(parentSessionKey, "leaked-if-child-shares-session")

		var foundChildTool tool.FuncTool
		for tl := range agent.AllOptions(options, agent.WithTool) {
			if ft, ok := tl.(tool.FuncTool); ok && ft.Name() == childTool.Name() {
				foundChildTool = ft
			}
		}
		require.NotNil(t, foundChildTool, "parent must have the child agent wrapped as a tool on its options")

		args, marshalErr := json.Marshal(map[string]string{"query": "delegate to child"})
		require.NoError(t, marshalErr)
		result, callErr := foundChildTool.Call(ctx, string(args))
		require.NoError(t, callErr)

		// Check #4: after the dispatch returns, the parent's own session
		// must not have picked up the child's marker.
		var v string
		if ok, _ := sess.Get(childSessionKey, &v); ok {
			parentSawChildMarkerAfterDispatch = true
		}

		return func(yield func(*agent.ResponseUpdate, error) bool) {
			text := "parent-response: " + result.(string)
			yield(&agent.ResponseUpdate{Contents: message.Contents{&message.TextContent{Text: text}}}, nil)
		}
	}
	parent := agent.New(agent.ProviderConfig{ProviderName: "stub-parent", Run: parentRun}, agent.Config{
		Name:            "parent",
		Tools:           []tool.Tool{childTool},
		HistoryProvider: harness.NewChatHistoryProvider(parentChat.ID, repo),
	})

	resp, err := parent.RunText(ctx, "please delegate").Collect()
	require.NoError(t, err)
	assert.Contains(t, resp.String(), "child-response")

	// #1: the parent's prior history must never have reached the child.
	require.NotEmpty(t, childReceivedMessages)
	for _, m := range childReceivedMessages {
		assert.NotContains(t, m.String(), parentContextMarker,
			"parent's conversation history leaked into the child agent's context")
	}

	// #2: the child ran in a different Session object than the parent's.
	require.NotNil(t, parentSession)
	require.NotNil(t, childReceivedSession)
	assert.NotSame(t, parentSession, childReceivedSession,
		"child subagent dispatch must run in its own fresh Session, not the parent's")

	// #3 and #4, evaluated after the run.
	assert.False(t, childSawParentSession, "child session must not see state the parent's session set")
	assert.False(t, parentSawChildMarkerAfterDispatch, "parent session must not pick up state the child's session set")

	// Belt-and-suspenders: the persisted parent chat's history (what a
	// *second* real turn would load) must reflect only the parent's own
	// turn, not any child-side content leaking into permanent storage.
	reloaded, err := repo.Get(ctx, parentChat.ID)
	require.NoError(t, err)
	found := false
	for _, m := range reloaded.Messages {
		if m.String() == "parent-response: child-response" {
			found = true
		}
	}
	assert.True(t, found, "parent's own response (which legitimately includes the child's tool result as text) should be persisted on the parent chat")
}

const (
	parentSessionKey = "test-parent-marker"
	childSessionKey  = "test-child-marker"
)
