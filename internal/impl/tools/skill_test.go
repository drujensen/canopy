package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drujensen/canopy/internal/impl/skillsource"
)

// callSkillTool invokes the Skill tool's Call method with in marshaled to
// JSON, the same "drive the tool through its real functool.FuncTool
// interface" pattern this package's other tool tests use (see
// file_read_test.go), rather than reaching into the closure directly.
func callSkillTool(t *testing.T, skills map[string]skillsource.SkillDefinition, in SkillInput) (SkillOutput, error) {
	t.Helper()
	skillTool := NewSkillTool(skills)
	args, err := json.Marshal(in)
	require.NoError(t, err)
	raw, err := skillTool.Call(context.Background(), string(args))
	if err != nil {
		return SkillOutput{}, err
	}
	out, ok := raw.(SkillOutput)
	require.True(t, ok, "Call must return a SkillOutput, got %T", raw)
	return out, nil
}

func testSkill(t *testing.T, name, body string) skillsource.SkillDefinition {
	t.Helper()
	dir := t.TempDir()
	return skillsource.SkillDefinition{
		Name:        name,
		Description: "a test skill",
		Body:        body,
		Dir:         dir,
	}
}

// TestSkillTool_ReturnsBody is level 2's core claim: given a skill name with
// no "file" input, the tool returns that skill's full Body verbatim.
func TestSkillTool_ReturnsBody(t *testing.T) {
	skill := testSkill(t, "pdf-processing", "# PDF Processing\n\nThis is the full skill body with distinctive content.")
	skills := map[string]skillsource.SkillDefinition{"pdf-processing": skill}

	out, err := callSkillTool(t, skills, SkillInput{Name: "pdf-processing"})
	require.NoError(t, err)
	assert.Equal(t, skill.Body, out.Content)
}

// TestSkillTool_UnknownName_IsClearError proves an unmatched name produces a
// clear, specific error rather than silently returning empty content — the
// task's own requirement ("don't silently return empty").
func TestSkillTool_UnknownName_IsClearError(t *testing.T) {
	skills := map[string]skillsource.SkillDefinition{
		"real-skill": testSkill(t, "real-skill", "body"),
	}

	out, err := callSkillTool(t, skills, SkillInput{Name: "ghost-skill"})
	require.Error(t, err)
	assert.Empty(t, out.Content)
	assert.Contains(t, err.Error(), "ghost-skill")
	assert.Contains(t, err.Error(), "real-skill", "the error should name what skills actually are loaded")
}

// TestSkillTool_File_ReadsSupportingFileFromSkillDir is level 3's core
// claim: a supporting file inside the matched skill's own Dir is readable
// via the "file" input, confined to that skill's directory.
func TestSkillTool_File_ReadsSupportingFileFromSkillDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "reference.md"), []byte("supporting file content, distinctive marker XYZZY"), 0o644))

	skill := skillsource.SkillDefinition{Name: "with-ref", Description: "d", Body: "see reference.md", Dir: dir}
	skills := map[string]skillsource.SkillDefinition{"with-ref": skill}

	out, err := callSkillTool(t, skills, SkillInput{Name: "with-ref", File: "reference.md"})
	require.NoError(t, err)
	assert.Contains(t, out.Content, "XYZZY")
}

// TestSkillTool_File_NestedSupportingFile proves a supporting file nested in
// a subdirectory of the skill's Dir is also reachable.
func TestSkillTool_File_NestedSupportingFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "scripts"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "scripts", "helper.py"), []byte("print('hi')"), 0o644))

	skill := skillsource.SkillDefinition{Name: "with-script", Description: "d", Body: "see scripts/helper.py", Dir: dir}
	skills := map[string]skillsource.SkillDefinition{"with-script": skill}

	out, err := callSkillTool(t, skills, SkillInput{Name: "with-script", File: "scripts/helper.py"})
	require.NoError(t, err)
	assert.Equal(t, "print('hi')", out.Content)
}

// TestSkillTool_File_PathTraversalOutOfDirRejected mirrors
// pathsafety_test.go's TestResolveSafePath_DotDotEscapeRejected: a "file"
// input that tries to walk out of the skill's own Dir must be rejected, not
// silently resolved against some wider root.
func TestSkillTool_File_PathTraversalOutOfDirRejected(t *testing.T) {
	dir := t.TempDir()
	skill := skillsource.SkillDefinition{Name: "confined", Description: "d", Body: "body", Dir: dir}
	skills := map[string]skillsource.SkillDefinition{"confined": skill}

	_, err := callSkillTool(t, skills, SkillInput{Name: "confined", File: "../../../etc/passwd"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes configured root")
}

// TestSkillTool_File_AbsolutePathOutsideDirRejected mirrors
// pathsafety_test.go's TestResolveSafePath_AbsolutePathOutsideRootRejected —
// an absolute path outside the skill's Dir is rejected the same way a
// relative traversal is.
func TestSkillTool_File_AbsolutePathOutsideDirRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s3cr3t"), 0o644))

	skill := skillsource.SkillDefinition{Name: "confined", Description: "d", Body: "body", Dir: dir}
	skills := map[string]skillsource.SkillDefinition{"confined": skill}

	_, err := callSkillTool(t, skills, SkillInput{Name: "confined", File: filepath.Join(outside, "secret.txt")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes configured root")
}

// TestSkillTool_File_NonexistentFile_IsClearError proves a "file" input
// naming a path that resolves safely within Dir but doesn't exist produces
// a clear error rather than an empty result.
func TestSkillTool_File_NonexistentFile_IsClearError(t *testing.T) {
	dir := t.TempDir()
	skill := skillsource.SkillDefinition{Name: "confined", Description: "d", Body: "body", Dir: dir}
	skills := map[string]skillsource.SkillDefinition{"confined": skill}

	_, err := callSkillTool(t, skills, SkillInput{Name: "confined", File: "does-not-exist.md"})
	require.Error(t, err)
}

// TestSkillTool_File_TwoSkillsStayIsolated proves one skill's "file" reads
// are confined to *its own* Dir and cannot reach into a different loaded
// skill's directory — level 3's confinement is per-skill, not shared across
// every loaded skill.
func TestSkillTool_File_TwoSkillsStayIsolated(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dirB, "only-in-b.txt"), []byte("b's secret"), 0o644))

	skills := map[string]skillsource.SkillDefinition{
		"skill-a": {Name: "skill-a", Description: "d", Body: "a", Dir: dirA},
		"skill-b": {Name: "skill-b", Description: "d", Body: "b", Dir: dirB},
	}

	// Asking for skill-a's tool with a relative path that happens to name a
	// file that only exists under skill-b's Dir must fail — skill-a's own
	// Dir is the confinement root, not the union of every loaded skill's Dir.
	_, err := callSkillTool(t, skills, SkillInput{Name: "skill-a", File: "only-in-b.txt"})
	require.Error(t, err)

	out, err := callSkillTool(t, skills, SkillInput{Name: "skill-b", File: "only-in-b.txt"})
	require.NoError(t, err)
	assert.Equal(t, "b's secret", out.Content)
}
