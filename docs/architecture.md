# Architecture

## Overview

Baton is a Go-based orchestration service designed to manage and execute AI-powered coding agents (such as those built on OpenAI's Codex or similar platforms). It polls a tracker (e.g., Linear) for issues in active states, reserves isolated workspaces for each issue, starts an agent runtime (e.g., codex app-server or opencode), and streams a workflow prompt to guide the agent's work. The service ensures agents run autonomously, handles retries on failures, and cleans up resources when issues reach terminal states.

Baton aims to automate software engineering tasks by integrating issue tracking, workspace management, agent execution, and observability into a cohesive system.

## High-Level Architecture

Baton operates as a long-running service that monitors a tracker for actionable issues. Upon detecting eligible issues, it:

1. Reserves a workspace per issue.
2. Launches an agent runtime with a predefined workflow.
3. Monitors agent progress and handles state transitions.
4. Cleans up workspaces upon issue resolution.

The system is event-driven, with a central orchestrator coordinating polling, dispatching, and lifecycle management.

### Key Principles

- **Isolation**: Each issue gets its own workspace to prevent interference.
- **Autonomy**: Agents run end-to-end without human intervention until completion or failure.
- **Observability**: Provides HTTP APIs and logging for monitoring agent states and performance.
- **Configurability**: Uses a YAML-based workflow file (`WORKFLOW.md`) for all settings.
- **Resilience**: Implements retry logic, backoff, and graceful shutdown.

## Components

Baton is structured into several Go packages under `internal/`. Below is a breakdown of the core components.

### Application (`internal/app`)

The main entry point that initializes and runs the service. It:

- Parses the workflow configuration.
- Sets up watchers for config changes.
- Launches the orchestrator and optional observability server.
- Handles signals for graceful shutdown.

The `Application` struct coordinates the lifecycle, ensuring all components start and stop cleanly.

### Orchestrator (`internal/orchestrator`)

The heart of Baton. It runs a select loop that:

- Polls the tracker at configurable intervals for candidate issues.
- Dispatches issues to agents if slots are available.
- Monitors running agents, handles updates (e.g., token usage, events), and retries on failures.
- Reconciles issue states and cleans up workspaces for terminal issues.

Key features:

- **Polling Cycle**: Fetches issues, sorts by priority, and dispatches up to the concurrency limit.
- **State Management**: Tracks running, claimed, and retrying issues.
- **Event Handling**: Processes agent updates, completions, and stalls.
- **Retry Logic**: Exponential backoff for failures, continuation checks for active issues.

### Agent Runner (`internal/agent`)

Manages the execution of individual agents. For each dispatched issue:

- Prepares the workspace (via workspace manager).
- Launches the agent runtime (e.g., `codex app-server` or `opencode serve`).
- Streams the workflow prompt to the agent.
- Monitors for completion, errors, or stalls.

The runner abstracts the agent backend, allowing different runtimes (Codex, OpenCode).

### Workspace Manager (`internal/workspace`)

Handles workspace creation, bootstrapping, and cleanup. Workspaces are isolated directories under a root path.

- **Creation**: Clones repositories, runs hooks (e.g., `git clone`, `go mod download`).
- **Cleanup**: Removes directories when issues terminate.
- **Isolation**: Ensures agents don't interfere with each other or the host.

### Tracker Client (`internal/tracker`)

Interfaces with the issue tracker (e.g., Linear via GraphQL). Provides:

- Fetching candidate issues.
- Retrieving issue states.
- Abstracts tracker-specific logic behind a common interface.

Supports pluggable trackers (currently Linear, with memory for testing).

### Configuration (`internal/config`)

Parses and validates the `WORKFLOW.md` file. Defines structs for:

- Tracker settings (API keys, states).
- Agent runtime (command, permissions).
- Workspace hooks.
- Observability options.

Reloads config on file changes without restarting.

### Observability (`internal/observability`)

Provides an optional HTTP API for monitoring:

- `/api/v1/state`: Current orchestrator state (running agents, totals).
- `/api/v1/<issue_id>`: Details for a specific issue.
- `/api/v1/refresh`: Triggers a poll cycle.

Also includes a status dashboard for real-time UI.

### Runtime (`internal/runtime`)

Handles communication with agent runtimes. Updates include token usage, events, and session info.

### Other Packages

- **Logging**: Centralized logging with structured output.
- **Prompt Builder**: Constructs prompts from workflow templates.
- **Specs Check**: Validates repository specs (e.g., hooks).
- **Status Dashboard**: Real-time UI for agent states.
- **Template Engine**: Renders workflow prompts.
- **Workflow**: Loads and watches the `WORKFLOW.md` file.
- **CLI**: Command-line interface, including subcommands for checks.

## Data Flow

1. **Initialization**: Application loads config, starts orchestrator and watchers.
2. **Polling**: Orchestrator fetches issues from tracker client.
3. **Dispatch**: For eligible issues, orchestrator claims them, workspace manager prepares isolation, agent runner launches the agent.
4. **Execution**: Agent runtime processes the issue, sending updates (events, tokens) back to orchestrator.
5. **Monitoring**: Orchestrator tracks progress, handles retries or stalls.
6. **Completion**: On success/error, orchestrator cleans up workspace, updates state.
7. **Cleanup**: Terminal issues trigger workspace removal.

## Workflow Lifecycle

Issues transition through states defined in `WORKFLOW.md`:

- **Active States** (e.g., "Todo", "In Progress"): Eligible for dispatch.
- **Terminal States** (e.g., "Done", "Closed"): Trigger cleanup.

Agents run until the issue resolves or fails. Baton enforces concurrency limits and priority sorting.

## Configuration

Configured via `WORKFLOW.md`, a YAML file with front matter:

- `tracker`: Kind, project, states, polling interval.
- `workspace`: Root path, creation/cleanup hooks.
- `agent`: Concurrency, runtime (codex/opencode).
- `agent_runtime`: Specific runtime settings (command, permissions).

Example:

```yaml
tracker:
  kind: linear
  project_slug: example
  active_states: ["Todo", "In Progress"]
polling:
  interval_ms: 5000
workspace:
  root: ~/workspaces
  hooks:
    after_create: git clone ... .
agent_runtime:
  kind: opencode
  opencode:
    command: opencode serve
```

## Observability and Monitoring

- **Logs**: Structured logs with issue IDs, sessions.
- **HTTP API**: JSON endpoints for state inspection.
- **Dashboard**: HTML UI for real-time monitoring.
- **Metrics**: Token usage, runtime seconds, error rates.

Baton provides comprehensive telemetry for debugging and optimization.