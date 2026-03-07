package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"baton/internal/config"
	"baton/internal/runtime"
	"baton/internal/tracker"
	"baton/internal/workflow"
)

func TestRuntimeRunTurnUsesClaudeSessionLifecycle(t *testing.T) {
	traceDir := t.TempDir()
	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "CC-1")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	command := writeFakeClaudeScript(t, traceDir)
	cfg := mustClaudeLinearConfig(t, workspaceRoot, command)
	client := New(cfg)

	sess, err := client.StartSession(workspace)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer client.StopSession(sess)

	runtimeSession := sess.(*session)
	var updates []runtime.Update
	result, err := client.RunTurn(sess, "first prompt", tracker.Issue{ID: "issue-1", Identifier: "CC-1"}, runtime.RunTurnOptions{
		Context: context.Background(),
		OnMessage: func(update runtime.Update) {
			updates = append(updates, update)
		},
	})
	if err != nil {
		t.Fatalf("run first turn: %v", err)
	}

	if result.SessionID != runtimeSession.sessionID+"-turn-1" {
		t.Fatalf("unexpected session id: %q", result.SessionID)
	}
	if result.ThreadID != runtimeSession.sessionID {
		t.Fatalf("unexpected thread id: %q", result.ThreadID)
	}
	if result.TurnID != "turn-1" {
		t.Fatalf("unexpected turn id: %q", result.TurnID)
	}

	firstArgs := readLines(t, filepath.Join(traceDir, "args-1.txt"))
	assertArgsContain(t, firstArgs, "--verbose")
	assertArgsContain(t, firstArgs, "--session-id", runtimeSession.sessionID)
	assertArgsContain(t, firstArgs, "--mcp-config")
	assertArgsContain(t, firstArgs, "--strict-mcp-config")
	assertArgsContain(t, firstArgs, "--permission-mode", "dontAsk")
	assertArgsContain(t, firstArgs, "--allowedTools", "Bash")
	assertArgsContain(t, firstArgs, "--allowedTools", "mcp__baton-tracker__tracker_get_issue")
	assertArgsContain(t, firstArgs, "--allowedTools", "mcp__baton-tracker__tracker_update_state")
	if strings.TrimSpace(readFile(t, filepath.Join(traceDir, "stdin-1.txt"))) != "first prompt" {
		t.Fatalf("expected prompt on stdin, got %q", readFile(t, filepath.Join(traceDir, "stdin-1.txt")))
	}

	assertUpdateEvent(t, updates, "session_started")
	assertUpdateEvent(t, updates, "codex/event/agent_message_content_delta")
	assertUpdateEvent(t, updates, "codex/event/token_count")
	assertUpdateEvent(t, updates, "turn_completed")

	result, err = client.RunTurn(sess, "second prompt", tracker.Issue{Identifier: "CC-1"}, runtime.RunTurnOptions{})
	if err != nil {
		t.Fatalf("run second turn: %v", err)
	}
	if result.TurnID != "turn-2" {
		t.Fatalf("unexpected second turn id: %q", result.TurnID)
	}

	secondArgs := readLines(t, filepath.Join(traceDir, "args-2.txt"))
	assertArgsContain(t, secondArgs, "--resume", runtimeSession.sessionID)
	assertArgsNotContain(t, secondArgs, "--session-id")
}

func TestRuntimeStartSessionConfiguresLinearTrackerMCPServer(t *testing.T) {
	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "CC-2")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	cfg := mustClaudeLinearConfig(t, workspaceRoot, writeFakeClaudeScript(t, t.TempDir()))
	client := New(cfg)

	sess, err := client.StartSession(workspace)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer client.StopSession(sess)

	runtimeSession := sess.(*session)
	if strings.TrimSpace(runtimeSession.mcpConfigPath) == "" {
		t.Fatal("expected mcp config path")
	}

	var payload struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(readFile(t, runtimeSession.mcpConfigPath)), &payload); err != nil {
		t.Fatalf("unmarshal mcp config: %v", err)
	}

	server, ok := payload.MCPServers[trackerMCPServerName]
	if !ok {
		t.Fatalf("missing tracker mcp server: %#v", payload.MCPServers)
	}
	if server.Type != "stdio" {
		t.Fatalf("unexpected server type: %q", server.Type)
	}
	if len(server.Args) != 1 || server.Args[0] != "mcp-tracker-server" {
		t.Fatalf("unexpected server args: %#v", server.Args)
	}
	if got := server.Env["BATON_TRACKER_KIND"]; got != "linear" {
		t.Fatalf("unexpected tracker kind env: %q", got)
	}
	if got := server.Env["BATON_LINEAR_API_KEY"]; got != "linear-token" {
		t.Fatalf("unexpected api key env: %q", got)
	}
	if got := server.Env["BATON_LINEAR_ENDPOINT"]; got != "https://api.linear.app/graphql" {
		t.Fatalf("unexpected endpoint env: %q", got)
	}
}

func TestRuntimeStartSessionConfiguresJiraTrackerMCPServer(t *testing.T) {
	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "CC-3")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	cfg := mustClaudeJiraConfig(t, workspaceRoot, writeFakeClaudeScript(t, t.TempDir()))
	client := New(cfg)

	sess, err := client.StartSession(workspace)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer client.StopSession(sess)

	runtimeSession := sess.(*session)
	var payload struct {
		MCPServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(readFile(t, runtimeSession.mcpConfigPath)), &payload); err != nil {
		t.Fatalf("unmarshal mcp config: %v", err)
	}

	server := payload.MCPServers[trackerMCPServerName]
	if got := server.Env["BATON_TRACKER_KIND"]; got != "jira" {
		t.Fatalf("unexpected tracker kind env: %q", got)
	}
	if got := server.Env["BATON_JIRA_BASE_URL"]; got != "https://example.atlassian.net" {
		t.Fatalf("unexpected jira base url env: %q", got)
	}
	if got := server.Env["BATON_JIRA_PROJECT_KEY"]; got != "KAN" {
		t.Fatalf("unexpected jira project env: %q", got)
	}
	if got := server.Env["BATON_JIRA_API_TOKEN"]; got != "jira-token" {
		t.Fatalf("unexpected jira token env: %q", got)
	}
}

func TestRuntimeRunTurnReturnsErrorOnClaudeErrorResult(t *testing.T) {
	traceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(traceDir, "mode.txt"), []byte("error"), 0o644); err != nil {
		t.Fatalf("write mode file: %v", err)
	}

	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "CC-4")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	cfg := mustClaudeMemoryConfig(t, workspaceRoot, writeFakeClaudeScript(t, traceDir))
	client := New(cfg)

	sess, err := client.StartSession(workspace)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer client.StopSession(sess)

	var updates []runtime.Update
	_, err = client.RunTurn(sess, "fail please", tracker.Issue{Identifier: "CC-4"}, runtime.RunTurnOptions{
		OnMessage: func(update runtime.Update) {
			updates = append(updates, update)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error result to surface boom, got %v", err)
	}
	assertUpdateEvent(t, updates, "turn_failed")
}

func mustClaudeMemoryConfig(t *testing.T, workspaceRoot string, command string) *config.Config {
	t.Helper()
	return mustClaudeConfig(t, workspaceRoot, map[string]any{
		"tracker": map[string]any{
			"kind": "memory",
		},
		"agent_runtime": map[string]any{
			"kind": "claudecode",
			"claudecode": map[string]any{
				"command": command,
			},
		},
	})
}

func mustClaudeLinearConfig(t *testing.T, workspaceRoot string, command string) *config.Config {
	t.Helper()
	return mustClaudeConfig(t, workspaceRoot, map[string]any{
		"tracker": map[string]any{
			"kind": "linear",
			"linear": map[string]any{
				"api_key":      "linear-token",
				"project_slug": "baton",
			},
		},
		"agent_runtime": map[string]any{
			"kind": "claudecode",
			"claudecode": map[string]any{
				"command": command,
			},
		},
	})
}

func mustClaudeJiraConfig(t *testing.T, workspaceRoot string, command string) *config.Config {
	t.Helper()
	return mustClaudeConfig(t, workspaceRoot, map[string]any{
		"tracker": map[string]any{
			"kind": "jira",
			"jira": map[string]any{
				"base_url":    "https://example.atlassian.net",
				"project_key": "KAN",
				"jql":         "key = KAN-4",
				"auth": map[string]any{
					"type":      "email_api_token",
					"email":     "jira@example.com",
					"api_token": "jira-token",
				},
			},
			"lifecycle": map[string]any{
				"backlog":      "Backlog",
				"todo":         "To Do",
				"in_progress":  "In Progress",
				"human_review": "In Review",
				"merging":      "Ready to Merge",
				"rework":       "Rework",
				"done":         "Done",
			},
		},
		"agent_runtime": map[string]any{
			"kind": "claudecode",
			"claudecode": map[string]any{
				"command": command,
			},
		},
	})
}

func mustClaudeConfig(t *testing.T, workspaceRoot string, raw map[string]any) *config.Config {
	t.Helper()

	merged := map[string]any{
		"workspace": map[string]any{
			"root": workspaceRoot,
		},
	}
	for key, value := range raw {
		merged[key] = value
	}

	cfg, err := config.FromWorkflow(
		filepath.Join(t.TempDir(), "WORKFLOW.md"),
		&workflow.Definition{Config: withTrackerDefaults(merged)},
	)
	if err != nil {
		t.Fatalf("FromWorkflow failed: %v", err)
	}
	return cfg
}

func withTrackerDefaults(raw map[string]any) map[string]any {
	merged := map[string]any{}
	for key, value := range raw {
		merged[key] = value
	}

	trackerRaw, _ := merged["tracker"].(map[string]any)
	if trackerRaw == nil {
		trackerRaw = map[string]any{}
	}
	trackerConfig := map[string]any{}
	for key, value := range trackerRaw {
		trackerConfig[key] = value
	}
	if _, ok := trackerConfig["lifecycle"]; !ok {
		trackerConfig["lifecycle"] = map[string]any{
			"backlog":      "Backlog",
			"todo":         "Todo",
			"in_progress":  "In Progress",
			"human_review": "In Review",
			"merging":      "Merging",
			"rework":       "Rework",
			"done":         "Done",
		}
	}
	if _, ok := trackerConfig["routing"]; !ok {
		trackerConfig["routing"] = map[string]any{
			"active_states":   []any{"Todo", "In Progress"},
			"terminal_states": []any{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"},
		}
	}
	merged["tracker"] = trackerConfig
	return merged
}

func writeFakeClaudeScript(t *testing.T, traceDir string) string {
	t.Helper()

	scriptPath := filepath.Join(traceDir, "fake-claude")
	content := "#!/bin/sh\n" +
		"trace_dir=" + shDoubleQuote(traceDir) + "\n" +
		"count_file=\"$trace_dir/count.txt\"\n" +
		"mode_file=\"$trace_dir/mode.txt\"\n" +
		"count=1\n" +
		"if [ -f \"$count_file\" ]; then count=$(( $(cat \"$count_file\") + 1 )); fi\n" +
		"printf '%s' \"$count\" > \"$count_file\"\n" +
		"printf '%s\\n' \"$@\" > \"$trace_dir/args-$count.txt\"\n" +
		"cat > \"$trace_dir/stdin-$count.txt\"\n" +
		"mcp=''\n" +
		"prev=''\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$prev\" = '--mcp-config' ]; then mcp=\"$arg\"; fi\n" +
		"  prev=\"$arg\"\n" +
		"done\n" +
		"if [ -n \"$mcp\" ]; then printf '%s' \"$mcp\" > \"$trace_dir/mcp-$count.txt\"; fi\n" +
		"mode='success'\n" +
		"if [ -f \"$mode_file\" ]; then mode=$(cat \"$mode_file\"); fi\n" +
		"if [ \"$mode\" = 'error' ]; then\n" +
		"  printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"error_during_execution\",\"is_error\":true,\"session_id\":\"fake-session\",\"errors\":[\"boom\"],\"usage\":{\"input_tokens\":1,\"output_tokens\":0,\"total_tokens\":1}}'\n" +
		"  exit 1\n" +
		"fi\n" +
		"printf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"hello from claude\"}]}}'\n" +
		"printf '%s\\n' '{\"type\":\"stream_event\",\"subtype\":\"mcp_tool_call_start\",\"tool\":\"tracker_get_issue\"}'\n" +
		"printf '%s\\n' '{\"type\":\"stream_event\",\"subtype\":\"mcp_tool_call_result\",\"tool\":\"tracker_get_issue\"}'\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"session_id\":\"fake-session\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}'\n"
	if err := os.WriteFile(scriptPath, []byte(content), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return scriptPath
}

func assertUpdateEvent(t *testing.T, updates []runtime.Update, event string) runtime.Update {
	t.Helper()
	for _, update := range updates {
		if update.Event == event {
			return update
		}
	}
	t.Fatalf("expected update event %q, got %#v", event, updates)
	return runtime.Update{}
}

func assertArgsContain(t *testing.T, args []string, values ...string) {
	t.Helper()
	for index := 0; index <= len(args)-len(values); index++ {
		matched := true
		for offset, value := range values {
			if args[index+offset] != value {
				matched = false
				break
			}
		}
		if matched {
			return
		}
	}
	t.Fatalf("expected args to contain %v, got %v", values, args)
}

func assertArgsNotContain(t *testing.T, args []string, value string) {
	t.Helper()
	for _, arg := range args {
		if arg == value {
			t.Fatalf("expected args not to contain %q, got %v", value, args)
		}
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	content := strings.TrimSpace(readFile(t, path))
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

func shDoubleQuote(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "\"", "\\\"")
	return `"` + replacer.Replace(value) + `"`
}
