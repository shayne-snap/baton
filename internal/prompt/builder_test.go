package prompt

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"baton/internal/config"
	"baton/internal/tracker"
	"baton/internal/workflow"
)

func TestPromptBuilderRendersIssueAndAttemptValues(t *testing.T) {
	t.Parallel()

	cfg := mustPromptConfig(t, "Ticket {{ issue.identifier }} {{ issue.title }} labels={{ issue.labels }} attempt={{ attempt }}")
	builder := NewBuilder(cfg)
	attempt := 3

	prompt, err := builder.BuildPrompt(tracker.Issue{
		Identifier:  "S-1",
		Title:       "Refactor backend request path",
		Description: "Replace transport layer",
		Labels:      []string{"backend"},
	}, &attempt)
	if err != nil {
		t.Fatalf("build prompt failed: %v", err)
	}

	if !strings.Contains(prompt, "Ticket S-1 Refactor backend request path") {
		t.Fatalf("unexpected prompt: %q", prompt)
	}
	if !strings.Contains(prompt, "labels=backend") {
		t.Fatalf("expected labels to render in prompt: %q", prompt)
	}
	if !strings.Contains(prompt, "attempt=3") {
		t.Fatalf("expected attempt to render in prompt: %q", prompt)
	}
}

func TestPromptBuilderRendersDatetimeFields(t *testing.T) {
	t.Parallel()

	cfg := mustPromptConfig(t, "Ticket {{ issue.identifier }} created={{ issue.created_at }} updated={{ issue.updated_at }}")
	builder := NewBuilder(cfg)

	createdAt := time.Date(2026, 2, 26, 18, 6, 48, 0, time.UTC)
	updatedAt := time.Date(2026, 2, 26, 18, 7, 3, 0, time.UTC)

	prompt, err := builder.BuildPrompt(tracker.Issue{
		Identifier: "MT-697",
		Title:      "Live smoke",
		CreatedAt:  &createdAt,
		UpdatedAt:  &updatedAt,
	}, nil)
	if err != nil {
		t.Fatalf("build prompt failed: %v", err)
	}

	if !strings.Contains(prompt, "created=2026-02-26T18:06:48Z") {
		t.Fatalf("expected created_at in prompt: %q", prompt)
	}
	if !strings.Contains(prompt, "updated=2026-02-26T18:07:03Z") {
		t.Fatalf("expected updated_at in prompt: %q", prompt)
	}
}

func TestPromptBuilderUsesDefaultWhenWorkflowPromptBlank(t *testing.T) {
	t.Parallel()

	cfg := mustPromptConfig(t, "   \n")
	builder := NewBuilder(cfg)

	prompt, err := builder.BuildPrompt(tracker.Issue{
		Identifier:  "MT-777",
		Title:       "Make fallback prompt useful",
		Description: "Include enough issue context to start working.",
	}, nil)
	if err != nil {
		t.Fatalf("build prompt failed: %v", err)
	}

	if !strings.Contains(prompt, "You are working on a Linear issue.") {
		t.Fatalf("expected default prompt header, got: %q", prompt)
	}
	if !strings.Contains(prompt, "Identifier: MT-777") {
		t.Fatalf("expected identifier in prompt: %q", prompt)
	}
}

func mustPromptConfig(t *testing.T, prompt string) *config.Config {
	t.Helper()

	cfg, err := config.FromWorkflow(
		filepath.Join(t.TempDir(), "WORKFLOW.md"),
		&workflow.Definition{
			Config: map[string]any{
				"tracker": map[string]any{"kind": "memory"},
			},
			PromptTemplate: prompt,
		},
	)
	if err != nil {
		t.Fatalf("FromWorkflow failed: %v", err)
	}
	return cfg
}
