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
// copied from the catalog's "api" field.
//
// Type mapping: the 3 catalog providers Canopy has a native adapter for
// (openai, anthropic, google — see nativeProviderTypes) get the matching
// entities.ProviderTypeXxx const; every other catalog provider uses its own
// catalog ID string directly as Type, which impl/providers.New's
// generalized fallback dispatch now supports. A provider is skipped
// (omitted from the result, with a reason appended to skipReasons) when it
// would be unusable via that dispatch: not one of the 3 native types, and
// no BaseURL in the catalog to call.
//
// Model selection: from a detected provider's Models, candidates are
// filtered to ToolCall == true (Canopy's whole architecture is
// tool-calling-based — a model that can't call tools isn't usable here),
// then sorted by ReleaseDate descending (most recent first; unparseable
// dates are skipped rather than erroring), then by model ID ascending as a
// deterministic tiebreaker for same-day releases (map iteration order is
// otherwise randomized in Go). The first candidate wins. A provider with
// zero tool-call-capable models is skipped (reason appended to
// skipReasons), not added with a broken/empty model.
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

		providerType, isNative := nativeProviderTypes[id]
		if !isNative {
			providerType = entities.ProviderType(id)
			if provider.BaseURL == "" {
				// Would dispatch to impl/providers.New's default case with
				// no BaseURL and error there — don't emit a config that
				// can never actually place a request.
				skipReasons = append(skipReasons, id+": "+matchedEnvVar+" is set, but models.dev has no base URL for this provider and it isn't one of Canopy's natively-adapted types")
				continue
			}
		}

		model, ok := pickDefaultModel(provider)
		if !ok {
			skipReasons = append(skipReasons, id+": "+matchedEnvVar+" is set, but this provider has no tool-call-capable model in the catalog")
			continue
		}

		name := providerDisplayName(provider)

		file.Providers = append(file.Providers, entities.ProviderConfig{
			Name:      name,
			Type:      providerType,
			APIKeyEnv: matchedEnvVar,
			BaseURL:   provider.BaseURL,
		})
		file.Models = append(file.Models, entities.ModelConfig{
			Name:                name,
			Provider:            name,
			ModelName:           model.ID,
			ContextWindowTokens: model.Limit.Context,
		})
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

// pickDefaultModel selects provider's best default model per
// DetectProviders' doc comment: tool-call-capable, most recently released,
// model ID as a deterministic tiebreaker.
func pickDefaultModel(provider modelsdev.ProviderData) (modelsdev.ModelData, bool) {
	var candidates []modelsdev.ModelData
	for _, m := range provider.Models {
		if m.ToolCall {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
		return modelsdev.ModelData{}, false
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
	return candidates[0], true
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
