# Canopy — Requirements

Status: draft v0.3 (supersedes v0.2 — scoped down to TUI+JSON, narrowed tool set, added Claude
Code file-format compatibility as a core requirement; see §5 for what changed and why)
Owner: drujensen

## 1. Summary

Canopy is a ground-up rewrite of `aiagent` (github.com/drujensen/aiagent), built on
`github.com/microsoft/agent-framework-go` (MAF-Go) as the harness. The bar is explicit: **harness
quality on par with Claude Code**, packaged as a standalone Go binary a user could plausibly run
*instead of* `claude` in an existing project — not a fork of Claude Code, not a UI clone, but
something that reads the same on-disk conventions (`.claude/agents/`, `.claude/skills/`,
`.mcp.json`, `CLAUDE.md`) so switching doesn't mean reconfiguring everything, and that aims to be
faster and lighter than a Node-based CLI by virtue of being a single static Go binary.

For v1, that means:

- **One frontend**: the Bubble Tea TUI. The web UI from earlier drafts of this project is
  deferred, not cut for good (§5).
- **One storage backend**: JSON files on disk. MongoDB is deferred, not cut for good (§5).
- **A small, Claude-Code-comparable core tool set** — not aiagent's full breadth. Extensibility
  comes from MCP, not from Canopy shipping ever more built-in tools.
- **File-based configuration for agents, skills, and MCP servers**, compatible with Claude Code's
  own formats, so an existing Claude Code project's `.claude/` directory works in Canopy largely
  unmodified.
- Canopy's own differentiator over Claude Code stays multi-provider support (OpenAI, Anthropic,
  Gemini, and OpenAI-compatible endpoints), which is genuinely useful and genuinely not something
  Claude Code does.

Canopy is a new Go module (`github.com/drujensen/canopy`), a new git repository, and a new
`~/.canopy` storage location. It is not an in-place migration of aiagent's data.

## 2. What "as good as Claude Code" means here

Claude Code's own orchestration is not a graph: it's one agentic loop (context → tool call →
result → repeat) plus a single dynamic primitive — a Task/Agent tool that spawns a fresh-context
subagent on demand, gets a result back, and is done. The quality comes from everything around that
loop, and this is what Canopy is actually building:

- **Tight, composable tools** (a small set of well-scoped tools, not a kitchen sink — §6, FR4).
- **Context management** so long sessions don't blow the context window — compaction, not just
  hoping the conversation stays short (§6, FR10).
- **A permission/approval layer** for risky actions, with both one-off and "always allow this"
  semantics (§6, FR5).
- **Progress visibility** — a todo/plan list the agent maintains and the user can see (§6, FR11).
- **Mode separation** — a "plan" mode that can't mutate anything vs. an "execute" mode that can
  (§6, FR12).
- **Subagent dispatch used for context isolation**, not workflow composition (§6, FR9).
- **Reading the same project configuration Claude Code reads** — agents, skills, MCP servers,
  project instructions — so the harness quality isn't undercut by every project needing bespoke
  Canopy config (§6, FR17–FR20). This is new in this revision and is what makes "drop-in" a real
  claim rather than marketing.

MAF-Go ships first-class support for the loop-quality items (see DESIGN.md for grounded
package-level detail); the file-format compatibility items are Canopy's own code to write, since
MAF-Go has no opinion on how agents/tools are configured on disk.

## 3. Background — what exists today

aiagent (Go, DDD-layered: `domain` / `impl` / `tui` / `ui`) provides a Bubble Tea TUI and an
Echo-based web server, hand-rolled provider integrations for nine providers, a hand-rolled
agentic loop with no context compaction, a broad tool set (bash, file ops, web search, fetch,
browser automation via go-rod, image generation, vision, git PR creation, MCP client, skills,
todo, memory, swagger), and dual JSON/MongoDB storage for Agents, Chats, Models, Providers, Tools,
Skills, Plans, and Tasks — all configured as database rows through repository interfaces, edited
via the TUI/web UI rather than as files. `agent_tool.go` already lets one configured agent invoke
another as a tool.

This revision keeps the harness-replacement rationale (provider calls, tool-calling, the loop —
see DESIGN.md) but explicitly does **not** carry over aiagent's "everything is a DB-backed entity
edited through a UI" model for Agents and Skills. Claude Code's agents and skills are files a user
edits directly (or a package manager installs); that's the model Canopy adopts for those two,
because it's what makes existing Claude Code projects portable to Canopy (§6, FR17/FR19).

## 4. Goals

1. **Bubble Tea TUI, v1 only.** No web UI in v1 (§5).
2. **JSON file storage, v1 only.** No MongoDB in v1 (§5).
3. **MAF-Go as the harness** for provider calls, tool-calling, the agentic loop, streaming,
   history, compaction, approvals, and subagent dispatch (unchanged from prior drafts).
4. **Provider parity with aiagent**, not Claude Code — this is Canopy's actual value-add. OpenAI,
   Anthropic, Gemini natively via MAF-Go; DeepSeek, Ollama, Groq, Mistral, Together, xAI via a
   generic OpenAI-compatible adapter.
5. **A core tool set comparable to Claude Code's**, not aiagent's full breadth (§6, FR4/FR6).
6. **File-based, Claude-Code-format-compatible configuration** for agents (`.claude/agents/*.md`),
   skills (`.claude/skills/*/SKILL.md`), MCP servers (`.mcp.json`), and project instructions
   (`CLAUDE.md`/`AGENTS.md`) — §6, FR17–FR20.
7. **Loop quality**: compaction, approvals with standing rules, todo/progress tracking, plan/execute
   mode — all first-class and visible in the TUI (unchanged from prior drafts, §6 FR5/FR10–FR12).
8. **Dynamic subagent dispatch**, not a graph engine (unchanged, §6 FR9).
9. **Faster and lighter than Claude Code** at the things that are directly comparable: process
   startup time, idle memory footprint, and binary distribution (a single static binary, no
   Node/npm runtime). This is a goal to validate with real measurements (Plan, Phase 9), not a
   number promised up front.

## 5. Non-goals (v1) — what changed from the previous draft, and why

- **No web UI in v1.** Not cut permanently — the DESIGN.md from prior drafts already scoped a web
  layer that reuses the same domain services, and that plan is still sound. It's just not being
  built alongside the TUI right now; a TUI-only build keeps the surface area small enough to get
  the loop-quality work (§4.7) right first.
- **No MongoDB in v1.** Same reasoning — the repository-interface pattern still supports adding a
  mongo implementation later without touching callers, it's just not implemented now. JSON files
  under `~/.canopy` (or project-local `.canopy/`) are the only storage for v1.
- **No graph/workflow engine.** Carried over from the previous revision (see that decision's
  rationale, still valid): Claude Code's quality doesn't come from a graph, and neither should
  Canopy's. Multi-agent composition stays dynamic (agent-as-tool, FR9) only.
- **A narrower built-in tool set than aiagent has today.** Browser automation (go-rod), image
  generation, vision, git PR creation, swagger, and memory are **not** ported as built-in tools in
  v1 — Claude Code doesn't ship them as built-ins either, and extensibility is meant to come from
  MCP (FR18), not from Canopy accumulating a larger built-in surface than Claude Code has. Any of
  these can come back later as an MCP server rather than a hand-maintained built-in tool.
- **No automatic migration** of existing `~/.aiagent` JSON/Mongo data into Canopy's schema.
- **No hosting of Canopy agents behind A2A/AG-UI/Copilot protocols**, and no importing the MAF-Go
  packages for them (dependency hygiene, unchanged).
- **No MCP *server* hosting** — Canopy is an MCP *client* only, same as Claude Code.
- **No claim of exact behavioral parity with Claude Code's proprietary internals** (its exact
  system prompt, its exact tool implementations, its hooks system, its settings.json permission
  model). The compatibility target is the *documented, file-based configuration surface*
  (agents/skills/MCP/project-instructions), not a black-box clone.

## 6. Functional requirements

| ID | Requirement |
|----|-------------|
| FR1 | Users can configure Providers (API key, base URL, model) and Models (provider + model name + parameters), and pair any Agent with any Model. |
| FR2 | Provider calls run through MAF-Go provider adapters: native for OpenAI, Anthropic, Gemini; a generic OpenAI-compatible adapter (custom base URL + key) for DeepSeek, Ollama, Groq, Mistral, Together, xAI. |
| FR3 | Provider/Model configuration is a small JSON file (not a database), consistent with FR-storage decisions in §5. |
| FR4 | The built-in tool set is comparable in breadth to Claude Code's: shell/bash execution, file read/write/search, directory listing, web search, and web fetch. That's the core set for v1 — see §5 for what's deliberately excluded. |
| FR5 | Tools that mutate state (bash, file write) require approval before running, using MAF-Go's `tool.ApprovalRequiredTool` and `agent/harness/toolapproval`, including one-off approval and standing "always allow this tool" / "always allow this tool with these arguments" rules that persist across restarts. |
| FR6 | MCP servers configured in `.mcp.json` (FR18) contribute additional tools automatically via `tool/mcptool` — this is the extensibility mechanism, not a growing built-in tool list. |
| FR7 | The agentic run loop (send message → model responds → tool calls execute → results fed back → repeat until final answer) is provided by MAF-Go's `agent/harness/loop` and `toolautocall`, not hand-written. |
| FR8 | Responses stream incrementally to the TUI, backed by MAF-Go's `ResponseStream` (`iter.Seq2[*ResponseUpdate, error]`). |
| FR9 | A single agent can invoke another configured agent as a tool call at runtime, on its own judgment, to work a subtask in an isolated context and return a clean result — MAF-Go's `tool/agenttool`, the Task-tool equivalent. No predefined graph, no fixed topology. |
| FR10 | Long conversations are automatically compacted before they threaten the model's context window (sliding window and/or summarization), using MAF-Go's `agent/compaction` package. |
| FR11 | Agents can maintain and surface a todo/progress list during multi-step tasks, using MAF-Go's `agent/harness/todo`; the TUI renders the current list live. |
| FR12 | Agents can operate in distinct modes (at minimum plan vs. execute), using MAF-Go's `agent/harness/agentmode`; the TUI shows the current mode and lets the user switch it. |
| FR13 | Chat history persists per session across turns and process restarts, via a JSON-backed `HistoryProvider`/`ChatRepository`. |
| FR14 | Session-scoped state from FR5/FR11/FR12 (approval rules, todo items, current mode) persists as one serialized `*agent.Session` blob on the Chat record (see DESIGN.md), so it survives a restart. |
| FR15 | The CLI supports at minimum: default TUI mode, `--storage=file` (the only supported value in v1, but the flag stays for forward compatibility with FR-deferred mongo support), `--global`/`-g`, `--version`. |
| FR16 | Structured logging (zap) captures run, middleware, and provider diagnostics; API keys and full sensitive payloads are never logged by default. |
| FR17 | Canopy discovers and loads agent definitions from `.claude/agents/*.md` (project, recursive) and `~/.claude/agents/*.md` (personal, recursive), parsing YAML frontmatter (`name`, `description` required; `tools`, `model` optional) with the markdown body as the system prompt — the same format and locations Claude Code uses, so an existing project's subagents load unmodified. Project-level definitions win name conflicts with personal ones. |
| FR18 | Canopy discovers and loads MCP server definitions from `.mcp.json` at the project root, matching Claude Code's schema: an `mcpServers` object keyed by name, each entry either `{command, args, env}` (stdio) or `{type: "http"/"sse", url}` (remote). |
| FR19 | Canopy discovers and loads skills from `.claude/skills/*/SKILL.md` (project) and `~/.claude/skills/*/SKILL.md` (personal), matching the Agent Skills spec: YAML frontmatter (`name`, `description`) always available to the agent, full body loaded when the skill is relevant, supporting files in the skill's directory loaded on demand (progressive disclosure). |
| FR20 | Canopy auto-loads project instructions from `CLAUDE.md` and/or `AGENTS.md` at the project root into the system prompt, matching Claude Code's project-instructions convention. |

## 7. Non-functional requirements

- **Security.** No API keys or secrets in logs by default. Bash/file tools retain input validation
  (command injection, path traversal) and approval-gating. MCP server configs are read, not
  auto-executed with elevated trust — approval-gating (FR5) applies to MCP-provided mutating tools
  too, wherever the tool is marked as requiring approval.
- **Portability.** Single static Go binary; no Node/npm runtime dependency (this is part of the
  "lighter" claim in §4.9, not just a footnote).
- **Performance.** Startup time and idle memory footprint should be measured against the `claude`
  CLI and against aiagent as a baseline once Canopy is functional (Plan, Phase 9) — this is a goal
  to validate, not a number to promise before measuring.
- **Toolchain.** Go 1.25+ (MAF-Go requires `go 1.25.0`).
- **Testability.** Unit test coverage with `testify`; dedicated tests for compaction, approval
  persistence, and the three file-format loaders (FR17–FR19), since those are Canopy's own code
  (not MAF-Go's) and the most likely place for subtle format-compatibility bugs.
- **Extensibility.** Adding a new provider should not require touching unrelated layers. Adding a
  new *tool* is deliberately supposed to mean "point at an MCP server," not "extend Canopy's Go
  code" — that's the point of FR6/FR18.
- **Dependency hygiene.** Only import the MAF-Go packages Canopy actually uses (unchanged from
  prior drafts): `agent`, `tool`, `agent/compaction`, `agent/harness/*`,
  `openaiprovider`/`anthropicprovider`/`geminiprovider`, `tool/{mcptool,shelltool,functool,
  agenttool}`. Avoid `workflow*`, `foundryprovider`, `a2aprovider`, `aguiprovider`,
  `copilotprovider`.

## 8. Risks / open questions

- **MAF-Go is public preview** — pin version, re-evaluate on upgrade. The loop-quality subsystems
  (`agent/compaction`, `agent/harness/{toolapproval,todo,agentmode}`) are the least stable surface
  within an already-preview framework.
- **Claude Code's file formats are not all a versioned, stable spec.** Agent Skills became an open
  spec in December 2025 and is the most stable of the three; the subagent markdown frontmatter and
  `.mcp.json` schema are documented but not formally versioned. Canopy should implement against
  the current documented format, expect occasional drift, and treat "reads Claude Code's config"
  as a compatibility target to maintain, not a one-time port.
- **"Faster and lighter" is unverified until measured.** Go's static-binary startup advantage over
  a Node CLI is a reasonable expectation, not a guarantee — MAF-Go's own dependency graph, tool
  execution overhead, and the compaction/approval bridging work could offset it. Flagged
  explicitly so Phase 9 benchmarking isn't skipped.
- **No decision yet on data-migration tooling** from aiagent's `~/.aiagent` storage.
- **Web UI and MongoDB are deferred, not designed away** — when either is picked back up, revisit
  whether the JSON-only/TUI-only decisions made here (e.g., flat-file provider config, `Chat`
  schema shape) still fit, rather than assuming they generalize unchanged.

## 9. Success criteria

- Given an existing Claude Code project (with `.claude/agents/`, `.claude/skills/`, `.mcp.json`,
  `CLAUDE.md`), running Canopy in that directory picks up the same agents, skills, and MCP
  servers without any Canopy-specific configuration.
- A long-running chat session automatically compacts without losing task continuity.
- Approval prompts support "approve once" and "always allow," and "always allow" survives a
  restart.
- A multi-step task shows a live todo/progress list in the TUI.
- A user can see and switch the agent's current mode (plan vs. execute) in the TUI.
- A parent agent can dispatch a subtask to another configured agent as a tool call and get back a
  clean result without polluting its own conversation context.
- Canopy's core tool set plus any configured MCP servers cover what a Claude Code session in the
  same project could do — minus the deliberately-excluded built-ins in §5.
- `go build`, `go vet`, and `go test ./...` are clean on Go 1.25 with only the intentionally
  scoped MAF-Go packages imported, no mongo-driver, no web-server dependency, in the v1 build.
- Canopy's cold-start time and idle memory footprint are measured against `claude` and reported,
  whatever the result — the goal is a real number, not an assumption.
