package services

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/impl/agentsource"
	jsonrepo "github.com/drujensen/canopy/internal/impl/repositories/json"
	"github.com/drujensen/canopy/internal/impl/tools"
)

// fakeMCPTool is a minimal tool.Tool stand-in for a tool an MCP server would
// expose, used to test AgentService's MCP-tool assembly policy
// (TestAgentService_BuildTools_MCPTools) without needing a real
// mcpclient.Client/Registry — that round trip is already covered end-to-end
// by internal/impl/mcpclient's own tests. Deliberately does not implement
// tool.FuncTool/tool.ApprovalRequiredTool since buildTools never calls or
// introspects a tool beyond Name(); mcpclient.Connect's own tests cover the
// ApprovalRequiredFunc-wrapping guarantee.
type fakeMCPTool struct{ name string }

func (f fakeMCPTool) Name() string        { return f.name }
func (f fakeMCPTool) Description() string { return "a fake MCP-provided tool for tests" }

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
	assert.Equal(t, "hello from agent service", resp.Response.String())

	stored, err := repo.Get(ctx, "chat-1")
	require.NoError(t, err)
	require.Len(t, stored.Messages, 2)
	assert.Equal(t, "hi there", stored.Messages[0].String())
	assert.Equal(t, "hello from agent service", stored.Messages[1].String())

	// A second turn must see the first turn's messages via the same
	// round-trip mechanism internal/impl/harness's own test covers directly.
	resp2, err := svc.RunText(ctx, "chat-1", "and again")
	require.NoError(t, err)
	assert.Equal(t, "hello from agent service", resp2.Response.String())
	stored2, err := repo.Get(ctx, "chat-1")
	require.NoError(t, err)
	assert.Len(t, stored2.Messages, 4)
}

// TestAgentService_StartChat_RecordsLastAgent asserts StartChat calls
// AgentServiceConfig.RecordLastAgent (post-v0.1.0 addendum, Design §5's
// addendum) with the started chat's agent name, on success.
func TestAgentService_StartChat_RecordsLastAgent(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)

	var recorded []string
	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}}},
		Repository:  repo,
		RecordLastAgent: func(name string) error {
			recorded = append(recorded, name)
			return nil
		},
	})

	_, err = svc.StartChat(context.Background(), "chat-1", "assistant")
	require.NoError(t, err)
	assert.Equal(t, []string{"assistant"}, recorded)
}

// TestAgentService_StartChat_NilRecordLastAgent_NoPanic asserts the default
// (every AgentServiceConfig that predates this feature, and every other
// existing test) — no RecordLastAgent set at all — is a safe no-op, not a
// nil-func-call panic.
func TestAgentService_StartChat_NilRecordLastAgent_NoPanic(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}}},
		Repository:  repo,
	})

	_, err = svc.StartChat(context.Background(), "chat-1", "assistant")
	require.NoError(t, err)
}

// TestAgentService_StartChat_RecordLastAgentError_DoesNotFailStartChat
// asserts remembering the last-used agent is genuinely best-effort (see
// RecordLastAgent's doc comment): a failure writing that state must never
// fail an otherwise-successful StartChat call.
func TestAgentService_StartChat_RecordLastAgentError_DoesNotFailStartChat(t *testing.T) {
	repo, err := jsonrepo.NewChatRepository(t.TempDir())
	require.NoError(t, err)
	svc := NewAgentService(AgentServiceConfig{
		Definitions:     Definitions{Agents: map[string]agentsource.AgentDefinition{"assistant": {Name: "assistant"}}},
		Repository:      repo,
		RecordLastAgent: func(name string) error { return errors.New("disk full") },
	})

	_, err = svc.StartChat(context.Background(), "chat-1", "assistant")
	require.NoError(t, err, "a RecordLastAgent failure must not fail StartChat")
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

// TestAgentService_BuildTools_MCPTools covers the MCP-tool assembly policy
// this phase adds to buildTools (Design §3.11, Requirements FR6/FR18): MCP
// tools follow the exact same allowlist rules buildTools already applies to
// core tools — an empty allowlist inherits every MCP tool alongside every
// core tool, a non-empty allowlist requires an MCP tool to be named
// explicitly by its plain (server-documented) name. containsToolName is
// defined in phase5_test.go.
func TestAgentService_BuildTools_MCPTools(t *testing.T) {
	newSvc := func(t *testing.T) *AgentService {
		t.Helper()
		return NewAgentService(AgentServiceConfig{
			Tools:    ToolsConfig{WorkingRoot: t.TempDir()},
			MCPTools: map[string]tool.Tool{"search_docs": fakeMCPTool{name: "search_docs"}},
		})
	}

	t.Run("no explicit allowlist inherits the MCP tool alongside core tools", func(t *testing.T) {
		svc := newSvc(t)
		got, err := svc.buildTools(agentsource.AgentDefinition{Name: "assistant"})
		require.NoError(t, err)
		// 6 core tools (no WebSearchBackend configured, so WebSearch is
		// omitted, matching TestAgentService_BuildTools' own count for that
		// case) + 1 MCP tool.
		assert.Len(t, got, 7)
		assert.True(t, containsToolName(got, "search_docs"))
	})

	t.Run("explicit allowlist not naming the MCP tool omits it", func(t *testing.T) {
		svc := newSvc(t)
		got, err := svc.buildTools(agentsource.AgentDefinition{Name: "assistant", Tools: []string{"FileRead"}})
		require.NoError(t, err)
		assert.Len(t, got, 1)
		assert.False(t, containsToolName(got, "search_docs"))
	})

	t.Run("explicit allowlist naming the MCP tool includes it", func(t *testing.T) {
		svc := newSvc(t)
		got, err := svc.buildTools(agentsource.AgentDefinition{Name: "assistant", Tools: []string{"FileRead", "search_docs"}})
		require.NoError(t, err)
		assert.Len(t, got, 2)
		assert.True(t, containsToolName(got, "search_docs"))
	})
}

// TestAgentService_BuildTools_MCPToolCollidesWithCoreToolName asserts a
// same-named MCP tool doesn't shadow a core built-in tool (see
// AgentServiceConfig.MCPTools' doc comment): the core tool wins, and
// buildTools' behavior for that name is unaffected by the colliding MCP
// entry.
func TestAgentService_BuildTools_MCPToolCollidesWithCoreToolName(t *testing.T) {
	svc := NewAgentService(AgentServiceConfig{
		Tools:    ToolsConfig{WorkingRoot: t.TempDir()},
		MCPTools: map[string]tool.Tool{"Bash": fakeMCPTool{name: "Bash"}},
	})

	got, err := svc.buildTools(agentsource.AgentDefinition{Name: "assistant"})
	require.NoError(t, err)
	// 6 core tools (no WebSearchBackend) and no extra entry for the
	// colliding "Bash" MCP tool — it must not be double-counted or replace
	// the real Bash tool.
	assert.Len(t, got, 6)
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
		gotP, gotM, err := svc.resolveProviderModel(agentsource.AgentDefinition{Name: "a"}, "")
		require.NoError(t, err)
		assert.Equal(t, p1, gotP)
		assert.Equal(t, m1, gotM)
	})

	t.Run("agent's own model override wins", func(t *testing.T) {
		gotP, gotM, err := svc.resolveProviderModel(agentsource.AgentDefinition{Name: "a", Model: "m2"}, "")
		require.NoError(t, err)
		assert.Equal(t, p2, gotP)
		assert.Equal(t, m2, gotM)
	})

	t.Run("unknown model reference is an error", func(t *testing.T) {
		_, _, err := svc.resolveProviderModel(agentsource.AgentDefinition{Name: "a", Model: "ghost"}, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ghost")
	})

	t.Run("missing default and no override is an error", func(t *testing.T) {
		bare := NewAgentService(AgentServiceConfig{Providers: []entities.ProviderConfig{p1}, Models: []entities.ModelConfig{m1}})
		_, _, err := bare.resolveProviderModel(agentsource.AgentDefinition{Name: "a"}, "")
		require.Error(t, err)
	})

	t.Run("chat model override (post-v0.1.0) wins over the agent's own model", func(t *testing.T) {
		gotP, gotM, err := svc.resolveProviderModel(agentsource.AgentDefinition{Name: "a", Model: "m1"}, "m2")
		require.NoError(t, err)
		assert.Equal(t, p2, gotP)
		assert.Equal(t, m2, gotM, "an explicit chat override must win over the agent definition's own model: frontmatter")
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

// TestAgentService_BuildTopLevelTools_SkipsMisconfiguredOtherAgent is a
// regression test for a real bug: buildTopLevelTools' "wrap every other
// agent as a subagent-dispatch tool" loop used to propagate a failure
// building *any* other agent as an outright error, meaning one agent
// referencing an unavailable tool (e.g. "Skill" with zero skills loaded, or
// "WebSearch" with no WebSearchBackend configured) broke starting *every*
// agent, including ones with nothing wrong with them. Now that failure is
// logged and the misconfigured agent is skipped as a subagent-dispatch
// option — "parent" must still build successfully, wrapping only the agent
// that's actually fine.
func TestAgentService_BuildTopLevelTools_SkipsMisconfiguredOtherAgent(t *testing.T) {
	provider, model := testProviderModel(t, "ok")
	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{
				"parent": {Name: "parent", Description: "parent agent"},
				"fine":   {Name: "fine", Description: "a normally-configured agent"},
				"broken": {Name: "broken", Description: "references a tool that isn't available", Tools: []string{"Skill"}},
			},
		},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Tools:        ToolsConfig{WorkingRoot: t.TempDir()}, // no skills loaded, so "Skill" is unavailable
	})
	ctx := context.Background()

	toolList, err := svc.buildTopLevelTools(ctx, agentsource.AgentDefinition{Name: "parent", Description: "parent agent"})
	require.NoError(t, err, "one misconfigured other agent must not break building parent's own tool list")

	var wrappedNames []string
	for _, tl := range toolList {
		wrappedNames = append(wrappedNames, tl.Name())
	}
	assert.Contains(t, wrappedNames, "fine", "the working other agent must still be wrapped as a subagent-dispatch tool")
	assert.NotContains(t, wrappedNames, "broken", "the misconfigured other agent must be skipped, not included half-built")
}

// TestAgentService_BuildTopLevelTools_OwnMisconfiguredToolsStillErrors
// proves the fix above didn't over-correct: a user directly selecting an
// agent whose *own* tools: allowlist references something unavailable must
// still get a clear, immediate error — only the eager "wrap other agents"
// loop became non-fatal, not def's own tool-list resolution.
func TestAgentService_BuildTopLevelTools_OwnMisconfiguredToolsStillErrors(t *testing.T) {
	svc := NewAgentService(AgentServiceConfig{
		Definitions: Definitions{
			Agents: map[string]agentsource.AgentDefinition{
				"broken": {Name: "broken", Description: "references a tool that isn't available", Tools: []string{"Skill"}},
			},
		},
		Tools: ToolsConfig{WorkingRoot: t.TempDir()},
	})
	ctx := context.Background()

	_, err := svc.buildTopLevelTools(ctx, agentsource.AgentDefinition{Name: "broken", Description: "references a tool that isn't available", Tools: []string{"Skill"}})
	require.Error(t, err, "an agent directly selected with its own unavailable tool must still fail loudly")
	assert.Contains(t, err.Error(), "Skill")
}

// TestAgentService_DefaultSDLCAgents_BuildOnZeroSkillsConfigured is an
// end-to-end regression test for the exact bug reported in practice:
// building "general" (or any other default agent) used to fail with
// `agent service: building subagent "research" for "general": agent
// service: agent "research" references unknown or unavailable tool
// "Skill"` on any install with zero skills configured, because
// research/design/plan's generated tools: allowlist named "Skill" (only
// available once at least one skill is loaded) and buildTopLevelTools used
// to propagate a failure building any *other* agent as a hard error. Uses
// agentsource.WriteDefaults' real generated content — not a hand-rolled
// AgentDefinition — so this exercises exactly what a fresh, zero-config
// install actually ships. A WebSearchBackend is configured (stubBackend),
// matching the reporter's real environment (TAVILY_API_KEY set) — the
// three SDLC agents also name WebSearch in their allowlist, and correctly
// still fail loudly if selected directly with no backend configured (see
// TestAgentService_BuildTopLevelTools_OwnMisconfiguredToolsStillErrors);
// that's intentional, by-design behavior this test isn't meant to cover.
func TestAgentService_DefaultSDLCAgents_BuildOnZeroSkillsConfigured(t *testing.T) {
	homeDir := t.TempDir()
	require.NoError(t, agentsource.WriteDefaults(filepath.Join(homeDir, ".canopy", "agents")))
	agents, err := agentsource.Load(t.TempDir(), homeDir)
	require.NoError(t, err)
	require.Len(t, agents, 5, "general, research, design, plan, execute")

	provider, model := testProviderModel(t, "ok")
	svc := NewAgentService(AgentServiceConfig{
		Definitions:  Definitions{Agents: agents},
		Providers:    []entities.ProviderConfig{provider},
		Models:       []entities.ModelConfig{model},
		DefaultModel: model.Name,
		Tools: ToolsConfig{
			WorkingRoot:      t.TempDir(),
			WebSearchBackend: stubBackend{}, // matches the reporter's real environment
		}, // no Skills configured — the exact reported scenario
	})
	ctx := context.Background()

	for name, def := range agents {
		_, err := svc.buildTopLevelTools(ctx, def)
		assert.NoError(t, err, "agent %q must build successfully with zero skills configured", name)
	}
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
