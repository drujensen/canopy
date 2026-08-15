package harness_test

import (
	"context"
	"iter"
	"testing"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/agent/harness/toolautocall"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/impl/harness"
)

// TestWireLoopQuality_ToolApprovalInterceptsBeforeAutocallExecutes is the
// empirical proof Design §3.6/PLAN.md Phase 5 asks for: agent/harness/
// toolapproval's middleware must land in agent.Config.Middlewares (which
// wraps an agent's entire invoke lifecycle) rather than
// agent.ProviderConfig.Middlewares (which wraps only the provider Run call,
// where every per-provider constructor already places
// agent/harness/toolautocall — see WireLoopQuality's doc comment for the
// full reasoning traced from framework source). Get that ordering backwards
// and approval-gating silently does nothing: toolautocall would auto-execute
// an ApprovalRequiredTool the instant the model requests it.
//
// This test builds the exact shape harness.Build produces — WireLoopQuality
// composing Config.Middlewares, wrapping a ProviderConfig whose own
// Middlewares carry toolautocall.New(...), exactly like every real
// per-provider constructor in impl/providers does — against a fake
// agent.ProviderConfig.Run so no real provider client is needed. It proves
// the ordering by observing the *tool's own side effect* (a counter),
// not just the shape of the returned messages:
//
//  1. A turn that would otherwise call an ApprovalRequiredTool leaves the
//     tool's underlying action un-run (counter still 0) immediately after
//     the run — the call must have been converted into an approval request
//     instead of executed.
//  2. Simulating "always approve" for that tool (feeding back the
//     AlwaysApproveToolApprovalResponseContent a real caller would send)
//     lets the same call actually execute (counter becomes 1).
func TestWireLoopQuality_ToolApprovalInterceptsBeforeAutocallExecutes(t *testing.T) {
	var calls int
	riskyTool := tool.ApprovalRequiredFunc(functool.MustNew(functool.Config{
		Name:        "risky",
		Description: "a tool whose execution must never happen without approval",
	}, func(context.Context, struct{}) (string, error) {
		calls++
		return "done", nil
	}))
	require.True(t, riskyTool.(tool.ApprovalRequiredTool).ApprovalRequired())

	var providerCalls int
	fakeRun := func(_ context.Context, _ []*message.Message, _ ...agent.Option) iter.Seq2[*agent.ResponseUpdate, error] {
		providerCalls++
		return func(yield func(*agent.ResponseUpdate, error) bool) {
			if providerCalls == 1 {
				// First turn: the model "decides" to call the risky tool.
				yield(&agent.ResponseUpdate{
					Role: message.RoleAssistant,
					Contents: []message.Content{
						&message.FunctionCallContent{CallID: "call-1", Name: "risky", Arguments: "{}"},
					},
				}, nil)
				return
			}
			// Any follow-up call (after the tool actually ran) just answers.
			yield(&agent.ResponseUpdate{
				Role:     message.RoleAssistant,
				Contents: []message.Content{&message.TextContent{Text: "acknowledged"}},
			}, nil)
		}
	}

	// This is exactly what harness.Build does: WireLoopQuality composes
	// Config.Middlewares (here, just toolapproval — no chat/repository
	// needed since we're constructing the agent directly rather than via
	// Build), then a real per-provider constructor would wrap Run with
	// toolautocall in ProviderConfig.Middlewares. We do the latter part by
	// hand since there's no seam to fake the provider client itself.
	cfg := harness.WireLoopQuality(harness.BuildParams{
		Config: agent.Config{
			Name:  "test-agent",
			Tools: []tool.Tool{riskyTool},
		},
	})

	a := agent.New(agent.ProviderConfig{
		ProviderName: "stub",
		Run:          fakeRun,
		Middlewares:  []agent.Middleware{toolautocall.New(toolautocall.Config{})},
	}, cfg)

	session := &agent.Session{}
	resp1, err := a.RunText(context.Background(), "please run the risky tool", agent.WithSession(session)).Collect()
	require.NoError(t, err)

	// #1: the tool's underlying action must NOT have run yet.
	assert.Equal(t, 0, calls, "an ApprovalRequiredTool must not execute before toolapproval intercepts and an approval is granted")

	var approvalReq *message.ToolApprovalRequestContent
	for _, m := range resp1.Messages {
		for _, c := range m.Contents {
			if req, ok := c.(*message.ToolApprovalRequestContent); ok {
				approvalReq = req
			}
		}
	}
	require.NotNil(t, approvalReq, "toolautocall should have surfaced a ToolApprovalRequestContent instead of executing the tool")

	// Simulate the caller (Plan Phase 6's TUI) choosing "always allow this
	// tool" in response to the surfaced request.
	approvalMsg := &message.Message{
		Role:     message.RoleUser,
		Contents: []message.Content{approvalReq.AlwaysApproveToolResponse()},
	}
	resp2, err := a.Run(context.Background(), []*message.Message{approvalMsg}, agent.WithSession(session)).Collect()
	require.NoError(t, err)

	// #2: now that approval was granted, the tool must actually have run.
	assert.Equal(t, 1, calls, "the tool must execute once approval is granted")
	assert.Contains(t, resp2.String(), "acknowledged")
}
