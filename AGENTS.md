# Agentic Coding Guidelines for canopy

Build/test/lint commands and conventions for agents (and humans) working in this repo. See
`docs/REQUIREMENTS.md`, `docs/DESIGN.md`, `docs/PLAN.md` for product/architecture context — read
the relevant section before implementing a phase, don't guess at the framework API.

## Build/Test Commands

- Build: `go build ./...`
- Vet: `go vet ./...`
- Format: `gofmt -l .` (should print nothing); fix with `gofmt -w .`
- Mod tidy: `go mod tidy`
- Test all: `go test ./...`
- Test with race + coverage: `go test ./... -race -cover`
- Test one package: `go test ./internal/impl/tools -run TestBashTool`

## QA Workflow (run after any code change, before considering a task done)

1. `gofmt -w .`
2. `go vet ./...`
3. `go mod tidy` (then check `git diff go.mod go.sum` is intentional, not accidental scope creep)
4. `go build ./...`
5. `go test ./... -race`

If any step fails, fix it before moving on — don't leave a phase "done" with a red build.

## Code Style

- **Architecture**: DDD layering — `domain/entities`, `domain/interfaces`, `domain/services` for
  business logic; `impl/*` for anything touching an external system (providers, tools, file-format
  loaders, storage); `tui/` for the Bubble Tea frontend. Domain code depends on interfaces, never
  on `impl` concrete types.
- **Error handling**: `fmt.Errorf("failed to %s: %w", operation, err)`, always check and
  propagate.
- **Context**: `context.Context` as the first parameter on any method that can block or call out.
- **Naming**: `NewXxx` constructors; interfaces end in `-er` where that reads naturally
  (`Repository`, `Source`); exported struct fields PascalCase.
- **Testing**: `testify` for assertions; table-driven tests for anything with more than two cases;
  test files end in `_test.go`; fixture-based tests (real example files, not just hand-built
  structs) for the file-format loaders in `impl/agentsource`, `impl/mcpsource`,
  `impl/skillsource`.
- **Logging**: structured logging via zap (bridged to the framework's `*slog.Logger` where MAF-Go
  wants one — see `docs/DESIGN.md` §3.10). Never log API keys or full sensitive payloads.
- **Dependency hygiene**: don't add an import from `github.com/microsoft/agent-framework-go` that
  isn't already on the whitelist in `docs/DESIGN.md` §1 without updating that document first — the
  whole point of the narrow import list is to keep `workflow*`, `foundryprovider`, `a2aprovider`,
  `aguiprovider`, `copilotprovider`, and their transitive dependencies (Azure SDK, etc.) out of
  `go.sum`. Verify with `go mod tidy && git diff go.sum` after adding any new MAF-Go import.

## Project Structure

- `cmd/hello/`: Phase-0 spike proving the framework dependency works; not a real entry point.
- `internal/domain/`: entities, repository/source interfaces, orchestration services.
- `internal/impl/`: providers, tools, harness wiring, file-format loaders (`agentsource`,
  `mcpsource`, `skillsource`, `projectcontext`), JSON repositories, config.
- `internal/tui/`: Bubble Tea frontend.
- `docs/`: requirements, design, plan — the source of truth for *why*, read before changing *what*.

## Commit Practices

- Run the QA workflow before every commit.
- Commit at phase boundaries (see `docs/PLAN.md`) once that phase's exit criteria are met, not
  mid-phase with a red build.
- Commit messages: focus on why, not what; no need to restate the diff.
