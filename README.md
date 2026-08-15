# Canopy

Canopy is a terminal-based AI coding agent built on
[agent-framework-go](https://github.com/microsoft/agent-framework-go), aiming for harness quality
on par with [Claude Code](https://claude.com/product/claude-code) — compaction, approval-gated
tool calls, todo tracking, plan/execute mode, and dynamic subagent dispatch, driven by MAF-Go's
own agentic loop rather than a hand-rolled one.

It ships as a single static Go binary with a Bubble Tea TUI and JSON file storage — no Node/npm
runtime, no database. Its differentiator over Claude Code is multi-provider support: point the
same harness at OpenAI, Anthropic, Google Gemini, or any OpenAI-compatible endpoint (DeepSeek,
Ollama, Groq, Mistral, Together, xAI), instead of being locked to one vendor.

## Claude Code file-format compatibility

Canopy reads the same project configuration Claude Code does, so an existing Claude Code project
mostly works unmodified:

| File/directory | What it configures |
|---|---|
| `.claude/agents/*.md` (project, recursive) and `~/.claude/agents/*.md` (personal) | Subagent definitions — YAML frontmatter (`name`, `description` required; `tools`, `model` optional) plus a markdown body used as the system prompt. Project-level definitions win name conflicts with personal ones. |
| `.mcp.json` (project root) | MCP server connections — an `mcpServers` object keyed by name, each entry either `{"command", "args", "env"}` (stdio) or `{"type": "http"\|"sse", "url"}` (remote). |
| `.claude/skills/*/SKILL.md` (project) and `~/.claude/skills/*/SKILL.md` (personal) | Agent Skills, with the same progressive disclosure Claude Code uses: every skill's name/description is always in context; the full body loads on demand via a `Skill` tool; supporting files load on demand via the file-read tool. |
| `CLAUDE.md` and/or `AGENTS.md` (project root) | Project instructions, prepended to the system prompt (both files, if present — `CLAUDE.md` first). |

None of these loaders execute anything beyond what's documented — an MCP server's `command` runs
as configured, with no implicit privilege escalation, and mutating tools (bash, file write),
including MCP-provided ones, are approval-gated the same way Claude Code's are.

## Configuring providers

Canopy's own configuration — which provider(s)/model(s) are available — is a flat JSON file, not a
database, since providers/models are Canopy's value-add over Claude Code and have no existing
Claude Code format to be compatible with:

- `~/.canopy/providers.json` — global, used with `--global`/`-g`.
- `.canopy/providers.json` at the project root — project-local override, used by default when
  present.

Example:

```json
{
  "providers": [
    { "name": "openai", "type": "openai", "api_key": "sk-..." },
    { "name": "anthropic", "type": "anthropic", "api_key": "sk-ant-..." },
    { "name": "local-ollama", "type": "ollama", "base_url": "http://localhost:11434/v1" }
  ],
  "models": [
    { "name": "gpt", "provider": "openai", "model_name": "gpt-4o-mini" },
    { "name": "claude", "provider": "anthropic", "model_name": "claude-sonnet-4-5", "context_window_tokens": 200000 },
    { "name": "llama", "provider": "local-ollama", "model_name": "llama3.1" }
  ]
}
```

Any agent definition can be paired with any configured model by name (an agent's frontmatter
`model:` field), or falls back to the first entry in `models` if it doesn't set one.
Supported `type` values: `openai`, `anthropic`, `gemini`, `deepseek`, `ollama`, `groq`, `mistral`,
`together`, `xai`.

## Build and run

Requires Go 1.25+.

```sh
make build          # dist/canopy, a static binary for the host GOOS/GOARCH
make build-all       # dist/canopy_<version>_<os>_<arch> for linux/darwin × amd64/arm64
make install         # go install with the version baked in, into $GOBIN
```

Every target bakes a version string into the binary at link time
(`-ldflags="-X main.version=..."`); override it explicitly for a real release:

```sh
make build VERSION=v0.1.0
```

Without `VERSION`, it defaults to `git describe` (or `dev` outside a git checkout). Run it from a
project directory that has at least one `.claude/agents/*.md` definition and a
`.canopy/providers.json`:

```sh
./dist/canopy            # project-local config
./dist/canopy --global   # ~/.canopy config instead
./dist/canopy --version
```

A `Dockerfile` is also provided as a secondary, optional packaging path (`make docker`) — the
static binary above is the primary way to run Canopy; the container exists for deployments that
specifically want one.

## Optional OpenTelemetry tracing

Off by default — no SDK is initialized, no tracing middleware is attached, zero overhead. Turn it
on with `--otel`, or implicitly by setting the standard `OTEL_EXPORTER_OTLP_ENDPOINT` (or
`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`) environment variable; spans export over OTLP/HTTP to
whatever collector those variables point at (default `http://localhost:4318`, per the OTel SDK
spec). A missing or unreachable collector never blocks startup or hangs a run — see
`internal/impl/tracing`'s doc comment for the mechanics.

## Status

Canopy has a working, testable core: multi-turn chat with tool calls and persisted history,
compaction, approval-gated tools, todo tracking, plan/execute mode, dynamic subagent dispatch, and
a Bubble Tea TUI, all driven through MAF-Go's own loop. See `docs/`:

- [`docs/REQUIREMENTS.md`](docs/REQUIREMENTS.md) — what Canopy must do and why.
- [`docs/DESIGN.md`](docs/DESIGN.md) — how it's built, grounded in the actual agent-framework-go
  APIs.
- [`docs/PLAN.md`](docs/PLAN.md) — phased implementation plan and current status.

See [`AGENTS.md`](AGENTS.md) for the dev workflow (build/test/lint commands, code style, QA steps)
if you're working on Canopy itself.
