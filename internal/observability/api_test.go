package observability

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"baton/internal/logging"
	"baton/internal/orchestrator"
)

type orchestratorStub struct {
	snapshotPayload map[string]any
	snapshotErr     error
	refreshPayload  map[string]any
	refreshErr      error
}

func (s *orchestratorStub) Snapshot(timeout time.Duration) (map[string]any, error) {
	if s.snapshotErr != nil {
		return nil, s.snapshotErr
	}
	return s.snapshotPayload, nil
}

func (s *orchestratorStub) RequestRefresh() (map[string]any, error) {
	if s.refreshErr != nil {
		return nil, s.refreshErr
	}
	return s.refreshPayload, nil
}

func TestStateIssueRefreshPayloads(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 3, 5, 10, 0, 0, 0, time.UTC)
	requestedAt := time.Date(2026, 3, 5, 10, 1, 12, 0, time.UTC)
	logsRoot := t.TempDir()
	issueLogsDir := logging.CodexIssueLogsDir(logsRoot, "MT-HTTP")
	if err := os.MkdirAll(issueLogsDir, 0o755); err != nil {
		t.Fatalf("mkdir issue logs dir: %v", err)
	}
	latestPath := filepath.Join(issueLogsDir, "latest.log")
	if err := os.WriteFile(latestPath, []byte("session log"), 0o644); err != nil {
		t.Fatalf("write latest session log: %v", err)
	}
	sessionPath := filepath.Join(issueLogsDir, "session-20260305T100000Z.log")
	if err := os.WriteFile(sessionPath, []byte("older session"), 0o644); err != nil {
		t.Fatalf("write session log: %v", err)
	}

	h := NewHandler(HandlerOptions{
		Orchestrator: &orchestratorStub{
			snapshotPayload: map[string]any{
				"running": []map[string]any{
					{
						"issue_id":             "issue-http",
						"identifier":           "MT-HTTP",
						"state":                "In Progress",
						"session_id":           "thread-http",
						"turn_count":           7,
						"last_codex_event":     "notification",
						"last_codex_message":   "rendered",
						"last_codex_timestamp": nil,
						"started_at":           startedAt,
						"codex_input_tokens":   4,
						"codex_output_tokens":  8,
						"codex_total_tokens":   12,
					},
				},
				"retrying": []map[string]any{
					{
						"issue_id":   "issue-retry",
						"identifier": "MT-RETRY",
						"attempt":    2,
						"due_in_ms":  2000,
						"error":      "boom",
					},
				},
				"codex_totals": map[string]any{
					"input_tokens":    4,
					"output_tokens":   8,
					"total_tokens":    12,
					"seconds_running": 42.5,
				},
				"rate_limits": map[string]any{"primary": map[string]any{"remaining": 11}},
			},
			refreshPayload: map[string]any{
				"queued":       true,
				"coalesced":    false,
				"requested_at": requestedAt,
				"operations":   []string{"poll", "reconcile"},
			},
		},
		WorkspaceRoot: "/tmp/symphony_workspaces",
		LogsRoot:      logsRoot,
	})

	ts := httptest.NewServer(h)
	defer ts.Close()

	state := getJSON(t, ts.URL+"/api/v1/state")
	assertEqual(t, state["counts"], map[string]any{"running": float64(1), "retrying": float64(1)})

	runningRows := state["running"].([]any)
	running := runningRows[0].(map[string]any)
	assertEqual(t, running["issue_identifier"], "MT-HTTP")
	assertEqual(t, running["turn_count"], float64(7))
	assertEqual(t, running["last_message"], "rendered")
	assertEqual(t, running["started_at"], "2026-03-05T10:00:00Z")
	assertEqual(t, running["tokens"], map[string]any{"input_tokens": float64(4), "output_tokens": float64(8), "total_tokens": float64(12)})

	issue := getJSON(t, ts.URL+"/api/v1/MT-HTTP")
	assertEqual(t, issue["issue_identifier"], "MT-HTTP")
	assertEqual(t, issue["issue_id"], "issue-http")
	assertEqual(t, issue["status"], "running")
	assertEqual(t, issue["workspace"], map[string]any{"path": filepath.Join("/tmp/symphony_workspaces", "MT-HTTP")})
	assertEqual(t, issue["attempts"], map[string]any{"restart_count": float64(0), "current_retry_attempt": float64(0)})
	logs := issue["logs"].(map[string]any)["codex_session_logs"].([]any)
	if len(logs) < 1 {
		t.Fatalf("expected codex session logs, got %#v", logs)
	}
	firstLog := logs[0].(map[string]any)
	assertEqual(t, firstLog["label"], "latest")
	assertEqual(t, firstLog["path"], latestPath)

	retryingIssue := getJSON(t, ts.URL+"/api/v1/MT-RETRY")
	assertEqual(t, retryingIssue["status"], "retrying")
	assertEqual(t, retryingIssue["issue_id"], "issue-retry")
	assertEqual(t, retryingIssue["last_error"], "boom")

	missingResp, err := http.Get(ts.URL + "/api/v1/MT-MISSING")
	if err != nil {
		t.Fatalf("get missing issue: %v", err)
	}
	defer missingResp.Body.Close()
	if missingResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected missing issue 404, got %d", missingResp.StatusCode)
	}
	assertEqual(t, decodeJSON(t, missingResp)["error"], map[string]any{
		"code":    "issue_not_found",
		"message": "Issue not found",
	})

	resp, err := http.Post(ts.URL+"/api/v1/refresh", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("post refresh: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected refresh 202, got %d", resp.StatusCode)
	}
	refresh := decodeJSON(t, resp)
	assertEqual(t, refresh["queued"], true)
	assertEqual(t, refresh["coalesced"], false)
	assertEqual(t, refresh["operations"], []any{"poll", "reconcile"})
	assertEqual(t, refresh["requested_at"], "2026-03-05T10:01:12Z")
}

func TestUnavailableAndTimeoutErrors(t *testing.T) {
	t.Parallel()

	unavailable := NewHandler(HandlerOptions{
		Orchestrator: &orchestratorStub{
			snapshotErr: orchestrator.ErrOrchestratorUnavailable,
			refreshErr:  orchestrator.ErrOrchestratorUnavailable,
		},
	})

	tsUnavailable := httptest.NewServer(unavailable)
	defer tsUnavailable.Close()

	stateUnavailable := getJSON(t, tsUnavailable.URL+"/api/v1/state")
	assertEqual(t, stateUnavailable["error"], map[string]any{
		"code":    "snapshot_unavailable",
		"message": "Snapshot unavailable",
	})

	resp, err := http.Post(tsUnavailable.URL+"/api/v1/refresh", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("post refresh unavailable: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected refresh 503, got %d", resp.StatusCode)
	}
	assertEqual(t, decodeJSON(t, resp)["error"], map[string]any{
		"code":    "orchestrator_unavailable",
		"message": "Orchestrator is unavailable",
	})

	timeout := NewHandler(HandlerOptions{
		Orchestrator: &orchestratorStub{snapshotErr: orchestrator.ErrSnapshotTimeout},
	})
	tsTimeout := httptest.NewServer(timeout)
	defer tsTimeout.Close()

	stateTimeout := getJSON(t, tsTimeout.URL+"/api/v1/state")
	assertEqual(t, stateTimeout["error"], map[string]any{
		"code":    "snapshot_timeout",
		"message": "Snapshot timed out",
	})

	issueResp, err := http.Get(tsUnavailable.URL + "/api/v1/MT-UNKNOWN")
	if err != nil {
		t.Fatalf("get unavailable issue: %v", err)
	}
	defer issueResp.Body.Close()
	if issueResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected unavailable issue 404, got %d", issueResp.StatusCode)
	}
	assertEqual(t, decodeJSON(t, issueResp)["error"], map[string]any{
		"code":    "issue_not_found",
		"message": "Issue not found",
	})
}

func TestStateUsesHumanizedMessageForNilAndStructuredEvents(t *testing.T) {
	t.Parallel()

	eventAt := time.Date(2026, 3, 5, 10, 15, 30, 0, time.UTC)
	h := NewHandler(HandlerOptions{
		Orchestrator: &orchestratorStub{
			snapshotPayload: map[string]any{
				"running": []map[string]any{
					{
						"issue_id":             "issue-nil",
						"identifier":           "MT-NIL",
						"state":                "In Progress",
						"session_id":           "thread-nil",
						"turn_count":           2,
						"last_codex_event":     "notification",
						"last_codex_message":   nil,
						"last_codex_timestamp": eventAt,
						"started_at":           eventAt,
						"codex_input_tokens":   1,
						"codex_output_tokens":  2,
						"codex_total_tokens":   3,
					},
					{
						"issue_id":         "issue-structured",
						"identifier":       "MT-STRUCT",
						"state":            "In Progress",
						"session_id":       "thread-structured",
						"turn_count":       3,
						"last_codex_event": "notification",
						"last_codex_message": map[string]any{
							"event": "notification",
							"message": map[string]any{
								"payload": map[string]any{
									"method": "codex/event/agent_message_delta",
									"params": map[string]any{
										"delta": "structured update",
									},
								},
							},
						},
						"last_codex_timestamp": eventAt,
						"started_at":           eventAt,
						"codex_input_tokens":   3,
						"codex_output_tokens":  5,
						"codex_total_tokens":   8,
					},
				},
				"retrying":     []map[string]any{},
				"codex_totals": map[string]any{},
				"rate_limits":  nil,
			},
		},
		WorkspaceRoot: "/tmp/symphony_workspaces",
	})

	ts := httptest.NewServer(h)
	defer ts.Close()

	state := getJSON(t, ts.URL+"/api/v1/state")
	rows := state["running"].([]any)
	nilMessage := rows[0].(map[string]any)
	structuredMessage := rows[1].(map[string]any)

	assertEqual(t, nilMessage["last_message"], "no codex message yet")
	assertEqual(t, structuredMessage["last_message"], "agent message streaming: structured update")
}

func TestStatePassesThroughNilCodexTotals(t *testing.T) {
	t.Parallel()

	h := NewHandler(HandlerOptions{
		Orchestrator: &orchestratorStub{
			snapshotPayload: map[string]any{
				"running":      []map[string]any{},
				"retrying":     []map[string]any{},
				"codex_totals": nil,
				"rate_limits":  nil,
			},
		},
	})

	ts := httptest.NewServer(h)
	defer ts.Close()

	state := getJSON(t, ts.URL+"/api/v1/state")
	assertEqual(t, state["codex_totals"], nil)
}

func TestMethodNotAllowedAndNotFound(t *testing.T) {
	t.Parallel()

	h := NewHandler(HandlerOptions{Orchestrator: &orchestratorStub{snapshotPayload: map[string]any{"running": []map[string]any{}, "retrying": []map[string]any{}, "codex_totals": map[string]any{}, "rate_limits": nil}}})
	ts := httptest.NewServer(h)
	defer ts.Close()

	cases := []struct {
		method string
		path   string
		status int
		code   string
	}{
		{method: http.MethodPost, path: "/", status: http.StatusMethodNotAllowed, code: "method_not_allowed"},
		{method: http.MethodPost, path: "/api/v1/state", status: http.StatusMethodNotAllowed, code: "method_not_allowed"},
		{method: http.MethodGet, path: "/api/v1/refresh", status: http.StatusMethodNotAllowed, code: "method_not_allowed"},
		{method: http.MethodPost, path: "/api/v1/MT-1", status: http.StatusMethodNotAllowed, code: "method_not_allowed"},
		{method: http.MethodGet, path: "/unknown", status: http.StatusNotFound, code: "not_found"},
	}

	for _, tc := range cases {
		req, err := http.NewRequest(tc.method, ts.URL+tc.path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		if resp.StatusCode != tc.status {
			t.Fatalf("%s %s expected status %d, got %d", tc.method, tc.path, tc.status, resp.StatusCode)
		}
		body := decodeJSON(t, resp)
		_ = resp.Body.Close()
		errorBody := body["error"].(map[string]any)
		if errorBody["code"] != tc.code {
			t.Fatalf("%s %s expected code %q, got %q", tc.method, tc.path, tc.code, errorBody["code"])
		}
	}
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for %s, got %d", url, resp.StatusCode)
	}
	return decodeJSON(t, resp)
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	return payload
}

func assertEqual(t *testing.T, got any, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected value\n  got: %#v\n want: %#v", got, want)
	}
}
