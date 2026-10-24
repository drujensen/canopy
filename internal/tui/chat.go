package tui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aymanbagabas/go-osc52/v2"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/agent/harness/todo"
	"github.com/microsoft/agent-framework-go/message"

	"github.com/drujensen/canopy/internal/domain/services"
	"github.com/drujensen/canopy/internal/impl/tools"
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
// either something the user sent (text), a completed assistant reply
// (segments — built up from streamed chunks, see chatModel.streaming), or
// a system-style informational entry (text, system: true — post-v0.1.0
// addendum: the ctrl+s skills browser folding a skill's real Body into the
// transcript so a user can see it without asking the model — see
// showSkill).
type transcriptEntry struct {
	role     message.Role
	text     string
	segments []textSegment
	system   bool
}

// segmentKind distinguishes how one piece of an assistant reply renders
// (post-v0.1.0 addendum answering "it's hard to tell tool output from the
// actual response"): the model's own prose gets a background-highlighted
// block, same treatment as a user message but a different shade
// (assistantMessageStyle); a tool call/result/error is a dim, indented,
// no-background aside instead — Claude Code's own "● Bash(...)" / "⎿ ..."
// convention — so the two never blend together even interleaved within
// one reply.
type segmentKind int

const (
	segmentProse segmentKind = iota
	segmentToolCall
	segmentToolResult
	segmentToolError
)

// textSegment is one piece of an assistant reply tagged with how it
// should render — see segmentKind.
type textSegment struct {
	kind segmentKind
	text string
}

// renderedLine is one physical row of the rendered transcript — already
// word-wrapped and padded to the viewport's content width, tagged with the
// lipgloss style that applies to it (post-v0.1.0 addendum: mouse-driven
// text selection). Flattening the transcript to this line-granular form
// (buildContentLines) rather than rendering each entry straight to a
// single multi-line string is what makes selection possible at all: a
// mouse coordinate maps directly to one (line index, rune column) pair,
// and highlighting a selection is just re-rendering the affected lines
// with an extra Reverse(true) on the selected rune range — see
// renderLine.
type renderedLine struct {
	text  string
	style lipgloss.Style
}

// selPoint identifies one end of a mouse-drag text selection as a
// position in c.contentLines: line is an index into that slice; col is a
// rune index into that line's (already-padded) text.
type selPoint struct {
	line, col int
}

// clearCopyToastMsg clears chatModel.copyToast a couple of seconds after a
// mouse-driven copy (copySelectionCmd's tea.Tick) — the same
// "acknowledge, then fade" convention Claude Code's own copy callout uses.
type clearCopyToastMsg struct{}

// chatModel is the chat screen (Design §5): a scrolling transcript fed by
// AgentService's streaming entry points, a composer, and a sidebar showing
// the live todo panel (Design §3.7).
type chatModel struct {
	chatID    string
	agentName string

	transcript []transcriptEntry

	// streaming accumulates the in-flight assistant reply's segments
	// (text/toolCall/toolResult/toolError — see segmentKind) as
	// streamChunkMsg values arrive, before it's folded into transcript by
	// finishStreaming once the turn's stream is done. appendStreamingText
	// merges consecutive text fragments into one prose segment rather than
	// growing one segment per fragment (streamed text typically arrives a
	// token at a time).
	streaming    []textSegment
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

	composer textarea.Model
	viewport viewport.Model

	// sidebarHidden toggles the agent/model/todo sidebar off (ctrl+b) for
	// anyone who just wants the extra width — the transcript/composer
	// simply take the full width while hidden; see mainWidth. Originally
	// added because a terminal's own line-based text selection had no way
	// to skip over the sidebar's columns; superseded for that specific
	// purpose by this package's own mouse-driven selection (hitTest/
	// mainColOffset), which is scoped to the main content pane regardless
	// of whether the sidebar is shown.
	sidebarHidden bool

	// pendingToolCalls maps a tool call's CallID to its tool Name, recorded
	// by applyUpdate's *message.FunctionCallContent case and consumed by
	// its *message.FunctionResultContent case (post-v0.1.0 addendum: richer
	// tool-call/result rendering) — the two arrive as separate streamed
	// updates, call first, so the result (which only carries a CallID, not
	// a Name) needs this to know which tool it's reporting on. Cleared per
	// entry as each result arrives; never grows unbounded across a turn.
	pendingToolCalls map[string]string

	todos []todo.Item

	// model is the currently active model name (post-v0.1.0 addendum,
	// Design §4/FR1 — AgentService.GetModel), shown in the sidebar.
	// Seeded at construction and refreshed after every completed turn
	// (RunResult.Model, via handleStreamMsg's streamDoneMsg case) and
	// immediately after a ctrl+o switch (modelChangedMsg, see model.go).
	model string

	// picker, when non-nil, is the in-chat overlay currently shown — the
	// ctrl+a agent-switch, ctrl+o model-switch, or ctrl+s skills-browser
	// picker (post-v0.1.0 addendum, Design §3.4/§4/§5/§3.11), pre-empting the
	// composer/transcript. pickerKind says which one, so handlePickerKey
	// knows whether a selection calls SetAgent/SetModel or just displays the
	// chosen skill's body.
	picker     *list.Model
	pickerKind pickerKind

	statusErr error

	width, height int

	// contentLines is the transcript's current flattened, line-granular
	// render (buildContentLines) — the source of truth refreshViewport's
	// SetContent is built from, and what hitTest/selectedText read against
	// for mouse-driven text selection (post-v0.1.0 addendum). Rebuilt
	// whenever the transcript actually changes; untouched by a selection
	// drag itself, which only changes *which* existing lines render
	// highlighted (rerenderContent).
	contentLines []renderedLine

	// selecting is true from a left-button mouse press until its matching
	// release — see handleMouse. selAnchor is where the drag started;
	// selCurrent is the drag's current (or final, once released) point.
	// hasSel is true once a drag has actually moved to a second point (a
	// plain click with no drag never becomes a selection to render or
	// copy) and stays true after release so the highlight persists until a
	// new press clears it, the same "stays highlighted until you click
	// elsewhere" convention every text editor uses.
	selecting             bool
	selAnchor, selCurrent selPoint
	hasSel                bool

	// mainColOffset is the screen column the main content pane (transcript/
	// composer) starts at — 0 when the sidebar is hidden, otherwise the
	// sidebar's own actual rendered width (border/padding included).
	// Recomputed by View on every render (the one place that already knows
	// it, having just measured the sidebar to lipgloss.JoinHorizontal it)
	// so hitTest can translate a raw mouse X coordinate into a column
	// within the transcript without re-rendering the sidebar itself.
	mainColOffset int

	// copyToast is a transient "Copied N lines" acknowledgment shown below
	// the composer after a mouse-driven copy (post-v0.1.0 addendum,
	// matching the callout Claude Code itself shows), cleared a couple of
	// seconds later by clearCopyToastMsg. Takes the same borrowed row
	// statusErr does (viewportHeight) — the two are never expected
	// simultaneously enough to need two reserved rows.
	copyToast string

	// copyToClipboard sends text to the system clipboard via an OSC 52
	// terminal escape sequence (copySelectionCmd) — a field rather than a
	// hardcoded call so tests can substitute a recording stub instead of
	// writing a real escape sequence to os.Stdout. Always non-nil: set to
	// the real OSC 52 writer by newChatModel.
	copyToClipboard func(string)
}

// composerKeyMap is textarea.DefaultKeyMap with InsertNewline rebound off
// "enter" (handleKey's own "enter" case submits the turn; the default
// InsertNewline binding of "enter"/"ctrl+m" would otherwise fight it for
// the same keypress — textarea.Update is only ever reached for keys
// handleKey's switch doesn't already claim) and onto "shift+enter"/"ctrl+j"
// instead, per the user-facing multi-line-input request. "ctrl+j" is the
// reliable one: it's the literal line-feed byte (0x0A), distinct from
// "enter"'s carriage-return byte (0x0D) at the wire level, so every
// terminal reports it unambiguously. "shift+enter" is wired too, for
// terminals that negotiate a distinct escape sequence for it, but Bubble
// Tea v1.3.10 has no built-in parsing for one — most terminals simply
// report plain "enter" for that keypress, so ctrl+j is what's documented
// to the user (composer's Placeholder) as the one that always works.
var composerKeyMap = func() textarea.KeyMap {
	km := textarea.DefaultKeyMap
	km.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "ctrl+j"), key.WithHelp("ctrl+j", "insert newline"))
	return km
}()

// newChatModel constructs a chatModel for a just-started or resumed chat,
// seeded with its current Todos/Model snapshot (Design §3.7,
// post-v0.1.0 Model addendum).
func newChatModel(chatID, agentName string, todos []todo.Item, model string, width, height int) *chatModel {
	composer := textarea.New()
	composer.Placeholder = "Type a message (ctrl+j for a new line) and press enter..."
	composer.ShowLineNumbers = false
	composer.Prompt = ""
	composer.KeyMap = composerKeyMap
	composer.Focus()
	composer.CharLimit = 4000

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	c := &chatModel{
		chatID:          chatID,
		agentName:       agentName,
		todos:           todos,
		model:           model,
		spinner:         sp,
		composer:        composer,
		viewport:        viewport.New(width, height),
		copyToClipboard: writeOSC52ToStdout,
	}
	c.resize(width, height)
	return c
}

// writeOSC52ToStdout is copyToClipboard's real implementation: writes text
// to the system clipboard via an OSC 52 terminal escape sequence, wrapped
// for tmux's own passthrough requirement when running inside one (tmux
// swallows an unwrapped OSC 52 sequence from a pane unless
// `set -g allow-passthrough on` is set — wrapping it removes that
// dependency on the user's tmux config). Writes through syncStdout
// (output.go), not raw os.Stdout directly — see stdoutMu's doc comment for
// why a direct write here used to silently corrupt the clipboard on some
// terminals.
func writeOSC52ToStdout(text string) {
	seq := osc52.New(text)
	if os.Getenv("TMUX") != "" {
		seq = seq.Tmux()
	}
	_, _ = seq.WriteTo(syncStdout{})
}

const (
	sidebarWidth  = 28
	composerLines = 3
)

// mainWidth returns how many columns the transcript/composer get: the full
// terminal width when the sidebar is hidden (ctrl+b) or too narrow a
// terminal to spare sidebarWidth columns for it, otherwise width minus the
// sidebar.
func (c *chatModel) mainWidth() int {
	if c.sidebarHidden {
		return c.width
	}
	w := c.width - sidebarWidth
	if w < 10 {
		return c.width
	}
	return w
}

// viewportHeight returns how many rows the transcript viewport itself
// gets: the whole chat screen height minus the composer/spinner bar, minus
// one more row while a status error line or a copy-toast (post-v0.1.0
// addendum) is currently being shown below it (View appends whichever one
// dynamically, outside the viewport) — borrowed only while one is actually
// displayed, rather than permanently reserved, so the common case (neither)
// renders using the terminal's full height instead of leaving rows
// permanently blank at the bottom.
func (c *chatModel) viewportHeight() int {
	h := c.height - composerLines
	if c.statusErr != nil || c.copyToast != "" {
		h--
	}
	if h < 1 {
		h = 1
	}
	return h
}

// resize adjusts the viewport/composer to fit width x height, reserving
// sidebarWidth columns for the todo sidebar (unless hidden, see
// mainWidth/sidebarHidden) and composerLines rows for the composer — see
// viewportHeight for the rest of the row budget.
func (c *chatModel) resize(width, height int) {
	c.width, c.height = width, height
	mainWidth := c.mainWidth()
	c.viewport.Width = mainWidth
	c.viewport.Height = c.viewportHeight()
	c.composer.SetWidth(mainWidth - 2)
	c.composer.SetHeight(composerLines)
	c.refreshViewport()
}

// handleKey processes a key press for the chat screen: overlay-picker
// navigation while picker is set, agent/model-switch or message-submit
// otherwise, and composer text editing as the fallback.
func (c *chatModel) handleKey(msg tea.KeyMsg, svc *services.AgentService, ctx context.Context) tea.Cmd {
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
	case "pgup", "ctrl+up":
		// Scrolls the transcript back through earlier history. Deliberately
		// not forwarded to the composer at all (unlike the default fallthrough
		// below) — none of these are bound to anything in the composer's own
		// keymap, so there's no ambiguity to guard against. Works regardless
		// of streamActive: scrolling back to read while a turn is still
		// running is a reasonable thing to want. "ctrl+up" is a second
		// binding for the same action (post-v0.1.0 addendum) — most Apple
		// keyboards have no physical PgUp/PgDn keys at all (fn+up/fn+down is
		// macOS's own translation to a real Page Up/Page Down keystroke,
		// which already worked before this addendum; ctrl+up/ctrl+down needs
		// no fn key and reaches the same action).
		c.viewport.PageUp()
		return nil
	case "pgdown", "ctrl+down":
		c.viewport.PageDown()
		return nil
	case "ctrl+b":
		// Toggles the sidebar (post-v0.1.0 addendum), for anyone who just
		// wants the extra width — no longer required for clean mouse
		// selection the way it originally was, since selection is now
		// scoped to the main content pane's own columns regardless (see
		// hitTest/mainColOffset). "ctrl+b" is otherwise only a redundant
		// alt-binding for "left" in the composer's own keymap
		// (textarea.DefaultKeyMap's CharacterBackward), so claiming it here
		// loses no composer functionality — "left" still moves the cursor
		// identically. Works regardless of streamActive, same as pgup/pgdown.
		c.sidebarHidden = !c.sidebarHidden
		c.resize(c.width, c.height)
		return nil
	case "ctrl+a":
		// Same guard streaming-sensitive actions elsewhere in this file
		// apply (see "enter" below): don't open the agent-switch overlay
		// mid-turn.
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
		// must not silently abandon a turn that's actively streaming.
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
// through a tea.Cmd/tea.Msg the way starting a turn does.
//
// The model picker (post-v0.1.0 addendum) uses ListModelSummaries rather
// than ListModels/newPickerList's plain-name pickerItem, so each entry can
// show its per-million-token request/response cost (modelPickerItem) the
// same way the ctrl+s skills browser shows each skill's description.
func (c *chatModel) openPicker(svc *services.AgentService, kind pickerKind) {
	if kind == pickerAgent {
		l := newPickerList(svc.ListAgents(), "Select an agent", c.mainWidth(), c.height-4)
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
	l := list.New(items, list.NewDefaultDelegate(), c.mainWidth(), c.height-4)
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
	l := list.New(items, list.NewDefaultDelegate(), c.mainWidth(), c.height-4)
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

// setModelCmd calls AgentService.SetModel for chosen (a ModelConfig.Name
// from the ctrl+o picker's list, post-v0.1.0 addendum) and returns a
// modelChangedMsg Model.Update applies to refresh the sidebar.
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
// which zero-values transcript/streaming and seeds
// todos/model fresh from the new (empty) chat's own state. Reusing
// that message means ctrl+n's "reset" is not a second, hand-rolled reset
// code path that could drift from what starting a chat from the picker
// screen already guarantees.
func (c *chatModel) startNewChatCmd(svc *services.AgentService, ctx context.Context) tea.Cmd {
	agentName := c.agentName
	previousModel := c.model
	return func() tea.Msg {
		chatID := newChatID(agentName)
		if _, err := svc.StartChat(ctx, chatID, agentName); err != nil {
			return chatStartedMsg{err: fmt.Errorf("starting new chat: %w", err)}
		}
		// Carry over the old chat's active model the same way agentName is
		// already carried over (post-v0.1.0 bugfix): without this, a fresh
		// chat's ModelOverride starts empty and resolveModelName falls back
		// to AgentServiceConfig.DefaultModel — the model last used as of
		// process *startup*, not whatever the user had actually switched to
		// via ctrl+o during this running session. SetModel here makes ctrl+n
		// behave consistently with ctrl+n's own agent carry-over, and also
		// records this as the new last-used model (AgentService.SetModel's
		// existing RecordLastModel side effect), so a following restart
		// picks up the same value too.
		if err := svc.SetModel(ctx, chatID, previousModel); err != nil {
			return chatStartedMsg{err: fmt.Errorf("carrying over model: %w", err)}
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

// startTurnCmd marks the chat as actively streaming and delegates to
// startTurn (stream.go) to drive the turn against svc via
// AgentService.RunMessagesStream.
//
// statusErr is cleared here (post-v0.1.0 addendum: "an error should only
// show for one turn, clearing on the next message sent or received") —
// starting a new turn is the one place both halves of that are guaranteed
// to happen, since every new turn is either about to send a fresh message
// or about to receive a fresh response. This is the only place statusErr is
// ever cleared; a config action that can itself streamErrMsg (setModelCmd/
// setAgentCmd) does not start a turn and so does not clear a
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
// catch). handleKey's "enter" case — the real entry point into this
// function — batches the spinner tick in itself instead, where the real
// Bubble Tea runtime (which does understand tea.BatchMsg) is what ends up
// running it.
func (c *chatModel) startTurnCmd(svc *services.AgentService, ctx context.Context, msgs []*message.Message) tea.Cmd {
	c.streamActive = true
	c.streaming = nil
	c.statusErr = nil
	// Clearing statusErr frees up the row viewportHeight was borrowing for
	// it (if any was showing) — refreshViewport picks that up immediately
	// rather than leaving the viewport one row short until the turn's first
	// chunk arrives and refreshes it anyway.
	c.refreshViewport()
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

// handleMouse applies one tea.MouseMsg to the chat's mouse-driven text
// selection (post-v0.1.0 addendum): a left-button press starts a fresh
// selection (clearing whatever was selected before) at the clicked
// position; motion while a press is active extends it to wherever the
// drag currently is; release commits it and, if the drag actually moved
// (a plain click with no drag selects nothing), copies the selected text
// to the system clipboard. model.go's Update routes every non-wheel
// MouseMsg here — wheel events go to c.viewport.Update instead, handling
// scroll the way they always have.
//
// Edge-autoscroll — continuing to scroll and extend the selection once the
// drag reaches the viewport's top/bottom edge — is a deliberately separate
// follow-up, not handled here: a drag that leaves the viewport's rows
// simply stops extending until it comes back, the same way clampToContent
// already clamps a drag that leaves the *columns* (into the sidebar, or
// past the right edge) rather than losing track of it entirely.
func (c *chatModel) handleMouse(msg tea.MouseMsg) tea.Cmd {
	switch msg.Action {
	case tea.MouseActionPress:
		if msg.Button != tea.MouseButtonLeft {
			return nil
		}
		line, col, ok := c.hitTest(msg.X, msg.Y)
		if !ok {
			return nil
		}
		c.selecting = true
		c.hasSel = false
		c.selAnchor = selPoint{line: line, col: col}
		c.selCurrent = c.selAnchor
		c.rerenderContent()
		return nil

	case tea.MouseActionMotion:
		if !c.selecting {
			return nil
		}
		line, col, ok := c.hitTest(msg.X, msg.Y)
		if !ok {
			line, col = c.clampToContent(msg.X, msg.Y)
		}
		next := selPoint{line: line, col: col}
		if next == c.selCurrent {
			return nil
		}
		c.selCurrent = next
		if c.selCurrent != c.selAnchor {
			c.hasSel = true
		}
		c.rerenderContent()
		return nil

	case tea.MouseActionRelease:
		if !c.selecting {
			return nil
		}
		c.selecting = false
		if !c.hasSel {
			return nil
		}
		return c.copySelectionCmd()
	}
	return nil
}

// hitTest maps a screen coordinate to a (line, col) position in
// c.contentLines — col is clamped into [0, len(line)] so a click past the
// end of a short (padded-with-spaces-but-still-shorter-in-rune-terms...
// no — every stored line is already padded to exactly the viewport's width,
// so this clamp is purely defensive) line still lands validly. Returns
// ok=false for a coordinate outside the viewport's own rows (the sidebar,
// composer, or an overlay picker occupy other rows/columns entirely) or
// to the left of the main content pane's columns (the sidebar, when
// shown) — there's nothing in the transcript to select there.
func (c *chatModel) hitTest(x, y int) (line, col int, ok bool) {
	if y < 0 || y >= c.viewport.Height {
		return 0, 0, false
	}
	mainX := x - c.mainColOffset
	if mainX < 0 {
		return 0, 0, false
	}
	lineIdx := c.viewport.YOffset + y
	if lineIdx < 0 || lineIdx >= len(c.contentLines) {
		return 0, 0, false
	}
	return lineIdx, clampCol(mainX, c.contentLines[lineIdx].text), true
}

// clampToContent is hitTest's fallback for a drag that has moved outside
// the viewport's rows/columns (but not necessarily outside the terminal
// screen) — clamping to the nearest valid line/column keeps the drag
// extending the selection right up to that edge instead of freezing the
// moment the cursor leaves the content area.
func (c *chatModel) clampToContent(x, y int) (line, col int) {
	if len(c.contentLines) == 0 {
		return 0, 0
	}
	if y < 0 {
		y = 0
	} else if y >= c.viewport.Height {
		y = c.viewport.Height - 1
	}
	lineIdx := c.viewport.YOffset + y
	if lineIdx < 0 {
		lineIdx = 0
	} else if lineIdx >= len(c.contentLines) {
		lineIdx = len(c.contentLines) - 1
	}
	return lineIdx, clampCol(x-c.mainColOffset, c.contentLines[lineIdx].text)
}

// clampCol clamps a raw column into [0, len([]rune(text))].
func clampCol(col int, text string) int {
	if col < 0 {
		return 0
	}
	if n := len([]rune(text)); col > n {
		return n
	}
	return col
}

// selectionLineRange returns the current selection's line range in
// top-to-bottom order, regardless of whether the drag ran upward or
// downward from its anchor.
func (c *chatModel) selectionLineRange() (minLine, maxLine int) {
	a, b := c.selAnchor.line, c.selCurrent.line
	if a > b {
		return b, a
	}
	return a, b
}

// selectionColsForLine returns the [start, end) rune range selected on
// content line lineIdx, lineRuneCount runes long, given the current
// (possibly either-direction) anchor/current selection — the full line for
// any line strictly between the selection's first and last line, and the
// actual column range for the first/last line, the same convention every
// text editor uses: it's the characters *between* the two click points,
// not whole lines, unless the drag happens to span more than one line.
func (c *chatModel) selectionColsForLine(lineIdx, lineRuneCount int) (start, end int) {
	a, b := c.selAnchor, c.selCurrent
	if a.line > b.line || (a.line == b.line && a.col > b.col) {
		a, b = b, a
	}
	switch {
	case lineIdx == a.line && lineIdx == b.line:
		return a.col, b.col
	case lineIdx == a.line:
		return a.col, lineRuneCount
	case lineIdx == b.line:
		return 0, b.col
	default:
		return 0, lineRuneCount
	}
}

// selectedText extracts the current selection's plain text (trailing pad
// spaces trimmed off each line — c.contentLines' lines are always padded
// to the viewport's full width, which would otherwise show up as trailing
// whitespace on every copied line) and reports how many lines it spans,
// for copySelectionCmd's clipboard payload and toast.
func (c *chatModel) selectedText() (text string, lineCount int) {
	if !c.hasSel {
		return "", 0
	}
	minLine, maxLine := c.selectionLineRange()
	out := make([]string, 0, maxLine-minLine+1)
	for i := minLine; i <= maxLine && i < len(c.contentLines); i++ {
		runes := []rune(c.contentLines[i].text)
		start, end := c.selectionColsForLine(i, len(runes))
		if start < 0 {
			start = 0
		}
		if end > len(runes) {
			end = len(runes)
		}
		if start > end {
			start = end
		}
		out = append(out, strings.TrimRight(string(runes[start:end]), " "))
	}
	return strings.Join(out, "\n"), len(out)
}

// copySelectionCmd sends the current selection to the system clipboard
// (c.copyToClipboard) and shows a transient "Copied N lines" acknowledgment
// — the same callout Claude Code itself shows on mouse-release — cleared a
// couple of seconds later by clearCopyToastMsg. Returns nil (not even a
// no-op tea.Cmd) if there's nothing selected, which handleMouse's release
// case already guards against but this checks again defensively since it's
// exported-shaped enough to call directly from a test.
func (c *chatModel) copySelectionCmd() tea.Cmd {
	text, lines := c.selectedText()
	if text == "" {
		return nil
	}
	c.copyToClipboard(text)
	label := "line"
	if lines != 1 {
		label = "lines"
	}
	c.copyToast = fmt.Sprintf("Copied %d %s", lines, label)
	// Not refreshViewport: that also calls GotoBottom, which would yank
	// the scroll position out from under a selection the user just made
	// by deliberately scrolling to a spot that likely isn't the bottom.
	// copyToast changes viewportHeight's own return value (it borrows a
	// row the same way statusErr does), so the height does need resizing
	// here; rerenderContent alone wouldn't pick that up.
	c.viewport.Height = c.viewportHeight()
	c.rerenderContent()
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return clearCopyToastMsg{} })
}

// handleStreamMsg applies one streaming event (see stream.go) to the chat's
// state: accumulating text for streamChunkMsg, folding the finished reply
// into the transcript and updating Todos for streamDoneMsg, or surfacing an
// error for streamErrMsg. svc/ctx are unused here (kept for signature
// symmetry with callers) — every case only touches c's own fields. Returns
// the next Cmd to run: re-arming waitForStreamEvent for a non-terminal
// chunk, or nil otherwise.
func (c *chatModel) handleStreamMsg(msg tea.Msg, svc *services.AgentService, ctx context.Context) tea.Cmd {
	switch msg := msg.(type) {
	case streamChunkMsg:
		c.applyUpdate(msg.update)
		c.refreshViewport()
		return waitForStreamEvent(c.streamCh)
	case streamDoneMsg:
		c.finishStreaming()
		if msg.result != nil {
			c.todos = msg.result.Todos
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
// in-flight state: text is merged into the streaming buffer's trailing
// prose segment so it renders incrementally (Requirements FR8); a tool
// call appends an argument-aware headline segment (toolCallHeadline, "●
// toolname(headline)" once rendered) instead of a bare "[calling
// toolname...]" repeated with no way to tell one shell command or file
// write from another; and a tool result (post-v0.1.0 addendum) appends its
// own follow-up segment — an error if the call failed, a line-diff stat
// for file_write, or a truncated output preview for run_shell — once it
// actually completes. Segments render distinctly from prose (renderEntry/
// renderSegments) specifically so tool output and the model's actual
// answer are never visually ambiguous.
func (c *chatModel) applyUpdate(update *agent.ResponseUpdate) {
	if update == nil {
		return
	}
	for _, content := range update.Contents {
		switch t := content.(type) {
		case *message.TextContent:
			c.appendStreamingText(t.Text)
		case *message.FunctionCallContent:
			if t.Name != "" {
				if c.pendingToolCalls == nil {
					c.pendingToolCalls = make(map[string]string)
				}
				c.pendingToolCalls[t.CallID] = t.Name
				c.streaming = append(c.streaming, textSegment{kind: segmentToolCall, text: toolCallHeadline(t.Name, t.Arguments)})
			}
		case *message.FunctionResultContent:
			if seg := c.toolResultSegment(t); seg.text != "" {
				c.streaming = append(c.streaming, seg)
			}
		}
	}
}

// appendStreamingText merges text into the trailing segment if it's
// already prose (streamed text typically arrives a token/fragment at a
// time, so this keeps one prose segment per contiguous run instead of one
// per fragment), or starts a new prose segment otherwise — the first text
// of the reply, or text resuming after a tool call/result segment.
func (c *chatModel) appendStreamingText(text string) {
	if n := len(c.streaming); n > 0 && c.streaming[n-1].kind == segmentProse {
		c.streaming[n-1].text += text
		return
	}
	c.streaming = append(c.streaming, textSegment{kind: segmentProse, text: text})
}

// toolCallHeadline returns a short, human-readable summary of a tool
// call's arguments for applyUpdate's "[...]" marker — Claude Code's own
// "Bash(npm test)"/"Write(path/to/file)" style, not just the bare tool
// name, so a run of several calls to the same tool (a single repeated
// word, with no way to tell one shell command or file path from another)
// becomes legible. Falls back to the tool name alone if arguments can't be
// parsed, and to the raw (truncated) arguments JSON for a tool name this
// function doesn't special-case — any MCP-provided tool, chiefly — rather
// than showing nothing extra at all.
func toolCallHeadline(name, argumentsJSON string) string {
	var args map[string]any
	_ = json.Unmarshal([]byte(argumentsJSON), &args)
	str := func(key string) string {
		s, _ := args[key].(string)
		return s
	}
	switch name {
	case "run_shell":
		if cmd := str("command"); cmd != "" {
			return fmt.Sprintf("%s(%s)", name, oneLinePreview(cmd, 100))
		}
	case "file_write", "file_read":
		if path := str("path"); path != "" {
			return fmt.Sprintf("%s(%s)", name, path)
		}
	case "file_search":
		if pattern := str("pattern"); pattern != "" {
			if path := str("path"); path != "" {
				return fmt.Sprintf("%s(%q in %s)", name, pattern, path)
			}
			return fmt.Sprintf("%s(%q)", name, pattern)
		}
	case "directory_list":
		return fmt.Sprintf("%s(%s)", name, cmp.Or(str("path"), "."))
	case "web_fetch":
		if url := str("url"); url != "" {
			return fmt.Sprintf("%s(%s)", name, url)
		}
	case "web_search":
		if q := str("query"); q != "" {
			return fmt.Sprintf("%s(%q)", name, q)
		}
	case "Skill":
		if n := str("name"); n != "" {
			return fmt.Sprintf("%s(%s)", name, n)
		}
	}
	if preview := oneLinePreview(argumentsJSON, 80); preview != "" && preview != "{}" {
		return fmt.Sprintf("%s(%s)", name, preview)
	}
	return name
}

// toolResultSegment builds the follow-up segment for frc once its tool
// call actually completes, consuming (and removing) the matching entry
// pendingToolCalls' doc comment describes. Returns a zero-value (empty
// text) segment for a tool this function doesn't special-case and that
// didn't error — most tools (file_read, web_search, MCP-provided ones,
// ...) stay as quiet as they were before this addendum; only a genuine
// failure always gets a segment, regardless of which tool it was. The
// caller (applyUpdate) only appends the result when text is non-empty.
func (c *chatModel) toolResultSegment(frc *message.FunctionResultContent) textSegment {
	name := c.pendingToolCalls[frc.CallID]
	delete(c.pendingToolCalls, frc.CallID)

	if frc.Error != nil {
		return textSegment{kind: segmentToolError, text: oneLinePreview(frc.Error.Error(), 200)}
	}

	switch name {
	case "run_shell":
		if out, ok := frc.Result.(string); ok {
			if preview := truncatePreview(out, 400); preview != "" {
				return textSegment{kind: segmentToolResult, text: preview}
			}
		}
	case "file_write":
		if out, ok := frc.Result.(tools.FileWriteOutput); ok {
			return textSegment{kind: segmentToolResult, text: fmt.Sprintf("wrote %s (+%d -%d lines, %d bytes)", out.Path, out.LinesAdded, out.LinesRemoved, out.BytesWritten)}
		}
	}
	return textSegment{}
}

// oneLinePreview collapses embedded newlines in s to spaces (the same
// idiom renderStatusErrLine already uses) and truncates to maxRunes,
// keeping a multi-line shell command or error message to one legible
// transcript line rather than however many rows its raw text would take.
func oneLinePreview(s string, maxRunes int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " "), "\n", " "))
	return truncatePreview(s, maxRunes)
}

// truncatePreview trims s to maxRunes, appending "…" when it had to cut
// anything, without collapsing internal newlines — used where multi-line
// output (a shell command's real stdout) is still meant to render as
// multiple lines, just capped in overall length.
func truncatePreview(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

// finishStreaming folds the accumulated streaming buffer into transcript as
// a completed assistant entry and clears the in-flight streaming state.
// Always releases streamCancel's context, however the turn ended (success,
// error, or a user-initiated cancellation) — see startTurnCmd's doc comment
// on why leaving that uncalled would leak a canceled-but-uncollected child
// context.
func (c *chatModel) finishStreaming() {
	if len(c.streaming) > 0 {
		c.transcript = append(c.transcript, transcriptEntry{role: message.RoleAssistant, segments: c.streaming})
	}
	c.streaming = nil
	c.streamActive = false
	c.streamCh = nil
	if c.streamCancel != nil {
		c.streamCancel()
		c.streamCancel = nil
	}
}

// refreshViewport rebuilds the transcript's flattened line representation
// (buildContentLines — plus any in-flight streaming text) and scrolls to
// the bottom, so new content is always visible as it streams in. Each
// entry is word-wrapped to the viewport's own width rather than left to
// run off-screen — without this, any line longer than the terminal's width
// (a long assistant reply, a pasted user message, a skill body shown via
// ctrl+s) either gets silently clipped by the terminal or scrolls off
// unreadably, exactly the bug this addendum fixes. Call this whenever the
// transcript's actual *content* changes; for a mouse-driven selection
// changing only which already-built lines render highlighted, use
// rerenderContent instead — it doesn't rebuild content or move the scroll
// position, which GotoBottom here deliberately does.
func (c *chatModel) refreshViewport() {
	// Recomputed on every call, not just resize: viewportHeight depends on
	// c.statusErr/c.copyToast, which change independently of any resize
	// (startTurnCmd clearing statusErr, handleStreamMsg's streamErrMsg case
	// setting it, copySelectionCmd setting copyToast) — see viewportHeight's
	// own doc comment.
	c.viewport.Height = c.viewportHeight()
	c.contentLines = c.buildContentLines()
	c.viewport.SetContent(c.renderContentLines())
	c.viewport.GotoBottom()
}

// rerenderContent re-applies styling to the already-built c.contentLines
// without rebuilding them or moving the scroll position (bubbles/
// viewport.SetContent doesn't reset YOffset on its own unless it's now out
// of bounds, so this is safe mid-scroll) — used while a mouse-drag
// selection is active or just released, since the user is deliberately
// looking at content that may not be at the bottom and extending/
// committing a selection must never yank the view out from under them.
func (c *chatModel) rerenderContent() {
	c.viewport.SetContent(c.renderContentLines())
}

// buildContentLines flattens the whole transcript (plus any in-flight
// streaming reply) into one line-per-renderedLine slice — see that type's
// own doc comment for why this flattened form, rather than one multi-line
// string per entry, is what mouse-driven selection needs. One blank
// (whitespace-padded, unstyled) separator line follows every entry,
// mirroring the "\n\n" join refreshViewport used before this addendum.
func (c *chatModel) buildContentLines() []renderedLine {
	width := c.viewport.Width
	var lines []renderedLine
	blank := renderedLine{text: strings.Repeat(" ", max(width, 0)), style: lipgloss.NewStyle()}
	for _, e := range c.transcript {
		lines = append(lines, entryLines(e, width)...)
		lines = append(lines, blank)
	}
	if len(c.streaming) > 0 {
		lines = append(lines, entryLines(transcriptEntry{role: message.RoleAssistant, segments: c.streaming}, width)...)
	}
	return lines
}

// renderContentLines joins c.contentLines into the string c.viewport.
// SetContent wants, applying each line's style — and, for any line
// c.hasSelection overlaps, an extra Reverse(true) highlight over the
// selected rune range (renderLine) — rather than the lines' plain text.
// An empty transcript (nothing in c.contentLines at all) renders the
// purely cosmetic empty-chat greeting instead of a blank scrollback; that
// never happens once c.contentLines has anything in it (even a single
// blank separator line counts), so a selection is never possible to make
// against the greeting in the first place.
func (c *chatModel) renderContentLines() string {
	if len(c.contentLines) == 0 {
		// A brand-new (or freshly ctrl+n'd) chat has nothing in transcript
		// and nothing streaming yet — rather than an empty scrollback,
		// render a purely cosmetic greeting to avoid dead space. This is
		// never appended to c.transcript, so it's never part of what a turn
		// sends to the model or what gets persisted — it exists only in
		// this one rendered frame, and the very next refreshViewport call
		// that has real content (the first user message, via handleKey's
		// "enter" case) replaces it outright rather than needing to be
		// explicitly cleared anywhere.
		return greetingStyle.Render(defaultGreeting)
	}
	rendered := make([]string, len(c.contentLines))
	for i, l := range c.contentLines {
		rendered[i] = c.renderLine(i, l)
	}
	return strings.Join(rendered, "\n")
}

// renderLine renders one content line, splitting it into pre/selected/post
// rune ranges and layering Reverse(true) onto just the selected piece when
// idx falls within the current selection (post-v0.1.0 addendum: mouse-
// driven text selection) — the line's own style still applies to all three
// pieces, so e.g. a selected run of assistant prose keeps its background
// and merely inverts on top of it, the same way a real text editor's
// selection highlight layers over existing syntax coloring rather than
// replacing it.
func (c *chatModel) renderLine(idx int, l renderedLine) string {
	if !c.hasSel {
		return l.style.Render(l.text)
	}
	minLine, maxLine := c.selectionLineRange()
	if idx < minLine || idx > maxLine {
		return l.style.Render(l.text)
	}
	runes := []rune(l.text)
	start, end := c.selectionColsForLine(idx, len(runes))
	if start >= end {
		return l.style.Render(l.text)
	}
	pre, sel, post := string(runes[:start]), string(runes[start:end]), string(runes[end:])
	return l.style.Render(pre) + l.style.Reverse(true).Render(sel) + l.style.Render(post)
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
		entry := transcriptEntry{role: m.Role, text: text}
		if m.Role == message.RoleAssistant {
			entry.segments = []textSegment{{kind: segmentProse, text: text}}
		}
		entries = append(entries, entry)
	}
	return entries
}

// entryLines renders one transcript entry into renderedLines, word-wrapped
// to width (the viewport's content width) via lipgloss's Width-based
// Render — the same wrap-not-clip idiom renderStatusErrLine already
// established for the status line. width <= 0 (not expected in practice,
// since resize always sets c.viewport.Width before the first
// refreshViewport call, but defensive against a zero-value chatModel in a
// test) skips wrapping entirely rather than passing a nonsensical width to
// lipgloss.
//
// No "you"/"assistant" label is rendered (post-v0.1.0 addendum) — a user
// message is instead set off by a lighter background block
// (userMessageStyle) spanning the full content width; an assistant reply
// renders segment-by-segment (segmentLines) — its own background block
// for prose (assistantMessageStyle, a different shade from a user
// message's, per the later addendum answering "it's hard to tell tool
// output from the actual response"), a dim indented aside with no
// background at all for a tool call/result/error. A system-style entry
// (ctrl+s skill body, "Cancelled.", etc.) keeps its "system" label, since
// removing that one specifically wasn't asked for and it's the only cue
// distinguishing it from a real assistant reply.
func entryLines(e transcriptEntry, width int) []renderedLine {
	if e.system {
		lines := []renderedLine{{text: "system", style: systemLabelStyle}}
		for _, l := range wrapAndPad(e.text, width) {
			lines = append(lines, renderedLine{text: l, style: lipgloss.NewStyle()})
		}
		return lines
	}
	if e.role == message.RoleUser {
		var lines []renderedLine
		for _, l := range wrapAndPad(e.text, width) {
			lines = append(lines, renderedLine{text: l, style: userMessageStyle})
		}
		return lines
	}
	segments := e.segments
	if len(segments) == 0 && e.text != "" {
		// Defensive fallback for an assistant entry built with only text set
		// (e.g. a test constructing one directly, bypassing applyUpdate/
		// finishStreaming) — treat it as a single prose segment rather than
		// silently rendering nothing.
		segments = []textSegment{{kind: segmentProse, text: e.text}}
	}
	var lines []renderedLine
	for _, seg := range segments {
		lines = append(lines, segmentLines(seg, width)...)
	}
	return lines
}

// wrapAndPad word-wraps text to width physical lines, each padded with
// trailing spaces to exactly width columns (lipgloss's Width-based Render
// does both in one pass) — width <= 0 skips wrapping/padding entirely.
func wrapAndPad(text string, width int) []string {
	if width <= 0 {
		return strings.Split(text, "\n")
	}
	return strings.Split(lipgloss.NewStyle().Width(width).Render(text), "\n")
}

// segmentLines renders one segment into renderedLines: prose gets
// assistantMessageStyle's full-width background block, same treatment
// entryLines gives a user message but a different shade; a tool call gets
// a "● " marker (toolCallStyle), a tool result or error gets a "  ⎿ "
// marker (toolResultStyle/toolErrorStyle) — Claude Code's own convention
// for a de-emphasized aside, deliberately left with no background at all
// so it reads as subordinate to the highlighted prose around it.
func segmentLines(seg textSegment, width int) []renderedLine {
	switch seg.kind {
	case segmentToolCall:
		return indentedLines("● ", seg.text, toolCallStyle, width)
	case segmentToolResult:
		return indentedLines("  ⎿ ", seg.text, toolResultStyle, width)
	case segmentToolError:
		return indentedLines("  ⎿ ✗ ", seg.text, toolErrorStyle, width)
	default: // segmentProse
		var lines []renderedLine
		for _, l := range wrapAndPad(seg.text, width) {
			lines = append(lines, renderedLine{text: l, style: assistantMessageStyle})
		}
		return lines
	}
}

// indentedLines renders a tool call/result/error block into renderedLines:
// prefix marks only the very first physical row (after word-wrapping to
// width minus the prefix's own width), and every continuation row —
// whether from an embedded newline in text or from wrapping a long one —
// gets blank indentation of the same width instead, so the block's text
// stays visually aligned under its own marker rather than running back
// out to the left margin. Each returned line is prefixWidth+contentWidth
// runes — the same total width every other kind of line pads to — so
// hitTest/selection never need to special-case a tool line's narrower
// wrapped content.
func indentedLines(prefix, text string, style lipgloss.Style, width int) []renderedLine {
	prefixWidth := lipgloss.Width(prefix)
	contentWidth := width - prefixWidth

	var physical []string
	for _, line := range strings.Split(text, "\n") {
		if contentWidth > 0 {
			physical = append(physical, strings.Split(lipgloss.NewStyle().Width(contentWidth).Render(line), "\n")...)
		} else {
			physical = append(physical, line)
		}
	}

	blank := strings.Repeat(" ", prefixWidth)
	lines := make([]renderedLine, len(physical))
	for i, pl := range physical {
		marker := prefix
		if i > 0 {
			marker = blank
		}
		lines[i] = renderedLine{text: marker + pl, style: style}
	}
	return lines
}

// View renders the chat screen: either an open overlay picker (ctrl+a/
// ctrl+o, post-v0.1.0 addendum) or the transcript with either the composer
// or the "thinking" spinner at the bottom, plus a status line for the last
// stream error (or, taking priority, a transient copy-toast — post-v0.1.0
// addendum: mouse-driven text selection), if either — beside the sidebar
// (agent + model + todos), unless ctrl+b has hidden it. Hiding the sidebar
// is no longer required for clean copying the way it used to be (mouse
// selection is scoped to the main content pane's own columns regardless —
// see hitTest/mainColOffset), but it's kept as an option for anyone who
// just wants the extra width. Tool approval no longer has a bottom-bar
// state of its own — it's auto-answered without ever stopping the spinner
// (post-v0.1.0 addendum).
func (c *chatModel) View(width, height int) string {
	if c.sidebarHidden {
		c.mainColOffset = 0
	}

	if c.picker != nil {
		if c.sidebarHidden {
			return c.picker.View()
		}
		sidebar := c.renderSidebar()
		c.mainColOffset = lipgloss.Width(sidebar)
		return lipgloss.JoinHorizontal(lipgloss.Top, sidebar, c.picker.View())
	}

	var bottom string
	switch {
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
	switch {
	case c.copyToast != "":
		// Takes priority over statusErr (post-v0.1.0 addendum: mouse-driven
		// copy) — the two share one borrowed row (viewportHeight) rather
		// than needing two, and a copy acknowledgment is short-lived enough
		// that losing statusErr's line for a couple of seconds is a fine
		// trade against reserving a row for both every single frame.
		main = lipgloss.JoinVertical(lipgloss.Left, main, copyToastStyle.Render(c.copyToast))
	case c.statusErr != nil:
		main = lipgloss.JoinVertical(lipgloss.Left, main, errorStyle.Render(c.renderStatusErrLine()))
	}

	if c.sidebarHidden {
		return main
	}
	// mainColOffset is set here, not just measured once in resize, because
	// hitTest (mouse-driven selection) needs the sidebar's own *actual*
	// rendered width (border/padding included) to translate a raw mouse X
	// coordinate into a column within the transcript — View is the one
	// place that already measures it to lipgloss.JoinHorizontal the two
	// panes together, so caching it here avoids re-rendering the sidebar
	// just to remeasure it on every mouse event.
	sidebar := c.renderSidebar()
	c.mainColOffset = lipgloss.Width(sidebar)
	return lipgloss.JoinHorizontal(lipgloss.Top, sidebar, main)
}

// renderSidebar renders the agent/model indicators (post-v0.1.0 §3.4/§4
// addendum) and the live todo panel (Design §3.7).
func (c *chatModel) renderSidebar() string {
	var b strings.Builder
	b.WriteString(modeStyle.Render("Agent: " + c.agentName))
	b.WriteString("\n(ctrl+a to switch)\n")
	b.WriteString(modeStyle.Render("Model: " + c.model))
	b.WriteString("\n(ctrl+o to switch)\n")
	b.WriteString("(ctrl+s to view skills)\n")
	b.WriteString("(ctrl+h for history)\n")
	b.WriteString("(ctrl+n for new chat)\n")
	b.WriteString("(ctrl+b to hide this)\n")
	b.WriteString("(scroll: pgup/pgdn)\n")
	b.WriteString("(or ctrl+up/down)\n\n")
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
	return sidebarStyle.Width(sidebarWidth - 2).Height(c.sidebarHeight()).Render(b.String())
}

// sidebarHeight matches the sidebar's rendered height to the main column's
// total (viewport + composer/spinner + optional status-error line, which
// together always sum to exactly c.height — see viewportHeight), so there's
// no height mismatch between the two JoinHorizontal'd blocks and no row
// left blank at the bottom of the screen.
func (c *chatModel) sidebarHeight() int {
	h := c.height
	if h < 1 {
		h = 1
	}
	return h
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
// can show in one screen and scrolling the sidebar/transcript
// above it out of view, exactly the secondary layout bug this task flagged.
// Collapsing embedded newlines to spaces and truncating to one line's worth
// of the main column's width keeps the error's footprint to the single row
// View's layout already visually allots it next to the composer/approval
// prompt.
func (c *chatModel) renderStatusErrLine() string {
	text := "error: " + strings.ReplaceAll(strings.ReplaceAll(c.statusErr.Error(), "\r\n", " "), "\n", " ")

	maxWidth := c.mainWidth()
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
