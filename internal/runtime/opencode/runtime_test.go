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

func TestRuntimeStartSessionInstallsLinearGraphQLTool(t *testing.T) {
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

	content, err := os.ReadFile(filepath.Join(workspace, ".opencode", "tools", "linear_graphql.js"))
	if err != nil {
		t.Fatalf("read installed tool: %v", err)
	}
	text := string(content)
	if !strings.Contains(text, `const ENDPOINT = "https://api.linear.app/graphql"`) {
		t.Fatalf("missing endpoint in tool: %s", text)
	}
	if !strings.Contains(text, `const API_KEY = "linear-token"`) {
		t.Fatalf("missing api key in tool: %s", text)
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
	createSession func(w http.ResponseWriter, r *http.Request)
	promptAsync   func(w http.ResponseWriter, r *http.Request)
	abort         func(w http.ResponseWriter, r *http.Request)
	deleteSession func(w http.ResponseWriter, r *http.Request)
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
