package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/drujensen/canopy/internal/domain/services"
	"github.com/drujensen/canopy/internal/impl/agentsource"
)

// screen identifies which top-level view Model is showing.
type screen int

const (
	screenAgentPicker screen = iota
	screenChat
	// screenHistory is the top-level chat-history browser (post-v0.1.0
	// addendum, Design §5's addendum — ctrl+h, reachable from
	// screenAgentPicker) — see historyList's own doc comment.
	screenHistory
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

	// fatalErr is a picker-level error (StartChat/GetTodos failing
	// for the picked agent) shown instead of transitioning to the chat
	// screen.
	fatalErr error

	// startAgent (post-v0.1.0 addendum, Design §5's addendum), when
	// non-empty, names an agent Init should immediately start a chat with
	// instead of leaving the user on the picker screen — cmd/canopy's
	// computeStartAgent resolves this before constructing Model (the
	// last-used agent if it still exists, else "general" if configured,
	// else "" to fall through to the picker exactly as before this
	// feature existed).
	startAgent string

	// resumeChatID (post-v0.1.0 addendum: --continue, Design §5's addendum),
	// when non-empty, names a chat ID Init should immediately resume
	// instead of either starting a new chat (startAgent) or showing the
	// picker — cmd/canopy resolves it to the most recently updated chat's
	// ID (ListChatSummaries) when --continue is passed. Takes priority over
	// startAgent in Init: --continue is an explicit, one-shot request to
	// resume a specific conversation, a stronger signal than the passive
	// "which agent was last active" default.
	resumeChatID string

	// historyList is the ctrl+h chat-history browser's item list
	// (post-v0.1.0 addendum, Design §5's addendum), populated by
	// historyLoadedMsg once loadHistoryCmd's real disk I/O (ListChatSummaries)
	// completes — see loadHistoryCmd's doc comment for why this can't be
	// built synchronously the way agentList already is. Only meaningful
	// while screen == screenHistory.
	historyList list.Model

	// historyErr is a screenAgentPicker/screenHistory-level error loading
	// chat history (post-v0.1.0 addendum) — shown as a one-line message
	// rather than fatalErr's whole-screen takeover, since failing to list
	// past sessions shouldn't block starting a new one.
	historyErr error
}

// newChatID generates a chat ID for a brand-new chat bound to agentName —
// the one ID-generation scheme Canopy uses, shared by the top-level agent
// picker's startChatCmd and the in-chat ctrl+n "new chat" keybinding
// (chatModel.startNewChatCmd, chat.go, post-v0.1.0 addendum) so there is
// only ever one way chat IDs get minted, not two schemes that could drift
// apart. Time-based (UnixNano) rather than a random UUID: simple, no new
// dependency, and collision-proof in practice for a single-process CLI tool
// creating chats one keypress at a time.
func newChatID(agentName string) string {
	return fmt.Sprintf("%s-%d", agentName, time.Now().UnixNano())
}

// NewModel constructs the picker-screen Model from svc and the agent
// definitions it was built from (Design §5: "agent picker sourced from
// agentsource, not a database list"). ctx is used for every AgentService
// call the picker/chat views make for the lifetime of the program — Bubble
// Tea's own tea.Cmd/tea.Msg loop has no per-call context of its own.
//
// startAgent (post-v0.1.0 addendum, Design §5's addendum), when non-empty,
// is an agent name Init will immediately start a chat with — see the
// startAgent field's own doc comment. resumeChatID (post-v0.1.0 addendum:
// --continue), when non-empty, is a chat ID Init will immediately resume
// instead — see the resumeChatID field's own doc comment for the priority
// between the two. The Model still always starts constructed on
// screenAgentPicker/with agentList populated regardless: starting or
// resuming a chat is I/O (AgentService.StartChat/GetChat/GetTodos)
// that can't happen synchronously inside NewModel, so Init defers to the
// exact same startChatCmd/resumeChatCmd -> chatStartedMsg path a manual
// picker selection or ctrl+h already uses — the picker screen is simply
// never shown if that Cmd succeeds before the user would have seen it.
func NewModel(ctx context.Context, svc *services.AgentService, agents map[string]agentsource.AgentDefinition, startAgent, resumeChatID string) Model {
	names := make([]string, 0, len(agents))
	for name := range agents {
		names = append(names, name)
	}
	agentsource.SortNames(names)

	items := make([]list.Item, 0, len(names))
	for _, name := range names {
		items = append(items, agentItem{def: agents[name]})
	}

	l := list.New(items, list.NewDefaultDelegate(), 80, 24)
	l.Title = "Select an agent"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(true)

	// historyList is constructed empty here (post-v0.1.0 addendum) rather
	// than left at its zero value: bubbles/list.Model isn't documented as
	// safe to use before list.New, and the every-WindowSizeMsg SetSize call
	// below runs long before the first ctrl+h press would otherwise
	// initialize it. historyLoadedMsg's handler (model.go's Update)
	// populates it with real items via SetItems once loadHistoryCmd
	// actually resolves.
	hl := list.New(nil, list.NewDefaultDelegate(), 80, 24)
	hl.Title = "History"
	hl.SetShowStatusBar(false)
	hl.SetFilteringEnabled(true)

	return Model{
		svc:          svc,
		ctx:          ctx,
		screen:       screenAgentPicker,
		agentList:    l,
		agentNames:   names,
		historyList:  hl,
		width:        80,
		height:       24,
		startAgent:   startAgent,
		resumeChatID: resumeChatID,
	}
}

// Init implements tea.Model. resumeChatID, when set (post-v0.1.0 addendum:
// --continue), takes priority — this immediately fires resumeChatCmd for
// it. Otherwise, startAgent, when set (post-v0.1.0 addendum: auto-resuming
// the last-used agent), fires the same startChatCmd a manual picker
// selection would. Either way the resulting chatStartedMsg is handled
// identically by Update's existing case, so a successful auto-start/resume
// transitions straight to screenChat and a failure surfaces via the
// existing fatalErr display, with no new error path. Otherwise (the
// picker's prior, only behavior) there's nothing to kick off until the user
// picks an agent or opens ctrl+h.
func (m Model) Init() tea.Cmd {
	if m.resumeChatID != "" {
		return resumeChatCmd(m.svc, m.ctx, m.resumeChatID)
	}
	if m.startAgent == "" {
		return nil
	}
	return m.startChatCmd(m.startAgent)
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
		m.historyList.SetSize(msg.Width, msg.Height)
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
		m.chat = newChatModel(msg.chatID, msg.agentName, msg.todos, msg.model, m.width, m.height)
		if len(msg.messages) > 0 {
			// A resume (ctrl+h/--continue, resumeChatCmd), not a genuinely
			// new chat — seed the transcript from prior history instead of
			// leaving it to show defaultGreeting, and never (re)attempt
			// title generation for a chat well past its first exchange
			// (titleAttempted's own doc comment).
			m.chat.transcript = reconstructTranscript(msg.messages)
			m.chat.titleAttempted = true
			m.chat.refreshViewport()
		}
		m.screen = screenChat
		return m, nil

	case historyLoadedMsg:
		// Which UI this populates depends on which screen asked for it
		// (post-v0.1.0 addendum: ctrl+h is reachable both from
		// screenAgentPicker and from an active chat) — see loadHistoryCmd's
		// doc comment for why this can't be answered synchronously and so
		// has to make this determination here instead of at the call site.
		if msg.err != nil {
			if m.screen == screenChat && m.chat != nil {
				m.chat.statusErr = fmt.Errorf("loading history: %w", msg.err)
				m.chat.refreshViewport()
			} else {
				m.historyErr = msg.err
			}
			return m, nil
		}
		items := make([]list.Item, 0, len(msg.summaries))
		for _, s := range msg.summaries {
			items = append(items, historyPickerItem{chatID: s.ID, title: s.Title, agentName: s.AgentName, updatedAt: s.UpdatedAt})
		}
		if m.screen == screenChat && m.chat != nil {
			l := list.New(items, list.NewDefaultDelegate(), m.chat.width-sidebarWidth, m.chat.height-4)
			l.Title = "History"
			l.SetShowStatusBar(false)
			l.SetFilteringEnabled(true)
			m.chat.picker = &l
			m.chat.pickerKind = pickerHistory
			return m, nil
		}
		m.historyErr = nil
		cmd := m.historyList.SetItems(items)
		m.historyList.SetSize(m.width, m.height)
		m.screen = screenHistory
		return m, cmd

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
		// Detected *before* handleStreamMsg runs (post-v0.1.0 addendum:
		// ctrl+h history browser's generated titles): a streamDoneMsg
		// completing the chat's first exchange is exactly the point
		// transcript goes from a lone user message (len 1) to that plus
		// the assistant's reply — handleStreamMsg's own finishStreaming is
		// what performs that append, so "was it 1 before" has to be
		// captured ahead of the call, not after. titleAttempted (checked
		// here too) guards against firing again on every later turn, and
		// is already true from construction for a resumed chat (see
		// chatStartedMsg's case above), which by definition is past its
		// first exchange already.
		firstExchangeJustCompleted := false
		if _, ok := msg.(streamDoneMsg); ok {
			firstExchangeJustCompleted = len(m.chat.transcript) == 1 && !m.chat.titleAttempted
		}
		cmd := m.chat.handleStreamMsg(msg)
		if firstExchangeJustCompleted {
			m.chat.titleAttempted = true
			cmd = tea.Batch(cmd, generateTitleCmd(m.svc, m.ctx, m.chat.chatID))
		}
		return m, cmd

	case spinner.TickMsg:
		// Only keeps ticking while a turn is actually in flight — once
		// streamActive flips false (finishStreaming, reached via
		// handleStreamMsg's streamDoneMsg/streamErrMsg cases), this simply
		// stops re-arming rather than needing an explicit "stop" message of
		// its own (post-v0.1.0 addendum: the "still working" spinner,
		// chat.go's View).
		if m.chat == nil || !m.chat.streamActive {
			return m, nil
		}
		var cmd tea.Cmd
		m.chat.spinner, cmd = m.chat.spinner.Update(msg)
		return m, cmd

	default:
		// Fixes a long-standing bug present since ctrl+a/ctrl+o's overlay
		// pickers were first introduced, not something new: bubbles/
		// list.Model's own filtering is asynchronous — typing into an open
		// filter updates FilterInput immediately, but list.Model.Update
		// returns a Cmd (producing list.FilterMatchesMsg) that has to be
		// run and fed back into that *same* list.Model before
		// VisibleItems() actually reflects the query. Every case above is
		// an exhaustive, explicit type switch, so any message type not
		// named above — FilterMatchesMsg chief among them, but also
		// whatever else bubbles/list or its FilterInput/paginator produce
		// internally — fell through to a bare "return m, nil" and was
		// silently dropped, no matter which list.Model it belonged to.
		// Confirmed directly: draining a real "/", then "openai" keystroke
		// sequence through an open picker without this case left every
		// item visible regardless of the filter text typed, reproducing
		// exactly the reported symptom. Route to whichever list.Model is
		// currently active: the in-chat overlay picker if one is open
		// (checked first — it can be open regardless of m.screen, since
		// screen only distinguishes the top-level picker/history/chat
		// views, not the chat-scoped overlay), else historyList/agentList
		// depending on m.screen.
		if m.chat != nil && m.chat.picker != nil {
			var cmd tea.Cmd
			*m.chat.picker, cmd = m.chat.picker.Update(msg)
			return m, cmd
		}
		switch m.screen {
		case screenHistory:
			var cmd tea.Cmd
			m.historyList, cmd = m.historyList.Update(msg)
			return m, cmd
		case screenAgentPicker:
			var cmd tea.Cmd
			m.agentList, cmd = m.agentList.Update(msg)
			return m, cmd
		}
		return m, nil
	}
}

// handleKey dispatches a key press to the active screen: global quit
// (ctrl+c) first, then picker navigation/selection or chat-screen handling
// (composer editing, approval-prompt decisions, agent/model switch).
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
			case "ctrl+h":
				// Post-v0.1.0 addendum. Same async-load reasoning as the
				// in-chat ctrl+h (chat.go's handleKey) — see
				// loadHistoryCmd's doc comment.
				return m, loadHistoryCmd(m.svc, m.ctx)
			case "q", "esc":
				return m, tea.Quit
			}
		}
		var cmd tea.Cmd
		m.agentList, cmd = m.agentList.Update(msg)
		return m, cmd

	case screenHistory:
		// Post-v0.1.0 addendum, Design §5's addendum: the top-level
		// counterpart to chatModel's pickerHistory overlay, for resuming a
		// chat before ever picking an agent for a new one.
		if m.historyList.FilterState() != list.Filtering {
			switch msg.String() {
			case "enter":
				item, ok := m.historyList.SelectedItem().(historyPickerItem)
				if !ok {
					return m, nil
				}
				return m, resumeChatCmd(m.svc, m.ctx, item.chatID)
			case "esc", "q":
				m.screen = screenAgentPicker
				return m, nil
			}
		}
		var cmd tea.Cmd
		m.historyList, cmd = m.historyList.Update(msg)
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
// StartChat) and seeds it with its (freshly initialized) Todos
// snapshot. Picking a *specific* prior chat to resume instead — the gap
// this doc comment used to flag — is resumeChatCmd (stream.go, post-v0.1.0
// addendum: ctrl+h/--continue), reachable via screenHistory/chatModel's
// pickerHistory overlay rather than from this screen's agentList.
func (m Model) startChatCmd(agentName string) tea.Cmd {
	svc := m.svc
	ctx := m.ctx
	return func() tea.Msg {
		chatID := newChatID(agentName)
		if _, err := svc.StartChat(ctx, chatID, agentName); err != nil {
			return chatStartedMsg{err: fmt.Errorf("starting chat: %w", err)}
		}
		todos, err := svc.GetTodos(ctx, chatID)
		if err != nil {
			return chatStartedMsg{err: fmt.Errorf("loading todos: %w", err)}
		}
		model, err := svc.GetModel(ctx, chatID)
		if err != nil {
			return chatStartedMsg{err: fmt.Errorf("loading model: %w", err)}
		}
		return chatStartedMsg{chatID: chatID, agentName: agentName, todos: todos, model: model}
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
		view := m.agentList.View()
		if m.historyErr != nil {
			view = errorStyle.Render(fmt.Sprintf("error loading history: %v", m.historyErr)) + "\n\n" + view
		}
		return view
	case screenHistory:
		return m.historyList.View()
	case screenChat:
		if m.chat == nil {
			return "Starting chat..."
		}
		return m.chat.View(m.width, m.height)
	}
	return ""
}
