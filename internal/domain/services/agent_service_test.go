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
	"github.com/drujensen/canopy/internal/impl/tools"
)

// chatCompletionResponse builds a minimal, valid OpenAI Chat Completions
// response body carrying the given assistant text, mirroring
// internal/impl/providers/factory_test.go's helper of the same shape, since
// AgentService is exercised against a real (stub) provider construction
// path rather than a hand-built agent.ProviderConfig, per this phase's
// testing guidance.
func chatCompletionResponse(text string) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion",
		"created": 1700000000,
		"model":   "test-model",
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": text},
				"finish_reason": "stop",
			},
		},
	}
}

func testProviderModel(t *testing.T, responseText string) (entities.ProviderConfig, entities.ModelConfig) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse(responseText))
	}))
	t.Cleanup(server.Close)

	provider := entities.ProviderConfig{
		Name:    "test-provider",
		Type:    entities.ProviderTypeOpenAI,
		APIKey:  "sk-test",
		BaseURL: server.URL + "/v1",
	}
	model := entities.ModelConfig{Name: "test-model", Provider: provider.Name, ModelName: "gpt-test"}
	return provider, model
}

// TestAgentService_RunText_RoundTrip asserts the end-to-end assembly path:
// StartChat persists a new chat, RunText builds the *agent.Agent for its
// AgentName and runs a turn, and the turn's messages land back on the
// persisted chat via impl/harness's ChatHistoryProvider wiring.
func TestAgentService_RunText_RoundTrip(t *testing.T) {
	provider, model := testProviderModel(t, "hello from agent service")
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{
				"assistant": {Name: "assistant", Description: "test agent", Instructions: "Be terse."},
			},
		},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Repository:   repo,
		Tools:        ToolsConfig{WorkingRoot: t.TempDir()},
	})

	ctx := context.Background()
	chat, err := svc.StartChat(ctx, "chat-1", "assistant")
	require.NoError(t, err)
	require.Equal(t, "assistant", chat.AgentName)

	resp, err := svc.RunText(ctx, "chat-1", "hi there")
	require.NoError(t, err)
	assert.Equal(t, "hello from agent service", resp.String())

	stored, err := repo.Get(ctx, "chat-1")
	require.NoError(t, err)
	require.Len(t, stored.Messages, 2)
	assert.Equal(t, "hi there", stored.Messages[0].String())
	assert.Equal(t, "hello from agent service", stored.Messages[1].String())

	// A second turn must see the first turn's messages via the same
	// round-trip mechanism internal/impl/harness's own test covers directly.
	resp2, err := svc.RunText(ctx, "chat-1", "and again")
	require.NoError(t, err)
	assert.Equal(t, "hello from agent service", resp2.String())
	stored2, err := repo.Get(ctx, "chat-1")
	require.NoError(t, err)
	assert.Len(t, stored2.Messages, 4)
}

// TestAgentService_StartChat_UnknownAgent asserts a clear error rather than
// silently creating a chat bound to a nonexistent agent definition.
func TestAgentService_StartChat_UnknownAgent(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	svc := NewAgentService(AgentServiceConfig{Repository: repo})

	_, err = svc.StartChat(context.Background(), "chat-1", "ghost")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghost")
}

// TestAgentService_BuildTools table-drives the tool-assembly logic (Design
// §3.2/§3.11): an empty allowlist inherits every core tool in a
// deterministic order; a non-empty allowlist resolves exactly those names;
// an unknown name is a clear error; WebSearch is only present when a
// backend is configured.
func TestAgentService_BuildTools(t *testing.T) {
	tests := []struct {
		name          string
		allowlist     []string
		withBackend   bool
		wantToolCount int
		wantErr       string
	}{
		{
			name:          "empty allowlist inherits every core tool",
			allowlist:     nil,
			withBackend:   true,
			wantToolCount: 7,
		},
		{
			name:          "empty allowlist without a search backend omits WebSearch",
			allowlist:     nil,
			withBackend:   false,
			wantToolCount: 6,
		},
		{
			name:          "explicit subset",
			allowlist:     []string{"FileRead", "Bash"},
			withBackend:   true,
			wantToolCount: 2,
		},
		{
			name:      "unknown tool name is an error",
			allowlist: []string{"NotARealTool"},
			wantErr:   "unknown or unavailable tool",
		},
		{
			name:      "explicit WebSearch without a configured backend is an error",
			allowlist: []string{"WebSearch"},
			wantErr:   "unknown or unavailable tool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &AgentService{toolsCfg: ToolsConfig{WorkingRoot: t.TempDir()}}
			if tt.withBackend {
				svc.toolsCfg.WebSearchBackend = stubBackend{}
			}
			def := agentsource.AgentDefinition{Name: "test-agent", Tools: tt.allowlist}

			got, err := svc.buildTools(def)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Len(t, got, tt.wantToolCount)
		})
	}
}

type stubBackend struct{}

func (stubBackend) Search(context.Context, string, int) ([]tools.WebSearchResult, error) {
	return nil, nil
}

// TestAgentService_ResolveProviderModel covers the model/provider resolution
// rules: an AgentDefinition's own "model" frontmatter override wins;
// otherwise AgentServiceConfig.DefaultModel is used; unknown model/provider
// references and a missing default are all clear errors.
func TestAgentService_ResolveProviderModel(t *testing.T) {
	p1 := entities.ProviderConfig{Name: "p1", Type: entities.ProviderTypeOpenAI}
	p2 := entities.ProviderConfig{Name: "p2", Type: entities.ProviderTypeAnthropic}
	m1 := entities.ModelConfig{Name: "m1", Provider: "p1", ModelName: "gpt-a"}
	m2 := entities.ModelConfig{Name: "m2", Provider: "p2", ModelName: "claude-a"}

	svc := NewAgentService(AgentServiceConfig{
		Providers:    []entities.ProviderConfig{p1, p2},
		Models:       []entities.ModelConfig{m1, m2},
		DefaultModel: "m1",
	})

	t.Run("uses default model when unset", func(t *testing.T) {
		gotP, gotM, err := svc.resolveProviderModel(agentsource.AgentDefinition{Name: "a"})
		require.NoError(t, err)
		assert.Equal(t, p1, gotP)
		assert.Equal(t, m1, gotM)
	})

	t.Run("agent's own model override wins", func(t *testing.T) {
		gotP, gotM, err := svc.resolveProviderModel(agentsource.AgentDefinition{Name: "a", Model: "m2"})
		require.NoError(t, err)
		assert.Equal(t, p2, gotP)
		assert.Equal(t, m2, gotM)
	})

	t.Run("unknown model reference is an error", func(t *testing.T) {
		_, _, err := svc.resolveProviderModel(agentsource.AgentDefinition{Name: "a", Model: "ghost"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ghost")
	})

	t.Run("missing default and no override is an error", func(t *testing.T) {
		bare := NewAgentService(AgentServiceConfig{Providers: []entities.ProviderConfig{p1}, Models: []entities.ModelConfig{m1}})
		_, _, err := bare.resolveProviderModel(agentsource.AgentDefinition{Name: "a"})
		require.Error(t, err)
	})
}

// TestAgentService_BuildTopLevelAgent_WrapsOtherAgentsAsSubagentTools
// asserts the assembly logic FR9/Design §3.4 depends on: when Definitions
// has more than one loaded agent, building the top-level agent for one of
// them adds a tool/agenttool-wrapped tool for every *other* loaded agent
// (and none for itself). The lower-level claim that dispatching through
// such a tool actually runs in an isolated session is verified separately
// and more thoroughly by TestSubagentDispatch_SessionIsolation; this test
// only checks that AgentService wires the mechanism at all.
func TestAgentService_BuildTopLevelAgent_WrapsOtherAgentsAsSubagentTools(t *testing.T) {
	provider, model := testProviderModel(t, "ok")
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{
				"parent": {Name: "parent", Description: "parent agent"},
				"helper": {Name: "helper", Description: "helper agent"},
				"other":  {Name: "other", Description: "other agent"},
			},
		},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Repository:   repo,
		Tools:        ToolsConfig{WorkingRoot: t.TempDir()},
	})

	ctx := context.Background()

	toolList, err := svc.buildTopLevelTools(ctx, agentsource.AgentDefinition{Name: "parent", Description: "parent agent"})
	require.NoError(t, err)

	// 6 core tools (no WebSearchBackend configured here, so WebSearch is
	// omitted) + 2 subagent tools (helper, other) — parent must not wrap
	// itself.
	assert.Len(t, toolList, 8)

	var wrappedNames []string
	for _, tl := range toolList {
		wrappedNames = append(wrappedNames, tl.Name())
	}
	assert.Contains(t, wrappedNames, "helper")
	assert.Contains(t, wrappedNames, "other")
	assert.NotContains(t, wrappedNames, "parent")
}

// TestAgentService_BuildInstructions asserts project instructions and an
// agent's own body instructions are concatenated project-first, matching
// projectcontext's own ordering convention (Design §3.11).
func TestAgentService_BuildInstructions(t *testing.T) {
	svc := &AgentService{defs: Definitions{ProjectInstructions: "Project rules."}}
	got := svc.buildInstructions(agentsource.AgentDefinition{Instructions: "Agent persona."})
	assert.Equal(t, "Project rules.\n\nAgent persona.", got)

	svc2 := &AgentService{}
	assert.Equal(t, "Agent persona.", svc2.buildInstructions(agentsource.AgentDefinition{Instructions: "Agent persona."}))
}
