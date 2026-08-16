package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/impl/agentsource"
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
	"github.com/drujensen/canopy/internal/impl/skillsource"
)

// testSkills is a small, fixed catalog reused by the tests in this file.
func testSkills() map[string]skillsource.SkillDefinition {
	return map[string]skillsource.SkillDefinition{
		"pdf-processing": {
			Name:        "pdf-processing",
			Description: "Extract text and tables from PDF files.",
			Body:        "# PDF Processing\n\nDistinctive body content: use pdftotext for extraction.",
			Dir:         "/tmp/does-not-matter",
		},
	}
}

// TestAgentService_BuildInstructions_SkillsCatalog_PresentWhenSkillsLoaded
// proves buildInstructions appends a "## Available Skills" section listing
// every loaded skill's name+description (level 1 of Design §3.11/FR19's
// progressive disclosure) when skills are configured, and that the
// catalog never includes a skill's full Body — only the Skill tool (level
// 2) returns that.
func TestAgentService_BuildInstructions_SkillsCatalog_PresentWhenSkillsLoaded(t *testing.T) {
	svc := &AgentService{defs: Definitions{
		ProjectInstructions: "Project rules.",
		Skills:              testSkills(),
	}}

	got := svc.buildInstructions(agentsource.AgentDefinition{Instructions: "Agent persona."})

	assert.Contains(t, got, "Project rules.")
	assert.Contains(t, got, "Agent persona.")
	assert.Contains(t, got, "## Available Skills")
	assert.Contains(t, got, "pdf-processing: Extract text and tables from PDF files.")
	assert.NotContains(t, got, "Distinctive body content", "the catalog must list only name+description, never a skill's full body")
}

// TestAgentService_BuildInstructions_SkillsCatalog_AbsentWhenNoSkills proves
// no empty "## Available Skills" header is added when no skills are
// loaded, matching Design's own instruction not to clutter every agent's
// prompt with an empty section.
func TestAgentService_BuildInstructions_SkillsCatalog_AbsentWhenNoSkills(t *testing.T) {
	svc := &AgentService{defs: Definitions{ProjectInstructions: "Project rules."}}

	got := svc.buildInstructions(agentsource.AgentDefinition{Instructions: "Agent persona."})

	assert.NotContains(t, got, "Available Skills")
	assert.Equal(t, "Project rules.\n\nAgent persona.", got, "buildInstructions' pre-addendum behavior must be unchanged when there are no skills")
}

// TestAgentService_BuildInstructions_NoSkillsNoInstructions_StillEmpty
// guards the zero-value case: with nothing configured at all, the result
// must still be "", exactly matching buildInstructions' behavior before
// this addendum.
func TestAgentService_BuildInstructions_NoSkillsNoInstructions_StillEmpty(t *testing.T) {
	svc := &AgentService{}
	assert.Equal(t, "", svc.buildInstructions(agentsource.AgentDefinition{}))
}

// TestAgentService_BuildTools_SkillTool_PresentWhenSkillsLoaded proves the
// Skill tool (NewSkillTool) is included in the assembled tool list for an
// agent that inherits everything (no explicit "tools" allowlist) once at
// least one skill is loaded.
func TestAgentService_BuildTools_SkillTool_PresentWhenSkillsLoaded(t *testing.T) {
	svc := &AgentService{defs: Definitions{Skills: testSkills()}}

	got, err := svc.buildTools(agentsource.AgentDefinition{Name: "a"}, "execute")
	require.NoError(t, err)
	assert.True(t, containsToolName(got, "Skill"), "the Skill tool must be present when skills are loaded")
}

// TestAgentService_BuildTools_SkillTool_AbsentWhenNoSkills proves the
// inverse: an agent/project with no loaded skills gets no Skill tool at
// all, rather than a tool offering nothing to look up.
func TestAgentService_BuildTools_SkillTool_AbsentWhenNoSkills(t *testing.T) {
	svc := &AgentService{}

	got, err := svc.buildTools(agentsource.AgentDefinition{Name: "a"}, "execute")
	require.NoError(t, err)
	assert.False(t, containsToolName(got, "Skill"), "no Skill tool should be offered when no skills are loaded")
}

// TestAgentService_Skill_EndToEnd_ModelCallsToolAndGetsRealBody is the
// full end-to-end claim: a real turn (fake httptest provider) where the
// model calls the Skill tool by name and the tool's real result — the
// actual loaded skill's Body, not a hallucination — flows back into the
// final response.
func TestAgentService_Skill_EndToEnd_ModelCallsToolAndGetsRealBody(t *testing.T) {
	skills := testSkills()

	var callCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			args, err := json.Marshal(map[string]string{"name": "pdf-processing"})
			require.NoError(t, err)
			_ = json.NewEncoder(w).Encode(toolCallChatCompletionResponse("call-1", "Skill", string(args)))
			return
		}
		// Second call: toolautocall's follow-up completion after the Skill
		// tool actually executed. Read the tool result straight off the
		// request body (loosely typed, mirroring mcp_stream_test.go's own
		// request-body inspection, since a tool-result message's "content"
		// field shape is agent-framework-go's to decide, not this test's)
		// and echo it back in the final assistant text, so the test can
		// assert the *real* tool output (not a hallucinated one) made it
		// into the model's context.
		var req map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		msgs, _ := req["messages"].([]any)
		var toolResultContent string
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok || mm["role"] != "tool" {
				continue
			}
			if s, ok := mm["content"].(string); ok {
				toolResultContent = s
			}
		}
		require.NotEmpty(t, toolResultContent, "the request to the model must carry the Skill tool's real result")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse("skill body was: " + toolResultContent))
	}))
	t.Cleanup(server.Close)

	provider := entities.ProviderConfig{Name: "p", Type: entities.ProviderTypeOpenAI, APIKey: "sk-test", BaseURL: server.URL + "/v1"}
	model := entities.ModelConfig{Name: "m", Provider: provider.Name, ModelName: "gpt-test"}

	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant", Description: "test agent"}},
			Skills: skills,
		},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Repository:   repo,
	})

	ctx := context.Background()
	_, err = svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)

	resp, err := svc.RunText(ctx, "chat-1", "what does the pdf-processing skill do, use the tool to check")
	require.NoError(t, err)
	assert.Contains(t, resp.Response.String(), "Distinctive body content", "the final response must reflect the skill's real body, not a hallucination")
}

// TestAgentService_ListSkills_SortedNameDescription proves ListSkills
// returns every loaded skill's Name/Description, sorted — the same
// determinism guarantee ListAgents/ListModels already provide, used by the
// TUI's ctrl+s skills-browser overlay.
func TestAgentService_ListSkills_SortedNameDescription(t *testing.T) {
	svc := &AgentService{defs: Definitions{Skills: map[string]skillsource.SkillDefinition{
		"zeta":  {Name: "zeta", Description: "z"},
		"alpha": {Name: "alpha", Description: "a"},
	}}}

	got := svc.ListSkills()
	require.Len(t, got, 2)
	assert.Equal(t, "alpha", got[0].Name)
	assert.Equal(t, "a", got[0].Description)
	assert.Equal(t, "zeta", got[1].Name)
}

// TestAgentService_GetSkillBody proves GetSkillBody returns the exact
// loaded Body for a known skill and a clear error for an unknown one.
func TestAgentService_GetSkillBody(t *testing.T) {
	svc := &AgentService{defs: Definitions{Skills: testSkills()}}

	body, err := svc.GetSkillBody("pdf-processing")
	require.NoError(t, err)
	assert.Contains(t, body, "Distinctive body content")

	_, err = svc.GetSkillBody("ghost")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghost")
}
