package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"baton/internal/codex"
	"baton/internal/config"
	"baton/internal/runtime"
	"baton/internal/tracker"
	"baton/internal/workflow"
)

func TestRuntimeRunTurnCompletesFromSessionIdle(t *testing.T) {
	t.Parallel()

	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "OC-1")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	events := make(chan string, 8)
	var createdBody map[string]any
	var promptBody map[string]any

	server := newFakeOpencodeServer(t, events, fakeOpencodeHandlers{
		createSession: func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&createdBody); err != nil {
				t.Fatalf("decode create session: %v", err)
			}
			writeJSON(t, w, map[string]any{"id": "sess-1"})
		},
		promptAsync: func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&promptBody); err != nil {
				t.Fatalf("decode prompt: %v", err)
			}
			writeJSON(t, w, map[string]any{"id": "msg-user-1"})
			events <- `{"directory":"` + workspace + `","payload":{"type":"message.updated","properties":{"info":{"id":"msg-assistant-1","sessionID":"sess-1","role":"assistant","tokens":{"input":11,"output":7,"total":18}}}}}`
			events <- `{"directory":"` + workspace + `","payload":{"type":"message.part.updated","properties":{"part":{"id":"part-text-1","sessionID":"sess-1","messageID":"msg-assistant-1","type":"text"}}}}`
			events <- `{"directory":"` + workspace + `","payload":{"type":"message.part.delta","properties":{"sessionID":"sess-1","messageID":"msg-assistant-1","partID":"part-text-1","field":"text","delta":"hello world"}}}`
			events <- `{"directory":"` + workspace + `","payload":{"type":"session.status","properties":{"sessionID":"sess-1","status":{"type":"idle"}}}}`
		},
	})
	defer server.Close()

	command := writeServeScript(t, testRoot, server.URL)
	cfg := mustOpencodeConfig(t, workspaceRoot, command)
	client := New(cfg)

	sess, err := client.StartSession(workspace)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer client.StopSession(sess)

	var updates []runtime.Update
	result, err := client.RunTurn(sess, "Implement it", tracker.Issue{ID: "issue-1", Identifier: "OC-1"}, runtime.RunTurnOptions{
		OnMessage: func(update runtime.Update) {
			updates = append(updates, update)
		},
	})
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}

	if result.SessionID != "sess-1-turn-1" {
		t.Fatalf("unexpected synthetic session id: %q", result.SessionID)
	}
	if result.ThreadID != "sess-1" {
		t.Fatalf("unexpected thread id: %q", result.ThreadID)
	}
	if result.TurnID != "turn-1" {
		t.Fatalf("unexpected turn id: %q", result.TurnID)
	}

	if got := stringValue(createdBody["title"]); got != "OC-1" {
		t.Fatalf("unexpected session title: %q", got)
	}
	if got := stringValue(createdBody["permission"]); got == "" {
		t.Fatalf("expected permission rules to be sent: %#v", createdBody)
	}
	parts, _ := promptBody["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("unexpected prompt parts: %#v", promptBody)
	}

	assertUpdateEvent(t, updates, "session_started")
	assertUpdateEvent(t, updates, "agent_message_delta")
	tokenUpdate := assertUpdateEvent(t, updates, "token_count")
	payload, _ := tokenUpdate.Payload.(map[string]any)
	usage := payload["tokenUsage"].(map[string]any)["total"].(map[string]any)
	if got := mapInteger(usage["total_tokens"]); got != 18 {
		t.Fatalf("unexpected total tokens: %#v", usage)
	}
	assertUpdateEvent(t, updates, "turn_completed")
}

func TestRuntimeRunTurnCompletesFromStatusPollWhenIdleEventMissing(t *testing.T) {
	t.Parallel()

	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "OC-7")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	restore := setStatusPollIntervalForTest(25 * time.Millisecond)
	defer restore()

	events := make(chan string, 2)
	statusCalls := 0
	server := newFakeOpencodeServer(t, events, fakeOpencodeHandlers{
		promptAsync: func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"id": "msg-user-7"})
		},
		sessionStatus: func(w http.ResponseWriter, r *http.Request) {
			statusCalls++
			if statusCalls == 1 {
				writeJSON(t, w, map[string]any{
					"sess-1": map[string]any{"type": "busy"},
				})
				return
			}
			writeJSON(t, w, map[string]any{})
		},
		sessionMessages: func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, []map[string]any{
				{
					"id":         "msg-assistant-7",
					"sessionID":  "sess-1",
					"parentID":   "msg-user-7",
					"role":       "assistant",
					"providerID": "xai",
					"modelID":    "grok-code-fast-1",
					"tokens": map[string]any{
						"input":  13,
						"output": 8,
						"total":  21,
					},
				},
			})
		},
	})
	defer server.Close()

	command := writeServeScript(t, testRoot, server.URL)
	cfg := mustOpencodeConfig(t, workspaceRoot, command)
	client := New(cfg)

	sess, err := client.StartSession(workspace)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer client.StopSession(sess)

	var updates []runtime.Update
	result, err := client.RunTurn(sess, "Implement it", tracker.Issue{Identifier: "OC-7"}, runtime.RunTurnOptions{
		OnMessage: func(update runtime.Update) {
			updates = append(updates, update)
		},
	})
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if result.TurnID != "turn-1" {
		t.Fatalf("unexpected turn id: %q", result.TurnID)
	}
	tokenUpdate := assertUpdateEvent(t, updates, "turn_completed")
	payload, _ := tokenUpdate.Payload.(map[string]any)
	usage, _ := payload["usage"].(map[string]any)
	if got := mapInteger(usage["total_tokens"]); got != 21 {
		t.Fatalf("unexpected total tokens: %#v", usage)
	}
}

func TestRuntimeStartSessionUsesConfiguredPermissionRules(t *testing.T) {
	t.Parallel()

	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "OC-4")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	events := make(chan string, 1)
	var createdBody map[string]any
	server := newFakeOpencodeServer(t, events, fakeOpencodeHandlers{
		createSession: func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&createdBody); err != nil {
				t.Fatalf("decode create session: %v", err)
			}
			writeJSON(t, w, map[string]any{"id": "sess-1"})
		},
	})
	defer server.Close()

	command := writeServeScript(t, testRoot, server.URL)
	rules := []map[string]any{
		{"permission": "*", "pattern": "*", "action": "allow"},
		{"permission": "question", "pattern": "*", "action": "deny"},
	}
	cfg := mustOpencodeConfig(t, workspaceRoot, command, rules)
	client := New(cfg)

	sess, err := client.StartSession(workspace)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer client.StopSession(sess)

	gotJSON, err := json.Marshal(createdBody["permission"])
	if err != nil {
		t.Fatalf("marshal got permissions: %v", err)
	}
	wantJSON, err := json.Marshal(rules)
	if err != nil {
		t.Fatalf("marshal want permissions: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("permission rules mismatch: got=%s want=%s", gotJSON, wantJSON)
	}
}

func TestRuntimeStartSessionConfiguresLinearMCPServer(t *testing.T) {
	t.Parallel()

	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "OC-5")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	events := make(chan string, 1)
	server := newFakeOpencodeServer(t, events, fakeOpencodeHandlers{})
	defer server.Close()

	command := writeServeScript(t, testRoot, server.URL)
	cfg := mustOpencodeLinearConfig(t, workspaceRoot, command)
	client := New(cfg)

	sess, err := client.StartSession(workspace)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer client.StopSession(sess)

	runtimeSession, ok := sess.(*session)
	if !ok {
		t.Fatalf("unexpected session type: %T", sess)
	}
	if strings.TrimSpace(runtimeSession.configDir) == "" {
		t.Fatal("expected temporary opencode config dir to be created")
	}

	content, err := os.ReadFile(filepath.Join(runtimeSession.configDir, "opencode.json"))
	if err != nil {
		t.Fatalf("read generated opencode config: %v", err)
	}
	text := string(content)
	if !strings.Contains(text, `"linear"`) {
		t.Fatalf("missing linear mcp server config: %s", text)
	}
	if !strings.Contains(text, `"mcp-linear-server"`) {
		t.Fatalf("missing baton mcp command in config: %s", text)
	}
	if !strings.Contains(text, `{env:BATON_LINEAR_API_KEY}`) {
		t.Fatalf("expected config to use env var indirection for api key: %s", text)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".opencode", "tools", "linear_graphql.js")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no workspace custom tool to be installed, stat err=%v", err)
	}
}

func TestRuntimeRunTurnIncludesToolErrorDetails(t *testing.T) {
	t.Parallel()

	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "OC-6")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	events := make(chan string, 4)
	server := newFakeOpencodeServer(t, events, fakeOpencodeHandlers{
		promptAsync: func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"id": "msg-user-1"})
			events <- `{"directory":"` + workspace + `","payload":{"type":"message.part.updated","properties":{"part":{"id":"part-tool-1","sessionID":"sess-1","messageID":"msg-assistant-1","type":"tool","tool":"linear_graphql","state":{"status":"running","input":{"query":"query { viewer { id } }"}}}}}}`
			events <- `{"directory":"` + workspace + `","payload":{"type":"message.part.updated","properties":{"part":{"id":"part-tool-1","sessionID":"sess-1","messageID":"msg-assistant-1","type":"tool","tool":"linear_graphql","state":{"status":"error","input":{"query":"query { viewer { id } }"},"error":"linear_api_status: 400 body=...","metadata":{"attempt":1}}}}}}`
			events <- `{"directory":"` + workspace + `","payload":{"type":"session.status","properties":{"sessionID":"sess-1","status":{"type":"idle"}}}}`
		},
	})
	defer server.Close()

	command := writeServeScript(t, testRoot, server.URL)
	cfg := mustOpencodeConfig(t, workspaceRoot, command)
	client := New(cfg)

	sess, err := client.StartSession(workspace)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer client.StopSession(sess)

	var updates []runtime.Update
	if _, err := client.RunTurn(sess, "Inspect tool failure", tracker.Issue{Identifier: "OC-6"}, runtime.RunTurnOptions{
		OnMessage: func(update runtime.Update) {
			updates = append(updates, update)
		},
	}); err != nil {
		t.Fatalf("run turn: %v", err)
	}

	itemCompleted := assertUpdateEvent(t, updates, "item_completed")
	payload, _ := itemCompleted.Payload.(map[string]any)
	item, _ := payload["item"].(map[string]any)
	if got := stringValue(item["error"]); got != "linear_api_status: 400 body=..." {
		t.Fatalf("unexpected tool error payload: %#v", item)
	}
	input, _ := item["input"].(map[string]any)
	if got := stringValue(input["query"]); got != "query { viewer { id } }" {
		t.Fatalf("unexpected tool input payload: %#v", item)
	}
}

func TestRuntimeRunTurnFailsFastWhenStatusPollFindsAssistantError(t *testing.T) {
	t.Parallel()

	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "OC-8")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	restore := setStatusPollIntervalForTest(25 * time.Millisecond)
	defer restore()

	events := make(chan string, 2)
	server := newFakeOpencodeServer(t, events, fakeOpencodeHandlers{
		promptAsync: func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"id": "msg-user-8"})
		},
		sessionStatus: func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{})
		},
		sessionMessages: func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, []map[string]any{
				{
					"id":         "msg-assistant-8",
					"sessionID":  "sess-1",
					"parentID":   "msg-user-8",
					"role":       "assistant",
					"providerID": "xai",
					"modelID":    "grok-code-fast-1",
					"tokens": map[string]any{
						"input":  0,
						"output": 0,
						"total":  0,
					},
					"error": map[string]any{
						"data": map[string]any{
							"message": "Rate limit exceeded",
						},
						"name": "APIError",
					},
				},
			})
		},
	})
	defer server.Close()

	command := writeServeScript(t, testRoot, server.URL)
	cfg := mustOpencodeConfig(t, workspaceRoot, command)
	client := New(cfg)

	sess, err := client.StartSession(workspace)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer client.StopSession(sess)

	start := time.Now()
	_, err = client.RunTurn(sess, "Implement it", tracker.Issue{Identifier: "OC-8"}, runtime.RunTurnOptions{})
	if err == nil || !strings.Contains(err.Error(), "Rate limit exceeded") {
		t.Fatalf("expected assistant error to be surfaced, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("expected fast failure, took %s", time.Since(start))
	}
}

func TestRuntimeRunTurnFailsWhenPermissionIsAsked(t *testing.T) {
	t.Parallel()

	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "OC-2")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	events := make(chan string, 4)
	server := newFakeOpencodeServer(t, events, fakeOpencodeHandlers{
		promptAsync: func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"id": "msg-user-1"})
			events <- `{"directory":"` + workspace + `","payload":{"type":"permission.asked","properties":{"id":"per-1","sessionID":"sess-1","permission":"bash","patterns":["*"],"metadata":{}}}}`
		},
	})
	defer server.Close()

	command := writeServeScript(t, testRoot, server.URL)
	cfg := mustOpencodeConfig(t, workspaceRoot, command)
	client := New(cfg)

	sess, err := client.StartSession(workspace)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer client.StopSession(sess)

	var updates []runtime.Update
	_, err = client.RunTurn(sess, "Run a command", tracker.Issue{Identifier: "OC-2"}, runtime.RunTurnOptions{
		OnMessage: func(update runtime.Update) {
			updates = append(updates, update)
		},
	})
	if !errors.Is(err, codex.ErrApprovalRequired) {
		t.Fatalf("expected approval required, got %v", err)
	}
	assertUpdateEvent(t, updates, "approval_required")
}

func TestRuntimeRunTurnAbortsOnContextCancel(t *testing.T) {
	t.Parallel()

	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	workspace := filepath.Join(workspaceRoot, "OC-3")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	events := make(chan string, 2)
	abortCalled := make(chan struct{}, 1)
	server := newFakeOpencodeServer(t, events, fakeOpencodeHandlers{
		promptAsync: func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"id": "msg-user-1"})
		},
		abort: func(w http.ResponseWriter, r *http.Request) {
			select {
			case abortCalled <- struct{}{}:
			default:
			}
			writeJSON(t, w, true)
		},
	})
	defer server.Close()

	command := writeServeScript(t, testRoot, server.URL)
	cfg := mustOpencodeConfig(t, workspaceRoot, command)
	client := New(cfg)

	sess, err := client.StartSession(workspace)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer client.StopSession(sess)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.RunTurn(sess, "Cancel it", tracker.Issue{Identifier: "OC-3"}, runtime.RunTurnOptions{Context: ctx})
		done <- err
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for run turn to exit")
	}

	select {
	case <-abortCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected abort endpoint to be called")
	}
}

type fakeOpencodeHandlers struct {
	createSession   func(w http.ResponseWriter, r *http.Request)
	sessionStatus   func(w http.ResponseWriter, r *http.Request)
	sessionMessages func(w http.ResponseWriter, r *http.Request)
	promptAsync     func(w http.ResponseWriter, r *http.Request)
	abort           func(w http.ResponseWriter, r *http.Request)
	deleteSession   func(w http.ResponseWriter, r *http.Request)
}

func newFakeOpencodeServer(t *testing.T, events <-chan string, handlers fakeOpencodeHandlers) *httptest.Server {
	t.Helper()

	var mu sync.Mutex
	sessionID := "sess-1"

	mux := http.NewServeMux()
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if handlers.createSession != nil {
			handlers.createSession(w, r)
			return
		}
		writeJSON(t, w, map[string]any{"id": sessionID})
	})
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not implement flusher")
		}
		fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", event)
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/global/event", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not implement flusher")
		}
		fmt.Fprint(w, "data: {\"payload\":{\"type\":\"server.connected\",\"properties\":{}}}\n\n")
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", event)
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/session/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/session/")
		path = strings.TrimSuffix(path, "/")
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodGet && path == "status":
			if handlers.sessionStatus != nil {
				handlers.sessionStatus(w, r)
				return
			}
			writeJSON(t, w, map[string]any{})
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/message"):
			if handlers.sessionMessages != nil {
				handlers.sessionMessages(w, r)
				return
			}
			writeJSON(t, w, []map[string]any{})
		case strings.HasSuffix(path, "/prompt_async"):
			if handlers.promptAsync != nil {
				handlers.promptAsync(w, r)
				return
			}
			writeJSON(t, w, map[string]any{"id": "msg-user-1"})
		case strings.HasSuffix(path, "/abort"):
			if handlers.abort != nil {
				handlers.abort(w, r)
				return
			}
			writeJSON(t, w, true)
		case r.Method == http.MethodDelete && strings.TrimSuffix(path, "/") == sessionID:
			if handlers.deleteSession != nil {
				handlers.deleteSession(w, r)
				return
			}
			writeJSON(t, w, true)
		default:
			http.NotFound(w, r)
		}
	})

	return httptest.NewServer(mux)
}

func mustOpencodeConfig(t *testing.T, workspaceRoot string, command string, permission ...[]map[string]any) *config.Config {
	t.Helper()
	opencodeConfig := map[string]any{
		"command": command,
	}
	if len(permission) > 0 && len(permission[0]) > 0 {
		rules := make([]any, 0, len(permission[0]))
		for _, rule := range permission[0] {
			rules = append(rules, rule)
		}
		opencodeConfig["permission"] = rules
	}
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
				"agent_runtime": map[string]any{
					"kind":     "opencode",
					"opencode": opencodeConfig,
				},
			},
		},
	)
	if err != nil {
		t.Fatalf("FromWorkflow failed: %v", err)
	}
	return cfg
}

func mustOpencodeLinearConfig(t *testing.T, workspaceRoot string, command string) *config.Config {
	t.Helper()
	cfg, err := config.FromWorkflow(
		filepath.Join(t.TempDir(), "WORKFLOW.md"),
		&workflow.Definition{
			Config: map[string]any{
				"tracker": map[string]any{
					"kind":         "linear",
					"api_key":      "linear-token",
					"project_slug": "baton",
				},
				"workspace": map[string]any{
					"root": workspaceRoot,
				},
				"agent_runtime": map[string]any{
					"kind": "opencode",
					"opencode": map[string]any{
						"command": command,
					},
				},
			},
		},
	)
	if err != nil {
		t.Fatalf("FromWorkflow failed: %v", err)
	}
	return cfg
}

func writeServeScript(t *testing.T, dir string, baseURL string) string {
	t.Helper()
	scriptPath := filepath.Join(dir, "fake-opencode")
	content := "#!/bin/sh\n" +
		"echo 'opencode server listening on " + baseURL + "'\n" +
		"while true; do sleep 1; done\n"
	if err := os.WriteFile(scriptPath, []byte(content), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return scriptPath + " serve"
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("write json: %v", err)
	}
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

func setStatusPollIntervalForTest(interval time.Duration) func() {
	previous := sessionStatusPollInterval
	sessionStatusPollInterval = interval
	return func() {
		sessionStatusPollInterval = previous
	}
}
