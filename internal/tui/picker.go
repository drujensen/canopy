package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/list"
)

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

// skillPickerItem adapts a services.SkillSummary's Name/Description to
// bubbles/list's Item interfaces for chatModel's ctrl+s skills-browser
// overlay (post-v0.1.0 addendum, Design §3.11/FR19). Unlike pickerItem
// (ctrl+a/ctrl+o, names only — ListAgents/ListModels return only names),
// the skills browser shows each skill's description too, per Design's own
// spec for the browser ("lists skill names+descriptions"); kept as two
// plain strings rather than importing domain/services.SkillSummary here so
// this file doesn't need that import for what's otherwise the same
// Title/Description/FilterValue shape as agentItem.
type skillPickerItem struct{ name, description string }

func (i skillPickerItem) Title() string       { return i.name }
func (i skillPickerItem) Description() string { return i.description }
func (i skillPickerItem) FilterValue() string { return i.name }

// modelPickerItem adapts a services.ModelSummary (name plus per-million-token
// request/response cost) to bubbles/list's Item interfaces for the ctrl+o
// model-switch overlay (post-v0.1.0 addendum) — the model-picker analogue of
// skillPickerItem showing a skill's description: pickerItem's blank
// Description() is no longer good enough here now that ListModelSummaries
// gives the picker something worth showing.
type modelPickerItem struct {
	name                                      string
	inputCostPerMillion, outputCostPerMillion float64
}

func (i modelPickerItem) Title() string { return i.name }

// Description formats cost as "$<input> in / $<output> out" (per million
// request/response tokens — the unit is implied, not spelled out, to keep
// the picker row short) when at least one side is known, or "" (matching
// pickerItem's blank Description, rather than a misleading "$0/$0") when a
// model's cost is entirely unset — true for every self-hosted/local model
// (Ollama and the like) and for any provider models.dev doesn't publish
// pricing for. Costs are formatted with up to 2 decimal places, trimmed of a
// trailing ".00" (most published per-million prices are whole or half
// dollars; a forced two decimals would make "$2 in / $10 out" read as
// "$2.00 in / $10.00 out" for no benefit) via %g-style trimming done by hand
// since Go's %g uses scientific notation past 6 significant digits, which a
// price like 1234.5 would otherwise trigger.
func (i modelPickerItem) Description() string {
	if i.inputCostPerMillion == 0 && i.outputCostPerMillion == 0 {
		return ""
	}
	return fmt.Sprintf("$%s in / $%s out", formatCost(i.inputCostPerMillion), formatCost(i.outputCostPerMillion))
}

func (i modelPickerItem) FilterValue() string { return i.name }

// formatCost renders a per-million-token dollar amount with up to 2 decimal
// places, dropping a trailing ".00"/".0" rather than always showing two
// decimals — see modelPickerItem.Description's doc comment for why %g isn't
// used directly.
func formatCost(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	return s
}

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

// historyPickerItem adapts a services.ChatSummary to bubbles/list's Item
// interfaces for ctrl+h's history browser (post-v0.1.0 addendum, Design
// §5's addendum) — both the top-level screenHistory screen and the in-chat
// overlay (chatModel's pickerHistory) share this one item type. Kept as
// plain fields rather than importing domain/services.ChatSummary here, the
// same reasoning skillPickerItem's own doc comment gives.
type historyPickerItem struct {
	chatID    string
	title     string
	agentName string
	updatedAt time.Time
}

// historyDateFormat is used both for a title-less entry's Title() fallback
// and for every entry's Description() — deliberately including both date
// and time (not just the date) since two sessions on the same day are
// common and would otherwise be indistinguishable in the picker.
const historyDateFormat = "Jan 2, 2006 3:04 PM"

// Title returns the generated title, or — the explicitly requested
// fallback (post-v0.1.0 addendum) — a formatted date when none exists yet
// (the chat's first turn hasn't completed) or generation failed
// (AgentService.GenerateChatTitle's doc comment).
func (i historyPickerItem) Title() string {
	if i.title != "" {
		return i.title
	}
	return i.updatedAt.Local().Format(historyDateFormat)
}

// Description always shows the agent name and date/time together — even
// when Title() is already falling back to a date, Description adds the
// agent name Title() alone can't carry, and when Title() is a real
// generated title, Description is what actually surfaces the date/agent a
// user picking between sessions needs.
func (i historyPickerItem) Description() string {
	return fmt.Sprintf("%s · %s", i.agentName, i.updatedAt.Local().Format(historyDateFormat))
}

func (i historyPickerItem) FilterValue() string { return i.title + " " + i.agentName }
