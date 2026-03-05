package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"baton/internal/config"
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

func TestDynamicToolLinearGraphQLSuccessAndGraphQLError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "token" {
			t.Fatalf("expected auth token header")
		}

		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		query, _ := payload["query"].(string)
		if query == "query Viewer { viewer { id } }" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"viewer": map[string]any{"id": "usr_123"}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":   nil,
			"errors": []any{map[string]any{"message": "boom"}},
		})
	}))
	defer server.Close()

	executor := NewDynamicToolExecutor(mustDynamicToolConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      "token",
			"project_slug": "proj",
			"endpoint":     server.URL,
		},
	}))

	okResult := executor.Execute(context.Background(), linearGraphQLTool, map[string]any{
		"query": "query Viewer { viewer { id } }",
	})
	if success, _ := okResult["success"].(bool); !success {
		t.Fatalf("expected success result, got %#v", okResult)
	}

	errResult := executor.Execute(context.Background(), linearGraphQLTool, map[string]any{
		"query": "query Broken { nope }",
	})
	if success, _ := errResult["success"].(bool); success {
		t.Fatalf("expected graphql error result failure, got %#v", errResult)
	}
}

func TestDynamicToolLinearGraphQLValidationErrors(t *testing.T) {
	t.Parallel()

	executor := NewDynamicToolExecutor(mustDynamicToolConfig(t, map[string]any{
		"tracker": map[string]any{"kind": "memory"},
	}))

	cases := []any{
		map[string]any{"variables": map[string]any{"x": 1}},
		[]any{"bad"},
		map[string]any{"query": "q", "variables": []any{"bad"}},
	}

	for _, tc := range cases {
		result := executor.Execute(context.Background(), linearGraphQLTool, tc)
		if success, _ := result["success"].(bool); success {
			t.Fatalf("expected validation failure for %#v, got %#v", tc, result)
		}
	}
}

func TestDynamicToolLinearGraphQLRejectsMultipleOperations(t *testing.T) {
	t.Parallel()

	executor := NewDynamicToolExecutor(mustDynamicToolConfig(t, map[string]any{
		"tracker": map[string]any{"kind": "memory"},
	}))

	result := executor.Execute(context.Background(), linearGraphQLTool, map[string]any{
		"query": `
			query ViewerA { viewer { id } }
			query ViewerB { viewer { name } }
		`,
	})
	if success, _ := result["success"].(bool); success {
		t.Fatalf("expected multi-operation validation failure, got %#v", result)
	}

	payload := decodeContentJSON(t, firstContentText(t, result))
	if got := mapStringValue(payload, "error", "message"); got != "`linear_graphql.query` must contain exactly one GraphQL operation." {
		t.Fatalf("unexpected multi-operation validation error: %#v", payload)
	}
}

func TestDynamicToolLinearGraphQLMissingTokenAndStatusMapping(t *testing.T) {
	t.Parallel()

	missingTokenExecutor := NewDynamicToolExecutor(mustDynamicToolConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      nil,
			"project_slug": "proj",
			"endpoint":     "http://127.0.0.1:1",
		},
	}))
	missingToken := missingTokenExecutor.Execute(context.Background(), linearGraphQLTool, map[string]any{
		"query": "query Viewer { viewer { id } }",
	})
	if success, _ := missingToken["success"].(bool); success {
		t.Fatalf("expected missing token failure")
	}
	tokenPayload := decodeContentJSON(t, firstContentText(t, missingToken))
	message := mapStringValue(tokenPayload, "error", "message")
	if message == "" {
		t.Fatalf("expected missing token message, got %#v", tokenPayload)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"errors":[{"message":"down"}]}`))
	}))
	defer server.Close()

	statusExecutor := NewDynamicToolExecutor(mustDynamicToolConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      "token",
			"project_slug": "proj",
			"endpoint":     server.URL,
		},
	}))
	statusResult := statusExecutor.Execute(context.Background(), linearGraphQLTool, map[string]any{
		"query": "query Viewer { viewer { id } }",
	})
	if success, _ := statusResult["success"].(bool); success {
		t.Fatalf("expected status failure, got %#v", statusResult)
	}
	statusPayload := decodeContentJSON(t, firstContentText(t, statusResult))
	if got := mapIntValue(statusPayload, "error", "status"); got != 503 {
		t.Fatalf("expected mapped status=503, got %#v", statusPayload)
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
