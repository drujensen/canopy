package tui

import "github.com/drujensen/canopy/internal/impl/agentsource"

// agentItem adapts an agentsource.AgentDefinition to bubbles/list's Item
// (FilterValue) and DefaultItem (Title/Description) interfaces, so the
// agent picker screen (Design §5's "agent picker sourced from agentsource,
// not a database list") can list loaded agent definitions directly, with no
// intermediate view-model type duplicating their fields.
type agentItem struct {
	def agentsource.AgentDefinition
}

func (i agentItem) Title() string       { return i.def.Name }
func (i agentItem) Description() string { return i.def.Description }
func (i agentItem) FilterValue() string { return i.def.Name }
