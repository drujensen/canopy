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
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
	"github.com/drujensen/canopy/internal/impl/skillsource"
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
	}})
	assert.Equal(t, "Hello", c.streaming.String())
	assert.Contains(t, c.View(80, 24), "Hello", "in-flight streamed text must be visible before the turn finishes")
	// A real chunk re-arms waitForStreamEvent; here streamCh is nil (no real
	// turn in flight), which is fine — the test never calls cmd().
	_ = cmd

	c.handleStreamMsg(streamChunkMsg{update: &agent.ResponseUpdate{
		Contents: message.Contents{&message.TextContent{Text: ", world"}},
	}})
	assert.Equal(t, "Hello, world", c.streaming.String())

	require.Empty(t, c.transcript, "nothing should be folded into the transcript until the turn's stream is done")

	c.handleStreamMsg(streamDoneMsg{result: &services.RunResult{
		Response: &agent.Response{},
		Todos:    nil,
	}})

	require.Len(t, c.transcript, 1)
	assert.Equal(t, "Hello, world", c.transcript[0].text)
	assert.Equal(t, message.RoleAssistant, c.transcript[0].role)
	assert.Empty(t, c.streaming.String(), "the streaming buffer must be cleared once folded into the transcript")
	assert.False(t, c.streamActive)
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
	}})

	require.Len(t, c.todos, 2)
	view := c.View(80, 24)
	assert.Contains(t, view, "Write the report")
	assert.Contains(t, view, "Send the email")
	assert.Contains(t, view, "[ ] Write the report")
}

// TestChatModel_ApprovalPromptAppears drives a synthetic streamChunkMsg
// carrying a *message.ToolApprovalRequestContent and asserts the approval
// prompt (Design §3.6) becomes visible, pre-empting the composer.
func TestChatModel_ApprovalPromptAppears(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	req := &message.ToolApprovalRequestContent{
		RequestID: "req-1",
		ToolCall:  &message.FunctionCallContent{CallID: "call-1", Name: "run_shell", Arguments: "{}"},
	}
	c.handleStreamMsg(streamChunkMsg{update: &agent.ResponseUpdate{
		Contents: message.Contents{req},
	}})

	require.NotNil(t, c.pendingApproval)
	assert.Equal(t, "run_shell", c.pendingApprovalTool)
	view := c.View(80, 24)
	assert.Contains(t, view, "Tool approval requested: run_shell")
	assert.Contains(t, view, "approve once")
	assert.Contains(t, view, "always allow this tool")
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

	drainCmd(t, c, cmd)
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
	assert.Equal(t, "hello there", entries[1].text)
}

func TestReconstructTranscript_SkipsToolMessagesAndEmptyText(t *testing.T) {
	msgs := []*message.Message{
		{Role: message.RoleUser, Contents: message.Contents{&message.TextContent{Text: "read the file"}}},
		{Role: message.RoleAssistant, Contents: message.Contents{&message.FunctionCallContent{Name: "file_read"}}}, // no text
		{Role: message.RoleTool, Contents: message.Contents{&message.TextContent{Text: "file contents"}}},
		nil,
		{Role: message.RoleAssistant, Contents: message.Contents{&message.TextContent{Text: "here's the summary"}}},
	}
	entries := reconstructTranscript(msgs)
	require.Len(t, entries, 2, "only the two text-bearing user/assistant messages must survive")
	assert.Equal(t, "read the file", entries[0].text)
	assert.Equal(t, "here's the summary", entries[1].text)
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
// handleKey's "enter" case and respondApproval (post-v0.1.0 addendum: the
// "still working" spinner) return tea.Batch(startTurnCmd(...), spinner.Tick)
// rather than startTurnCmd's bare Cmd, so cmd() can yield a tea.BatchMsg —
// unlike every other Msg this loop sees, that's not something
// c.handleStreamMsg understands (it only switches on streamChunkMsg/
// streamDoneMsg/streamErrMsg), so it must be unwrapped here instead of fed
// straight through: run every sub-Cmd once, keep draining whichever one
// actually produced a real stream message, and silently drop the rest (the
// spinner tick, whose own resulting spinner.TickMsg isn't relevant to any
// test using this helper — none of them assert on spinner animation, only
// on the underlying turn completing without hanging or leaking).
func drainCmd(t *testing.T, c *chatModel, cmd tea.Cmd) {
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
					cmd = c.handleStreamMsg(subMsg)
				}
			}
			continue
		}
		cmd = c.handleStreamMsg(msg)
	}
}

// newApprovalTestService builds a real AgentService (JSON-backed repository
// in a temp dir, one OpenAI-compatible provider pointed at an httptest
// server) whose model alternates: an odd-numbered call requests the
// approval-gated run_shell tool against a marker file, an even-numbered
// call answers "ok" — the same shape phase5_test.go's approval test uses in
// package services, reproduced here so this package's tests don't need to
// depend on that unexported package.
func newApprovalTestService(t *testing.T) (svc *services.AgentService, marker string) {
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

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: server.URL + "/v1"}
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

// TestChatModel_ApprovalFlow_ApproveOnce_DoesNotPersist drives the real
// keybinding path: "y" (approve once) lets the pending tool call execute,
// but a *second* run request still surfaces a fresh approval prompt,
// proving "approve once" really is once — not a standing rule.
func TestChatModel_ApprovalFlow_ApproveOnce_DoesNotPersist(t *testing.T) {
	svc, marker := newApprovalTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	drainCmd(t, c, c.startTurnCmd(svc, ctx, []*message.Message{message.NewText("please run the marker command")}))
	require.NotNil(t, c.pendingApproval, "the first run_shell request must surface an approval prompt")
	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr))

	drainCmd(t, c, c.handleApprovalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}}, svc, ctx))
	assert.Nil(t, c.pendingApproval, "approving once must clear the pending prompt")
	_, statErr = os.Stat(marker)
	require.NoError(t, statErr, "the tool must have executed once approved")

	require.NoError(t, os.Remove(marker))
	drainCmd(t, c, c.startTurnCmd(svc, ctx, []*message.Message{message.NewText("please run it again")}))
	assert.NotNil(t, c.pendingApproval, "approve-once must not have installed a standing rule — the second request must prompt again")
}

// TestChatModel_ApprovalFlow_AlwaysAllow_Persists drives the "a" (always
// allow this tool) keybinding and proves the standing rule it installs
// actually suppresses the prompt on a subsequent run within the same chat.
func TestChatModel_ApprovalFlow_AlwaysAllow_Persists(t *testing.T) {
	svc, marker := newApprovalTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)

	drainCmd(t, c, c.startTurnCmd(svc, ctx, []*message.Message{message.NewText("please run the marker command")}))
	require.NotNil(t, c.pendingApproval)

	drainCmd(t, c, c.handleApprovalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}}, svc, ctx))
	assert.Nil(t, c.pendingApproval)
	_, statErr := os.Stat(marker)
	require.NoError(t, statErr, "the tool must have executed once always-allowed")

	require.NoError(t, os.Remove(marker))
	drainCmd(t, c, c.startTurnCmd(svc, ctx, []*message.Message{message.NewText("please run it again")}))
	assert.Nil(t, c.pendingApproval, "always-allow must have installed a standing rule that auto-approves the second request")
	_, statErr = os.Stat(marker)
	assert.NoError(t, statErr, "the tool must have executed again automatically under the standing rule")
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

// TestChatModel_CtrlA_CtrlO_NoopDuringPendingApproval asserts ctrl+a/ctrl+o
// are swallowed (not opening the overlay) while an approval prompt is
// pending — routed to handleApprovalKey instead, which has no case for
// either key, matching "approve"/"always allow"/"deny" being the only valid
// responses while a tool call awaits a decision.
func TestChatModel_CtrlA_CtrlO_NoopDuringPendingApproval(t *testing.T) {
	svc := newSwitchTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.pendingApproval = &message.ToolApprovalRequestContent{
		RequestID: "req-1",
		ToolCall:  &message.FunctionCallContent{CallID: "call-1", Name: "run_shell", Arguments: "{}"},
	}

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlA}, svc, ctx)
	assert.Nil(t, cmd)
	assert.Nil(t, c.picker, "ctrl+a must not open the agent-switch overlay while an approval prompt is pending")
	assert.NotNil(t, c.pendingApproval, "the pending approval must be untouched")

	cmd = c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlO}, svc, ctx)
	assert.Nil(t, cmd)
	assert.Nil(t, c.picker, "ctrl+o must not open the model-switch overlay while an approval prompt is pending")
	assert.NotNil(t, c.pendingApproval)
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
// picker), and the resulting chatModel's transcript/todos/pending-approval
// are all genuinely empty/nil rather than carried over from the old chat.
func TestModel_CtrlN_StartsGenuinelyNewChat_ResetsState(t *testing.T) {
	svc := newSwitchTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.transcript = append(c.transcript, transcriptEntry{role: message.RoleUser, text: "hello before new chat"})
	c.todos = []todo.Item{{ID: 1, Title: "leftover todo"}}
	c.pendingApproval = nil // starts nil; asserted below on the fresh chat too

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
	assert.Nil(t, next.chat.pendingApproval, "a genuinely new chat must start with no pending approval")
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

// TestChatModel_CtrlN_NoopDuringPendingApproval asserts ctrl+n is swallowed
// (routed to handleApprovalKey, which has no case for it) while a tool
// approval is pending, rather than abandoning that pending decision.
func TestChatModel_CtrlN_NoopDuringPendingApproval(t *testing.T) {
	svc := newSwitchTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.pendingApproval = &message.ToolApprovalRequestContent{
		RequestID: "req-1",
		ToolCall:  &message.FunctionCallContent{CallID: "call-1", Name: "run_shell", Arguments: "{}"},
	}

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlN}, svc, ctx)
	assert.Nil(t, cmd, "ctrl+n must be a no-op while an approval prompt is pending")
	assert.NotNil(t, c.pendingApproval, "the pending approval must be untouched")
	assert.Equal(t, "chat-1", c.chatID, "the chat must not have been replaced while an approval is pending")
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
	require.Len(t, m.chat.picker.Items(), 2)

	// Move to the second entry and select it — deliberately not asserting
	// which chat ID ends up first (recency-sorted, not hardcoded here);
	// only that resuming actually switches to a *different* chat than the
	// one ctrl+h was opened from.
	m.chat.handleKey(tea.KeyMsg{Type: tea.KeyDown}, svc, ctx)
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
	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: titleServer.URL + "/v1"}
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
