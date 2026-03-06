package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"baton/internal/config"
	"baton/internal/runtime"
	"baton/internal/tracker"

	zlog "github.com/rs/zerolog/log"
)

var (
	ErrInvalidWorkspaceCWD = errors.New("invalid_workspace_cwd")
	ErrResponseTimeout     = errors.New("response_timeout")
	ErrTurnTimeout         = errors.New("turn_timeout")
	ErrPortExit            = errors.New("port_exit")
	ErrResponseError       = errors.New("response_error")
	ErrTurnFailed          = errors.New("turn_failed")
	ErrTurnCancelled       = errors.New("turn_cancelled")
	ErrTurnInputRequired   = errors.New("turn_input_required")
	ErrApprovalRequired    = errors.New("approval_required")
)

const (
	initializeID                   = 1
	threadStartID                  = 2
	turnStartID                    = 3
	commandExecutionApprovalMethod = "item/commandExecution/requestApproval"
	fileChangeApprovalMethod       = "item/fileChange/requestApproval"
	legacyExecApprovalMethod       = "execCommandApproval"
	legacyPatchApprovalMethod      = "applyPatchApproval"
	toolCallMethod                 = "item/tool/call"
	toolRequestUserInputMethod     = "item/tool/requestUserInput"
	turnCompletedMethod            = "turn/completed"
	turnFailedMethod               = "turn/failed"
	turnCancelledMethod            = "turn/cancelled"
	turnInputRequiredMethod        = "turn/input_required"
	decisionAcceptForSession       = "acceptForSession"
	decisionApprovedForSession     = "approved_for_session"
	maxStreamLogBytes              = 1_000
)

var noisyStreamLogPattern = regexp.MustCompile(`(?i)\b(error|warn|warning|failed|fatal|panic|exception)\b`)

type InvalidWorkspaceCwdError struct {
	Reason        string
	WorkspacePath string
	WorkspaceRoot string
}

func (e *InvalidWorkspaceCwdError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: reason=%s workspace=%s root=%s", ErrInvalidWorkspaceCWD, e.Reason, e.WorkspacePath, e.WorkspaceRoot)
}

func (e *InvalidWorkspaceCwdError) Unwrap() error { return ErrInvalidWorkspaceCWD }

type ResponseError struct {
	Payload any
}

func (e *ResponseError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %v", ErrResponseError, e.Payload)
}

func (e *ResponseError) Unwrap() error { return ErrResponseError }

type TurnError struct {
	Code    error
	Payload any
}

func (e *TurnError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Payload)
}

func (e *TurnError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Code
}

type PortExitError struct {
	Status int
}

func (e *PortExitError) Error() string {
	return fmt.Sprintf("%s: %d", ErrPortExit, e.Status)
}

func (e *PortExitError) Unwrap() error { return ErrPortExit }

type Update = runtime.Update
type TurnResult = runtime.TurnResult
type MessageHandler = runtime.MessageHandler
type ToolExecutor = runtime.ToolExecutor
type RunTurnOptions = runtime.RunTurnOptions

type AppServer struct {
	config       *config.Config
	toolExecutor *ToolExecutor
}

type Session struct {
	cmd               *exec.Cmd
	stdin             io.WriteCloser
	msgCh             chan incomingLine
	exitCh            chan error
	writeMu           sync.Mutex
	approvalPolicy    any
	autoApprove       bool
	threadSandbox     string
	turnSandboxPolicy map[string]any
	threadID          string
	workspace         string
	metadata          map[string]any
}

type incomingLine struct {
	raw     string
	payload map[string]any
	err     error
}

func NewAppServer(cfg *config.Config) *AppServer {
	executor := NewDynamicToolExecutor(cfg)
	var wrapped ToolExecutor = executor.Execute
	return &AppServer{
		config:       cfg,
		toolExecutor: &wrapped,
	}
}

func (a *AppServer) Run(workspace string, prompt string, issue tracker.Issue, opts RunTurnOptions) (*TurnResult, error) {
	session, err := a.StartSession(workspace)
	if err != nil {
		return nil, err
	}
	defer a.StopSession(session)
	return a.RunTurn(session, prompt, issue, opts)
}

func (a *AppServer) StartSession(workspace string) (*Session, error) {
	if err := a.validateWorkspaceCWD(workspace); err != nil {
		return nil, err
	}

	shellPath, err := exec.LookPath("bash")
	if err != nil {
		return nil, err
	}

	absWorkspace := filepath.Clean(workspace)
	cmd := exec.Command(shellPath, "-lc", a.config.CodexCommand())
	cmd.Dir = absWorkspace

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
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

	settings, err := a.config.CodexRuntimeSettings(absWorkspace)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}

	session := &Session{
		cmd:               cmd,
		stdin:             stdin,
		msgCh:             make(chan incomingLine, 256),
		exitCh:            make(chan error, 1),
		approvalPolicy:    settings.ApprovalPolicy,
		autoApprove:       settings.ApprovalPolicy == "never",
		threadSandbox:     settings.ThreadSandbox,
		turnSandboxPolicy: settings.TurnSandboxPolicy,
		workspace:         absWorkspace,
		metadata: map[string]any{
			"codex_app_server_pid": fmt.Sprintf("%d", cmd.Process.Pid),
		},
	}

	go session.readStdout(stdout)
	go session.drainStderr(stderr)
	go func() {
		session.exitCh <- cmd.Wait()
	}()

	if err := a.sendInitialize(session); err != nil {
		a.StopSession(session)
		return nil, err
	}
	if err := a.sendInitialized(session); err != nil {
		a.StopSession(session)
		return nil, err
	}

	threadID, err := a.startThread(session)
	if err != nil {
		a.StopSession(session)
		return nil, err
	}
	session.threadID = threadID
	return session, nil
}

func (a *AppServer) RunTurn(session *Session, prompt string, issue tracker.Issue, opts RunTurnOptions) (*TurnResult, error) {
	handler := opts.OnMessage
	if handler == nil {
		handler = func(Update) {}
	}
	turnContext := opts.Context
	if turnContext == nil {
		turnContext = context.Background()
	}

	executor := opts.ToolExecutor
	if executor == nil {
		executor = *a.toolExecutor
	}

	turnID, err := a.startTurn(session, prompt, issue)
	if err != nil {
		a.emit(handler, session, Update{
			Event:   "startup_failed",
			Payload: map[string]any{"reason": err.Error()},
		})
		return nil, err
	}

	sessionID := fmt.Sprintf("%s-%s", session.threadID, turnID)
	a.emit(handler, session, Update{
		Event: "session_started",
		Payload: map[string]any{
			"session_id": sessionID,
			"thread_id":  session.threadID,
			"turn_id":    turnID,
		},
	})

	turnTimeout := time.Duration(a.config.CodexTurnTimeoutMS()) * time.Millisecond
	if turnTimeout <= 0 {
		turnTimeout = time.Hour
	}

	for {
		select {
		case <-turnContext.Done():
			a.emit(handler, session, Update{
				Event:   "turn_ended_with_error",
				Payload: map[string]any{"reason": turnContext.Err().Error()},
			})
			return nil, turnContext.Err()
		case line := <-session.msgCh:
			if line.err != nil {
				logNonJSONStreamLine(line.raw, "turn stream")
				a.emit(handler, session, Update{
					Event:   "malformed",
					Payload: line.raw,
					Raw:     line.raw,
				})
				continue
			}
			payload := line.payload
			method := stringValue(payload["method"])
			if method == "" {
				continue
			}

			switch method {
			case turnCompletedMethod:
				a.emit(handler, session, Update{
					Event:   "turn_completed",
					Payload: payload,
					Raw:     line.raw,
				})
				return &TurnResult{
					Result:    "turn_completed",
					SessionID: sessionID,
					ThreadID:  session.threadID,
					TurnID:    turnID,
				}, nil

			case turnFailedMethod:
				a.emit(handler, session, Update{Event: "turn_failed", Payload: payload, Raw: line.raw})
				err := &TurnError{Code: ErrTurnFailed, Payload: payload}
				a.emit(handler, session, Update{
					Event:   "turn_ended_with_error",
					Payload: map[string]any{"session_id": sessionID, "reason": err.Error()},
				})
				return nil, err

			case turnCancelledMethod:
				a.emit(handler, session, Update{Event: "turn_cancelled", Payload: payload, Raw: line.raw})
				err := &TurnError{Code: ErrTurnCancelled, Payload: payload}
				a.emit(handler, session, Update{
					Event:   "turn_ended_with_error",
					Payload: map[string]any{"session_id": sessionID, "reason": err.Error()},
				})
				return nil, err
			}

			if needsInput(method, payload) {
				a.emit(handler, session, Update{Event: "turn_input_required", Payload: payload, Raw: line.raw})
				err := &TurnError{Code: ErrTurnInputRequired, Payload: payload}
				a.emit(handler, session, Update{
					Event:   "turn_ended_with_error",
					Payload: map[string]any{"session_id": sessionID, "reason": err.Error()},
				})
				return nil, err
			}

			switch method {
			case commandExecutionApprovalMethod:
				if !session.autoApprove {
					err := &TurnError{Code: ErrApprovalRequired, Payload: payload}
					a.emit(handler, session, Update{Event: "approval_required", Payload: payload, Raw: line.raw})
					a.emit(handler, session, Update{
						Event:   "turn_ended_with_error",
						Payload: map[string]any{"session_id": sessionID, "reason": err.Error()},
					})
					return nil, err
				}
				if err := a.replyApprovalDecision(session, payload["id"], decisionAcceptForSession); err != nil {
					return nil, err
				}
				a.emit(handler, session, Update{
					Event:    "approval_auto_approved",
					Payload:  payload,
					Raw:      line.raw,
					Decision: decisionAcceptForSession,
				})
				continue

			case fileChangeApprovalMethod:
				if !session.autoApprove {
					err := &TurnError{Code: ErrApprovalRequired, Payload: payload}
					a.emit(handler, session, Update{Event: "approval_required", Payload: payload, Raw: line.raw})
					a.emit(handler, session, Update{
						Event:   "turn_ended_with_error",
						Payload: map[string]any{"session_id": sessionID, "reason": err.Error()},
					})
					return nil, err
				}
				if err := a.replyApprovalDecision(session, payload["id"], decisionAcceptForSession); err != nil {
					return nil, err
				}
				a.emit(handler, session, Update{
					Event:    "approval_auto_approved",
					Payload:  payload,
					Raw:      line.raw,
					Decision: decisionAcceptForSession,
				})
				continue

			case legacyExecApprovalMethod, legacyPatchApprovalMethod:
				if !session.autoApprove {
					err := &TurnError{Code: ErrApprovalRequired, Payload: payload}
					a.emit(handler, session, Update{Event: "approval_required", Payload: payload, Raw: line.raw})
					a.emit(handler, session, Update{
						Event:   "turn_ended_with_error",
						Payload: map[string]any{"session_id": sessionID, "reason": err.Error()},
					})
					return nil, err
				}
				if err := a.replyApprovalDecision(session, payload["id"], decisionApprovedForSession); err != nil {
					return nil, err
				}
				a.emit(handler, session, Update{
					Event:    "approval_auto_approved",
					Payload:  payload,
					Raw:      line.raw,
					Decision: decisionApprovedForSession,
				})
				continue

			case toolRequestUserInputMethod:
				a.emit(handler, session, Update{Event: "turn_input_required", Payload: payload, Raw: line.raw})
				err := &TurnError{Code: ErrTurnInputRequired, Payload: payload}
				a.emit(handler, session, Update{
					Event:   "turn_ended_with_error",
					Payload: map[string]any{"session_id": sessionID, "reason": err.Error()},
				})
				return nil, err

			case toolCallMethod:
				toolName, arguments := parseToolCall(payload)
				result := executor(context.Background(), toolName, arguments)
				if err := a.sendMessage(session, map[string]any{
					"id":     payload["id"],
					"result": result,
				}); err != nil {
					return nil, err
				}
				if isSuccessResult(result) {
					a.emit(handler, session, Update{Event: "tool_call_completed", Payload: payload, Raw: line.raw})
				} else if strings.TrimSpace(toolName) == "" {
					a.emit(handler, session, Update{Event: "unsupported_tool_call", Payload: payload, Raw: line.raw})
				} else {
					a.emit(handler, session, Update{Event: "tool_call_failed", Payload: payload, Raw: line.raw})
				}
				continue
			}

			a.emit(handler, session, Update{Event: "notification", Payload: payload, Raw: line.raw})

		case err := <-session.exitCh:
			status := exitStatus(err)
			if status == 0 && a.consumeBufferedTurnCompletion(session, handler) {
				return &TurnResult{
					Result:    "turn_completed",
					SessionID: sessionID,
					ThreadID:  session.threadID,
					TurnID:    turnID,
				}, nil
			}
			turnErr := &PortExitError{Status: status}
			a.emit(handler, session, Update{
				Event:   "turn_ended_with_error",
				Payload: map[string]any{"session_id": sessionID, "reason": turnErr.Error()},
			})
			return nil, turnErr

		case <-time.After(turnTimeout):
			turnErr := &TurnError{Code: ErrTurnTimeout, Payload: nil}
			a.emit(handler, session, Update{
				Event:   "turn_ended_with_error",
				Payload: map[string]any{"session_id": sessionID, "reason": turnErr.Error()},
			})
			return nil, turnErr
		}
	}
}

func (a *AppServer) consumeBufferedTurnCompletion(session *Session, handler MessageHandler) bool {
	for {
		select {
		case line := <-session.msgCh:
			if line.err != nil {
				logNonJSONStreamLine(line.raw, "turn stream")
				a.emit(handler, session, Update{
					Event:   "malformed",
					Payload: line.raw,
					Raw:     line.raw,
				})
				continue
			}

			method := stringValue(line.payload["method"])
			if method != turnCompletedMethod {
				continue
			}

			a.emit(handler, session, Update{
				Event:   "turn_completed",
				Payload: line.payload,
				Raw:     line.raw,
			})
			return true
		default:
			return false
		}
	}
}

func (a *AppServer) StopSession(session *Session) {
	if session == nil || session.cmd == nil || session.cmd.Process == nil {
		return
	}
	_ = session.stdin.Close()
	_ = session.cmd.Process.Kill()
}

func (a *AppServer) sendInitialize(session *Session) error {
	payload := map[string]any{
		"method": "initialize",
		"id":     initializeID,
		"params": map[string]any{
			"capabilities": map[string]any{
				"experimentalApi": true,
			},
			"clientInfo": map[string]any{
				"name":    "baton-orchestrator",
				"title":   "Baton Orchestrator",
				"version": "0.1.0",
			},
		},
	}
	if err := a.sendMessage(session, payload); err != nil {
		return err
	}
	_, err := a.awaitResponse(session, initializeID)
	return err
}

func (a *AppServer) sendInitialized(session *Session) error {
	return a.sendMessage(session, map[string]any{
		"method": "initialized",
		"params": map[string]any{},
	})
}

func (a *AppServer) startThread(session *Session) (string, error) {
	payload := map[string]any{
		"method": "thread/start",
		"id":     threadStartID,
		"params": map[string]any{
			"approvalPolicy": session.approvalPolicy,
			"sandbox":        session.threadSandbox,
			"cwd":            filepath.Clean(session.workspace),
			"dynamicTools":   ToolSpecs(),
		},
	}
	if err := a.sendMessage(session, payload); err != nil {
		return "", err
	}

	result, err := a.awaitResponse(session, threadStartID)
	if err != nil {
		return "", err
	}
	threadPayload, _ := result["thread"].(map[string]any)
	threadID := stringValue(threadPayload["id"])
	if strings.TrimSpace(threadID) == "" {
		return "", &ResponseError{Payload: map[string]any{"invalid_thread_payload": result}}
	}
	return threadID, nil
}

func (a *AppServer) startTurn(session *Session, prompt string, issue tracker.Issue) (string, error) {
	payload := map[string]any{
		"method": "turn/start",
		"id":     turnStartID,
		"params": map[string]any{
			"threadId": session.threadID,
			"input": []any{
				map[string]any{
					"type": "text",
					"text": prompt,
				},
			},
			"cwd":            filepath.Clean(session.workspace),
			"title":          fmt.Sprintf("%s: %s", issue.Identifier, issue.Title),
			"approvalPolicy": session.approvalPolicy,
			"sandboxPolicy":  session.turnSandboxPolicy,
		},
	}
	if err := a.sendMessage(session, payload); err != nil {
		return "", err
	}

	result, err := a.awaitResponse(session, turnStartID)
	if err != nil {
		return "", err
	}
	turnPayload, _ := result["turn"].(map[string]any)
	turnID := stringValue(turnPayload["id"])
	if strings.TrimSpace(turnID) == "" {
		return "", &ResponseError{Payload: map[string]any{"invalid_turn_payload": result}}
	}
	return turnID, nil
}

func (a *AppServer) awaitResponse(session *Session, requestID int) (map[string]any, error) {
	timeout := time.Duration(a.config.CodexReadTimeoutMS()) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	for {
		select {
		case line := <-session.msgCh:
			if line.err != nil {
				logNonJSONStreamLine(line.raw, "response stream")
				continue
			}
			if !matchesRequestID(line.payload["id"], requestID) {
				continue
			}
			if responseErr, hasErr := line.payload["error"]; hasErr {
				return nil, &ResponseError{Payload: responseErr}
			}
			result, hasResult := line.payload["result"].(map[string]any)
			if !hasResult {
				return nil, &ResponseError{Payload: line.payload}
			}
			return result, nil

		case err := <-session.exitCh:
			status := exitStatus(err)
			if status == 0 {
				if result, ok, consumeErr := a.consumeBufferedResponse(session, requestID); consumeErr != nil {
					return nil, consumeErr
				} else if ok {
					return result, nil
				}
			}
			return nil, &PortExitError{Status: status}

		case <-time.After(timeout):
			return nil, &TurnError{Code: ErrResponseTimeout}
		}
	}
}

func (a *AppServer) consumeBufferedResponse(session *Session, requestID int) (map[string]any, bool, error) {
	for {
		select {
		case line := <-session.msgCh:
			if line.err != nil {
				logNonJSONStreamLine(line.raw, "response stream")
				continue
			}
			if !matchesRequestID(line.payload["id"], requestID) {
				continue
			}
			if responseErr, hasErr := line.payload["error"]; hasErr {
				return nil, true, &ResponseError{Payload: responseErr}
			}
			result, hasResult := line.payload["result"].(map[string]any)
			if !hasResult {
				return nil, true, &ResponseError{Payload: line.payload}
			}
			return result, true, nil
		default:
			return nil, false, nil
		}
	}
}

func (a *AppServer) sendMessage(session *Session, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	session.writeMu.Lock()
	defer session.writeMu.Unlock()
	_, err = session.stdin.Write(raw)
	return err
}

func (a *AppServer) validateWorkspaceCWD(workspace string) error {
	workspacePath := filepath.Clean(workspace)
	workspaceRoot := filepath.Clean(a.config.WorkspaceRoot())
	rel, err := filepath.Rel(workspaceRoot, workspacePath)
	if err != nil {
		return &InvalidWorkspaceCwdError{Reason: "outside_workspace_root", WorkspacePath: workspacePath, WorkspaceRoot: workspaceRoot}
	}
	if workspacePath == workspaceRoot {
		return &InvalidWorkspaceCwdError{Reason: "workspace_root", WorkspacePath: workspacePath, WorkspaceRoot: workspaceRoot}
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == "." {
		return &InvalidWorkspaceCwdError{Reason: "outside_workspace_root", WorkspacePath: workspacePath, WorkspaceRoot: workspaceRoot}
	}
	return nil
}

func (a *AppServer) replyApprovalDecision(session *Session, id any, decision string) error {
	return a.sendMessage(session, map[string]any{
		"id": id,
		"result": map[string]any{
			"decision": decision,
		},
	})
}

func (s *Session) readStdout(stdout io.Reader) {
	reader := bufio.NewReader(stdout)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if len(strings.TrimSpace(line)) > 0 {
				s.msgCh <- parseIncomingLine(line)
			}
			return
		}
		s.msgCh <- parseIncomingLine(line)
	}
}

func (s *Session) drainStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		// Stderr is diagnostic only; it is not part of protocol parsing.
		logNonJSONStreamLine(scanner.Text(), "turn stream")
	}
}

func logNonJSONStreamLine(data string, streamLabel string) {
	text := strings.TrimSpace(data)
	if text == "" {
		return
	}
	if len(text) > maxStreamLogBytes {
		text = text[:maxStreamLogBytes]
	}

	message := fmt.Sprintf("Codex %s output: %s", strings.TrimSpace(streamLabel), text)
	if noisyStreamLogPattern.MatchString(text) {
		zlog.Warn().Msg(message)
		return
	}
	zlog.Debug().Msg(message)
}

func parseIncomingLine(rawLine string) incomingLine {
	trimmed := strings.TrimSpace(rawLine)
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()

	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return incomingLine{raw: trimmed, err: err}
	}
	return incomingLine{raw: trimmed, payload: payload}
}

func matchesRequestID(value any, requestID int) bool {
	switch typed := value.(type) {
	case float64:
		return int(typed) == requestID
	case int:
		return typed == requestID
	case json.Number:
		parsed, err := typed.Int64()
		return err == nil && int(parsed) == requestID
	default:
		return false
	}
}

func parseToolCall(payload map[string]any) (string, any) {
	params, _ := payload["params"].(map[string]any)
	toolName := strings.TrimSpace(stringValue(params["name"]))
	if toolName == "" {
		toolName = strings.TrimSpace(stringValue(params["tool"]))
	}
	return toolName, params["arguments"]
}

func isSuccessResult(result map[string]any) bool {
	value, _ := result["success"].(bool)
	return value
}

func needsInput(method string, payload map[string]any) bool {
	if method == toolRequestUserInputMethod {
		return false
	}
	if method == turnInputRequiredMethod {
		return true
	}
	params, _ := payload["params"].(map[string]any)
	if boolFromAny(params["requiresInput"]) {
		return true
	}
	if boolFromAny(params["inputRequired"]) {
		return true
	}
	return false
}

func boolFromAny(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	default:
		return false
	}
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return ""
	}
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func (a *AppServer) emit(handler MessageHandler, session *Session, update Update) {
	if handler == nil {
		return
	}
	update.Timestamp = time.Now().UTC()
	update.AppServerPID = stringValue(session.metadata["codex_app_server_pid"])
	update.CodexAppServerPID = update.AppServerPID
	handler(update)
}
