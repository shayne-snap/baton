package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"baton/internal/codex"
	"baton/internal/config"
	"baton/internal/runtime"
	"baton/internal/tracker"
)

var serveReadyPattern = regexp.MustCompile(`opencode server listening on (http://[^\s]+)`)

type Runtime struct {
	config *config.Config
}

type session struct {
	cmd         *exec.Cmd
	baseURL     string
	workspace   string
	processPID  string
	sessionID   string
	httpClient  *http.Client
	eventReader io.Closer
	events      chan sseEvent
	exitCh      chan error
	permission  []map[string]any

	mu        sync.Mutex
	turnCount int
	partTypes map[string]string
	lastUsage map[string]any
	stopped   bool
	stopOnce  sync.Once
}

type sseEvent struct {
	Directory string         `json:"directory"`
	Payload   map[string]any `json:"payload"`
}

func New(cfg *config.Config) runtime.Runtime {
	return &Runtime{config: cfg}
}

func (r *Runtime) StartSession(workspace string) (runtime.Session, error) {
	if err := validateWorkspaceCWD(r.config.WorkspaceRoot(), workspace); err != nil {
		return nil, err
	}

	shellPath := "/bin/sh"
	cmd := exec.Command(shellPath, "-lc", r.config.OpencodeCommand())
	cmd.Dir = workspace

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	readyCh := make(chan string, 1)
	exitCh := make(chan error, 1)
	go readServeOutput(stdout, readyCh)
	go drainServeStderr(stderr)
	go func() {
		exitCh <- cmd.Wait()
	}()

	baseURL, err := waitForServeReady(readyCh, exitCh)
	if err != nil {
		return nil, err
	}

	sess := &session{
		cmd:        cmd,
		baseURL:    strings.TrimRight(baseURL, "/"),
		workspace:  workspace,
		processPID: fmt.Sprintf("%d", cmd.Process.Pid),
		httpClient: &http.Client{},
		events:     make(chan sseEvent, 256),
		exitCh:     exitCh,
		partTypes:  make(map[string]string),
		permission: r.opencodePermissionRules(),
	}

	if err := installLinearGraphQLTool(workspace, r.config); err != nil {
		sess.stop()
		return nil, err
	}

	sessionID, err := sess.createSession()
	if err != nil {
		sess.stop()
		return nil, err
	}
	sess.sessionID = sessionID

	stream, err := sess.connectEventStream()
	if err != nil {
		sess.stop()
		return nil, err
	}
	sess.eventReader = stream
	go sess.readEvents(stream)

	return sess, nil
}

func (r *Runtime) RunTurn(sess runtime.Session, prompt string, issue tracker.Issue, opts runtime.RunTurnOptions) (*runtime.TurnResult, error) {
	opencodeSession, ok := sess.(*session)
	if !ok || opencodeSession == nil {
		return nil, fmt.Errorf("invalid runtime session type %T", sess)
	}

	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}

	opencodeSession.resetTurnState()
	turnCount, syntheticSessionID, turnID := opencodeSession.startTurn()
	if opts.OnMessage != nil {
		opts.OnMessage(runtime.Update{
			Event:        "session_started",
			Timestamp:    time.Now().UTC(),
			AppServerPID: opencodeSession.processPID,
			Payload: map[string]any{
				"session_id":         syntheticSessionID,
				"runtime_session_id": opencodeSession.sessionID,
				"thread_id":          opencodeSession.sessionID,
				"turn_id":            turnID,
				"turn_count":         turnCount,
				"issue_id":           issue.ID,
				"issue_identifier":   issue.Identifier,
			},
		})
	}

	if err := opencodeSession.promptAsync(ctx, prompt); err != nil {
		return nil, err
	}

	for {
		select {
		case <-ctx.Done():
			opencodeSession.abortTurn(context.Background())
			return nil, ctx.Err()
		case err := <-opencodeSession.exitCh:
			if err == nil {
				return nil, fmt.Errorf("opencode server exited unexpectedly")
			}
			return nil, fmt.Errorf("opencode server exited unexpectedly: %w", err)
		case event, ok := <-opencodeSession.events:
			if !ok {
				return nil, fmt.Errorf("opencode event stream closed unexpectedly")
			}
			done, result, err := opencodeSession.handleEvent(event, syntheticSessionID, turnID, opts.OnMessage)
			if err != nil {
				return nil, err
			}
			if done {
				return result, nil
			}
		}
	}
}

func (r *Runtime) StopSession(sess runtime.Session) {
	opencodeSession, ok := sess.(*session)
	if !ok || opencodeSession == nil {
		return
	}
	opencodeSession.stop()
}

func (s *session) createSession() (string, error) {
	body := map[string]any{
		"title":      filepath.Base(s.workspace),
		"permission": s.permission,
	}
	var response struct {
		ID string `json:"id"`
	}
	if err := s.postJSON(context.Background(), http.MethodPost, "/session", body, &response); err != nil {
		return "", err
	}
	if strings.TrimSpace(response.ID) == "" {
		return "", fmt.Errorf("opencode session create returned empty id")
	}
	return response.ID, nil
}

func (r *Runtime) opencodePermissionRules() []map[string]any {
	if rules := r.config.OpencodePermissionRules(); len(rules) > 0 {
		return rules
	}
	return defaultPermissionRules()
}

func installLinearGraphQLTool(workspace string, cfg *config.Config) error {
	if cfg == nil || strings.TrimSpace(cfg.Tracker.Kind) != "linear" {
		return nil
	}
	apiKey := strings.TrimSpace(cfg.LinearAPIToken())
	if apiKey == "" {
		return nil
	}

	toolDir := filepath.Join(workspace, ".opencode", "tools")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		return err
	}

	toolPath := filepath.Join(toolDir, "linear_graphql.js")
	return os.WriteFile(toolPath, []byte(renderLinearGraphQLTool(strings.TrimSpace(cfg.LinearEndpoint()), apiKey)), 0o644)
}

func renderLinearGraphQLTool(endpoint string, apiKey string) string {
	if endpoint == "" {
		endpoint = "https://api.linear.app/graphql"
	}
	content := `import { tool } from "@opencode-ai/plugin"

const ENDPOINT = "__LINEAR_ENDPOINT__"
const API_KEY = "__LINEAR_API_KEY__"

export default tool({
  description: "Execute a raw GraphQL query or mutation against Linear using Baton's configured auth.",
  args: {
    query: tool.schema.string().describe("GraphQL query or mutation document to execute against Linear."),
    variables: tool.schema.any().optional().describe("Optional GraphQL variables object."),
  },
  async execute(args) {
    const response = await fetch(ENDPOINT, {
      method: "POST",
      headers: {
        "Authorization": API_KEY,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        query: args.query,
        variables: args.variables ?? {},
      }),
    })

    const text = await response.text()
    if (!response.ok) {
      throw new Error("linear_api_status: " + response.status + " body=" + text)
    }

    return text
  },
})
`
	content = strings.ReplaceAll(content, "__LINEAR_ENDPOINT__", jsStringLiteral(endpoint))
	content = strings.ReplaceAll(content, "__LINEAR_API_KEY__", jsStringLiteral(apiKey))
	return content
}

func jsStringLiteral(value string) string {
	replacer := strings.NewReplacer(
		`\\`, `\\\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
		"</", `<\/`,
	)
	return replacer.Replace(value)
}

func (s *session) connectEventStream() (io.ReadCloser, error) {
	req, err := s.newRequest(context.Background(), http.MethodGet, "/global/event", nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, fmt.Errorf("opencode global event stream returned status %s", resp.Status)
	}
	return resp.Body, nil
}

func (s *session) promptAsync(ctx context.Context, prompt string) error {
	body := map[string]any{
		"parts": []map[string]any{
			{
				"type": "text",
				"text": prompt,
			},
		},
	}
	path := fmt.Sprintf("/session/%s/prompt_async", s.sessionID)
	return s.postJSON(ctx, http.MethodPost, path, body, nil)
}

func (s *session) abortTurn(ctx context.Context) {
	path := fmt.Sprintf("/session/%s/abort", s.sessionID)
	_ = s.postJSON(ctx, http.MethodPost, path, map[string]any{}, nil)
}

func (s *session) deleteSession(ctx context.Context) {
	path := fmt.Sprintf("/session/%s", s.sessionID)
	_ = s.postJSON(ctx, http.MethodDelete, path, nil, nil)
}

func (s *session) stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()

		if strings.TrimSpace(s.sessionID) != "" {
			s.abortTurn(context.Background())
			s.deleteSession(context.Background())
		}
		if s.eventReader != nil {
			_ = s.eventReader.Close()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-s.exitCh:
			case <-time.After(500 * time.Millisecond):
				_ = s.cmd.Process.Kill()
				select {
				case <-s.exitCh:
				case <-time.After(500 * time.Millisecond):
				}
			}
		}
	})
}

func (s *session) startTurn() (int, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turnCount++
	turnID := fmt.Sprintf("turn-%d", s.turnCount)
	return s.turnCount, fmt.Sprintf("%s-%s", s.sessionID, turnID), turnID
}

func (s *session) resetTurnState() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.partTypes = make(map[string]string)
	s.lastUsage = nil
}

func (s *session) handleEvent(
	event sseEvent,
	syntheticSessionID string,
	turnID string,
	handler runtime.MessageHandler,
) (bool, *runtime.TurnResult, error) {
	payload := event.Payload
	if len(payload) == 0 {
		return false, nil, nil
	}
	eventType := stringValue(payload["type"])
	properties, _ := payload["properties"].(map[string]any)

	switch eventType {
	case "server.connected", "server.heartbeat":
		return false, nil, nil
	case "message.updated":
		if !sameSession(properties, s.sessionID) {
			return false, nil, nil
		}
		info, _ := properties["info"].(map[string]any)
		if stringValue(info["role"]) != "assistant" {
			return false, nil, nil
		}
		usage := assistantUsage(info)
		s.mu.Lock()
		s.lastUsage = usage
		s.mu.Unlock()
		emitUpdate(handler, s.processPID, "token_count", map[string]any{
			"tokenUsage": map[string]any{
				"total": usage,
			},
			"runtime": "opencode",
		})
	case "message.part.updated":
		part, _ := properties["part"].(map[string]any)
		if !sameSession(part, s.sessionID) {
			return false, nil, nil
		}
		partID := stringValue(part["id"])
		partType := stringValue(part["type"])
		s.mu.Lock()
		if partID != "" && partType != "" {
			s.partTypes[partID] = partType
		}
		s.mu.Unlock()

		if partType == "tool" {
			state, _ := part["state"].(map[string]any)
			status := stringValue(state["status"])
			if status == "running" {
				emitUpdate(handler, s.processPID, "item_started", map[string]any{
					"item": map[string]any{
						"type": "tool",
						"id":   partID,
						"tool": stringValue(part["tool"]),
					},
					"runtime": "opencode",
				})
			}
			if status == "completed" || status == "error" {
				emitUpdate(handler, s.processPID, "item_completed", map[string]any{
					"item": map[string]any{
						"type":   "tool",
						"id":     partID,
						"tool":   stringValue(part["tool"]),
						"status": status,
					},
					"runtime": "opencode",
				})
			}
		}
	case "message.part.delta":
		if stringValue(properties["sessionID"]) != s.sessionID {
			return false, nil, nil
		}
		partID := stringValue(properties["partID"])
		field := stringValue(properties["field"])
		delta := stringValue(properties["delta"])
		s.mu.Lock()
		partType := s.partTypes[partID]
		s.mu.Unlock()
		switch {
		case partType == "reasoning" && field == "text":
			emitUpdate(handler, s.processPID, "agent_reasoning_delta", map[string]any{
				"delta":   delta,
				"part_id": partID,
				"runtime": "opencode",
			})
		case (partType == "text" || partType == "") && field == "text":
			emitUpdate(handler, s.processPID, "agent_message_delta", map[string]any{
				"delta":   delta,
				"part_id": partID,
				"runtime": "opencode",
			})
		default:
			emitUpdate(handler, s.processPID, "notification", map[string]any{
				"method": "message.part.delta",
				"params": properties,
			})
		}
	case "permission.asked":
		if !sameSession(properties, s.sessionID) {
			return false, nil, nil
		}
		emitUpdate(handler, s.processPID, "approval_required", map[string]any{
			"request_id": properties["id"],
			"permission": properties["permission"],
			"patterns":   properties["patterns"],
			"metadata":   properties["metadata"],
			"runtime":    "opencode",
		})
		return false, nil, &codex.TurnError{Code: codex.ErrApprovalRequired, Payload: properties}
	case "question.asked":
		if !sameSession(properties, s.sessionID) {
			return false, nil, nil
		}
		emitUpdate(handler, s.processPID, "turn_input_required", map[string]any{
			"request_id": properties["id"],
			"questions":  properties["questions"],
			"runtime":    "opencode",
		})
		return false, nil, &codex.TurnError{Code: codex.ErrTurnInputRequired, Payload: properties}
	case "session.error":
		if !sameSession(properties, s.sessionID) {
			return false, nil, nil
		}
		emitUpdate(handler, s.processPID, "notification", map[string]any{
			"method": "session.error",
			"params": properties,
		})
		return false, nil, fmt.Errorf("opencode session error: %s", sessionErrorMessage(properties))
	case "session.status":
		if !sameSession(properties, s.sessionID) {
			return false, nil, nil
		}
		status, _ := properties["status"].(map[string]any)
		if stringValue(status["type"]) != "idle" {
			return false, nil, nil
		}
		s.mu.Lock()
		usage := cloneMap(s.lastUsage)
		s.mu.Unlock()
		completedPayload := map[string]any{
			"method": "turn_completed",
			"usage":  usage,
			"params": map[string]any{
				"session_id":         syntheticSessionID,
				"runtime_session_id": s.sessionID,
				"thread_id":          s.sessionID,
				"turn_id":            turnID,
				"usage":              usage,
				"runtime":            "opencode",
			},
		}
		emitUpdate(handler, s.processPID, "turn_completed", completedPayload)
		return true, &runtime.TurnResult{
			SessionID: syntheticSessionID,
			ThreadID:  s.sessionID,
			TurnID:    turnID,
			Result:    completedPayload,
		}, nil
	}

	return false, nil, nil
}

func (s *session) readEvents(stream io.Reader) {
	defer close(s.events)
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lines []string

	flush := func() {
		if len(lines) == 0 {
			return
		}
		var dataLines []string
		for _, line := range lines {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		lines = nil
		if len(dataLines) == 0 {
			return
		}
		var event sseEvent
		if err := json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &event); err != nil {
			return
		}
		s.events <- event
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		lines = append(lines, line)
	}
	flush()
}

func (s *session) newRequest(ctx context.Context, method string, path string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Opencode-Directory", s.workspace)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (s *session) postJSON(ctx context.Context, method string, path string, body any, into any) error {
	req, err := s.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("opencode request %s %s failed: %s %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if into == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func waitForServeReady(readyCh <-chan string, exitCh <-chan error) (string, error) {
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case baseURL := <-readyCh:
			if strings.TrimSpace(baseURL) == "" {
				continue
			}
			return baseURL, nil
		case err := <-exitCh:
			if err == nil {
				return "", fmt.Errorf("opencode server exited before startup")
			}
			return "", fmt.Errorf("opencode server exited before startup: %w", err)
		case <-timeout.C:
			return "", fmt.Errorf("timed out waiting for opencode server startup")
		}
	}
}

func readServeOutput(stdout io.Reader, readyCh chan<- string) {
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		match := serveReadyPattern.FindStringSubmatch(line)
		if len(match) == 2 {
			readyCh <- match[1]
		}
	}
}

func drainServeStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
	}
}

func validateWorkspaceCWD(workspaceRoot string, workspace string) error {
	workspacePath := filepath.Clean(workspace)
	rootPath := filepath.Clean(workspaceRoot)
	rel, err := filepath.Rel(rootPath, workspacePath)
	if err != nil {
		return &codex.InvalidWorkspaceCwdError{Reason: "outside_workspace_root", WorkspacePath: workspacePath, WorkspaceRoot: rootPath}
	}
	if workspacePath == rootPath {
		return &codex.InvalidWorkspaceCwdError{Reason: "workspace_root", WorkspacePath: workspacePath, WorkspaceRoot: rootPath}
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return &codex.InvalidWorkspaceCwdError{Reason: "outside_workspace_root", WorkspacePath: workspacePath, WorkspaceRoot: rootPath}
	}
	return nil
}

func defaultPermissionRules() []map[string]any {
	return []map[string]any{
		{"permission": "*", "pattern": "*", "action": "allow"},
		{"permission": "external_directory", "pattern": "*", "action": "deny"},
		{"permission": "question", "pattern": "*", "action": "deny"},
		{"permission": "plan_enter", "pattern": "*", "action": "deny"},
		{"permission": "plan_exit", "pattern": "*", "action": "deny"},
	}
}

func assistantUsage(info map[string]any) map[string]any {
	tokens, _ := info["tokens"].(map[string]any)
	usage := map[string]any{
		"input_tokens":  mapInteger(tokens["input"]),
		"output_tokens": mapInteger(tokens["output"]),
		"total_tokens":  mapInteger(firstNonNil(tokens["total"], sumTokenFields(tokens))),
	}
	if usage["total_tokens"].(int) < 0 {
		usage["total_tokens"] = maxInt(0, usage["input_tokens"].(int)+usage["output_tokens"].(int))
	}
	return usage
}

func sumTokenFields(tokens map[string]any) any {
	if len(tokens) == 0 {
		return nil
	}
	input := mapInteger(tokens["input"])
	output := mapInteger(tokens["output"])
	if input < 0 && output < 0 {
		return nil
	}
	return maxInt(0, input) + maxInt(0, output)
}

func mapInteger(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		i, err := v.Int64()
		if err == nil {
			return int(i)
		}
	}
	return -1
}

func emitUpdate(handler runtime.MessageHandler, pid string, event string, payload map[string]any) {
	if handler == nil {
		return
	}
	handler(runtime.Update{
		Event:        event,
		Timestamp:    time.Now().UTC(),
		AppServerPID: pid,
		Payload:      payload,
	})
}

func sameSession(payload map[string]any, sessionID string) bool {
	if payload == nil {
		return false
	}
	for _, key := range []string{"sessionID", "id"} {
		if value, ok := payload[key]; ok {
			if stringValue(value) == sessionID {
				return true
			}
		}
	}
	if info, ok := payload["info"].(map[string]any); ok {
		if stringValue(info["sessionID"]) == sessionID {
			return true
		}
	}
	if part, ok := payload["part"].(map[string]any); ok {
		if stringValue(part["sessionID"]) == sessionID {
			return true
		}
	}
	return stringValue(payload["sessionID"]) == sessionID
}

func sessionErrorMessage(payload map[string]any) string {
	errPayload, _ := payload["error"].(map[string]any)
	if data, ok := errPayload["data"].(map[string]any); ok {
		if message := stringValue(data["message"]); strings.TrimSpace(message) != "" {
			return message
		}
	}
	if name := stringValue(errPayload["name"]); strings.TrimSpace(name) != "" {
		return name
	}
	return "unknown session error"
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	default:
		return fmt.Sprintf("%v", value)
	}
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func cloneMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return map[string]any{}
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func maxInt(a int, b int) int {
	if a > b {
		return a
	}
	return b
}
