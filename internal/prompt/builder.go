package prompt

import (
	"fmt"
	"time"

	"baton/internal/config"
	"baton/internal/template"
	"baton/internal/tracker"
)

type Builder struct {
	config *config.Config
	engine template.Engine
}

func NewBuilder(cfg *config.Config) *Builder {
	return &Builder{
		config: cfg,
		engine: template.NewLiquidEngine(),
	}
}

func (b *Builder) BuildPrompt(issue tracker.Issue, attempt *int) (string, error) {
	var attemptValue any
	if attempt != nil {
		attemptValue = *attempt
	}
	lifecycle := b.config.TrackerLifecycle()

	bindings := map[string]any{
		"attempt": attemptValue,
		"issue":   normalizeValue(issue),
		"tracker": map[string]any{
			"kind": b.config.TrackerKind(),
			"lifecycle": map[string]any{
				"backlog":      lifecycle.Backlog,
				"todo":         lifecycle.Todo,
				"in_progress":  lifecycle.InProgress,
				"human_review": lifecycle.HumanReview,
				"merging":      lifecycle.Merging,
				"rework":       lifecycle.Rework,
				"done":         lifecycle.Done,
			},
		},
	}

	out, err := b.engine.Render(b.config.WorkflowPrompt(), bindings)
	if err != nil {
		return "", err
	}

	return out, nil
}

func normalizeValue(value any) any {
	switch typed := value.(type) {
	case tracker.Issue:
		return map[string]any{
			"id":                 typed.ID,
			"identifier":         typed.Identifier,
			"title":              typed.Title,
			"description":        typed.Description,
			"priority":           typed.Priority,
			"state":              typed.State,
			"branch_name":        typed.BranchName,
			"url":                typed.URL,
			"labels":             normalizeValue(typed.Labels),
			"blocked_by":         normalizeValue(typed.BlockedBy),
			"created_at":         normalizeValue(typed.CreatedAt),
			"updated_at":         normalizeValue(typed.UpdatedAt),
			"assignee_id":        typed.AssigneeID,
			"assigned_to_worker": typed.AssignedToWorker,
		}
	case tracker.BlockerRef:
		return map[string]any{
			"id":         typed.ID,
			"identifier": typed.Identifier,
			"state":      typed.State,
		}
	case *int:
		if typed == nil {
			return nil
		}
		return *typed
	case *time.Time:
		if typed == nil {
			return nil
		}
		return typed.UTC().Format(time.RFC3339)
	case time.Time:
		return typed.UTC().Format(time.RFC3339)
	case []string:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, item)
		}
		return out
	case []tracker.BlockerRef:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, normalizeValue(item))
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, normalizeValue(item))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = normalizeValue(item)
		}
		return out
	case nil, string, int, int64, float64, bool:
		return typed
	default:
		return fmt.Sprint(typed)
	}
}
