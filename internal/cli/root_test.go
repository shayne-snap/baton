package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootCommandRequiresAcknowledgementFlag(t *testing.T) {
	t.Parallel()

	workflowPath := writeTestWorkflow(t)

	cmd := NewRootCommand()
	cmd.SetArgs([]string{workflowPath})
	cmd.SetContext(context.Background())

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected missing acknowledgement flag error")
	}
	if !strings.Contains(err.Error(), acknowledgementFlag) {
		t.Fatalf("expected acknowledgement error, got %v", err)
	}
}

func TestRootCommandRunsWithExplicitWorkflowPath(t *testing.T) {
	t.Parallel()

	workflowPath := writeTestWorkflow(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cmd := NewRootCommand()
	cmd.SetArgs([]string{
		"--" + acknowledgementFlag,
		workflowPath,
	})
	cmd.SetContext(ctx)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected command to exit cleanly with canceled context, got %v", err)
	}
}

func TestRootCommandRejectsEmptyExplicitLogsRoot(t *testing.T) {
	t.Parallel()

	workflowPath := writeTestWorkflow(t)
	cmd := NewRootCommand()
	cmd.SetArgs([]string{
		"--" + acknowledgementFlag,
		workflowPath,
		"--logs-root=",
	})
	cmd.SetContext(context.Background())

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected usage error for empty explicit logs root")
	}
	if !strings.Contains(err.Error(), "Usage: baton") {
		t.Fatalf("expected usage error, got %v", err)
	}
}

func TestRootCommandRejectsNegativeExplicitPort(t *testing.T) {
	t.Parallel()

	workflowPath := writeTestWorkflow(t)
	cmd := NewRootCommand()
	cmd.SetArgs([]string{
		"--" + acknowledgementFlag,
		workflowPath,
		"--port", "-1",
	})
	cmd.SetContext(context.Background())

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected usage error for negative explicit port")
	}
	if !strings.Contains(err.Error(), "Usage: baton") {
		t.Fatalf("expected usage error, got %v", err)
	}
}

func TestRootCommandRejectsMultiplePositionalWorkflowPaths(t *testing.T) {
	t.Parallel()

	workflowPath := writeTestWorkflow(t)
	otherWorkflowPath := writeTestWorkflow(t)
	cmd := NewRootCommand()
	cmd.SetArgs([]string{
		"--" + acknowledgementFlag,
		workflowPath,
		otherWorkflowPath,
	})
	cmd.SetContext(context.Background())

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected usage error for multiple workflow paths")
	}
	if !strings.Contains(err.Error(), "Usage: baton") {
		t.Fatalf("expected usage error, got %v", err)
	}
}

func writeTestWorkflow(t *testing.T) string {
	t.Helper()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	content := strings.Join([]string{
		"---",
		"tracker:",
		"  kind: memory",
		"  lifecycle:",
		"    backlog: Backlog",
		"    todo: Todo",
		"    in_progress: In Progress",
		"    human_review: In Review",
		"    merging: Merging",
		"    rework: Rework",
		"    done: Done",
		"---",
		"Prompt",
		"",
	}, "\n")
	if err := os.WriteFile(workflowPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return workflowPath
}
