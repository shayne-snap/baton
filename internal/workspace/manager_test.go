package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"baton/internal/config"
	"baton/internal/tracker"
	"baton/internal/workflow"
)

func TestWorkspacePathDeterministicPerIssueIdentifier(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "workspaces")
	manager := mustManager(t, root, map[string]any{})
	issue := tracker.Issue{Identifier: "MT/Det"}

	first, err := manager.CreateForIssue(context.Background(), issue)
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}
	second, err := manager.CreateForIssue(context.Background(), issue)
	if err != nil {
		t.Fatalf("second create failed: %v", err)
	}

	if first != second {
		t.Fatalf("workspace path should be deterministic, got %q and %q", first, second)
	}
	if got := filepath.Base(first); got != "MT_Det" {
		t.Fatalf("workspace basename mismatch: %q", got)
	}
}

func TestWorkspaceReusePreservesLocalChangesAndCleansTmpArtifacts(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "workspaces")
	manager := mustManager(t, root, map[string]any{
		"hooks": map[string]any{
			"after_create": "echo first > README.md",
		},
	})

	issue := tracker.Issue{Identifier: "MT-REUSE"}
	workspacePath, err := manager.CreateForIssue(context.Background(), issue)
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	if err := os.WriteFile(filepath.Join(workspacePath, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspacePath, "local-progress.txt"), []byte("in progress\n"), 0o644); err != nil {
		t.Fatalf("write local progress: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspacePath, "deps"), 0o755); err != nil {
		t.Fatalf("mkdir deps: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspacePath, "deps", "cache.txt"), []byte("cached deps\n"), 0o644); err != nil {
		t.Fatalf("write deps cache: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspacePath, "tmp"), 0o755); err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspacePath, "tmp", "scratch.txt"), []byte("remove me\n"), 0o644); err != nil {
		t.Fatalf("write tmp scratch: %v", err)
	}

	secondPath, err := manager.CreateForIssue(context.Background(), issue)
	if err != nil {
		t.Fatalf("second create failed: %v", err)
	}
	if secondPath != workspacePath {
		t.Fatalf("workspace path changed on reuse: %q vs %q", workspacePath, secondPath)
	}

	readme, err := os.ReadFile(filepath.Join(secondPath, "README.md"))
	if err != nil {
		t.Fatalf("read readme: %v", err)
	}
	if string(readme) != "changed\n" {
		t.Fatalf("README should be preserved, got %q", string(readme))
	}
	if _, err := os.Stat(filepath.Join(secondPath, "tmp", "scratch.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected tmp artifact to be removed, err=%v", err)
	}
}

func TestWorkspaceReplacesStaleNonDirectoryPath(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "workspaces")
	manager := mustManager(t, root, map[string]any{})
	staleWorkspace := filepath.Join(root, "MT-STALE")

	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.WriteFile(staleWorkspace, []byte("old state\n"), 0o644); err != nil {
		t.Fatalf("write stale file: %v", err)
	}

	workspacePath, err := manager.CreateForIssue(context.Background(), tracker.Issue{Identifier: "MT-STALE"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if workspacePath != staleWorkspace {
		t.Fatalf("workspace path mismatch: %q", workspacePath)
	}
	info, err := os.Stat(workspacePath)
	if err != nil {
		t.Fatalf("stat workspace: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("workspace should be directory")
	}
}

func TestWorkspaceRejectsSymlinkEscapes(t *testing.T) {
	t.Parallel()

	testRoot := t.TempDir()
	workspaceRoot := filepath.Join(testRoot, "workspaces")
	outsideRoot := filepath.Join(testRoot, "outside")
	symlinkPath := filepath.Join(workspaceRoot, "MT-SYM")

	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		t.Fatalf("mkdir workspace root: %v", err)
	}
	if err := os.MkdirAll(outsideRoot, 0o755); err != nil {
		t.Fatalf("mkdir outside root: %v", err)
	}
	if err := os.Symlink(outsideRoot, symlinkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	manager := mustManager(t, workspaceRoot, map[string]any{})
	_, err := manager.CreateForIssue(context.Background(), tracker.Issue{Identifier: "MT-SYM"})
	if !errors.Is(err, ErrWorkspaceSymlinkEscape) {
		t.Fatalf("expected symlink escape error, got %v", err)
	}
}

func TestWorkspaceRemoveRejectsRootPath(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "workspaces")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	manager := mustManager(t, root, map[string]any{})

	err := manager.Remove(context.Background(), root)
	if !errors.Is(err, ErrWorkspaceEqualsRoot) {
		t.Fatalf("expected workspace equals root error, got %v", err)
	}
}

func TestWorkspaceAfterCreateHookFailures(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "workspaces")
	manager := mustManager(t, root, map[string]any{
		"hooks": map[string]any{
			"after_create": "echo nope && exit 17",
		},
	})

	_, err := manager.CreateForIssue(context.Background(), tracker.Issue{Identifier: "MT-FAIL"})
	if !errors.Is(err, ErrWorkspaceHookFailed) {
		t.Fatalf("expected hook failed error, got %v", err)
	}

	hookErr := new(HookFailedError)
	if !errors.As(err, &hookErr) {
		t.Fatalf("expected HookFailedError, got %T", err)
	}
	if hookErr.ExitCode != 17 {
		t.Fatalf("expected exit code 17, got %d", hookErr.ExitCode)
	}
}

func TestWorkspaceAfterCreateHookTimeout(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "workspaces")
	manager := mustManager(t, root, map[string]any{
		"hooks": map[string]any{
			"after_create": "sleep 1",
			"timeout_ms":   10,
		},
	})

	_, err := manager.CreateForIssue(context.Background(), tracker.Issue{Identifier: "MT-TIMEOUT"})
	if !errors.Is(err, ErrWorkspaceHookTimeout) {
		t.Fatalf("expected hook timeout error, got %v", err)
	}
}

func TestWorkspaceCreatesEmptyDirectoryWithoutAfterCreateHook(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "workspaces")
	manager := mustManager(t, root, map[string]any{})

	workspacePath, err := manager.CreateForIssue(context.Background(), tracker.Issue{Identifier: "MT-608"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	entries, err := os.ReadDir(workspacePath)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty directory, got %d entries", len(entries))
	}
}

func TestRemoveIssueWorkspacesRemovesOnlyTarget(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "workspaces")
	manager := mustManager(t, root, map[string]any{})

	targetWorkspace := filepath.Join(root, "S_1")
	untouchedWorkspace := filepath.Join(root, "OTHER")
	if err := os.MkdirAll(targetWorkspace, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.MkdirAll(untouchedWorkspace, 0o755); err != nil {
		t.Fatalf("mkdir untouched: %v", err)
	}
	if err := os.WriteFile(filepath.Join(targetWorkspace, "marker.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("write target marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(untouchedWorkspace, "marker.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("write untouched marker: %v", err)
	}

	if err := manager.RemoveIssueWorkspaces(context.Background(), "S_1"); err != nil {
		t.Fatalf("remove issue workspaces failed: %v", err)
	}
	if _, err := os.Stat(targetWorkspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target workspace should be removed, err=%v", err)
	}
	if _, err := os.Stat(untouchedWorkspace); err != nil {
		t.Fatalf("untouched workspace should exist, err=%v", err)
	}
}

func TestRemoveIssueWorkspacesHandlesMissingRootAndEmptyIdentifier(t *testing.T) {
	t.Parallel()

	missingRoot := filepath.Join(t.TempDir(), "missing-workspaces")
	manager := mustManager(t, missingRoot, map[string]any{})

	if err := manager.RemoveIssueWorkspaces(context.Background(), "S-2"); err != nil {
		t.Fatalf("expected cleanup to ignore missing root, got %v", err)
	}
	if err := manager.RemoveIssueWorkspaces(context.Background(), ""); err != nil {
		t.Fatalf("expected cleanup to ignore empty identifier, got %v", err)
	}
}

func TestWorkspaceRemoveContinuesWhenBeforeRemoveHookFails(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "workspaces")
	manager := mustManager(t, root, map[string]any{
		"hooks": map[string]any{
			"before_remove": "echo nope && exit 17",
		},
	})

	workspacePath, err := manager.CreateForIssue(context.Background(), tracker.Issue{Identifier: "MT-RM-FAIL"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if err := manager.Remove(context.Background(), workspacePath); err != nil {
		t.Fatalf("remove should ignore before_remove hook failures: %v", err)
	}
	if _, err := os.Stat(workspacePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace should be removed, err=%v", err)
	}
}

func TestWorkspaceRemoveContinuesWhenBeforeRemoveHookTimesOut(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "workspaces")
	manager := mustManager(t, root, map[string]any{
		"hooks": map[string]any{
			"before_remove": "sleep 1",
			"timeout_ms":    10,
		},
	})

	workspacePath, err := manager.CreateForIssue(context.Background(), tracker.Issue{Identifier: "MT-RM-TIMEOUT"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if err := manager.Remove(context.Background(), workspacePath); err != nil {
		t.Fatalf("remove should ignore before_remove hook timeouts: %v", err)
	}
	if _, err := os.Stat(workspacePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace should be removed, err=%v", err)
	}
}

func mustManager(t *testing.T, workspaceRoot string, overrides map[string]any) Manager {
	t.Helper()

	cfgMap := map[string]any{
		"tracker": map[string]any{
			"kind": "memory",
		},
		"workspace": map[string]any{
			"root": workspaceRoot,
		},
	}

	for key, value := range overrides {
		cfgMap[key] = value
	}

	cfg, err := config.FromWorkflow(filepath.Join(t.TempDir(), "WORKFLOW.md"), &workflow.Definition{
		Config: cfgMap,
	})
	if err != nil {
		t.Fatalf("build config: %v", err)
	}

	return NewManager(cfg)
}
