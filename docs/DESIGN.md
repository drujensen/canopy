# Canopy — Technical Design

Status: draft v0.3 (supersedes v0.2 — TUI+JSON only for v1, narrowed tool set, adds a Claude Code
file-format compatibility layer; see REQUIREMENTS.md §5 for the full list of what changed)
Depends on: REQUIREMENTS.md

## 1. Module and dependency baseline

```
module github.com/drujensen/canopy
go 1.25

require (
    github.com/microsoft/agent-framework-go vX.Y.Z   // pin exact version, public preview
    github.com/charmbracelet/bubbletea ...            // TUI — the only frontend in v1
    github.com/charmbracelet/bubbles ...
    github.com/charmbracelet/lipgloss ...
    github.com/google/uuid ...
    gopkg.in/yaml.v3 ...                               // agent/skill frontmatter parsing (§3.11)
    github.com/joho/godotenv ...
)
```

Not required in v1 (deferred per REQUIREMENTS.md §5, not deleted from the plan — see that
document for the reasoning): `go.mongodb.org/mongo-driver`, `github.com/labstack/echo/v4`,
`github.com/gorilla/websocket`. Dropping these isn't just fewer imports — it's fewer moving parts
while the loop-quality work (§3.5–§3.8) gets built and tested, and it's a smaller, more honestly
"lightweight" binary for the performance goal in Requirements §4.9.

MAF-Go's own `go.mod` additionally requires the Azure SDK, `a2a-go`, `ag-ui` Go SDK, and the
Copilot SDK — those are only pulled into Canopy's *build* if Canopy imports the MAF-Go packages
that use them. Canopy imports none of them, and imports none of `workflow`, `workflow/
agentworkflow`, or `workflow/checkpoint` either (no graph engine — REQUIREMENTS.md §5).

Packages Canopy imports from MAF-Go: `agent`, `tool`, `message`, `agent/compaction`,
`agent/harness/{loop,toolautocall,toolapproval,todo,agentmode}`,
`provider/{openaiprovider,anthropicprovider,geminiprovider}`,
`tool/{mcptool,shelltool,functool,agenttool}`, `provider/otelprovider` (Plan Phase 7, §3.10 —
optional OTel tracing middleware; only reached when a caller explicitly enables tracing, see
`internal/impl/tracing`).

## 2. Layering

Canopy keeps aiagent's DDD layering (`domain` / `impl` / `tui`) but the layer that changes most in
this revision is `impl`: Agent and Skill configuration stops being database rows behind a
repository interface and becomes files on disk, read the way Claude Code reads them. Chat/session
data is the one thing that still needs a real repository (JSON-only in v1).

```
internal/
  domain/
    entities/        # Chat, Message, ProviderConfig, ModelConfig — no Agent/Skill/Tool entities
    interfaces/       # ChatRepository (JSON-only impl in v1); AgentSource, SkillSource, MCPSource
    services/         # orchestration logic; delegates to MAF-Go instead of hand-rolled loop
  impl/
    providers/        # builds agent.ProviderConfig per configured provider (§4)
    tools/             # narrow core set: shell, file read/write/search, directory, web search/fetch
    harness/           # wires agent/harness/{loop,toolautocall,toolapproval,todo,agentmode}
                        # and agent/compaction into each *agent.Agent at construction time
    agentsource/       # NEW — loads .claude/agents/*.md (§3.11)
    skillsource/       # NEW — loads .claude/skills/*/SKILL.md (§3.11)
    mcpsource/         # NEW — loads .mcp.json, constructs tool/mcptool clients (§3.11)
    projectcontext/    # NEW — loads CLAUDE.md / AGENTS.md into the system prompt (§3.11)
    repositories/
      json/            # ChatRepository only, in v1
    config/            # .env / flags; small flat-JSON provider/model config (§4, §6)
  tui/                 # bubbletea: chat view + approval prompts, todo panel, mode indicator
```

No `ui/` (web) package in v1 — deferred per REQUIREMENTS.md §5, and no `mongo` package under
`repositories/` — same reasoning. Both are additive later without restructuring what's here: the
domain layer already only talks to `ChatRepository` as an interface.

## 3. Core MAF-Go concepts Canopy builds on

Grounded directly in the framework source (`github.com/microsoft/agent-framework-go`), not
inferred from documentation summaries — see the earlier research embedded in this document's
history for the actual files read.

### 3.1 Agent construction

```go
// agent package
type ProviderConfig struct {
    ProviderName string
    Run          RunFunc // func(ctx, []*message.Message, ...Option) iter.Seq2[*ResponseUpdate, error]
    Middlewares  []Middleware
    Format, Unmarshal ...
    CreateSession ...
}

type Config struct {
    ID, Name, Description string
    HistoryProvider  HistoryProvider
    ContextProviders []ContextProvider
    Logger           *slog.Logger
    Middlewares      []Middleware
    Tools            []tool.Tool
    RunOptions       []Option
}

func New(prov ProviderConfig, cfg Config) *Agent
func (a *Agent) RunText(ctx, msg string, opts ...Option) ResponseStream
func (a *Agent) Run(ctx, msgs []*message.Message, opts ...Option) ResponseStream
```

`ResponseStream` is `iter.Seq2[*ResponseUpdate, error]` — range it to drive TUI incremental
rendering per `ResponseUpdate`.

Per-provider convenience constructors exist, e.g. `anthropicprovider.NewAgent(client,
AgentConfig) *agent.Agent`. Canopy's `impl/providers` wraps these behind one factory keyed by
provider type. `Config.ContextProviders` and `Config.Middlewares` are where §3.5–§3.8 attach, and
`Config.Tools` is where an agent definition loaded by `agentsource` (§3.11) ends up.

### 3.2 Tools — a narrow core set (Requirements FR4)

```go
// tool package
type Tool interface { Name() string; Description() string }
type SchemaTool interface { Tool; Schema() any; ReturnSchema() any }
type FuncTool interface { SchemaTool; Call(ctx, args string) (any, error) }
type ApprovalRequiredTool interface { Tool; ApprovalRequired() bool }
func ApprovalRequiredFunc(t FuncTool) FuncTool
```

Canopy's built-in `tool.FuncTool` set, deliberately sized to match Claude Code's own core tools
rather than aiagent's full breadth (Requirements §5):

| Canopy tool | Backing | Approval-gated? |
|---|---|---|
| Bash/shell | `tool/shelltool` (framework-provided local shell) | Yes |
| File read | `impl/tools`, ported from aiagent's `file_read.go` | No |
| File write | `impl/tools`, ported from aiagent's `file_write.go` | Yes |
| File search / grep | `impl/tools`, ported from aiagent's `file_search.go` | No |
| Directory listing | `impl/tools`, ported from aiagent's `directory.go` | No |
| Web search | `impl/tools`, ported from aiagent's `web_search.go` | No |
| Web fetch | `impl/tools`, ported from aiagent's `fetch.go` | No |

Everything else aiagent has today — browser automation (go-rod), image generation, vision, git PR
creation, swagger, memory — is **not** ported as a built-in in v1. A user who wants that
functionality points an MCP server at it (Requirements FR6/FR18); Canopy doesn't grow its own Go
code to cover it. This is a real behavior change from the earlier drafts of this document, not
just smaller scope — it's the mechanism that keeps Canopy's tool surface honestly comparable to
Claude Code's instead of drifting back to aiagent's breadth.

MCP tools use `tool/mcptool` directly (§3.11). Skills are exposed to the model as a tool too — see
§3.11's skill-loading design, which mirrors Claude Code's progressive disclosure rather than
dumping every skill's full content into context up front.

Addendum (post-v0.1.0): the `Skill` tool this section always described (`impl/tools/skill.go`,
`NewSkillTool`) is now actually built and wired in — §3.11 was previously fully specified but
never implemented (no `Skill` tool existed anywhere, `buildInstructions`/`buildTools` never
referenced `Definitions.Skills` at all). `coreToolNames` grew from seven entries to eight (`Skill`
appended); like `WebSearch` (only constructed when a backend is configured), `Skill` is only
constructed into `AgentService.coreTools()`'s available map — and therefore only offered to any
agent — when at least one skill is loaded (`len(s.defs.Skills) > 0`), so a project with no skills
gets no tool offering nothing to look up. Not approval-gated, the same tier as `FileRead`. See
§3.11's own addendum below for the full three-level design as implemented, including level 3's
path-confinement decision.

### 3.3 The agentic loop

`agent/harness/loop` and `toolautocall` provide the run loop and automatic tool-call
execution/feedback (wired in by default via the per-provider constructors unless
`DisableFuncAutoCall` is set). Canopy's `impl/harness` configures which middlewares attach to each
`*agent.Agent` at construction time; it does not reimplement any control flow. §3.5–§3.8 cover the
rest of `agent/harness`.

### 3.4 Dynamic subagent dispatch — Claude Code's Task tool, equivalent (FR9)

```go
// tool/agenttool
func New(a *agent.Agent, config Config) tool.FuncTool
```

Wraps any `*agent.Agent` as a `tool.FuncTool`. Add it to a parent agent's `Config.Tools`, and the
parent decides at runtime whether to call it.

**Context isolation is automatic.** `agenttool`'s `Call` invokes `t.agent.RunText(ctx, in.Query,
t.opts...)` without a `WithSession` option unless the caller's `Config.RunOptions` supplies one;
`Agent.prepareRun` creates a brand-new session whenever none is present. So every subagent
invocation via `agenttool` runs in a fresh session by default — the same "forked, isolated
context" behavior as Claude Code's Task tool, for free.

Agents available for dispatch are exactly the agent definitions `agentsource` loads (§3.11) —
there's no separate "subagent registry"; any loaded agent can be either the top-level agent the
user is talking to, or a tool another agent calls, depending on how it's invoked.

### 3.5 Context compaction (FR10)

`agent/compaction`:

```go
type Trigger func(*MessageIndex) bool
func TokensExceed(maxTokens int) Trigger
func MessagesExceed(maxMessages int) Trigger
func TurnsExceed(maxTurns int) Trigger
func HasToolCalls() Trigger
func All(triggers ...Trigger) Trigger
func Any(triggers ...Trigger) Trigger

type Strategy interface { Compact(ctx, *MessageIndex) (bool, error) }
type PipelineStrategy struct { /* runs strategies in sequence over the same index */ }

func NewContextProvider(cfg ContextProviderConfig) agent.ContextProvider
```

`impl/harness` configures one compaction `ContextProvider` per agent: `TokensExceed(N)` (N derived
from the agent's configured model context window) driving a sliding-window-then-summarization
`PipelineStrategy`, attached via `Config.ContextProviders`. Runs automatically before each turn.

### 3.6 Approvals and permissions (FR5)

`agent/harness/toolapproval`:

```go
type Rule struct { /* ToolName + optional Arguments; nil Arguments matches all invocations */ }
func New(cfg Config) agent.Middleware
```

Pauses the loop on an `ApprovalRequiredTool` call, emits a `ToolApprovalRequestContent`; the
caller responds with approval/denial or "always approve"
(`AlwaysApproveToolApprovalResponseContent`), which becomes a `Rule` checked automatically on
future calls. State lives on `*agent.Session` (§3.9) under its own key.

TUI renders a pending approval request as a prompt with "approve once" / "always allow this
tool" — Claude Code's two-tier permission model.

### 3.7 Todo / progress tracking (FR11)

`agent/harness/todo` — a `ContextProvider` exposing five tools directly to the model (`todos_add`
and four others covering update/complete/list/clear), state on `*agent.Session` (§3.9):

```go
func New(opts *Options) *Provider
func (p *Provider) GetAllItems(opts ...agent.Option) []Item
func (p *Provider) GetRemainingItems(opts ...agent.Option) []Item
```

`impl/harness` attaches `todo.New(nil)`; the TUI reads `GetAllItems`/`GetRemainingItems` to render
a live panel. No aiagent todo tool is ported — this replaces it outright.

### 3.8 Mode switching (FR12)

`agent/harness/agentmode` — a `ContextProvider` tracking a named mode (defaults to plan/execute),
state on `*agent.Session` (§3.9):

```go
type Mode struct { Name, Description string }
func New(cfg Config) *Provider
func (p *Provider) GetMode(opts ...agent.Option) string
func (p *Provider) SetMode(mode string, opts ...agent.Option) error
```

Exposes a tool so the model can request a mode switch itself (Claude Code's `ExitPlanMode`
equivalent); the TUI also calls `SetMode` directly on a user keybinding. Plan mode should pair
with §3.6 (mutating tools stay approval-required or withheld while planning) — the exact
mechanism is `impl/harness`'s to define, flagged for Plan Phase 5.

### 3.9 History and sessions — one mechanism persists §3.6–§3.8's state

`agent.HistoryProvider` backed by `ChatRepository` (JSON, v1) handles chat history (FR13).
Separately, `agent.Session` is a key/value bag scoped to one conversation, explicitly built to be
persisted — its doc comment: *"a Session can be serialized and deserialized directly with
`encoding/json`, so that it can be saved in a persistent store."* `toolapproval`, `todo`, and
`agentmode` all store their state via `session.Get`/`session.Set`, each under its own key in that
one `Session` — none expose a separate store hook of their own. So the integration point is a
single one: after each `Agent.Run`, `impl/harness` serializes the `*agent.Session`
(`json.Marshal`) onto the Chat record's `SessionState []byte` field (§6); before the next run, it
deserializes and passes it via `agent.WithSession(session)`. One write/read path covers approvals,
todos, and mode together (FR14).

### 3.10 Structured output, observability

- `ProviderConfig.Format`/`Unmarshal` for typed responses where needed.
- `Config.Logger` (a `*slog.Logger`) feeds run/middleware/provider diagnostics; bridged to zap via
  a `slog.Handler` adapter. `provider/otelprovider` for optional OTel tracing, deferred to Plan
  Phase 8 (not core to v1 loop functionality).

### 3.11 Claude Code file-format compatibility layer (FR17–FR20)

This is new code Canopy has to write — MAF-Go has no opinion on how agents/tools are configured on
disk. Grounded against Claude Code's documented, current format (see Requirements §8 for the
caveat that this isn't a formally versioned spec, Agent Skills excepted).

**`impl/agentsource` (FR17).** Scans `.claude/agents/**/*.md` (project root, recursive) and
`~/.claude/agents/**/*.md` (personal, recursive). Each file: YAML frontmatter between `---`
fences (`name`, `description` required; `tools` — comma-separated allowlist, omitted = inherit
everything; `model` — optional override) followed by a markdown body used verbatim as the
`agent.Config.Description`/system-prompt instructions. Project-level names win conflicts with
personal ones. Output: a `map[string]AgentDefinition` the domain layer turns into `*agent.Agent`
instances (via `impl/providers` for the model, `impl/tools` + MCP + skills for the tool list).

**`impl/mcpsource` (FR18).** Reads `.mcp.json` at the project root:
`{"mcpServers": {"<name>": {"command", "args", "env"}}}` for stdio servers, or
`{"type": "http"|"sse", "url": "..."}` for remote ones. For each entry, constructs an MCP client
and wraps its tools via `tool/mcptool`, added to every agent's tool list alongside the core
built-ins (§3.2).

**`impl/skillsource` (FR19).** Scans `.claude/skills/*/SKILL.md` (project) and
`~/.claude/skills/*/SKILL.md` (personal). Implements the same progressive disclosure the Agent
Skills spec describes: level 1 — every skill's `name`+`description` frontmatter is always in the
system prompt (cheap, so the model knows what's available); level 2 — a single `Skill` tool (à la
aiagent's existing `skill_tool.go`, kept in concept, re-pointed at this loader) that, given a
skill name, returns the full `SKILL.md` body; level 3 — any supporting files the body references
are read on demand via the existing file-read tool, not preloaded. This keeps context usage
proportional to what's actually used, matching the spec's intent rather than aiagent's simpler
"skill is just a stored prompt fragment" model.

**`impl/projectcontext` (FR20).** Reads `CLAUDE.md` and/or `AGENTS.md` at the project root (both,
if both exist — concatenated, `CLAUDE.md` first) and prepends the content to the agent's system
instructions via `Config.RunOptions`/`agent.WithInstructions`.

All four loaders run once at startup (or on a file-watch/refresh trigger — TBD in Plan Phase 3.5,
not required for v1) and their output feeds `domain/services`' agent-construction path. None of
them touch `ChatRepository` — they're read-only against the project directory, matching how Claude
Code itself treats these files as source-controlled project config, not application state.

Addendum (post-v0.1.0): `impl/agentsource.Load` also scans `~/.canopy/agents/**/*.md` as a third
source, lower-precedence than both project and personal `.claude/agents`. `cmd/canopy`'s `run()`
calls the new `agentsource.WriteDefault` to generate a single default "general" agent there the
first time `Load` returns zero definitions from all three sources, so a brand-new install has
something to select in the TUI's picker instead of a hard startup error — Canopy's closest match to
Claude Code's own no-file-required first-run experience given Canopy's picker-based UX. The
directory is Canopy-specific (`~/.canopy`, not `~/.claude`) so the generated fallback never leaks
into the user's real Claude Code config.

Addendum (post-v0.1.0): `impl/skillsource`'s progressive disclosure, described above in the
original text of this section but never actually wired into `domain/services` until now, is
implemented as follows.

- **Level 1** (always-visible catalog): `AgentService.buildInstructions` appends a
  `## Available Skills` section — one `- <name>: <description>` line per loaded skill, sorted by
  name (`skillsCatalog`) — after the project/agent instruction blocks. Only `name`+`description`
  ever appear here, never a skill's `Body`; the section is omitted entirely when no skills are
  loaded, so an agent/project with none configured sees no trace of this feature in its prompt.
- **Level 2** (on-demand full body): the `Skill` tool (`impl/tools/skill.go`, `NewSkillTool`,
  wired into `AgentService.coreTools()` per §3.2's addendum above) takes `{name}` and returns that
  skill's full `SKILL.md` `Body`, or a clear, specific error naming what skills *are* loaded when
  `name` doesn't match — never a silent empty result.
- **Level 3** (supporting files): the same `Skill` tool accepts an optional second field,
  `{name, file}` — when `file` is set, it reads `filepath.Join(skill.Dir, file)` instead of
  returning the body. This is a deliberate design choice, not reuse of the existing `FileRead`
  tool: `FileRead` is confined to `ToolsConfig.WorkingRoot` (the project directory) via
  `pathsafety.go`'s `resolveSafePath`, and that confinement is load-bearing — it exists to stop a
  model-chosen, otherwise-untrusted path from escaping the project. A *personal* skill loaded from
  `~/.claude/skills/<name>/` sits entirely outside `WorkingRoot`, so routing its supporting-file
  reads through `FileRead` would mean either rejecting them outright or weakening `FileRead`'s
  confinement for every other caller — and a skill's own directory is trusted local config the
  user explicitly installed, not model-supplied input, so it doesn't belong under `FileRead`'s
  boundary in the first place. Instead, the `Skill` tool calls the same `resolveSafePath` helper
  `FileRead` uses, just with the matched skill's own `Dir` as the confinement root instead of the
  project-wide `WorkingRoot` — a `file` path can only ever resolve into the one skill the model
  named, project or personal, and a `../` escape attempt out of that skill's directory is rejected
  exactly the way `FileRead` rejects one against its own root.
- **TUI (`ctrl+s`)**: a read-only skills-browser overlay (`chatModel.openSkillPicker`,
  `internal/tui/chat.go`), reusing `picker.go`'s overlay pattern (`ctrl+a`/`ctrl+o`) but, since
  browsing a skill isn't a chat-level switch, never calling an `AgentService` mutator — selecting
  an entry folds that skill's real `Body` into the transcript as a system-style informational
  entry (`showSkill`) via the new read-only `AgentService.ListSkills()`/`GetSkillBody()`. Zero
  skills loaded produces a brief "No skills configured." transcript entry rather than a silent,
  unexplained no-op.

## 4. Provider adapter design (FR2)

`impl/providers/factory.go` — one function, keyed on configured provider type:

| Provider | Canopy implementation |
|---|---|
| OpenAI | `provider/openaiprovider` (native) |
| Anthropic | `provider/anthropicprovider` (native) |
| Google | `provider/geminiprovider` (native) |
| DeepSeek, Ollama, Groq, Mistral, Together, xAI | `impl/providers/openaicompat.go` — thin wrapper constructing `openaiprovider` with a custom `BaseURL` |

```go
type OpenAICompatible struct {
    BaseURL string
    APIKey  string
    Model   string
}
func NewOpenAICompatible(cfg OpenAICompatible) *agent.Agent
```

Provider/model configuration itself is a flat JSON file (`~/.canopy/providers.json` or
project-local `.canopy/providers.json`), not a database — consistent with §2/§6. This is Canopy's
own config, distinct from the Claude-format files in §3.11: providers/models are Canopy's
value-add over Claude Code (multi-provider support), so there's no existing format to be
compatible with here.

Addendum (post-v0.1.0): zero-config first run, mirroring §3.11's addendum for agents. Rather than
hard-erroring when `providers.json` is empty/missing, `cmd/canopy`'s `run()` fetches the
free, unauthenticated `https://models.dev/api.json` catalog (`impl/modelsdev`, 24h local cache at
`<providers.json's directory>/models-cache.json` — no new path-resolution scheme) and calls
`impl/config.DetectProviders(catalog, os.Environ())`. For each catalog provider whose associated
API-key env var (`Env` in the catalog, e.g. `OPENAI_API_KEY`) is actually set, it builds a
`ProviderConfig` carrying `APIKeyEnv` (the var's *name*) instead of a literal `APIKey`, paired with
the most recently released tool-call-capable model the catalog lists for that provider (recency +
`tool_call` filter, no hardcoded per-provider preferred-model list — deliberately generic so any
provider the catalog adds later just works). `entities.ProviderConfig.APIKeyEnv` is resolved into
`APIKey` by `ProviderStore.Load` via `os.Getenv`, in memory only, right after reading the file — a
literal `APIKey` already present always wins, and the raw secret is never persisted to
`providers.json`. If detection finds at least one provider, the result is saved and Canopy proceeds
with it immediately, same as the default-agent feature; if the live fetch itself fails, the
existing hard error's wording is extended to say so; if the fetch succeeds but nothing is detected,
the hard error lists the exact env var names Canopy checked (pulled live from the catalog, not a
stale hardcoded list). A `--refresh-providers` flag forces a live re-fetch (bypassing the 24h cache)
and re-runs detection, **adding** any newly-detectable provider to the existing file — matched by
`ProviderConfig.Name`, an already-present entry (even a hand-edited one) is never touched.

The table above ("Provider | Canopy implementation") is no longer a closed list at the dispatch
level: `impl/providers.New`'s `default:` case now routes *any* unrecognized `cfg.Type` through the
same generic OpenAI-compatible adapter as long as `cfg.BaseURL` is set — the auto-detected
`ProviderType` for a non-native catalog provider is just that provider's own catalog ID string
(e.g. `"deepinfra"`, `"cerebras"`), which `New` has never seen a named const for and doesn't need
one. Only an unrecognized type with no `BaseURL` is still a hard error (no endpoint to call). The 9
named `ProviderType` consts are unchanged and still take the explicit native/named-compat paths in
the switch when they match.

Surprise vs. the predecessor aiagent project's version of this catalog: the live models.dev
response has no `"type"` field at all (aiagent's struct carried one, always empty against real
data), and several major, well-known providers — groq, togetherai (not `"together"` — the catalog's
own ID differs from Canopy's `ProviderTypeTogether` const value), mistral, and xai among them — omit
the `"api"` (base URL) field entirely, presumably because their consuming SDKs bake in a default
base URL elsewhere. `DetectProviders` accounts for this: a non-native provider with no catalog base
URL is skipped rather than emitting a config that would fail at dispatch time.

Addendum (post-v0.1.0): `ProviderTypeOllama` was pulled out of the generic OpenAI-compatible
dispatch table above and given its own path, `impl/providers.newOllama` (`ollama.go`), for two
Ollama-specific reasons that don't belong in the path every hosted OpenAI-compatible provider
shares:

- **Base URL ergonomics.** Every other OpenAI-compatible provider is a hosted API whose documented
  base URL is exactly the string that belongs in `ProviderConfig.BaseURL` — nothing to normalize.
  Ollama is the one provider in this list a user is realistically self-hosting and typing from
  memory, so `normalizeOllamaBaseURL` accepts a bare host (`"ai.drujensen.com"`,
  `"localhost:11434"`) and fills in a `"https://"` scheme (a missing scheme defaults to HTTPS, not
  HTTP, since anything other than localhost is overwhelmingly reverse-proxied behind TLS — a user
  who genuinely wants plain HTTP still types `"http://"` explicitly) and the trailing `"/v1"` path,
  rather than requiring the user to type the full Chat-Completions-compatible URL.
- **A confirmed wire-format bug** in Ollama's OpenAI-compatible *streaming* endpoint
  (`POST /v1/chat/completions`, `"stream": true`): a tool-call delta chunk carries a spurious
  `"content":""` alongside `"tool_calls"` in the same delta object, which real OpenAI traffic never
  does (the field is omitted entirely, not sent empty). That co-present empty field trips a
  state-detection bug in the `openai-go` v3 SDK's `ChatCompletionAccumulator` — its
  content-vs-tool-call classification checks whether the `"content"` field is *present* in the raw
  JSON before checking `"tool_calls"`, so it misclassifies the delta and the accumulator's
  `JustFinishedToolCall()` never fires. `agent-framework-go`'s `provider/openaiprovider` streaming
  path relies exclusively on `JustFinishedToolCall()` to surface a tool call to the agent harness —
  so with unpatched Ollama traffic, a tool call is fully assembled inside the SDK's internal state
  but silently never reaches Canopy at all (confirmed directly: replaying a live streaming response
  from a real Ollama server through the SDK's own accumulator, in the exact call pattern
  `openaiprovider` uses, showed the tool call fully present in the final accumulated state while
  `JustFinishedToolCall()` never once returned true across the whole stream). `ollama_transport.go`
  installs an `http.RoundTripper` on the Ollama client (mirroring §3's Gemini
  `functionCall.id`-patching transport precedent above) that rewrites each SSE event's JSON on the
  way back from the server, deleting the `"content"` key from any `choices[].delta` object that
  also carries a non-empty `"tool_calls"` array and an empty `"content"` value — restoring the shape
  the SDK's accumulator already expects, before it ever parses the response. Only
  `"text/event-stream"` responses are touched; non-streaming Chat Completions calls read tool calls
  directly off the parsed body and were never affected by this bug. Verified end to end against a
  real self-hosted Ollama server (0.32.13): a live tool call is now auto-invoked by the harness and
  its result correctly reaches the model's final answer, which silently failed before this fix.

## 5. TUI

The only frontend in v1 (Requirements §5). Existing aiagent TUI concepts (chat view, streaming
response rendering) port over consuming `agent.ResponseStream`. New/changed:

- Approval-prompt component: one-off vs. always-allow (§3.6).
- Live todo panel (§3.7).
- Mode indicator/switcher in the status line (§3.8).
- Agent picker sourced from `agentsource` (§3.11) instead of a database-backed agent list.
- No web UI, no `ui/` package, no WebSocket streaming — deferred (Requirements §5). When it comes
  back, it's an additive frontend consuming the same `domain/services` the TUI does; nothing in
  this design assumes TUI-only forever, just TUI-only *now*.

Addendum (post-v0.1.0): a chat's agent and model are switchable mid-session, not just fixed at
`StartChat` time. `ctrl+a` and `ctrl+o` each open an in-chat `bubbles/list` overlay
(`internal/tui/picker.go`, sharing the same item/list construction the top-level agent-picker
screen uses) sourced from the new `AgentService.ListAgents()`/`ListModels()`. Selecting an entry
calls the new `AgentService.SetAgent`/`SetModel` against the *same* chat ID and returns to the chat
screen — neither ends the chat or clears its history; only `Chat.AgentName`/`Chat.ModelOverride`
change, and `buildTopLevelAgent` already re-resolves both fresh on every turn, so the very next
message is driven by the switched agent/model with the full prior transcript intact. Both
keybindings are no-ops while a tool-approval prompt is pending or a turn is actively streaming, the
same guard `enter` (sending a message) already applies. The sidebar gained `Agent: ...` and
`Model: ...` lines next to the existing `Mode: ...` line, each with its own `(ctrl+x to switch)`
hint.

Addendum (post-v0.1.0): `ctrl+n`, pressed from the chat screen, starts a genuinely new chat —
`AgentService.StartChat` with a freshly minted chat ID (the same `<agent>-<UnixNano>` scheme the
top-level picker's own `startChatCmd` already used, now factored into a shared `newChatID` so
there is only one ID-generation scheme) bound to the *current* chat's agent (`chat.AgentName`;
`ctrl+a` already handles picking a different agent for the new chat in-place, so `ctrl+n` doesn't
force the top-level picker screen). It reuses the existing `chatStartedMsg`/`Model.Update` path a
fresh pick from the picker screen already produces — `Model.Update` discards the old `*chatModel`
entirely and constructs a brand-new one via `newChatModel`, so transcript/streaming/pending-approval
reset to zero values and todos/mode/model are seeded fresh from the new, empty chat, with no second,
hand-rolled "reset in place" code path to keep in sync with that one. Guarded the same way
`ctrl+a`/`ctrl+o` are: a no-op while a turn is actively streaming or a tool approval is pending.

Also addendum (post-v0.1.0): `ctrl+s` opens a read-only skills-browser overlay — see §3.11's own
addendum for the full three-level skill design this exposes.

## 6. Data model

Much smaller than earlier drafts, because Agent/Skill configuration moved from database entities
to files (§3.11):

```go
// domain/entities/chat.go — the one entity that still needs a real repository
type Chat struct {
    ID, AgentName string   // AgentName references an agentsource-loaded definition, not a DB row
    Messages []Message
    SessionState []byte    // serialized *agent.Session — see §3.9
    CreatedAt, UpdatedAt time.Time
}
```

`ChatRepository` (interface in `domain/interfaces`, JSON implementation only in v1 —
`repositories/json`) is the sole persistence path. Provider/model config (§4) is a separate flat
JSON file, not part of this entity or repository. There is no Agent, Tool, Skill, Provider, or
Model repository — those concepts are either files on disk (§3.11) or the small flat config (§4).

This is a significant simplification versus aiagent's seven-repository model, and it's intentional
(Requirements §4.5 "keep it simple"): fewer things to keep in sync between a database and the
files a user actually edits.

Addendum (post-v0.1.0): `Chat` gained `ModelOverride string` (FR1, §4/§5's addendum above) — empty
(every pre-existing chat) means normal resolution (the agent definition's own `model:` frontmatter,
or `AgentServiceConfig.DefaultModel`); non-empty names a `ModelConfig.Name` to use for this chat
specifically, set via `AgentService.SetModel`. `AgentService.resolveProviderModel` now takes the
override as an explicit parameter rather than reading it off `Chat` itself, so the choice of who
gets to supply one is a call-site decision, not baked into the function: `buildTopLevelAgent`
passes `chat.ModelOverride`, but `buildSubagentAgent` always passes `""`, deliberately never
inheriting the parent chat's override — consistent with §3.4's subagent context-isolation model, a
dispatched subagent has no `Chat` of its own to read one from in the first place.

## 7. Testing strategy

- Unit tests per service with `testify`.
- Tool contract tests: every built-in tool satisfies `tool.FuncTool`; approval-gated ones report
  `ApprovalRequired() == true`.
- Compaction tests: configured `Trigger`s fire at the right threshold; a `PipelineStrategy` run
  doesn't drop messages needed for task continuity.
- Session-state round-trip test: `Chat.SessionState` serialize/deserialize, asserting a standing
  approval `Rule`, in-progress todo items, and current mode all survive a simulated restart
  together.
- **File-format loader tests (new emphasis in this revision)** — `agentsource`, `mcpsource`,
  `skillsource`, `projectcontext` are Canopy's own code, the most likely place for
  compatibility bugs against Claude Code's format: fixture-based tests using real example
  `.claude/agents/*.md`, `.mcp.json`, and `SKILL.md` files (including malformed/partial frontmatter
  cases) rather than only hand-constructed Go structs.
- Provider adapter tests: table-driven, one per provider, against a local mock HTTP server.

## 8. Risks carried from Requirements

- MAF-Go public-preview API churn — pin version, track upstream changelog before upgrading.
- Go 1.25 toolchain bump required.
- File-format compatibility (§3.11) is Canopy-maintained, not guaranteed by any dependency —
  Claude Code's subagent/`.mcp.json` formats aren't formally versioned, so the loaders should be
  built to fail loud and specific (clear error on a file it can't parse) rather than silently
  ignoring a definition it doesn't understand.
- The "faster and lighter" goal (Requirements §4.9) is unverified until Plan Phase 9's
  benchmarking; nothing in this design should assume it's already true.
