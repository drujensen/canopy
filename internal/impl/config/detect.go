package config

import (
	"sort"
	"strings"
	"time"

	"github.com/drujensen/canopy/internal/domain/entities"
	"github.com/drujensen/canopy/internal/impl/modelsdev"
)

// nativeProviderTypes maps a models.dev catalog provider ID to the
// entities.ProviderType Canopy has a native SDK adapter for (Design §4).
// Verified against a live fetch of https://models.dev/api.json: the
// catalog's own ID for Google's Gemini models is "google", not "gemini" —
// Canopy's ProviderTypeGemini const ("gemini") is the right value to assign
// on the resulting ProviderConfig, it's only the *catalog key* that differs.
// Every other catalog provider ID is used verbatim as ProviderType (see
// impl/providers.New's generalized fallback dispatch, post-v0.1.0
// addendum) — no other catalog provider needs a translation table entry.
var nativeProviderTypes = map[string]entities.ProviderType{
	"openai":    entities.ProviderTypeOpenAI,
	"anthropic": entities.ProviderTypeAnthropic,
	"google":    entities.ProviderTypeGemini,
}

// fallbackBaseURLs is a small, deliberately minimal table of well-known,
// stable API base URLs for catalog providers whose models.dev entry omits
// the "api" field entirely (confirmed against a live fetch of
// https://models.dev/api.json on 2026-08-15: xai, groq, mistral, and
// togetherai all have "env" set but no "api" — see ProviderData's doc
// comment). This is a catalog-gap-filling fallback only, never an override:
// DetectProviders consults it only when provider.BaseURL is empty, so any
// base URL the catalog does supply always wins. It is NOT a general
// mechanism for guessing at providers Canopy isn't confident about — every
// entry here was individually verified (web search against the provider's
// own docs, and a live unauthenticated request confirming the host answers
// with 401 rather than a connection/DNS failure) to be that provider's real,
// current, OpenAI-compatible Chat Completions base URL:
//
//   - xai: https://api.x.ai/v1 — per docs.x.ai, OpenAI SDK-compatible.
//   - groq: https://api.groq.com/openai/v1 — per console.groq.com/docs/openai.
//   - mistral: https://api.mistral.ai/v1 — per docs.mistral.ai/api.
//   - togetherai: https://api.together.ai/v1 — per
//     docs.together.ai/docs/inference/openai-compatibility (the current
//     canonical domain; the older api.together.xyz/v1 also still answers,
//     but .ai is what Together's own docs show today).
//
// A provider legitimately staying skipped (with a clear skipReasons message)
// is safer than a wrong hardcoded URL silently misdirecting API calls, so
// don't add an entry here without that same level of verification.
var fallbackBaseURLs = map[string]string{
	"xai":        "https://api.x.ai/v1",
	"groq":       "https://api.groq.com/openai/v1",
	"mistral":    "https://api.mistral.ai/v1",
	"togetherai": "https://api.together.ai/v1",
}

// releaseDateLayout is the "YYYY-MM-DD" layout the large majority of
// models.dev models use for release_date. Some (~3% in a live fetch) use a
// coarser "YYYY-MM"; those are skipped rather than erroring (see
// DetectProviders' doc comment).
const releaseDateLayout = "2006-01-02"

// DetectProviders turns "which provider API-key env vars are actually set"
// plus the models.dev catalog into a ready-to-save ProvidersFile — Canopy's
// zero-config auto-detection (post-v0.1.0 addendum to Requirements
// FR1-FR3/Design §4), the provider-config analogue of
// agentsource.WriteDefault's zero-config default agent.
//
// For each catalog provider: if any of its Env names are set (non-empty) in
// environ (pass os.Environ() from the caller; kept as a parameter so this
// stays testable without mutating real process env), a ProviderConfig is
// built with APIKeyEnv set to whichever Env name matched first (never the
// literal key — see entities.ProviderConfig's doc comment) and BaseURL
// taken from the catalog's "api" field, falling back to fallbackBaseURLs
// when the catalog omits it (see that var's doc comment — catalog data
// always wins when present).
//
// Type mapping: the 3 catalog providers Canopy has a native adapter for
// (openai, anthropic, google — see nativeProviderTypes) get the matching
// entities.ProviderTypeXxx const; every other catalog provider uses its own
// catalog ID string directly as Type, which impl/providers.New's
// generalized fallback dispatch now supports. A provider is skipped
// (omitted from the result, with a reason appended to skipReasons) when it
// would be unusable via that dispatch: not one of the 3 native types, and
// no BaseURL available (catalog or fallback table) to call.
//
// Model selection (post-v0.1.0 addendum: "all models," not just one — the
// user explicitly wants every tool-call-capable model of a detected
// provider available in the ctrl+o model-switcher, not just whichever one
// this heuristic likes best): from a detected provider's Models, candidates
// are filtered to ToolCall == true (Canopy's whole architecture is
// tool-calling-based — a model that can't call tools isn't usable here),
// then sorted by ReleaseDate descending (most recent first; unparseable
// dates are skipped rather than erroring), then by model ID ascending as a
// deterministic tiebreaker for same-day releases (map iteration order is
// otherwise randomized in Go). Every candidate becomes its own
// entities.ModelConfig, appended to file.Models in that sorted order — so
// the most-recently-released tool-call-capable model is always first. This
// matters beyond readability: cmd/canopy's zero-config path picks
// providersFile.Models[0].Name as the implicit DefaultModel, and relies on
// "first" already being a deliberately-chosen sensible default rather than
// an arbitrary one; DetectProviders preserves that guarantee by construction
// instead of cmd/canopy needing to know anything about it. A provider with
// zero tool-call-capable models is skipped entirely (reason appended to
// skipReasons), not added with a broken/empty model.
//
// Model naming: each ModelConfig.Name must be unique within the file — it's
// the key the ctrl+o model-switcher and AgentDefinition's "model:"
// frontmatter reference (see entities.ModelConfig's doc comment). Names are
// "<provider-id>/<model-id>" (e.g. "anthropic/claude-opus-4-5"), which stays
// readable in the picker, is deterministic, and is collision-free: model IDs
// are already unique within one provider's catalog entry (it's a Go map
// keyed by ID), and the "<provider-id>/" prefix keeps two different
// providers' models from colliding with each other even if they happened to
// share a model ID string.
//
// detected is the list of provider names actually added to the result, for
// caller-side logging (Requirements: cmd/canopy prints which providers were
// auto-configured and from which env var, never key values).
func DetectProviders(catalog *modelsdev.Catalog, environ []string) (file ProvidersFile, detected []string, skipReasons []string) {
	if catalog == nil {
		return ProvidersFile{}, nil, nil
	}

	env := parseEnviron(environ)

	// Deterministic iteration order: Catalog is a map, and map iteration
	// order is randomized in Go — without sorting, output order (and thus
	// "first configured provider" in any downstream default-model choice)
	// would vary run to run.
	ids := make([]string, 0, len(*catalog))
	for id := range *catalog {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		provider := (*catalog)[id]

		matchedEnvVar := ""
		for _, name := range provider.Env {
			if v, ok := env[name]; ok && v != "" {
				matchedEnvVar = name
				break
			}
		}
		if matchedEnvVar == "" {
			continue // env var not set: not a candidate at all, not a "skip"
		}

		baseURL := provider.BaseURL
		if baseURL == "" {
			baseURL = fallbackBaseURLs[id]
		}

		providerType, isNative := nativeProviderTypes[id]
		if !isNative {
			providerType = entities.ProviderType(id)
			if baseURL == "" {
				// Would dispatch to impl/providers.New's default case with
				// no BaseURL and error there — don't emit a config that
				// can never actually place a request.
				skipReasons = append(skipReasons, id+": "+matchedEnvVar+" is set, but models.dev has no base URL for this provider (and none in Canopy's fallback table either), and it isn't one of Canopy's natively-adapted types")
				continue
			}
		}

		models := pickToolCallModels(provider)
		if len(models) == 0 {
			skipReasons = append(skipReasons, id+": "+matchedEnvVar+" is set, but this provider has no tool-call-capable model in the catalog")
			continue
		}

		name := providerDisplayName(provider)

		file.Providers = append(file.Providers, entities.ProviderConfig{
			Name:      name,
			Type:      providerType,
			APIKeyEnv: matchedEnvVar,
			BaseURL:   baseURL,
		})
		for _, model := range models {
			file.Models = append(file.Models, entities.ModelConfig{
				Name:                       name + "/" + model.ID,
				Provider:                   name,
				ModelName:                  model.ID,
				ContextWindowTokens:        model.Limit.Context,
				InputCostPerMillionTokens:  model.Cost.Input,
				OutputCostPerMillionTokens: model.Cost.Output,
			})
		}
		detected = append(detected, name)
	}

	return file, detected, skipReasons
}

// providerDisplayName picks the catalog's own provider ID as the
// ProviderConfig.Name: it's stable and JSON/reference-safe (no spaces or
// mixed case to worry about round-tripping), unlike the catalog's "name"
// field (e.g. "OpenAI", "xAI") which exists purely for display elsewhere.
func providerDisplayName(provider modelsdev.ProviderData) string {
	if provider.ID != "" {
		return provider.ID
	}
	return provider.Name
}

// pickToolCallModels returns every tool-call-capable model in provider
// (Canopy's whole architecture is tool-calling-based — a model that can't
// call tools isn't usable here), sorted most-recently-released first (model
// ID ascending as a deterministic tiebreaker for same-day releases or
// unparseable dates — map iteration order is otherwise randomized in Go).
// Callers get every candidate, not just one, per the "I was expecting to
// see all the models available for that provider" feedback that prompted
// this — DetectProviders adds one entities.ModelConfig per entry returned
// here, so all of them end up selectable via ctrl+o. The first entry is
// still a deliberately-good default: it's what main.go's "first model in
// the file wins" default-selection logic picks when nothing else overrides
// it, so recency-first ordering here is load-bearing, not cosmetic.
func pickToolCallModels(provider modelsdev.ProviderData) []modelsdev.ModelData {
	var candidates []modelsdev.ModelData
	for _, m := range provider.Models {
		if m.ToolCall {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		ti, erri := time.Parse(releaseDateLayout, candidates[i].ReleaseDate)
		tj, errj := time.Parse(releaseDateLayout, candidates[j].ReleaseDate)
		oki, okj := erri == nil, errj == nil
		switch {
		case oki && okj && !ti.Equal(tj):
			return ti.After(tj)
		case oki && !okj:
			return true // a parseable date outranks an unparseable one
		case !oki && okj:
			return false
		default:
			return candidates[i].ID < candidates[j].ID // deterministic tiebreak
		}
	})
	return candidates
}

// parseEnviron turns a "KEY=VALUE" slice (the shape of os.Environ()) into a
// lookup map, so DetectProviders can be tested without touching real
// process environment variables.
func parseEnviron(environ []string) map[string]string {
	m := make(map[string]string, len(environ))
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		m[k] = v
	}
	return m
}
