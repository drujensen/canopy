package config

// last_agent.json (post-v0.1.0 addendum) records which agent was most
// recently made active in this project (or globally, mirroring
// providers.json's own --global/-g convention) — read at startup so a new
// session can resume it automatically instead of always landing on the
// agent picker (Design §5's addendum has the full auto-resume/fallback
// design). This file deliberately has no path-resolution helpers of its own
// (unlike ProviderStore's NewGlobalProviderStore/NewProjectProviderStore):
// cmd/canopy already resolves providerStore.Path() once and derives every
// other per-project state file (models-cache.json, chats/, canopy.log) from
// filepath.Dir(providerStore.Path()) — last_agent.json is one more sibling
// in that same directory, not a fourth resolution scheme.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// lastAgentFile is the on-disk shape: deliberately just the one field, kept
// as a small JSON object rather than a bare string file so it can grow
// (e.g. a timestamp) later without an incompatible format change.
type lastAgentFile struct {
	AgentName string `json:"agent_name"`
}

// LoadLastAgent returns the agent name last recorded via SaveLastAgent at
// path, or "" if none has ever been recorded. A missing file is not an
// error — it returns ("", nil), the same "not there yet" convention
// modelsdev.Load and ProviderStore.Load already use — since every project's
// first-ever run has no last-used agent to report.
func LoadLastAgent(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("config: failed to read last-used agent %q: %w", path, err)
	}
	var f lastAgentFile
	if err := json.Unmarshal(data, &f); err != nil {
		return "", fmt.Errorf("config: failed to decode last-used agent %q: %w", path, err)
	}
	return f.AgentName, nil
}

// SaveLastAgent records name as the most recently used agent at path,
// creating path's parent directory if needed (the same posture
// modelsdev.Save takes — this can be the very first file ever written into
// a project's .canopy directory, e.g. on a project using only the global
// ~/.canopy store so far).
func SaveLastAgent(path, name string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("config: failed to create directory %q for last-used agent: %w", dir, err)
	}
	data, err := json.MarshalIndent(lastAgentFile{AgentName: name}, "", "  ")
	if err != nil {
		return fmt.Errorf("config: failed to marshal last-used agent: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("config: failed to write last-used agent %q: %w", path, err)
	}
	return nil
}
