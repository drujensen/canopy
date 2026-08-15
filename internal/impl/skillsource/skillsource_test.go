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
