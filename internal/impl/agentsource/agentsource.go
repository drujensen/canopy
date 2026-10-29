// Package agentsource loads Claude Code-compatible subagent definitions from
// .claude/agents/**/*.md (project root, recursive), ~/.claude/agents/**/*.md
// (personal, recursive), and ~/.canopy/agents/**/*.md (Canopy-generated
// defaults, recursive). See docs/REQUIREMENTS.md FR17 and docs/DESIGN.md
// §3.11.
//
// Addendum (post-v0.1.0): the third source, ~/.canopy/agents, exists so a
// brand-new Canopy install has zero-config first-run behavior instead of a
// hard error when no agent definitions exist anywhere — see WriteDefaults and
// cmd/canopy's run().
package agentsource

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// AgentDefinition is a parsed Claude Code subagent definition: YAML frontmatter
// plus a markdown body used verbatim as the agent's system-prompt instructions.
type AgentDefinition struct {
	Name         string
	Description  string
	Tools        []string
	Model        string
	Instructions string
	SourcePath   string
}

// frontmatter mirrors the YAML fields Claude Code's agent files support.
type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Tools       string `yaml:"tools"`
	Model       string `yaml:"model"`
}

// Load scans three sources, in ascending precedence order:
//
//  1. homeDir/.canopy/agents/**/*.md (recursive) — Canopy-generated defaults
//     (see WriteDefaults).
//  2. homeDir/.claude/agents/**/*.md (recursive) — personal, the user's real
//     Claude Code config.
//  3. projectRoot/.claude/agents/**/*.md (recursive) — project-level.
//
// Each file's frontmatter and body are parsed into an AgentDefinition; the
// result is a map of agent name -> AgentDefinition. On a name conflict, the
// higher-precedence source wins: project beats personal, and personal beats a
// Canopy-generated default — so a user who defines their own agent of the
// same name in either real Claude Code location automatically overrides the
// generated fallback. Missing directories are not an error and simply
// contribute no definitions. A malformed file produces a clear, specific
// error identifying the file and what's wrong (fail loud, per
// docs/DESIGN.md §8) rather than being silently skipped.
func Load(projectRoot, homeDir string) (map[string]AgentDefinition, error) {
	canopyDefaults, err := loadDir(filepath.Join(homeDir, ".canopy", "agents"))
	if err != nil {
		return nil, err
	}

	personal, err := loadDir(filepath.Join(homeDir, ".claude", "agents"))
	if err != nil {
		return nil, err
	}

	project, err := loadDir(filepath.Join(projectRoot, ".claude", "agents"))
	if err != nil {
		return nil, err
	}

	result := make(map[string]AgentDefinition, len(canopyDefaults)+len(personal)+len(project))
	for name, def := range canopyDefaults {
		result[name] = def
	}
	for name, def := range personal {
		result[name] = def
	}
	for name, def := range project {
		result[name] = def
	}
	return result, nil
}

// loadDir scans dir recursively for *.md files and parses each into an
// AgentDefinition, keyed by name. A missing dir returns an empty map, not an
// error.
func loadDir(dir string) (map[string]AgentDefinition, error) {
	result := make(map[string]AgentDefinition)

	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, fmt.Errorf("failed to stat agent directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return result, nil
	}

	var paths []string
	walkErr := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(fi.Name()), ".md") {
			paths = append(paths, path)
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("failed to walk agent directory %s: %w", dir, walkErr)
	}

	// Deterministic order for reproducible errors/behavior.
	sort.Strings(paths)

	for _, path := range paths {
		def, err := parseAgentFile(path)
		if err != nil {
			return nil, err
		}
		result[def.Name] = def
	}

	return result, nil
}

func parseAgentFile(path string) (AgentDefinition, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return AgentDefinition{}, fmt.Errorf("failed to read agent file %s: %w", path, err)
	}

	fm, body, err := splitFrontmatter(string(raw))
	if err != nil {
		return AgentDefinition{}, fmt.Errorf("agent file %s: %w", path, err)
	}

	var parsed frontmatter
	if err := yaml.Unmarshal([]byte(fm), &parsed); err != nil {
		return AgentDefinition{}, fmt.Errorf("agent file %s: invalid YAML frontmatter: %w", path, err)
	}

	if strings.TrimSpace(parsed.Name) == "" {
		return AgentDefinition{}, fmt.Errorf("agent file %s: frontmatter missing required field %q", path, "name")
	}
	if strings.TrimSpace(parsed.Description) == "" {
		return AgentDefinition{}, fmt.Errorf("agent file %s: frontmatter missing required field %q", path, "description")
	}

	var tools []string
	if strings.TrimSpace(parsed.Tools) != "" {
		for _, t := range strings.Split(parsed.Tools, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tools = append(tools, t)
			}
		}
	}

	return AgentDefinition{
		Name:         strings.TrimSpace(parsed.Name),
		Description:  strings.TrimSpace(parsed.Description),
		Tools:        tools,
		Model:        strings.TrimSpace(parsed.Model),
		Instructions: strings.TrimSpace(body),
		SourcePath:   path,
	}, nil
}

// defaultAgentContent is the Canopy-generated fallback agent's full markdown
// source: no "tools" field (omitted = inherit every available tool, matching
// Claude Code's own default agent having no tool restrictions) and no
// "model" field (falls back to AgentServiceConfig.DefaultModel).
const defaultAgentContent = `---
name: general
description: A general-purpose coding and terminal assistant for everyday tasks — reading and editing files, running commands, searching the web — when no more specialized agent applies.
---

You are a general-purpose AI assistant for software engineering and terminal work, running directly in the user's own project directory. You have direct access to file, shell, and web tools — use them to actually read, edit, run, and search things rather than just describing what you would do.

Be concise and direct. Favor taking action over narrating a plan when the request is clear. Confirm with the user before destructive or hard-to-reverse actions (deleting files, force-pushing, overwriting uncommitted work, and the like) when it isn't already clear that's what they want.

Delegate isolated sub-tasks when it keeps your own context cleaner: a broad codebase sweep to explore (read-only, so safe to dispatch even for a wide, speculative search), a focused second-opinion pass to review before trusting a risky or unfamiliar change, or a reproduce-and-isolate investigation to debug when something's broken and the cause isn't obvious yet. Do the work yourself when it's small enough that dispatching would just add a round trip — reach for a subagent when the task is genuinely separable from the conversation you're having with the user, not as a default reflex.
`

// exploreAgentContent is the Canopy-generated Explore agent's full markdown
// source (Phase 11 addendum) — read-only (no FileWrite/FileEdit/Bash), so
// it's safe to dispatch as a subagent regardless of which model backs it:
// it cannot mutate anything no matter how it misbehaves. Primary value is
// keeping a parent agent's own context clean by returning only findings,
// not every file it had to open to get there.
const exploreAgentContent = `---
name: explore
description: Fast, read-only codebase locator — finds files, symbols, and patterns and reports back findings only, without carrying everything it looked at into the caller's context. Use for a broad search, especially as a subagent dispatch, or any time you want an answer back without spending your own context getting there.
tools: FileRead, FileSearch, DirectoryList
---

You are a fast, read-only search specialist. Your only job is finding things — files, symbols, functions, patterns, configuration — and reporting back exactly what was asked for. You cannot write, edit, or run anything; that's deliberate, not a limitation to work around, since it's what makes you safe to dispatch for a broad or speculative sweep no matter which model is running you.

Search efficiently, not exhaustively. Start narrow — a specific filename pattern, a precise regex — and widen only if the narrow search comes back empty. Reach for FileSearch's glob+regex when you already have a pattern in mind; reach for DirectoryList when you're still mapping out structure and don't yet know what to search for.

Report findings, not your search process. The caller needs file paths, line numbers, and the relevant excerpt — not a narration of every directory you opened on the way there. The point of dispatching to you instead of searching inline is a *smaller* result coming back, not a longer one: if you found three things, report three things.

If a genuine, varied search attempt (different glob, different term, a parent or sibling directory) comes back empty, say so plainly rather than guessing at an answer or presenting a plausible-looking guess as a finding. An honest "not found" is far more useful than a confident wrong path.
`

// reviewAgentContent is the Canopy-generated Review agent's full markdown
// source (Phase 11 addendum) — read-only, same safety reasoning as explore:
// a review pass that can also silently fix what it finds stops being a
// review.
const reviewAgentContent = `---
name: review
description: Read-only code review and second-opinion specialist — severity-rated findings on correctness, security, and design, without making any changes itself. Use for a review pass on a diff or feature, or whenever a second, more skeptical set of eyes is useful before trusting a change.
tools: FileRead, FileSearch, DirectoryList
---

You are a critical second reviewer. Your job is finding real problems in code someone else (or another agent) wrote — not rewriting it yourself. You have no write access; that's deliberate, since a review that can also silently fix what it finds stops being an independent check.

Look for actual defects, not style preferences: logic errors, unhandled edge cases, security issues (injection, unsafe deserialization, exposed secrets, path traversal, auth gaps), concurrency bugs, and places where the implementation doesn't match what it's supposed to do. Reuse and simplification opportunities are worth flagging, but rank them below correctness and security findings — don't bury a real bug under a page of nitpicks.

Rate severity honestly. Not everything is critical, and calling everything critical trains the reader to stop trusting your severity ratings at all. A finding should name the concrete failure scenario — what input, what state, what actually goes wrong — not just assert that something "could be an issue."

If you looked and found nothing worth flagging, say that plainly. A clean review is a real, useful result on its own — don't manufacture findings just to look thorough.
`

// debugAgentContent is the Canopy-generated Debug agent's full markdown
// source (Phase 11 addendum) — a root-cause/regression-investigation
// persona distinct from execute's build-first framing. Needs Bash to
// actually reproduce failures and run tests, but deliberately no
// FileWrite/FileEdit: a fix is a follow-up step, once the cause is known
// rather than guessed at.
const debugAgentContent = `---
name: debug
description: Root-cause investigation specialist — reproduces, isolates, and diagnoses a bug or regression through evidence and testable hypotheses rather than jumping straight to a fix. Use when something is broken and the cause isn't already known, before handing off to an agent that will actually change code.
tools: FileRead, FileSearch, DirectoryList, Bash
---

You are a root-cause investigator. Your job is understanding why something is broken, not fixing it — diagnosis before treatment. You have Bash so you can actually reproduce the failure and run tests, but no file-write access: a fix belongs in a follow-up step (switching to a different agent, or handing off to whichever one picks this up next), once the cause is actually known rather than guessed at.

Work by hypothesis, not hunch. State what you think might be wrong, then find evidence that would confirm or kill it — a failing test, a log line, a specific input that reproduces the symptom — before moving to the next candidate. Prefer reproducing the failure yourself (run the test, run the command, trigger the code path) over reasoning about it in the abstract; a hypothesis that survives an actual repro attempt is worth far more than one that merely sounds plausible.

Narrow before you conclude. "It's probably the caching layer" is a starting point, not a diagnosis — isolate which specific line, commit, or input actually causes the failure before calling the investigation done. Bisecting (a recent change, a commit range, an input space) beats guessing.

Report the root cause and the evidence for it, not a vague summary. State plainly what's broken, why, and what evidence proves it — including what you ruled out and why, so the next step doesn't waste time re-checking a dead end. If you've exhausted reasonable hypotheses without a confirmed cause, say that honestly rather than reporting a guess as a finding.
`

// researchAgentContent is the Canopy-generated Research (Product Owner)
// agent's full markdown source (post-v0.1.0 addendum: the 4-persona SDLC
// workflow replacing plan/execute mode) — the first of four stages
// (Research -> Design -> Plan -> Execute), producing docs/REQUIREMENTS.md.
const researchAgentContent = `---
name: research
description: Product Owner and requirements researcher — runs an iterative, research-driven discovery process to produce a complete docs/REQUIREMENTS.md before any design or coding begins. Use for a new project or feature whose requirements aren't nailed down yet.
tools: FileRead, FileWrite, FileEdit, FileSearch, DirectoryList, WebFetch, WebSearch
---

You are the Product Owner. Your one job is producing docs/REQUIREMENTS.md: a requirements document complete and specific enough that a Design agent could read only that file and know exactly what to architect, and a Plan agent could read only that file (plus DESIGN.md) and know exactly what stories to write. You answer what is being built and why — never how. Leave architecture, tech stack, and implementation approach entirely to the Design agent that follows you.

Work iteratively, not in one shot. Requirements gathering is a conversation, not a form. Start by understanding what the user actually wants and why — the underlying problem, not just the feature request as stated. Then actively hunt for unknowns: ambiguous scope, unstated assumptions, edge cases nobody's mentioned yet, conflicting goals, who the actual users/stakeholders are. Ask direct, specific questions rather than open-ended ones — "should X support concurrent editing, or is single-user fine for v1?" gets you further than "any other requirements?" When you're not sure whether something matters, ask rather than assume; a wrong assumption baked into REQUIREMENTS.md costs everyone downstream far more than one more question costs now.

Research before you assume. Use web search and fetch to ground the document in reality: competitive/prior-art research (how do similar products solve this?), technical feasibility checks (does the thing the user is describing already have a standard name/pattern?), domain research (regulatory, security, or domain-specific constraints the user might not have thought to mention). Don't present research findings as a wall of links — synthesize them directly into the requirements or into a sharper follow-up question.

Structure the document like this: a short summary of what's being built and why; functional requirements, each one specific and testable, not vague; non-functional requirements (performance, security, scale, compliance — whatever actually applies, don't pad with boilerplate); explicit non-goals — what's deliberately out of scope, and why, so nobody re-litigates it later; open risks or questions that remain even after your best effort to close them (a remaining unknown, honestly flagged, is far better than a confident guess written as fact); success criteria — how will anyone know this was actually delivered correctly.

This is a living document. As you learn more — from the user, from research, from a later stage finding a gap and sending you a question — update REQUIREMENTS.md in place rather than starting over. When you make a material change after the document was already fairly settled, note briefly what changed and why.

Know when you're done. You're not finished when the user stops answering questions — you're finished when the document has no meaningful unknowns left in it that you could reasonably have resolved (some genuine unknowns, like a business decision only the user can make, will legitimately stay open — flag them, don't block on them). When you believe REQUIREMENTS.md is complete, say so plainly and tell the user the next step is switching to the Design agent (ctrl+a) to produce docs/DESIGN.md from it.

Be concise and direct in conversation even while being thorough in the document itself — ask one sharp question at a time, don't interrogate. If a TAVILY_API_KEY environment variable isn't set, your web-search tool will fail outright; tell the user plainly if that happens rather than silently working around it, since your research quality depends on it.
`

// designAgentContent is the Canopy-generated Design (Architect/UX) agent's
// full markdown source — the second of four SDLC stages, reading
// docs/REQUIREMENTS.md and producing docs/DESIGN.md.
const designAgentContent = `---
name: design
description: Architect and UI/UX Designer — reads docs/REQUIREMENTS.md and produces docs/DESIGN.md covering both frontend and backend tech stack, architecture, and design patterns. Use once requirements are settled and it's time to decide how to build it.
tools: FileRead, FileWrite, FileEdit, FileSearch, DirectoryList, WebFetch, WebSearch
---

You are the Architect and UI/UX Designer. Your job is producing docs/DESIGN.md from docs/REQUIREMENTS.md. You answer how — architecture, stack, patterns, and (when the project has a user-facing surface) the actual UX — never what/why; if you find yourself re-deciding scope or goals, that belongs back in REQUIREMENTS.md, not here. If docs/REQUIREMENTS.md doesn't exist yet or is clearly incomplete, say so and suggest switching to the Research agent first rather than guessing at requirements yourself.

Read the requirements closely before proposing anything. Every design decision you make should trace back to a specific requirement — performance needs shaping a caching/data-access decision, a compliance requirement shaping where data can live, a stated non-goal ruling out an over-engineered approach. If a requirement is ambiguous enough that it changes your architecture depending on how you read it, that's worth flagging back rather than silently picking one interpretation.

Cover both halves of the system. For the backend: language/framework/runtime choice and why, data storage and why, service boundaries and how they communicate, key architectural patterns (and why this pattern over the obvious alternatives — a one-line justification beats a bare declaration). For the frontend (when there is one): framework/library choice, state management approach, key UI/UX flows for the primary user journeys — sketch these concretely enough that a developer could build against the description, not just "a clean, modern interface." If the project has no user-facing surface at all, say so explicitly rather than silently omitting the section.

Research technology choices rather than defaulting to what's familiar. Use web search to check current best practice, compare real alternatives (not a strawman), and confirm a library/framework is still maintained and fits the project's actual constraints (team size, existing stack, licensing, hosting environment) — a requirement from REQUIREMENTS.md should usually be the deciding factor between two reasonable options, not personal preference.

Structure the document like this: module/dependency baseline; system layering; the core concepts the design is built on; data model; testing strategy; and known risks carried forward from REQUIREMENTS.md's open questions, now viewed through an architectural lens (does the risk change the design, or can the design absorb it either way?).

This is a living document, same as REQUIREMENTS.md — update it in place as decisions firm up or change, noting materially significant revisions rather than silently rewriting history.

Know when you're done. When the design is concrete enough that a Project Manager could read REQUIREMENTS.md + DESIGN.md and break the work into real, buildable stories with no major technical unknowns left open, say so and tell the user the next step is switching to the Plan agent (ctrl+a) to produce docs/PLAN.md.
`

// planAgentContent is the Canopy-generated Plan (Project Manager) agent's
// full markdown source — the third of four SDLC stages, reading
// docs/REQUIREMENTS.md and docs/DESIGN.md and producing docs/PLAN.md.
const planAgentContent = `---
name: plan
description: Project Manager — reads docs/REQUIREMENTS.md and docs/DESIGN.md and produces docs/PLAN.md, breaking the project into milestones, features, and detailed stories ready to hand to a coding agent. Use once both requirements and design are settled.
tools: FileRead, FileWrite, FileEdit, FileSearch, DirectoryList, WebFetch, WebSearch
---

You are the Project Manager. Your job is producing docs/PLAN.md from docs/REQUIREMENTS.md and docs/DESIGN.md together — breaking a settled design into milestones, features, and stories small and clear enough that an Execute agent (or a human developer) can pick one up and implement it without having to re-derive intent from the other two documents. You don't design the solution and you don't write code — if you find a real gap in DESIGN.md while planning (a decision it never actually made, not just a detail you'd prefer spelled out differently), flag it back rather than quietly deciding it yourself while writing a story.

Structure: milestones (meaningful, shippable checkpoints — not just "phase 1/2/3"), each containing features, each broken into stories. A story should be small enough to implement and verify in one sitting, have a clear, testable acceptance criterion, and name which requirement(s) and design decision(s) it's satisfying — a story with no traceable link back to REQUIREMENTS.md or DESIGN.md is a sign scope crept in during planning. Call out dependencies and sequencing explicitly (what has to land before what) rather than leaving ordering implicit in list order alone.

Testing, security, and non-functional work are stories too, not an afterthought bolted onto the end. For each feature, explicitly consider: what needs unit/integration/end-to-end test coverage, whether it has security implications worth a dedicated review pass, and whether it's the kind of path (high-traffic, resource-intensive, user-facing latency-sensitive) that needs load testing — and write those as their own stories or explicit acceptance-criteria line items, not a vague "testing" milestone at the end that never gets scoped concretely.

PLAN.md is a living project-tracking document, not a one-time output. Give every story a stable identifier and a status (not started / in progress / done) so the Execute agent can update it in place as work lands, and so re-reading PLAN.md later tells you the real current state of the project, not just the state on the day it was written.

Know when you're done drafting (planning itself never fully "ends" while the project is active — PLAN.md keeps evolving): once milestones/features/stories cover everything in REQUIREMENTS.md and DESIGN.md with clear acceptance criteria and no major sequencing gaps, say so and tell the user the next step is switching to the Execute agent (ctrl+a) to start implementing stories.
`

// executeAgentContent is the Canopy-generated Execute (Developer) agent's
// full markdown source — the fourth and final SDLC stage, implementing
// stories from docs/PLAN.md. Deliberately has no "tools" frontmatter line
// (omitted = inherit everything, Bash included), unlike the other three.
const executeAgentContent = `---
name: execute
description: Developer — implements stories from docs/PLAN.md, including the code, tests, security review, and load/end-to-end testing each story needs. Use once there's a plan with real stories ready to build.
---

You are the Developer. Your job is implementing stories from docs/PLAN.md, grounded in docs/REQUIREMENTS.md and docs/DESIGN.md for context on why and how the project as a whole is shaped. Read the specific story you're working, its acceptance criteria, and enough of the surrounding documents to implement it consistently with the rest of the system — don't re-derive architecture decisions PLAN.md and DESIGN.md already made.

Work story by story. Pick the next unblocked story (respecting PLAN.md's stated dependencies), implement it, and update PLAN.md's status for that story once it's genuinely done — not just coded, but verified. "Done" for a story includes whatever DESIGN.md/PLAN.md called for: the code itself, tests covering it (unit and integration as appropriate — actually run the project's test suite, not just write tests and assume), a look at security implications for anything touching user input, external data, secrets, or permissions, and load or end-to-end testing for any story PLAN.md flagged as needing it. If a story's acceptance criteria turn out to be unclear or wrong once you're actually building it, say so and fix PLAN.md rather than silently building something else.

You have full tool access, including Bash and file writes — both approval-gated, as always. Use that access to actually build and verify, not just describe what you'd do. Run the real test suite, run real security/lint checks the project already has configured, and use whatever load/e2e tooling the project has (or the story specifies) rather than asserting confidence without running anything.

Delegate isolated sub-tasks when it keeps your own context cleaner. You can dispatch any other loaded agent — including yourself, or general for a quick unrelated lookup — as a subagent tool for a self-contained piece of work that doesn't need your full running context (a subagent starts with a clean slate and reports back a result, the same way a fresh task assignment would). This is especially useful for a large story that's really several independent pieces, or a testing/security/load-testing pass that reads more cleanly as its own focused pass than woven through the main implementation conversation. If the user later adds more specialized agents (a dedicated test-engineer or security-reviewer persona, say), they become available to dispatch the same way, with no changes needed here.

You can run fully autonomously or fully interactively, and everything in between — read the room. Sometimes the right move is pairing closely with the user: explain a nontrivial decision before making it, check in before a big or hard-to-reverse change, work one story at a time with visible progress. Sometimes the ask is "just get the whole plan built" — in that case, work through stories with minimal narration and only surface real blockers or decisions genuinely outside your judgment. Match the level of interaction to what the user actually asked for in this session, not a fixed setting.
`

// defaultAgentFiles lists every agent definition file WriteDefaults writes,
// filename to content: general.md (the original, mode-agnostic fallback
// agent), the three read-only specialist agents (Phase 11 addendum —
// explore/review/debug, each safe to dispatch regardless of which model
// backs it since none can mutate anything), and the four SDLC-persona
// agents (post-v0.1.0 addendum) that replace the old plan/execute mode
// toggle — Research -> Design -> Plan -> Execute, each producing the next
// stage's input document. Order here is purely for readability;
// WriteDefaults writes them in this order but each file's own idempotency
// is independent of the others.
var defaultAgentFiles = []struct {
	filename string
	content  string
}{
	{"general.md", defaultAgentContent},
	{"explore.md", exploreAgentContent},
	{"review.md", reviewAgentContent},
	{"debug.md", debugAgentContent},
	{"research.md", researchAgentContent},
	{"design.md", designAgentContent},
	{"plan.md", planAgentContent},
	{"execute.md", executeAgentContent},
}

// WriteDefaults writes Canopy's default agent definition files into dir (the
// caller passes homeDir/.canopy/agents), creating dir if it doesn't exist.
// It exists so a brand-new Canopy install has agents to pick in the TUI's
// picker without requiring the user to hand-write a .claude/agents/*.md file
// first — see cmd/canopy's run() and this package's doc comment addendum.
//
// Idempotent per file: for each entry in defaultAgentFiles, if
// dir/<filename> already exists — from a prior run, possibly hand-edited by
// the user since — that one file is left untouched rather than overwritten;
// every other missing file is still (re)written. This means a user who
// already has some but not all of these files (e.g. an install that
// predates the four SDLC-persona agents) gets exactly the missing ones
// filled in, not a wholesale overwrite.
func WriteDefaults(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create default agent directory %s: %w", dir, err)
	}

	for _, f := range defaultAgentFiles {
		path := filepath.Join(dir, f.filename)
		if _, err := os.Stat(path); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("failed to stat default agent file %s: %w", path, err)
		}

		if err := os.WriteFile(path, []byte(f.content), 0o644); err != nil {
			return fmt.Errorf("failed to write default agent file %s: %w", path, err)
		}
	}
	return nil
}

// sdlcAgentOrder is the fixed display order for Canopy's own default agents
// (WriteDefaults) in the top-level agent picker and the in-chat ctrl+a
// overlay (post-v0.1.0 addendum, extended by Phase 11): general first, then
// the three read-only specialists (explore/review/debug — always useful,
// model-agnostic, not part of any pipeline), then the SDLC pipeline in the
// order a user actually works through it. Any agent name not in this map —
// including a project's own custom agents, or one of these eight renamed —
// isn't a special case to handle, it's just not in the map, so SortNames
// falls back to plain alphabetical ordering for it. See SortNames's own doc
// comment.
var sdlcAgentOrder = map[string]int{
	"general":  0,
	"explore":  1,
	"review":   2,
	"debug":    3,
	"research": 4,
	"design":   5,
	"plan":     6,
	"execute":  7,
}

// SortNames sorts names in place for display in an agent picker: any name
// matching one of Canopy's own default agents (sdlcAgentOrder) sorts first,
// in that fixed pipeline order; every other name — a project's own custom
// agents, or a renamed default — sorts after, in plain alphabetical order.
// Used by both the top-level picker (tui.NewModel) and the in-chat ctrl+a
// overlay (AgentService.ListAgents) so the two never drift apart.
//
// Deliberately name-keyed, not id-keyed or config-driven: renaming an agent
// (its "name" frontmatter field) is a completely ordinary, supported edit —
// nothing here treats that as an error or a special case to detect. A
// renamed agent simply stops matching a key in sdlcAgentOrder and falls
// into the alphabetical group like any other unrecognized name; nothing
// breaks, no code needs to change, it just no longer sorts to a fixed
// pipeline position.
func SortNames(names []string) {
	sort.Slice(names, func(i, j int) bool {
		pi, oki := sdlcAgentOrder[names[i]]
		pj, okj := sdlcAgentOrder[names[j]]
		switch {
		case oki && okj:
			return pi < pj
		case oki != okj:
			return oki
		default:
			return names[i] < names[j]
		}
	})
}

// splitFrontmatter separates a file's leading "---\n...\n---" YAML block from
// its markdown body. It returns an error if the file has no valid frontmatter
// fences.
func splitFrontmatter(content string) (frontmatterYAML, body string, err error) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	trimmed := strings.TrimLeft(normalized, "\n")

	if !strings.HasPrefix(trimmed, "---\n") && trimmed != "---" {
		return "", "", fmt.Errorf("missing YAML frontmatter: file must start with a %q fence", "---")
	}

	rest := strings.TrimPrefix(trimmed, "---\n")
	rest = strings.TrimPrefix(rest, "---")

	idx := strings.Index(rest, "\n---")
	if idx == -1 {
		return "", "", fmt.Errorf("missing closing %q fence for YAML frontmatter", "---")
	}

	frontmatterYAML = rest[:idx]

	afterClose := rest[idx+len("\n---"):]
	// Consume the rest of the closing fence's line.
	if nl := strings.Index(afterClose, "\n"); nl != -1 {
		body = afterClose[nl+1:]
	} else {
		body = ""
	}

	return frontmatterYAML, body, nil
}
