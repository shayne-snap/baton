package workflow

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatcherEmitsChangeOnWrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("---\ntracker:\n  kind: memory\n---\ninitial\n"), 0o644); err != nil {
		t.Fatalf("write initial workflow: %v", err)
	}

	watcher, err := NewWatcher(workflowPath)
	if err != nil {
		t.Fatalf("NewWatcher failed: %v", err)
	}
	defer func() {
		watcher.mu.Lock()
		watcher.started = false
		watcher.mu.Unlock()
		_ = watcher.Close()
	}()

	events := make(chan ChangeEvent, 8)
	if err := watcher.Start(func(event ChangeEvent) {
		events <- event
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if err := os.WriteFile(workflowPath, []byte("---\ntracker:\n  kind: memory\n---\nupdated\n"), 0o644); err != nil {
		t.Fatalf("write updated workflow: %v", err)
	}

	select {
	case event := <-events:
		if filepath.Clean(event.Path) != filepath.Clean(workflowPath) {
			t.Fatalf("unexpected event path: %q", event.Path)
		}
		if event.Source != "fsnotify" && event.Source != "poll" {
			t.Fatalf("unexpected event source: %q", event.Source)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("expected workflow change event")
	}
}

func TestWatcherStartOnlyOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("prompt"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	watcher, err := NewWatcher(workflowPath)
	if err != nil {
		t.Fatalf("NewWatcher failed: %v", err)
	}
	defer func() {
		watcher.mu.Lock()
		watcher.started = false
		watcher.mu.Unlock()
		_ = watcher.Close()
	}()

	if err := watcher.Start(func(ChangeEvent) {}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if err := watcher.Start(func(ChangeEvent) {}); err == nil {
		t.Fatal("expected second start to fail")
	}
}

func TestWatcherPollDetectsContentChangeWithSameMTimeAndSize(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("aaaa"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	info, err := os.Stat(workflowPath)
	if err != nil {
		t.Fatalf("stat workflow: %v", err)
	}
	originalMTime := info.ModTime()

	watcher, err := NewWatcher(workflowPath)
	if err != nil {
		t.Fatalf("NewWatcher failed: %v", err)
	}
	defer func() {
		watcher.mu.Lock()
		watcher.started = false
		watcher.mu.Unlock()
		_ = watcher.Close()
	}()

	watcher.mu.Lock()
	watcher.started = true
	watcher.mu.Unlock()

	if changed := watcher.fileChangedByPoll(); changed {
		t.Fatal("first poll should only set baseline stamp")
	}

	if err := os.WriteFile(workflowPath, []byte("bbbb"), 0o644); err != nil {
		t.Fatalf("rewrite workflow: %v", err)
	}
	if err := os.Chtimes(workflowPath, originalMTime, originalMTime); err != nil {
		t.Fatalf("chtimes workflow: %v", err)
	}

	if changed := watcher.fileChangedByPoll(); !changed {
		t.Fatal("expected poll change with same mtime/size but different content")
	}
}
