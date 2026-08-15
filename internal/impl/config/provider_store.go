// Package config holds Canopy's own (non-Claude-Code-format) configuration:
// the flat JSON provider/model config file (Design §4, Requirements
// FR1/FR3).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/drujensen/canopy/internal/domain/entities"
)

const (
	// canopyDirName is the directory name used both under the user's home
	// directory (global config) and under a project root (project-local
	// override) — mirrors aiagent's --global/-g flag concept
	// (Requirements FR15).
	canopyDirName = ".canopy"

	providersFileName = "providers.json"
)

// ProvidersFile is the on-disk shape of the flat provider/model config file
// (Design §4): one JSON file holding every configured provider and model.
type ProvidersFile struct {
	Providers []entities.ProviderConfig `json:"providers"`
	Models    []entities.ModelConfig    `json:"models"`
}

// ProviderStore reads and writes the flat provider/model config file
// (Requirements FR1/FR3, Design §4) at a fixed path. Use
// NewGlobalProviderStore, NewProjectProviderStore, or
// NewProviderStoreForProject to resolve that path; NewProviderStore is for
// tests and other callers that already know the exact file to use.
type ProviderStore struct {
	path string
}

// NewProviderStore returns a store bound to an explicit file path.
func NewProviderStore(path string) *ProviderStore {
	return &ProviderStore{path: path}
}

// NewGlobalProviderStore returns a store bound to
// ~/.canopy/providers.json.
func NewGlobalProviderStore() (*ProviderStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("provider store: failed to resolve home directory: %w", err)
	}
	return NewProviderStore(filepath.Join(home, canopyDirName, providersFileName)), nil
}

// NewProjectProviderStore returns a store bound to
// <projectRoot>/.canopy/providers.json.
func NewProjectProviderStore(projectRoot string) *ProviderStore {
	return NewProviderStore(filepath.Join(projectRoot, canopyDirName, providersFileName))
}

// NewProviderStoreForProject resolves which store to use the way aiagent's
// --global/-g flag implies (Requirements FR15): when global is false, a
// project-local .canopy/providers.json under projectRoot is preferred if it
// already exists, falling back to the global ~/.canopy/providers.json
// otherwise. When global is true, the global store is always used
// regardless of whether a project-local file exists. Wiring an actual CLI
// flag onto this is left to a later phase — this only supports both paths
// at the store-construction level.
func NewProviderStoreForProject(projectRoot string, global bool) (*ProviderStore, error) {
	if !global {
		projectStore := NewProjectProviderStore(projectRoot)
		if _, err := os.Stat(projectStore.path); err == nil {
			return projectStore, nil
		}
	}
	return NewGlobalProviderStore()
}

// Path returns the file path this store reads from and writes to.
func (s *ProviderStore) Path() string {
	return s.path
}

// Load reads the provider/model config file. A missing file is not an
// error — it returns an empty ProvidersFile, since a fresh install has no
// config yet.
func (s *ProviderStore) Load() (*ProvidersFile, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ProvidersFile{}, nil
		}
		return nil, fmt.Errorf("provider store: failed to read %q: %w", s.path, err)
	}
	var file ProvidersFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("provider store: failed to unmarshal %q: %w", s.path, err)
	}
	return &file, nil
}

// Save writes the provider/model config file atomically (temp file +
// rename), creating parent directories as needed.
func (s *ProviderStore) Save(file *ProvidersFile) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("provider store: failed to create config directory %q: %w", dir, err)
	}

	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("provider store: failed to marshal config: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-providers-*")
	if err != nil {
		return fmt.Errorf("provider store: failed to create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("provider store: failed to write config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("provider store: failed to sync config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("provider store: failed to close temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("provider store: failed to rename temp file into place: %w", err)
	}
	return nil
}
