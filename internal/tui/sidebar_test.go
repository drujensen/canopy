package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/lipgloss"
	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/domain/services"
	"github.com/drujensen/canopy/internal/impl/agentsource"

	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
	"github.com/drujensen/canopy/internal/impl/skillsource"
)

// newCapabilitiesSwitchTestService builds a real AgentService with two
// agents that genuinely differ in what's available to them — "assistant"
// inherits every tool (including Skill, since a skill is loaded) and
// "restricted" only has Bash — so a switch between them exercises a real
// capability change, unlike newSwitchTestService's two identical agents.
func newCapabilitiesSwitchTestService(t *testing.T) *services.AgentService {
	t.Helper()
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	return services.NewAgentService(services.AgentServiceConfig{
		Definitions: services.Definitions{
			Agents: map[string]agentsource.AgentDefinition{
				"assistant":  {Name: "assistant"},
				"restricted": {Name: "restricted", Tools: []string{"Bash"}},
			},
			Skills: map[string]skillsource.SkillDefinition{
				"skill-a": {Name: "skill-a", Description: "does a thing"},
			},
		},
		Providers:    []entities.ProviderConfig{{Name: "p", Type: entities.ProviderTypeOpenAI}},
		Models:       []entities.ModelConfig{{Name: "m1", Provider: "p"}},
		DefaultModel: "m1",
		Repository:   repo,
	})
}

// TestChatModel_AgentSwitch_RefreshesToolAndSkillCounts drives the real
// ctrl+a path (mirroring TestChatModel_CtrlA_OpensAgentPicker_
// SelectingSwitchesAgentKeepsChat's own shape) between two agents with
// genuinely different capabilities, and asserts agentChangedMsg carries
// the new agent's real counts — and that applying it the way Model.Update
// does (model.go's agentChangedMsg case) updates the sidebar fields.
func TestChatModel_AgentSwitch_RefreshesToolAndSkillCounts(t *testing.T) {
	svc := newCapabilitiesSwitchTestService(t)
	ctx := context.Background()
	_, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	startCaps, err := svc.AgentCapabilities(ctx, "assistant")
	require.NoError(t, err)

	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.toolCount = startCaps.ToolCount
	c.skillCount = startCaps.SkillCount
	require.Greater(t, c.skillCount, 0, "'assistant' inherits the Skill tool, so this must be nonzero for the switch below to be a meaningful assertion")

	cmd := c.handleKey(tea.KeyMsg{Type: tea.KeyCtrlA}, svc, ctx)
	assert.Nil(t, cmd)
	require.NotNil(t, c.picker)

	// Agent names are sorted (ListAgents): "assistant", "restricted". Move
	// down to select "restricted".
	c.handleKey(tea.KeyMsg{Type: tea.KeyDown}, svc, ctx)
	selectCmd := c.handleKey(tea.KeyMsg{Type: tea.KeyEnter}, svc, ctx)
	require.NotNil(t, selectCmd)

	msg := selectCmd()
	agentMsg, ok := msg.(agentChangedMsg)
	require.True(t, ok, "got %#v", msg)
	assert.Equal(t, "restricted", agentMsg.agentName)
	assert.Equal(t, 0, agentMsg.capabilities.SkillCount, "'restricted' has no Skill tool in its allowlist")

	// Model.Update's agentChangedMsg case (model.go) — simulated here the
	// same way TestChatModel_CtrlA_OpensAgentPicker_
	// SelectingSwitchesAgentKeepsChat simulates its own agentName update.
	c.agentName = agentMsg.agentName
	c.toolCount = agentMsg.capabilities.ToolCount
	c.skillCount = agentMsg.capabilities.SkillCount

	assert.Equal(t, 0, c.skillCount)
	assert.NotEqual(t, startCaps.ToolCount, c.toolCount, "switching to a Bash-only agent must change the tool count too")
}

// TestRenderSidebar_ShowsToolSkillCountsAndCost is an integration-level
// check that renderSidebar actually wires in the new toolCount/skillCount
// fields and usageCostLine's compact running-total cost, given known
// values. The sidebar deliberately shows only the aggregate cost, not
// renderUsageLines' full per-model breakdown (see usageCostLine's own doc
// comment on why) — that breakdown is covered by TestRenderUsageLines_
// ExactFormat and TestChatModel_CtrlU_ShowsFullUsageBreakdown instead.
func TestRenderSidebar_ShowsToolSkillCountsAndCost(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	c.toolCount = 7
	c.skillCount = 3
	c.usage = map[string]entities.ModelUsage{
		"m1":     {InputTokens: 1000, OutputTokens: 200, CostUSD: 0.0123},
		"sub-m1": {InputTokens: 50, OutputTokens: 25, CostUSD: 0.0004},
	}

	rendered := c.renderSidebar()

	assert.Contains(t, rendered, "Tools: 7")
	assert.Contains(t, rendered, "Skills: 3")
	assert.Contains(t, rendered, "Cost: $0.0127", "the sidebar must show the summed cost across both models, not either one alone")
	assert.NotContains(t, rendered, "sub-m1", "the full per-model breakdown belongs to ctrl+u, not the always-visible sidebar")
}

// TestRenderUsageLines_ExactFormat is a direct, wrapping-independent unit
// test of renderUsageLines' output shape — two lines per model (name,
// then its numbers — see that function's own doc comment on why not one)
// plus a combined "total" pair only when more than one model
// contributed, sorted by name.
func TestRenderUsageLines_ExactFormat(t *testing.T) {
	lines := renderUsageLines(map[string]entities.ModelUsage{
		"m1":     {InputTokens: 1000, OutputTokens: 200, CostUSD: 0.0123},
		"sub-m1": {InputTokens: 50, OutputTokens: 25, CostUSD: 0.0004},
	})
	assert.Equal(t, []string{
		"  m1",
		"    1000 in / 200 out, $0.0123",
		"  sub-m1",
		"    50 in / 25 out, $0.0004",
		"  total",
		"    1050 in / 225 out, $0.0127",
	}, lines)
}

// TestRenderUsageLines_SingleModel_NoTotalLine asserts the "total" line
// only appears once more than one model has contributed — a single
// model's own numbers are already the total, so repeating them would be
// redundant.
func TestRenderUsageLines_SingleModel_NoTotalLine(t *testing.T) {
	lines := renderUsageLines(map[string]entities.ModelUsage{
		"m1": {InputTokens: 10, OutputTokens: 5, CostUSD: 0.001},
	})
	assert.Equal(t, []string{"  m1", "    10 in / 5 out, $0.0010"}, lines)
}

// TestUsageCostLine_FreeModelWithRealTokens_ShowsZeroDollarsNotNoneYet
// asserts a chat run entirely against a free/self-hosted model (e.g. an
// Ollama model with no published per-million-token rate) — real, nonzero
// tokens but exactly zero cost (mergeUsage's own doc comment) — shows
// "$0.0000", not "(none yet)": usageCostLine is a cost readout and stays
// in money even when that money is genuinely zero, per explicit user
// feedback that it must never switch to a token count instead. "(none
// yet)" is reserved for a chat with no usage recorded at all.
func TestUsageCostLine_FreeModelWithRealTokens_ShowsZeroDollarsNotNoneYet(t *testing.T) {
	got := usageCostLine(map[string]entities.ModelUsage{
		"drujensen/qwen3.8": {InputTokens: 99147, OutputTokens: 1857, CostUSD: 0},
	})
	assert.Equal(t, "$0.0000", got)
	assert.NotEqual(t, "(none yet)", got)
}

// TestRenderSidebar_NoUsageYet_ShowsPlaceholder asserts a brand-new chat
// (nil usage, the newChatModel default) shows a plain placeholder instead
// of an empty "Usage:" section or a misleading "$0.0000" line.
func TestRenderSidebar_NoUsageYet_ShowsPlaceholder(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	assert.Contains(t, c.renderSidebar(), "Cost: (none yet)")
}

func TestDisplayWorkingDirectory_UsesHomeAbbreviation(t *testing.T) {
	tests := []struct {
		name, dir, home, want string
	}{
		{"home", "/home/alice", "/home/alice", "~"},
		{"child", "/home/alice/project", "/home/alice", "~/project"},
		{"outside", "/tmp/project", "/home/alice", "/tmp/project"},
		{"sibling", "/home/alice-other", "/home/alice", "/home/alice-other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, displayWorkingDirectory(tt.dir, tt.home))
		})
	}
}

func TestRenderSidebar_ShowsTitleAndDirectory(t *testing.T) {
	c := newChatModel("chat-1", "assistant", nil, "m1", 80, 24)
	rendered := c.renderSidebar()

	assert.Contains(t, rendered, "Canopy")
	assert.Contains(t, rendered, "Directory:")
	// Long paths wrap inside the fixed-width sidebar, so verify the
	// directory label is present rather than expecting the whole path on one line.
	assert.Contains(t, rendered, "~")
}

// TestFitToWidth is a direct unit test of fitToWidth's trim-plus-ellipsis
// behavior at the boundaries — text already within width is left
// untouched, text past width is trimmed with a "…" cue, and degenerate
// widths (1, 0, negative) never panic or return more than width columns.
func TestFitToWidth(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		width int
		want  string
	}{
		{"fits", "m1", 10, "m1"},
		{"exact", "abc", 3, "abc"},
		{"trim one", "abcd", 3, "ab…"},
		{"trim many", "abcdef", 2, "a…"},
		{"width one", "abcd", 1, "a"},
		{"zero width", "abcd", 0, ""},
		{"negative width", "abcd", -5, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fitToWidth(tt.text, tt.width)
			assert.Equal(t, tt.want, got)
			if tt.width >= 1 {
				assert.LessOrEqual(t, lipgloss.Width(got), tt.width, "output must never exceed its budgeted width")
			}
		})
	}
}

// TestSidebarSettingLineFit_ShortNameUnchanged asserts a value already
// within the sidebar's budget renders identically to plain
// sidebarSettingLine — no spurious ellipsis or trimming for names that
// already fit.
func TestSidebarSettingLineFit_ShortNameUnchanged(t *testing.T) {
	assert.Equal(t, sidebarSettingLine("Model: m1", "^O"), sidebarSettingLineFit("Model: m1", "^O"))
	assert.Equal(t, sidebarSettingLine("Model: gpt-4o", "^O"), sidebarSettingLineFit("Model: gpt-4o", "^O"))
}

// TestSidebarSettingLineFit_LongNameTrimsToOneRow asserts the reported
// bug's fix directly: a value long enough that value+gap+hint would
// otherwise overflow sidebarContentWidth (and wrap the ^O hint onto a
// second row) now gets trimmed to fit, keeping the ellipsis and the hint
// on the same single row.
func TestSidebarSettingLineFit_LongNameTrimsToOneRow(t *testing.T) {
	longName := "drujensen/qwen3.8-super-large-very-long-slug"
	value := "Model: " + longName

	got := sidebarSettingLineFit(value, "^O")

	// Exactly one content row's worth of columns (32 =
	// sidebarContentWidth), not two rows' worth — i.e. no wrap.
	assert.Equal(t, sidebarContentWidth, lipgloss.Width(got))
	assert.Contains(t, got, "…", "the trim must be signalled with an ellipsis")
	assert.Contains(t, got, "^O", "the hint must survive the trim, on the same row")
	assert.NotContains(t, got, longName, "the full over-long name must be trimmed, not rendered whole")
}

// TestRenderSidebar_LongModelNameStaysOneRow is the integration-level
// check matching the reported bug end-to-end: with a long real-world-
// shaped model slug set on the chat, the sidebar renders the Model:^O
// line on a single row (trimmed, not wrapped), so the ^O hint is still
// visible right there rather than orphaned on its own row.
func TestRenderSidebar_LongModelNameStaysOneRow(t *testing.T) {
	longName := "drujensen/qwen3.8-super-large-very-long-slug"
	c := newChatModel("chat-1", "assistant", nil, longName, 80, 24)
	rendered := c.renderSidebar()

	var modelLines []string
	for _, l := range strings.Split(rendered, "\n") {
		if strings.Contains(l, "Model:") {
			modelLines = append(modelLines, l)
		}
	}
	require.NotEmpty(t, modelLines)
	assert.Len(t, modelLines, 1, "the Model line must stay on a single physical row (no wrap)")
	assert.Contains(t, modelLines[0], "^O")
	assert.Contains(t, modelLines[0], "…")
	assert.NotContains(t, rendered, longName)
}
