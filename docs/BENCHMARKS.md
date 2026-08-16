# Canopy — Performance Benchmarks vs. Claude Code

Status: v1, measured 2026-08-15
Context: Requirements §4.9/§8 frame "faster and lighter than Claude Code" as a goal to validate,
not a number to promise before measuring. This is that measurement, reported as-is.

## Environment

- Single Linux x86_64 dev machine, one run session, no other significant load controlled for
  beyond normal background processes. Not a clean-room benchmark rig — treat magnitudes as
  directionally reliable, not lab-precise.
- Canopy: `dist/canopy` built via `make build VERSION=v0.1.0` (`CGO_ENABLED=0`, `-trimpath`,
  stripped).
- Claude Code: the installed native binary (`claude`, v2.1.233) — not the older Node/npm
  distribution. Comparing against whichever distribution is actually installed is the fairer,
  more current comparison; a Node-based install would likely show a larger gap on cold start.

## Binary size

| | Size |
|---|---|
| Canopy (`dist/canopy`) | 33,059,000 bytes (~31.5 MiB) |
| Claude Code (installed binary) | 324,598,064 bytes (~309.6 MiB) |

**~9.8x smaller.**

## Cold-start time

Proxy: `<binary> --version`, wall-clock via `date +%s%N` before/after, 10 runs each. This measures
process-startup overhead (binary load + runtime init), not full "ready for a real chat" startup —
neither tool's `--version` path loads project config, agents, or MCP servers, so this isn't the
whole startup story for either (see "What this doesn't measure" below).

| | Run times (ms) | Average |
|---|---|---|
| Canopy | 5.15, 5.38, 4.31, 4.17, 4.39, 3.88, 4.04, 3.77, 4.20, 3.71 | **4.30 ms** |
| Claude Code | 75.90, 74.43, 77.87, 76.12, 74.63, 75.14, 72.73, 76.71, 75.98, 73.98 | **75.35 ms** |

**~17.5x faster.**

## Idle memory footprint (RSS)

Both launched fresh in a scratch project directory via tmux, given a few seconds to settle to
their idle/ready prompt, measured with `ps -o rss`.

| | RSS |
|---|---|
| Canopy (agent picker → chat screen, idle) | 24,912 KB (~24.3 MiB) |
| Claude Code (idle prompt, bypass-permissions mode) | 408,532 KB (~399 MiB) |

**~16.4x smaller.**

Methodology note, reported transparently rather than smoothed over: the first attempt at this
measurement grabbed the wrong process twice — `pgrep -f`/`ps aux | grep` matched substrings against
other processes' full command lines (once a local MCP bridge subprocess, once this very shell
session's own command text, which happened to contain the string "dist/canopy" because it was
being typed into a `tmux send-keys` call). Both false readings were considerably lower than the
real number. The figures above are from a corrected measurement — `ps aux | grep "dist/canopy" |
grep -v grep | grep -v "bash -c"` and an explicit numeric PID confirmed via `ps -o cmd` — not the
first (wrong) ones. Left in this document as a caution against trusting a suspiciously convenient
first number, for anyone reproducing this later.

## What this doesn't measure

- **Full "ready to do real work" startup**, including loading `.claude/agents/`,
  `.claude/skills/`, `.mcp.json`, and connecting to any configured MCP servers — for Canopy this is
  `cmd/canopy/main.go`'s `run()` before the TUI launches; for Claude Code it's whatever its own
  equivalent init does. Neither `--version` path exercises this, and MCP server connection time in
  particular (subprocess spawn, protocol handshake) is not accounted for on either side.
- **Steady-state memory under real use** (a long conversation, many tool calls, compaction cycles)
  — only idle footprint right after reaching the ready prompt.
- **Response latency** — this is entirely provider/model-bound for both tools and isn't a
  meaningful point of comparison between the two harnesses themselves.
- A controlled, multi-machine, multiple-trial statistical benchmark. Ten runs on one machine is
  enough to show these aren't noise-level differences, not enough to publish a confidence interval.

## Conclusion

On every axis actually measured, Canopy is meaningfully smaller and faster to start than the
installed Claude Code binary — consistent with the architectural bet (a single static Go binary
vs. a much larger packaged runtime) rather than a coincidence of one favorable number. Requirements
§4.9's goal is validated directionally; the caveats above are real gaps for a more rigorous
benchmark later, not reasons to distrust the magnitude of the difference.
