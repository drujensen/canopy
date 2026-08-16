package services

import (
	"testing"

	"github.com/microsoft/agent-framework-go/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// TestWithReconstructedApprovalFunctionCalls_EmptyCallID_PreFixPersistedChat
// exercises withReconstructedApprovalFunctionCalls' fcc.CallID == "" branch
// directly, independent of any live/fake HTTP transport, to answer this
// task's investigation question: is that branch still reachable now that
// internal/impl/providers/gemini_transport.go patches missing Gemini
// functionCall.id fields before genai's decoder (and therefore every
// downstream *message.FunctionCallContent Canopy code sees) ever sees them?
//
// Answer: yes, for exactly one real scenario — a chat persisted to disk
// *before* the transport fix existed (or before it was ever deployed for a
// given install) can already contain a *message.ToolApprovalRequestContent
// whose snapshotted ToolCall has CallID == "" (chat storage round-trips
// through JSON, so there is no way to retroactively patch already-persisted
// history just by shipping a new binary). If a user resumes that chat and
// answers the pending approval today, ToolApprovalRequestContent.CreateResponse
// clones that same empty CallID onto the new turn's ToolApprovalResponseContent,
// and this branch is what repairs it — for both this turn's response *and*
// the matching historical request sitting in chat.Messages, exactly as the
// function's own doc comment describes. For any *newly* received Gemini
// function call (post-fix), the transport has already patched a non-empty
// CallID before this code ever runs, so the branch is not exercised on that
// path — but it is not dead: it is the intended patch path for pre-fix
// persisted state ("defense in depth" against exactly the migration case a
// binary upgrade produces, not a hypothetical).
func TestWithReconstructedApprovalFunctionCalls_EmptyCallID_PreFixPersistedChat(t *testing.T) {
	// Simulates a *message.ToolApprovalRequestContent already sitting in
	// chat.Messages, persisted by a pre-fix binary against real (empty-id)
	// Gemini traffic — exactly what a JSON round-trip through
	// jsonrepo.ChatRepository would have produced before gemini_transport.go
	// existed.
	preFixRequest := &message.ToolApprovalRequestContent{
		RequestID: "req-1",
		ToolCall: &message.FunctionCallContent{
			CallID:    "", // never patched: pre-dates the transport fix
			Name:      "echo",
			Arguments: `{"message":"hi"}`,
		},
	}
	chat := &entities.Chat{
		ID: "chat-1",
		Messages: []*message.Message{
			{
				Role:     message.RoleAssistant,
				Contents: []message.Content{preFixRequest},
			},
		},
	}

	// The user answers the pending approval today (post-fix binary).
	// CreateResponse clones the request's ToolCall verbatim, so the
	// response's own snapshot also has CallID == "" — this is exactly what
	// a real resumed pre-fix chat would produce, not a hand-crafted
	// shortcut.
	approvalResponse := preFixRequest.CreateResponse(true, "")
	fcc, ok := approvalResponse.ToolCall.(*message.FunctionCallContent)
	require.True(t, ok)
	require.Empty(t, fcc.CallID, "sanity: the response snapshot must start out empty, mirroring CreateResponse's real clone-carries-empty-CallID behavior")

	msgs := []*message.Message{
		{Role: message.RoleUser, Contents: []message.Content{approvalResponse}},
	}

	out, chatPatched := withReconstructedApprovalFunctionCalls(chat, msgs)

	assert.True(t, chatPatched, "the historical request's empty CallID must have been repaired in place")

	// The historical request sitting in chat.Messages must now carry the
	// same non-empty, synthetic CallID as the reconstructed call below —
	// this is the "repaired on both sides, in lockstep" guarantee the
	// function's doc comment describes.
	patchedHistCallID := preFixRequest.ToolCall.(*message.FunctionCallContent).CallID
	assert.NotEmpty(t, patchedHistCallID)

	// out must be msgs prefixed with one new assistant message carrying the
	// reconstructed FunctionCallContent.
	require.Len(t, out, len(msgs)+1)
	synthetic := out[0]
	assert.Equal(t, message.RoleAssistant, synthetic.Role)
	require.Len(t, synthetic.Contents, 1)
	reconstructed, ok := synthetic.Contents[0].(*message.FunctionCallContent)
	require.True(t, ok)
	assert.Equal(t, patchedHistCallID, reconstructed.CallID, "the reconstructed call and the patched history entry must agree on the synthetic id")
	assert.Equal(t, "echo", reconstructed.Name)
	assert.True(t, reconstructed.InformationalOnly)

	// The rest of msgs is passed through unchanged, appended after the
	// synthetic message.
	assert.Same(t, msgs[0], out[1])
}

// TestWithReconstructedApprovalFunctionCalls_NonEmptyCallID_BranchNotTriggered
// is the mirror case: a *message.ToolApprovalRequestContent whose ToolCall
// already carries a non-empty CallID — exactly what every Gemini response
// looks like today, post-transport-fix, and what every OpenAI/Anthropic
// response has always looked like — must not trigger the empty-CallID
// repair branch at all: chatPatched stays false, and the id used in the
// reconstructed call is the original one, verbatim, not a freshly-minted
// synthetic one.
func TestWithReconstructedApprovalFunctionCalls_NonEmptyCallID_BranchNotTriggered(t *testing.T) {
	req := &message.ToolApprovalRequestContent{
		RequestID: "req-1",
		ToolCall: &message.FunctionCallContent{
			CallID:    "canopy-transport-fc-1", // as gemini_transport.go would have set it
			Name:      "echo",
			Arguments: `{"message":"hi"}`,
		},
	}
	chat := &entities.Chat{
		Messages: []*message.Message{
			{Role: message.RoleAssistant, Contents: []message.Content{req}},
		},
	}
	approvalResponse := req.CreateResponse(true, "")
	msgs := []*message.Message{
		{Role: message.RoleUser, Contents: []message.Content{approvalResponse}},
	}

	out, chatPatched := withReconstructedApprovalFunctionCalls(chat, msgs)

	assert.False(t, chatPatched, "an already-non-empty CallID must never be treated as needing a patch")
	require.Len(t, out, len(msgs)+1)
	reconstructed, ok := out[0].Contents[0].(*message.FunctionCallContent)
	require.True(t, ok)
	assert.Equal(t, "canopy-transport-fc-1", reconstructed.CallID, "the original (already-patched-by-transport) id must be preserved verbatim")
}
