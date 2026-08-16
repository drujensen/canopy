package tools

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"

	"github.com/drujensen/canopy/internal/impl/skillsource"
)

// SkillInput is the model-facing input for the Skill tool.
type SkillInput struct {
	Name string `json:"name" jsonschema:"Name of the skill to look up, from the '## Available Skills' list in the system prompt."`

	// File, when set, names a supporting file inside the matched skill's own
	// directory to read instead of the skill's SKILL.md body — see
	// NewSkillTool's doc comment for why this is a field on this tool rather
	// than a job for the file-read tool.
	File string `json:"file,omitempty" jsonschema:"Optional. A supporting file referenced from the skill's body (e.g. 'reference.md'), read from that skill's own directory instead of returning the body. Leave empty to get the skill's full body."`
}

// SkillOutput is the model-facing output for the Skill tool.
type SkillOutput struct {
	Content string `json:"content"`
}

// NewSkillTool builds Canopy's Skill tool (docs/DESIGN.md §3.2/§3.11's
// progressive-disclosure levels 2 and 3, docs/REQUIREMENTS.md FR19): given a
// skill name, returns that skill's full SKILL.md body (level 2). The system
// prompt every agent gets (AgentService.buildInstructions/skillsCatalog,
// domain/services) only ever lists each loaded skill's name+description
// (level 1) — cheap, always-visible — so a full body only enters context
// when a turn actually calls this tool for it.
//
// When Input.File is also set, this returns the content of that file
// instead of the body (level 3): a skill's SKILL.md commonly says something
// like "see reference.md for details," pointing at another file inside its
// own directory (skillsource.SkillDefinition.Dir), and the model is
// expected to fetch that via this same tool rather than the file-read tool.
//
// # Why level 3 doesn't just reuse the file-read tool, and isn't a security oversight
//
// Canopy's file-read tool (NewFileReadTool) confines every read to
// ToolsConfig.WorkingRoot — the project directory — via resolveSafePath,
// and that confinement is load-bearing: it exists to stop a model-chosen,
// otherwise-untrusted path from escaping the project it's meant to work in
// (see pathsafety.go's own doc comment). A *personal* skill loaded from
// ~/.claude/skills/<name>/ (skillsource.Load scans both project and
// personal directories) sits entirely outside WorkingRoot, so routing a
// skill's supporting-file reads through file-read would either reject them
// outright or require weakening file-read's confinement for every other
// caller too — and file-read's confinement boundary exists precisely
// because the paths it's handed are otherwise-untrusted model input, a
// property that doesn't change just because this particular path happens to
// originate from a skill.
//
// The skill's own directory is a different trust category: it's local
// configuration the user explicitly installed (a SKILL.md file they wrote
// or copied onto disk), not something the model supplied. So instead of
// loosening file-read, this tool confines File reads to the *matched
// skill's own* Dir specifically — reusing the exact same resolveSafePath
// helper file-read uses, just called with a narrower, per-skill root
// instead of the project-wide WorkingRoot. A path can only ever resolve
// into the one skill the model named, project or personal, never anywhere
// else — the confinement automatically travels with whichever skill.Dir was
// selected, and a "../../escape" attempt is rejected exactly the way
// file-read would reject one against its own root (see this package's
// pathsafety_test.go for the shared traversal-rejection behavior; the same
// tests are mirrored for this tool in skill_test.go).
//
// Not approval-gated: reading a skill's body or a supporting file is
// read-only, the same tier as FileRead.
func NewSkillTool(skills map[string]skillsource.SkillDefinition) tool.FuncTool {
	return functool.MustNew(functool.Config{
		Name:        "Skill",
		Description: "Look up a skill's full instructions by name (see the '## Available Skills' list in the system prompt). Pass 'file' to read a supporting file the skill's body references, confined to that skill's own directory.",
	}, func(ctx context.Context, in SkillInput) (SkillOutput, error) {
		skill, ok := skills[in.Name]
		if !ok {
			names := make([]string, 0, len(skills))
			for name := range skills {
				names = append(names, name)
			}
			sort.Strings(names)
			return SkillOutput{}, fmt.Errorf("skill tool: unknown skill %q (loaded skills: %v)", in.Name, names)
		}

		if in.File == "" {
			return SkillOutput{Content: skill.Body}, nil
		}

		path, err := resolveSafePath(skill.Dir, in.File)
		if err != nil {
			return SkillOutput{}, fmt.Errorf("skill tool: failed to read supporting file %q for skill %q: %w", in.File, in.Name, err)
		}

		info, err := os.Stat(path)
		if err != nil {
			return SkillOutput{}, fmt.Errorf("skill tool: failed to read supporting file %q for skill %q: %w", in.File, in.Name, err)
		}
		if info.IsDir() {
			return SkillOutput{}, fmt.Errorf("skill tool: %q is a directory, not a file", in.File)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return SkillOutput{}, fmt.Errorf("skill tool: failed to read supporting file %q for skill %q: %w", in.File, in.Name, err)
		}
		return SkillOutput{Content: string(data)}, nil
	})
}
