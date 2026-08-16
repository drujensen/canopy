package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/agent/harness/todo"
	"github.com/microsoft/agent-framework-go/message"

	"github.com/drujensen/canopy/internal/domain/services"
)

// pickerKind identifies which in-chat overlay picker (see chatModel.picker)
// is currently open, so handlePickerKey knows which AgentService setter a
// selection should call.
type pickerKind int

const (
	pickerNone pickerKind = iota
	pickerAgent
	pickerModel
	// pickerSkill identifies the ctrl+s read-only skills browser (post-v0.1.0
	// addendum, Design §3.11/FR19) — unlike pickerAgent/pickerModel, selecting
	// an entry never calls an AgentService mutator; it shows the skill's real
	// Body in the transcript instead (see chatModel.showSkill).
	pickerSkill
	// pickerHistory identifies the ctrl+h chat-history browser (post-v0.1.0
	// addendum, Design §5's addendum) — unlike every other picker here,
	// selecting an entry doesn't mutate *this* chat at all; it resumes a
	// *different* one entirely (resumeChatCmd, stream.go), which
	// Model.Update's existing chatStartedMsg case handles by replacing
	// m.chat outright, the same as picking an agent from the top-level
	// screen does.
	pickerHistory
)

// transcriptEntry is one finished message rendered in the chat transcript —
// either something the user sent, a completed assistant reply (built up
// from streamed chunks, see chatModel.streaming), or a system-style
// informational entry (post-v0.1.0 addendum: the ctrl+s skills browser
// folding a skill's real Body into the transcript so a user can see it
// without asking the model — see showSkill).
type transcriptEntry struct {
	role   message.Role
	text   string
	system bool
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

	// streamCancel cancels the context the turn currently in flight (if any)
	// was started with — set by startTurnCmd (a fresh context.WithCancel
	// derived from the caller's long-lived ctx, one per turn) and called by
	// finishStreaming unconditionally once the turn ends, however it ends
	// (success, error, or a user-initiated "esc" cancellation below), so a
	// canceled turn's derived context is always released rather than
	// accumulating as a child of the program's long-lived context. nil
	// whenever no turn is in flight.
	streamCancel context.CancelFunc

	// spinner animates while streamActive is true (post-v0.1.0 addendum: a
	// visual "still working" indicator — see View's use of it in place of
	// the composer during a turn). Ticked by Model.Update's spinner.TickMsg
	// case (model.go), which only keeps re-arming the tick while
	// streamActive stays true.
	spinner spinner.Model

	// titleAttempted guards generateTitleCmd (post-v0.1.0 addendum: ctrl+h
	// history browser) from firing more than once per chatModel instance —
	// only the completion of this chat's *first* exchange should attempt
	// title generation, whether it succeeds or fails (see
	// AgentService.GenerateChatTitle's doc comment on why a failure is
	// never retried automatically). A resumed chat (ctrl+h/--continue)
	// starts with this already true when it already has a Title, via
	// newChatModel's caller — see Model.Update's chatStartedMsg case.
	titleAttempted bool

	composer textinput.Model
	viewport viewport.Model

	todos []todo.Item
	mode  string

	// model is the currently active model name (post-v0.1.0 addendum,
	// Design §4/FR1 — AgentService.GetModel), shown in the sidebar next to
	// Mode. Seeded at construction and refreshed after every completed turn
	// (RunResult.Model, via handleStreamMsg's streamDoneMsg case) and
	// immediately after a ctrl+o switch (modelChangedMsg, see model.go).
	model string

	// pendingApproval is the tool-approval request currently awaiting a
	// user decision (Design §3.6), or nil when none is pending. While set,
	// the composer does not receive keystrokes — handleKey routes to
	// handleApprovalKey instead.
	pendingApproval     *message.ToolApprovalRequestContent
	pendingApprovalTool string

	// picker, when non-nil, is the in-chat overlay currently shown — the
	// ctrl+a agent-switch, ctrl+o model-switch, or ctrl+s skills-browser
	// picker (post-v0.1.0 addendum, Design §3.4/§4/§5/§3.11), pre-empting the
	// composer/transcript the same way pendingApproval does. pickerKind says
	// which one, so handlePickerKey knows whether a selection calls
	// SetAgent/SetModel or just displays the chosen skill's body.
	picker     *list.Model
	pickerKind pickerKind

	statusErr error

	width, height int
}

// newChatModel constructs a chatModel for a just-started or resumed chat,
// seeded with its current Todos/Mode/Model snapshot (Design §3.7/§3.8,
// post-v0.1.0 Model addendum).
func newChatModel(chatID, agentName string, todos []todo.Item, mode, model string, width, height int) *chatModel {
	composer := textinput.New()
	composer.Placeholder = "Type a message and press enter..."
	composer.Focus()
	composer.CharLimit = 4000

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	c := &chatModel{
		chatID:    chatID,
		agentName: agentName,
		todos:     todos,
		mode:      mode,
		model:     model,
		spinner:   sp,
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
// while pendingApproval is set, overlay-picker navigation while picker is
// set, mode/agent/model-switch or message-submit otherwise, and composer
// text editing as the fallback.
func (c *chatModel) handleKey(msg tea.KeyMsg, svc *services.AgentService, ctx context.Context) tea.Cmd {
	if c.pendingApproval != nil {
		return c.handleApprovalKey(msg, svc, ctx)
	}
	if c.picker != nil {
		return c.handlePickerKey(msg, svc, ctx)
	}

	switch msg.String() {
	case "esc":
		// Cancels the in-flight turn (post-v0.1.0 addendum), returning
		// control to the composer — see cancelTurn's doc comment for why
		// this only needs to call streamCancel and not touch streamActive/
		// statusErr/etc. directly: handleStreamMsg's streamErrMsg case,
		// reached the moment the canceled turn's context.Canceled error
		// arrives on the same channel every other terminal event already
		// does, is what actually resets state. A no-op when no turn is
		// active — esc has no other meaning on this screen.
		c.cancelTurn()
		return nil
	case "ctrl+p":
		return c.toggleModeCmd(svc, ctx)
	case "ctrl+a":
		// Same guard streaming-sensitive actions elsewhere in this file
		// apply (see "enter" below): don't open the agent-switch overlay
		// mid-turn. Not-nil pendingApproval is already excluded above.
		if c.streamActive {
			return nil
		}
		c.openPicker(svc, pickerAgent)
		return nil
	case "ctrl+o":
		if c.streamActive {
			return nil
		}
		c.openPicker(svc, pickerModel)
		return nil
	case "ctrl+s":
		// Same guard as ctrl+a/ctrl+o: don't open the skills browser
		// mid-turn. It's read-only (no AgentService mutator involved), but
		// staying consistent with the other overlays keeps the composer's
		// "no overlay while streaming" invariant simple and uniform.
		if c.streamActive {
			return nil
		}
		c.openSkillPicker(svc)
		return nil
	case "ctrl+h":
		// Same guard as the other overlays, post-v0.1.0 addendum. Unlike
		// them, this can't populate c.picker synchronously (loadHistoryCmd's
		// doc comment explains why — real disk I/O) — Model.Update's
		// historyLoadedMsg case is what actually opens the overlay, once
		// this Cmd's result comes back.
		if c.streamActive {
			return nil
		}
		return loadHistoryCmd(svc, ctx)
	case "ctrl+n":
		// Guarded the same way ctrl+a/ctrl+o/enter are: starting a new chat
		// must not silently abandon a turn that's actively streaming. A
		// pending approval is already excluded above (pendingApproval != nil
		// routes to handleApprovalKey instead, which has no ctrl+n case).
		if c.streamActive {
			return nil
		}
		return c.startNewChatCmd(svc, ctx)
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
		return tea.Batch(c.startTurnCmd(svc, ctx, []*message.Message{message.NewText(text)}), c.spinner.Tick)
	}

	if c.streamActive {
		return nil
	}
	var cmd tea.Cmd
	c.composer, cmd = c.composer.Update(msg)
	return cmd
}

// openPicker builds and shows one of the in-chat overlay pickers (ctrl+a
// switch-agent, ctrl+o switch-model — post-v0.1.0 addendum) from
// AgentService.ListAgents()/ListModelSummaries(). Both are pure, in-memory
// reads (no I/O, unlike everything else this file calls through svc), so
// this runs synchronously inside handleKey rather than round-tripping
// through a tea.Cmd/tea.Msg the way starting a turn or toggling mode do.
//
// The model picker (post-v0.1.0 addendum) uses ListModelSummaries rather
// than ListModels/newPickerList's plain-name pickerItem, so each entry can
// show its per-million-token request/response cost (modelPickerItem) the
// same way the ctrl+s skills browser shows each skill's description.
func (c *chatModel) openPicker(svc *services.AgentService, kind pickerKind) {
	if kind == pickerAgent {
		l := newPickerList(svc.ListAgents(), "Select an agent", c.width-sidebarWidth, c.height-4)
		c.picker = &l
		c.pickerKind = kind
		return
	}

	summaries := svc.ListModelSummaries()
	items := make([]list.Item, 0, len(summaries))
	for _, m := range summaries {
		items = append(items, modelPickerItem{
			name:                 m.Name,
			inputCostPerMillion:  m.InputCostPerMillionTokens,
			outputCostPerMillion: m.OutputCostPerMillionTokens,
		})
	}
	l := list.New(items, list.NewDefaultDelegate(), c.width-sidebarWidth, c.height-4)
	l.Title = "Select a model"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(true)
	c.picker = &l
	c.pickerKind = kind
}

// openSkillPicker builds and shows the read-only ctrl+s skills-browser
// overlay (post-v0.1.0 addendum, Design §3.11/FR19), sourced from
// AgentService.ListSkills() — a pure, in-memory read like openPicker's own
// ListAgents()/ListModels() calls, so this too runs synchronously inside
// handleKey.
//
// Unlike openPicker's two overlays, zero loaded skills is a real,
// expected case (not every project configures any), and silently doing
// nothing would leave the user wondering whether the keybinding even
// fired. So that case is handled directly here rather than opening an
// empty, useless list: a brief system-style transcript entry says so, with
// no overlay shown at all.
func (c *chatModel) openSkillPicker(svc *services.AgentService) {
	skills := svc.ListSkills()
	if len(skills) == 0 {
		c.transcript = append(c.transcript, transcriptEntry{system: true, text: "No skills configured."})
		c.refreshViewport()
		return
	}

	items := make([]list.Item, 0, len(skills))
	for _, sk := range skills {
		items = append(items, skillPickerItem{name: sk.Name, description: sk.Description})
	}
	l := list.New(items, list.NewDefaultDelegate(), c.width-sidebarWidth, c.height-4)
	l.Title = "Skills"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(true)
	c.picker = &l
	c.pickerKind = pickerSkill
}

// handlePickerKey routes a key press to whichever in-chat overlay picker is
// currently open (see openPicker/openSkillPicker): "enter" selects the
// highlighted item — dispatching to SetAgent/SetModel for the ctrl+a/ctrl+o
// overlays, or displaying the chosen skill's body (showSkill) for the
// ctrl+s browser, which never mutates the chat — "esc" cancels back to the
// chat screen with no change, and any other key is forwarded to the
// underlying bubbles/list.Model for navigation/filtering — mirroring
// model.go's own screenAgentPicker case, reused here for the chat-scoped
// overlay instead of a separate top-level screen.
func (c *chatModel) handlePickerKey(msg tea.KeyMsg, svc *services.AgentService, ctx context.Context) tea.Cmd {
	if c.picker.FilterState() != list.Filtering {
		switch msg.String() {
		case "enter":
			kind := c.pickerKind
			if kind == pickerSkill {
				item, ok := c.picker.SelectedItem().(skillPickerItem)
				c.picker = nil
				c.pickerKind = pickerNone
				if !ok {
					return nil
				}
				c.showSkill(svc, item.name)
				return nil
			}
			if kind == pickerModel {
				item, ok := c.picker.SelectedItem().(modelPickerItem)
				c.picker = nil
				c.pickerKind = pickerNone
				if !ok {
					return nil
				}
				return c.setModelCmd(svc, ctx, item.name)
			}
			if kind == pickerHistory {
				item, ok := c.picker.SelectedItem().(historyPickerItem)
				c.picker = nil
				c.pickerKind = pickerNone
				if !ok {
					return nil
				}
				return resumeChatCmd(svc, ctx, item.chatID)
			}
			item, ok := c.picker.SelectedItem().(pickerItem)
			c.picker = nil
			c.pickerKind = pickerNone
			if !ok {
				return nil
			}
			return c.setAgentCmd(svc, ctx, item.name)
		case "esc":
			c.picker = nil
			c.pickerKind = pickerNone
			return nil
		}
	}
	var cmd tea.Cmd
	*c.picker, cmd = c.picker.Update(msg)
	return cmd
}

// showSkill folds skillName's full Body into the transcript as a
// system-style informational entry (post-v0.1.0 addendum, Design
// §3.11/FR19's ctrl+s browser) — the same content the model-facing Skill
// tool (level 2) returns, shown here directly to the user so they don't
// need to ask the model to find out what a skill does. GetSkillBody is a
// pure in-memory map lookup (no I/O), so — like openPicker/openSkillPicker
// — this runs synchronously rather than round-tripping through a tea.Cmd.
func (c *chatModel) showSkill(svc *services.AgentService, skillName string) {
	body, err := svc.GetSkillBody(skillName)
	if err != nil {
		c.transcript = append(c.transcript, transcriptEntry{system: true, text: fmt.Sprintf("error: %v", err)})
		c.refreshViewport()
		return
	}
	c.transcript = append(c.transcript, transcriptEntry{system: true, text: fmt.Sprintf("Skill: %s\n\n%s", skillName, body)})
	c.refreshViewport()
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
	return tea.Batch(c.startTurnCmd(svc, ctx, []*message.Message{approvalMsg}), c.spinner.Tick)
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

// setModelCmd calls AgentService.SetModel for chosen (a ModelConfig.Name
// from the ctrl+o picker's list, post-v0.1.0 addendum) and returns a
// modelChangedMsg Model.Update applies to refresh the sidebar — mirroring
// toggleModeCmd's modeChangedMsg pattern.
func (c *chatModel) setModelCmd(svc *services.AgentService, ctx context.Context, chosen string) tea.Cmd {
	chatID := c.chatID
	return func() tea.Msg {
		if err := svc.SetModel(ctx, chatID, chosen); err != nil {
			return streamErrMsg{err: fmt.Errorf("switching model: %w", err)}
		}
		return modelChangedMsg{model: chosen}
	}
}

// setAgentCmd calls AgentService.SetAgent for chosen (an agent name from the
// ctrl+a picker's list, post-v0.1.0 addendum) and returns an agentChangedMsg
// Model.Update applies to refresh the sidebar. Unlike Model.startChatCmd
// (the top-level picker screen's "start a brand-new chat" path), this never
// creates a new chat — the same chat ID and history carry over, only which
// agent drives the next turn changes (see AgentService.SetAgent's doc
// comment).
func (c *chatModel) setAgentCmd(svc *services.AgentService, ctx context.Context, chosen string) tea.Cmd {
	chatID := c.chatID
	return func() tea.Msg {
		if err := svc.SetAgent(ctx, chatID, chosen); err != nil {
			return streamErrMsg{err: fmt.Errorf("switching agent: %w", err)}
		}
		return agentChangedMsg{agentName: chosen}
	}
}

// startNewChatCmd starts a genuinely new chat (ctrl+n, post-v0.1.0
// addendum) bound to the *same* agent the current chat is using
// (c.agentName) — if the user also wants a different agent for the new
// chat, ctrl+a already handles that in-place, the same as for an existing
// chat; ctrl+n does not force the user back through the top-level agent
// picker. The new chat's ID is minted with newChatID (model.go), the exact
// same scheme the top-level picker's own startChatCmd uses, so there is
// only ever one ID-generation scheme in this codebase.
//
// This produces the same chatStartedMsg the top-level picker's
// startChatCmd does, deliberately: Model.Update's chatStartedMsg case
// already does exactly the reset ctrl+n needs — it discards the old
// *chatModel entirely and constructs a brand-new one via newChatModel,
// which zero-values transcript/streaming/pendingApproval and seeds
// todos/mode/model fresh from the new (empty) chat's own state. Reusing
// that message means ctrl+n's "reset" is not a second, hand-rolled reset
// code path that could drift from what starting a chat from the picker
// screen already guarantees.
func (c *chatModel) startNewChatCmd(svc *services.AgentService, ctx context.Context) tea.Cmd {
	agentName := c.agentName
	return func() tea.Msg {
		chatID := newChatID(agentName)
		if _, err := svc.StartChat(ctx, chatID, agentName); err != nil {
			return chatStartedMsg{err: fmt.Errorf("starting new chat: %w", err)}
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

// startTurnCmd marks the chat as actively streaming and delegates to
// startTurn (stream.go) to drive the turn against svc via
// AgentService.RunMessagesStream.
//
// statusErr is cleared here (post-v0.1.0 addendum: "an error should only
// show for one turn, clearing on the next message sent or received") —
// starting a new turn is the one place both halves of that are guaranteed
// to happen, since every new turn is either about to send a fresh message
// or about to receive a fresh response. This is the only place statusErr is
// ever cleared; a config action that can itself streamErrMsg (toggleModeCmd/
// setModelCmd/setAgentCmd) does not start a turn and so does not clear a
// pending turn error.
//
// ctx is not passed to startTurn directly: a fresh context.WithCancel
// derived from it is, so a later "esc" (cancelTurn) has something to
// cancel that affects only this one turn, not svc's other callers or the
// program's long-lived ctx itself. finishStreaming always calls the
// resulting cancel func once the turn ends, whichever way, so this never
// leaks a canceled-but-uncollected child context.
//
// Deliberately returns the bare stream-wait Cmd, not batched with
// c.spinner.Tick: the tests exercising this exact function directly
// (stream_leak_test.go, mcp_stream_test.go) feed its result straight into
// drainCmd, a hand-rolled synchronous test harness that only understands
// this package's own streamChunkMsg/streamDoneMsg/streamErrMsg — a
// tea.BatchMsg (what tea.Batch's Cmd produces) isn't one of those, so
// batching here would make drainCmd return after the very first tick
// without ever reading the real stream, silently orphaning startTurn's
// forwarding goroutine (exactly the leak stream_leak_test.go exists to
// catch). handleKey's "enter" case and respondApproval — the two real,
// interactive entry points into this function — batch the spinner tick in
// themselves instead, where the real Bubble Tea runtime (which does
// understand tea.BatchMsg) is what ends up running it.
func (c *chatModel) startTurnCmd(svc *services.AgentService, ctx context.Context, msgs []*message.Message) tea.Cmd {
	c.streamActive = true
	c.streaming.Reset()
	c.statusErr = nil
	turnCtx, cancel := context.WithCancel(ctx)
	c.streamCancel = cancel
	ch, cmd := startTurn(turnCtx, svc, c.chatID, msgs)
	c.streamCh = ch
	return cmd
}

// cancelTurn cancels the currently in-flight turn's context, if any (a
// no-op when none is active). It deliberately does nothing else: the
// canceled context.Canceled error still has to travel back through
// AgentService.RunMessagesStream's stream/finalize and arrive as this same
// turn's terminal streamErrMsg — see handleStreamMsg's streamErrMsg case,
// which is what actually resets streamActive/streamCh/streamCancel via
// finishStreaming and folds a "Cancelled." note into the transcript instead
// of a raw error. Resetting state here too, ahead of that event, would let
// the user start a second turn while the first turn's goroutine is still
// running and still holds the only reference to the old streamCh — a
// message send on that now-unwatched, unbuffered channel would block
// forever (see stream_leak_test.go's goroutine-leak coverage for this
// exact channel-ownership contract).
func (c *chatModel) cancelTurn() {
	if c.streamCancel != nil {
		c.streamCancel()
	}
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
			c.model = msg.result.Model
		}
		c.refreshViewport()
		return nil
	case streamErrMsg:
		c.finishStreaming()
		if errors.Is(msg.err, context.Canceled) {
			// A user-initiated "esc" (cancelTurn) rather than a genuine
			// failure — a "Cancelled." system note reads as an
			// acknowledgment of what the user just did, not an error worth
			// pinning under the composer the way a real provider failure
			// is (statusErr stays nil).
			c.transcript = append(c.transcript, transcriptEntry{system: true, text: "Cancelled."})
		} else {
			c.statusErr = msg.err
		}
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
// Always releases streamCancel's context, however the turn ended (success,
// error, or a user-initiated cancellation) — see startTurnCmd's doc comment
// on why leaving that uncalled would leak a canceled-but-uncollected child
// context.
func (c *chatModel) finishStreaming() {
	if c.streaming.Len() > 0 {
		c.transcript = append(c.transcript, transcriptEntry{role: message.RoleAssistant, text: c.streaming.String()})
	}
	c.streaming.Reset()
	c.streamActive = false
	c.streamCh = nil
	if c.streamCancel != nil {
		c.streamCancel()
		c.streamCancel = nil
	}
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
	if b.Len() == 0 {
		// A brand-new (or freshly ctrl+n'd) chat has nothing in transcript
		// and nothing streaming yet — rather than an empty scrollback,
		// render a purely cosmetic greeting to avoid dead space. This is
		// never appended to c.transcript, so it's never part of what a turn
		// sends to the model or what gets persisted — it exists only in
		// this one rendered frame, and the very next refreshViewport call
		// that has real content (the first user message, via handleKey's
		// "enter" case) replaces it outright rather than needing to be
		// explicitly cleared anywhere.
		b.WriteString(greetingStyle.Render(defaultGreeting))
	}
	c.viewport.SetContent(b.String())
	c.viewport.GotoBottom()
}

// defaultGreeting is the placeholder shown in place of an empty transcript
// (refreshViewport) — purely cosmetic, never part of chat history.
const defaultGreeting = "How can I help you today?"

// reconstructTranscript converts a resumed chat's persisted
// []*message.Message (post-v0.1.0 addendum: ctrl+h/--continue,
// resumeChatCmd) into displayable transcriptEntry values, so Model.Update's
// chatStartedMsg case can seed a resumed *chatModel's transcript the same
// way a live session builds it up turn by turn.
//
// Only user/assistant messages with non-empty rendered text are included —
// deliberately mirroring what a *live* session's transcript ever contains
// in the first place: applyUpdate only ever appends TextContent to
// c.streaming (folded into a transcriptEntry by finishStreaming), never a
// raw tool-result or empty tool-call-only message. A Tool-role result
// message, or a message whose only content is a function call with no
// accompanying text, is skipped rather than rendered as a confusing empty
// entry. This is a known, deliberate simplification: full historical
// tool-call/approval-prompt formatting isn't reconstructed on resume, only
// the conversational text — resuming a chat mid-tool-call is not a
// supported scenario (the pending approval itself doesn't survive a
// restart either, per Design §3.9's session-state contract).
func reconstructTranscript(messages []*message.Message) []transcriptEntry {
	entries := make([]transcriptEntry, 0, len(messages))
	for _, m := range messages {
		if m == nil {
			continue
		}
		if m.Role != message.RoleUser && m.Role != message.RoleAssistant {
			continue
		}
		text := strings.TrimSpace(m.String())
		if text == "" {
			continue
		}
		entries = append(entries, transcriptEntry{role: m.Role, text: text})
	}
	return entries
}

func renderEntry(e transcriptEntry) string {
	if e.system {
		return systemLabelStyle.Render("system") + "\n" + e.text
	}
	label := userLabelStyle.Render("you")
	if e.role == message.RoleAssistant {
		label = assistantLabelStyle.Render("assistant")
	}
	return label + "\n" + e.text
}

// View renders the chat screen: a sidebar (agent + mode + model + todos)
// beside either an open overlay picker (ctrl+a/ctrl+o, post-v0.1.0 addendum)
// or the transcript with either the composer or a pending approval prompt at
// the bottom, plus a status line for the last stream error, if any.
func (c *chatModel) View(width, height int) string {
	sidebar := c.renderSidebar()

	if c.picker != nil {
		return lipgloss.JoinHorizontal(lipgloss.Top, sidebar, c.picker.View())
	}

	var bottom string
	switch {
	case c.pendingApproval != nil:
		bottom = c.renderApprovalPrompt()
	case c.streamActive:
		// A visual "still working" indicator (post-v0.1.0 addendum) in
		// place of the composer, which doesn't accept input while
		// streamActive anyway (handleKey's own guard) — also doubles as
		// the discoverability hint for "esc" now canceling the turn.
		bottom = c.spinner.View() + " Thinking... (esc to cancel)"
	default:
		bottom = c.composer.View()
	}

	main := lipgloss.JoinVertical(lipgloss.Left, c.viewport.View(), bottom)
	if c.statusErr != nil {
		main = lipgloss.JoinVertical(lipgloss.Left, main, errorStyle.Render(c.renderStatusErrLine()))
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, sidebar, main)
}

// renderSidebar renders the agent/mode/model indicators (Design §3.8,
// post-v0.1.0 §3.4/§4 addendum) and the live todo panel (Design §3.7).
func (c *chatModel) renderSidebar() string {
	var b strings.Builder
	b.WriteString(modeStyle.Render("Agent: " + c.agentName))
	b.WriteString("\n(ctrl+a to switch)\n")
	b.WriteString(modeStyle.Render("Mode: " + c.mode))
	b.WriteString("\n(ctrl+p to switch)\n")
	b.WriteString(modeStyle.Render("Model: " + c.model))
	b.WriteString("\n(ctrl+o to switch)\n")
	b.WriteString("(ctrl+s to view skills)\n")
	b.WriteString("(ctrl+h for history)\n")
	b.WriteString("(ctrl+n for new chat)\n\n")
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
