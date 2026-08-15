// Package tui is Canopy's Bubble Tea frontend (Design §5, Requirements
// FR8/FR11/FR12): an agent picker backed by agentsource-loaded definitions
// that hands off into a chat view driving turns through
// domain/services.AgentService's streaming entry points
// (RunTextStream/RunMessagesStream), rendering incremental response
// content, tool-approval prompts, a live todo panel, and a mode
// indicator/switcher.
package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/agent/harness/todo"
	"github.com/microsoft/agent-framework-go/message"

	"github.com/drujensen/canopy/internal/domain/services"
)

// streamChunkMsg carries one incremental *agent.ResponseUpdate from a turn's
// stream, forwarded to Bubble Tea's event loop as it arrives.
type streamChunkMsg struct {
	update *agent.ResponseUpdate
}

// streamDoneMsg reports a turn's stream was fully drained and its result
// (the same *services.RunResult shape RunMessages returns synchronously —
// see AgentService.RunMessagesStream's doc comment) finalized, including
// session persistence.
type streamDoneMsg struct {
	result *services.RunResult
}

// streamErrMsg reports a turn's stream (or its finalize call) failed.
type streamErrMsg struct {
	err error
}

// modeChangedMsg reports a user-initiated mode switch (AgentService.SetMode)
// completed.
type modeChangedMsg struct {
	mode string
}

// chatStartedMsg reports the outcome of picking an agent in the picker
// screen: either a newly started chat (id, agent name, and its initial
// Todos/Mode snapshot) or an error, which the top-level Model surfaces
// instead of transitioning to the chat screen.
type chatStartedMsg struct {
	chatID    string
	agentName string
	todos     []todo.Item
	mode      string
	err       error
}

// startTurn runs one streaming turn against svc for chatID with msgs on a
// background goroutine, forwarding every event onto a fresh channel: zero or
// more streamChunkMsg values (one per *agent.ResponseUpdate), followed by
// exactly one terminal streamDoneMsg or streamErrMsg. It returns both that
// channel (so the caller can keep re-arming waitForStreamEvent after each
// non-terminal event — see chatModel.handleStreamMsg) and the tea.Cmd that
// reads the first event off it.
//
// This is the TUI's only caller of AgentService.RunMessagesStream — the
// streaming entry point Phase 6 added to AgentService specifically so a
// caller like this one can render *agent.ResponseUpdate content as it
// arrives instead of waiting for a full turn to collect (see
// AgentService.RunMessagesStream's doc comment for why that method returns
// a (stream, finalize) pair rather than a single blocking call).
func startTurn(ctx context.Context, svc *services.AgentService, chatID string, msgs []*message.Message) (chan tea.Msg, tea.Cmd) {
	ch := make(chan tea.Msg)
	go func() {
		defer close(ch)
		stream, finalize, err := svc.RunMessagesStream(ctx, chatID, msgs)
		if err != nil {
			ch <- streamErrMsg{err: err}
			return
		}
		for update, err := range stream {
			if err != nil {
				ch <- streamErrMsg{err: err}
				return
			}
			ch <- streamChunkMsg{update: update}
		}
		result, err := finalize()
		if err != nil {
			ch <- streamErrMsg{err: err}
			return
		}
		ch <- streamDoneMsg{result: result}
	}()
	return ch, waitForStreamEvent(ch)
}

// waitForStreamEvent returns a tea.Cmd that reads the next event off ch. A
// closed channel yields no tea.Msg (Bubble Tea treats a nil Msg as a no-op),
// which only happens after startTurn's goroutine has already sent a
// terminal streamDoneMsg/streamErrMsg, so callers don't need to special-case
// it.
func waitForStreamEvent(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}
