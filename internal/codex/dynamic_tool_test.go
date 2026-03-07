package codex

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"baton/internal/config"
	"baton/internal/tracker"
	"baton/internal/workflow"
)

func TestDynamicToolUnsupportedTool(t *testing.T) {
	t.Parallel()

	executor := NewDynamicToolExecutor(mustDynamicToolConfig(t, map[string]any{
		"tracker": map[string]any{"kind": "memory"},
	}))

	result := executor.Execute(context.Background(), "not_a_real_tool", map[string]any{})
	if success, _ := result["success"].(bool); success {
		t.Fatalf("expected unsupported tool failure, got %#v", result)
	}
	text := firstContentText(t, result)
	decoded := decodeContentJSON(t, text)
	if decoded["error"] == nil {
		t.Fatalf("expected error payload, got %#v", decoded)
	}
}

func TestDynamicToolLinearGraphQLUnsupported(t *testing.T) {
	t.Parallel()

	executor := NewDynamicToolExecutor(mustDynamicToolConfig(t, map[string]any{
		"tracker": map[string]any{"kind": "linear"},
	}))

	result := executor.Execute(context.Background(), "linear_graphql", map[string]any{
		"query": "query Viewer { viewer { id } }",
	})
	if success, _ := result["success"].(bool); success {
		t.Fatalf("expected linear_graphql to be unsupported, got %#v", result)
	}
}

func TestDynamicToolTrackerGetIssueAndUpdateState(t *testing.T) {
	t.Parallel()

	executor := NewDynamicToolExecutor(mustDynamicToolConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "memory",
		},
	}))

	tracker.SetMemoryIssues([]tracker.Issue{
		{ID: "issue-1", Identifier: "BAC-1", Title: "Title", State: "Todo", URL: "https://linear.app/issue/BAC-1"},
	})
	t.Cleanup(tracker.ClearMemoryIssues)

	getResult := executor.Execute(context.Background(), trackerGetIssueTool, map[string]any{
		"issue_id": "issue-1",
	})
	if success, _ := getResult["success"].(bool); !success {
		t.Fatalf("expected get issue success, got %#v", getResult)
	}
	payload := decodeContentJSON(t, firstContentText(t, getResult))
	issue, _ := payload["issue"].(map[string]any)
	if got, _ := issue["identifier"].(string); got != "BAC-1" {
		t.Fatalf("unexpected identifier payload: %#v", payload)
	}

	updateResult := executor.Execute(context.Background(), trackerUpdateStateTool, map[string]any{
		"issue_id": "issue-1",
		"state":    "In Review",
	})
	if success, _ := updateResult["success"].(bool); !success {
		t.Fatalf("expected update state success, got %#v", updateResult)
	}
}

func TestDynamicToolTrackerWorkpadAndLink(t *testing.T) {
	t.Parallel()

	executor := NewDynamicToolExecutor(mustDynamicToolConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "memory",
		},
	}))

	commentResult := executor.Execute(context.Background(), trackerUpsertWorkpadCommentTool, map[string]any{
		"issue_id": "issue-2",
		"body":     "workpad content",
	})
	if success, _ := commentResult["success"].(bool); !success {
		t.Fatalf("expected workpad comment success, got %#v", commentResult)
	}

	linkResult := executor.Execute(context.Background(), trackerAddLinkTool, map[string]any{
		"issue_id": "issue-2",
		"url":      "https://github.com/openai/openai",
		"title":    "repo",
	})
	if success, _ := linkResult["success"].(bool); !success {
		t.Fatalf("expected add link success, got %#v", linkResult)
	}
}

func mustDynamicToolConfig(t *testing.T, cfgMap map[string]any) *config.Config {
	t.Helper()
	cfg, err := config.FromWorkflow(filepath.Join(t.TempDir(), "WORKFLOW.md"), &workflow.Definition{
		Config: cfgMap,
	})
	if err != nil {
		t.Fatalf("FromWorkflow failed: %v", err)
	}
	return cfg
}

func firstContentText(t *testing.T, result map[string]any) string {
	t.Helper()
	items, _ := result["contentItems"].([]any)
	if len(items) == 0 {
		t.Fatalf("missing contentItems in %#v", result)
	}
	item, _ := items[0].(map[string]any)
	text, _ := item["text"].(string)
	return text
}

func decodeContentJSON(t *testing.T, text string) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("decode content text: %v text=%q", err, text)
	}
	return decoded
}

func mapStringValue(root map[string]any, section string, key string) string {
	s, _ := root[section].(map[string]any)
	value, _ := s[key].(string)
	return value
}

func mapIntValue(root map[string]any, section string, key string) int {
	s, _ := root[section].(map[string]any)
	switch typed := s[key].(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	default:
		return -1
	}
}
