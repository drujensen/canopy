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
`agent/harness/{loop,toolautocall,toolapproval,todo}` (post-v0.1.0: `agentmode` was imported here
too, before §3.8's addendum superseded it),
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
    harness/           # wires agent/harness/{loop,toolautocall,toolapproval,todo}
                        # and agent/compaction into each *agent.Agent at construction time
                        # (post-v0.1.0: agentmode was wired here too — see §3.8's addendum)
    agentsource/       # NEW — loads .claude/agents/*.md (§3.11)
    skillsource/       # NEW — loads .claude/skills/*/SKILL.md (§3.11)
    mcpsource/         # NEW — loads .mcp.json, constructs tool/mcptool clients (§3.11)
    projectcontext/    # NEW — loads CLAUDE.md / AGENTS.md into the system prompt (§3.11)
    repositories/
      json/            # ChatRepository only, in v1
    config/            # .env / flags; small flat-JSON provider/model config (§4, §6)
  tui/                 # bubbletea: chat view + approval prompts, todo panel
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

Addendum (post-v0.1.0): `ToolsConfig.WebSearchBackend` — the `WebSearchBackend` interface this
section always described — went from always `nil` (no default search backend ever wired in
`cmd/canopy`) to conditionally wired from a `TAVILY_API_KEY` environment variable, the same
zero-config env-var pattern `impl/config/detect.go` already uses for provider auto-detection. New
file `internal/impl/tools/tavily_backend.go` (`NewTavilyBackend`), a second concrete
`WebSearchBackend` alongside the existing self-hostable `NewSearXNGBackend` — `POST
https://api.tavily.com/search` with a bearer `Authorization` header, parsing `results[].{title,
url, content}` into `WebSearchResult{Title, URL, Snippet}` the same shape SearXNG's backend
produces. Chosen because the 4-persona SDLC agents (see §3.11's addendum below) all genuinely rely
on `WebSearch` working out of the box, and Tavily needs only an API key (no self-hosted instance to
stand up first) to get there. An agent whose `tools:` frontmatter names `WebSearch` still
hard-errors at every turn if `TAVILY_API_KEY` isn't set — a real, known first-run gap for a user
who adopts the SDLC agents without setting the key, not new error-handling code.

### 3.3 The agentic loop

`agent/harness/loop` and `toolautocall` provide the run loop and automatic tool-call
execution/feedback (wired in by default via the per-provider constructors unless
`DisableFuncAutoCall` is set). Canopy's `impl/harness` configures which middlewares attach to each
`*agent.Agent` at construction time; it does not reimplement any control flow. §3.5–§3.7 cover the
rest of `agent/harness` (§3.8 formerly did too, before it was superseded — see that section's
addendum).

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

**Bugfix addendum (post-v0.1.0).** `AgentService.buildTopLevelTools` (the function that assembles
def's own tools plus one `agenttool.New`-wrapped subagent tool per *other* loaded agent) used to
propagate a failure building any other agent as a hard error, aborting def's own construction
entirely — so one agent definition referencing an unavailable tool (`WebSearch` with no
`WebSearchBackend` configured, `Skill` with zero skills loaded) broke starting *every* agent,
including ones with nothing wrong with them, since every agent's construction eagerly tries to
wrap every other one. This surfaced in practice via the SDLC agents (§3.11's addendum): their
generated `tools:` allowlist originally named `Skill`, which isn't available until at least one
skill is loaded, so any zero-skills install couldn't start `general` (or anything else) at all.
Fixed by making the wrap-every-other-agent loop non-fatal: a subagent that fails to build is
logged (`AgentService`'s own `Logger`, if set) and skipped, not propagated — the same "one broken
piece doesn't stop the rest" posture Requirements §7 already establishes for a failed MCP server
connection. def's *own* tool list still fails loudly (a user directly selecting a misconfigured
agent deserves an immediate, clear error) — only the eager background-wrapping step became
resilient.

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

**Bugfix addenda (post-v0.1.0): two confirmed agent-framework-go bugs in this exact area, both
fixed at Canopy's own boundary with the framework (`domain/services.AgentService` and
`impl/harness.ChatHistoryProvider`) rather than patching the vendored framework itself.**
Both follow the same shape: `toolautocall`'s own request/response reconciliation
(`extractAndRemoveToolApprovalRequestsAndResponses`) requires every non-`InformationalOnly`
`ToolApprovalRequestContent` anywhere in a turn's message history to have a matching
`ToolApprovalResponseContent`, and errors loudly (`"ToolApprovalRequestContent found with
ToolCall.CallID(s) '...' that have no matching ToolApprovalResponseContent"`) the instant it finds
one that doesn't — by design, this validation is meant to catch a genuinely malformed request, not
framework bookkeeping gaps of the framework's own making.

1. **`withReconstructedApprovalFunctionCalls`** (`agent_service.go`) works around `toolautocall`
   never re-adding a matching `*message.FunctionCallContent` alongside the
   `*message.FunctionResultContent` it reconstructs for an approved/rejected call — confirmed by
   dumping the raw HTTP request body a real approval-then-continue turn produces (a `"tool"` role
   message with no preceding matching `"assistant"` tool-call message, malformed by both the
   OpenAI and Gemini API's own contract). Fixed by synthesizing and prepending an
   `InformationalOnly` assistant message carrying that `FunctionCallContent` before every `a.Run`
   call whose new `msgs` answer a pending approval. Also repairs a related empty-`CallID` wrinkle
   specific to pre-`gemini_transport.go`-fix persisted chats. See the function's own doc comment
   for the full empirical trail (this is the most heavily-annotated function in the codebase for a
   reason — the framework interaction here is genuinely subtle).
2. **`harness.RemoveOrphanedApprovalRequests`** (`impl/harness/approval_repair.go`) fixes a second,
   distinct bug, found from two real user reports of the same symptom: under a standing "always
   approve" rule, the *first* auto-approved tool call in a turn persists a correctly matched
   request+response pair, but every auto-approved call *after* that first one in the *same* turn
   persists only the request — its response, though the call demonstrably executed (confirmed via a
   real HTTP-call-count assertion in the regression test), never lands in `Chat.Messages` at all.
   That orphaned request then breaks *every* subsequent turn on the chat, forever, the moment
   `toolautocall`'s validation sees it again.
   
   The first fix attempt lived entirely in `domain/services.AgentService` (repairing before/after
   each outer `RunMessages`/`RunMessagesStream` call) and turned out to be *insufficient*: a second
   real report reproduced with the failing call's ID absent from the persisted chat file entirely —
   proof the failure happened *inside* a still-running `a.Run` call, never returning control to
   `AgentService` at all. Root cause: `toolapproval`'s internal auto-approval loop doesn't make one
   combined invoke() round trip for a whole turn — it makes one *complete, independent* invoke()
   round trip (its own `HistoryProvider.Invoking` reload and `Invoked` persist) *per newly-issued
   tool call*. A long enough chain of auto-approved calls within a single turn means a *later*
   internal round reloads history that a *slightly earlier* internal round (of the very same turn)
   already left orphaned, and fails right there — a point `AgentService`'s own before/after repair
   can never reach.
   
   Fixed by moving the repair into `impl/harness.ChatHistoryProvider.Invoked` itself (still exported
   as `harness.RemoveOrphanedApprovalRequests` so `AgentService` can also call it) — the one place
   that genuinely runs on *every* individual persist, including the ones nested inside
   `toolapproval`'s internal loop, mid-turn. Repairing right before each persist stops an orphan
   from ever surviving long enough for a later internal round of the *same* turn to trip over it.
   `AgentService.RunMessages`/`RunMessagesStream` still additionally call the same function
   before/after the outer turn — that's what self-heals a chat *already* left broken by an
   older binary, a case the per-persist repair alone can't reach (it only ever sees what a *live*
   run passes through it). Repairing by removal rather than fabricating a matching response is the
   safer choice in both places: the request is stale bookkeeping noise by the time it's found (the
   underlying call already ran to completion, which is how its request ever became visible in
   history in the first place — there is no real approval decision left outstanding for a human or
   a standing rule to make).

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

### 3.8 Mode switching (FR12) — superseded (post-v0.1.0)

**Superseded by the 4-persona SDLC agent workflow.** Everything below this line describes the
original plan/execute mode design as it shipped through Plan Phase 8; it has been removed from the
codebase (`agent/harness/agentmode` is no longer imported anywhere in Canopy) and replaced outright
by four ordinary agent definitions — `research`, `design`, `plan`, `execute` — switched via the
same `ctrl+a` agent picker every other agent already uses (§3.4/§5), not a separate mode
keybinding. See §3.11's addendum below for the new agents themselves, and REQUIREMENTS.md's FR12
row for the requirements-level version of this note. This section is kept, not deleted, per this
document's own addendum convention — a reader following the original Phase 5 design should be able
to see what changed and why, not just find the section gone.

**Why replace it instead of extending it.** In practice, the user's actual planning workflow is a
fixed four-stage pipeline — research/requirements, design/architecture, project planning, then
execution — not a binary plan/execute toggle. Each stage needs a genuinely different persona,
system prompt, and tool set (a requirements-gathering stage has no business running `Bash`; an
execute stage needs everything). Canopy's agent-definition system (§3.11) already gives each agent
its own system prompt and its own `tools:` allowlist — precisely the mechanism plan mode was
reinventing at the mode level instead of the agent level. Once that was recognized, the mode system
became redundant: withholding `Bash`/`FileWrite` via `isPlanModeRestricted` (a runtime toggle) and
withholding them via an agent definition's `tools:` frontmatter (a static, per-persona allowlist)
were doing the same job, and the agent-level version composes with everything else in §3.11 (the
`ctrl+a` picker, per-agent subagent dispatch, per-agent `model:` overrides) for free.

**Backward compatibility.** `agent.Session`'s state is a generic, unvalidated
`map[string]*stateValue` (§3.9) — `agentmode` was the only code that ever read or wrote its
`"agentModeState"` key. A chat persisted before this change, with that key still sitting in its
`SessionState` blob, keeps loading and running fine: the key simply rides along as harmless,
orphaned data on every future re-serialize, since nothing looks for it anymore. Proven by a
regression test (`internal/impl/harness/session_test.go`) that round-trips a session carrying that
orphaned key through `LoadSession`/`SerializeSession`.

---

Original design (superseded, kept for history): `agent/harness/agentmode` — a `ContextProvider`
tracking a named mode (defaults to plan/execute), state on `*agent.Session` (§3.9):

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

### 3.9 History and sessions — one mechanism persists §3.6/§3.7's state

`agent.HistoryProvider` backed by `ChatRepository` (JSON, v1) handles chat history (FR13).
Separately, `agent.Session` is a key/value bag scoped to one conversation, explicitly built to be
persisted — its doc comment: *"a Session can be serialized and deserialized directly with
`encoding/json`, so that it can be saved in a persistent store."* `toolapproval` and `todo` both
store their state via `session.Get`/`session.Set`, each under its own key in that one `Session` —
neither exposes a separate store hook of its own. So the integration point is a single one: after
each `Agent.Run`, `impl/harness` serializes the `*agent.Session` (`json.Marshal`) onto the Chat
record's `SessionState []byte` field (§6); before the next run, it deserializes and passes it via
`agent.WithSession(session)`. One write/read path covers approvals and todos together (FR14).
(Post-v0.1.0: this blob was also where `agentmode`'s mode state lived, under its own
`"agentModeState"` key — see §3.8's addendum on why that's superseded and why an old chat's
now-orphaned key is harmless.)

**Bugfix addendum (post-v0.1.0), found in the same investigation as §3.6's two approval bugs but
distinct from both:** a real user report — a chat resumed via `--continue` showed one typed message
duplicated up to 8 times in the rendered transcript. Root cause is the same underlying framework
behavior §3.6's `harness.RemoveOrphanedApprovalRequests` addendum documents: `toolapproval`'s
internal auto-approval loop makes one complete, independent `invoke()` round trip — including its
own `ChatHistoryProvider.Invoked` persist call — per newly-issued tool call within a *single* outer
turn, and resends the exact same `*message.Message` objects (the turn's actual new messages, e.g.
the user's own typed text) unchanged on every one of those internal rounds. `Invoked`'s existing
`SourceTypeHistoryProvider` filter only recognizes content loaded by *that same round's own*
`Invoking` call as "already history" — it has no way to know a message was already persisted by an
*earlier* round of the *same* turn, so it re-appends and re-persists it every time.

Fixed by giving `ChatHistoryProvider` per-instance state: a `persistedThisTurn
map[*message.Message]bool`, keyed by pointer identity (not content — the user typing the same words
twice in two genuinely separate turns must never be deduplicated against each other; pointer
identity can only match when the framework hands back the literal same Go object). This is safe to
scope to the instance itself specifically because one `ChatHistoryProvider` is constructed fresh per
outer turn (`impl/harness.Build`, called once per `AgentService.RunMessages`/`RunMessagesStream`
call) and reused for that turn's entire `a.Run` duration, including every internal `toolapproval`
round — exactly the lifetime the dedup needs, no more and no less.

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
calls `agentsource.WriteDefaults` to generate Canopy's default agents there so a brand-new install
has something to select in the TUI's picker instead of a hard startup error — Canopy's closest
match to Claude Code's own no-file-required first-run experience given Canopy's picker-based UX.
The directory is Canopy-specific (`~/.canopy`, not `~/.claude`) so the generated fallback never
leaks into the user's real Claude Code config. Each file is independently idempotent (an
already-existing file is left untouched, a missing one is filled in).

The trigger (`canopyAgentsDirNeedsDefaults`) is `~/.canopy/agents` itself being missing or
containing zero files — not the total agent count across all three sources. This is a deliberate
choice over the simpler "`Load` returned zero definitions from anywhere" check this addendum
originally described: gating on the *directory's* own state means a `~/.canopy/agents` emptied
back down to zero files regenerates the full default set on the next run, while a directory that
already has at least one file — Canopy's own defaults, hand-authored agents, anything — is never
touched, regardless of whether the user also has agents configured elsewhere in project/personal
`.claude/agents`.

Addendum (post-v0.1.0): `agentsource.WriteDefaults` was extended from writing one file
(`general.md`) to writing five, replacing §3.8's superseded plan/execute mode toggle with four
SDLC-persona agents, each an ordinary `agentsource.AgentDefinition` — no new mechanism, just more
`.md` files under the same `~/.canopy/agents/` tier, switchable via the same `ctrl+a` picker every
agent already uses:

| Agent      | Persona            | Reads                          | Produces               | Tools                                                              |
|------------|---------------------|---------------------------------|-------------------------|---------------------------------------------------------------------|
| `research` | Product Owner       | (user, web)                     | `docs/REQUIREMENTS.md` | `FileRead, FileWrite, FileSearch, DirectoryList, WebFetch, WebSearch` |
| `design`   | Architect / UX      | `REQUIREMENTS.md`               | `docs/DESIGN.md`       | same as `research`                                                   |
| `plan`     | Project Manager     | `REQUIREMENTS.md`, `DESIGN.md`  | `docs/PLAN.md`         | same as `research`                                                   |
| `execute`  | Developer           | `PLAN.md` (+ the other two)     | working code, tests    | *(no `tools:` line — inherits everything, including `Bash`)*         |

`research`/`design`/`plan` deliberately omit `Bash` — each is a documentation-producing stage, and
Design/Requirements/Plan documents don't need shell access to write; `execute` is the one stage
that does real implementation work, so it inherits the full tool set the way `general` always has.
All four get `WebSearch` (per the explicit design goal that each stage does real research, not just
writes from what it already knows) — see §4's Tavily addendum below for the concrete backend this
now resolves to. Each stage's system prompt tells the user which agent to switch to next
(`ctrl+a`) once its own document is judged complete; there's no automatic hand-off, matching every
other agent-to-agent transition in Canopy today (always a user action, never implicit). `general`
is unchanged and still the right choice for work that isn't part of this SDLC flow. Every loaded
agent (these four included) is automatically also a subagent-dispatch tool for every other agent
(§3.4) with no opt-out — `execute`'s own system prompt leans on this directly, telling it that it
can dispatch itself or `general` for isolated sub-tasks.

**Bugfix (same addendum):** `research`/`design`/`plan`'s `tools:` line originally also named
`Skill`, matching `coreToolNames`' full eight-entry list. That was wrong: `Skill` (like
`WebSearch`) is only constructed into `coreTools()`'s available map when at least one skill is
actually loaded (`len(s.defs.Skills) > 0` — see §3.2's Skill-tool addendum), and `buildTools`'
explicit-allowlist branch treats naming an unavailable tool as a hard error, by design (proven by
`TestAgentService_BuildTools`'s "explicit WebSearch without a configured backend is an error"
case). Because every agent eagerly builds every *other* agent as a subagent-dispatch tool at
construction time (`buildTopLevelTools`'s wrap-every-other-agent loop, above), this didn't just
break selecting `research`/`design`/`plan` directly — it broke starting *any* agent, including
`general`, on any install with zero skills configured (the common case for a fresh install with no
`.claude/skills` directory yet). Fixed by dropping `Skill` from all three `tools:` lines; none of
the three personas' system prompts ever referenced the Skill tool in the first place, so nothing
about their actual behavior changes.

**Picker order.** `ListAgents` (used by both the top-level picker and the in-chat `ctrl+a` overlay
— §5) and the top-level picker's own `NewModel` both sort agent names via a new
`agentsource.SortNames`, not plain alphabetical `sort.Strings`, so the five default agents actually
read as a pipeline instead of scattering alphabetically (`design, execute, general, plan,
research`): `general` first, then `research -> design -> plan -> execute` in that fixed order.
This is deliberately name-keyed (a small `map[string]int` of the five known names, checked at sort
time), not a config field or a required naming scheme — a project's own custom agents, or one of
these five renamed by a user, simply aren't in that map and fall back to plain alphabetical order
after the known ones, exactly like `sort.Strings` would already do for names `SortNames` has never
heard of. Renaming a default agent is an ordinary, fully supported edit: nothing panics, nothing
needs updating elsewhere, the renamed agent just stops sorting to a fixed pipeline position.

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

Addendum (post-v0.1.0): `skillsource.Load` gained a third, lowest-precedence source —
`~/.canopy/skills/*/SKILL.md` — mirroring `agentsource.Load`'s own `~/.canopy/agents` addendum
exactly, down to the same precedence rule (project beats personal beats Canopy-generated default)
and the same `skillsource.WriteDefaults` idempotency contract (an already-existing skill's
`SKILL.md` is left untouched; a missing one is written). `cmd/canopy`'s `run()` calls it under the
identical `dirMissingOrEmpty` trigger the default-agents block uses (§3.11's earlier addendum,
above the SDLC-agent table) — shared logic, not a parallel reimplementation — so deleting the
skill later regenerates it the same way deleting a default agent file does.

Canopy ships one default skill this way: `mcp-server-setup`, which helps a user find an MCP server
from a public registry/directory (the official `modelcontextprotocol/servers` repo, Smithery,
Glama, PulseMCP, or a targeted web search) and write the resulting entry into `.mcp.json` (§3.11's
`impl/mcpsource` section above) — useful with zero project-specific setup, the same reasoning that
motivates the default SDLC agents. Because level 1 (name+description) is always in every agent's
system prompt regardless of that agent's own `tools:` allowlist (`skillsCatalog`, above), every
agent — including `research`/`design`/`plan`, which don't list `Skill` in their allowlist — still
*knows* this skill exists; only agents whose tool list actually includes `Skill` (`general` and
`execute`, both via the "no `tools:` line, inherit everything" path) can invoke it to fetch the
full body and act on it, which fits: adding a new tool integration is execution/tooling work, not
something a requirements/design/planning stage needs to do itself.

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

Addendum (post-v0.1.0, model cost display): the one deliberate exception to that "never touched"
guarantee is per-model cost. `cmd/canopy`'s `updateExistingModelCosts` refreshes
`InputCostPerMillionTokens`/`OutputCostPerMillionTokens` (see §4's own cost addendum below) on
every existing `ModelConfig` — matched by `(Provider, ModelName)`, not `Name`, since a user is free
to rename a model entry's display `Name` after Canopy first wrote it — including one whose provider
was already configured and so skipped entirely by the provider-level merge above. Cost is catalog
metadata a user has no real reason to hand-edit away from whatever the provider actually charges,
unlike `BaseURL`/`APIKey`/`ModelName` itself, which stay untouched on an existing entry exactly as
before. A model with no match in the freshly-detected catalog data (a self-hosted model models.dev
has no pricing for at all, or a provider that isn't currently detectable) is left completely
untouched, cost included — this only ever refreshes toward fresher data Canopy actually has in
hand, never clears a value to zero for absence of new data.

Addendum (post-v0.1.0, model list sync): a real gap in the above, confirmed against a live catalog
fetch — an already-configured provider's model *list* never grew on `--refresh-providers`, only its
existing models' cost did. A provider hand-configured (or configured by an earlier, pre-"list every
tool-call-capable model" version of `DetectProviders`) with just one model stayed stuck at one
model forever, no matter how many more the catalog later listed for it: `mergeNewProviders` only
ever populates a provider's model list at the moment that provider itself is first detected, and an
already-present provider is skipped entirely by that function, models included. Confirmed live:
`deepseek` (1 configured vs. 4 in the catalog), `google` (1 vs. 22), and `github-copilot` (1 vs. 33)
were all silently missing the large majority of their available models — in `ctrl+o`, this looked
like three oddly-named entries (`Name` matching the provider's own name, not `DetectProviders`'
`"<provider>/<model-id>"` scheme) rather than the full list every freshly-auto-detected provider
gets.

Two new functions close this, both scoped to providers the models.dev catalog actually re-verified
*this run* — present in `detected.Providers`, meaning the env var is currently set and the catalog
has live data for it:

- `mergeNewModelsForExistingProviders` adds any catalog model for an eligible provider not already
  in `dst.Models` (matched by `(Provider, ModelName)`, the same `modelKey` `updateExistingModelCosts`
  uses).
- `removeStaleModelsForRedetectedProviders` — the "sync, not just add" half, added when explicitly
  asked whether stale models get cleaned up too — removes any existing model for an eligible
  provider whose `(Provider, ModelName)` the catalog no longer lists (deprecated, renamed, or
  dropped).

The re-detection scoping is load-bearing for removal specifically, not just a nicety: a provider
*not* re-detected this run — its env var currently unset, or it was never a catalog provider at all
(a self-hosted/manually-added provider, e.g. Ollama pointed at a private server) — is completely
untouched by either function. `detected.Models` can structurally never contain anything for a
provider the catalog has no opinion on, so without this scoping, a naive "remove what's not in
detected" would delete *every* model of a self-hosted provider on every `--refresh-providers` run —
exactly the case flagged as needing to stay untouched. Verified live: after the fix, `deepseek`/
`google`/`github-copilot` (and, it turned out, `openai`, the same gap) synced to the catalog's full
model counts, `drujensen` (a private Ollama server, 2 hand-configured models) was untouched, and a
second run was a clean no-op.

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
`Model: ...` lines, each with its own `(ctrl+x to switch)` hint. (Post-v0.1.0: a third `Mode: ...`
line with a `ctrl+p` hint lived here too, before §3.8's addendum superseded it — the sidebar now
shows only Agent/Model plus the live todo panel.)

Addendum (post-v0.1.0): `ctrl+n`, pressed from the chat screen, starts a genuinely new chat —
`AgentService.StartChat` with a freshly minted chat ID (the same `<agent>-<UnixNano>` scheme the
top-level picker's own `startChatCmd` already used, now factored into a shared `newChatID` so
there is only one ID-generation scheme) bound to the *current* chat's agent (`chat.AgentName`;
`ctrl+a` already handles picking a different agent for the new chat in-place, so `ctrl+n` doesn't
force the top-level picker screen). It reuses the existing `chatStartedMsg`/`Model.Update` path a
fresh pick from the picker screen already produces — `Model.Update` discards the old `*chatModel`
entirely and constructs a brand-new one via `newChatModel`, so transcript/streaming/pending-approval
reset to zero values and todos/model are seeded fresh from the new, empty chat, with no second,
hand-rolled "reset in place" code path to keep in sync with that one. Guarded the same way
`ctrl+a`/`ctrl+o` are: a no-op while a turn is actively streaming or a tool approval is pending.

Also addendum (post-v0.1.0): `ctrl+s` opens a read-only skills-browser overlay — see §3.11's own
addendum for the full three-level skill design this exposes.

**Bugfix addendum (post-v0.1.0).** `SetModel`'s `Chat.ModelOverride` is scoped to the one chat it
was set on, by design — but restarting Canopy without `--continue` doesn't resume that chat, it
starts a *brand-new* one (via `computeStartAgent`'s auto-resume-agent, not auto-resume-chat), so a
new chat's `ModelOverride` starts empty and resolves through `resolveModelName`'s next fallback:
the agent's own `model:` frontmatter (none of the default agents set one), then
`AgentServiceConfig.DefaultModel`. Before this fix, `DefaultModel` was always
`providersFile.Models[0].Name` — whichever model happened to sort first in `providers.json`, not
necessarily one the user ever chose — so a model switched to via `ctrl+o`, used for a while, then
abandoned by restarting, silently reverted on the very next session. Fixed the same way
`last_agent.json` already solves the equivalent problem for agents: `AgentService.SetModel` now
calls a new `AgentServiceConfig.RecordLastModel` callback (mirroring `RecordLastAgent` exactly,
including its best-effort "logged, never propagated" failure contract) on every successful switch;
`cmd/canopy`'s `run()` persists it to a new sibling file, `last_model.json`, and reads it back via
a new `computeDefaultModel(models, lastUsed)` helper (mirroring `computeStartAgent`'s exact
shape) to compute `DefaultModel`: the last-used model if it's still configured, otherwise
`models[0].Name` as before.

**Follow-up bugfix (post-v0.1.0), same area.** The fix above only helps across a process
*restart* — `AgentServiceConfig.DefaultModel` is read once at construction and never changes for
the life of the running `AgentService`, so `ctrl+n` (start a genuinely new chat, same session)
still fell back to whatever `DefaultModel` was at *startup*, ignoring a `ctrl+o` switch made
earlier in that same session. `chatModel.startNewChatCmd` already carries the current chat's
`agentName` over onto the new chat; it now also carries over the current chat's active `model` the
same way, via an explicit `SetModel` call right after `StartChat` (which, as a side effect, also
records it as the new last-used model — so a following restart picks up the same value too, not
just same-session `ctrl+n`). Genuinely new agent-facing behavior, not just an implementation
detail: `ctrl+n`'s new chat now always starts on the model the user was actually last using, the
same guarantee it already gave for agent.

Addendum (post-v0.1.0, in-turn UX): three related gaps closed together, all scoped to a single
turn's lifecycle.

- **`esc` cancels the in-flight turn.** `chatModel.startTurnCmd` derives a per-turn
  `context.WithCancel(ctx)` (not `ctx` itself) and stores the cancel func on `streamCancel`;
  `handleKey`'s new `"esc"` case just calls it (`cancelTurn`), a no-op when nothing is in flight.
  Deliberately does nothing else — the resulting `context.Canceled` error still has to travel back
  through `AgentService.RunMessagesStream`'s stream/finalize and arrive as this turn's own terminal
  `streamErrMsg` on `streamCh` before `finishStreaming` actually resets `streamActive`/`streamCh`/
  `streamCancel`, the same channel-ownership contract `stream_leak_test.go` already covers for
  cancellation. `handleStreamMsg`'s `streamErrMsg` case now special-cases `errors.Is(err,
  context.Canceled)`: a "Cancelled." system note in the transcript instead of setting `statusErr` —
  a user-initiated cancel isn't a failure worth pinning under the composer.
- **A spinner shows while a turn is in flight.** `chatModel.spinner` (`bubbles/spinner`, already a
  sub-package of the already-required `bubbles` module — no new `go.mod` entry) replaces the
  composer in `View` while `streamActive`, alongside an "esc to cancel" hint. Ticking it required
  `tea.Batch(startTurnCmd(...), spinner.Tick)` at the two real interactive entry points
  (`handleKey`'s `"enter"` case, `respondApproval`) — deliberately *not* inside `startTurnCmd`
  itself, since `stream_leak_test.go`/`mcp_stream_test.go` call `startTurnCmd` directly and feed its
  result into `drainCmd`, a hand-rolled synchronous test harness that didn't originally understand
  `tea.BatchMsg`; `drainCmd` was taught to unwrap one level of batch (run every sub-`Cmd`, keep
  draining whichever produced a real stream message, drop the rest) rather than routing every
  `startTurnCmd` caller through `tea.Batch` and risking the same silent-truncation failure mode
  elsewhere. `Model.Update` gained a `spinner.TickMsg` case that only keeps re-arming the tick while
  `streamActive` stays true.
- **A turn error only shows for one turn.** `statusErr` previously had no clearing path at all —
  once set, it stayed pinned under the composer across every subsequent turn, success or failure,
  until the whole chat was torn down. `startTurnCmd` now clears it unconditionally at the top,
  before the new turn's own result can arrive — the one point both "next message sent" and "next
  response received" are guaranteed to follow.

Addendum (post-v0.1.0, auto-resume last-used agent): starting Canopy no longer always lands on the
top-level agent picker. `cmd/canopy`'s `run()` resolves a small per-project state file,
`last_agent.json` (`impl/config.LoadLastAgent`/`SaveLastAgent`) — a sibling of `providers.json`/
`models-cache.json`/`chats/` in whatever directory `providerStore.Path()` already resolved to
(project-local `.canopy/` or global `~/.canopy/`, matching `--global`/`-g`), so this needed no new
path-resolution scheme. `computeStartAgent(agents, lastUsed)` decides what to do with it: the
last-used agent if it's still configured; otherwise `"general"` if that's configured
(`agentsource.WriteDefault`'s own default-agent name, reused here so a brand-new install — no
`last_agent.json` yet — still starts with zero friction); otherwise `""`, meaning fall through to
the picker screen exactly as before this feature existed — there's no single sensible agent to
guess among several unrelated ones. `tui.NewModel` gained a `startAgent string` parameter threaded
into a new `Model.Init()`: when non-empty, `Init` fires the same `startChatCmd` a manual picker
selection already produces, so a successful auto-resume reaches `screenChat` via the exact same
`chatStartedMsg` path (and a failure surfaces via the exact same `fatalErr` display) — no second,
parallel "start a chat" code path to keep in sync.

Recording which agent was last used is symmetric with reading it: `AgentServiceConfig` gained an
optional `RecordLastAgent func(agentName string) error` (nil for every pre-existing caller/test — a
safe no-op), called by both `StartChat` and `SetAgent` on success — the two service-level choke
points that cover all three UI-level places "which agent is active" can change (the top-level
picker, ctrl+n, ctrl+a), so `cmd/canopy` only wires one callback rather than hooking three separate
TUI call sites. Deliberately best-effort: a `RecordLastAgent` failure is logged (`AgentService`'s
own `Logger`, if set) but never propagated, since forgetting the last-used agent for next time is a
convenience regression, not a reason to fail an otherwise-successful `StartChat`/`SetAgent` call.

Addendum (post-v0.1.0, chat history browser — ctrl+h/--continue): a chat can now be resumed with
its full prior transcript, not just started fresh.

- **Listing/reading.** `AgentService.ListChatSummaries` wraps `interfaces.ChatRepository.List`
  (already existed, previously unused by AgentService's exported surface) into `[]ChatSummary`
  (ID/Title/AgentName/UpdatedAt), sorted most-recently-updated first — the one place recency
  ordering is computed, shared by ctrl+h and `--continue`. `AgentService.GetChat` exposes the full
  `*entities.Chat` (Messages included) — unlike every other accessor (GetTodos/GetModel),
  which deliberately expose one derived value each, resuming needs the raw history too.
- **Resuming.** `tui.resumeChatCmd` (stream.go) loads a chat's full state (`GetChat`/`GetTodos`/
  `GetModel`) and returns the same `chatStartedMsg` a fresh picker selection already
  produces, with a new `messages []*message.Message` field populated. `Model.Update`'s existing
  `chatStartedMsg` case reconstructs `transcript` from it (`reconstructTranscript`, chat.go) —
  only user/assistant messages with non-empty rendered text, deliberately not reconstructing
  historical tool-call/approval-prompt formatting (a resumed chat mid-tool-call isn't a supported
  scenario; the pending approval itself doesn't survive a restart either, per §3.9's session-state
  contract). One function, three callers: `Model.Init` (`--continue`), the top-level `screenHistory`
  screen, and `chatModel`'s in-chat `pickerHistory` overlay — resuming is identical regardless of
  entry point. Deliberate scope cut: resuming does *not* call `RecordLastAgent` (no natural
  "this agent is now active" hook the way `StartChat`/`SetAgent` already have one) — a restart's
  zero-flag auto-resume (the addendum above) still reflects the last agent *started or switched to*,
  not merely resumed.
- **ctrl+h, two entry points.** Unlike ctrl+a/ctrl+o/ctrl+s's overlays (`ListAgents`/
  `ListModelSummaries`/`ListSkills` — pure in-memory reads, built synchronously inside `handleKey`),
  `ListChatSummaries` is real disk I/O, so ctrl+h has to go through the normal Cmd/Msg round trip
  (`loadHistoryCmd` → `historyLoadedMsg`) like starting a turn does. `Model.Update`'s
  `historyLoadedMsg` case decides which UI it populates based on which screen requested it:
  `screenAgentPicker` → a new top-level `screenHistory` screen (mirroring `screenAgentPicker`'s own
  `list.Model` pattern, since `Model` had no overlay concept of its own before this); an active chat
  → `chatModel`'s existing overlay mechanism, a new `pickerHistory` kind. Both list items are
  `historyPickerItem` (picker.go): `Title()` returns the generated title, falling back — the
  explicitly requested behavior — to a formatted date (`historyDateFormat`) when none exists yet or
  generation failed; `Description()` always shows agent name + date regardless.
- **Title generation.** `AgentService.GenerateChatTitle` builds a minimal, tool-less `*agent.Agent`
  via `providers.New` directly (the same lightweight construction `buildSubagentAgent` already
  uses, skipping `buildTopLevelAgent`/`harness.Build`'s tool/history/compaction wiring a single
  throwaway completion doesn't need) against the chat's own resolved provider/model, prompts it with
  the first user message + first assistant reply (`buildTitlePrompt`), and persists the sanitized
  result (`sanitizeTitle` — trims quotes/whitespace, collapses embedded newlines, caps length) onto
  `Chat.Title` — deliberately not touching `UpdatedAt`, so generating a title can't skew
  recency ordering. `Model.Update`'s `streamChunkMsg`/`streamDoneMsg`/`streamErrMsg` case detects a
  `streamDoneMsg` completing a chat's *first* exchange (`transcript` going from length 1 to 2) and
  batches in `generateTitleCmd` — fire-and-forget (returns no message; nothing in the UI needs to
  react to a title landing, since the history browser reads it fresh from disk next time it opens).
  `chatModel.titleAttempted` guards against ever firing twice for the same chat — set the moment
  first-exchange completion is detected (not after generation finishes, so a slow/failed generation
  can't race a second attempt), and already `true` at construction for a resumed chat (`chatStartedMsg`
  handling), which by definition is past its first exchange. On any failure (no messages yet,
  provider error, empty/unusable response), `Title` is left untouched and the error is logged
  (`AgentService`'s own `Logger`) but never surfaced to the user — the history browser's date
  fallback already covers it.
- **`--continue`.** Resolved in `cmd/canopy` after `svc` is constructed (unlike `startAgent`, it
  needs a real `ListChatSummaries` call, not a purely in-memory decision): `summaries[0].ID` if any
  chats exist, otherwise a non-fatal stderr note and normal startup (the already-resolved
  `startAgent`, or the picker). `tui.NewModel` gained a `resumeChatID` parameter/field, taking
  priority over `startAgent` in `Init` — an explicit, one-shot `--continue` is a stronger signal than
  the passive last-used-agent default.

Addendum (post-v0.1.0, empty-chat greeting): a brand-new (or freshly `ctrl+n`'d) chat's viewport no
longer starts as dead space. `chatModel.refreshViewport` renders a purely cosmetic
`defaultGreeting` ("How can I help you today?") whenever there's nothing else to show — no
`transcript` entries and nothing currently streaming — styled distinctly (`greetingStyle`: faint,
italic) from a real transcript entry. It is never appended to `transcript` itself, so it's never
part of what a turn sends to the model and never persisted; the very next `refreshViewport` call
that has real content (the first user message, via `handleKey`'s `"enter"` case) simply omits it
rather than needing an explicit "clear the greeting" step anywhere.

Addendum (post-v0.1.0, picker filtering bug fix): `/`-filtering any `bubbles/list.Model`-backed
picker — the top-level agent picker, ctrl+o's model picker, and every other in-chat overlay — did
nothing: `FilterInput` updated and `FilterState()` correctly reported `Filtering`, but
`VisibleItems()` never actually narrowed no matter what was typed. Root cause, confirmed directly:
`bubbles/list.Model`'s filtering is asynchronous by design — typing changes `FilterInput`
immediately, but `list.Model.Update` returns a `Cmd` (producing `list.FilterMatchesMsg`) that has to
be run and fed back into that *same* `list.Model` before `VisibleItems()` reflects the query at all.
`Model.Update` was (and, since ctrl+a/ctrl+o's overlay pickers were first introduced, always had
been) an exhaustive, explicit type switch with no `default:` case — any message type not named
above, `FilterMatchesMsg` chief among them, fell through to a bare `return m, nil` and was silently
dropped. Fixed with a `default:` case routing an unrecognized message to whichever `list.Model` is
currently active: the in-chat overlay picker (`chatModel.picker`) if one is open, else
`historyList`/`agentList` depending on `m.screen`. Not scoped to ctrl+o specifically — the root
cause was in `Model.Update` itself, so it affected every filterable list equally; the fix covers all
of them the same way.

Addendum (post-v0.1.0, provider retry/backoff): `impl/providers.maxProviderRetries`
(`openaicompat.go`) makes retry-on-429 an explicit, uniform choice across all four provider
families rather than silently inheriting whatever each vendored SDK defaults to. Confirmed directly
against each SDK's own source: `openai-go`/`anthropic-sdk-go` both default to only 2 retries
(`internal/requestconfig/requestconfig.go`'s `MaxRetries: 2`, retrying 408/409/429/5xx with
exponential backoff+jitter, honoring a `Retry-After` header when the server sends one) —
`openaiRetryOption()`/`anthropicoption.WithMaxRetries(maxProviderRetries)` bump both to 5 (applied
at every construction site: `newOpenAINative`, `newAnthropic`, `newOpenAICompatible`, `newOllama`).

**Bugfix (post-v0.1.0), same addendum:** this originally claimed `google.golang.org/genai`
(Gemini) "already defaults to 5 retry attempts" and left it unconfigured on that basis. That was
wrong — a misreading of the SDK source that went unnoticed until a real Gemini 429 ("quota
exceeded") propagated as an immediate hard error with zero retries. genai's `retryHTTPRequest`
(`common.go`) opens with `if opts == nil { return do(req) }`: a nil `*HTTPRetryOptions` means
exactly one attempt, and `ClientConfig.HTTPOptions.RetryOptions` is nil unless a caller sets it —
retries are opt-in, matching the Python/JS SDKs' documented behavior, not on by default.
`defaultRetryAttempts = 5`/`defaultRetryHTTPStatusCodes` (which does include 429) are only the
*values* substituted for an unset field once `HTTPRetryOptions` is non-nil, not a default-on
retry policy. Fixed in `factory.go`'s `newGemini`: it now sets `HTTPOptions.RetryOptions` explicitly
(`Attempts: maxProviderRetries`, everything else left at genai's own defaults), so all four
provider families get the same retry budget for the same class of error, instead of Gemini
silently getting zero retries while believing it already matched the others.

**Caveat also worth documenting**, since it's easy to conflate the two: a "quota exceeded" 429 (a
hard daily/monthly cap genuinely exhausted) is not the same failure as a transient rate-limit 429
(too many requests this minute). Retrying helps the second case; for the first, every retry fails
identically until the quota window itself resets, so the user still sees a 429 in the end — just
after the retry budget's cumulative backoff instead of immediately. This fix closes the "we never
even tried" gap; it cannot manufacture quota that genuinely isn't there.

Addendum (post-v0.1.0, bugfix: transcript word-wrap). `chat.go`'s `renderEntry` used to concatenate
a transcript entry's label and raw text straight into the viewport with no width constraint at
all — `refreshViewport` fed the result directly to `bubbles/viewport.SetContent`, which does not
wrap content on its own. Any line longer than the terminal's width (a long assistant reply, a
pasted user message, a skill's full body shown via `ctrl+s`) ran off-screen unreadably instead of
wrapping. `chat.go` already had a *different* line-width fix for `statusErr`
(`renderStatusErrLine`, clamping the one-line status row to fit rather than letting it overflow) —
this addendum closes the equivalent gap for the transcript body itself, which that earlier fix
never covered. `renderEntry`'s body text is now wrapped through
`lipgloss.NewStyle().Width(width).Render(text)`, the same wrap-not-clip idiom, but applied at the
paragraph level instead of a single clamped line, so multi-line replies keep their real line
breaks while any individual too-long line still wraps at word boundaries. `refreshViewport` passes
`c.viewport.Width` (the exact content-column width `resize` already computes, reserving
`sidebarWidth`) as that width, so wrapped text always fits the actual rendered column regardless of
terminal size.

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
  approval `Rule` and in-progress todo items survive a simulated restart together. (Post-v0.1.0:
  also asserts an old chat's orphaned `agentModeState` key from before §3.8's addendum round-trips
  harmlessly, not just that current state survives.)
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
