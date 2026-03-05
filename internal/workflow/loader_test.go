package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPromptOnlyFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "PROMPT_ONLY_WORKFLOW.md")
	if err := os.WriteFile(path, []byte("Prompt only\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	definition, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}

	if len(definition.Config) != 0 {
		t.Fatalf("expected empty config, got %#v", definition.Config)
	}
	if definition.PromptTemplate != "Prompt only" {
		t.Fatalf("unexpected prompt: %q", definition.PromptTemplate)
	}
}

func TestLoadUnterminatedFrontMatter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "UNTERMINATED_WORKFLOW.md")
	content := "---\ntracker:\n  kind: linear\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	definition, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}

	tracker, ok := definition.Config["tracker"].(map[string]any)
	if !ok {
		t.Fatalf("expected tracker map, got %#v", definition.Config["tracker"])
	}
	if tracker["kind"] != "linear" {
		t.Fatalf("unexpected tracker.kind: %#v", tracker["kind"])
	}
	if definition.PromptTemplate != "" {
		t.Fatalf("expected empty prompt, got %q", definition.PromptTemplate)
	}
}

func TestLoadRejectsNonMapFrontMatter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "INVALID_FRONT_MATTER_WORKFLOW.md")
	content := "---\n- not-a-map\n---\nPrompt body\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := LoadFile(path)
	if !errors.Is(err, ErrWorkflowFrontMatterNotAMap) {
		t.Fatalf("expected ErrWorkflowFrontMatterNotAMap, got %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "MISSING_WORKFLOW.md")
	_, err := LoadFile(path)
	if !errors.Is(err, ErrMissingWorkflowFile) {
		t.Fatalf("expected ErrMissingWorkflowFile, got %v", err)
	}
}
