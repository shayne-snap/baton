package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"baton/internal/config"
	"baton/internal/tracker"

	"github.com/rs/zerolog"
)

var (
	ErrWorkspaceEqualsRoot     = errors.New("workspace_equals_root")
	ErrWorkspaceOutsideRoot    = errors.New("workspace_outside_root")
	ErrWorkspaceSymlinkEscape  = errors.New("workspace_symlink_escape")
	ErrWorkspacePathUnreadable = errors.New("workspace_path_unreadable")
	ErrWorkspaceHookFailed     = errors.New("workspace_hook_failed")
	ErrWorkspaceHookTimeout    = errors.New("workspace_hook_timeout")
)

var workspaceKeySanitizer = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

type PathValidationError struct {
	Code      error
	Workspace string
	Root      string
	Component string
	Cause     error
}

func (e *PathValidationError) Error() string {
	if e == nil {
		return ""
	}
	switch {
	case errors.Is(e.Code, ErrWorkspaceSymlinkEscape):
		return fmt.Sprintf("%s: component=%s root=%s", e.Code, e.Component, e.Root)
	case e.Cause != nil:
		return fmt.Sprintf("%s: workspace=%s root=%s cause=%v", e.Code, e.Workspace, e.Root, e.Cause)
	default:
		return fmt.Sprintf("%s: workspace=%s root=%s", e.Code, e.Workspace, e.Root)
	}
}

func (e *PathValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Code
}

type HookFailedError struct {
	Hook     string
	ExitCode int
	Output   string
}

func (e *HookFailedError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: hook=%s status=%d output=%q", ErrWorkspaceHookFailed, e.Hook, e.ExitCode, e.Output)
}

func (e *HookFailedError) Unwrap() error {
	return ErrWorkspaceHookFailed
}

type HookTimeoutError struct {
	Hook      string
	TimeoutMS int
}

func (e *HookTimeoutError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: hook=%s timeout_ms=%d", ErrWorkspaceHookTimeout, e.Hook, e.TimeoutMS)
}

func (e *HookTimeoutError) Unwrap() error {
	return ErrWorkspaceHookTimeout
}

type Manager interface {
	CreateForIssue(ctx context.Context, issue tracker.Issue) (string, error)
	Remove(ctx context.Context, path string) error
	RemoveIssueWorkspaces(ctx context.Context, identifier string) error
	RunBeforeRunHook(ctx context.Context, workspace string, issue tracker.Issue) error
	RunAfterRunHook(ctx context.Context, workspace string, issue tracker.Issue)
}

type manager struct {
	config *config.Config
	logger zerolog.Logger
}

func NewManager(cfg *config.Config, logger ...zerolog.Logger) Manager {
	log := zerolog.Nop()
	if len(logger) > 0 {
		log = logger[0]
	}
	return &manager{
		config: cfg,
		logger: log,
	}
}

func (m *manager) CreateForIssue(ctx context.Context, issue tracker.Issue) (string, error) {
	workspace := filepath.Join(m.config.WorkspaceRoot(), safeIdentifier(issue.Identifier))
	if err := m.validateWorkspacePath(workspace); err != nil {
		return "", err
	}

	createdNow, err := ensureWorkspace(workspace)
	if err != nil {
		return "", err
	}

	if createdNow && strings.TrimSpace(m.config.HookAfterCreate()) != "" {
		if err := m.runHook(ctx, workspace, "after_create", m.config.HookAfterCreate(), issue); err != nil {
			return "", err
		}
	}

	return workspace, nil
}

func (m *manager) Remove(ctx context.Context, path string) error {
	_, statErr := os.Stat(path)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return os.RemoveAll(path)
		}
		return statErr
	}

	if err := m.validateWorkspacePath(path); err != nil {
		return err
	}

	if strings.TrimSpace(m.config.HookBeforeRemove()) != "" {
		_ = m.runHook(
			ctx,
			path,
			"before_remove",
			m.config.HookBeforeRemove(),
			tracker.Issue{Identifier: filepath.Base(path)},
		)
	}

	return os.RemoveAll(path)
}

func (m *manager) RemoveIssueWorkspaces(ctx context.Context, identifier string) error {
	if strings.TrimSpace(identifier) == "" {
		return nil
	}
	workspace := filepath.Join(m.config.WorkspaceRoot(), safeIdentifier(identifier))
	_ = m.Remove(ctx, workspace)
	return nil
}

func (m *manager) RunBeforeRunHook(ctx context.Context, workspace string, issue tracker.Issue) error {
	if strings.TrimSpace(m.config.HookBeforeRun()) == "" {
		return nil
	}
	return m.runHook(ctx, workspace, "before_run", m.config.HookBeforeRun(), issue)
}

func (m *manager) RunAfterRunHook(ctx context.Context, workspace string, issue tracker.Issue) {
	if strings.TrimSpace(m.config.HookAfterRun()) == "" {
		return
	}
	_ = m.runHook(ctx, workspace, "after_run", m.config.HookAfterRun(), issue)
}

func (m *manager) validateWorkspacePath(path string) error {
	expandedWorkspace, err := filepath.Abs(path)
	if err != nil {
		return &PathValidationError{Code: ErrWorkspacePathUnreadable, Workspace: path, Root: m.config.WorkspaceRoot(), Cause: err}
	}
	expandedRoot, err := filepath.Abs(m.config.WorkspaceRoot())
	if err != nil {
		return &PathValidationError{Code: ErrWorkspacePathUnreadable, Workspace: expandedWorkspace, Root: m.config.WorkspaceRoot(), Cause: err}
	}

	if expandedWorkspace == expandedRoot {
		return &PathValidationError{Code: ErrWorkspaceEqualsRoot, Workspace: expandedWorkspace, Root: expandedRoot}
	}

	rel, err := filepath.Rel(expandedRoot, expandedWorkspace)
	if err != nil {
		return &PathValidationError{Code: ErrWorkspaceOutsideRoot, Workspace: expandedWorkspace, Root: expandedRoot, Cause: err}
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return &PathValidationError{Code: ErrWorkspaceOutsideRoot, Workspace: expandedWorkspace, Root: expandedRoot}
	}

	current := expandedRoot
	for _, segment := range splitPath(rel) {
		next := filepath.Join(current, segment)
		info, lstatErr := os.Lstat(next)
		if lstatErr != nil {
			if errors.Is(lstatErr, os.ErrNotExist) {
				break
			}
			return &PathValidationError{
				Code:      ErrWorkspacePathUnreadable,
				Workspace: expandedWorkspace,
				Root:      expandedRoot,
				Component: next,
				Cause:     lstatErr,
			}
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return &PathValidationError{
				Code:      ErrWorkspaceSymlinkEscape,
				Workspace: expandedWorkspace,
				Root:      expandedRoot,
				Component: next,
			}
		}
		current = next
	}

	return nil
}

func (m *manager) runHook(parent context.Context, workspace string, hookName string, command string, issue tracker.Issue) error {
	timeoutMS := m.config.HookTimeoutMS()
	if timeoutMS <= 0 {
		timeoutMS = 60_000
	}
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" {
		issueID = "n/a"
	}
	issueIdentifier := strings.TrimSpace(issue.Identifier)
	if issueIdentifier == "" {
		issueIdentifier = "issue"
	}

	m.logger.Info().Msgf(
		"Running workspace hook hook=%s issue_id=%s issue_identifier=%s workspace=%s",
		hookName,
		issueID,
		issueIdentifier,
		workspace,
	)

	ctx, cancel := context.WithTimeout(parent, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-lc", command)
	cmd.Dir = workspace
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	err := cmd.Run()
	if err == nil {
		return nil
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		m.logger.Warn().Msgf(
			"Workspace hook timed out hook=%s issue_id=%s issue_identifier=%s workspace=%s timeout_ms=%d",
			hookName,
			issueID,
			issueIdentifier,
			workspace,
			timeoutMS,
		)
		return &HookTimeoutError{Hook: hookName, TimeoutMS: timeoutMS}
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		sanitizedOutput := sanitizeHookOutput(combined.String(), 2048)
		m.logger.Warn().Msgf(
			"Workspace hook failed hook=%s issue_id=%s issue_identifier=%s workspace=%s status=%d output=%q",
			hookName,
			issueID,
			issueIdentifier,
			workspace,
			exitErr.ExitCode(),
			sanitizedOutput,
		)
		return &HookFailedError{
			Hook:     hookName,
			ExitCode: exitErr.ExitCode(),
			Output:   sanitizedOutput,
		}
	}

	m.logger.Warn().Err(err).Msgf(
		"Workspace hook execution error hook=%s issue_id=%s issue_identifier=%s workspace=%s",
		hookName,
		issueID,
		issueIdentifier,
		workspace,
	)

	return err
}

func ensureWorkspace(workspace string) (bool, error) {
	info, err := os.Stat(workspace)
	if err == nil {
		if info.IsDir() {
			cleanTmpArtifacts(workspace)
			return false, nil
		}
		if removeErr := os.RemoveAll(workspace); removeErr != nil {
			return false, removeErr
		}
		if createErr := os.MkdirAll(workspace, 0o755); createErr != nil {
			return false, createErr
		}
		return true, nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	if createErr := os.MkdirAll(workspace, 0o755); createErr != nil {
		return false, createErr
	}
	return true, nil
}

func cleanTmpArtifacts(workspace string) {
	_ = os.RemoveAll(filepath.Join(workspace, ".elixir_ls"))
	_ = os.RemoveAll(filepath.Join(workspace, "tmp"))
}

func safeIdentifier(identifier string) string {
	if identifier == "" {
		identifier = "issue"
	}
	return workspaceKeySanitizer.ReplaceAllString(identifier, "_")
}

func sanitizeHookOutput(output string, maxBytes int) string {
	if len(output) <= maxBytes {
		return output
	}
	return output[:maxBytes] + "... (truncated)"
}

func splitPath(path string) []string {
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" && part != "." {
			filtered = append(filtered, part)
		}
	}
	return filtered
}
