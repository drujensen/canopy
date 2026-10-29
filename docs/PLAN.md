# Canopy — Implementation Plan

Status: v0.4 (2026-10-25; supersedes v0.3 — v0.3's Phase 0–8 are all **done** and shipped as
`v0.1.0`, so this revision records that, adds Phase 9 to capture the post-v0.1.0 shipped body that
v0.3 had no home for, and re-homes the deferred/candidate work v0.3 left dangling as loose
"sequencing notes.")
Depends on: REQUIREMENTS.md, DESIGN.md

Phased, milestone-based (no calendar dates). Each phase lists exit criteria. **Why** a decision was
made (especially removed/superseded features) lives in DESIGN.md and REQUIREMENTS.md with full
rationale — this plan states *what* shipped and *what's next*, and points at those docs rather than
restating them.

## Status as of v0.4

| Phase | Status |
|---|---|
| 0–8 | **Done** — shipped as `v0.1.0`, tagged at commit `c731abd`. |
| 9 (post-v0.1.0) | **Done** — 34 commits after the tag, latest `5742933`. |
| 10 (deferred/candidate) | **Not started, not scheduled** — real, stated-but-unscoped work, carried forward. |
| 11 (Edit tool, read-only agents, agent/model pinning) | **Done.** |
| 12 (headless mode, loop/spend guardrails, git safety net, context visibility, @-mention, slash commands, OAuth) | **Not started** — scoped below. |

Build/test/lint commands and QA workflow: see `AGENTS.md`. Benchmark numbers: see
`docs/BENCHMARKS.md`.

---

## Phase 0–8 — shipped as `v0.1.0` (historical record)

All eight phases met their exit criteria and were tagged `v0.1.0` at `c731abd`. Per-phase detail is
preserved here only at status granularity; the decision rationale is in DESIGN.md/REQUIREMENTS.md.

- **Phase 0 — Bootstrap.** Module `github.com/drujensen/canopy`, Go 1.25, `agent-framework-go`
  pinned, hello-world spike confirming the excluded dependency families (`workflow*`,
  `foundryprovider`, `a2aprovider`, `aguiprovider`, `copilotprovider`, Azure, Mongo, echo, websocket)
  stay out of `go.sum`. CI (build/vet/test).
- **Phase 1 — Storage & provider config.** `Chat` entity + JSON `ChatRepository` (with
  `SessionState []byte` from the start); flat JSON provider/model config (`~/.canopy/providers.json`
  / project-local override); `impl/config`.
- **Phase 2 — Providers.** `impl/providers/factory.go`; native OpenAI/Anthropic/Gemini;
  OpenAI-compatible adapter for DeepSeek/Ollama/Groq/Mistral/Together/xAI; table-driven mock-HTTP
  tests.
- **Phase 3 — Core tools.** bash/shell, file read/write/search, directory, web search/fetch — the
  narrow FR4 set. **Approval-gating tried here and later removed entirely (Phase 5 addendum); do not
  reintroduce — DESIGN.md §3.6.**
- **Phase 3.5 — Claude Code file-format compat.** `agentsource` / `mcpsource` / `skillsource` /
  `projectcontext` loaders + fixture tests against real example files.
- **Phase 4 — Agentic loop core.** `impl/harness` on `agent/harness/loop` + `toolautocall`;
  `HistoryProvider` over `ChatRepository`; dynamic subagent dispatch with session-isolation test;
  `domain/services` orchestration; zap logging bridge.
- **Phase 5 — Loop quality.** Compaction (FR10) and todo (FR11). **Approvals (FR5) and
  `agentmode` (FR12) were both removed/superseded post-v0.1.0** — approvals never shipped a real
  prompt (auto-approval + chat-corrupting bugs) and `agentmode` was replaced by the four SDLC-persona
  agents; see DESIGN.md §3.6/§3.8 addenda.
- **Phase 6 — TUI.** Bubble Tea chat screen, streaming render, agent picker from `agentsource`,
  live todo panel; manual QA in a real terminal.
- **Phase 6.5 — MCP tool construction** + two live bugfixes.
- **Phase 7 — Security, observability, packaging.** zap bridge finished; `web_fetch` SSRF denylist
  (the Phase-3 review's flagged open point, resolved); OTel tracing optional/off-by-default; single
  static binary (`make build`), Docker secondary.
- **Phase 8 — Beta hardening & benchmarking.** `-race` in CI; goroutine-leak proof around
  `ResponseStream`; **benchmark vs. `claude` CLI done and reported in `docs/BENCHMARKS.md`** (~9.8x
  smaller binary, ~17.5x faster cold-start, ~16.4x lower idle RSS, measured 2026-08-15); **`v0.1.0`
  tagged.**

---

## Phase 9 — post-v0.1.0 (DONE, 2026-08-15 → 2026-10-25)

Thirty-four commits after the `v0.1.0` tag, shipping the "usable as a real `claude` replacement"
layer. v0.3 had no phase for this body of work — hence the addenda-in-code-comments drift this
revision clears up. Grouped by area; every line is committed, tested, and reflected in the README.

- **Zero-config first run (FR1/FR17/FR19 no-longer-hard-error cases).**
  - Auto-create default agents (`general` + the four `research`/`design`/`plan`/`execute`
    SDLC personas) to `~/.canopy/agents`; auto-create default `mcp-server-setup` skill to
    `~/.canopy/skills` — both per-file idempotent, never overwriting user edits.
  - models.dev auto-detection: free 24h-cached catalog, detect set API-key env vars, write matching
    `ProviderConfig` as `api_key_env` (never the literal), paired with the newest
    tool-call-capable model; additive re-sync + per-model cost refresh on startup;
    `--refresh-providers` forces a live re-fetch.
  - **Any** unrecognized provider `type` with a `base_url` now routes through the OpenAI-compatible
    adapter — "support any provider," not just the original 9.

- **Mid-session switching (FR1 addendum).** `ctrl+a` agent picker + `ctrl+o` model picker, both
  switching the active chat live; `ModelOverride` persisted per-chat so a switch survives a restart;
  last-selected agent/model remembered across sessions (`last_agent.json`/`last_model.json`).

- **Skills made real (FR19 addendum).** The `Skill` tool implements progressive disclosure end to
  end (name/desc always in context → body on demand → supporting file on demand, confined to the
  skill's own directory); `ctrl+s` read-only skills browser; `ctrl+n` new-chat-without-picker.

- **Chat-history & session UX.** `ctrl+h` history browser with chat delete; `--continue` resume;
  auto-resume last agent; greeting placeholder for an empty transcript; **a cancelled turn's own
  message now persists instead of being lost**; **resumed chats show their tool calls/results**.

- **TUI polish & cross-platform.** Mouse text selection + real clipboard (system tool / `clip.exe`,
  not just OSC 52); scrolling incl. `ctrl+up`/`ctrl+down` no-fn-key binding; hideable sidebar;
  multi-line input; full terminal height; visually separated tool-call output from prose; live
  spinner + one-turn error display.

- **Provider reliability.** Additive retry/backoff: `429`, plus Groq's ITPM rate-limit that arrives
  as `413` (not `429`); 5xx; Gemini/Ollama/DeepSeek adapter bugfixes; **a user-visible notice when a
  request is throttled** rather than a silent retry.

- **Usage & cost visibility (latest committed work).** Per-chat, per-model token + USD cost
  persisted on `Chat.Usage` (survives resume); a context-propagated `usageCollector` + a locally
  re-implemented usage-tracking subagent tool (the stock `tool/agenttool` discards
  `resp.Usage()`) so subagent dispatch is billed at its own model's rate; sidebar shows running cost
  and `N tools, M skills`; **`ctrl+t` tools browser and `ctrl+u` usage/cost overlay.**

**Exit criteria (met):** all of the above committed and green under `go build`/`go vet`/
`go test ./... -race`; README updated to describe zero-config first run, provider config, the
switchers, the Skill tool, and the no-approval-gating posture.

---

## Phase 10 — deferred & candidate work (NOT started, not scheduled)

Real, stated-but-unscoped items carried forward so they aren't lost in code-comment addenda again.
Each needs its own requirements/design pass and a phase added **before** building (see the
governance note). Nothing here is assumed to be next.

- **Data migration from aiagent** (`~/.aiagent` JSON/Mongo → Canopy's schema) — REQUIREMENTS §8
  "no decision yet."
- **Web UI** — deferred, not cut (REQUIREMENTS §5); would reuse `domain/services` + `ChatRepository`
  as an additive path, not a rewrite.
- **MongoDB storage** — deferred, not cut (REQUIREMENTS §5); same repository-interface additive
  path; `--storage=file` flag already kept for forward compatibility (FR15).
- **MAF-Go re-evaluation** — it's public preview; re-check the pinned version and the
  `agent/compaction`, `agent/harness/*` surfaces before a routine bump (REQUIREMENTS §8, DESIGN.md §1).
- **Project-wide cost view** — a natural follow-on to Phase 9's per-chat `Chat.Usage`: sum the field
  across every chat in the repository. Currently unscoped.

(Deferred items are *candidates awaiting scope*, not a committed roadmap — picking one up means
writing its requirements/exit criteria into a new numbered phase first, not just starting.)

---

## Phase 11 — Edit tool, specialized read-only agents, agent/model pinning (DONE)

Scoped out of a design-review conversation (2026-10-28) critiquing the built-in agent/tool set for
gaps, not carried forward as a vague idea — this phase exists so the three items below don't go
the same "addendum in a comment, never in the plan" route Phase 9 had to retroactively fix (see the
Governance note). Starting with the Edit tool; the other two follow.

- **Targeted `Edit` tool. DONE.** `internal/impl/tools/file_write.go` only
  supports full-file overwrite (`FileWriteInput.Content` — "Overwrites any existing file at
  path"), with no Claude-Code-`Edit`-equivalent string-replace tool. Every single-line fix
  currently forces the model to retransmit the whole file, costing output tokens proportional to
  file size instead of change size, and risking silent corruption wherever the model's memory of
  untouched lines drifts from disk — worse on smaller/local models, which are far more likely to
  mangle a large verbatim retransmission than a targeted diff.
  - Exit criteria: new `tool.FuncTool` (`NewFileEditTool` or similar) taking `{path, old_string,
    new_string, replace_all}`; same path-safety confinement as `FileRead`/`FileWrite`
    (`pathsafety.go`); errors (not silent no-ops) when `old_string` isn't found, or isn't unique
    and `replace_all` is false; same added/removed line-stat output `FileWriteOutput` already
    gives the TUI; unit tests; wired into the tool-name allowlist agent frontmatter `tools:` lines
    reference (alongside `FileWrite`, not replacing it — full overwrite still has legitimate uses,
    e.g. creating a new file).

- **Three new built-in read-only agents: `explore`, `review`, `debug`. DONE.** Current built-in set
  (`general` + the four SDLC personas) has no agent that's safe to delegate to regardless of which
  model backs it, because none of them withhold write access. A read-only agent is low-risk to
  dispatch even to a weak/untrusted model, since it cannot mutate anything no matter how it
  misbehaves.
  - `explore` — `FileRead`/`FileSearch`/`DirectoryList` only. Fast, cheap, parallelizable codebase
    locator; primary value is keeping a parent agent's own context clean by returning only findings,
    not every file it had to open to get there.
  - `review` — read-only code-review / second-opinion pass, severity-rated findings, no write
    access.
  - `debug` — root-cause/regression-investigation persona (reproduce → isolate → hypothesize),
    distinct from `execute`'s build-first framing; needs `Bash` to actually reproduce/run tests,
    otherwise read-only.
  - `general.md`'s system prompt gets a short delegation paragraph (mirroring `execute.md`'s
    existing one) once these three exist, since `AgentService.buildTopLevelTools` already wraps
    every other loaded agent as a dispatch tool for `general` — nothing currently tells `general`
    to actually reach for them.
  - Exit criteria: three new `.md` defaults added to `agentsource.WriteDefaults`'s seeded set
    (`~/.canopy/agents`, per-file idempotent like the existing five); picked up automatically by the
    `ctrl+a` picker and by every other agent's subagent-dispatch wrapping; `general.md` updated;
    fixture/loader tests extended to cover the three new names.

- **Project-level agent → model pinning. DONE.** Today `resolveModelName` (`agent_service.go:1265`)
  only has two tiers below the model catalog itself — `chat.ModelOverride` (session, ctrl+o) and
  the agent definition's own `model:` frontmatter — so pinning a specific model to a specific agent
  *for one project* currently means forking that agent's entire `.md` file into
  `.claude/agents/`, freezing its prompt body at whatever it said the day it was forked. Add a
  third tier instead: an optional `agent_models` map in the existing project-local
  `.canopy/providers.json` (already gitignored/per-checkout, no new config file needed) naming a
  model per agent name, resolved between `chat.ModelOverride` and the agent's own frontmatter
  `model:`.
  - Precedence: `chat.ModelOverride` → project `agent_models[name]` → agent frontmatter `model:` →
    `defaultModel`.
  - Must fail soft: if `agent_models` names a model not present in the resolved model list (e.g. a
    teammate without the pinned local Ollama model running, if this file is ever deliberately
    checked in despite the default gitignore), log and fall through to the next tier — the same
    "one bad config doesn't break everything" posture DESIGN.md §3.4's bugfix addendum already
    established for subagent construction — not a hard error.
  - Also fixes a related gap: switching agents mid-chat via `ctrl+a` currently leaves a stale
    `chat.ModelOverride` in place, silently overriding the newly-selected agent's own pin (project
    or frontmatter) until the user manually clears it with a second `ctrl+o`. Agent switch should
    re-resolve through both pin tiers rather than carrying the old override forward unconditionally.
  - Exit criteria: `agent_models` field added to the provider-config schema (`impl/config`) and
    `resolveModelName`/`resolveProviderModel`; ctrl+a re-resolution fix; tests covering precedence
    order and the fail-soft fallback.

---

## Phase 12 — headless mode, loop/spend guardrails, git safety net, context visibility, @-mention, slash commands, OAuth (NOT started)

Scoped out of a design-review conversation (2026-10-28, same thread as Phase 11) auditing what's
still missing against "works in almost all situations," after checking each candidate against
REQUIREMENTS.md §5's existing non-goals so this doesn't re-litigate decisions already made (no web
UI, no vision/image input, no claim of parity with Claude Code's hooks system, etc. — those stay
out of scope and aren't repeated here).

- **Non-interactive/headless mode.** `cmd/canopy/main.go`'s `run()` always ends in
  `tea.NewProgram(...)` — there is no one-shot `-p "prompt"` path, no stdin-pipe input, no
  machine-readable output format. Every existing flag (`--global`, `--storage`, `--version`,
  `--otel`, `--refresh-providers`, `--continue`) assumes an interactive TUI session, so Canopy
  currently cannot be used from CI, a git hook, or a script pipeline at all — the single biggest
  gap against "almost all situations."
  - Exit criteria: a new flag (e.g. `-p`/`--print`) that runs one turn against a fresh or
    `--continue`d chat without starting the Bubble Tea program, reads the prompt from the flag's
    argument or stdin if piped, and writes the final response to stdout — plus a machine-readable
    output mode (e.g. `--output-format json`) covering at least the final response text, the model
    used, and token/cost usage, so a script can parse it without screen-scraping. Reuses
    `domain/services.AgentService` directly (`StartChat`/`RunText`), no new orchestration path.

- **Runaway-loop / spend-cap guardrail.** DESIGN.md §3.6 deliberately removed all approval-gating;
  the natural follow-up question is what stops a bad tool-call loop from running away — on tokens,
  on real API spend, or on wall-clock time. Nothing in `impl/harness` sets a max-turns/max-tool-calls
  ceiling today.
  - First step is investigation, not code: confirm whether MAF-Go's `agent/harness/loop` /
    `toolautocall` already enforces some cap internally. If it does, document what it is and where
    (DESIGN.md), and decide whether Canopy's own default is tight enough. If it doesn't, add a
    configurable per-turn ceiling (max tool calls and/or a wall-clock timeout) in `impl/harness`,
    surfaced as a clear error/stop rather than a silent infinite loop.

- **Git-aware safety net.** With no approval-gating, the implicit assumption is "git is the user's
  safety net," but nothing today makes that easy: no dirty-tree warning before a multi-file
  `FileEdit`/`FileWrite` sweep, no sidebar git-branch/dirty indicator.
  - Exit criteria: a `git status --porcelain` check (best-effort, non-fatal if `git` isn't
    installed or the project isn't a repo) surfaced as a one-line sidebar indicator (branch name +
    dirty/clean) and/or a one-time warning when a chat starts with uncommitted changes already
    present. Read-only and advisory — this is visibility, not a new blocking/approval mechanism
    (DESIGN.md §3.6's "no tool call is ever blocked on a human decision" stays intact).

- **Manual context control.** Compaction (`impl/harness/compaction.go`) is fully automatic and
  silent; `ctrl+u` shows per-model cost/token usage but not "how close am I to this model's context
  window," and there's no way to force a compact before a known-large operation.
  - Exit criteria: sidebar or `ctrl+u` shows current context usage as a fraction of
    `ModelConfig.ContextWindowTokens`, and a new manual trigger (e.g. a composer command or
    keybinding) forces `agent/compaction`'s pipeline to run immediately rather than waiting for the
    next turn's automatic check.

- **`@`-mention file-reference picker.** The composer is a plain `bubbles/textarea` with no
  special handling for `@` — a user has to remember and type the exact relative path of any file
  they want to point the model at. This is pure composer convenience, not a new capability (the
  model can already `FileRead` any path mentioned in a prompt) — deliberately scoped to *inserting
  a path*, not auto-reading file content into context on mention, which would be a separate,
  larger decision (silently injecting arbitrarily large content).
  - Reuses the overlay-picker infrastructure already backing `ctrl+a`/`ctrl+o`/`ctrl+s`
    (`internal/tui/picker.go`'s `bubbles/list`-based pattern), triggered inline from the composer
    on `@` instead of a dedicated keybinding, fuzzy-filtering project files as the user keeps
    typing, and inserting the selected relative path at the cursor on selection.
  - Exit criteria: typing `@` opens the picker; continued typing filters it; selecting an entry
    replaces `@<filter text>` with the chosen relative path; `esc` cancels without inserting
    anything. Confined to the project root the same way `FileSearch`/`DirectoryList` already are.
  - Drag-and-drop is explicitly *not* separate implementation work: in virtually every terminal, a
    dropped file is delivered as its path pasted into the focused input, which `bubbles/textarea`
    already accepts as plain text with no code change needed — verify this manually in the terminal
    you actually use rather than building anything. Actual drag-and-drop of file *content* (e.g. an
    image) is a GUI-terminal-specific feature squarely inside REQUIREMENTS.md §5's existing
    vision/image-input non-goal, and stays out of scope.

- **Slash-command extension point** (lower priority — parity gap, not a missing capability).
  Claude Code supports reusable, named, templated prompts via `.claude/commands/*.md`; Canopy has
  no equivalent loader. Low urgency because the `Skill` tool already covers most of the same
  need (a reusable, on-demand, named prompt) with the exact file-based, progressive-disclosure
  pattern Canopy already favors — worth a loader (`commandsource`, mirroring `agentsource`/
  `skillsource`) only if real usage shows skills aren't a good enough substitute in practice.

- **OAuth/subscription login** (flagged, likely staying out of scope). Canopy only supports
  `api_key`/`api_key_env` — no OAuth flow for a Claude Pro/Max subscription the way Claude Code
  has. Real limitation for a subscription user who'd rather not pay per-token, but OAuth is
  Anthropic-specific plumbing that cuts against "multi-provider, not locked to one vendor" —
  Canopy's whole differentiator (README's opening paragraph) — so this needs an explicit decision
  to take on that asymmetry before it's worth scoping, not a default yes.

---

## Governance note — why v0.4 exists, and how to keep it that way

The v0.3 → v0.4 gap happened because post-v0.1.0 work was tracked only as `post-v0.1.0 addendum`
code comments, not in this plan — so the plan described a product that no longer existed. To keep
the plan and the code from diverging again:

1. **Add a phase here before building a feature, not after.** A phase's exit criteria exist when the
   work starts, so the plan always describes the *intended* shape, not a changelog of surprises.
2. **Rationale stays in DESIGN.md/REQUIREMENTS.md.** This file states what shipped and what's next;
   the *why* (esp. removed/superseded features like approvals/`agentmode`) lives in the design doc,
   and this file links to it instead of duplicating it.
3. **The commit log is the changelog, not the plan.** v0.4's Phase 9 is a one-time reconciliation of
   the 34 post-v0.1.0 commits; going forward, per-feature history belongs in `git log`, and this
   file keeps only phase-level status.
4. **Cross-reference integrity.** A dangling pointer into this file (e.g. "Plan Phase N") is a bug in
   whichever doc owns it. v0.4's rewrite changed this file's phase numbering, so every call-site in
   the other docs was checked against it and fixed in the same pass (the `REQUIREMENTS.md §4.9/§7`
   and `DESIGN.md §4/§8` "Plan Phase N" refs now each name the phase the v0.3/v0.4 plan they point
   into actually means).
