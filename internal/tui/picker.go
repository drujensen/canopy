package tui

import "github.com/charmbracelet/bubbles/list"

// pickerItem adapts a plain name string to bubbles/list's Item
// (FilterValue) and DefaultItem (Title/Description) interfaces, the same
// role agentItem (agentpicker.go) plays for the top-level agent picker
// screen. It backs both of chatModel's in-chat overlay pickers (ctrl+a
// switch-agent, ctrl+o switch-model — post-v0.1.0 addendum, Design
// §3.4/§4/§5): both are sourced from AgentService.ListAgents()/ListModels(),
// which only return names, not full agentsource.AgentDefinition values, so
// there's no Description to show here the way agentItem has one.
type pickerItem struct{ name string }

func (i pickerItem) Title() string       { return i.name }
func (i pickerItem) Description() string { return "" }
func (i pickerItem) FilterValue() string { return i.name }

// newPickerList builds a bubbles/list.Model listing names, titled title,
// styled the same way NewModel's own agentList is (list.NewDefaultDelegate,
// no status bar, filtering enabled) so the chat screen's in-chat overlay
// pickers look and behave consistently with the top-level agent picker
// screen and with each other.
func newPickerList(names []string, title string, width, height int) list.Model {
	items := make([]list.Item, 0, len(names))
	for _, n := range names {
		items = append(items, pickerItem{name: n})
	}
	l := list.New(items, list.NewDefaultDelegate(), width, height)
	l.Title = title
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(true)
	return l
}
