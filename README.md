# Baton

Baton is a Go implementation of [Symphony](https://github.com/openai/symphony)’.

## What Baton does
- Polls your tracker (Linear by default) for claimable issues and reserves an isolated workspace
  per issue.
- Starts `codex app-server` from the workspace, streams a workflow prompt, and keeps the session
  alive until the issue resolves to `Done`, `Closed`, `Cancelled`, or `Duplicate`.
- Provides the `linear_graphql` helper tool when needed so skills can access Linear without
  repeated authentication.
- Cleans up workspaces once issues leave the active state and shuts down agents cleanly.

## Preparing your repository
1. Align your repository with harness engineering practices so Baton can run reliably.
2. Write a `WORKFLOW.md` that follows the tracker/workspace/hooks/agent/codex schema described in
   the SPEC and use YAML front matter for configuration.
3. Export tracker credentials such as `LINEAR_API_KEY` so hooks and tools can authenticate.
4. Copy the helper skills you need (`commit`, `push`, `pull`, `land`, `linear`) and ensure the
   `linear` skill can reach the `linear_graphql` tool Baton serves.
5. Mirror any custom tracker states (e.g., `Rework`, `Human Review`, `Merging`) in your workflow
   configuration and tracker settings so Baton’s lifecycle matches expectations.

## Configuration highlights
- Put workspace bootstrapping commands like `git clone ... .` into `hooks.after_create`.
- `codex.command` should launch `codex app-server` with your chosen sandbox policies; Baton uses
  workspace-scoped sandboxes by default and accepts strings or objects for approval policies.
- Path values support `~` and `$VAR`; Baton resolves them before spawning child processes.
- Environment-backed fields such as `tracker.api_key` can be set to `$LINEAR_API_KEY` so Baton
  reads the correct runtime value.
- If `WORKFLOW.md` is missing or invalid, Baton refuses to start so you notice configuration
  issues immediately.

## Running Baton
```sh
cd /Users/goranka/Engineer/ai/backagent/baton
go build -o bin/baton ./cmd/baton
./bin/baton --i-understand-that-this-will-be-running-without-the-usual-guardrails WORKFLOW.md
```
- Provide the workflow path as the lone positional argument; it defaults to `WORKFLOW.md`.
- Flags:
  - `--logs-root` overrides the default logs directory (current working directory).
  - `--i-understand-that-this-will-be-running-without-the-usual-guardrails` acknowledges the
    experimental nature of the agent runner and is required to start Baton.
- Baton installs signal handlers so `Ctrl+C` gracefully stops agents and closes Codex sessions.

## Observability & Testing
- Logs land under the configured logs root (default: `logs/`) and include workspace paths and
  workflow metadata for each agent invocation.
- The optional HTTP API exposes state at `/api/v1/state`, `/api/v1/<issue_identifier>`, and
  `/api/v1/refresh` for troubleshooting.
- Run `go test ./...` to cover the CLI, workflow parsing, and orchestration code; there is no single
  `make` target yet, but the Go test suite exercises the main packages involved in running Baton.
