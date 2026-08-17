package harness

import (
	"testing"

	"github.com/microsoft/agent-framework-go/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// TestRemoveOrphanedApprovalRequests_DropsUnansweredRequest is a direct unit
// test for the confirmed agent-framework-go bug RemoveOrphanedApprovalRequests
// works around (see its own doc comment for the full empirical finding):
// under a standing "always approve" rule, the *first* auto-approved tool
// call in a turn persists a correctly matched ToolApprovalRequestContent +
// ToolApprovalResponseContent pair, but every auto-approved call *after*
// that first one in the same turn persists only the request — its response
// is silently dropped by the framework's own history bookkeeping, even
// though the tool call itself did execute. Constructs exactly that broken
// shape directly (not through a live provider round-trip) and asserts the
// orphaned request is removed while an answered one is left untouched.
func TestRemoveOrphanedApprovalRequests_DropsUnansweredRequest(t *testing.T) {
	answeredReq := &message.ToolApprovalRequestContent{
		RequestID: "req-answered",
		ToolCall:  &message.FunctionCallContent{CallID: "call-1", Name: "run_shell", Arguments: `{}`},
	}
	orphanedReq := &message.ToolApprovalRequestContent{
		RequestID: "req-orphaned",
		ToolCall:  &message.FunctionCallContent{CallID: "call-2", Name: "run_shell", Arguments: `{}`},
	}
	chat := &entities.Chat{
		Messages: []*message.Message{
			{Role: message.RoleAssistant, Contents: []message.Content{answeredReq}},
			{Role: message.RoleUser, Contents: []message.Content{answeredReq.CreateResponse(true, "")}},
			// req-orphaned's matching response is deliberately absent —
			// this is the exact bug's persisted shape.
			{Role: message.RoleAssistant, Contents: []message.Content{orphanedReq}},
			{Role: message.RoleAssistant, Contents: []message.Content{&message.TextContent{Text: "unrelated, must survive untouched"}}},
		},
	}

	changed := RemoveOrphanedApprovalRequests(chat)

	require.True(t, changed, "an orphaned request must be reported as a real repair")
	require.Len(t, chat.Messages, 3, "the now-empty message that only held the orphaned request must be dropped entirely, not left behind empty")
	for _, m := range chat.Messages {
		for _, c := range m.Contents {
			if req, ok := c.(*message.ToolApprovalRequestContent); ok {
				assert.NotEqual(t, "req-orphaned", req.RequestID, "the orphaned request must be gone")
			}
		}
	}
	// The answered request and the unrelated message must both survive.
	var sawAnswered, sawUnrelated bool
	for _, m := range chat.Messages {
		for _, c := range m.Contents {
			switch v := c.(type) {
			case *message.ToolApprovalRequestContent:
				if v.RequestID == "req-answered" {
					sawAnswered = true
				}
			case *message.TextContent:
				if v.Text == "unrelated, must survive untouched" {
					sawUnrelated = true
				}
			}
		}
	}
	assert.True(t, sawAnswered, "an already-answered request must never be removed")
	assert.True(t, sawUnrelated, "unrelated content must never be touched")
}

// TestRemoveOrphanedApprovalRequests_NothingToRepairIsANoOp asserts the
// common case (every request has a matching response, or there are no
// approval requests in history at all) reports changed=false and leaves
// chat.Messages completely untouched — this runs on *every single persist*
// (ChatHistoryProvider.Invoked), so the common case must stay cheap and
// inert when there's nothing wrong.
func TestRemoveOrphanedApprovalRequests_NothingToRepairIsANoOp(t *testing.T) {
	req := &message.ToolApprovalRequestContent{
		RequestID: "req-1",
		ToolCall:  &message.FunctionCallContent{CallID: "call-1", Name: "run_shell", Arguments: `{}`},
	}
	original := []*message.Message{
		{Role: message.RoleAssistant, Contents: []message.Content{req}},
		{Role: message.RoleUser, Contents: []message.Content{req.CreateResponse(true, "")}},
		{Role: message.RoleAssistant, Contents: []message.Content{&message.TextContent{Text: "hi"}}},
	}
	chat := &entities.Chat{Messages: append([]*message.Message(nil), original...)}

	changed := RemoveOrphanedApprovalRequests(chat)

	assert.False(t, changed)
	assert.Equal(t, original, chat.Messages)
}

// TestRemoveOrphanedApprovalRequests_InformationalOnlyNeverRemoved asserts
// an InformationalOnly ToolApprovalRequestContent (the reconstructed replay
// content domain/services' withReconstructedApprovalFunctionCalls
// synthesizes, or a framework-internal marker) is never mistaken for a
// genuinely orphaned, still-pending request — only a real,
// non-InformationalOnly request with no matching response is a repair
// target.
func TestRemoveOrphanedApprovalRequests_InformationalOnlyNeverRemoved(t *testing.T) {
	req := &message.ToolApprovalRequestContent{
		RequestID: "req-1",
		ToolCall: &message.FunctionCallContent{
			CallID:            "call-1",
			Name:              "run_shell",
			Arguments:         `{}`,
			InformationalOnly: true,
		},
	}
	chat := &entities.Chat{
		Messages: []*message.Message{
			{Role: message.RoleAssistant, Contents: []message.Content{req}},
		},
	}

	changed := RemoveOrphanedApprovalRequests(chat)

	assert.False(t, changed, "an InformationalOnly request is not a genuine pending approval and must never be removed")
	require.Len(t, chat.Messages, 1)
}
