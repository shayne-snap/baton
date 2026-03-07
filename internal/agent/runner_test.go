package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"baton/internal/codex"
	"baton/internal/config"
	"baton/internal/tracker"
	"baton/internal/workflow"
	"baton/internal/workspace"
)

type stubTracker struct {
	fetchByIDs func(ctx context.Context, ids []string) ([]tracker.Issue, error)
}

func (s *stubTracker) FetchCandidateIssues(ctx context.Context) ([]tracker.Issue, error) {
	return []tracker.Issue{}, nil
}

func (s *stubTracker) FetchIssuesByStates(ctx context.Context, states []string) ([]tracker.Issue, error) {
	return []tracker.Issue{}, nil
}

func (s *stubTracker) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]tracker.Issue, error) {
	if s.fetchByIDs != nil {
		return s.fetchByIDs(ctx, ids)
	}
	return []tracker.Issue{}, nil
}

func (s *stubTracker) CreateComment(context.Context, string, string) error {
	return nil
}

func (s *stubTracker) UpdateIssueState(context.Context, string, string) error {
	return nil
}

func (s *stubTracker) AddLink(context.Context, string, string, string) error {
	return nil
}

func TestAgentRunnerKeepsWorkspaceAfterSuccessfulCodexRun(t *testing.T) {
	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	codexBinary := filepath.Join(testRoot, "fake-codex")

	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		t.Fatalf("mkdir workspace root: %v", err)
	}

	writeScript(t, codexBinary, `
count=0
while IFS= read -r line; do
  count=$((count + 1))
  case "$count" in
    1) printf '%s\n' '{"id":1,"result":{}}' ;;
    2) ;;
    3) printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-1"}}}' ;;
    4)
      printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1"}}}'
      printf '%s\n' '{"method":"turn/completed"}'
      exit 0
      ;;
  esac
done
`)

	cfg := mustRunnerConfig(t, workspaceRoot, codexBinary+" app-server", 20, "Ticket {{ issue.identifier }}")
	ws := workspace.NewManager(cfg)
	tr := &stubTracker{}
	r := NewRunner(cfg, ws, tr)

	issue := tracker.Issue{
		Identifier:  "S-99",
		Title:       "Smoke test",
		Description: "Run and keep workspace",
		State:       "In Progress",
		Labels:      []string{"backend"},
	}

	beforeEntries, _ := os.ReadDir(workspaceRoot)
	before := map[string]struct{}{}
	for _, e := range beforeEntries {
		before[e.Name()] = struct{}{}
	}

	if err := r.Run(context.Background(), issue, RunOptions{
		IssueStateFetcher: func(ctx context.Context, ids []string) ([]tracker.Issue, error) {
			return []tracker.Issue{{Identifier: "S-99", State: "Done"}}, nil
		},
	}); err != nil {
		t.Fatalf("agent run failed: %v", err)
	}

	afterEntries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		t.Fatalf("read workspace root: %v", err)
	}

	createdName := ""
	for _, e := range afterEntries {
		if _, ok := before[e.Name()]; !ok && e.Name() == "S-99" {
			createdName = e.Name()
			break
		}
	}
	if createdName == "" {
		// if it already existed due to retries during local runs, still allow.
		createdName = "S-99"
	}

	workspacePath := filepath.Join(workspaceRoot, createdName)
	if _, err := os.Stat(workspacePath); err != nil {
		t.Fatalf("workspace should exist: %v", err)
	}
}

func TestAgentRunnerForwardsTimestampedCodexUpdates(t *testing.T) {
	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	codexBinary := filepath.Join(testRoot, "fake-codex")

	if err := os.MkdirAll(filepath.Join(workspaceRoot, "MT-99"), 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	writeScript(t, codexBinary, `
count=0
while IFS= read -r line; do
  count=$((count + 1))
  case "$count" in
    1) printf '%s\n' '{"id":1,"result":{}}' ;;
    2) ;;
    3) printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-live"}}}' ;;
    4)
      printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-live"}}}'
      printf '%s\n' '{"method":"turn/completed"}'
      ;;
  esac
done
`)

	cfg := mustRunnerConfig(t, workspaceRoot, codexBinary+" app-server", 20, "Ticket {{ issue.identifier }}")
	ws := workspace.NewManager(cfg)
	tr := &stubTracker{}
	r := NewRunner(cfg, ws, tr)

	issue := tracker.Issue{
		ID:         "issue-live-updates",
		Identifier: "MT-99",
		Title:      "Smoke test",
		State:      "In Progress",
	}

	var updates []codex.Update
	err := r.Run(context.Background(), issue, RunOptions{
		OnCodexUpdate: func(issueID string, update codex.Update) {
			if issueID == issue.ID {
				updates = append(updates, update)
			}
		},
		IssueStateFetcher: func(ctx context.Context, ids []string) ([]tracker.Issue, error) {
			return []tracker.Issue{{ID: issue.ID, Identifier: issue.Identifier, State: "Done"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("agent run failed: %v", err)
	}

	found := false
	for _, u := range updates {
		if u.Event == "session_started" {
			found = true
			if u.Timestamp.IsZero() {
				t.Fatal("expected timestamped update")
			}
			payload, _ := u.Payload.(map[string]any)
			if payload["session_id"] != "thread-live-turn-live" {
				t.Fatalf("unexpected session id: %#v", payload["session_id"])
			}
		}
	}
	if !found {
		t.Fatalf("expected session_started update, got %#v", updates)
	}
}

func TestAgentRunnerContinuationAndMaxTurns(t *testing.T) {
	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	codexBinary := filepath.Join(testRoot, "fake-codex")
	traceFile := filepath.Join(testRoot, "codex.trace")
	t.Setenv("SYMP_TEST_CODEX_TRACE", traceFile)

	if err := os.MkdirAll(filepath.Join(workspaceRoot, "MT-247"), 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	writeScript(t, codexBinary, `
trace_file="${SYMP_TEST_CODEX_TRACE:-/tmp/codex.trace}"
run_id="$(date +%s%N)-$$"
printf 'RUN:%s\n' "$run_id" >> "$trace_file"
count=0
while IFS= read -r line; do
  count=$((count + 1))
  printf 'JSON:%s\n' "$line" >> "$trace_file"
  case "$count" in
    1) printf '%s\n' '{"id":1,"result":{}}' ;;
    2) ;;
    3) printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-cont"}}}' ;;
    4)
      printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-cont-1"}}}'
      printf '%s\n' '{"method":"turn/completed"}'
      ;;
    5)
      printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-cont-2"}}}'
      printf '%s\n' '{"method":"turn/completed"}'
      ;;
  esac
done
`)

	cfg := mustRunnerConfig(t, workspaceRoot, codexBinary+" app-server", 3, "You are an agent for this repository. Ticket {{ issue.identifier }}")
	ws := workspace.NewManager(cfg)
	tr := &stubTracker{}
	r := NewRunner(cfg, ws, tr)

	fetchCount := 0
	issue := tracker.Issue{
		ID:         "issue-continue",
		Identifier: "MT-247",
		Title:      "Continue until done",
		State:      "In Progress",
	}

	err := r.Run(context.Background(), issue, RunOptions{
		IssueStateFetcher: func(ctx context.Context, ids []string) ([]tracker.Issue, error) {
			fetchCount++
			state := "Done"
			if fetchCount == 1 {
				state = "In Progress"
			}
			return []tracker.Issue{{ID: issue.ID, Identifier: issue.Identifier, Title: issue.Title, State: state}}, nil
		},
	})
	if err != nil {
		t.Fatalf("agent run failed: %v", err)
	}
	if fetchCount != 2 {
		t.Fatalf("expected two issue-state fetches, got %d", fetchCount)
	}

	lines := splitTraceLines(t, traceFile)
	if countLinesPrefix(lines, "RUN:") != 1 {
		t.Fatalf("expected one app-server run, trace=%v", lines)
	}
	if countJSONMethod(lines, "thread/start") != 1 {
		t.Fatalf("expected one thread/start, trace=%v", lines)
	}
	turnTexts := extractTurnInputTexts(lines)
	if len(turnTexts) != 2 {
		t.Fatalf("expected two turn/start payloads, got %d (%v)", len(turnTexts), turnTexts)
	}
	if !strings.Contains(turnTexts[0], "You are an agent for this repository.") {
		t.Fatalf("first turn should contain workflow prompt, got: %q", turnTexts[0])
	}
	if strings.Contains(turnTexts[1], "You are an agent for this repository.") {
		t.Fatalf("continuation turn should not resend initial prompt, got: %q", turnTexts[1])
	}
	if !strings.Contains(turnTexts[1], "Continuation guidance:") {
		t.Fatalf("continuation guidance missing, got: %q", turnTexts[1])
	}
	if !strings.Contains(turnTexts[1], "continuation turn #2 of 3") {
		t.Fatalf("expected continuation turn count in guidance, got: %q", turnTexts[1])
	}
}

func TestAgentRunnerStopsWhenMaxTurnsReached(t *testing.T) {
	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	codexBinary := filepath.Join(testRoot, "fake-codex")
	traceFile := filepath.Join(testRoot, "codex.trace")
	t.Setenv("SYMP_TEST_CODEX_TRACE", traceFile)

	if err := os.MkdirAll(filepath.Join(workspaceRoot, "MT-248"), 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	writeScript(t, codexBinary, `
trace_file="${SYMP_TEST_CODEX_TRACE:-/tmp/codex.trace}"
printf 'RUN\n' >> "$trace_file"
count=0
while IFS= read -r line; do
  count=$((count + 1))
  printf 'JSON:%s\n' "$line" >> "$trace_file"
  case "$count" in
    1) printf '%s\n' '{"id":1,"result":{}}' ;;
    2) ;;
    3) printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-max"}}}' ;;
    4)
      printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-max-1"}}}'
      printf '%s\n' '{"method":"turn/completed"}'
      ;;
    5)
      printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-max-2"}}}'
      printf '%s\n' '{"method":"turn/completed"}'
      ;;
  esac
done
`)

	cfg := mustRunnerConfig(t, workspaceRoot, codexBinary+" app-server", 2, "Ticket {{ issue.identifier }}")
	ws := workspace.NewManager(cfg)
	tr := &stubTracker{}
	r := NewRunner(cfg, ws, tr)

	issue := tracker.Issue{
		ID:         "issue-max-turns",
		Identifier: "MT-248",
		Title:      "Stop at max turns",
		State:      "In Progress",
	}

	err := r.Run(context.Background(), issue, RunOptions{
		IssueStateFetcher: func(ctx context.Context, ids []string) ([]tracker.Issue, error) {
			return []tracker.Issue{{ID: issue.ID, Identifier: issue.Identifier, State: "In Progress"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("agent run failed: %v", err)
	}

	lines := splitTraceLines(t, traceFile)
	if countLinesPrefix(lines, "RUN") != 1 {
		t.Fatalf("expected single run, trace=%v", lines)
	}
	if countJSONMethod(lines, "turn/start") != 2 {
		t.Fatalf("expected exactly 2 turns for max_turns=2, trace=%v", lines)
	}
}

func mustRunnerConfig(t *testing.T, workspaceRoot string, codexCommand string, maxTurns int, prompt string) *config.Config {
	t.Helper()
	cfg, err := config.FromWorkflow(
		filepath.Join(t.TempDir(), "WORKFLOW.md"),
		&workflow.Definition{
			Config: map[string]any{
				"tracker": map[string]any{
					"kind": "memory",
				},
				"workspace": map[string]any{
					"root": workspaceRoot,
				},
				"codex": map[string]any{
					"command":         codexCommand,
					"read_timeout_ms": 20_000,
				},
				"agent": map[string]any{
					"max_turns": maxTurns,
				},
			},
			PromptTemplate: prompt,
		},
	)
	if err != nil {
		t.Fatalf("FromWorkflow failed: %v", err)
	}
	return cfg
}

func writeScript(t *testing.T, path string, body string) {
	t.Helper()
	content := "#!/bin/sh\n" + strings.TrimSpace(body) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
}

func splitTraceLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func countLinesPrefix(lines []string, prefix string) int {
	count := 0
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			count++
		}
	}
	return count
}

func countJSONMethod(lines []string, method string) int {
	count := 0
	for _, line := range lines {
		if !strings.HasPrefix(line, "JSON:") {
			continue
		}
		payload := strings.TrimPrefix(line, "JSON:")
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			continue
		}
		if decoded["method"] == method {
			count++
		}
	}
	return count
}

func extractTurnInputTexts(lines []string) []string {
	out := []string{}
	for _, line := range lines {
		if !strings.HasPrefix(line, "JSON:") {
			continue
		}
		payload := strings.TrimPrefix(line, "JSON:")
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			continue
		}
		if decoded["method"] != "turn/start" {
			continue
		}
		params, _ := decoded["params"].(map[string]any)
		input, _ := params["input"].([]any)
		parts := make([]string, 0, len(input))
		for _, item := range input {
			row, _ := item.(map[string]any)
			text, _ := row["text"].(string)
			parts = append(parts, text)
		}
		out = append(out, strings.Join(parts, "\n"))
	}
	return out
}
