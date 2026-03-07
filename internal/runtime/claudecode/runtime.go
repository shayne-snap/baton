package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"baton/internal/codex"
	"baton/internal/config"
	"baton/internal/runtime"
	"baton/internal/tracker"
)

const (
	trackerMCPServerName = "baton-tracker"
	defaultLinearAPIURL  = "https://api.linear.app/graphql"
)

var defaultAllowedTools = []string{
	"Bash",
	"Glob",
	"Grep",
	"LS",
	"Read",
	"Edit",
	"Write",
	"MultiEdit",
}

var trackerAllowedTools = []string{
	"mcp__baton-tracker__tracker_get_issue",
	"mcp__baton-tracker__tracker_update_state",
	"mcp__baton-tracker__tracker_upsert_workpad_comment",
	"mcp__baton-tracker__tracker_add_link",
}

type Runtime struct {
	config *config.Config
}

type session struct {
	workspace     string
	sessionID     string
	configDir     string
	mcpConfigPath string

	mu        sync.Mutex
	turnCount int
	stopped   bool
}

func New(cfg *config.Config) runtime.Runtime {
	return &Runtime{config: cfg}
}

func (r *Runtime) StartSession(workspace string) (runtime.Session, error) {
	if err := validateWorkspaceCWD(r.config.WorkspaceRoot(), workspace); err != nil {
		return nil, err
	}

	sessionID, err := newUUID()
	if err != nil {
		return nil, err
	}

	configDir, mcpConfigPath, err := prepareClaudeMCPConfig(r.config)
	if err != nil {
		return nil, err
	}

	return &session{
		workspace:     workspace,
		sessionID:     sessionID,
		configDir:     configDir,
		mcpConfigPath: mcpConfigPath,
	}, nil
}

func (r *Runtime) RunTurn(sess runtime.Session, prompt string, issue tracker.Issue, opts runtime.RunTurnOptions) (*runtime.TurnResult, error) {
	claudeSession, ok := sess.(*session)
	if !ok || claudeSession == nil {
		return nil, fmt.Errorf("invalid runtime session type %T", sess)
	}

	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}

	turnCount, turnID, syntheticSessionID := claudeSession.startTurn()
	commandLine := buildClaudeCommand(r.config, claudeSession, turnCount)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-lc", commandLine)
	cmd.Dir = claudeSession.workspace
	cmd.Stdin = strings.NewReader(prompt)

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

	processPID := ""
	if cmd.Process != nil {
		processPID = strconv.Itoa(cmd.Process.Pid)
	}

	emitUpdate(opts.OnMessage, processPID, "session_started", map[string]any{
		"session_id":         syntheticSessionID,
		"runtime_session_id": claudeSession.sessionID,
		"thread_id":          claudeSession.sessionID,
		"turn_id":            turnID,
		"turn_count":         turnCount,
		"issue_id":           issue.ID,
		"issue_identifier":   issue.Identifier,
	})

	var stderrBuf bytes.Buffer
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&stderrBuf, stderr)
		close(stderrDone)
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var result *runtime.TurnResult
	var turnErr error

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			emitMalformed(opts.OnMessage, processPID, line)
			continue
		}

		nextResult, nextErr := handleClaudeEvent(opts.OnMessage, processPID, payload, line, syntheticSessionID, claudeSession.sessionID, turnID)
		if nextResult != nil && result == nil {
			result = nextResult
		}
		if nextErr != nil && turnErr == nil {
			turnErr = nextErr
		}
	}

	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	<-stderrDone

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, scanErr
	}
	if turnErr != nil {
		return nil, turnErr
	}
	if waitErr != nil {
		return nil, fmt.Errorf("claude command failed: %w: %s", waitErr, strings.TrimSpace(stderrBuf.String()))
	}
	if result != nil {
		return result, nil
	}

	stderrText := strings.TrimSpace(stderrBuf.String())
	if stderrText == "" {
		stderrText = "claude command exited without a result event"
	}
	return nil, fmt.Errorf("%s", stderrText)
}

func (r *Runtime) StopSession(sess runtime.Session) {
	claudeSession, ok := sess.(*session)
	if !ok || claudeSession == nil {
		return
	}
	claudeSession.stop()
}

func (s *session) startTurn() (int, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.turnCount++
	turnCount := s.turnCount
	turnID := fmt.Sprintf("turn-%d", turnCount)
	return turnCount, turnID, fmt.Sprintf("%s-%s", s.sessionID, turnID)
}

func (s *session) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	if strings.TrimSpace(s.configDir) != "" {
		_ = os.RemoveAll(s.configDir)
	}
}

func buildClaudeCommand(cfg *config.Config, sess *session, turnCount int) string {
	args := []string{
		"-p",
		"--verbose",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--permission-mode", cfg.ClaudeCodePermissionMode(),
	}

	if turnCount <= 1 {
		args = append(args, "--session-id", sess.sessionID)
	} else {
		args = append(args, "--resume", sess.sessionID)
	}

	if !cfg.ClaudeCodeSessionPersistence() {
		args = append(args, "--no-session-persistence")
	}
	if model := strings.TrimSpace(cfg.ClaudeCodeModel()); model != "" {
		args = append(args, "--model", model)
	}
	if prompt := strings.TrimSpace(cfg.ClaudeCodeAppendSystemPrompt()); prompt != "" {
		args = append(args, "--append-system-prompt", prompt)
	}
	if strings.TrimSpace(sess.mcpConfigPath) != "" {
		args = append(args, "--mcp-config", sess.mcpConfigPath)
		if cfg.ClaudeCodeMCPStrict() {
			args = append(args, "--strict-mcp-config")
		}
	}
	for _, tool := range claudeAllowedTools(cfg, sess.mcpConfigPath != "") {
		args = append(args, "--allowedTools", tool)
	}
	for _, tool := range cfg.ClaudeCodeDisallowedTools() {
		trimmed := strings.TrimSpace(tool)
		if trimmed == "" {
			continue
		}
		args = append(args, "--disallowedTools", trimmed)
	}

	parts := make([]string, 0, len(args)+1)
	parts = append(parts, strings.TrimSpace(cfg.ClaudeCodeCommand()))
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func claudeAllowedTools(cfg *config.Config, includeTrackerTools bool) []string {
	if configured := cfg.ClaudeCodeAllowedTools(); len(configured) > 0 {
		return uniqueNonEmptyStrings(configured)
	}
	tools := append([]string{}, defaultAllowedTools...)
	if includeTrackerTools {
		tools = append(tools, trackerAllowedTools...)
	}
	return uniqueNonEmptyStrings(tools)
}

func prepareClaudeMCPConfig(cfg *config.Config) (string, string, error) {
	if cfg == nil {
		return "", "", nil
	}

	trackerKind := strings.ToLower(strings.TrimSpace(cfg.TrackerKind()))
	if trackerKind != "linear" && trackerKind != "jira" {
		return "", "", nil
	}

	env := map[string]string{
		"BATON_TRACKER_KIND":     trackerKind,
		"BATON_TRACKER_ASSIGNEE": strings.TrimSpace(cfg.TrackerAssignee()),
	}

	switch trackerKind {
	case "linear":
		apiKey := strings.TrimSpace(cfg.LinearAPIToken())
		if apiKey == "" {
			return "", "", nil
		}
		env["BATON_LINEAR_API_KEY"] = apiKey
		env["BATON_LINEAR_ENDPOINT"] = linearEndpointOrDefault(strings.TrimSpace(cfg.LinearEndpoint()))
	case "jira":
		baseURL := strings.TrimSpace(cfg.JiraBaseURL())
		projectKey := strings.TrimSpace(cfg.JiraProjectKey())
		email := strings.TrimSpace(cfg.JiraEmail())
		apiToken := strings.TrimSpace(cfg.JiraAPIToken())
		if baseURL == "" || projectKey == "" || email == "" || apiToken == "" {
			return "", "", nil
		}
		env["BATON_JIRA_BASE_URL"] = baseURL
		env["BATON_JIRA_PROJECT_KEY"] = projectKey
		env["BATON_JIRA_JQL"] = strings.TrimSpace(cfg.JiraJQL())
		env["BATON_JIRA_AUTH_TYPE"] = strings.TrimSpace(cfg.JiraAuthType())
		env["BATON_JIRA_EMAIL"] = email
		env["BATON_JIRA_API_TOKEN"] = apiToken
	}

	configDir, err := os.MkdirTemp("", "baton-claudecode-*")
	if err != nil {
		return "", "", err
	}
	cleanup := func(err error) (string, string, error) {
		_ = os.RemoveAll(configDir)
		return "", "", err
	}

	executable, err := os.Executable()
	if err != nil {
		return cleanup(err)
	}

	payload := map[string]any{
		"mcpServers": map[string]any{
			trackerMCPServerName: map[string]any{
				"type":    "stdio",
				"command": filepath.Clean(executable),
				"args":    []any{"mcp-tracker-server"},
				"env":     env,
			},
		},
	}

	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return cleanup(err)
	}

	configPath := filepath.Join(configDir, "mcp.json")
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		return cleanup(err)
	}
	return configDir, configPath, nil
}

func handleClaudeEvent(
	handler runtime.MessageHandler,
	processPID string,
	payload map[string]any,
	raw string,
	syntheticSessionID string,
	threadID string,
	turnID string,
) (*runtime.TurnResult, error) {
	eventType := strings.TrimSpace(stringValue(payload["type"]))
	switch eventType {
	case "assistant":
		if text := extractClaudeText(payload); text != "" {
			emitWrapperDelta(handler, processPID, "agent_message_content_delta", text, raw)
		}
	case "stream_event":
		if handled := emitClaudeStreamEvent(handler, processPID, payload, raw); handled {
			return nil, nil
		}
		if text := extractClaudeText(payload); text != "" {
			emitWrapperDelta(handler, processPID, "agent_message_content_delta", text, raw)
		}
		emitUpdate(handler, processPID, "notification", payload)
	case "system":
		emitUpdate(handler, processPID, "notification", payload)
	case "result":
		usage := extractClaudeUsage(payload)
		if len(usage) > 0 {
			emitWrapperUsage(handler, processPID, usage, raw)
		}
		if isClaudeErrorResult(payload) {
			eventPayload := map[string]any{
				"method":     "turn_failed",
				"session_id": syntheticSessionID,
				"thread_id":  threadID,
				"turn_id":    turnID,
				"usage":      usage,
				"result":     payload,
			}
			emitUpdate(handler, processPID, "turn_failed", eventPayload)
			return nil, fmt.Errorf("claude turn failed: %s", summarizeClaudeError(payload))
		}

		resultPayload := map[string]any{
			"method":     "turn_completed",
			"session_id": syntheticSessionID,
			"thread_id":  threadID,
			"turn_id":    turnID,
			"usage":      usage,
			"result":     payload,
		}
		emitUpdate(handler, processPID, "turn_completed", resultPayload)
		return &runtime.TurnResult{
			Result:    payload,
			SessionID: syntheticSessionID,
			ThreadID:  threadID,
			TurnID:    turnID,
		}, nil
	default:
		if text := extractClaudeText(payload); text != "" {
			emitWrapperDelta(handler, processPID, "agent_message_content_delta", text, raw)
		} else {
			emitUpdate(handler, processPID, "notification", payload)
		}
	}

	return nil, nil
}

func emitClaudeStreamEvent(handler runtime.MessageHandler, processPID string, payload map[string]any, raw string) bool {
	eventName := strings.ToLower(strings.TrimSpace(firstNonEmpty(
		stringValue(payload["subtype"]),
		stringValue(payload["event"]),
		stringValue(mapPath(payload, "event", "type")),
		stringValue(mapPath(payload, "delta", "type")),
	)))

	if eventName == "" {
		return false
	}
	if strings.Contains(eventName, "content_block_delta") || strings.Contains(eventName, "text_delta") || strings.Contains(eventName, "message_delta") {
		if text := extractClaudeText(payload); text != "" {
			emitWrapperDelta(handler, processPID, "agent_message_content_delta", text, raw)
			return true
		}
	}
	if strings.Contains(eventName, "mcp") || strings.Contains(eventName, "tool_use") || strings.Contains(eventName, "tool_call") {
		suffix := "mcp_tool_call_begin"
		if strings.Contains(eventName, "stop") || strings.Contains(eventName, "end") || strings.Contains(eventName, "complete") || strings.Contains(eventName, "result") {
			suffix = "mcp_tool_call_end"
		}
		emitWrapperEvent(handler, processPID, suffix, payload, raw)
		return true
	}
	if strings.Contains(eventName, "bash") || strings.Contains(eventName, "command") || strings.Contains(eventName, "shell") {
		suffix := "exec_command_begin"
		if strings.Contains(eventName, "stop") || strings.Contains(eventName, "end") || strings.Contains(eventName, "complete") || strings.Contains(eventName, "result") {
			suffix = "exec_command_end"
		}
		emitWrapperEvent(handler, processPID, suffix, payload, raw)
		return true
	}
	return false
}

func emitMalformed(handler runtime.MessageHandler, processPID string, raw string) {
	if handler == nil {
		return
	}
	handler(runtime.Update{
		Event:        "malformed",
		Timestamp:    time.Now().UTC(),
		AppServerPID: processPID,
		Raw:          raw,
		Payload:      raw,
	})
}

func emitWrapperDelta(handler runtime.MessageHandler, processPID string, suffix string, text string, raw string) {
	emitUpdateRaw(handler, processPID, "codex/event/"+suffix, map[string]any{
		"params": map[string]any{
			"msg": map[string]any{
				"payload": map[string]any{
					"content": text,
				},
			},
		},
	}, raw)
}

func emitWrapperUsage(handler runtime.MessageHandler, processPID string, usage map[string]any, raw string) {
	emitUpdateRaw(handler, processPID, "codex/event/token_count", map[string]any{
		"params": map[string]any{
			"msg": map[string]any{
				"payload": map[string]any{
					"info": map[string]any{
						"total_token_usage": usage,
					},
				},
			},
		},
	}, raw)
}

func emitWrapperEvent(handler runtime.MessageHandler, processPID string, suffix string, payload map[string]any, raw string) {
	emitUpdateRaw(handler, processPID, "codex/event/"+suffix, map[string]any{
		"params": map[string]any{
			"msg": payload,
		},
	}, raw)
}

func emitUpdate(handler runtime.MessageHandler, processPID string, event string, payload any) {
	emitUpdateRaw(handler, processPID, event, payload, "")
}

func emitUpdateRaw(handler runtime.MessageHandler, processPID string, event string, payload any, raw string) {
	if handler == nil {
		return
	}
	handler(runtime.Update{
		Event:        event,
		Timestamp:    time.Now().UTC(),
		AppServerPID: processPID,
		Payload:      payload,
		Raw:          raw,
	})
}

func extractClaudeUsage(payload map[string]any) map[string]any {
	usage := normalizeUsageMap(mapOf(payload["usage"]))
	if len(usage) > 0 {
		return usage
	}

	modelUsage := mapOf(payload["modelUsage"])
	if len(modelUsage) == 0 {
		return nil
	}
	total := map[string]int{
		"input_tokens":  0,
		"output_tokens": 0,
		"total_tokens":  0,
	}
	found := false
	for _, value := range modelUsage {
		next := normalizeUsageMap(mapOf(value))
		if len(next) == 0 {
			continue
		}
		found = true
		total["input_tokens"] += integerValue(next["input_tokens"])
		total["output_tokens"] += integerValue(next["output_tokens"])
		total["total_tokens"] += integerValue(next["total_tokens"])
	}
	if !found {
		return nil
	}
	if total["total_tokens"] <= 0 {
		total["total_tokens"] = total["input_tokens"] + total["output_tokens"]
	}
	return map[string]any{
		"input_tokens":  total["input_tokens"],
		"output_tokens": total["output_tokens"],
		"total_tokens":  total["total_tokens"],
	}
}

func normalizeUsageMap(input map[string]any) map[string]any {
	if len(input) == 0 {
		return nil
	}

	in := integerValue(firstNonNil(input["input_tokens"], input["prompt_tokens"], input["inputTokens"], input["promptTokens"]))
	out := integerValue(firstNonNil(input["output_tokens"], input["completion_tokens"], input["outputTokens"], input["completionTokens"]))
	total := integerValue(firstNonNil(input["total_tokens"], input["total"], input["totalTokens"]))
	if total <= 0 {
		total = maxInt(0, in) + maxInt(0, out)
	}
	if in < 0 && out < 0 && total <= 0 {
		return nil
	}
	return map[string]any{
		"input_tokens":  maxInt(0, in),
		"output_tokens": maxInt(0, out),
		"total_tokens":  maxInt(0, total),
	}
}

func isClaudeErrorResult(payload map[string]any) bool {
	if flag, ok := payload["is_error"].(bool); ok && flag {
		return true
	}
	subtype := strings.TrimSpace(stringValue(payload["subtype"]))
	return strings.HasPrefix(subtype, "error_")
}

func summarizeClaudeError(payload map[string]any) string {
	var parts []string
	if subtype := strings.TrimSpace(stringValue(payload["subtype"])); subtype != "" {
		parts = append(parts, subtype)
	}
	if errorsRaw, ok := payload["errors"].([]any); ok {
		for _, value := range errorsRaw {
			text := strings.TrimSpace(stringValue(value))
			if text == "" {
				text = strings.TrimSpace(stringValue(mapPath(value, "message")))
			}
			if text != "" {
				parts = append(parts, text)
			}
		}
	}
	if len(parts) == 0 {
		return "unknown error"
	}
	return strings.Join(parts, ": ")
}

func extractClaudeText(value any) string {
	text := strings.TrimSpace(strings.Join(collectClaudeText(value), ""))
	if text == "" {
		return ""
	}
	return text
}

func collectClaudeText(value any) []string {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{typed}
	case []any:
		var parts []string
		for _, item := range typed {
			parts = append(parts, collectClaudeText(item)...)
		}
		return parts
	case map[string]any:
		if strings.EqualFold(stringValue(typed["type"]), "tool_use") || strings.EqualFold(stringValue(typed["type"]), "tool_result") {
			return nil
		}
		var parts []string
		for _, key := range []string{"text", "delta", "content", "message"} {
			if child, ok := typed[key]; ok {
				parts = append(parts, collectClaudeText(child)...)
			}
		}
		return parts
	default:
		return nil
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

func linearEndpointOrDefault(endpoint string) string {
	if strings.TrimSpace(endpoint) == "" {
		return defaultLinearAPIURL
	}
	return endpoint
}

func newUUID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		data[0:4],
		data[4:6],
		data[6:8],
		data[8:10],
		data[10:16],
	), nil
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func uniqueNonEmptyStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

func mapPath(value any, path ...string) any {
	current := value
	for _, key := range path {
		mapped, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = mapped[key]
	}
	return current
}

func mapOf(value any) map[string]any {
	if mapped, ok := value.(map[string]any); ok {
		return mapped
	}
	return nil
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return fmt.Sprintf("%v", value)
	}
}

func integerValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return int(parsed)
		}
	}
	return -1
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func maxInt(left int, right int) int {
	if left > right {
		return left
	}
	return right
}
