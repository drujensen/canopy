package skillsource

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

	writeFile(t, filepath.Join(projectRoot, ".claude", "skills", "pdf-fill", "SKILL.md"), `---
name: pdf-fill
description: Fill out PDF forms programmatically.
---

# PDF Fill

Use pdftk to fill forms. See reference.pdf for a sample form.
`)

	writeFile(t, filepath.Join(homeDir, ".claude", "skills", "personal-skill", "SKILL.md"), `---
name: personal-skill
description: A personal-only skill.
---
Personal skill body.
`)

	defs, err := Load(projectRoot, homeDir)
	require.NoError(t, err)
	require.Len(t, defs, 2)

	pdf, ok := defs["pdf-fill"]
	require.True(t, ok)
	assert.Equal(t, "pdf-fill", pdf.Name)
	assert.Equal(t, "Fill out PDF forms programmatically.", pdf.Description)
	assert.Contains(t, pdf.Body, "Use pdftk to fill forms.")
	assert.Equal(t, filepath.Join(projectRoot, ".claude", "skills", "pdf-fill"), pdf.Dir)

	personal, ok := defs["personal-skill"]
	require.True(t, ok)
	assert.Equal(t, "Personal skill body.", personal.Body)
}

func TestLoad_ProjectWinsNameConflict(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	writeFile(t, filepath.Join(projectRoot, ".claude", "skills", "shared", "SKILL.md"), `---
name: shared
description: Project version.
---
Project body.
`)
	writeFile(t, filepath.Join(homeDir, ".claude", "skills", "shared", "SKILL.md"), `---
name: shared
description: Personal version.
---
Personal body.
`)

	defs, err := Load(projectRoot, homeDir)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, "Project version.", defs["shared"].Description)
}

func TestLoad_MissingDirectories(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	defs, err := Load(projectRoot, homeDir)
	require.NoError(t, err)
	assert.Empty(t, defs)
}

func TestLoad_SkipsDirsWithoutSkillFile(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	// A subdirectory under skills/ with no SKILL.md should just be ignored.
	require.NoError(t, os.MkdirAll(filepath.Join(projectRoot, ".claude", "skills", "empty-dir"), 0o755))

	defs, err := Load(projectRoot, homeDir)
	require.NoError(t, err)
	assert.Empty(t, defs)
}

func TestLoad_MalformedFrontmatter_MissingFences(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	path := filepath.Join(projectRoot, ".claude", "skills", "broken", "SKILL.md")
	writeFile(t, path, "no frontmatter at all\n")

	_, err := Load(projectRoot, homeDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	assert.Contains(t, err.Error(), "frontmatter")
}

func TestLoad_MalformedFrontmatter_MissingRequiredField(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	path := filepath.Join(projectRoot, ".claude", "skills", "no-name", "SKILL.md")
	writeFile(t, path, `---
description: Missing a name.
---
Body.
`)

	_, err := Load(projectRoot, homeDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	assert.Contains(t, err.Error(), "name")
}

// TestLoad_CanopyDefaultSource covers the third, lowest-precedence source
// (post-v0.1.0 addendum, mirroring agentsource's ~/.canopy/agents tier).
func TestLoad_CanopyDefaultSource(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	writeFile(t, filepath.Join(homeDir, ".canopy", "skills", "canopy-default", "SKILL.md"), `---
name: canopy-default
description: A Canopy-generated default skill.
---
Default body.
`)

	defs, err := Load(projectRoot, homeDir)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, "canopy-default", defs["canopy-default"].Name)
}

// TestLoad_PersonalWinsOverCanopyDefault and
// TestLoad_ProjectWinsOverCanopyDefaultAndPersonal assert the precedence
// order Load's own doc comment describes: canopy-default < personal <
// project, mirroring agentsource_test.go's equivalent coverage.
func TestLoad_PersonalWinsOverCanopyDefault(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	writeFile(t, filepath.Join(homeDir, ".canopy", "skills", "shared-name", "SKILL.md"), `---
name: shared-name
description: Canopy default version.
---
Default body.
`)
	writeFile(t, filepath.Join(homeDir, ".claude", "skills", "shared-name", "SKILL.md"), `---
name: shared-name
description: Personal version.
---
Personal body.
`)

	defs, err := Load(projectRoot, homeDir)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, "Personal version.", defs["shared-name"].Description)
}

func TestLoad_ProjectWinsOverCanopyDefaultAndPersonal(t *testing.T) {
	projectRoot := t.TempDir()
	homeDir := t.TempDir()

	writeFile(t, filepath.Join(homeDir, ".canopy", "skills", "shared-name", "SKILL.md"), `---
name: shared-name
description: Canopy default version.
---
Default body.
`)
	writeFile(t, filepath.Join(homeDir, ".claude", "skills", "shared-name", "SKILL.md"), `---
name: shared-name
description: Personal version.
---
Personal body.
`)
	writeFile(t, filepath.Join(projectRoot, ".claude", "skills", "shared-name", "SKILL.md"), `---
name: shared-name
description: Project version.
---
Project body.
`)

	defs, err := Load(projectRoot, homeDir)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, "Project version.", defs["shared-name"].Description)
}

// TestWriteDefaults_WritesLoadableSkill asserts the generated
// mcp-server-setup skill is well-formed enough for Load to parse it
// successfully, with a non-empty description (level-1 catalog entry) and
// body (level-2/3 content).
func TestWriteDefaults_WritesLoadableSkill(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".canopy", "skills")

	require.NoError(t, WriteDefaults(dir))

	path := filepath.Join(dir, "mcp-server-setup", "SKILL.md")
	_, err := os.Stat(path)
	require.NoError(t, err)

	defs, err := Load(t.TempDir(), filepath.Dir(filepath.Dir(dir)))
	require.NoError(t, err)
	require.Len(t, defs, 1)

	def, ok := defs["mcp-server-setup"]
	require.True(t, ok)
	assert.Equal(t, "mcp-server-setup", def.Name)
	assert.NotEmpty(t, def.Description)
	assert.NotEmpty(t, def.Body)
	assert.Contains(t, def.Body, ".mcp.json")
}

// TestWriteDefaults_Idempotent mirrors agentsource's own idempotency
// contract: an already-existing default skill file is left untouched.
func TestWriteDefaults_Idempotent(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, WriteDefaults(dir))

	path := filepath.Join(dir, "mcp-server-setup", "SKILL.md")
	custom := "---\nname: mcp-server-setup\ndescription: Hand-edited by the user.\n---\nCustom body.\n"
	require.NoError(t, os.WriteFile(path, []byte(custom), 0o644))

	require.NoError(t, WriteDefaults(dir))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, custom, string(got), "an already-existing default skill file must be left untouched")
}
