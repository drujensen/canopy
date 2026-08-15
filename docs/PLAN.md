# Canopy — Implementation Plan

Status: draft v0.3 (supersedes v0.2 — TUI+JSON only, narrowed tools, added a file-format
compatibility phase, added a performance-benchmark step; see REQUIREMENTS.md §5)
Depends on: REQUIREMENTS.md, DESIGN.md

Phased, milestone-based (no calendar dates — add them once effort is estimated). Each phase lists
exit criteria; don't start a phase until the previous one's exit criteria are met.

## Phase 0 — Bootstrap

- Initialize `github.com/drujensen/canopy` module, Go 1.25 toolchain.
- Pin an exact `agent-framework-go` version; verify `go mod tidy` succeeds.
- Port an `AGENTS.md`-style dev workflow doc from aiagent, adjusted for the new module/toolchain.
- Spike: one hello-world agent using `openaiprovider`, `RunText`, printed to stdout, confirming
  Azure/A2A/AG-UI/Copilot/workflow/mongo/echo/websocket are *not* pulled into the build (Design §1).
- CI (build, vet, test) mirroring aiagent's QA workflow.

**Exit criteria:** repo builds, hello-world agent streams a real response, CI green, `go.sum`
confirmed free of the excluded dependency families.

## Phase 1 — Storage and provider config

- `Chat` entity + `ChatRepository` (JSON only — Design §6); include the `SessionState []byte`
  field from the start, not as a later migration.
- Flat JSON provider/model config (`~/.canopy/providers.json` / project-local override) — read/
  write, no database (Design §4).
- Port `impl/config` (env/flags) unchanged in spirit.

**Exit criteria:** a Chat round-trips through the JSON repository including a non-empty
`SessionState` blob; provider config loads/saves correctly for at least one provider.

## Phase 2 — Providers

- `impl/providers/factory.go` dispatching on provider type.
- Native adapters: OpenAI, Anthropic, Gemini.
- `impl/providers/openaicompat.go` for DeepSeek, Ollama, Groq, Mistral, Together, xAI.
- Table-driven adapter tests against a local mock HTTP server.

**Exit criteria:** every provider in Requirements FR2 can be configured and complete a real (or
mocked, in CI) chat turn.

## Phase 3 — Core tools

Port the narrow tool set from Design §3.2 — bash/shell, file read/write/search, directory, web
search/fetch — each as its own PR-sized unit ending in a `tool.FuncTool` implementation plus a
contract test. Do **not** port browser automation, image generation, vision, git PR, swagger, or
memory in this phase (Requirements §5) — if one of those turns out to be genuinely needed, it
should show up as an MCP server in Phase 3.5, not a built-in here.

**Exit criteria:** the seven core tools exist, pass contract tests, and are callable end-to-end by
a test agent; approval-gating verified for bash and file-write.

## Phase 3.5 — Claude Code file-format compatibility layer

New phase in this revision (Design §3.11) — this is what makes "drop-in" a real claim, so it's
sequenced before the agentic loop is wired up, not after.

- `impl/agentsource`: load `.claude/agents/*.md` (project + personal, recursive), parse
  frontmatter + body, produce `AgentDefinition`s.
- `impl/mcpsource`: load `.mcp.json`, construct `tool/mcptool` clients per configured server.
- `impl/skillsource`: load `.claude/skills/*/SKILL.md`, implement progressive disclosure (name+
  description always available, full body via a `Skill` tool, supporting files read on demand).
- `impl/projectcontext`: load `CLAUDE.md`/`AGENTS.md` into system instructions.
- Fixture-based tests for all four against real example files, including malformed/partial
  frontmatter (Design §7).

**Exit criteria:** pointed at a real, existing Claude Code project directory, Canopy's loaders
correctly enumerate its agents, MCP servers, and skills, and load its project instructions —
verified against that actual directory, not just synthetic fixtures.

## Phase 4 — Agentic loop core

- `impl/harness` wiring `agent/harness/loop` and `toolautocall`.
- `HistoryProvider` backed by `ChatRepository`.
- Dynamic subagent dispatch: any agent loaded by `agentsource` can be wrapped via
  `tool/agenttool` and added to another agent's `Config.Tools` (FR9); test that inspects the
  parent's history after a subagent call to verify session isolation (Design §3.4).
- `domain/services` agent-run orchestration on top of `agent.New` + harness wiring.
- Structured logging (`Config.Logger`) bridged to zap.

**Exit criteria:** a configured Agent + Model holds a multi-turn conversation with tool calls and
persisted history; a parent agent dispatches to a child agent as a tool call; both driven purely
through MAF-Go's loop.

## Phase 5 — Loop quality: compaction, approvals, todo, mode

Give this real time — it's the phase that actually earns "as good as Claude Code."

- **Compaction (FR10):** `TokensExceed(N)`-triggered `PipelineStrategy` per agent; regression test
  against a long-conversation fixture for task continuity.
- **Approvals (FR5):** wire `agent/harness/toolapproval`; persist `Rule` state via the
  `Chat.SessionState` blob (Phase 1) so "always allow" survives a restart.
- **Todo (FR11):** wire `agent/harness/todo`.
- **Mode (FR12):** wire `agent/harness/agentmode` (default plan/execute); decide and implement how
  plan mode restricts mutating tools (Design §3.8's flagged open point).

**Exit criteria:** all four work end-to-end against a real provider: a session compacts under
load, an approval rule persists across a process restart, a todo list updates as the agent works,
a mode switch (agent- and user-initiated) changes tool availability.

## Phase 6 — TUI

- Chat view consuming `ResponseStream` for incremental rendering.
- Approval-prompt component, live todo panel, mode indicator/switcher (Design §5).
- Agent picker sourced from `agentsource`, not a database list.
- Manual QA pass in a real terminal: multi-turn chat that triggers compaction, an approval-gated
  tool call, a todo-tracked task, a mode switch, and at least one dynamic subagent dispatch.

**Exit criteria:** a full session — chat, approvals, todo, mode, subagent dispatch — works
end-to-end in the TUI against a real provider.

## Phase 7 — Observability, security, packaging

- `Config.Logger` → zap bridge finished; `provider/otelprovider` wired as optional (not required
  for v1 usage).
- Security pass: no secrets in logs by default, bash/file input validation and approval-gating
  re-verified, MCP-provided mutating tools respect approval-gating the same as built-ins, standing
  "always allow" rules can't be tricked into covering a broader call than approved.
- Single static binary build/release process (replaces aiagent's Docker-first packaging — a
  standalone binary is the point of the "lighter" goal); Docker remains optional, not primary.
- Canopy's own `README.md`/`AGENTS.md`.

**Exit criteria:** a released binary runs standalone with no runtime dependency beyond the OS; a
security review pass finds no regressions against aiagent's posture, with specific attention to
the file-format loaders (Phase 3.5) not executing anything from a loaded file beyond what's
documented (e.g. an MCP server's `command` runs as configured, not with any implicit escalation).

## Phase 8 — Beta hardening and benchmarking

- Fill test-coverage gaps from Phases 1–7; add `-race` to CI.
- Load/streaming check: long chat forcing multiple compaction cycles, confirm no goroutine leaks
  around `ResponseStream` iteration.
- **Benchmark against `claude` CLI (Requirements §4.9, §8):** cold-start time and idle memory
  footprint, measured, reported as-is — not adjusted to hit a target. If Canopy isn't actually
  faster/lighter, that's a real finding to act on (profile and fix, or revise the claim), not to
  skip measuring.
- Cut `v0.1.0`.

**Exit criteria:** `go test ./... -race` clean; benchmark numbers recorded; v0.1.0 tagged.

## Sequencing notes

- Phase 1 and Phase 2 can run in parallel once Phase 0 is done.
- Phase 3 (core tools) can start as soon as Phase 1's `Chat`/config shapes exist; doesn't need
  Phase 2 finished.
- Phase 3.5 (file-format loaders) has no dependency on Phases 1–3 beyond Phase 0's module setup —
  it's pure file parsing — and could in principle run in parallel with Phases 1–3. It's listed
  after Phase 3 here because agent construction (Phase 4) needs both the tool set and the loaders
  to exist, and doing tools first keeps the loader's `AgentDefinition → *agent.Agent` wiring
  simpler to test (real tools to attach, not stubs).
- Phase 4 needs Phases 1–3.5 done.
- Phase 5 needs Phase 4.
- Phase 6 needs Phase 5.
- Web UI and MongoDB support (deferred per Requirements §5) are candidate future phases, picked up
  as additive work against the same `domain/services`/`ChatRepository` interface, not a rewrite.
  Data-migration tooling from aiagent's `~/.aiagent` storage remains a candidate, unplanned item.
