# Development Guide

This guide covers local development for Baton itself (not just running Baton in a repo).

## Prerequisites

- Go `1.25.0` (matches `go.mod`)
- Git

## Clone and bootstrap

```sh
git clone https://github.com/shayne-snap/baton
cd baton
go mod download
```

## Daily development loop

1. Make code changes.
2. Run tests:
   ```sh
   go test ./...
   ```
3. Build the binary:
   ```sh
   go build -o bin/baton ./cmd/baton
   ```
4. Run Baton with a workflow file:
   ```sh
   ./bin/baton --i-understand-that-this-will-be-running-without-the-usual-guardrails WORKFLOW.md
   ```

## Workflow runtime configuration

Baton prefers `agent_runtime` to pick the backend runtime.

```yaml
agent_runtime:
  kind: codex
  codex:
    command: codex app-server
```

Supported values for `agent_runtime.kind`:

- `codex`
- `opencode`

The legacy top-level `codex:` block is still accepted for backward compatibility.

## Helpful commands

- Run all tests:
  ```sh
  go test ./...
  ```
- Run only one package:
  ```sh
  go test ./internal/orchestrator -run TestName
  ```
- Check CLI usage:
  ```sh
  go run ./cmd/baton --help
  ```
- Validate `WORKFLOW.md` behavior quickly:
  ```sh
  go run ./cmd/baton --i-understand-that-this-will-be-running-without-the-usual-guardrails WORKFLOW.md
  ```

## Project layout

- `cmd/baton`: CLI entrypoint
- `internal/app`: application wiring and runtime startup
- `internal/orchestrator`: issue polling and agent lifecycle orchestration
- `internal/workflow`: `WORKFLOW.md` parsing/loading/watching
- `internal/runtime`: runtime adapters (`codex`, `opencode`)
- `internal/tracker`: tracker abstraction and implementations

## Debugging and observability

- Default logs are written under `logs/` in the current directory.
- You can expose the optional observability API with `--port <port>`.
- API endpoints:
  - `/api/v1/state`
  - `/api/v1/<issue_identifier>`
  - `/api/v1/refresh`
