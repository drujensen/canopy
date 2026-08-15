package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/agent/harness/todo"
	"github.com/microsoft/agent-framework-go/message"

	"github.com/drujensen/canopy/internal/domain/services"
)

// transcriptEntry is one finished message rendered in the chat transcript —
// either something the user sent or a completed assistant reply (built up
// from streamed chunks, see chatModel.streaming).
type transcriptEntry struct {
	role message.Role
	text string
}

// chatModel is the chat screen (Design §5): a scrolling transcript fed by
// AgentService's streaming entry points, a composer, a tool-approval prompt
// that preempts the composer while a request is pending (Design §3.6), and
// a sidebar showing the live todo panel (Design §3.7) and the current
// plan/execute mode with a switch keybinding (Design §3.8).
type chatModel struct {
	chatID    string
	agentName string

	transcript []transcriptEntry

	// streaming accumulates the in-flight assistant reply's text as
	// streamChunkMsg values arrive, before it's folded into transcript by
	// finishStreaming once the turn's stream is done.
	streaming    strings.Builder
	streamActive bool

	// streamCh is the channel the turn currently in flight (if any) is
	// forwarding events on — kept so handleStreamMsg can keep re-arming
	// waitForStreamEvent after every non-terminal event.
	streamCh chan tea.Msg

	composer textinput.Model
	viewport viewport.Model

	todos []todo.Item
	mode  string

	// pendingApproval is the tool-approval request currently awaiting a
	// user decision (Design §3.6), or nil when none is pending. While set,
	// the composer does not receive keystrokes — handleKey routes to
	// handleApprovalKey instead.
	pendingApproval     *message.ToolApprovalRequestContent
	pendingApprovalTool string

	statusErr error

	width, height int
}

// newChatModel constructs a chatModel for a just-started or resumed chat,
// seeded with its current Todos/Mode snapshot (Design §3.7/§3.8).
func newChatModel(chatID, agentName string, todos []todo.Item, mode string, width, height int) *chatModel {
	composer := textinput.New()
	composer.Placeholder = "Type a message and press enter..."
	composer.Focus()
	composer.CharLimit = 4000

	c := &chatModel{
		chatID:    chatID,
		agentName: agentName,
		todos:     todos,
		mode:      mode,
		composer:  composer,
		viewport:  viewport.New(width, height),
	}
	c.resize(width, height)
	return c
}

const sidebarWidth = 28

// resize adjusts the viewport/composer to fit width x height, reserving
// sidebarWidth columns for the todo/mode sidebar and a couple of rows for
// the composer/approval-prompt line.
func (c *chatModel) resize(width, height int) {
	c.width, c.height = width, height
	mainWidth := width - sidebarWidth
	if mainWidth < 10 {
		mainWidth = width
	}
	c.viewport.Width = mainWidth
	viewportHeight := height - 4
	if viewportHeight < 1 {
		viewportHeight = 1
	}
	c.viewport.Height = viewportHeight
	c.composer.Width = mainWidth - 2
	c.refreshViewport()
}

// handleKey processes a key press for the chat screen: approval decisions
// while pendingApproval is set, mode-switch/message-submit otherwise, and
// composer text editing as the fallback.
func (c *chatModel) handleKey(msg tea.KeyMsg, svc *services.AgentService, ctx context.Context) tea.Cmd {
	if c.pendingApproval != nil {
		return c.handleApprovalKey(msg, svc, ctx)
	}

	switch msg.String() {
	case "ctrl+p":
		return c.toggleModeCmd(svc, ctx)
	case "enter":
		if c.streamActive {
			return nil
		}
		text := strings.TrimSpace(c.composer.Value())
		if text == "" {
			return nil
		}
		c.composer.Reset()
		c.transcript = append(c.transcript, transcriptEntry{role: message.RoleUser, text: text})
		c.refreshViewport()
		return c.startTurnCmd(svc, ctx, []*message.Message{message.NewText(text)})
	}

	if c.streamActive {
		return nil
	}
	var cmd tea.Cmd
	c.composer, cmd = c.composer.Update(msg)
	return cmd
}

// handleApprovalKey maps the approval prompt's keybindings (Design §3.6:
// "approve once" / "always allow this tool", plus deny) onto the matching
// message.ToolApprovalRequestContent response content, and starts a new
// turn carrying it back to AgentService — the only way to answer a pending
// approval request (see AgentService.RunMessages' doc comment).
func (c *chatModel) handleApprovalKey(msg tea.KeyMsg, svc *services.AgentService, ctx context.Context) tea.Cmd {
	req := c.pendingApproval
	switch msg.String() {
	case "y":
		return c.respondApproval(svc, ctx, req.CreateResponse(true, ""))
	case "a":
		return c.respondApproval(svc, ctx, req.AlwaysApproveToolResponse())
	case "n":
		return c.respondApproval(svc, ctx, req.CreateResponse(false, "denied by user"))
	}
	return nil
}

func (c *chatModel) respondApproval(svc *services.AgentService, ctx context.Context, content message.Content) tea.Cmd {
	c.pendingApproval = nil
	c.pendingApprovalTool = ""
	approvalMsg := &message.Message{Role: message.RoleUser, Contents: []message.Content{content}}
	return c.startTurnCmd(svc, ctx, []*message.Message{approvalMsg})
}

// toggleModeCmd flips between "plan" and "execute" (Design §3.8's user
// keybinding path — a mode switch initiated by the user, not the model)
// via AgentService.SetMode.
func (c *chatModel) toggleModeCmd(svc *services.AgentService, ctx context.Context) tea.Cmd {
	next := "plan"
	if c.mode == "plan" {
		next = "execute"
	}
	chatID := c.chatID
	return func() tea.Msg {
		if err := svc.SetMode(ctx, chatID, next); err != nil {
			return streamErrMsg{err: fmt.Errorf("switching mode: %w", err)}
		}
		return modeChangedMsg{mode: next}
	}
}

// startTurnCmd marks the chat as actively streaming and delegates to
// startTurn (stream.go) to drive the turn against svc via
// AgentService.RunMessagesStream.
func (c *chatModel) startTurnCmd(svc *services.AgentService, ctx context.Context, msgs []*message.Message) tea.Cmd {
	c.streamActive = true
	c.streaming.Reset()
	ch, cmd := startTurn(ctx, svc, c.chatID, msgs)
	c.streamCh = ch
	return cmd
}

// handleStreamMsg applies one streaming event (see stream.go) to the chat's
// state: accumulating text/detecting an approval request for
// streamChunkMsg, folding the finished reply into the transcript and
// updating Todos/Mode for streamDoneMsg, or surfacing an error for
// streamErrMsg. It returns the next Cmd to run — re-arming
// waitForStreamEvent for a non-terminal chunk, or nil once the turn is over.
func (c *chatModel) handleStreamMsg(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case streamChunkMsg:
		c.applyUpdate(msg.update)
		c.refreshViewport()
		return waitForStreamEvent(c.streamCh)
	case streamDoneMsg:
		c.finishStreaming()
		if msg.result != nil {
			c.todos = msg.result.Todos
			c.mode = msg.result.Mode
		}
		c.refreshViewport()
		return nil
	case streamErrMsg:
		c.finishStreaming()
		c.statusErr = msg.err
		c.refreshViewport()
		return nil
	}
	return nil
}

// applyUpdate folds one *agent.ResponseUpdate's contents into the chat's
// in-flight state: text is appended to the streaming buffer so it renders
// incrementally (Requirements FR8); a tool-approval request pauses the
// composer and surfaces the approval prompt (Design §3.6).
func (c *chatModel) applyUpdate(update *agent.ResponseUpdate) {
	if update == nil {
		return
	}
	for _, content := range update.Contents {
		switch t := content.(type) {
		case *message.TextContent:
			c.streaming.WriteString(t.Text)
		case *message.ToolApprovalRequestContent:
			c.pendingApproval = t
			c.pendingApprovalTool = toolCallName(t.ToolCall)
		case *message.FunctionCallContent:
			if t.Name != "" {
				c.streaming.WriteString(fmt.Sprintf("\n[calling %s...]\n", t.Name))
			}
		}
	}
}

// finishStreaming folds the accumulated streaming buffer into transcript as
// a completed assistant entry and clears the in-flight streaming state.
func (c *chatModel) finishStreaming() {
	if c.streaming.Len() > 0 {
		c.transcript = append(c.transcript, transcriptEntry{role: message.RoleAssistant, text: c.streaming.String()})
	}
	c.streaming.Reset()
	c.streamActive = false
	c.streamCh = nil
}

// toolCallName extracts a human-readable tool name from a
// message.ToolCallContent for display in the approval prompt, falling back
// to the call ID for a tool-call shape this package doesn't special-case.
func toolCallName(tc message.ToolCallContent) string {
	switch t := tc.(type) {
	case *message.FunctionCallContent:
		if t != nil {
			return t.Name
		}
	case *message.MCPServerToolCallContent:
		if t != nil {
			return t.Name
		}
	}
	if tc != nil {
		return tc.GetCallID()
	}
	return "unknown tool"
}

// refreshViewport re-renders the transcript (plus any in-flight streaming
// text) into the viewport and scrolls to the bottom, so new content is
// always visible as it streams in.
func (c *chatModel) refreshViewport() {
	var b strings.Builder
	for _, e := range c.transcript {
		b.WriteString(renderEntry(e))
		b.WriteString("\n\n")
	}
	if c.streaming.Len() > 0 {
		b.WriteString(renderEntry(transcriptEntry{role: message.RoleAssistant, text: c.streaming.String()}))
	}
	c.viewport.SetContent(b.String())
	c.viewport.GotoBottom()
}

func renderEntry(e transcriptEntry) string {
	label := userLabelStyle.Render("you")
	if e.role == message.RoleAssistant {
		label = assistantLabelStyle.Render("assistant")
	}
	return label + "\n" + e.text
}

// View renders the chat screen: a sidebar (mode + todos) beside the
// transcript, with either the composer or a pending approval prompt at the
// bottom, and a status line for the last stream error, if any.
func (c *chatModel) View(width, height int) string {
	sidebar := c.renderSidebar()

	var bottom string
	if c.pendingApproval != nil {
		bottom = c.renderApprovalPrompt()
	} else {
		bottom = c.composer.View()
	}

	main := lipgloss.JoinVertical(lipgloss.Left, c.viewport.View(), bottom)
	if c.statusErr != nil {
		main = lipgloss.JoinVertical(lipgloss.Left, main, errorStyle.Render(c.renderStatusErrLine()))
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, sidebar, main)
}

// renderSidebar renders the mode indicator/switcher (Design §3.8) and the
// live todo panel (Design §3.7).
func (c *chatModel) renderSidebar() string {
	var b strings.Builder
	b.WriteString(modeStyle.Render("Mode: " + c.mode))
	b.WriteString("\n(ctrl+p to switch)\n\n")
	b.WriteString("Todos:\n")
	if len(c.todos) == 0 {
		b.WriteString("  (none yet)\n")
	}
	for _, item := range c.todos {
		mark := "[ ]"
		title := item.Title
		if item.IsComplete {
			mark = "[x]"
			title = todoDoneStyle.Render(title)
		}
		fmt.Fprintf(&b, "  %s %s\n", mark, title)
	}
	return sidebarStyle.Width(sidebarWidth - 2).Height(height(c)).Render(b.String())
}

func height(c *chatModel) int {
	h := c.height - 2
	if h < 1 {
		h = 1
	}
	return h
}

// renderApprovalPrompt renders the two-tier permission prompt Design §3.6
// requires: "approve once" and "always allow this tool", plus a deny option.
func (c *chatModel) renderApprovalPrompt() string {
	return approvalStyle.Render(fmt.Sprintf(
		"Tool approval requested: %s\n[y] approve once   [a] always allow this tool   [n] deny",
		c.pendingApprovalTool,
	))
}

// renderStatusErrLine formats c.statusErr as a single display line, clamped
// to the chat screen's main-column width.
//
// Without this, View's layout budget (resize reserves exactly height-4 rows
// for the viewport/composer, with no row set aside for an error line at all)
// silently breaks for any error whose text is wider than the terminal or
// contains embedded newlines (a real provider error, e.g. a JSON error body
// from a real API failure, routinely has both): lipgloss.Render doesn't wrap
// or clip on its own, so the raw string is handed straight to the terminal,
// which wraps a too-wide line or renders embedded newlines as genuinely
// separate rows — pushing the total rendered height past what the terminal
// can show in one screen and scrolling the sidebar/mode-indicator/transcript
// above it out of view, exactly the secondary layout bug this task flagged.
// Collapsing embedded newlines to spaces and truncating to one line's worth
// of the main column's width keeps the error's footprint to the single row
// View's layout already visually allots it next to the composer/approval
// prompt.
func (c *chatModel) renderStatusErrLine() string {
	text := "error: " + strings.ReplaceAll(strings.ReplaceAll(c.statusErr.Error(), "\r\n", " "), "\n", " ")

	maxWidth := c.width - sidebarWidth
	if maxWidth < 10 {
		maxWidth = c.width
	}
	if maxWidth <= 0 {
		return text
	}

	runes := []rune(text)
	if len(runes) <= maxWidth {
		return text
	}
	if maxWidth == 1 {
		return string(runes[:1])
	}
	return string(runes[:maxWidth-1]) + "…"
}
