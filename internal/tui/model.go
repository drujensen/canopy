package tui

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/drujensen/canopy/internal/domain/services"
	"github.com/drujensen/canopy/internal/impl/agentsource"
)

// screen identifies which top-level view Model is showing.
type screen int

const (
	screenAgentPicker screen = iota
	screenChat
)

// Model is Canopy's top-level Bubble Tea model (Design §5): an agent picker
// sourced from agentsource-loaded definitions, handing off into a chat view
// once an agent is picked (see chatModel in chat.go for that screen's own
// state/behavior).
type Model struct {
	svc *services.AgentService
	ctx context.Context

	screen screen

	agentList  list.Model
	agentNames []string

	chat *chatModel

	width, height int

	// fatalErr is a picker-level error (StartChat/GetTodos/GetMode failing
	// for the picked agent) shown instead of transitioning to the chat
	// screen.
	fatalErr error
}

// NewModel constructs the picker-screen Model from svc and the agent
// definitions it was built from (Design §5: "agent picker sourced from
// agentsource, not a database list"). ctx is used for every AgentService
// call the picker/chat views make for the lifetime of the program — Bubble
// Tea's own tea.Cmd/tea.Msg loop has no per-call context of its own.
func NewModel(ctx context.Context, svc *services.AgentService, agents map[string]agentsource.AgentDefinition) Model {
	names := make([]string, 0, len(agents))
	for name := range agents {
		names = append(names, name)
	}
	sort.Strings(names)

	items := make([]list.Item, 0, len(names))
	for _, name := range names {
		items = append(items, agentItem{def: agents[name]})
	}

	l := list.New(items, list.NewDefaultDelegate(), 80, 24)
	l.Title = "Select an agent"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(true)

	return Model{
		svc:        svc,
		ctx:        ctx,
		screen:     screenAgentPicker,
		agentList:  l,
		agentNames: names,
		width:      80,
		height:     24,
	}
}

// Init implements tea.Model. There's nothing to kick off until the user
// picks an agent — the picker's items are already loaded synchronously by
// NewModel from the file-format loaders' output (Design §3.11), which ran
// at startup before the program was constructed (see cmd/canopy/main.go).
func (m Model) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model, routing streaming/chat-lifecycle messages
// (see stream.go) to the active chat, if any, key presses to the active
// screen, and window-resize messages to both the picker list and the chat
// screen's own layout.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.agentList.SetSize(msg.Width, msg.Height)
		if m.chat != nil {
			m.chat.resize(msg.Width, msg.Height)
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case chatStartedMsg:
		if msg.err != nil {
			m.fatalErr = msg.err
			return m, nil
		}
		m.chat = newChatModel(msg.chatID, msg.agentName, msg.todos, msg.mode, msg.model, m.width, m.height)
		m.screen = screenChat
		return m, nil

	case modeChangedMsg:
		if m.chat != nil {
			m.chat.mode = msg.mode
		}
		return m, nil

	case modelChangedMsg:
		if m.chat != nil {
			m.chat.model = msg.model
		}
		return m, nil

	case agentChangedMsg:
		if m.chat != nil {
			m.chat.agentName = msg.agentName
		}
		return m, nil

	case streamChunkMsg, streamDoneMsg, streamErrMsg:
		if m.chat == nil {
			return m, nil
		}
		return m, m.chat.handleStreamMsg(msg)
	}

	return m, nil
}

// handleKey dispatches a key press to the active screen: global quit
// (ctrl+c) first, then picker navigation/selection or chat-screen handling
// (composer editing, approval-prompt decisions, mode switch).
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}

	switch m.screen {
	case screenAgentPicker:
		if m.agentList.FilterState() != list.Filtering {
			switch msg.String() {
			case "enter":
				item, ok := m.agentList.SelectedItem().(agentItem)
				if !ok {
					return m, nil
				}
				return m, m.startChatCmd(item.def.Name)
			case "q", "esc":
				return m, tea.Quit
			}
		}
		var cmd tea.Cmd
		m.agentList, cmd = m.agentList.Update(msg)
		return m, cmd

	case screenChat:
		if m.chat == nil {
			return m, nil
		}
		return m, m.chat.handleKey(msg, m.svc, m.ctx)
	}

	return m, nil
}

// startChatCmd starts a brand-new chat bound to agentName (AgentService.
// StartChat) and seeds it with its (freshly initialized) Todos/Mode
// snapshot.
//
// Judgment call: Design §5 describes the picker as letting the user "start
// or resume" a chat, but AgentService's exported surface (Phase 5) has no
// way to enumerate previously started chats by agent name — only
// interfaces.ChatRepository does, and AgentService intentionally keeps that
// dependency unexported (see agent_service.go's package doc comment on
// AgentService's own scope). Rather than reach around AgentService to the
// repository directly from this package (which would blur the DDD boundary
// AGENTS.md asks callers to respect — the TUI depends on domain/services,
// not on impl/repositories), every agent pick here starts a fresh chat.
// Resuming a specific prior chat is a real, flagged follow-up, not an
// oversight.
func (m Model) startChatCmd(agentName string) tea.Cmd {
	svc := m.svc
	ctx := m.ctx
	return func() tea.Msg {
		chatID := fmt.Sprintf("%s-%d", agentName, time.Now().UnixNano())
		if _, err := svc.StartChat(ctx, chatID, agentName); err != nil {
			return chatStartedMsg{err: fmt.Errorf("starting chat: %w", err)}
		}
		todos, err := svc.GetTodos(ctx, chatID)
		if err != nil {
			return chatStartedMsg{err: fmt.Errorf("loading todos: %w", err)}
		}
		mode, err := svc.GetMode(ctx, chatID)
		if err != nil {
			return chatStartedMsg{err: fmt.Errorf("loading mode: %w", err)}
		}
		model, err := svc.GetModel(ctx, chatID)
		if err != nil {
			return chatStartedMsg{err: fmt.Errorf("loading model: %w", err)}
		}
		return chatStartedMsg{chatID: chatID, agentName: agentName, todos: todos, mode: mode, model: model}
	}
}

// View implements tea.Model.
func (m Model) View() string {
	if m.fatalErr != nil {
		return errorStyle.Render(fmt.Sprintf("canopy: %v", m.fatalErr)) + "\n\npress ctrl+c to quit"
	}

	switch m.screen {
	case screenAgentPicker:
		if len(m.agentNames) == 0 {
			return "No agents configured — add a .claude/agents/*.md file and restart.\n\npress ctrl+c to quit"
		}
		return m.agentList.View()
	case screenChat:
		if m.chat == nil {
			return "Starting chat..."
		}
		return m.chat.View(m.width, m.height)
	}
	return ""
}
