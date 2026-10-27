package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/agent/harness/todo"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/domain/services"
	"github.com/drujensen/canopy/internal/impl/agentsource"
	"github.com/drujensen/canopy/internal/impl/providers"
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
	"github.com/drujensen/canopy/internal/impl/skillsource"
	"github.com/drujensen/canopy/internal/impl/tools"
)

// --- synthetic-message tests: no real AgentService/network involved ---
//
// Bubble Tea models are unit-testable by driving Update/handleStreamMsg
// directly with hand-built messages (Plan Phase 6's testing guidance) —
// these tests never touch a real provider or repository.

// TestChatModel_StreamedContentAccumulates drives handleStreamMsg with
// synthetic streamChunkMsg values carrying *message.TextContent, and
// asserts the in-flight text accumulates incrementally (Requirements FR8)
// before being folded into the transcript once a terminal streamDoneMsg
// arrives.
func TestChatModel_StreamedContentAccumulates(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	cmd := c.handleStreamMsg(streamChunkMsg{update: &agent.ResponseUpdate{
		Contents: message.Contents{&message.TextContent{Text: "Hello"}},
	}}, nil, nil)
	assert.Equal(t, "Hello", streamingText(c))
	assert.Contains(t, c.View(80, 24), "Hello", "in-flight streamed text must be visible before the turn finishes")
	// A real chunk re-arms waitForStreamEvent; here streamCh is nil (no real
	// turn in flight), which is fine — the test never calls cmd().
	_ = cmd

	c.handleStreamMsg(streamChunkMsg{update: &agent.ResponseUpdate{
		Contents: message.Contents{&message.TextContent{Text: ", world"}},
	}}, nil, nil)
	assert.Equal(t, "Hello, world", streamingText(c))

	require.Empty(t, c.transcript, "nothing should be folded into the transcript until the turn's stream is done")

	// nil svc/ctx is safe here: handleStreamMsg's streamDoneMsg case never
	// touches either.
	c.handleStreamMsg(streamDoneMsg{result: &services.RunResult{
		Response: &agent.Response{},
		Todos:    nil,
	}}, nil, nil)

	require.Len(t, c.transcript, 1)
	assert.Equal(t, "Hello, world", segmentsText(c.transcript[0].segments))
	assert.Equal(t, message.RoleAssistant, c.transcript[0].role)
	assert.Empty(t, streamingText(c), "the streaming buffer must be cleared once folded into the transcript")
	assert.False(t, c.streamActive)
}

// TestChatModel_RetryNotice_ShowsThenClearsOnRealContent drives
// handleStreamMsg with a synthetic streamRetryMsg (stream.go) the way a
// real 413/429 throttle from providers.openaiRetryableStatusOption would
// arrive mid-turn, and asserts: the bottom status line shows it in place of
// "Thinking..." while streamActive, and it's cleared the moment real
// content starts flowing (streamChunkMsg) — the throttle has resolved by
// then, so it shouldn't linger.
func TestChatModel_RetryNotice_ShowsThenClearsOnRealContent(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.streamActive = true // set by startTurnCmd in real use; handleStreamMsg doesn't touch it

	c.handleStreamMsg(streamRetryMsg{notice: providers.RetryNotice{
		StatusCode:  http.StatusRequestEntityTooLarge,
		Reason:      "rate limited (413, too many input tokens per minute)",
		Attempt:     1,
		MaxAttempts: 6,
		Delay:       2 * time.Second,
	}}, nil, nil)

	assert.NotEmpty(t, c.retryNotice)
	view := c.View(80, 24)
	assert.Contains(t, view, "Throttled")
	assert.Contains(t, view, "rate limited (413")
	assert.NotContains(t, view, "Thinking...", "the retry notice must replace, not join, the plain Thinking line")

	c.handleStreamMsg(streamChunkMsg{update: &agent.ResponseUpdate{
		Contents: message.Contents{&message.TextContent{Text: "hello after retry"}},
	}}, nil, nil)

	assert.Empty(t, c.retryNotice, "a real chunk means the throttle resolved; the notice must not linger")
	assert.Contains(t, c.View(80, 24), "Thinking...")
}

// TestChatModel_TodoPanelReflectsRunResult proves the live todo panel
// (Design §3.7) renders whatever Todos a turn's RunResult carries.
func TestChatModel_TodoPanelReflectsRunResult(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	c.handleStreamMsg(streamDoneMsg{result: &services.RunResult{
		Response: &agent.Response{},
		Todos: []todo.Item{
			{ID: 1, Title: "Write the report", IsComplete: false},
			{ID: 2, Title: "Send the email", IsComplete: true},
		},
	}}, nil, nil)

	require.Len(t, c.todos, 2)
	view := c.View(80, 24)
	assert.Contains(t, view, "Write the report")
	assert.Contains(t, view, "Send the email")
	assert.Contains(t, view, "[ ] Write the report")
}

// TestChatModel_View_LongMultilineError_StaysOneLineAndKeepsSidebarVisible
// guards against the secondary layout bug this task flagged: a long,
// multi-line statusErr (e.g. a real provider's JSON error body, which
// commonly contains embedded newlines and exceeds the terminal's width) used
// to be handed straight to lipgloss.Render with no wrapping/clipping,
// growing the rendered view taller than resize's fixed viewport/composer
// budget accounts for and, in a real terminal, scrolling the
// sidebar/transcript above it out of view. Asserts the
// rendered view's error line is clamped to the chat's main-column width (so
// it can't add unbudgeted rows) and that the sidebar's agent indicator is
// still present in the same render.
func TestChatModel_View_LongMultilineError_StaysOneLineAndKeepsSidebarVisible(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.statusErr = fmt.Errorf("error: POST \"https://api.example.com/v1/chat/completions\": 429 Too Many Requests {\n  \"error\": {\n    \"message\": \"You exceeded your current quota, please check your plan and billing details. For more information on this error, see: https://platform.openai.com/docs/guides/error-codes/api-errors.\",\n    \"type\": \"insufficient_quota\",\n    \"param\": null,\n    \"code\": \"insufficient_quota\"\n  }\n}")

	view := c.View(80, 24)

	assert.NotContains(t, view, "\n\n\n", "a clamped single-line error must not introduce extra blank rows into the rendered view")
	for _, line := range strings.Split(view, "\n") {
		assert.LessOrEqual(t, len([]rune(line)), 80, "no rendered line (including the error line) should exceed the requested width")
	}
	assert.Contains(t, view, "Agent: assistant", "the sidebar's agent indicator must still render alongside a long status error")
}

// TestChatModel_View_LongTranscriptLine_WordWraps is a regression test for a
// real reported bug: transcript text (a long assistant reply, a pasted user
// message, a long skill body shown via ctrl+s) was rendered with no width
// constraint at all — renderEntry just concatenated label+text straight into
// the viewport, so any line longer than the terminal's width ran off-screen
// unreadably instead of wrapping. Asserts every rendered line stays within
// the requested total width and that none of the original words were
// dropped or corrupted by wrapping.
func TestChatModel_View_LongTranscriptLine_WordWraps(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	longLine := "This is a single very long line of assistant response text with no embedded newlines at all that must be word-wrapped to fit the available viewport width instead of running off the edge of the terminal and becoming unreadable to the user."
	c.transcript = append(c.transcript, transcriptEntry{role: message.RoleAssistant, text: longLine})
	c.refreshViewport()

	view := c.View(80, 24)

	for _, line := range strings.Split(view, "\n") {
		assert.LessOrEqual(t, len([]rune(line)), 80, "no rendered line should exceed the requested total width")
	}
	// Every word from the original text must still be present somewhere in
	// the rendered view — wrapping must break lines at spaces, never drop or
	// mangle the actual words.
	for _, word := range strings.Fields(longLine) {
		assert.Contains(t, view, word, "wrapping must not drop or corrupt words from the original text")
	}
}

// TestChatModel_EmptyTranscript_ShowsGreeting_ClearedByFirstMessage asserts
// the post-v0.1.0 addendum: a brand-new chat's viewport shows a cosmetic
// greeting instead of dead space, that greeting is never part of
// transcript (so it can never be sent to the model or persisted), and it's
// gone the instant a real message exists — proving handleKey's "enter"
// case's own refreshViewport call already replaces it, no separate
// "clear the greeting" step needed anywhere.
func TestChatModel_EmptyTranscript_ShowsGreeting_ClearedByFirstMessage(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	assert.Contains(t, c.viewport.View(), defaultGreeting, "an empty chat must show the greeting rather than a blank scrollback")
	assert.Empty(t, c.transcript, "the greeting must never be appended to transcript")

	c.transcript = append(c.transcript, transcriptEntry{role: message.RoleUser, text: "hello"})
	c.refreshViewport()

	assert.NotContains(t, c.viewport.View(), defaultGreeting, "the greeting must disappear once the transcript has real content")
	assert.Contains(t, c.viewport.View(), "hello")
}

// TestChatModel_View_StreamActive_ShowsSpinnerNotComposer asserts the
// post-v0.1.0 "still working" indicator: while a turn is in flight, View
// replaces the composer with a spinner + status line (and the esc-to-cancel
// hint), and doesn't also render the composer's own placeholder text —
// there should be exactly one bottom-area affordance, not both layered.
func TestChatModel_View_StreamActive_ShowsSpinnerNotComposer(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	idleView := c.View(80, 24)
	assert.Contains(t, idleView, "Type a message", "the composer's placeholder must render while idle")
	assert.NotContains(t, idleView, "Thinking", "no spinner/status text while idle")

	c.streamActive = true
	activeView := c.View(80, 24)
	assert.Contains(t, activeView, "Thinking", "a visual indicator must render while a turn is in flight")
	assert.Contains(t, activeView, "esc to cancel", "the indicator must tell the user how to cancel")
	assert.NotContains(t, activeView, "Type a message", "the composer's placeholder must not render while streaming")
}

// TestChatModel_StartTurnCmd_ClearsPriorStatusErr asserts the post-v0.1.0
// "an error should only show for one turn" behavior: a statusErr left over
// from a previous turn's failure must be gone the instant the next turn
// starts (startTurnCmd), not merely once that next turn itself finishes —
// otherwise a fast-failing new turn could layer a fresh error on screen
// alongside the stale one for a moment, or a canceled new turn could leave
// the *old* error visible again.
func TestChatModel_StartTurnCmd_ClearsPriorStatusErr(t *testing.T) {
	svc, closeServer := newLeakTestService(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("ok"))
	})
	t.Cleanup(closeServer)

	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.statusErr = fmt.Errorf("stale error from a previous turn")

	// startTurnCmd itself, synchronously, before any stream event has been
	// processed — proves the clear happens at turn-start, not as a side
	// effect of the turn's own eventual success/failure.
	cmd := c.startTurnCmd(svc, ctx, []*message.Message{message.NewText("hi")})
	assert.Nil(t, c.statusErr, "statusErr must already be cleared once startTurnCmd returns, before the turn even completes")

	drainCmd(t, c, svc, ctx, cmd)
	assert.Nil(t, c.statusErr, "a successful turn must not have re-introduced an error")
}

// TestModel_NoAgentsConfigured_ShowsActionableMessage covers the picker
// screen's empty-state, which cmd/canopy/main.go's own startup check exists
// specifically to avoid reaching in the first place — this asserts the TUI
// itself is defensive too.
func TestModel_NoAgentsConfigured_ShowsActionableMessage(t *testing.T) {
	m := NewModel(context.Background(), nil, nil, "", "")
	assert.Contains(t, m.View(), "No agents configured")
}

// TestNewModel_SortsAgentNames asserts the picker's agent list is in a
// deterministic (sorted) order rather than Go's randomized map iteration
// order, since a flaky picker order would make manual QA (and any future
// snapshot test) unreliable.
func TestNewModel_SortsAgentNames(t *testing.T) {
	m := NewModel(context.Background(), nil, map[string]agentsource.AgentDefinition{
		"zeta":  {Name: "zeta", Description: "z"},
		"alpha": {Name: "alpha", Description: "a"},
		"mid":   {Name: "mid", Description: "m"},
	}, "", "")
	assert.Equal(t, []string{"alpha", "mid", "zeta"}, m.agentNames)
}

// TestModel_Init_AutoResumesStartAgent asserts a non-empty startAgent
// (post-v0.1.0 addendum: cmd/canopy's computeStartAgent) makes Init produce
// the same Cmd a manual picker selection would — draining it (one Cmd -> one
// Msg -> Model.Update, the same loop Bubble Tea's real runtime performs)
// must land on screenChat with a real *chatModel bound to that agent,
// without the user ever seeing the picker screen.
func TestModel_Init_AutoResumesStartAgent(t *testing.T) {
	svc, closeServer := newLeakTestService(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("ok"))
	})
	t.Cleanup(closeServer)

	m := NewModel(context.Background(), svc, map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}}, "assistant", "")
	require.Equal(t, screenAgentPicker, m.screen, "Model is still constructed on the picker screen; only Init decides whether it's ever shown")

	cmd := m.Init()
	require.NotNil(t, cmd, "a non-empty startAgent must produce a Cmd")

	msg := cmd()
	require.IsType(t, chatStartedMsg{}, msg)

	next, _ := m.Update(msg)
	m2 := next.(Model)
	assert.Equal(t, screenChat, m2.screen)
	require.NotNil(t, m2.chat)
	assert.Equal(t, "assistant", m2.chat.agentName)
	assert.Nil(t, m2.fatalErr)
}

// TestModel_Init_EmptyStartAgent_StaysOnPicker asserts the prior, default
// behavior (no last-used-agent record, or explicitly no agents configured)
// is unchanged: Init returns nil and the picker screen is what's shown.
func TestModel_Init_EmptyStartAgent_StaysOnPicker(t *testing.T) {
	m := NewModel(context.Background(), nil, map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}}, "", "")
	assert.Nil(t, m.Init())
	assert.Equal(t, screenAgentPicker, m.screen)
}

// TestModel_Init_ResumeChatID_TakesPriorityOverStartAgent asserts
// --continue's resumeChatID (post-v0.1.0 addendum) wins over startAgent
// when both are somehow set — --continue is an explicit, one-shot request,
// a stronger signal than the passive last-used-agent default.
func TestModel_Init_ResumeChatID_TakesPriorityOverStartAgent(t *testing.T) {
	svc, closeServer := newLeakTestService(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("ok"))
	})
	t.Cleanup(closeServer)

	ctx := context.Background()
	_, err := svc.StartChat(ctx, "existing-chat", "assistant")
	require.NoError(t, err)

	m := Model{svc: svc, ctx: ctx, screen: screenAgentPicker, startAgent: "assistant", resumeChatID: "existing-chat"}
	cmd := m.Init()
	require.NotNil(t, cmd)

	msg := cmd()
	started, ok := msg.(chatStartedMsg)
	require.True(t, ok)
	assert.Equal(t, "existing-chat", started.chatID, "resumeChatID must be used, not startAgent's fresh-chat path")
}

// ---------------------------------------------------------------------
// reconstructTranscript (post-v0.1.0 addendum: ctrl+h/--continue)
// ---------------------------------------------------------------------

func TestReconstructTranscript_IncludesUserAndAssistantText(t *testing.T) {
	msgs := []*message.Message{
		{Role: message.RoleUser, Contents: message.Contents{&message.TextContent{Text: "hi"}}},
		{Role: message.RoleAssistant, Contents: message.Contents{&message.TextContent{Text: "hello there"}}},
	}
	entries := reconstructTranscript(msgs)
	require.Len(t, entries, 2)
	assert.Equal(t, message.RoleUser, entries[0].role)
	assert.Equal(t, "hi", entries[0].text)
	assert.Equal(t, message.RoleAssistant, entries[1].role)
	assert.Equal(t, "hello there", segmentsText(entries[1].segments), "an assistant turn renders from segments, the same as a live turn (finishStreaming), not entry.text")
}

// TestReconstructTranscript_IncludesToolCallsAndResults is the regression
// test for the behavior change answering "why can't I see the tool calls
// after resuming a chat": a FunctionCallContent (assistant-role) and its
// matching FunctionResultContent (its own tool-role message — exactly the
// shape a real turn persists, confirmed against AgentService's actual
// output, not a guess) must now render as toolCall/toolResult segments
// within the surrounding assistant turn, the same as they would have
// rendered live via applyUpdate. nil messages are still skipped (a resumed
// chat's slice is never expected to contain one, but reconstructTranscript
// has always tolerated it defensively).
func TestReconstructTranscript_IncludesToolCallsAndResults(t *testing.T) {
	msgs := []*message.Message{
		{Role: message.RoleUser, Contents: message.Contents{&message.TextContent{Text: "read the file"}}},
		{Role: message.RoleAssistant, Contents: message.Contents{&message.FunctionCallContent{CallID: "call-1", Name: "file_read", Arguments: `{"path":"a.txt"}`}}},
		{Role: message.RoleTool, Contents: message.Contents{&message.FunctionResultContent{CallID: "call-1", Result: "file contents"}}},
		nil,
		{Role: message.RoleAssistant, Contents: message.Contents{&message.TextContent{Text: "here's the summary"}}},
	}
	entries := reconstructTranscript(msgs)
	require.Len(t, entries, 2)
	assert.Equal(t, message.RoleUser, entries[0].role)
	assert.Equal(t, "read the file", entries[0].text)

	require.Equal(t, message.RoleAssistant, entries[1].role)
	require.Len(t, entries[1].segments, 2, "the tool call headline and the final text must both be present (file_read has no special-cased result preview, so only the call itself and the reply show)")
	assert.Equal(t, segmentToolCall, entries[1].segments[0].kind)
	assert.Contains(t, entries[1].segments[0].text, "file_read")
	assert.Contains(t, entries[1].segments[0].text, "a.txt")
	assert.Equal(t, segmentProse, entries[1].segments[1].kind)
	assert.Equal(t, "here's the summary", entries[1].segments[1].text)
}

// TestReconstructTranscript_ToolResultSurvivesJSONRoundTrip proves
// decodeFunctionResult actually does its job: FunctionResultContent.Result
// is declared `any` with no concrete type preserved across persistence
// (message.FunctionResultContent's own (de)serialization, confirmed
// directly against a real persisted chat — see decodeFunctionResult's doc
// comment), so a resumed chat's Result for a file_write call arrives as a
// generic map[string]any, not the original tools.FileWriteOutput — exactly
// what's built here instead of using the struct literal, to exercise the
// real post-resume shape rather than the live in-process one.
func TestReconstructTranscript_ToolResultSurvivesJSONRoundTrip(t *testing.T) {
	genericResult := map[string]any{"path": "a.txt", "lines_added": float64(3), "lines_removed": float64(1), "bytes_written": float64(42)}
	msgs := []*message.Message{
		{Role: message.RoleUser, Contents: message.Contents{&message.TextContent{Text: "update the file"}}},
		{Role: message.RoleAssistant, Contents: message.Contents{&message.FunctionCallContent{CallID: "call-1", Name: "file_write", Arguments: `{"path":"a.txt"}`}}},
		{Role: message.RoleTool, Contents: message.Contents{&message.FunctionResultContent{CallID: "call-1", Result: genericResult}}},
	}
	entries := reconstructTranscript(msgs)
	require.Len(t, entries, 2)
	require.Len(t, entries[1].segments, 2)
	assert.Equal(t, segmentToolResult, entries[1].segments[1].kind)
	assert.Equal(t, "wrote a.txt (+3 -1 lines, 42 bytes)", entries[1].segments[1].text)
}

func TestReconstructTranscript_Empty(t *testing.T) {
	assert.Empty(t, reconstructTranscript(nil))
}

// --- real-AgentService integration tests: exercise the actual production
// wiring (chatModel.handleKey -> startTurnCmd -> startTurn ->
// AgentService.RunMessagesStream) end to end against a mock HTTP provider,
// draining the returned tea.Cmd chain synchronously the same way Bubble
// Tea's runtime would (one Cmd -> one Msg -> next Cmd), proving the
// approval flow's two keybindings (Design §3.6) actually thread the right
// message.Content back to AgentService, not just that the reducer logic
// looks right in isolation. ---

// toolCallChatCompletionResponse mirrors phase5_test.go's helper of the
// same name (package services): a minimal OpenAI Chat Completions response
// requesting a single tool call.
func toolCallChatCompletionResponse(callID, funcName, argsJSON string) map[string]any {
	return map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion", "created": 1700000000, "model": "test-model",
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role": "assistant", "content": nil,
				"tool_calls": []map[string]any{{
					"id": callID, "type": "function",
					"function": map[string]any{"name": funcName, "arguments": argsJSON},
				}},
			},
			"finish_reason": "tool_calls",
		}},
	}
}

func chatCompletionResponse(text string) map[string]any {
	return map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion", "created": 1700000000, "model": "test-model",
		"choices": []map[string]any{{
			"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop",
		}},
	}
}

// drainCmd repeatedly executes cmd, feeding each resulting tea.Msg back into
// c.handleStreamMsg to obtain the next Cmd, the same loop Bubble Tea's
// runtime performs — until a nil Cmd (or nil Msg, i.e. a closed channel)
// ends the turn.
//
// handleKey's "enter" case (post-v0.1.0 addendum: the "still working"
// spinner) returns tea.Batch(startTurnCmd(...), spinner.Tick) rather than
// startTurnCmd's bare Cmd, so cmd() can yield a tea.BatchMsg — unlike every
// other Msg this loop sees, that's not something c.handleStreamMsg
// understands (it only switches on streamChunkMsg/streamDoneMsg/
// streamErrMsg), so it must be unwrapped here instead of fed straight
// through: run every sub-Cmd once, keep draining whichever one actually
// produced a real stream message, and silently drop the rest (the spinner
// tick, whose own resulting spinner.TickMsg isn't relevant to any test
// using this helper — none of them assert on spinner animation, only on
// the underlying turn completing without hanging or leaking).
//
// svc/ctx are threaded through to every handleStreamMsg call for signature
// symmetry; handleStreamMsg itself doesn't use them.
// segmentsText concatenates segments' raw texts, newline-joined — a
// test-only convenience so assertions can check content without caring
// about per-segment kind/styling (production rendering goes through
// renderSegments, which does care).
func segmentsText(segments []textSegment) string {
	parts := make([]string, len(segments))
	for i, seg := range segments {
		parts[i] = seg.text
	}
	return strings.Join(parts, "\n")
}

// streamingText is segmentsText for c's in-flight streaming buffer.
func streamingText(c *chatModel) string {
	return segmentsText(c.streaming)
}

func drainCmd(t *testing.T, c *chatModel, svc *services.AgentService, ctx context.Context, cmd tea.Cmd) {
	t.Helper()
	for cmd != nil {
		msg := cmd()
		if msg == nil {
			return
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			cmd = nil
			for _, sub := range batch {
				if sub == nil {
					continue
				}
				subMsg := sub()
				if subMsg == nil {
					continue
				}
				switch subMsg.(type) {
				case streamChunkMsg, streamDoneMsg, streamErrMsg:
					cmd = c.handleStreamMsg(subMsg, svc, ctx)
				}
			}
			continue
		}
		cmd = c.handleStreamMsg(msg, svc, ctx)
	}
}

// newBashToolTestService builds a real AgentService (JSON-backed repository
// in a temp dir, one OpenAI-compatible provider pointed at an httptest
// server) whose model alternates: an odd-numbered call requests the
// run_shell tool against a marker file, an even-numbered call answers "ok"
// — toolautocall makes exactly this odd/even pair of calls per tool
// invocation (the tool-call request, then the follow-up completion once
// the tool has executed), reproduced here so this package's tests don't
// need to depend on the unexported services package's own helpers.
func newBashToolTestService(t *testing.T) (svc *services.AgentService, marker string) {
	t.Helper()
	workDir := t.TempDir()
	marker = filepath.Join(workDir, "marker.txt")

	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		if n%2 == 1 {
			args, err := json.Marshal(map[string]string{"command": fmt.Sprintf("echo run-%d >> %s", n, marker)})
			require.NoError(t, err)
			_ = json.NewEncoder(w).Encode(toolCallChatCompletionResponse(fmt.Sprintf("call-%d", n), "run_shell", string(args)))
			return
		}
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("ok"))
	}))
	t.Cleanup(server.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderType("openai-compatible"), APIKey: "sk-test", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gpt-test"}

	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc = services.NewAgentService(services.AgentServiceConfig{
		Definitions: services.Definitions{
			Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant", Description: "test agent"}},
		},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Repository:   repo,
		Tools:        services.ToolsConfig{WorkingRoot: workDir},
	})
	return svc, marker
}

// TestChatModel_ToolExecutesWithoutApproval is this package's coverage that
// the Bash tool runs directly, with no approval step ever in the way.
// Drives the real path (startTurnCmd, drainCmd) for two consecutive
// run_shell requests in the same chat and asserts both actually execute the
// tool, immediately, within their own single turn.
func TestChatModel_ToolExecutesWithoutApproval(t *testing.T) {
	svc, marker := newBashToolTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	drainCmd(t, c, svc, ctx, c.startTurnCmd(svc, ctx, []*message.Message{message.NewText("please run the marker command")}))
	_, statErr := os.Stat(marker)
	require.NoError(t, statErr, "the tool must have executed automatically, with no approval step in the way")

	require.NoError(t, os.Remove(marker))
	drainCmd(t, c, svc, ctx, c.startTurnCmd(svc, ctx, []*message.Message{message.NewText("please run it again")}))
	_, statErr = os.Stat(marker)
	assert.NoError(t, statErr, "the tool must have executed again automatically on a second request")
}

// TestChatModel_ResumedChat_ShowsToolCallsAndResults is the real
// end-to-end regression test behind reconstructTranscript's behavior
// change: run an actual tool-using turn through the real
// AgentService/JSON-backed-repository path, then resume that exact chat
// the way --continue/ctrl+h do (resumeChatCmd), and confirm the resumed
// transcript actually shows the tool call — not just the user/assistant
// text either side of it, which is all a resumed chat used to show.
func TestChatModel_ResumedChat_ShowsToolCallsAndResults(t *testing.T) {
	svc, marker := newBashToolTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	drainCmd(t, c, svc, ctx, c.startTurnCmd(svc, ctx, []*message.Message{message.NewText("please run the marker command")}))
	_, statErr := os.Stat(marker)
	require.NoError(t, statErr, "the tool must have actually executed for this test to mean anything")

	resumeMsg := resumeChatCmd(svc, ctx, "chat-1")()
	started, ok := resumeMsg.(chatStartedMsg)
	require.True(t, ok)
	require.NoError(t, started.err)
	require.NotEmpty(t, started.messages)

	transcript := reconstructTranscript(started.messages)
	var sawToolCall bool
	for _, e := range transcript {
		for _, seg := range e.segments {
			if seg.kind == segmentToolCall && strings.Contains(seg.text, "run_shell") {
				sawToolCall = true
			}
		}
	}
	assert.True(t, sawToolCall, "resuming the chat must show the tool call that ran during the live session, not just the final text")
}

// TestChatModel_ResumedChat_ShowsRealUsageCost reproduces a reported bug:
// switching to a chat via ctrl+h's history browser showed the sidebar's
// Cost line as "(none yet)" even though the chat's real conversation (and
// its real token spend) was plainly visible in the resumed transcript.
// Drives a real turn against a provider that reports actual token usage,
// confirms the live chat's own c.usage reflects it, then resumes that exact
// chat the way ctrl+h does (resumeChatCmd) and asserts the resumed
// chatStartedMsg carries the identical usage — not an empty map.
func TestChatModel_ResumedChat_ShowsRealUsageCost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "created": 1700000000, "model": "test-model",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150},
		})
	}))
	t.Cleanup(server.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderType("openai-compatible"), APIKey: "sk-test", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m1", Provider: "p", ModelName: "gpt-test", InputCostPerMillionTokens: 2, OutputCostPerMillionTokens: 10}
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	svc := services.NewAgentService(services.AgentServiceConfig{
		Definitions:  services.Definitions{Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}}},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: "m1",
		Repository:   repo,
		Tools:        services.ToolsConfig{WorkingRoot: t.TempDir()},
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	drainCmd(t, c, svc, ctx, c.startTurnCmd(svc, ctx, []*message.Message{message.NewText("hi")}))
	require.NotEmpty(t, c.usage, "the live chat's own usage must reflect the turn that just ran")

	resumeMsg := resumeChatCmd(svc, ctx, "chat-1")()
	started, ok := resumeMsg.(chatStartedMsg)
	require.True(t, ok)
	require.NoError(t, started.err)

	assert.Equal(t, c.usage, started.usage, "resuming the same chat via ctrl+h must carry the real recorded usage, not an empty map")
	assert.NotEqual(t, "(none yet)", usageCostLine(started.usage))
}

// --- ctrl+a (switch agent) / ctrl+o (switch model) overlay tests
// (post-v0.1.0 addendum, Design §3.4/§4/§5) ---
//
// Per the coordinator's correction to this feature's original design, ctrl+a
// does NOT navigate to the top-level agent picker screen or start a new
// chat — it behaves symmetrically with ctrl+o: an in-chat overlay
// (chatModel.picker) that, on selection, calls AgentService.SetAgent/
// SetModel against the *same* chat ID and returns to the chat screen with
// history untouched.

// newSwitchTestService builds a real AgentService (JSON-backed repository in
// a temp dir, two agent definitions, two models) for driving the ctrl+a/
// ctrl+o overlay's real keybinding path without needing a provider/network
// call — SetAgent/SetModel only touch the JSON repository, the same reason
// TestChatModel_ModeToggle_RealSetMode's service needs no provider.
func newSwitchTestService(t *testing.T) *services.AgentService {
	t.Helper()
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	return services.NewAgentService(services.AgentServiceConfig{
		Definitions: services.Definitions{
			Agents: map[string]agentsource.AgentDefinition{
				"assistant": {Name: "assistant"},
				"helper":    {Name: "helper"},
			},
		},
		Providers:    []entities.ProviderConfig{{Name: "p", Type: entities.ProviderTypeOpenAI}},
		Models:       []entities.ModelConfig{{Name: "m1", Provider: "p"}, {Name: "m2", Provider: "p"}},
		DefaultModel: "m1",
		Repository:   repo,
	})
}

// TestChatModel_CtrlA_OpensAgentPicker_SelectingSwitchesAgentKeepsChat drives
// the real ctrl+a keybinding path end to end: it opens the overlay
// (pickerAgent), navigates to and selects the other loaded agent, and
// asserts SetAgent was actually called (via the real AgentService/
// repository, not just that a message got produced) — the persisted chat's
// AgentName changes, the chat ID never changes, and the transcript built up
// before the switch is untouched, proving this is an in-place agent switch,
// not "abandon this chat and start a new one".
func TestChatModel_CtrlA_OpensAgentPicker_SelectingSwitchesAgentKeepsChat(t *testing.T) {
	svc := newSwitchTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.transcript = append(c.transcript, transcriptEntry{role: message.RoleUser, text: "hello before the switch"})

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlA}, svc, ctx)
	assert.Nil(t, cmd, "opening the picker is a synchronous, in-memory ListAgents() read — no Cmd needed")
	require.NotNil(t, c.picker, "ctrl+a must open the in-chat overlay picker")
	assert.Equal(t, pickerAgent, c.pickerKind)

	// Agent names are sorted (ListAgents): "assistant", "helper". Move down
	// to select "helper", the agent the chat isn't currently bound to.
	c.handleKey(tea.KeyMsg{Type: tea.KeyDown}, svc, ctx)
	selectCmd := c.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, svc, ctx)
	assert.Nil(t, c.picker, "selecting an entry must close the overlay immediately")

	require.NotNil(t, selectCmd)
	msg := selectCmd()
	agentMsg, ok := msg.(agentChangedMsg)
	require.True(t, ok, "selecting an agent must produce an agentChangedMsg, got %#v", msg)
	assert.Equal(t, "helper", agentMsg.agentName)

	// Model.Update applies agentChangedMsg onto c.agentName (see model.go) —
	// simulate that here, matching TestChatModel_ModeToggle_RealSetMode's own
	// note on Bubble Tea's Cmd/Msg round trip.
	c.agentName = agentMsg.agentName
	assert.Equal(t, "helper", c.agentName)

	chat, err := svc.GetModel(ctx, "chat-1") // any real-repository read proves the chat ID is unchanged
	require.NoError(t, err)
	assert.Equal(t, "m1", chat, "the chat's model must be untouched by an agent switch")

	require.Len(t, c.transcript, 1, "switching agents must not clear or reset the in-chat transcript")
	assert.Equal(t, "hello before the switch", c.transcript[0].text)
}

// TestChatModel_CtrlO_OpensModelPicker_SelectingSwitchesModel mirrors the
// ctrl+a test above for ctrl+o/SetModel.
func TestChatModel_CtrlO_OpensModelPicker_SelectingSwitchesModel(t *testing.T) {
	svc := newSwitchTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlO}, svc, ctx)
	assert.Nil(t, cmd)
	require.NotNil(t, c.picker, "ctrl+o must open the in-chat overlay picker")
	assert.Equal(t, pickerModel, c.pickerKind)

	// Model names are sorted (ListModels): "m1", "m2". Move down to select
	// "m2".
	c.handleKey(tea.KeyMsg{Type: tea.KeyDown}, svc, ctx)
	selectCmd := c.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, svc, ctx)
	assert.Nil(t, c.picker)

	require.NotNil(t, selectCmd)
	msg := selectCmd()
	modelMsg, ok := msg.(modelChangedMsg)
	require.True(t, ok, "selecting a model must produce a modelChangedMsg, got %#v", msg)
	assert.Equal(t, "m2", modelMsg.model)

	c.model = modelMsg.model
	assert.Equal(t, "m2", c.model)

	gotModel, err := svc.GetModel(ctx, "chat-1")
	require.NoError(t, err)
	assert.Equal(t, "m2", gotModel, "SetModel must have actually persisted the switch, not just produced a message")
}

// TestChatModel_CtrlA_CtrlO_NoopDuringActiveStream asserts ctrl+a/ctrl+o are
// no-ops while a turn is actively streaming, the same guard "enter"
// (sending a message) already applies — switching agent/model mid-tool-call
// or mid-response must not be possible.
func TestChatModel_CtrlA_CtrlO_NoopDuringActiveStream(t *testing.T) {
	svc := newSwitchTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.streamActive = true

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlA}, svc, ctx)
	assert.Nil(t, cmd)
	assert.Nil(t, c.picker, "ctrl+a must not open the agent-switch overlay while a stream is active")

	cmd = c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlO}, svc, ctx)
	assert.Nil(t, cmd)
	assert.Nil(t, c.picker, "ctrl+o must not open the model-switch overlay while a stream is active")
	assert.True(t, c.streamActive, "streamActive must be untouched by the no-op key presses")
}

// --- ctrl+n (start a genuinely new chat) tests (post-v0.1.0 addendum) ---

// TestModel_CtrlN_StartsGenuinelyNewChat_ResetsState drives the real ctrl+n
// keybinding path end to end: chatModel.startNewChatCmd's tea.Cmd against a
// real AgentService, then Model.Update applying the resulting chatStartedMsg
// the same way it does for the top-level picker's own chat start. Asserts a
// brand-new chat ID is used (not the old chat rewritten), the new chat is
// bound to the *same* agent the old one was using (ctrl+n doesn't force the
// picker), and the resulting chatModel's transcript/todos are all
// genuinely empty/nil rather than carried over from the old chat.
func TestModel_CtrlN_StartsGenuinelyNewChat_ResetsState(t *testing.T) {
	svc := newSwitchTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.transcript = append(c.transcript, transcriptEntry{role: message.RoleUser, text: "hello before new chat"})
	c.todos = []todo.Item{{ID: 1, Title: "leftover todo"}}

	m := Model{svc: svc, ctx: ctx, screen: screenChat, chat: c, width: 80, height: 24}

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlN}, svc, ctx)
	require.NotNil(t, cmd, "ctrl+n must produce a tea.Cmd that starts a new chat")
	msg := cmd()
	startedMsg, ok := msg.(chatStartedMsg)
	require.True(t, ok, "ctrl+n must produce a chatStartedMsg, got %#v", msg)
	require.NoError(t, startedMsg.err)

	assert.NotEqual(t, "chat-1", startedMsg.chatID, "ctrl+n must generate a genuinely new chat ID, not reuse the old one")
	assert.Equal(t, "assistant", startedMsg.agentName, "the new chat must keep the current agent — ctrl+n must not force the user back through the agent picker")
	assert.Empty(t, startedMsg.todos, "a brand-new chat must start with no leftover todos")
	assert.Equal(t, "m1", startedMsg.model, "the new chat must carry over the old chat's active model")

	updated, updCmd := m.Update(startedMsg)
	assert.Nil(t, updCmd)
	next := updated.(Model)
	require.NotNil(t, next.chat)
	assert.Equal(t, startedMsg.chatID, next.chat.chatID)
	assert.Empty(t, next.chat.transcript, "a genuinely new chat must start with an empty transcript")
	assert.Empty(t, next.chat.todos, "a genuinely new chat must start with no leftover todos")
	assert.False(t, next.chat.streamActive)
	assert.Equal(t, "m1", next.chat.model)

	// Prove it's a new chat file, not the old one rewritten: the old chat ID
	// is still resolvable on its own and untouched.
	oldStillModel, err := svc.GetModel(ctx, "chat-1")
	require.NoError(t, err)
	assert.Equal(t, "m1", oldStillModel, "the old chat must still exist, untouched, under its own ID")
}

// TestModel_CtrlN_CarriesOverSwitchedModel is a regression test for a real
// reported bug: ctrl+n's new chat always resolved to
// AgentServiceConfig.DefaultModel — the model last used as of process
// *startup* — instead of whatever the user had actually switched to via
// ctrl+o earlier in the running session, because a fresh chat's
// ModelOverride starts empty and DefaultModel never changes after
// construction. Switches the current chat to a non-default model first,
// then asserts ctrl+n's new chat picks up that switched-to model, not
// DefaultModel — mirroring how ctrl+n already carries over the current
// agent instead of falling back to some session-wide default agent.
func TestModel_CtrlN_CarriesOverSwitchedModel(t *testing.T) {
	svc := newSwitchTestService(t) // DefaultModel: "m1", also configures "m2"
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)
	require.NoError(t, svc.SetModel(ctx, "chat-1", "m2"), "simulate the user switching models via ctrl+o earlier in the session")

	c := newChatModel("chat-1", "assistant", nil, "m2", 80, 24)

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlN}, svc, ctx)
	require.NotNil(t, cmd)
	msg := cmd()
	startedMsg, ok := msg.(chatStartedMsg)
	require.True(t, ok)
	require.NoError(t, startedMsg.err)

	assert.Equal(t, "m2", startedMsg.model, "ctrl+n's new chat must carry over the switched-to model, not fall back to DefaultModel (\"m1\")")

	// Also confirm it's a real, persisted override on the new chat, not just
	// a value that happened to be in the returned message.
	persistedModel, err := svc.GetModel(ctx, startedMsg.chatID)
	require.NoError(t, err)
	assert.Equal(t, "m2", persistedModel)
}

// TestChatModel_CtrlN_NoopDuringActiveStream asserts ctrl+n does not
// abandon an in-flight turn — the same guard ctrl+a/ctrl+o/enter already
// apply.
func TestChatModel_CtrlN_NoopDuringActiveStream(t *testing.T) {
	svc := newSwitchTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.streamActive = true

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlN}, svc, ctx)
	assert.Nil(t, cmd, "ctrl+n must be a no-op while a turn is actively streaming")
	assert.True(t, c.streamActive, "streamActive must be untouched by the no-op key press")
	assert.Equal(t, "chat-1", c.chatID, "the chat must not have been replaced while streaming")
}

// --- ctrl+s (skills browser) tests (post-v0.1.0 addendum, Design §3.11/FR19) ---

// newSkillsTestService builds a real AgentService with one loaded skill
// (GetSkillBody/ListSkills only touch in-memory Definitions.Skills, no
// provider/repository I/O needed for these tests beyond StartChat itself).
func newSkillsTestService(t *testing.T) *services.AgentService {
	t.Helper()
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	return services.NewAgentService(services.AgentServiceConfig{
		Definitions: services.Definitions{
			Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}},
			Skills: map[string]skillsource.SkillDefinition{
				"pdf-processing": {
					Name:        "pdf-processing",
					Description: "Extract text and tables from PDF files.",
					Body:        "# PDF Processing\n\nDistinctive real skill body content, marker XYZZY.",
					Dir:         t.TempDir(),
				},
			},
		},
		Providers:    []entities.ProviderConfig{{Name: "p", Type: entities.ProviderTypeOpenAI}},
		Models:       []entities.ModelConfig{{Name: "m1", Provider: "p"}},
		DefaultModel: "m1",
		Repository:   repo,
	})
}

// TestChatModel_CtrlS_OpensSkillsBrowser_SelectingShowsRealBody drives the
// real ctrl+s keybinding path end to end: opens the overlay, selects the
// only loaded skill, and asserts the skill's actual Body (not a
// placeholder) lands in the transcript as a system-style entry.
func TestChatModel_CtrlS_OpensSkillsBrowser_SelectingShowsRealBody(t *testing.T) {
	svc := newSkillsTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS}, svc, ctx)
	assert.Nil(t, cmd, "opening the skills browser is a synchronous, in-memory ListSkills() read — no Cmd needed")
	require.NotNil(t, c.picker, "ctrl+s must open the in-chat skills-browser overlay")
	assert.Equal(t, pickerSkill, c.pickerKind)

	selectCmd := c.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, svc, ctx)
	assert.Nil(t, selectCmd, "selecting a skill is also synchronous — GetSkillBody is a pure in-memory lookup")
	assert.Nil(t, c.picker, "selecting an entry must close the overlay")

	require.Len(t, c.transcript, 1, "the skill's body must be folded into the transcript")
	assert.True(t, c.transcript[0].system, "the skill-detail entry must be marked system, not a real user/assistant message")
	assert.Contains(t, c.transcript[0].text, "Distinctive real skill body content, marker XYZZY", "the real Body must be shown, not a hallucination or placeholder")

	view := c.View(80, 24)
	assert.Contains(t, view, "XYZZY", "the shown skill content must actually be visible in the rendered view")
}

// TestChatModel_CtrlS_NoSkillsConfigured_ShowsSensibleMessage asserts ctrl+s
// is non-broken (not a silent no-op with no feedback) when zero skills are
// loaded.
func TestChatModel_CtrlS_NoSkillsConfigured_ShowsSensibleMessage(t *testing.T) {
	svc := newSwitchTestService(t) // no skills configured
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS}, svc, ctx)
	assert.Nil(t, cmd)
	assert.Nil(t, c.picker, "no overlay should open when there are no skills to browse")
	require.Len(t, c.transcript, 1, "ctrl+s with no skills must still give visible feedback, not a silent no-op")
	assert.True(t, c.transcript[0].system)
	assert.Contains(t, c.transcript[0].text, "No skills configured")
}

// TestChatModel_CtrlS_EscCancelsWithNoChange proves "esc" closes the skills
// overlay without adding anything to the transcript, mirroring ctrl+a/
// ctrl+o's own cancel behavior.
func TestChatModel_CtrlS_EscCancelsWithNoChange(t *testing.T) {
	svc := newSkillsTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS}, svc, ctx)
	require.NotNil(t, c.picker)

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyEsc}, svc, ctx)
	assert.Nil(t, cmd)
	assert.Nil(t, c.picker, "esc must close the skills overlay")
	assert.Equal(t, pickerNone, c.pickerKind)
	assert.Empty(t, c.transcript, "cancelling the skills browser must not add anything to the transcript")
}

// TestChatModel_CtrlS_NoopDuringActiveStream asserts ctrl+s does not open
// the skills browser mid-turn, the same guard ctrl+a/ctrl+o apply.
func TestChatModel_CtrlS_NoopDuringActiveStream(t *testing.T) {
	svc := newSkillsTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.streamActive = true

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS}, svc, ctx)
	assert.Nil(t, cmd)
	assert.Nil(t, c.picker, "ctrl+s must not open the skills browser while a stream is active")
}

// TestChatModel_CtrlT_OpensToolsBrowser_SelectingShowsDetail drives the real
// ctrl+t keybinding path end to end: opens the overlay (pickerTool),
// selects the alphabetically-first built-in tool ("Bash" — ListTools sorts
// by name, and newSwitchTestService's "assistant" agent sets no tools:
// allowlist, so every built-in tool is available), and asserts the tool's
// name and source land in the transcript as a system-style entry.
func TestChatModel_CtrlT_OpensToolsBrowser_SelectingShowsDetail(t *testing.T) {
	svc := newSwitchTestService(t) // no MCP tools configured
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlT}, svc, ctx)
	assert.Nil(t, cmd, "opening the tools browser is a synchronous, in-memory ListTools() read — no Cmd needed")
	require.NotNil(t, c.picker, "ctrl+t must open the in-chat tools-browser overlay")
	assert.Equal(t, pickerTool, c.pickerKind)

	selectCmd := c.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, svc, ctx)
	assert.Nil(t, selectCmd, "selecting a tool is also synchronous — no AgentService mutator involved")
	assert.Nil(t, c.picker, "selecting an entry must close the overlay")

	require.Len(t, c.transcript, 1, "the selected tool's detail must be folded into the transcript")
	assert.True(t, c.transcript[0].system, "the tool-detail entry must be marked system, not a real user/assistant message")
	assert.Contains(t, c.transcript[0].text, "Tool: Bash (Built-in)")

	view := c.View(80, 24)
	assert.Contains(t, view, "Bash", "the shown tool content must actually be visible in the rendered view")
}

// TestChatModel_CtrlT_EscCancelsWithNoChange proves "esc" closes the tools
// overlay without adding anything to the transcript, mirroring ctrl+s's own
// cancel behavior.
func TestChatModel_CtrlT_EscCancelsWithNoChange(t *testing.T) {
	svc := newSwitchTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlT}, svc, ctx)
	require.NotNil(t, c.picker)

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyEsc}, svc, ctx)
	assert.Nil(t, cmd)
	assert.Nil(t, c.picker, "esc must close the tools overlay")
	assert.Equal(t, pickerNone, c.pickerKind)
	assert.Empty(t, c.transcript, "cancelling the tools browser must not add anything to the transcript")
}

// TestChatModel_CtrlT_NoopDuringActiveStream asserts ctrl+t does not open
// the tools browser mid-turn, the same guard ctrl+a/ctrl+o/ctrl+s apply.
func TestChatModel_CtrlT_NoopDuringActiveStream(t *testing.T) {
	svc := newSwitchTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.streamActive = true

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlT}, svc, ctx)
	assert.Nil(t, cmd)
	assert.Nil(t, c.picker, "ctrl+t must not open the tools browser while a stream is active")
}

// TestChatModel_CtrlU_ShowsFullUsageBreakdown drives the real ctrl+u
// keybinding path: it folds renderUsageLines' full per-model breakdown
// into the transcript, the detail the sidebar's own compact "Cost: ..."
// line (usageCostLine) deliberately leaves out.
func TestChatModel_CtrlU_ShowsFullUsageBreakdown(t *testing.T) {
	svc := newSwitchTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.usage = map[string]entities.ModelUsage{
		"m1":     {InputTokens: 1000, OutputTokens: 200, CostUSD: 0.0123},
		"sub-m1": {InputTokens: 50, OutputTokens: 25, CostUSD: 0.0004},
	}

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlU}, svc, ctx)
	assert.Nil(t, cmd, "showUsage is a synchronous, in-memory read of c.usage — no Cmd needed")

	require.Len(t, c.transcript, 1)
	assert.True(t, c.transcript[0].system)
	assert.Contains(t, c.transcript[0].text, "sub-m1")
	assert.Contains(t, c.transcript[0].text, "total")
}

// TestChatModel_CtrlU_NoopDuringActiveStream asserts ctrl+u does not show
// the usage breakdown mid-turn, the same guard the other overlays apply.
func TestChatModel_CtrlU_NoopDuringActiveStream(t *testing.T) {
	svc := newSwitchTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.streamActive = true

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlU}, svc, ctx)
	assert.Nil(t, cmd)
	assert.Empty(t, c.transcript, "ctrl+u must not show the usage breakdown while a stream is active")
}

// ---------------------------------------------------------------------
// ctrl+h history browser (post-v0.1.0 addendum, Design §5's addendum)
// ---------------------------------------------------------------------

// TestModel_CtrlH_TopLevel_LoadsHistoryAndResumesSelectedChat drives the
// real ctrl+h path from the top-level agent-picker screen: loadHistoryCmd's
// real ListChatSummaries call, historyLoadedMsg opening screenHistory, and
// selecting the one entry resuming it with its prior transcript restored.
func TestModel_CtrlH_TopLevel_LoadsHistoryAndResumesSelectedChat(t *testing.T) {
	svc, closeServer := newLeakTestService(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("4"))
	})
	t.Cleanup(closeServer)

	ctx := context.Background()
	_, err := svc.StartChat(ctx, "old-chat", "assistant")
	require.NoError(t, err)
	_, err = svc.RunText(ctx, "old-chat", "what is 2+2?")
	require.NoError(t, err)

	m := NewModel(ctx, svc, map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}}, "", "")

	next, loadCmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlH})
	m = next.(Model)
	require.NotNil(t, loadCmd)
	loadedMsg := loadCmd()
	require.IsType(t, historyLoadedMsg{}, loadedMsg)

	next2, _ := m.Update(loadedMsg)
	m = next2.(Model)
	require.Equal(t, screenHistory, m.screen)
	require.Len(t, m.historyList.Items(), 1)

	next3, resumeCmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = next3.(Model)
	require.NotNil(t, resumeCmd)
	resumeMsg := resumeCmd()
	started, ok := resumeMsg.(chatStartedMsg)
	require.True(t, ok)
	require.NoError(t, started.err)
	assert.Equal(t, "old-chat", started.chatID)

	next4, _ := m.Update(resumeMsg)
	m = next4.(Model)
	assert.Equal(t, screenChat, m.screen)
	require.NotNil(t, m.chat)
	assert.NotEmpty(t, m.chat.transcript, "resuming must reconstruct the prior transcript")
	assert.True(t, m.chat.titleAttempted, "a resumed chat must never (re)trigger title generation")
}

// TestChatModel_CtrlH_OpensOverlay_SelectingResumesToDifferentChat is the
// in-chat counterpart: ctrl+h while already chatting opens the overlay
// (chatModel.picker/pickerHistory) rather than a top-level screen, and
// selecting a *different* chat than the one currently open replaces
// Model.chat entirely (via the same chatStartedMsg path a top-level pick
// uses).
func TestChatModel_CtrlH_OpensOverlay_SelectingResumesToDifferentChat(t *testing.T) {
	svc, closeServer := newLeakTestService(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("ok"))
	})
	t.Cleanup(closeServer)

	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-a", "assistant")
	require.NoError(t, err)
	_, err = svc.RunText(ctx, "chat-a", "hello from chat a")
	require.NoError(t, err)
	_, err = svc.StartChat(ctx, "chat-b", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-b", "assistant", nil, "m1", 80, 24)

	loadCmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlH}, svc, ctx)
	require.NotNil(t, loadCmd)
	loadedMsg := loadCmd()
	require.IsType(t, historyLoadedMsg{}, loadedMsg)

	m := Model{svc: svc, ctx: ctx, screen: screenChat, chat: c}
	next, _ := m.Update(loadedMsg)
	m = next.(Model)
	require.NotNil(t, m.chat.picker)
	require.Equal(t, pickerHistory, m.chat.pickerKind)
	// Just chat-a: the in-chat overlay excludes the chat it was opened
	// from (post-v0.1.0 addendum: "d" to delete would otherwise be able
	// to delete the chat it's showing out from under its own session —
	// see historyLoadedMsg's doc comment), so chat-b never appears here.
	require.Len(t, m.chat.picker.Items(), 1)

	resumeCmd := m.chat.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, svc, ctx)
	require.NotNil(t, resumeCmd)
	resumeMsg := resumeCmd()
	started, ok := resumeMsg.(chatStartedMsg)
	require.True(t, ok)
	require.NoError(t, started.err)

	final, _ := m.Update(resumeMsg)
	m2 := final.(Model)
	require.NotNil(t, m2.chat)
	assert.NotEqual(t, "chat-b", m2.chat.chatID, "must have switched to the other chat")
}

// TestChatModel_PickerHistoryDelete_RequiresConfirmation drives "d" in the
// in-chat ctrl+h overlay end-to-end (post-v0.1.0 addendum): the first "d"
// on an item only arms confirmDelete's prompt — the item must still be
// there, and the list's header must show the confirm text — and only a
// second "d" on that same item actually deletes it via the real
// AgentService.DeleteChat, confirmed by feeding the resulting
// chatDeletedMsg back through Model.Update and checking the item is gone
// both from the list and from storage.
func TestChatModel_PickerHistoryDelete_RequiresConfirmation(t *testing.T) {
	svc, closeServer := newLeakTestService(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("ok"))
	})
	t.Cleanup(closeServer)

	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-current", "assistant")
	require.NoError(t, err)
	_, err = svc.StartChat(ctx, "chat-x", "assistant")
	require.NoError(t, err)
	_, err = svc.StartChat(ctx, "chat-y", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-current", "assistant", nil, "m1", 80, 24)
	loadCmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlH}, svc, ctx)
	require.NotNil(t, loadCmd)
	loadedMsg := loadCmd()

	m := Model{svc: svc, ctx: ctx, screen: screenChat, chat: c}
	next, _ := m.Update(loadedMsg)
	m = next.(Model)
	require.NotNil(t, m.chat.picker)
	require.Len(t, m.chat.picker.Items(), 2, "the current chat must be excluded, leaving chat-x and chat-y")

	// Select whichever item is chat-x — recency-sorted, not assumed to
	// land at a fixed index.
	idx := -1
	for i, it := range m.chat.picker.Items() {
		if it.(historyPickerItem).chatID == "chat-x" {
			idx = i
		}
	}
	require.GreaterOrEqual(t, idx, 0)
	m.chat.picker.Select(idx)

	cmd := m.chat.handlePickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}}, svc, ctx)
	assert.Nil(t, cmd, "the first d must only arm the confirmation, not return a delete command")
	assert.Len(t, m.chat.picker.Items(), 2, "arming must not delete anything yet")
	assert.Equal(t, historyDeleteConfirmTitle, m.chat.picker.Title)

	cmd = m.chat.handlePickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}}, svc, ctx)
	require.NotNil(t, cmd, "a second d on the same item must confirm and return the delete command")
	assert.Equal(t, historyListTitle, m.chat.picker.Title, "confirming must restore the normal header")

	deletedMsg := cmd()
	deleted, ok := deletedMsg.(chatDeletedMsg)
	require.True(t, ok)
	require.NoError(t, deleted.err)
	assert.Equal(t, "chat-x", deleted.chatID)

	next2, _ := m.Update(deletedMsg)
	m = next2.(Model)
	require.Len(t, m.chat.picker.Items(), 1, "a confirmed delete must remove just that one item")
	remaining := m.chat.picker.Items()[0].(historyPickerItem)
	assert.Equal(t, "chat-y", remaining.chatID)

	_, err = svc.GetChat(ctx, "chat-x")
	assert.Error(t, err, "the chat must actually be gone from storage too, not just the list")
}

// TestModel_CtrlH_TopLevel_DeleteRequiresConfirmation is
// TestChatModel_PickerHistoryDelete_RequiresConfirmation's counterpart for
// the top-level screenHistory screen (reachable before any chat has been
// started, so there's no "current chat" to exclude the way the in-chat
// overlay does).
func TestModel_CtrlH_TopLevel_DeleteRequiresConfirmation(t *testing.T) {
	svc, closeServer := newLeakTestService(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("ok"))
	})
	t.Cleanup(closeServer)

	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	m := NewModel(ctx, svc, map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}}, "", "")

	next, loadCmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlH})
	m = next.(Model)
	require.NotNil(t, loadCmd)
	loadedMsg := loadCmd()
	next2, _ := m.Update(loadedMsg)
	m = next2.(Model)
	require.Equal(t, screenHistory, m.screen)
	require.Len(t, m.historyList.Items(), 1)

	next3, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = next3.(Model)
	assert.Nil(t, cmd, "the first d must only arm the confirmation")
	assert.Len(t, m.historyList.Items(), 1)
	assert.Equal(t, historyDeleteConfirmTitle, m.historyList.Title)

	next4, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = next4.(Model)
	require.NotNil(t, cmd, "a second d on the same item must confirm and return the delete command")

	deletedMsg := cmd()
	next5, _ := m.Update(deletedMsg)
	m = next5.(Model)
	assert.Empty(t, m.historyList.Items(), "the deleted chat must be gone from the list")

	_, err = svc.GetChat(ctx, "chat-1")
	assert.Error(t, err, "the chat must actually be gone from storage too")
}

// TestModel_CtrlH_TopLevel_MovingSelectionCancelsPendingDelete covers the
// other half of the safety property: any key other than a repeated "d"
// drops an armed confirmation, so navigating away and back can't leave a
// stale prompt for an unrelated later "d" to confirm.
func TestModel_CtrlH_TopLevel_MovingSelectionCancelsPendingDelete(t *testing.T) {
	svc, closeServer := newLeakTestService(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("ok"))
	})
	t.Cleanup(closeServer)

	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)
	_, err = svc.StartChat(ctx, "chat-2", "assistant")
	require.NoError(t, err)

	m := NewModel(ctx, svc, map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}}, "", "")
	next, loadCmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlH})
	m = next.(Model)
	loadedMsg := loadCmd()
	next2, _ := m.Update(loadedMsg)
	m = next2.(Model)
	require.Len(t, m.historyList.Items(), 2)

	next3, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = next3.(Model)
	require.Nil(t, cmd)
	require.Equal(t, historyDeleteConfirmTitle, m.historyList.Title)

	next4, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	m = next4.(Model)
	assert.Nil(t, cmd)
	assert.Equal(t, historyListTitle, m.historyList.Title, "moving the selection must cancel the armed prompt")

	// A "d" right after that must arm again, not instantly delete.
	next5, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = next5.(Model)
	assert.Nil(t, cmd)
	assert.Len(t, m.historyList.Items(), 2, "the cancelled-then-rearmed prompt must not have deleted anything")
}

// TestModel_FirstExchangeCompletes_TriggersTitleGeneration is the direct
// test of Model.Update's streamChunkMsg/streamDoneMsg/streamErrMsg case
// (post-v0.1.0 addendum): a streamDoneMsg completing a chat's first
// exchange (transcript going from length 1 to 2) must batch in
// generateTitleCmd, and running that Cmd must actually persist a title
// generated from the chat's real messages.
func TestModel_FirstExchangeCompletes_TriggersTitleGeneration(t *testing.T) {
	titleServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("Arithmetic Question"))
	}))
	t.Cleanup(titleServer.Close)
	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderType("openai-compatible"), APIKey: "sk-test", BaseURL: titleServer.URL + "/v1"}
	model := entities.ModelConfig{Name: "m1", Provider: provider.Name, ModelName: "gpt-test"}

	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := services.NewAgentService(services.AgentServiceConfig{
		Definitions: services.Definitions{
			Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}},
		},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Repository:   repo,
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	// Seed the persisted chat's Messages directly — GenerateChatTitle reads
	// from the repository, not from chatModel.transcript, so this stands in
	// for "a real turn already completed and was persisted" without needing
	// to drive an actual streaming turn through this test.
	chat, err := repo.Get(ctx, "chat-1")
	require.NoError(t, err)
	chat.Messages = []*message.Message{
		{Role: message.RoleUser, Contents: message.Contents{&message.TextContent{Text: "what is 6*7?"}}},
		{Role: message.RoleAssistant, Contents: message.Contents{&message.TextContent{Text: "42"}}},
	}
	require.NoError(t, repo.Update(ctx, chat))

	c := newChatModel("chat-1", "assistant", nil, model.Name, 80, 24)
	c.transcript = []transcriptEntry{{role: message.RoleUser, text: "what is 6*7?"}}
	require.False(t, c.titleAttempted)

	m := Model{svc: svc, ctx: ctx, screen: screenChat, chat: c}
	next, cmd := m.Update(streamDoneMsg{result: &services.RunResult{Response: &agent.Response{}}})
	m = next.(Model)
	require.True(t, m.chat.titleAttempted, "titleAttempted must flip the moment the first exchange is detected, not after generation completes")
	require.NotNil(t, cmd)

	// cmd is tea.Batch(handleStreamMsg's own nil cmd, generateTitleCmd(...)):
	// with one side nil, tea.Batch collapses to the other Cmd directly
	// rather than a genuine tea.BatchMsg (compactCmds' own documented
	// behavior) — handle both shapes so this doesn't depend on that
	// implementation detail.
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, sub := range batch {
			if sub != nil {
				sub()
			}
		}
	}
	// Otherwise cmd() already ran generateTitleCmd's own body directly.

	got, err := repo.Get(ctx, "chat-1")
	require.NoError(t, err)
	assert.Equal(t, "Arithmetic Question", got.Title)
}

// TestModel_SecondExchange_DoesNotRetriggerTitleGeneration asserts
// titleAttempted actually guards against firing on every turn, not just the
// first — a second streamDoneMsg on the same chatModel must not re-batch
// generateTitleCmd.
func TestModel_SecondExchange_DoesNotRetriggerTitleGeneration(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.titleAttempted = true
	c.transcript = []transcriptEntry{
		{role: message.RoleUser, text: "first"},
		{role: message.RoleAssistant, text: "reply"},
		{role: message.RoleUser, text: "second"},
	}

	m := Model{screen: screenChat, chat: c}
	next, cmd := m.Update(streamDoneMsg{result: &services.RunResult{Response: &agent.Response{}}})
	m = next.(Model)
	if cmd != nil {
		msg := cmd()
		_, isBatch := msg.(tea.BatchMsg)
		assert.False(t, isBatch, "a second turn's streamDoneMsg must not batch in generateTitleCmd again")
	}
}

// ---------------------------------------------------------------------
// list.Model filtering (post-v0.1.0 addendum: a long-standing bug, present
// since ctrl+a/ctrl+o's overlay pickers were first introduced, not
// something newly broken). bubbles/list.Model's own filtering is
// asynchronous: typing into an open filter updates FilterInput
// immediately, but list.Model.Update returns a Cmd (producing
// list.FilterMatchesMsg) that has to be run and fed back into that *same*
// list.Model before VisibleItems() reflects the query at all. Model.Update
// is an exhaustive, explicit type switch with no default case routing
// unrecognized messages anywhere, so FilterMatchesMsg was silently
// dropped — every item stayed visible no matter what was typed, reproducing
// the reported symptom exactly (filtering "openai" in the ctrl+o picker did
// nothing).
// ---------------------------------------------------------------------

// driveKey simulates what the real bubbletea runtime does for one keypress:
// dispatch it through Model.Update, then run the returned Cmd. The real
// runtime specifically unpacks a tea.BatchMsg into its constituent Cmds and
// feeds each resulting Msg back through Update individually — list.Model
// batches its cursor-blink Cmd together with filterItems' Cmd, so without
// unpacking, this test would never see the real list.FilterMatchesMsg at
// all (mirrors drainCmd's own tea.BatchMsg handling, model_test.go). Only
// one level deep, not a loop until cmd is nil: textinput's cursor-blink Cmd
// re-arms itself forever via tea.Tick while focused, so looping would hang;
// one level is enough for FilterMatchesMsg to make it back to the
// list.Model, which is all these tests need.
func driveKey(m Model, msg tea.Msg) Model {
	next, cmd := m.Update(msg)
	m = next.(Model)
	if cmd == nil {
		return m
	}
	resultMsg := cmd()
	if batch, ok := resultMsg.(tea.BatchMsg); ok {
		for _, sub := range batch {
			if sub == nil {
				continue
			}
			subMsg := sub()
			if subMsg == nil {
				continue
			}
			next, _ = m.Update(subMsg)
			m = next.(Model)
		}
		return m
	}
	if resultMsg == nil {
		return m
	}
	next, _ = m.Update(resultMsg)
	return next.(Model)
}

func typeIntoFilter(t *testing.T, m Model, query string) Model {
	t.Helper()
	m = driveKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	for _, r := range query {
		m = driveKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

// TestChatModel_CtrlO_FilterActuallyNarrowsVisibleItems is the exact
// reported bug: opening ctrl+o's model picker, pressing "/", and typing
// "openai" must narrow VisibleItems() to matches, not leave every model
// visible.
func TestChatModel_CtrlO_FilterActuallyNarrowsVisibleItems(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	svc := services.NewAgentService(services.AgentServiceConfig{
		Definitions: services.Definitions{Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}}},
		Providers: []entities.ProviderConfig{
			{Name: "openai", Type: entities.ProviderTypeOpenAI},
			{Name: "groq", Type: entities.ProviderTypeGroq},
		},
		Models: []entities.ModelConfig{
			{Name: "openai", Provider: "openai", ModelName: "gpt-5.6"},
			{Name: "groq/qwen", Provider: "groq", ModelName: "qwen"},
			{Name: "groq/llama", Provider: "groq", ModelName: "llama"},
		},
		DefaultModel: "openai",
		Repository:   repo,
	})
	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "openai", 80, 24)
	c.openPicker(svc, pickerModel)
	require.Len(t, c.picker.Items(), 3)

	m := typeIntoFilter(t, Model{svc: svc, ctx: ctx, screen: screenChat, chat: c}, "openai")

	require.Equal(t, list.Filtering, m.chat.picker.FilterState())
	visible := m.chat.picker.VisibleItems()
	require.Len(t, visible, 1, "filtering \"openai\" must narrow the list down, not leave every model visible")
	assert.Equal(t, "openai", visible[0].(modelPickerItem).name)
}

// TestModel_TopLevelAgentPicker_FilterActuallyNarrowsVisibleItems proves
// the fix isn't scoped to just the chat-overlay picker — the same
// FilterMatchesMsg-dropping bug affected the top-level agentList too, since
// Model.Update's exhaustiveness was the root cause, not anything specific
// to chatModel's overlay.
func TestModel_TopLevelAgentPicker_FilterActuallyNarrowsVisibleItems(t *testing.T) {
	m := NewModel(context.Background(), nil, map[string]agentsource.AgentDefinition{
		"assistant": {Name: "assistant"},
		"reviewer":  {Name: "reviewer"},
		"writer":    {Name: "writer"},
	}, "", "")
	require.Len(t, m.agentList.Items(), 3)

	m = typeIntoFilter(t, m, "review")

	require.Equal(t, list.Filtering, m.agentList.FilterState())
	visible := m.agentList.VisibleItems()
	require.Len(t, visible, 1)
	assert.Equal(t, "reviewer", visible[0].(agentItem).def.Name)
}

// TestChatModel_CtrlB_TogglesSidebarVisibility is this package's coverage
// for the post-v0.1.0 sidebar-hide addendum: ctrl+b hides the sidebar
// entirely (so a terminal's own line-based copy/paste selection has nothing
// but transcript columns to grab) and the transcript/composer reclaim the
// full width; a second ctrl+b restores it.
func TestChatModel_CtrlB_TogglesSidebarVisibility(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	assert.Contains(t, c.View(80, 24), "Agent: assistant", "the sidebar renders by default")
	assert.False(t, c.sidebarHidden)

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlB}, nil, nil)
	assert.Nil(t, cmd)
	assert.True(t, c.sidebarHidden)
	hiddenView := c.View(80, 24)
	assert.NotContains(t, hiddenView, "Agent: assistant", "ctrl+b must hide the sidebar")
	assert.Equal(t, c.width, c.viewport.Width, "the transcript must reclaim the full width once the sidebar is hidden")

	cmd = c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlB}, nil, nil)
	assert.Nil(t, cmd)
	assert.False(t, c.sidebarHidden)
	assert.Contains(t, c.View(80, 24), "Agent: assistant", "a second ctrl+b must show the sidebar again")
}

// TestChatModel_PageUpPageDown_ScrollsTranscript is this package's coverage
// for the post-v0.1.0 scrolling fix: before this, pgup/pgdown fell through
// handleKey's default case straight into the composer, which silently
// ignored them — the transcript viewport never moved, so a long
// conversation's earlier messages were permanently unreachable once they
// scrolled past the bottom-anchored view. pgup/pgdown are now handled
// directly against c.viewport, bypassing the composer entirely.
func TestChatModel_PageUpPageDown_ScrollsTranscript(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 40, 10)
	for i := 0; i < 30; i++ {
		c.transcript = append(c.transcript, transcriptEntry{role: message.RoleAssistant, text: fmt.Sprintf("line %d", i)})
	}
	c.refreshViewport()
	require.True(t, c.viewport.AtBottom(), "refreshViewport must leave the transcript scrolled to the bottom")

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyPgUp}, nil, nil)
	assert.Nil(t, cmd)
	assert.False(t, c.viewport.AtBottom(), "pgup must scroll the transcript away from the bottom, not be silently swallowed")

	cmd = c.handleKey(tea.KeyMsg{Type: tea.KeyPgDown}, nil, nil)
	assert.Nil(t, cmd)
	assert.True(t, c.viewport.AtBottom(), "pgdown must scroll back down to the bottom of this short transcript")
}

// TestChatModel_CtrlUpCtrlDown_AlsoScrollsTranscript is this package's
// coverage for the post-v0.1.0 addendum adding ctrl+up/ctrl+down as a
// second binding for the same scroll action pgup/pgdown already perform —
// most Apple keyboards have no physical PgUp/PgDn keys at all.
func TestChatModel_CtrlUpCtrlDown_AlsoScrollsTranscript(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 40, 10)
	for i := 0; i < 30; i++ {
		c.transcript = append(c.transcript, transcriptEntry{role: message.RoleAssistant, text: fmt.Sprintf("line %d", i)})
	}
	c.refreshViewport()
	require.True(t, c.viewport.AtBottom())

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlUp}, nil, nil)
	assert.Nil(t, cmd)
	assert.False(t, c.viewport.AtBottom(), "ctrl+up must scroll the transcript away from the bottom")

	cmd = c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlDown}, nil, nil)
	assert.Nil(t, cmd)
	assert.True(t, c.viewport.AtBottom(), "ctrl+down must scroll back down to the bottom of this short transcript")
}

// TestChatModel_CtrlJ_InsertsNewlineThenEnterSubmitsWholeMessage is this
// package's coverage for the post-v0.1.0 multi-line-composer addendum:
// ctrl+j inserts a literal newline into the composer without submitting
// (unlike a plain "enter", handleKey's own switch never claims "ctrl+j", so
// it falls through to the composer's own textarea.KeyMap, rebound to treat
// ctrl+j/shift+enter as InsertNewline instead of "enter"), and the entire
// multi-line value submits as a single transcript entry once the user
// does press plain "enter".
func TestChatModel_CtrlJ_InsertsNewlineThenEnterSubmitsWholeMessage(t *testing.T) {
	svc, closeServer := newLeakTestService(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("ok"))
	})
	t.Cleanup(closeServer)

	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	for _, r := range "first line" {
		c.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}, svc, ctx)
	}
	c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlJ}, svc, ctx)
	assert.Empty(t, c.transcript, "ctrl+j must not submit the turn")
	for _, r := range "second line" {
		c.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}, svc, ctx)
	}
	require.Equal(t, "first line\nsecond line", c.composer.Value(), "ctrl+j must insert a literal newline rather than submitting")
	require.Empty(t, c.transcript, "nothing should be sent to the model until a plain enter")

	submitCmd := c.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, svc, ctx)
	require.NotNil(t, submitCmd)
	require.Len(t, c.transcript, 1)
	assert.Equal(t, "first line\nsecond line", c.transcript[0].text, "the whole multi-line message must submit as one entry, newline included")
	assert.Empty(t, c.composer.Value(), "the composer must be cleared after submitting")

	drainCmd(t, c, svc, ctx, submitCmd)
}

// TestChatModel_View_NoRoleLabels is this package's coverage for the
// post-v0.1.0 addendum removing the "you"/"assistant" role labels from the
// rendered transcript — renderEntry now renders a user message as a
// background-highlighted block and an assistant reply as plain text, with
// no label line preceding either.
func TestChatModel_View_NoRoleLabels(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.transcript = append(c.transcript,
		transcriptEntry{role: message.RoleUser, text: "hello there"},
		transcriptEntry{role: message.RoleAssistant, text: "hi, how can I help"},
	)
	c.refreshViewport()

	content := c.viewport.View()
	assert.Contains(t, content, "hello there")
	assert.Contains(t, content, "hi, how can I help")
	assert.NotContains(t, content, "you", "the old 'you' role label must no longer render")
	assert.NotContains(t, content, "assistant", "the old 'assistant' role label must no longer render")
}

// TestChatModel_View_FillsFullTerminalHeight is this package's coverage
// for the post-v0.1.0 addendum removing the layout budget's unjustified
// slack: View's total rendered height (sidebar and main column alike)
// must equal exactly the requested terminal height, with no rows left
// permanently blank at the bottom — whether or not a status error line is
// currently being shown below the composer (viewportHeight borrows a row
// for it dynamically rather than always reserving one).
func TestChatModel_View_FillsFullTerminalHeight(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	lines := strings.Split(c.View(80, 24), "\n")
	assert.Len(t, lines, 24, "the idle view must use every row of the requested terminal height")

	c.statusErr = fmt.Errorf("boom")
	c.refreshViewport()
	lines = strings.Split(c.View(80, 24), "\n")
	assert.Len(t, lines, 24, "the view with a status error line showing must still use exactly the requested terminal height")
}

// TestToolCallHeadline_KnownTools_ShowArguments is this package's coverage
// for the post-v0.1.0 addendum replacing the bare "[calling toolname...]"
// marker with an argument-aware headline — Claude Code's own
// "Bash(npm test)"/"Write(path/to/file)" style — so a run of several calls
// to the same tool is actually distinguishable.
func TestToolCallHeadline_KnownTools_ShowArguments(t *testing.T) {
	cases := []struct {
		name, argsJSON, want string
	}{
		{"run_shell", `{"command":"npm test"}`, "run_shell(npm test)"},
		{"file_write", `{"path":"README.md","content":"ignored here"}`, "file_write(README.md)"},
		{"file_read", `{"path":"go.mod"}`, "file_read(go.mod)"},
		{"directory_list", `{"path":"internal/tui"}`, "directory_list(internal/tui)"},
		{"directory_list", `{}`, "directory_list(.)"},
		{"web_fetch", `{"url":"https://example.com"}`, "web_fetch(https://example.com)"},
		{"web_search", `{"query":"golang diff libraries"}`, `web_search("golang diff libraries")`},
		{"Skill", `{"name":"mcp-server-setup"}`, "Skill(mcp-server-setup)"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, toolCallHeadline(c.name, c.argsJSON), "name=%s args=%s", c.name, c.argsJSON)
	}
}

// TestToolCallHeadline_UnknownTool_FallsBackToArgsPreview covers an
// MCP-provided tool (or any name this package doesn't special-case): it
// still shows the raw arguments rather than nothing beyond the bare name.
func TestToolCallHeadline_UnknownTool_FallsBackToArgsPreview(t *testing.T) {
	got := toolCallHeadline("echo", `{"message":"hi"}`)
	assert.Equal(t, `echo({"message":"hi"})`, got)

	assert.Equal(t, "echo", toolCallHeadline("echo", `{}`), "empty arguments must not render as a pointless 'echo({})'")
	assert.Equal(t, "echo", toolCallHeadline("echo", ""), "unparseable/empty arguments must fall back to the bare name, not panic")
}

// TestChatModel_ApplyUpdate_RunShellCallAndResult_BothRender is this
// package's coverage for the full call-then-result flow through
// applyUpdate: the call surfaces the actual command (not just "run_shell"
// repeated with no way to tell commands apart), and the result — arriving
// as a separate, later update, correlated by CallID via pendingToolCalls
// — follows up with the tool's real output.
func TestChatModel_ApplyUpdate_RunShellCallAndResult_BothRender(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	c.applyUpdate(&agent.ResponseUpdate{Contents: message.Contents{
		&message.FunctionCallContent{CallID: "call-1", Name: "run_shell", Arguments: `{"command":"echo hi"}`},
	}})
	assert.Contains(t, streamingText(c), "run_shell(echo hi)")
	assert.Len(t, c.pendingToolCalls, 1, "the call must be recorded, awaiting its result")

	c.applyUpdate(&agent.ResponseUpdate{Contents: message.Contents{
		&message.FunctionResultContent{CallID: "call-1", Result: "hi\nexit_code: 0"},
	}})
	assert.Contains(t, streamingText(c), "hi")
	assert.Contains(t, streamingText(c), "exit_code: 0")
	assert.Empty(t, c.pendingToolCalls, "the result must consume its pending entry")
}

// TestChatModel_ApplyUpdate_FileWriteResult_ShowsLineDiffStat is this
// package's coverage for the part of this addendum the user actually
// asked for: a file_write result renders a git-diff-style "+N -M lines"
// stat (tools.FileWriteOutput.LinesAdded/LinesRemoved), not just a bare
// "wrote a file" confirmation.
func TestChatModel_ApplyUpdate_FileWriteResult_ShowsLineDiffStat(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	c.applyUpdate(&agent.ResponseUpdate{Contents: message.Contents{
		&message.FunctionCallContent{CallID: "call-1", Name: "file_write", Arguments: `{"path":"README.md","content":"..."}`},
	}})
	c.applyUpdate(&agent.ResponseUpdate{Contents: message.Contents{
		&message.FunctionResultContent{CallID: "call-1", Result: tools.FileWriteOutput{
			Path: "README.md", BytesWritten: 42, LinesAdded: 8, LinesRemoved: 3,
		}},
	}})

	out := streamingText(c)
	assert.Contains(t, out, "README.md")
	assert.Contains(t, out, "+8")
	assert.Contains(t, out, "-3")
}

// TestChatModel_ApplyUpdate_ToolError_AlwaysRendersRegardlessOfTool is this
// package's coverage that a failed call is never silently swallowed, even
// for a tool with no other special-cased result rendering.
func TestChatModel_ApplyUpdate_ToolError_AlwaysRendersRegardlessOfTool(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	c.applyUpdate(&agent.ResponseUpdate{Contents: message.Contents{
		&message.FunctionCallContent{CallID: "call-1", Name: "file_read", Arguments: `{"path":"missing.txt"}`},
	}})
	c.applyUpdate(&agent.ResponseUpdate{Contents: message.Contents{
		&message.FunctionResultContent{CallID: "call-1", Error: fmt.Errorf("no such file or directory")},
	}})

	require.Len(t, c.streaming, 2)
	assert.Equal(t, segmentToolError, c.streaming[1].kind, "a failed call must produce a segmentToolError, regardless of which tool it was")
	assert.Contains(t, c.streaming[1].text, "no such file or directory")

	// The "✗" marker itself is added at render time (indentedLines), not
	// stored in the raw segment text — check it actually reaches the
	// rendered transcript too, not just the right segment kind.
	var rendered strings.Builder
	for _, l := range segmentLines(c.streaming[1], 80) {
		rendered.WriteString(l.style.Render(l.text))
	}
	assert.Contains(t, rendered.String(), "✗")
}

// TestChatModel_MouseDrag_SelectsAndCopiesText is this package's coverage
// for mouse-driven text selection (post-v0.1.0 addendum): a left-button
// press starts a selection, motion extends it, and release commits it and
// copies the selected text via the injectable copyToClipboard hook —
// exactly the OSC 52 + toast behavior Claude Code's own mouse-release
// callout performs, verified here without touching a real terminal.
func TestChatModel_MouseDrag_SelectsAndCopiesText(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 40, 10)
	c.sidebarHidden = true
	for i := 0; i < 5; i++ {
		c.transcript = append(c.transcript, transcriptEntry{role: message.RoleUser, text: fmt.Sprintf("line %d", i)})
	}
	c.refreshViewport()
	c.View(40, 10) // measures mainColOffset (0, sidebar hidden)

	var copied string
	c.copyToClipboard = func(s string) { copied = s }

	// buildContentLines interleaves a blank separator line after every
	// transcript entry, and refreshViewport's GotoBottom has scrolled past
	// the first entry/separator pair here — Y:1 is the first row that
	// actually lands on text ("line 2") rather than a blank separator.
	cmd := c.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 0, Y: 1})
	assert.Nil(t, cmd)
	assert.True(t, c.selecting)
	assert.False(t, c.hasSel, "a press alone selects nothing until it moves")

	cmd = c.handleMouse(tea.MouseMsg{Action: tea.MouseActionMotion, X: 6, Y: 1})
	assert.Nil(t, cmd)
	assert.True(t, c.hasSel, "motion past the anchor must start a real selection")

	cmd = c.handleMouse(tea.MouseMsg{Action: tea.MouseActionRelease})
	// copySelectionCmd writes to the clipboard and sets copyToast
	// synchronously, before returning the tea.Tick command that only
	// clears the toast later — no need to run cmd itself (and its 2s
	// delay) to observe either effect.
	require.NotNil(t, cmd, "releasing a real drag must return the copy command")

	assert.Equal(t, "line 2", copied)
	assert.Equal(t, "Copied 1 line", c.copyToast)
	assert.False(t, c.selecting)
	assert.False(t, c.hasSel, "the highlight must clear as soon as the copy completes, not linger until the next press")
}

// TestChatModel_MouseClick_NoDrag_DoesNotCopy covers the "plain click"
// case handleMouse's release branch guards against: a press and release at
// the same point must not copy anything or show a toast, since nothing was
// ever selected.
func TestChatModel_MouseClick_NoDrag_DoesNotCopy(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 40, 10)
	c.sidebarHidden = true
	c.transcript = append(c.transcript, transcriptEntry{role: message.RoleUser, text: "hello"})
	c.refreshViewport()
	c.View(40, 10)

	called := false
	c.copyToClipboard = func(string) { called = true }

	c.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 2, Y: 0})
	cmd := c.handleMouse(tea.MouseMsg{Action: tea.MouseActionRelease})

	assert.Nil(t, cmd)
	assert.False(t, called, "a click with no drag must not copy anything")
	assert.Empty(t, c.copyToast)
}

// TestChatModel_MouseSelection_ExcludesSidebar covers the scoping the user
// explicitly asked for: a selection must only ever cover the main
// transcript pane, never the sidebar, regardless of whether it's shown —
// a press over the sidebar's own columns must not start a selection at
// all.
func TestChatModel_MouseSelection_ExcludesSidebar(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 60, 10)
	c.transcript = append(c.transcript, transcriptEntry{role: message.RoleUser, text: "hello"})
	c.refreshViewport()
	c.View(60, 10) // sidebar visible: measures a non-zero mainColOffset

	require.Greater(t, c.mainColOffset, 0, "the sidebar must occupy real columns for this test to be meaningful")

	cmd := c.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 0, Y: 0})
	assert.Nil(t, cmd)
	assert.False(t, c.selecting, "a press inside the sidebar's columns must not start a selection")
}

// TestChatModel_MouseWheel_NotRoutedToSelection documents that
// handleMouse never sees wheel events at all: model.go's own Update routes
// those straight to c.viewport.Update instead (so scrolling and selection
// can coexist), but handleMouse itself only reacts to Press/Motion/Release
// of the left button — any other action/button is a no-op here.
func TestChatModel_MouseWheel_NotRoutedToSelection(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 40, 10)
	c.sidebarHidden = true
	c.transcript = append(c.transcript, transcriptEntry{role: message.RoleUser, text: "hello"})
	c.refreshViewport()
	c.View(40, 10)

	cmd := c.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	assert.Nil(t, cmd)
	assert.False(t, c.selecting, "a non-left button must never start a selection")
}

// TestChatModel_EdgeDirection covers edgeDirection's pure boundary logic
// in isolation (post-v0.1.0 addendum: Feature 2's edge-triggered
// auto-scroll) — the top/bottom rows themselves are the trigger, not some
// inset deadzone band.
func TestChatModel_EdgeDirection(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 40, 10)
	c.viewport.Height = 10

	assert.Equal(t, -1, c.edgeDirection(0), "the top row itself must trigger upward auto-scroll")
	assert.Equal(t, -1, c.edgeDirection(-3), "above the viewport entirely must still trigger upward auto-scroll")
	assert.Equal(t, 0, c.edgeDirection(5), "the middle of the viewport must never auto-scroll")
	assert.Equal(t, 1, c.edgeDirection(9), "the bottom row itself must trigger downward auto-scroll")
	assert.Equal(t, 1, c.edgeDirection(20), "below the viewport entirely must still trigger downward auto-scroll")
}

// TestChatModel_MouseDrag_BottomEdge_AutoScrollsAndExtendsSelection is this
// package's coverage for Feature 2: holding a selection drag at the
// viewport's bottom row must scroll the transcript downward and keep
// extending the selection to the newly revealed line — repeatedly, via
// handleAutoScrollTick's self-rescheduling chain, since a mouse stationary
// at the edge generates no further MouseActionMotion events on its own.
func TestChatModel_MouseDrag_BottomEdge_AutoScrollsAndExtendsSelection(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 40, 10)
	c.sidebarHidden = true
	for i := 0; i < 30; i++ {
		c.transcript = append(c.transcript, transcriptEntry{role: message.RoleUser, text: fmt.Sprintf("line %d", i)})
	}
	c.refreshViewport()
	c.View(40, 10)
	c.viewport.GotoTop()
	startOffset := c.viewport.YOffset

	cmd := c.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 0, Y: 1})
	require.Nil(t, cmd)
	require.True(t, c.selecting)

	// Y: c.viewport.Height-1 is the bottom row itself — edgeDirection's own
	// trigger condition, not a coordinate beyond the viewport.
	cmd = c.handleMouse(tea.MouseMsg{Action: tea.MouseActionMotion, X: 0, Y: c.viewport.Height - 1})
	require.NotNil(t, cmd, "reaching the bottom edge must start the auto-scroll tick chain")
	assert.Equal(t, 1, c.autoScrollDir)
	assert.True(t, c.autoScrollActive)
	assert.Greater(t, c.viewport.YOffset, startOffset, "the first edge motion must scroll immediately, not wait for a tick")
	assert.True(t, c.hasSel)

	offsetAfterFirstStep := c.viewport.YOffset
	lineAfterFirstStep := c.selCurrent.line

	// Simulate the tick chain firing a few times without actually waiting
	// on the real tea.Tick delay (model_test.go's established convention:
	// call the handler directly instead of running the returned tea.Cmd).
	for i := 0; i < 3; i++ {
		cmd = c.handleAutoScrollTick()
		require.NotNil(t, cmd, "the chain must keep rescheduling itself while autoScrollDir is still set")
	}
	assert.Greater(t, c.viewport.YOffset, offsetAfterFirstStep, "repeated ticks must keep scrolling downward")
	assert.Greater(t, c.selCurrent.line, lineAfterFirstStep, "repeated ticks must keep extending the selection")

	// Moving back into the viewport's middle must stop the scrolling
	// immediately (autoScrollDir reset), even though autoScrollActive
	// doesn't clear until the chain's next tick notices.
	cmd = c.handleMouse(tea.MouseMsg{Action: tea.MouseActionMotion, X: 0, Y: 5})
	assert.Nil(t, cmd)
	assert.Equal(t, 0, c.autoScrollDir)

	offsetAfterStop := c.viewport.YOffset
	cmd = c.handleAutoScrollTick()
	assert.Nil(t, cmd, "the chain must not reschedule itself once autoScrollDir is back to 0")
	assert.False(t, c.autoScrollActive)
	assert.Equal(t, offsetAfterStop, c.viewport.YOffset, "a tick after stopping must not scroll any further")
}

// TestChatModel_MouseRelease_StopsAutoScroll covers the other way an
// auto-scrolling drag ends: releasing the mouse button, not just moving
// back into the viewport's middle.
func TestChatModel_MouseRelease_StopsAutoScroll(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 40, 10)
	c.sidebarHidden = true
	for i := 0; i < 30; i++ {
		c.transcript = append(c.transcript, transcriptEntry{role: message.RoleUser, text: fmt.Sprintf("line %d", i)})
	}
	c.refreshViewport()
	c.View(40, 10)
	c.viewport.GotoTop()

	c.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 0, Y: 1})
	c.handleMouse(tea.MouseMsg{Action: tea.MouseActionMotion, X: 0, Y: c.viewport.Height - 1})
	require.Equal(t, 1, c.autoScrollDir)

	cmd := c.handleMouse(tea.MouseMsg{Action: tea.MouseActionRelease})
	require.NotNil(t, cmd, "releasing a real drag must still return the copy command")
	assert.Equal(t, 0, c.autoScrollDir, "release must stop auto-scroll immediately, not wait for the next tick")

	cmd = c.handleAutoScrollTick()
	assert.Nil(t, cmd)
	assert.False(t, c.autoScrollActive)
}
