// Package providers builds a *agent.Agent from Canopy's own
// ProviderConfig/ModelConfig configuration (Design §4). It is the only place
// in Canopy that constructs provider SDK clients (openai-go,
// anthropic-sdk-go, google.golang.org/genai) and hands them to the
// agent-framework-go per-provider constructors.
package providers

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/provider/anthropicprovider"
	"github.com/microsoft/agent-framework-go/provider/geminiprovider"
	"github.com/microsoft/agent-framework-go/provider/openaiprovider"
	"github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"
	"google.golang.org/genai"

	"github.com/drujensen/canopy/internal/domain/entities"
)

// New constructs a *agent.Agent for the given provider/model configuration,
// dispatching on cfg.Type (Design §4). agentCfg carries the caller's
// agent.Config (name, tools, context providers, etc.) unchanged; only the
// provider-specific client and Model get filled in here.
//
// ctx is used only to construct the Gemini client (genai.NewClient requires
// one); the OpenAI and Anthropic clients are constructed synchronously and
// ignore it.
//
// Dispatch (post-v0.1.0 addendum, Design §4): the 9 named ProviderType
// consts still get their own explicit case below, same as before. Any other
// cfg.Type — including one Canopy has never heard of, e.g. auto-detected
// straight from the models.dev catalog's own provider ID string (see
// config.DetectProviders) — now also succeeds via the generic
// newOpenAICompatible path as long as cfg.BaseURL is set, on the same
// rationale that already justified sharing that one path across DeepSeek,
// Ollama, Groq, Mistral, Together, and xAI: the large majority of providers
// only implement the OpenAI Chat Completions wire format. Only an
// unrecognized type with no BaseURL is a real error — there's genuinely no
// endpoint to call.
func New(ctx context.Context, cfg entities.ProviderConfig, model entities.ModelConfig, agentCfg agent.Config) (*agent.Agent, error) {
	switch cfg.Type {
	case entities.ProviderTypeOpenAI:
		return newOpenAINative(cfg, model, agentCfg), nil

	case entities.ProviderTypeAnthropic:
		return newAnthropic(cfg, model, agentCfg), nil

	case entities.ProviderTypeGemini:
		return newGemini(ctx, cfg, model, agentCfg)

	case entities.ProviderTypeDeepSeek,
		entities.ProviderTypeOllama,
		entities.ProviderTypeGroq,
		entities.ProviderTypeMistral,
		entities.ProviderTypeTogether,
		entities.ProviderTypeXAI:
		return newOpenAICompatible(cfg, model, agentCfg)

	default:
		if cfg.BaseURL != "" {
			return newOpenAICompatible(cfg, model, agentCfg)
		}
		return nil, fmt.Errorf("providers: unrecognized provider type %q for provider %q (no base_url configured, so there's no endpoint to call)", cfg.Type, cfg.Name)
	}
}

// newOpenAINative builds an OpenAI-backed agent via the Chat Completions API
// (openaiprovider.NewChatCompletionsAgent, not the Responses API — Design
// §4/PLAN.md Phase 2 keeps native OpenAI on the same API surface as the
// OpenAI-compatible adapter below). cfg.BaseURL is honored when set (e.g.
// pointing at Azure OpenAI or a test server) but is not required.
func newOpenAINative(cfg entities.ProviderConfig, model entities.ModelConfig, agentCfg agent.Config) *agent.Agent {
	opts := []openaioption.RequestOption{openaioption.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		opts = append(opts, openaioption.WithBaseURL(cfg.BaseURL))
	}
	client := openai.NewClient(opts...)
	return openaiprovider.NewChatCompletionsAgent(client, openaiprovider.AgentConfig{
		Config: agentCfg,
		Model:  model.ModelName,
	})
}

// newAnthropic builds an Anthropic-backed agent via anthropicprovider.NewAgent.
func newAnthropic(cfg entities.ProviderConfig, model entities.ModelConfig, agentCfg agent.Config) *agent.Agent {
	opts := []anthropicoption.RequestOption{anthropicoption.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		opts = append(opts, anthropicoption.WithBaseURL(cfg.BaseURL))
	}
	client := anthropic.NewClient(opts...)
	return anthropicprovider.NewAgent(client, anthropicprovider.AgentConfig{
		Config: agentCfg,
		Model:  model.ModelName,
	})
}

// newGemini builds a Google Gemini-backed agent via geminiprovider.NewAgent.
func newGemini(ctx context.Context, cfg entities.ProviderConfig, model entities.ModelConfig, agentCfg agent.Config) (*agent.Agent, error) {
	clientCfg := &genai.ClientConfig{APIKey: cfg.APIKey}
	if cfg.BaseURL != "" {
		clientCfg.HTTPOptions = genai.HTTPOptions{BaseURL: cfg.BaseURL}
	}
	client, err := genai.NewClient(ctx, clientCfg)
	if err != nil {
		return nil, fmt.Errorf("providers: failed to construct gemini client for provider %q: %w", cfg.Name, err)
	}
	return geminiprovider.NewAgent(client, geminiprovider.AgentConfig{
		Config: agentCfg,
		Model:  model.ModelName,
	}), nil
}
