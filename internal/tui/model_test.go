package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

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
	c := newChatModel("chat-1", "assistant", nil, "execute", 80, 24)

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
		Mode:     "execute",
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
	c := newChatModel("chat-1", "assistant", nil, "execute", 80, 24)

	c.handleStreamMsg(streamDoneMsg{result: &services.RunResult{
		Response: &agent.Response{},
		Todos: []todo.Item{
			{ID: 1, Title: "Write the report", IsComplete: false},
			{ID: 2, Title: "Send the email", IsComplete: true},
		},
		Mode: "execute",
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
	c := newChatModel("chat-1", "assistant", nil, "execute", 80, 24)

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

// TestModel_ModeIndicatorReflectsSwitch drives the top-level Model with a
// synthetic modeChangedMsg (what toggleModeCmd's tea.Cmd produces once
// AgentService.SetMode succeeds) and asserts both the chat's mode field and
// the rendered mode indicator (Design §3.8) reflect the switch.
func TestModel_ModeIndicatorReflectsSwitch(t *testing.T) {
	m := Model{
		screen: screenChat,
		chat:   newChatModel("chat-1", "assistant", nil, "execute", 80, 24),
		width:  80, height: 24,
	}
	assert.Contains(t, m.View(), "Mode: execute")

	updated, cmd := m.Update(modeChangedMsg{mode: "plan"})
	assert.Nil(t, cmd)
	next := updated.(Model)
	assert.Equal(t, "plan", next.chat.mode)
	assert.Contains(t, next.View(), "Mode: plan")
}

// TestModel_NoAgentsConfigured_ShowsActionableMessage covers the picker
// screen's empty-state, which cmd/canopy/main.go's own startup check exists
// specifically to avoid reaching in the first place — this asserts the TUI
// itself is defensive too.
func TestModel_NoAgentsConfigured_ShowsActionableMessage(t *testing.T) {
	m := NewModel(context.Background(), nil, nil)
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
	})
	assert.Equal(t, []string{"alpha", "mid", "zeta"}, m.agentNames)
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
func drainCmd(t *testing.T, c *chatModel, cmd tea.Cmd) {
	t.Helper()
	for cmd != nil {
		msg := cmd()
		if msg == nil {
			return
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

	c := newChatModel("chat-1", "assistant", nil, "execute", 80, 24)

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

	c := newChatModel("chat-1", "assistant", nil, "execute", 80, 24)

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

// TestChatModel_ModeToggle_RealSetMode exercises toggleModeCmd against a
// real AgentService (SetMode only touches the JSON repository, no provider
// call needed), proving the ctrl+p keybinding produces the modeChangedMsg
// Model.Update expects, with the correct next mode.
func TestChatModel_ModeToggle_RealSetMode(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	svc := services.NewAgentService(services.AgentServiceConfig{
		Definitions: services.Definitions{
			Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}},
		},
		Repository: repo,
	})
	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "execute", 80, 24)

	cmd := c.toggleModeCmd(svc, ctx)
	require.NotNil(t, cmd)
	msg := cmd()
	modeMsg, ok := msg.(modeChangedMsg)
	require.True(t, ok)
	assert.Equal(t, "plan", modeMsg.mode)

	// c.mode itself is only updated by Model.Update on receipt of
	// modeChangedMsg (see model.go) — toggleModeCmd reads c.mode as it was
	// when the Cmd was constructed, matching Bubble Tea's own "Cmds close
	// over model state at construction time" pattern. Simulate that receipt
	// here before toggling again.
	c.mode = modeMsg.mode
	msg = c.toggleModeCmd(svc, ctx)()
	modeMsg, ok = msg.(modeChangedMsg)
	require.True(t, ok)
	assert.Equal(t, "execute", modeMsg.mode, "toggling from plan must switch back to execute")
}
