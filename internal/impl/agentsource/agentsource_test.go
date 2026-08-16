package agentsource

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestLoad_HappyPath(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	writeFile(t, filepath.Join(projectRoot, ".claude", "agents", "reviewer.md"), `---
name: reviewer
description: Reviews code changes for correctness.
tools: Read, Grep, Bash
model: claude-opus
---

You are a careful code reviewer. Focus on correctness bugs.
`)

	// Nested subdirectory should be found recursively.
	writeFile(t, filepath.Join(projectRoot, ".claude", "agents", "nested", "helper.md"), `---
name: helper
description: A small helper agent.
---
Body for helper.
`)

	writeFile(t, filepath.Join(homeDir, ".claude", "agents", "personal-only.md"), `---
name: personal-only
description: Only defined personally.
---
Personal body.
`)

	defs, err := Load(projectRoot, homeDir)
	require.NoError(t, err)
	require.Len(t, defs, 3)

	reviewer, ok := defs["reviewer"]
	require.True(t, ok)
	assert.Equal(t, "reviewer", reviewer.Name)
	assert.Equal(t, "Reviews code changes for correctness.", reviewer.Description)
	assert.Equal(t, []string{"Read", "Grep", "Bash"}, reviewer.Tools)
	assert.Equal(t, "claude-opus", reviewer.Model)
	assert.Equal(t, "You are a careful code reviewer. Focus on correctness bugs.", reviewer.Instructions)
	assert.Equal(t, filepath.Join(projectRoot, ".claude", "agents", "reviewer.md"), reviewer.SourcePath)

	helper, ok := defs["helper"]
	require.True(t, ok)
	assert.Equal(t, "Body for helper.", helper.Instructions)
	assert.Nil(t, helper.Tools) // omitted tools => inherit everything => nil/empty

	personal, ok := defs["personal-only"]
	require.True(t, ok)
	assert.Equal(t, "Personal body.", personal.Instructions)
}

func TestLoad_ProjectWinsNameConflict(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	writeFile(t, filepath.Join(projectRoot, ".claude", "agents", "shared.md"), `---
name: shared
description: Project version.
---
Project body.
`)
	writeFile(t, filepath.Join(homeDir, ".claude", "agents", "shared.md"), `---
name: shared
description: Personal version.
---
Personal body.
`)

	defs, err := Load(projectRoot, homeDir)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, "Project version.", defs["shared"].Description)
	assert.Equal(t, "Project body.", defs["shared"].Instructions)
}

func TestLoad_MissingDirectories(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	defs, err := Load(projectRoot, homeDir)
	require.NoError(t, err)
	assert.Empty(t, defs)
}

func TestLoad_MalformedFrontmatter_MissingFences(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	path := filepath.Join(projectRoot, ".claude", "agents", "broken.md")
	writeFile(t, path, "no frontmatter here, just markdown\n")

	_, err := Load(projectRoot, homeDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	assert.Contains(t, err.Error(), "frontmatter")
}

func TestLoad_MalformedFrontmatter_MissingRequiredField(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	path := filepath.Join(projectRoot, ".claude", "agents", "no-description.md")
	writeFile(t, path, `---
name: incomplete
---
Body.
`)

	_, err := Load(projectRoot, homeDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	assert.Contains(t, err.Error(), "description")
}

func TestLoad_CanopyDefaultSource(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	writeFile(t, filepath.Join(homeDir, ".canopy", "agents", "general.md"), `---
name: general
description: Canopy-generated default.
---
Default body.
`)

	defs, err := Load(projectRoot, homeDir)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, "Canopy-generated default.", defs["general"].Description)
	assert.Equal(t, "Default body.", defs["general"].Instructions)
}

func TestLoad_PersonalWinsOverCanopyDefault(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	writeFile(t, filepath.Join(homeDir, ".canopy", "agents", "general.md"), `---
name: general
description: Canopy-generated default.
---
Default body.
`)
	writeFile(t, filepath.Join(homeDir, ".claude", "agents", "general.md"), `---
name: general
description: User's real personal agent.
---
Personal body.
`)

	defs, err := Load(projectRoot, homeDir)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, "User's real personal agent.", defs["general"].Description)
	assert.Equal(t, "Personal body.", defs["general"].Instructions)
}

func TestLoad_ProjectWinsOverCanopyDefaultAndPersonal(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	writeFile(t, filepath.Join(homeDir, ".canopy", "agents", "general.md"), `---
name: general
description: Canopy-generated default.
---
Default body.
`)
	writeFile(t, filepath.Join(homeDir, ".claude", "agents", "general.md"), `---
name: general
description: Personal version.
---
Personal body.
`)
	writeFile(t, filepath.Join(projectRoot, ".claude", "agents", "general.md"), `---
name: general
description: Project version.
---
Project body.
`)

	defs, err := Load(projectRoot, homeDir)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, "Project version.", defs["general"].Description)
	assert.Equal(t, "Project body.", defs["general"].Instructions)
}

func TestLoad_CanopyDefaultCoexistsWithOtherNames(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	writeFile(t, filepath.Join(homeDir, ".canopy", "agents", "general.md"), `---
name: general
description: Canopy-generated default.
---
Default body.
`)
	writeFile(t, filepath.Join(projectRoot, ".claude", "agents", "reviewer.md"), `---
name: reviewer
description: Reviews code changes.
---
Reviewer body.
`)

	defs, err := Load(projectRoot, homeDir)
	require.NoError(t, err)
	require.Len(t, defs, 2)
	assert.Contains(t, defs, "general")
	assert.Contains(t, defs, "reviewer")
}

func TestWriteDefault_WritesLoadableAgent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".canopy", "agents")

	require.NoError(t, WriteDefault(dir))

	path := filepath.Join(dir, "general.md")
	_, err := os.Stat(path)
	require.NoError(t, err)

	defs, err := Load(t.TempDir(), filepath.Dir(filepath.Dir(dir)))
	require.NoError(t, err)
	require.Len(t, defs, 1)

	def, ok := defs["general"]
	require.True(t, ok)
	assert.Equal(t, "general", def.Name)
	assert.NotEmpty(t, def.Description)
	assert.Nil(t, def.Tools)
	assert.Empty(t, def.Model)
	assert.NotEmpty(t, def.Instructions)
}

func TestWriteDefault_Idempotent(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, WriteDefault(dir))

	path := filepath.Join(dir, "general.md")
	custom := "---\nname: general\ndescription: Hand-edited by the user.\n---\nCustom body.\n"
	require.NoError(t, os.WriteFile(path, []byte(custom), 0o644))

	require.NoError(t, WriteDefault(dir))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, custom, string(got))
}

func TestLoad_MalformedFrontmatter_InvalidYAML(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	path := filepath.Join(projectRoot, ".claude", "agents", "bad-yaml.md")
	writeFile(t, path, "---\nname: [unterminated\ndescription: broken\n---\nBody.\n")

	_, err := Load(projectRoot, homeDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
}
