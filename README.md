# Canopy

Canopy is a terminal-based AI coding agent built on
[agent-framework-go](https://github.com/microsoft/agent-framework-go), aiming for harness quality
on par with [Claude Code](https://claude.com/product/claude-code) — compaction, todo tracking,
dynamic subagent dispatch, and a set of SDLC-persona agents (research → design → plan → execute),
driven by MAF-Go's own agentic loop rather than a hand-rolled one.

It ships as a single static Go binary with a Bubble Tea TUI and JSON file storage — no Node/npm
runtime, no database. Its differentiator over Claude Code is multi-provider support: point the
same harness at OpenAI, Anthropic, Google Gemini, or any OpenAI-compatible endpoint (DeepSeek,
Ollama, Groq, Mistral, Together, xAI), instead of being locked to one vendor.

## Claude Code file-format compatibility

Canopy reads the same project configuration Claude Code does, so an existing Claude Code project
mostly works unmodified:

| File/directory | What it configures |
|---|---|
| `.claude/agents/**/*.md` (project, recursive), `~/.claude/agents/**/*.md` (personal, recursive), and `~/.canopy/agents/**/*.md` (Canopy's own defaults) | Subagent definitions — YAML frontmatter (`name`, `description` required; `tools`, `model` optional) plus a markdown body used as the system prompt. Precedence is project, then personal, then Canopy's defaults. |
| `.mcp.json` (project root) | MCP server connections — an `mcpServers` object keyed by name, each entry either `{"command", "args", "env"}` (stdio) or `{"type": "http"\|"sse", "url"}` (remote). |
| `.claude/skills/*/SKILL.md` (project), `~/.claude/skills/*/SKILL.md` (personal), and `~/.canopy/skills/*/SKILL.md` (Canopy's own defaults) | Agent Skills, with the same progressive disclosure Claude Code uses: every skill's name/description is always in context; the full body loads on demand via a `Skill` tool; supporting files load on demand via that same tool, confined to the skill's own directory. |
| `CLAUDE.md` and/or `AGENTS.md` (project root) | Project instructions, prepended to the system prompt (both files, if present — `CLAUDE.md` first). |

The `~/.canopy/agents` and `~/.canopy/skills` sources are Canopy's smallest-precedence layer,
seeded automatically on first run (see [Zero-configuration first
run](#zero-configuration-first-run)) — they never shadow a project's or your own `~/.claude`
definitions of the same name.

None of these loaders execute anything beyond what's documented — an MCP server's `command` runs
as configured, with no implicit privilege escalation. There is deliberately **no approval-gating
layer**: a tool call (built-in or MCP-provided, bash and file-write included) executes as soon as
the model requests it. What actually constrains a tool is its own input validation — path-traversal
confinement in the file tools, an SSRF denylist rejecting loopback/private/link-local destinations
in web fetch — not a confirmation prompt. See `docs/DESIGN.md` §3.6 for why the approval flow was
removed.

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
    { "name": "anthropic", "type": "anthropic", "api_key_env": "ANTHROPIC_API_KEY" },
    { "name": "local-ollama", "type": "ollama", "base_url": "http://localhost:11434/v1" }
  ],
  "models": [
    { "name": "gpt", "provider": "openai", "model_name": "gpt-4o-mini" },
    { "name": "claude", "provider": "anthropic", "model_name": "claude-sonnet-4-5", "context_window_tokens": 200000 },
    { "name": "llama", "provider": "local-ollama", "model_name": "llama3.1" }
  ]
}
```

Set either `api_key` (a literal secret) or `api_key_env` (the *name* of an environment variable to
resolve the key from at load time — only the variable's name is ever written to disk, never the
secret). An explicit `api_key` always wins over `api_key_env`.

Any agent definition can be paired with any configured model by name (an agent's frontmatter
`model:` field), falling back to the model you last switched to with `ctrl+o`, and failing that to
the first entry in `models`.

The nine `type` values with first-class adapters are `openai`, `anthropic`, `gemini`, `deepseek`,
`ollama`, `groq`, `mistral`, `together`, and `xai`. Any other `type` also works as long as
`base_url` is set — it's routed through the generic OpenAI-compatible adapter, so a provider not on
that list needs no code change to use.

## Zero-configuration first run

Neither a `.claude/agents/*.md` file nor a `.canopy/providers.json` is required to start. On first
run Canopy:

1. Writes five default agents (`general`, plus the `research`/`design`/`plan`/`execute` SDLC
   personas) to `~/.canopy/agents`, and one default skill (`mcp-server-setup`) to
   `~/.canopy/skills`. Per-file idempotent, so a later-deleted default file regenerates on the next
   run and your own edits are never overwritten.
2. Fetches the free [models.dev](https://models.dev) catalog (24h-cached locally, no
   authentication), checks which provider API-key environment variables are already set, and
   writes a `providers.json` pairing each match with its most recently released tool-call-capable
   model. The provider's API key is stored as `api_key_env`, never as a literal.

Ordinary startup afterwards re-syncs against the cached catalog, additively: newly detectable
providers and newly listed models get added, models the catalog no longer lists for a re-detected
provider get removed, and per-model cost is refreshed. A provider you configured by hand — a
private Ollama server, say — is never touched, since the catalog has no opinion on it. `--refresh-providers`
forces an immediate live re-fetch instead of waiting out the cache.

## Build and run

Requires Go 1.25+.

```sh
make build          # dist/canopy, a static binary for the host GOOS/GOARCH
make build-all       # dist/canopy_<version>_<os>_<arch> for Linux/macOS/Windows × amd64/arm64
make install         # go install with the version baked in, into $GOBIN
```

Every target bakes a version string into the binary at link time
(`-ldflags="-X main.version=..."`); override it explicitly for a real release:

```sh
make build VERSION=v0.1.0
```

`make build-all VERSION=v0.1.0` produces release binaries for Linux, macOS, and Windows on
amd64 and arm64. Windows binaries use the `.exe` suffix:

```text
dist/canopy_v0.1.0_linux_amd64
dist/canopy_v0.1.0_linux_arm64
dist/canopy_v0.1.0_darwin_amd64
dist/canopy_v0.1.0_darwin_arm64
dist/canopy_v0.1.0_windows_amd64.exe
dist/canopy_v0.1.0_windows_arm64.exe
```

### Creating a GitHub release

Push a semantic-version tag such as `v0.2.0` to the repository:

```sh
git tag v0.2.0
git push origin v0.2.0
```

The `release.yml` GitHub Actions workflow runs for tags matching `v*`, builds all six binaries
with the tag as the embedded version, and attaches them to a GitHub Release. The release workflow
requires the repository's Actions setting **Workflow permissions** to allow read and write access,
which is provided by the workflow's `contents: write` permission.


Without `VERSION`, it defaults to `git describe` (or `dev` outside a git checkout). Run it from the
project directory you want Canopy to work in:

```sh
./dist/canopy                     # project-local config
./dist/canopy --global            # ~/.canopy config instead
./dist/canopy --continue          # resume the most recent chat session
./dist/canopy --refresh-providers # force a live models.dev re-fetch
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
streaming responses, context compaction, todo/progress tracking, dynamic subagent dispatch, MCP
client support, and a Bubble Tea TUI, all driven through MAF-Go's own loop. Measure the "faster and
lighter" claim in [`docs/BENCHMARKS.md`](docs/BENCHMARKS.md). See `docs/`:

- [`docs/REQUIREMENTS.md`](docs/REQUIREMENTS.md) — what Canopy must do and why.
- [`docs/DESIGN.md`](docs/DESIGN.md) — how it's built, grounded in the actual agent-framework-go
  APIs.
- [`docs/PLAN.md`](docs/PLAN.md) — phased implementation plan and current status.

See [`AGENTS.md`](AGENTS.md) for the dev workflow (build/test/lint commands, code style, QA steps)
if you're working on Canopy itself.
