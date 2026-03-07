# Baton Development Guide

This document describes how to develop Baton locally.

## Prerequisites

- Go `1.25+` (the repository currently uses `go 1.25.0` in `go.mod`)
- Git
- Optional: tracker credentials such as `LINEAR_API_KEY` if you run against Linear

## 1. Get the code

```sh
git clone https://github.com/shayne-snap/baton.git
cd baton
```

## 2. Install dependencies

```sh
go mod download
```

## 3. Configure workflow runtime

Baton now prefers `agent_runtime` to select the backend used for agent turns.

Supported values today:

- `codex`
- `opencode`

Legacy compatibility:

- The old top-level `codex:` block is still accepted, but new workflows should use `agent_runtime`.

Example (`codex` runtime):

```yaml
agent_runtime:
  kind: codex
  codex:
    command: codex app-server
```

Example (`opencode` runtime):

```yaml
agent_runtime:
  kind: opencode
  opencode:
    command: opencode serve
```

## 4. Start Baton locally

```sh
go build -o bin/baton ./cmd/baton
./bin/baton --i-understand-that-this-will-be-running-without-the-usual-guardrails WORKFLOW.md
```

Notes:

- Baton expects a workflow file path as the positional argument (defaults to `WORKFLOW.md`).
- `WORKFLOW.md` in this repo includes a complete example configuration.

## 5. Run tests

```sh
go test ./...
```

## 6. Helpful repo map

- `cmd/baton/main.go`: CLI entrypoint
- `internal/app`: application bootstrap and lifecycle
- `internal/orchestrator`: issue polling and agent orchestration loop
- `internal/workflow`: workflow parsing/loading/watching
- `internal/runtime`: agent runtime adapters (`codex` and `opencode`)
- `internal/tracker`: tracker abstraction and implementations

## 7. Typical development loop

1. Update code and/or workflow handling.
2. Run targeted tests first (package-level), then `go test ./...`.
3. Rebuild and run Baton against a local or test workflow.
4. Verify logs and state endpoints (if enabled) for orchestration behavior.
