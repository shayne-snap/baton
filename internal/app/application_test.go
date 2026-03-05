package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"baton/internal/logging"
)

func TestResolveWorkflowPath(t *testing.T) {
	t.Parallel()

	explicit, err := resolveWorkflowPath(" /tmp/custom.md ", func() (string, error) {
		return "/ignored", nil
	})
	if err != nil {
		t.Fatalf("resolve explicit workflow path: %v", err)
	}
	if explicit != "/tmp/custom.md" {
		t.Fatalf("unexpected explicit workflow path: %q", explicit)
	}

	defaultPath, err := resolveWorkflowPath("", func() (string, error) {
		return "/repo", nil
	})
	if err != nil {
		t.Fatalf("resolve default workflow path: %v", err)
	}
	if defaultPath != filepath.Join("/repo", "WORKFLOW.md") {
		t.Fatalf("unexpected default workflow path: %q", defaultPath)
	}

	_, err = resolveWorkflowPath("", func() (string, error) {
		return "", errors.New("cwd failed")
	})
	if err == nil {
		t.Fatal("expected cwd resolution error")
	}
}

func TestApplicationWorkflowReloadKeepsLastKnownGoodOnInvalidUpdate(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	writeWorkflow(t, workflowPath, "---\ntracker:\n  kind: memory\npolling:\n  interval_ms: 30000\nobservability:\n  dashboard_enabled: false\n---\ninitial prompt\n")

	logsRoot := t.TempDir()
	app, err := New(Options{
		WorkflowPath: workflowPath,
		LogsRoot:     logsRoot,
		Port:         -1,
		Logger:       logging.NewDefault(logging.Options{LogsRoot: logsRoot}),
	})
	if err != nil {
		t.Fatalf("new application: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- app.Run(ctx)
	}()

	assertEventually(t, 5*time.Second, func() bool {
		return app.config.PollIntervalMS() == 30_000
	})

	writeWorkflow(t, workflowPath, "---\ntracker:\n  kind: memory\npolling:\n  interval_ms: 1200\nobservability:\n  dashboard_enabled: false\n---\nupdated prompt\n")
	assertEventually(t, 8*time.Second, func() bool {
		return app.config.PollIntervalMS() == 1200 && app.config.WorkflowPrompt() == "updated prompt"
	})

	writeWorkflow(t, workflowPath, "---\ntracker: [\n---\nbroken\n")
	time.Sleep(500 * time.Millisecond)
	if got := app.config.PollIntervalMS(); got != 1200 {
		t.Fatalf("expected last known good poll interval after invalid workflow, got %d", got)
	}
	if got := app.config.WorkflowPrompt(); got != "updated prompt" {
		t.Fatalf("expected last known good prompt after invalid workflow, got %q", got)
	}

	cancel()
	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("app run returned error: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for app run to stop")
	}
}

func writeWorkflow(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
}

func assertEventually(t *testing.T, timeout time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
