// Package tui is Canopy's Bubble Tea frontend (Design §5, Requirements
// FR8/FR11/FR12): an agent picker backed by agentsource-loaded definitions
// that hands off into a chat view driving turns through
// domain/services.AgentService's streaming entry points
// (RunTextStream/RunMessagesStream), rendering incremental response
// content, tool-approval prompts, and a live todo panel.
package tui

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/agent/harness/todo"
	"github.com/microsoft/agent-framework-go/message"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/domain/services"
	"github.com/drujensen/canopy/internal/impl/providers"
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

// streamRetryMsg reports that a provider request within the current turn
// was throttled (rate-limited, or a transient server error) and is about to
// be retried — see providers.RetryNotice. It's non-terminal, the same way
// streamChunkMsg is: handleStreamMsg re-arms waitForStreamEvent after
// handling it rather than ending the turn, since the retry it describes is
// still in flight on startTurn's goroutine.
type streamRetryMsg struct {
	notice providers.RetryNotice
}

// modelChangedMsg reports a user-initiated model switch (AgentService.
// SetModel, the ctrl+o model-picker overlay — post-v0.1.0 addendum, Design
// §4/FR1) completed.
type modelChangedMsg struct {
	model string
}

// agentChangedMsg reports a user-initiated agent switch (AgentService.
// SetAgent, the ctrl+a in-chat agent-picker overlay — post-v0.1.0 addendum,
// Design §3.4/§5) completed. Unlike chatStartedMsg (the top-level picker
// screen's "start a brand-new chat" path), this never carries a new chatID —
// the chat and its history are unchanged, only which agent definition drives
// subsequent turns.
type agentChangedMsg struct {
	agentName string

	// capabilities is the newly-active agent's services.AgentCapabilities
	// (post-v0.1.0 addendum: sidebar "N tools, M skills" line) — refreshed
	// here because switching agents can change both counts (a different
	// tools: allowlist, or whether Skill is even in it).
	capabilities services.AgentCapabilities
}

// chatStartedMsg reports the outcome of picking an agent in the picker
// screen, or (post-v0.1.0 addendum) resuming a chat via ctrl+h/--continue:
// either a chat ready to display (id, agent name, its current Todos/
// Model snapshot, and — resume only — messages, its full prior history) or
// an error, which the top-level Model surfaces instead of transitioning to
// the chat screen.
type chatStartedMsg struct {
	chatID    string
	agentName string
	todos     []todo.Item
	model     string
	err       error

	// messages is nil for a genuinely new chat (startChatCmd/
	// startNewChatCmd) and populated for a resumed one (resumeChatCmd) —
	// Model.Update's chatStartedMsg case reconstructs it into the new
	// *chatModel's transcript (reconstructTranscript, chat.go) when
	// present, so a resumed chat shows its prior conversation instead of
	// starting blank.
	messages []*message.Message

	// capabilities is agentName's services.AgentCapabilities (post-v0.1.0
	// addendum: sidebar "N tools, M skills" line), resolved by every
	// producer of this message (startChatCmd, startNewChatCmd,
	// resumeChatCmd) since it's needed the moment the chat screen first
	// renders, not just after agentChangedMsg's later ctrl+a refresh.
	capabilities services.AgentCapabilities

	// usage is the chat's persisted entities.Chat.Usage — nil for a
	// genuinely new chat (startChatCmd/startNewChatCmd), populated from
	// GetChat for a resumed one (resumeChatCmd) the same way messages is,
	// so --continue/ctrl+h shows the chat's running cost immediately
	// rather than resetting it to zero until the next turn completes.
	usage map[string]entities.ModelUsage
}

// resumeChatCmd loads chatID's full state (post-v0.1.0 addendum: ctrl+h
// history browser and --continue) and returns the same chatStartedMsg a
// fresh pick from the picker screen already produces, with messages
// populated — see that field's doc comment. Used by three callers: Model's
// own Init (a --continue-resolved chatID), the top-level screenHistory
// screen, and chatModel's in-chat ctrl+h overlay (chat.go) — one function so
// all three resume identically rather than three hand-rolled copies.
//
// Deliberate scope cut: unlike StartChat/SetAgent, resuming a chat does not
// call AgentService.RecordLastAgent — GetChat/GetTodos/GetModel are
// pure reads with no natural "this agent is now active" hook the way
// StartChat/SetAgent already have one. A restart's zero-flag auto-resume
// (Design §5's earlier addendum) therefore still reflects the last agent a
// chat was *started* or *switched to* with, not the last one merely
// resumed — a narrow enough gap not to warrant adding a write path to what
// would otherwise stay a read-only resume flow.
func resumeChatCmd(svc *services.AgentService, ctx context.Context, chatID string) tea.Cmd {
	return func() tea.Msg {
		chat, err := svc.GetChat(ctx, chatID)
		if err != nil {
			return chatStartedMsg{err: fmt.Errorf("resuming chat: %w", err)}
		}
		todos, err := svc.GetTodos(ctx, chatID)
		if err != nil {
			return chatStartedMsg{err: fmt.Errorf("loading todos: %w", err)}
		}
		model, err := svc.GetModel(ctx, chatID)
		if err != nil {
			return chatStartedMsg{err: fmt.Errorf("loading model: %w", err)}
		}
		capabilities, err := svc.AgentCapabilities(ctx, chat.AgentName)
		if err != nil {
			return chatStartedMsg{err: fmt.Errorf("loading agent capabilities: %w", err)}
		}
		return chatStartedMsg{
			chatID:       chatID,
			agentName:    chat.AgentName,
			todos:        todos,
			model:        model,
			messages:     chat.Messages,
			capabilities: capabilities,
			usage:        chat.Usage,
		}
	}
}

// historyLoadedMsg reports the outcome of ctrl+h's ListChatSummaries call
// (post-v0.1.0 addendum) — Model.Update opens the top-level screenHistory
// screen or the in-chat history overlay depending on which screen requested
// it (see loadHistoryCmd's doc comment for why this can't be answered
// synchronously the way ctrl+a/ctrl+o/ctrl+s's overlays are).
type historyLoadedMsg struct {
	summaries []services.ChatSummary
	err       error
}

// loadHistoryCmd calls AgentService.ListChatSummaries (post-v0.1.0
// addendum: ctrl+h) asynchronously. Unlike openPicker/openSkillPicker's
// ListAgents/ListModelSummaries/ListSkills — pure in-memory reads that can
// build their overlay synchronously inside handleKey — ListChatSummaries
// calls through to interfaces.ChatRepository.List, real disk I/O (reading
// every persisted chat file), so it has to go through the normal
// Cmd/Msg round trip like starting a turn does, not the picker overlays'
// synchronous shortcut.
func loadHistoryCmd(svc *services.AgentService, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		summaries, err := svc.ListChatSummaries(ctx)
		return historyLoadedMsg{summaries: summaries, err: err}
	}
}

// chatDeletedMsg reports the outcome of deleting a chat (post-v0.1.0
// addendum: "d" in ctrl+h's history browser) — chatID is always set, even
// on error, so Model.Update knows which list item to remove on success or
// which one to report as failed. Routed to whichever list the deleted
// item came from — the top-level screenHistory screen or chatModel's
// in-chat overlay — the same dual-dispatch historyLoadedMsg's own doc
// comment explains for the same reason (ctrl+h is reachable from both).
type chatDeletedMsg struct {
	chatID string
	err    error
}

// deleteChatCmd calls AgentService.DeleteChat asynchronously — real disk
// I/O (removing the chat's JSON file), the same reasoning loadHistoryCmd's
// own doc comment gives for not doing this synchronously inline in
// handleKey/handlePickerKey.
func deleteChatCmd(svc *services.AgentService, ctx context.Context, chatID string) tea.Cmd {
	return func() tea.Msg {
		err := svc.DeleteChat(ctx, chatID)
		return chatDeletedMsg{chatID: chatID, err: err}
	}
}

// generateTitleCmd calls AgentService.GenerateChatTitle (post-v0.1.0
// addendum) for chatID and returns nil — fire-and-forget, on the same
// "a nil tea.Msg is a no-op" contract waitForStreamEvent's own doc comment
// already relies on. Nothing in the UI needs to react to a title actually
// landing (the history browser reads it fresh from disk, via
// ListChatSummaries, the next time it's opened), so there's no
// titleGeneratedMsg to route back through Update — only Model.Update's
// caller (the streamDoneMsg case) needs to know *whether* to fire this, via
// chatModel's own titleAttempted guard, not what the outcome was. A failure
// is logged by AgentService itself (its Logger, if set) the same
// best-effort way RecordLastAgent's own failures already are — see
// GenerateChatTitle's doc comment on why a failed generation is never
// user-facing as an error.
func generateTitleCmd(svc *services.AgentService, ctx context.Context, chatID string) tea.Cmd {
	return func() tea.Msg {
		_, _ = svc.GenerateChatTitle(ctx, chatID)
		return nil
	}
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
		// WithRetryNotifier (providers package): reports a throttled/
		// retried provider request back onto ch as a non-terminal
		// streamRetryMsg, the same way a real response chunk arrives as
		// streamChunkMsg — see that type's doc comment for why a blocking
		// send here is safe (a waitForStreamEvent(ch) Cmd is always already
		// pending while this goroutine runs).
		notifyCtx := providers.WithRetryNotifier(ctx, func(notice providers.RetryNotice) {
			ch <- streamRetryMsg{notice: notice}
		})
		stream, finalize, err := svc.RunMessagesStream(notifyCtx, chatID, msgs)
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
