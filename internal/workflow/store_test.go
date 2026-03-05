package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreCurrentAndForceReloadKeepsLastKnownGood(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	writeWorkflowFile(t, workflowPath, "---\ntracker:\n  kind: memory\n---\ninitial prompt\n")

	store, err := NewStore(workflowPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	current, err := store.Current()
	if err != nil {
		t.Fatalf("current failed: %v", err)
	}
	if got := current.PromptTemplate; got != "initial prompt" {
		t.Fatalf("unexpected initial prompt: %q", got)
	}

	writeWorkflowFile(t, workflowPath, "---\ntracker:\n  kind: memory\npolling:\n  interval_ms: 1200\n---\nupdated prompt\n")
	if err := store.ForceReload(); err != nil {
		t.Fatalf("force reload updated workflow: %v", err)
	}
	updated, err := store.Current()
	if err != nil {
		t.Fatalf("current after update failed: %v", err)
	}
	if got := updated.PromptTemplate; got != "updated prompt" {
		t.Fatalf("unexpected updated prompt: %q", got)
	}

	writeWorkflowFile(t, workflowPath, "---\ntracker: [\n---\nbroken\n")
	if err := store.ForceReload(); err == nil {
		t.Fatal("expected force reload failure for invalid workflow")
	}
	lastGood, err := store.Current()
	if err != nil {
		t.Fatalf("current after invalid workflow failed: %v", err)
	}
	if got := lastGood.PromptTemplate; got != "updated prompt" {
		t.Fatalf("expected last known good prompt, got %q", got)
	}
}

func TestStorePollReload(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	writeWorkflowFile(t, workflowPath, "---\ntracker:\n  kind: memory\n---\npoll-1\n")

	store, err := NewStore(workflowPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := store.Start(); err != nil {
		t.Fatalf("start store: %v", err)
	}
	defer store.Close()

	writeWorkflowFile(t, workflowPath, "---\ntracker:\n  kind: memory\n---\npoll-2\n")
	assertEventuallyStore(t, 5*time.Second, func() bool {
		current, err := store.Current()
		if err != nil {
			return false
		}
		return current.PromptTemplate == "poll-2"
	})
}

func TestStoreStartOnlyOnce(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	writeWorkflowFile(t, workflowPath, "---\ntracker:\n  kind: memory\n---\nprompt\n")

	store, err := NewStore(workflowPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := store.Start(); err != nil {
		t.Fatalf("start store: %v", err)
	}
	defer store.Close()

	if err := store.Start(); !errors.Is(err, ErrStoreAlreadyStarted) {
		t.Fatalf("expected ErrStoreAlreadyStarted, got %v", err)
	}
}

func TestStoreSetPathSwitchAndFallback(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowA := filepath.Join(dir, "WORKFLOW-A.md")
	workflowB := filepath.Join(dir, "WORKFLOW-B.md")
	missing := filepath.Join(dir, "MISSING-WORKFLOW.md")

	writeWorkflowFile(t, workflowA, "---\ntracker:\n  kind: memory\n---\nprompt-a\n")
	writeWorkflowFile(t, workflowB, "---\ntracker:\n  kind: memory\n---\nprompt-b\n")

	store, err := NewStore(workflowA)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	current, err := store.Current()
	if err != nil {
		t.Fatalf("current failed: %v", err)
	}
	if current.PromptTemplate != "prompt-a" {
		t.Fatalf("expected prompt-a, got %q", current.PromptTemplate)
	}

	if err := store.SetPath(workflowB); err != nil {
		t.Fatalf("set path to workflowB failed: %v", err)
	}
	if got := store.Path(); got != filepath.Clean(workflowB) {
		t.Fatalf("expected path=%q, got %q", workflowB, got)
	}
	current, err = store.Current()
	if err != nil {
		t.Fatalf("current after set path failed: %v", err)
	}
	if current.PromptTemplate != "prompt-b" {
		t.Fatalf("expected prompt-b, got %q", current.PromptTemplate)
	}

	if err := store.SetPath(missing); err == nil {
		t.Fatal("expected set path missing workflow failure")
	}
	current, err = store.Current()
	if err != nil {
		t.Fatalf("current after missing set path failed: %v", err)
	}
	if current.PromptTemplate != "prompt-b" {
		t.Fatalf("expected last-known-good prompt-b, got %q", current.PromptTemplate)
	}

	writeWorkflowFile(t, workflowA, "---\ntracker: [\n---\nbroken\n")
	if err := store.SetPath(workflowA); err == nil {
		t.Fatal("expected set path invalid workflow failure")
	}
	current, err = store.Current()
	if err != nil {
		t.Fatalf("current after invalid set path failed: %v", err)
	}
	if current.PromptTemplate != "prompt-b" {
		t.Fatalf("expected last-known-good prompt-b after invalid set path, got %q", current.PromptTemplate)
	}
}

func TestStorePathSourceSwitchAndFallback(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowA := filepath.Join(dir, "WORKFLOW-A.md")
	workflowB := filepath.Join(dir, "WORKFLOW-B.md")
	missing := filepath.Join(dir, "MISSING-WORKFLOW.md")

	writeWorkflowFile(t, workflowA, "---\ntracker:\n  kind: memory\n---\nprompt-a\n")
	writeWorkflowFile(t, workflowB, "---\ntracker:\n  kind: memory\n---\nprompt-b\n")

	activePath := workflowA
	store, err := NewStoreWithPathSource(workflowA, func() string {
		return activePath
	})
	if err != nil {
		t.Fatalf("new store with path source: %v", err)
	}

	current, err := store.Current()
	if err != nil {
		t.Fatalf("current failed: %v", err)
	}
	if current.PromptTemplate != "prompt-a" {
		t.Fatalf("expected prompt-a, got %q", current.PromptTemplate)
	}

	activePath = workflowB
	current, err = store.Current()
	if err != nil {
		t.Fatalf("current after path switch failed: %v", err)
	}
	if current.PromptTemplate != "prompt-b" {
		t.Fatalf("expected prompt-b, got %q", current.PromptTemplate)
	}
	if got := store.Path(); got != filepath.Clean(workflowB) {
		t.Fatalf("expected active path=%q, got %q", workflowB, got)
	}

	activePath = missing
	current, err = store.Current()
	if err != nil {
		t.Fatalf("current should keep last-known-good on path source error, got %v", err)
	}
	if current.PromptTemplate != "prompt-b" {
		t.Fatalf("expected last-known-good prompt-b, got %q", current.PromptTemplate)
	}
	if got := store.Path(); got != filepath.Clean(workflowB) {
		t.Fatalf("expected path to remain %q after failed switch, got %q", workflowB, got)
	}

	if err := store.ForceReload(); err == nil {
		t.Fatal("expected force reload failure for missing sourced path")
	}
}

func writeWorkflowFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write workflow file: %v", err)
	}
}

func assertEventuallyStore(t *testing.T, timeout time.Duration, predicate func() bool) {
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
