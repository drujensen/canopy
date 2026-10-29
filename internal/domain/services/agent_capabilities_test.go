package services

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/impl/agentsource"
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
	"github.com/drujensen/canopy/internal/impl/skillsource"
)

// newCapabilitiesTestService builds a real AgentService (no provider
// network calls needed — AgentCapabilities never invokes a.Run) with two
// agents ("assistant", tools: unset so it inherits everything including
// the Skill tool; "restricted", an explicit allowlist with no Skill tool)
// and two loaded skills, so ToolCount/SkillCount can be checked against
// known values for each shape.
func newCapabilitiesTestService(t *testing.T) *AgentService {
	t.Helper()
	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test"}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gpt-test"}
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	return NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{
				"assistant":  {Name: "assistant", Description: "inherits every tool"},
				"restricted": {Name: "restricted", Description: "only Bash", Tools: []string{"Bash"}},
			},
			Skills: map[string]skillsource.SkillDefinition{
				"skill-a": {Name: "skill-a", Description: "does a thing"},
				"skill-b": {Name: "skill-b", Description: "does another thing"},
			},
		},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Repository:   repo,
		Tools:        ToolsConfig{WorkingRoot: t.TempDir()},
	})
}

// TestAgentCapabilities_InheritsEverything_IncludesSkillsAndSubagent
// asserts the common case (no "tools" allowlist): SkillCount reflects
// every loaded skill because the Skill tool is in the inherited set, and
// ToolCount includes a subagent-dispatch tool for the one other loaded
// agent ("restricted") on top of every core tool buildTools would
// otherwise report.
func TestAgentCapabilities_InheritsEverything_IncludesSkillsAndSubagent(t *testing.T) {
	svc := newCapabilitiesTestService(t)

	caps, err := svc.AgentCapabilities(context.Background(), "assistant")
	require.NoError(t, err)

	assert.Equal(t, 2, caps.SkillCount, "both loaded skills must be visible to an agent with no tools: allowlist")
	// Every core tool except WebSearch (no backend configured: Bash,
	// FileRead, FileWrite, FileEdit, FileSearch, DirectoryList, WebFetch,
	// Skill = 8) plus one subagent-dispatch tool for the other loaded agent
	// ("restricted") = 9.
	assert.Equal(t, 9, caps.ToolCount)
}

// TestAgentCapabilities_ExplicitAllowlistWithoutSkill_ZeroSkills asserts
// an agent whose own "tools" allowlist doesn't include Skill reports zero
// skills, even though skills are loaded globally — it genuinely cannot
// reach any of them.
func TestAgentCapabilities_ExplicitAllowlistWithoutSkill_ZeroSkills(t *testing.T) {
	svc := newCapabilitiesTestService(t)

	caps, err := svc.AgentCapabilities(context.Background(), "restricted")
	require.NoError(t, err)

	assert.Equal(t, 0, caps.SkillCount)
	assert.Equal(t, 2, caps.ToolCount, "Bash itself, plus one subagent-dispatch tool for 'assistant'")
}

// TestAgentCapabilities_UnknownAgent_Errors asserts a clear error rather
// than a zero-value result for a name that doesn't resolve to any loaded
// agent definition.
func TestAgentCapabilities_UnknownAgent_Errors(t *testing.T) {
	svc := newCapabilitiesTestService(t)
	_, err := svc.AgentCapabilities(context.Background(), "nonexistent")
	assert.Error(t, err)
}

// TestAgentService_ListTools_InheritsEverything_SortedAndLabeled asserts
// ListTools, for an agent with no tools: allowlist, returns exactly the 9
// entries AgentCapabilities.ToolCount counts for "assistant" (see
// TestAgentCapabilities_InheritsEverything_IncludesSkillsAndSubagent): 8
// core tools labeled "Built-in" (no MCP server is configured in
// newCapabilitiesTestService, so "MCP" never appears) plus one
// subagent-dispatch tool for "restricted" labeled "Subagent" — proving
// ListTools' count matches the sidebar's own "Tools: N" exactly, not just
// the built-in/MCP subset.
func TestAgentService_ListTools_InheritsEverything_SortedAndLabeled(t *testing.T) {
	svc := newCapabilitiesTestService(t)

	got, err := svc.ListTools(context.Background(), "assistant")
	require.NoError(t, err)

	require.Len(t, got, 9)
	bySource := map[string]int{}
	for _, ts := range got {
		bySource[ts.Source]++
		assert.NotEmpty(t, ts.Description)
	}
	assert.Equal(t, 8, bySource["Built-in"])
	assert.Equal(t, 1, bySource["Subagent"])
	assert.Equal(t, 0, bySource["MCP"], "no MCP server is configured in this test service")

	names := make([]string, len(got))
	for i, ts := range got {
		names[i] = ts.Name
	}
	assert.Equal(t, []string{"Bash", "DirectoryList", "FileEdit", "FileRead", "FileSearch", "FileWrite", "Skill", "WebFetch", "restricted"}, names)
}

// TestAgentService_ListTools_RespectsAllowlist asserts ListTools for
// "restricted" (tools: [Bash] only) returns that one built-in entry plus
// the subagent-dispatch tool for "assistant" — the full inherited core set
// "assistant" gets is correctly excluded, but the subagent tool isn't
// gated by the allowlist (buildTopLevelTools' own behavior: subagent
// dispatch tools are always layered on top).
func TestAgentService_ListTools_RespectsAllowlist(t *testing.T) {
	svc := newCapabilitiesTestService(t)

	got, err := svc.ListTools(context.Background(), "restricted")
	require.NoError(t, err)

	require.Len(t, got, 2)
	assert.Equal(t, "Bash", got[0].Name)
	assert.Equal(t, "Built-in", got[0].Source)
	assert.Equal(t, "assistant", got[1].Name)
	assert.Equal(t, "Subagent", got[1].Source)
}

// TestAgentService_ListTools_UnknownAgent_Errors mirrors
// TestAgentCapabilities_UnknownAgent_Errors for ListTools' own
// agentName-lookup guard.
func TestAgentService_ListTools_UnknownAgent_Errors(t *testing.T) {
	svc := newCapabilitiesTestService(t)
	_, err := svc.ListTools(context.Background(), "nonexistent")
	assert.Error(t, err)
}
