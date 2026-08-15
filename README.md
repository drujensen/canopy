# Canopy

Canopy is a rewrite of [aiagent](https://github.com/drujensen/aiagent) built on top of
[agent-framework-go](https://github.com/microsoft/agent-framework-go), aiming for harness quality
on par with Claude Code — a single well-engineered agentic loop (compaction, approvals, todo
tracking, plan/execute mode, dynamic subagent dispatch) rather than a workflow/graph engine.

For v1: a standalone Go binary with a Bubble Tea TUI, JSON file storage only, and a small
Claude-Code-comparable core tool set. It reads the same project configuration Claude Code does —
`.claude/agents/*.md`, `.claude/skills/*/SKILL.md`, `.mcp.json`, `CLAUDE.md`/`AGENTS.md` — so an
existing Claude Code project mostly works unmodified. Its own differentiator is multi-provider
support (OpenAI, Anthropic, Gemini, and OpenAI-compatible endpoints), which Claude Code doesn't
have. The aspiration is to be faster and lighter than a Node-based CLI; that's a goal to measure,
not a guarantee.

Status: pre-implementation. See `docs/`:

- [`docs/REQUIREMENTS.md`](docs/REQUIREMENTS.md) — what Canopy must do and why.
- [`docs/DESIGN.md`](docs/DESIGN.md) — how it's built, grounded in the actual
  agent-framework-go APIs.
- [`docs/PLAN.md`](docs/PLAN.md) — phased implementation plan.
