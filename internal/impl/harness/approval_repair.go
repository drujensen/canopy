package harness

import (
	"slices"

	"github.com/microsoft/agent-framework-go/message"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// RemoveOrphanedApprovalRequests works around a confirmed agent-framework-go
// bug: agent/harness/toolapproval's own internal auto-approval loop
// (toolapproval.go's run(), Step 3 — re-invokes the wrapped agent chain
// once per newly-issued tool call while a standing "always approve" rule
// keeps auto-approving, without ever surfacing a request to the caller)
// makes one complete, independent invoke() round trip per internal
// iteration — each with its own HistoryProvider.Invoking (reload) and
// Invoked (persist) call, not one combined round trip for the whole outer
// turn. Confirmed empirically (see this package's approval_repair_test.go
// and domain/services' own TestAgentService_ChainedAutoApprove_*
// end-to-end coverage): with a standing rule already installed, the
// *first* auto-approved call in a turn persists a correctly matched
// ToolApprovalRequestContent + ToolApprovalResponseContent pair
// (RequestID-linked) — but every auto-approved call *after* that first one
// in the same turn persists only the request; its matching response,
// though the tool call demonstrably did execute (a real HTTP round trip to
// the provider happens either way, and the model's very next reply depends
// on the tool's real result), is never added to the persisted history at
// all.
//
// Left unrepaired, that dangling, unanswered request makes toolautocall's
// own request/response reconciliation
// (agent-framework-go's extractAndRemoveToolApprovalRequestsAndResponses)
// fail loudly with "ToolApprovalRequestContent found with
// ToolCall.CallID(s) '...' that have no matching ToolApprovalResponseContent"
// the *next* time anything re-validates the full message history against
// it — and because each of toolapproval's internal iterations reloads that
// same history fresh (via HistoryProvider.Invoking) before making its own
// next() call, a long enough chain of auto-approved calls can trip this
// failure *inside a single still-running outer turn*, before
// domain/services.AgentService.RunMessages/RunMessagesStream ever gets
// control back — a real user reproduction confirmed this: the failing
// call's ID never appeared in the persisted chat file at all, proving the
// whole round (including whatever history had already been committed by
// earlier, otherwise-successful iterations of the *same* turn) failed
// before AgentService's own post-turn repair (removeOrphanedApprovalRequests,
// agent_service.go) ever ran. That AgentService-level repair alone is not
// enough — it only ever gets a chance to run *between* turns, never inside
// one.
//
// This is why the fix lives here, in ChatHistoryProvider.Invoked itself,
// not only at the AgentService layer: Invoked is what actually runs on
// *every* individual persist, including the ones nested inside
// toolapproval's internal loop, mid-turn. Repairing right before each
// persist prevents an orphan from ever surviving long enough for a *later*
// internal iteration of the *same* turn to reload it and trip validation.
// AgentService.RunMessages/RunMessagesStream still additionally call the
// same repair before/after the outer turn — that's what self-heals a chat
// already left broken by a pre-fix binary, a case this per-persist repair
// alone can't reach (it only ever sees what a *live* run passes through
// it).
//
// Repairing by *removal* rather than fabricating a matching response is
// the safer choice: the request is stale bookkeeping noise by the time
// it's found (the underlying call already ran to completion, which is how
// its request ever became visible in history in the first place — there
// is no real approval decision left outstanding for a human or a standing
// rule to make), and removing it needs no assumption about what a
// fabricated response's Approved/RejectionReason fields should say.
func RemoveOrphanedApprovalRequests(chat *entities.Chat) bool {
	answered := make(map[string]bool, len(chat.Messages))
	for _, m := range chat.Messages {
		if m == nil {
			continue
		}
		for _, c := range m.Contents {
			if resp, ok := c.(*message.ToolApprovalResponseContent); ok && resp != nil {
				answered[resp.RequestID] = true
			}
		}
	}

	var changed bool
	for i, m := range chat.Messages {
		if m == nil {
			continue
		}
		kept := make([]message.Content, 0, len(m.Contents))
		var removedAny bool
		for _, c := range m.Contents {
			if req, ok := c.(*message.ToolApprovalRequestContent); ok && req != nil {
				if fcc, ok := req.ToolCall.(*message.FunctionCallContent); ok && fcc != nil && !fcc.InformationalOnly && !answered[req.RequestID] {
					removedAny = true
					changed = true
					continue
				}
			}
			kept = append(kept, c)
		}
		if removedAny {
			clone := m.Clone()
			clone.Contents = kept
			chat.Messages[i] = clone
		}
	}
	if changed {
		chat.Messages = slices.DeleteFunc(chat.Messages, func(m *message.Message) bool {
			return m == nil || len(m.Contents) == 0
		})
	}
	return changed
}
