package config

// last_model.json (post-v0.1.0 addendum) records which model was most
// recently made active in this project (or globally, mirroring
// providers.json's own --global/-g convention) — read at startup so a
// brand-new chat's default model resolution picks up where the user left
// off instead of always falling back to providers.json's first-listed
// model. Mirrors last_agent_store.go exactly, including its "small JSON
// object, not a bare string file" and "sibling of providers.json, no new
// path-resolution scheme" reasoning — see that file's own doc comment.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// lastModelFile is the on-disk shape: deliberately just the one field, kept
// as a small JSON object rather than a bare string file so it can grow
// (e.g. a timestamp) later without an incompatible format change.
type lastModelFile struct {
	ModelName string `json:"model_name"`
}

// LoadLastModel returns the model name last recorded via SaveLastModel at
// path, or "" if none has ever been recorded. A missing file is not an
// error — it returns ("", nil), the same "not there yet" convention
// LoadLastAgent already uses — since every project's first-ever run has no
// last-used model to report.
func LoadLastModel(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("config: failed to read last-used model %q: %w", path, err)
	}
	var f lastModelFile
	if err := json.Unmarshal(data, &f); err != nil {
		return "", fmt.Errorf("config: failed to decode last-used model %q: %w", path, err)
	}
	return f.ModelName, nil
}

// SaveLastModel records name as the most recently used model at path,
// creating path's parent directory if needed (the same posture
// SaveLastAgent takes).
func SaveLastModel(path, name string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("config: failed to create directory %q for last-used model: %w", dir, err)
	}
	data, err := json.MarshalIndent(lastModelFile{ModelName: name}, "", "  ")
	if err != nil {
		return fmt.Errorf("config: failed to marshal last-used model: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("config: failed to write last-used model %q: %w", path, err)
	}
	return nil
}
