package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"baton/internal/config"
	"baton/internal/tracker"
	"baton/internal/workflow"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

func TestAppServerRejectsWorkspaceRootAndOutsideRoot(t *testing.T) {

	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	outsideWorkspace := filepath.Join(testRoot, "outside")
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		t.Fatalf("mkdir workspace root: %v", err)
	}
	if err := os.MkdirAll(outsideWorkspace, 0o755); err != nil {
		t.Fatalf("mkdir outside workspace: %v", err)
	}

	client := mustAppServer(t, workspaceRoot, map[string]any{})
	issue := tracker.Issue{
		ID:         "issue-workspace-guard",
		Identifier: "MT-999",
		Title:      "Validate workspace guard",
	}

	_, err := client.Run(workspaceRoot, "guard", issue, RunTurnOptions{})
	if !errors.Is(err, ErrInvalidWorkspaceCWD) {
		t.Fatalf("expected ErrInvalidWorkspaceCWD for workspace root, got %v", err)
	}

	_, err = client.Run(outsideWorkspace, "guard", issue, RunTurnOptions{})
	if !errors.Is(err, ErrInvalidWorkspaceCWD) {
		t.Fatalf("expected ErrInvalidWorkspaceCWD for outside path, got %v", err)
	}
}

func TestAppServerMarksInputRequiredAsHardFailure(t *testing.T) {

	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "MT-88")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	traceFile := filepath.Join(testRoot, "codex-input.trace")
	codexBinary := filepath.Join(testRoot, "fake-codex")
	writeScript(t, codexBinary, `
trace_file="${SYMP_TEST_CODEX_TRACE:-/tmp/codex-input.trace}"
count=0
while IFS= read -r line; do
  count=$((count + 1))
  printf 'JSON:%s\n' "$line" >> "$trace_file"
  case "$count" in
    1)
      printf '%s\n' '{"id":1,"result":{}}'
      ;;
    2)
      ;;
    3)
      printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-88"}}}'
      ;;
    4)
      printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-88"}}}'
      printf '%s\n' '{"method":"turn/input_required","id":"resp-1","params":{"requiresInput":true,"reason":"blocked"}}'
      ;;
    *)
      exit 0
      ;;
  esac
done
`)
	t.Setenv("SYMP_TEST_CODEX_TRACE", traceFile)

	client := mustAppServer(t, workspaceRoot, map[string]any{
		"codex": map[string]any{
			"command": codexBinary + " app-server",
		},
	})
	issue := tracker.Issue{
		ID:         "issue-input",
		Identifier: "MT-88",
		Title:      "Input needed",
	}

	_, err := client.Run(workspace, "Needs input", issue, RunTurnOptions{})
	if !errors.Is(err, ErrTurnInputRequired) {
		t.Fatalf("expected ErrTurnInputRequired, got %v", err)
	}
}

func TestAppServerFailsWhenApprovalRequiredUnderSaferDefaults(t *testing.T) {

	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "MT-89")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	codexBinary := filepath.Join(testRoot, "fake-codex")
	writeScript(t, codexBinary, `
count=0
while IFS= read -r line; do
  count=$((count + 1))
  case "$count" in
    1)
      printf '%s\n' '{"id":1,"result":{}}'
      ;;
    2)
      ;;
    3)
      printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-89"}}}'
      ;;
    4)
      printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-89"}}}'
      printf '%s\n' '{"id":99,"method":"item/commandExecution/requestApproval","params":{"command":"gh pr view","cwd":"/tmp","reason":"need approval"}}'
      ;;
    *)
      sleep 1
      ;;
  esac
done
`)

	client := mustAppServer(t, workspaceRoot, map[string]any{
		"codex": map[string]any{
			"command": codexBinary + " app-server",
		},
	})
	issue := tracker.Issue{
		ID:         "issue-approval-required",
		Identifier: "MT-89",
		Title:      "Approval required",
	}

	_, err := client.Run(workspace, "Handle approval request", issue, RunTurnOptions{})
	if !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("expected ErrApprovalRequired, got %v", err)
	}
}

func TestAppServerAutoApprovesCommandExecutionWhenPolicyNever(t *testing.T) {

	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "MT-89")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	traceFile := filepath.Join(testRoot, "codex-auto-approve.trace")
	codexBinary := filepath.Join(testRoot, "fake-codex")
	writeScript(t, codexBinary, `
trace_file="${SYMP_TEST_CODEX_TRACE:-/tmp/codex-auto-approve.trace}"
count=0
while IFS= read -r line; do
  count=$((count + 1))
  printf 'JSON:%s\n' "$line" >> "$trace_file"
  case "$count" in
    1)
      printf '%s\n' '{"id":1,"result":{}}'
      ;;
    2)
      ;;
    3)
      printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-89"}}}'
      ;;
    4)
      printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-89"}}}'
      printf '%s\n' '{"id":99,"method":"item/commandExecution/requestApproval","params":{"command":"gh pr view","cwd":"/tmp","reason":"need approval"}}'
      ;;
    5)
      printf '%s\n' '{"method":"turn/completed"}'
      exit 0
      ;;
    *)
      exit 0
      ;;
  esac
done
`)
	t.Setenv("SYMP_TEST_CODEX_TRACE", traceFile)

	client := mustAppServer(t, workspaceRoot, map[string]any{
		"codex": map[string]any{
			"command":         codexBinary + " app-server",
			"approval_policy": "never",
		},
	})
	issue := tracker.Issue{
		ID:         "issue-auto-approve",
		Identifier: "MT-89",
		Title:      "Auto approve request",
	}

	if _, err := client.Run(workspace, "Handle approval request", issue, RunTurnOptions{}); err != nil {
		t.Fatalf("expected run to succeed, got %v", err)
	}

	lines := readTraceJSONLines(t, traceFile)
	if !containsInitializeWithExperimentalAPI(lines) {
		t.Fatalf("expected initialize payload with experimentalApi=true, lines=%#v", lines)
	}
	if !containsDynamicToolSpec(lines) {
		t.Fatalf("expected thread/start payload to include linear_graphql dynamic tool spec")
	}
	if !containsApprovalDecision(lines, 99, decisionAcceptForSession) {
		t.Fatalf("expected approval decision payload for id=99")
	}
}

func TestAppServerFailsOnMCPToolInputRequestWhenPolicyNever(t *testing.T) {
	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "MT-717")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	traceFile := filepath.Join(testRoot, "codex-tool-user-input-auto-approve.trace")
	codexBinary := filepath.Join(testRoot, "fake-codex")
	writeScript(t, codexBinary, `
trace_file="${SYMP_TEST_CODEX_TRACE:-/tmp/codex-tool-user-input-auto-approve.trace}"
count=0
while IFS= read -r line; do
  count=$((count + 1))
  printf 'JSON:%s\n' "$line" >> "$trace_file"
  case "$count" in
    1)
      printf '%s\n' '{"id":1,"result":{}}'
      ;;
    2)
      ;;
    3)
      printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-717"}}}'
      ;;
    4)
      printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-717"}}}'
      printf '%s\n' '{"id":110,"method":"item/tool/requestUserInput","params":{"itemId":"call-717","questions":[{"header":"Approve app tool call?","id":"mcp_tool_call_approval_call-717","isOther":false,"isSecret":false,"options":[{"description":"Run the tool and continue.","label":"Approve Once"},{"description":"Run the tool and remember this choice for this session.","label":"Approve this Session"},{"description":"Decline this tool call and continue.","label":"Deny"},{"description":"Cancel this tool call","label":"Cancel"}],"question":"The linear MCP server wants to run the tool."}]}}'
      ;;
    5)
      printf '%s\n' '{"method":"turn/completed"}'
      exit 0
      ;;
    *)
      exit 0
      ;;
  esac
done
`)
	t.Setenv("SYMP_TEST_CODEX_TRACE", traceFile)

	client := mustAppServer(t, workspaceRoot, map[string]any{
		"codex": map[string]any{
			"command":         codexBinary + " app-server",
			"approval_policy": "never",
		},
	})
	issue := tracker.Issue{
		ID:         "issue-tool-user-input-auto-approve",
		Identifier: "MT-717",
		Title:      "Auto approve tool input request",
	}

	_, err := client.Run(workspace, "Handle tool input approval", issue, RunTurnOptions{})
	if !errors.Is(err, ErrTurnInputRequired) {
		t.Fatalf("expected ErrTurnInputRequired, got %v", err)
	}

	lines := readTraceJSONLines(t, traceFile)
	if containsToolInputAnswer(lines, 110, "mcp_tool_call_approval_call-717", "Approve this Session") {
		t.Fatalf("did not expect MCP tool approval answer payload for id=110")
	}
}

func TestAppServerFailsOnGenericToolInputRequest(t *testing.T) {
	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "MT-718")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	traceFile := filepath.Join(testRoot, "codex-tool-user-input-noninteractive.trace")
	codexBinary := filepath.Join(testRoot, "fake-codex")
	writeScript(t, codexBinary, `
trace_file="${SYMP_TEST_CODEX_TRACE:-/tmp/codex-tool-user-input-noninteractive.trace}"
count=0
while IFS= read -r line; do
  count=$((count + 1))
  printf 'JSON:%s\n' "$line" >> "$trace_file"
  case "$count" in
    1)
      printf '%s\n' '{"id":1,"result":{}}'
      ;;
    2)
      ;;
    3)
      printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-718"}}}'
      ;;
    4)
      printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-718"}}}'
      printf '%s\n' '{"id":120,"method":"item/tool/requestUserInput","params":{"questions":[{"id":"freeform-q","question":"Describe the failure."}]}}'
      ;;
    5)
      printf '%s\n' '{"method":"turn/completed"}'
      exit 0
      ;;
    *)
      exit 0
      ;;
  esac
done
`)
	t.Setenv("SYMP_TEST_CODEX_TRACE", traceFile)

	client := mustAppServer(t, workspaceRoot, map[string]any{
		"codex": map[string]any{
			"command": codexBinary + " app-server",
		},
	})
	issue := tracker.Issue{
		ID:         "issue-tool-user-input-noninteractive",
		Identifier: "MT-718",
		Title:      "Non interactive tool input",
	}

	_, err := client.Run(workspace, "Handle tool input request", issue, RunTurnOptions{})
	if !errors.Is(err, ErrTurnInputRequired) {
		t.Fatalf("expected ErrTurnInputRequired, got %v", err)
	}

	lines := readTraceJSONLines(t, traceFile)
	if containsToolInputAnswer(lines, 120, "freeform-q", "") {
		t.Fatalf("did not expect tool input answer payload for id=120")
	}
}

func TestAppServerRejectsUnsupportedDynamicToolCallsWithoutStalling(t *testing.T) {

	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "MT-596")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	traceFile := filepath.Join(testRoot, "codex-unsupported-tool.trace")
	codexBinary := filepath.Join(testRoot, "fake-codex")
	writeScript(t, codexBinary, `
trace_file="${SYMP_TEST_CODEX_TRACE:-/tmp/codex-unsupported-tool.trace}"
count=0
while IFS= read -r line; do
  count=$((count + 1))
  printf 'JSON:%s\n' "$line" >> "$trace_file"
  case "$count" in
    1)
      printf '%s\n' '{"id":1,"result":{}}'
      ;;
    2)
      ;;
    3)
      printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-596"}}}'
      ;;
    4)
      printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-596"}}}'
      printf '%s\n' '{"id":103,"method":"item/tool/call","params":{"name":"unknown_tool","callId":"call-596","arguments":{"x":1}}}'
      ;;
    5)
      printf '%s\n' '{"method":"turn/completed"}'
      exit 0
      ;;
    *)
      exit 0
      ;;
  esac
done
`)
	t.Setenv("SYMP_TEST_CODEX_TRACE", traceFile)

	client := mustAppServer(t, workspaceRoot, map[string]any{
		"codex": map[string]any{
			"command": codexBinary + " app-server",
		},
	})
	issue := tracker.Issue{
		ID:         "issue-unsupported-tool",
		Identifier: "MT-596",
		Title:      "Unsupported tool call",
	}

	if _, err := client.Run(workspace, "Handle unsupported tool", issue, RunTurnOptions{}); err != nil {
		t.Fatalf("expected run to succeed, got %v", err)
	}

	lines := readTraceJSONLines(t, traceFile)
	if !containsToolCallFailureResult(lines, 103) {
		t.Fatalf("expected tool failure response payload for id=103")
	}
}

func TestAppServerEmitsSupportedDynamicToolSuccessEvent(t *testing.T) {
	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "MT-630")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token" {
			t.Fatalf("expected Authorization header to be token, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"viewer": map[string]any{"id": "usr_123"}},
		})
	}))
	defer server.Close()

	traceFile := filepath.Join(testRoot, "codex-supported-tool-success.trace")
	codexBinary := filepath.Join(testRoot, "fake-codex")
	writeScript(t, codexBinary, `
trace_file="${SYMP_TEST_CODEX_TRACE:-/tmp/codex-supported-tool-success.trace}"
count=0
while IFS= read -r line; do
  count=$((count + 1))
  printf 'JSON:%s\n' "$line" >> "$trace_file"
  case "$count" in
    1)
      printf '%s\n' '{"id":1,"result":{}}'
      ;;
    2)
      ;;
    3)
      printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-630"}}}'
      ;;
    4)
      printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-630"}}}'
      printf '%s\n' '{"id":130,"method":"item/tool/call","params":{"name":"linear_graphql","callId":"call-630","arguments":{"query":"query Viewer { viewer { id } }"}}}'
      ;;
    5)
      printf '%s\n' '{"method":"turn/completed"}'
      exit 0
      ;;
    *)
      exit 0
      ;;
  esac
done
`)
	t.Setenv("SYMP_TEST_CODEX_TRACE", traceFile)

	client := mustAppServer(t, workspaceRoot, map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      "token",
			"project_slug": "proj",
			"endpoint":     server.URL,
		},
		"codex": map[string]any{
			"command": codexBinary + " app-server",
		},
	})
	issue := tracker.Issue{
		ID:         "issue-supported-tool-success",
		Identifier: "MT-630",
		Title:      "Supported tool success",
	}

	events := []string{}
	var eventsMu sync.Mutex
	onMessage := func(update Update) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, update.Event)
	}

	if _, err := client.Run(workspace, "Handle supported tool success", issue, RunTurnOptions{OnMessage: onMessage}); err != nil {
		t.Fatalf("expected run to succeed, got %v", err)
	}

	eventsMu.Lock()
	recordedEvents := append([]string(nil), events...)
	eventsMu.Unlock()
	if !containsEvent(recordedEvents, "tool_call_completed") {
		t.Fatalf("expected tool_call_completed event, got %#v", recordedEvents)
	}
	if containsEvent(recordedEvents, "tool_call_failed") {
		t.Fatalf("did not expect tool_call_failed event, got %#v", recordedEvents)
	}

	lines := readTraceJSONLines(t, traceFile)
	if !containsToolCallResult(lines, 130, true) {
		t.Fatalf("expected successful tool result payload for id=130")
	}
}

func TestAppServerEmitsSupportedDynamicToolFailureEvent(t *testing.T) {
	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "MT-631")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	traceFile := filepath.Join(testRoot, "codex-supported-tool-failure.trace")
	codexBinary := filepath.Join(testRoot, "fake-codex")
	writeScript(t, codexBinary, `
trace_file="${SYMP_TEST_CODEX_TRACE:-/tmp/codex-supported-tool-failure.trace}"
count=0
while IFS= read -r line; do
  count=$((count + 1))
  printf 'JSON:%s\n' "$line" >> "$trace_file"
  case "$count" in
    1)
      printf '%s\n' '{"id":1,"result":{}}'
      ;;
    2)
      ;;
    3)
      printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-631"}}}'
      ;;
    4)
      printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-631"}}}'
      printf '%s\n' '{"id":131,"method":"item/tool/call","params":{"name":"linear_graphql","callId":"call-631","arguments":{"query":"query Viewer { viewer { id } }"}}}'
      ;;
    5)
      printf '%s\n' '{"method":"turn/completed"}'
      exit 0
      ;;
    *)
      exit 0
      ;;
  esac
done
`)
	t.Setenv("SYMP_TEST_CODEX_TRACE", traceFile)

	client := mustAppServer(t, workspaceRoot, map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"project_slug": "proj",
		},
		"codex": map[string]any{
			"command": codexBinary + " app-server",
		},
	})
	issue := tracker.Issue{
		ID:         "issue-supported-tool-failure",
		Identifier: "MT-631",
		Title:      "Supported tool failure",
	}

	events := []string{}
	var eventsMu sync.Mutex
	onMessage := func(update Update) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, update.Event)
	}

	if _, err := client.Run(workspace, "Handle supported tool failure", issue, RunTurnOptions{OnMessage: onMessage}); err != nil {
		t.Fatalf("expected run to succeed, got %v", err)
	}

	eventsMu.Lock()
	recordedEvents := append([]string(nil), events...)
	eventsMu.Unlock()
	if !containsEvent(recordedEvents, "tool_call_failed") {
		t.Fatalf("expected tool_call_failed event, got %#v", recordedEvents)
	}
	if containsEvent(recordedEvents, "tool_call_completed") {
		t.Fatalf("did not expect tool_call_completed event, got %#v", recordedEvents)
	}

	lines := readTraceJSONLines(t, traceFile)
	if !containsToolCallResult(lines, 131, false) {
		t.Fatalf("expected failed tool result payload for id=131")
	}
}

func TestAppServerBuffersPartialLineUntilNewline(t *testing.T) {
	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "MT-632")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	codexBinary := filepath.Join(testRoot, "fake-codex")
	writeScript(t, codexBinary, `
count=0
while IFS= read -r line; do
  count=$((count + 1))
  case "$count" in
    1)
      printf '%s\n' '{"id":1,"result":{}}'
      ;;
    2)
      ;;
    3)
      printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-632"}}}'
      ;;
    4)
      printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-632"}}}'
      printf '%s' '{"method":"turn/com'
      sleep 0.05
      printf '%s\n' 'pleted"}'
      exit 0
      ;;
    *)
      exit 0
      ;;
  esac
done
`)

	client := mustAppServer(t, workspaceRoot, map[string]any{
		"codex": map[string]any{
			"command": codexBinary + " app-server",
		},
	})
	issue := tracker.Issue{
		ID:         "issue-partial-line",
		Identifier: "MT-632",
		Title:      "Partial line buffering",
	}

	events := []string{}
	var eventsMu sync.Mutex
	onMessage := func(update Update) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, update.Event)
	}

	if _, err := client.Run(workspace, "Handle partial line buffering", issue, RunTurnOptions{OnMessage: onMessage}); err != nil {
		t.Fatalf("expected run to succeed, got %v", err)
	}

	eventsMu.Lock()
	recordedEvents := append([]string(nil), events...)
	eventsMu.Unlock()
	if !containsEvent(recordedEvents, "turn_completed") {
		t.Fatalf("expected turn_completed event, got %#v", recordedEvents)
	}
}

func TestAppServerRunTurnReturnsWhenContextCanceled(t *testing.T) {
	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "MT-590")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	codexBinary := filepath.Join(testRoot, "fake-codex")
	writeScript(t, codexBinary, `
count=0
while IFS= read -r line; do
  count=$((count + 1))
  case "$count" in
    1)
      printf '%s\n' '{"id":1,"result":{}}'
      ;;
    2)
      ;;
    3)
      printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-590"}}}'
      ;;
    4)
      printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-590"}}}'
      ;;
    *)
      sleep 30
      ;;
  esac
done
`)

	client := mustAppServer(t, workspaceRoot, map[string]any{
		"codex": map[string]any{
			"command":         codexBinary + " app-server",
			"turn_timeout_ms": 60_000,
		},
	})
	issue := tracker.Issue{
		ID:         "issue-turn-cancel",
		Identifier: "MT-590",
		Title:      "Turn cancellation",
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	_, err := client.Run(workspace, "cancel turn", issue, RunTurnOptions{Context: ctx})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}

func TestAppServerCapturesCodexSideOutputAndLogsIt(t *testing.T) {
	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "MT-92")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	codexBinary := filepath.Join(testRoot, "fake-codex")
	writeScript(t, codexBinary, `
count=0
while IFS= read -r line; do
  count=$((count + 1))
  case "$count" in
    1)
      printf '%s\n' '{"id":1,"result":{}}'
      ;;
    2)
      ;;
    3)
      printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-92"}}}'
      ;;
    4)
      printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-92"}}}'
      printf '%s\n' 'warning: this is stderr noise' >&2
      printf '%s\n' '{"method":"turn/completed"}'
      exit 0
      ;;
    *)
      exit 0
      ;;
  esac
done
`)

	client := mustAppServer(t, workspaceRoot, map[string]any{
		"codex": map[string]any{
			"command": codexBinary + " app-server",
		},
	})
	issue := tracker.Issue{
		ID:         "issue-stderr",
		Identifier: "MT-92",
		Title:      "Capture stderr",
	}

	var logBuffer bytes.Buffer
	previous := zlog.Logger
	zlog.Logger = zerolog.New(&logBuffer).Level(zerolog.DebugLevel)
	t.Cleanup(func() {
		zlog.Logger = previous
	})

	if _, err := client.Run(workspace, "capture stderr", issue, RunTurnOptions{}); err != nil {
		t.Fatalf("expected run to succeed, got %v", err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for !strings.Contains(logBuffer.String(), "Codex turn stream output: warning: this is stderr noise") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(logBuffer.String(), "Codex turn stream output: warning: this is stderr noise") {
		t.Fatalf("expected stream log line, got %q", logBuffer.String())
	}
}

func mustAppServer(t *testing.T, workspaceRoot string, overrides map[string]any) *AppServer {
	t.Helper()

	cfgMap := map[string]any{
		"tracker": map[string]any{
			"kind": "memory",
		},
		"workspace": map[string]any{
			"root": workspaceRoot,
		},
		"codex": map[string]any{
			"read_timeout_ms": 20_000,
		},
	}
	for key, value := range overrides {
		cfgMap[key] = value
	}
	codexConfig, _ := cfgMap["codex"].(map[string]any)
	if codexConfig == nil {
		codexConfig = map[string]any{}
	}
	if _, ok := codexConfig["read_timeout_ms"]; !ok {
		codexConfig["read_timeout_ms"] = 20_000
	}
	cfgMap["codex"] = codexConfig

	cfg, err := config.FromWorkflow(filepath.Join(t.TempDir(), "WORKFLOW.md"), &workflow.Definition{
		Config: cfgMap,
	})
	if err != nil {
		t.Fatalf("FromWorkflow failed: %v", err)
	}

	return NewAppServer(cfg)
}

func writeScript(t *testing.T, scriptPath string, body string) {
	t.Helper()

	content := "#!/bin/sh\n" + strings.TrimSpace(body) + "\n"
	if err := os.WriteFile(scriptPath, []byte(content), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
}

func readTraceJSONLines(t *testing.T, tracePath string) []map[string]any {
	t.Helper()

	raw, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}

	lines := strings.Split(string(raw), "\n")
	out := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if !strings.HasPrefix(line, "JSON:") {
			continue
		}
		payload := strings.TrimPrefix(line, "JSON:")
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Fatalf("decode trace payload failed: %v (%q)", err, payload)
		}
		out = append(out, decoded)
	}
	return out
}

func containsInitializeWithExperimentalAPI(lines []map[string]any) bool {
	for _, line := range lines {
		id := intValue(line["id"])
		if id != 1 {
			continue
		}
		params, _ := line["params"].(map[string]any)
		caps, _ := params["capabilities"].(map[string]any)
		if value, _ := caps["experimentalApi"].(bool); value {
			return true
		}
	}
	return false
}

func containsDynamicToolSpec(lines []map[string]any) bool {
	for _, line := range lines {
		id := intValue(line["id"])
		if id != 2 {
			continue
		}
		params, _ := line["params"].(map[string]any)
		dynamicTools, _ := params["dynamicTools"].([]any)
		for _, rawTool := range dynamicTools {
			tool, _ := rawTool.(map[string]any)
			name, _ := tool["name"].(string)
			if name == "linear_graphql" {
				inputSchema, _ := tool["inputSchema"].(map[string]any)
				required, _ := inputSchema["required"].([]any)
				if len(required) == 1 && required[0] == "query" {
					return true
				}
			}
		}
	}
	return false
}

func containsApprovalDecision(lines []map[string]any, id int, decision string) bool {
	for _, line := range lines {
		if intValue(line["id"]) != id {
			continue
		}
		result, _ := line["result"].(map[string]any)
		if result["decision"] == decision {
			return true
		}
	}
	return false
}

func containsToolCallFailureResult(lines []map[string]any, id int) bool {
	return containsToolCallResult(lines, id, false)
}

func containsToolCallResult(lines []map[string]any, id int, expectedSuccess bool) bool {
	for _, line := range lines {
		if intValue(line["id"]) != id {
			continue
		}
		result, _ := line["result"].(map[string]any)
		success, _ := result["success"].(bool)
		if success == expectedSuccess {
			return true
		}
	}
	return false
}

func containsEvent(events []string, expected string) bool {
	for _, event := range events {
		if event == expected {
			return true
		}
	}
	return false
}

func containsToolInputAnswer(lines []map[string]any, id int, questionID string, expected string) bool {
	for _, line := range lines {
		if intValue(line["id"]) != id {
			continue
		}
		result, _ := line["result"].(map[string]any)
		answers, _ := result["answers"].(map[string]any)
		rawAnswer, _ := answers[questionID].(map[string]any)
		values, _ := rawAnswer["answers"].([]any)
		if len(values) == 0 {
			continue
		}
		value, _ := values[0].(string)
		if strings.TrimSpace(expected) == "" {
			return strings.TrimSpace(value) != ""
		}
		if value == expected {
			return true
		}
	}
	return false
}

func intValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	default:
		return -1
	}
}
