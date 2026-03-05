package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"baton/internal/config"
	"baton/internal/workflow"
)

func TestIssueHelpers(t *testing.T) {
	t.Parallel()

	issue := Issue{
		ID:               "abc",
		Labels:           []string{"frontend", "infra"},
		AssignedToWorker: false,
	}

	labels := issue.LabelNames()
	if len(labels) != 2 || labels[0] != "frontend" || labels[1] != "infra" {
		t.Fatalf("unexpected label names: %#v", labels)
	}
	if issue.AssignedToWorker {
		t.Fatal("expected issue to be marked as not assigned to worker")
	}
}

func TestNormalizeIssueBlockersAndAssignee(t *testing.T) {
	t.Parallel()

	rawIssue := map[string]any{
		"id":          "issue-1",
		"identifier":  "MT-1",
		"title":       "Blocked todo",
		"description": "Needs dependency",
		"priority":    2.0,
		"state":       map[string]any{"name": "Todo"},
		"branchName":  "mt-1",
		"url":         "https://example.org/issues/MT-1",
		"assignee": map[string]any{
			"id": "user-1",
		},
		"labels": map[string]any{
			"nodes": []any{map[string]any{"name": "Backend"}},
		},
		"inverseRelations": map[string]any{
			"nodes": []any{
				map[string]any{
					"type": "blocks",
					"issue": map[string]any{
						"id":         "issue-2",
						"identifier": "MT-2",
						"state":      map[string]any{"name": "In Progress"},
					},
				},
				map[string]any{
					"type": "relatesTo",
					"issue": map[string]any{
						"id":         "issue-3",
						"identifier": "MT-3",
						"state":      map[string]any{"name": "Done"},
					},
				},
			},
		},
		"createdAt": "2026-01-01T00:00:00Z",
		"updatedAt": "2026-01-02T00:00:00Z",
	}

	filter := &assigneeFilter{matchValues: map[string]struct{}{"user-1": {}}}
	issue := normalizeIssue(rawIssue, filter)

	if len(issue.BlockedBy) != 1 {
		t.Fatalf("expected one blocker, got %#v", issue.BlockedBy)
	}
	if issue.BlockedBy[0].ID != "issue-2" || issue.BlockedBy[0].Identifier != "MT-2" || issue.BlockedBy[0].State != "In Progress" {
		t.Fatalf("unexpected blocker: %#v", issue.BlockedBy[0])
	}
	if len(issue.Labels) != 1 || issue.Labels[0] != "backend" {
		t.Fatalf("unexpected labels: %#v", issue.Labels)
	}
	if issue.Priority == nil || *issue.Priority != 2 {
		t.Fatalf("unexpected priority: %#v", issue.Priority)
	}
	if issue.State != "Todo" {
		t.Fatalf("unexpected state: %q", issue.State)
	}
	if issue.AssigneeID != "user-1" {
		t.Fatalf("unexpected assignee id: %q", issue.AssigneeID)
	}
	if !issue.AssignedToWorker {
		t.Fatal("expected issue assigned to worker")
	}
}

func TestNormalizeIssueMarksUnassignedAsNotRouted(t *testing.T) {
	t.Parallel()

	rawIssue := map[string]any{
		"id":         "issue-99",
		"identifier": "MT-99",
		"title":      "Someone else's task",
		"state":      map[string]any{"name": "Todo"},
		"assignee": map[string]any{
			"id": "user-2",
		},
	}
	filter := &assigneeFilter{matchValues: map[string]struct{}{"user-1": {}}}
	issue := normalizeIssue(rawIssue, filter)
	if issue.AssignedToWorker {
		t.Fatal("expected issue not assigned to worker")
	}
}

func TestPaginationHelpers(t *testing.T) {
	t.Parallel()

	page1 := []Issue{
		{ID: "issue-1", Identifier: "MT-1"},
		{ID: "issue-2", Identifier: "MT-2"},
	}
	page2 := []Issue{
		{ID: "issue-3", Identifier: "MT-3"},
	}

	merged := mergeIssuePages([][]Issue{page1, page2})
	if len(merged) != 3 {
		t.Fatalf("unexpected merged length: %d", len(merged))
	}
	if merged[0].Identifier != "MT-1" || merged[1].Identifier != "MT-2" || merged[2].Identifier != "MT-3" {
		t.Fatalf("unexpected merged order: %#v", merged)
	}

	_, _, err := nextPageCursor(pageInfo{HasNextPage: true, EndCursor: ""})
	if !errors.Is(err, ErrLinearMissingEndCursor) {
		t.Fatalf("expected missing cursor error, got %v", err)
	}
}

func TestFetchIssueStatesByIDsEmptyIsNoop(t *testing.T) {
	t.Parallel()

	client := mustLinearClient(t, map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      "token",
			"project_slug": "project",
			"endpoint":     "http://127.0.0.1:1",
		},
	})

	issues, err := client.FetchIssueStatesByIDs(context.Background(), []string{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("expected empty issue list, got %#v", issues)
	}
}

func TestFetchCandidateIssuesRequiresTokenAndProject(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")

	client := mustLinearClient(t, map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      nil,
			"project_slug": nil,
			"endpoint":     "http://127.0.0.1:1",
		},
	})

	_, err := client.FetchCandidateIssues(context.Background())
	if !errors.Is(err, ErrMissingLinearAPIToken) {
		t.Fatalf("expected missing token error, got %v", err)
	}

	client = mustLinearClient(t, map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      "token",
			"project_slug": nil,
			"endpoint":     "http://127.0.0.1:1",
		},
	})
	_, err = client.FetchCandidateIssues(context.Background())
	if !errors.Is(err, ErrMissingLinearProjectSlug) {
		t.Fatalf("expected missing project slug error, got %v", err)
	}
}

func TestGraphQLNon200ReturnsStatusError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Variable \"$ids\" got invalid value","extensions":{"code":"BAD_USER_INPUT"}}]}`))
	}))
	defer server.Close()

	client := mustLinearClient(t, map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      "token",
			"project_slug": "project",
			"endpoint":     server.URL,
		},
	})

	_, err := client.graphql(context.Background(), "query Viewer { viewer { id } }", map[string]any{}, "")
	if !errors.Is(err, ErrLinearAPIStatus) {
		t.Fatalf("expected linear api status error, got %v", err)
	}
	statusErr := new(GraphQLStatusError)
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected GraphQLStatusError, got %T", err)
	}
	if statusErr.Status != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", statusErr.Status)
	}
}

func TestFetchIssuesByStatesMapsGraphQLErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errors": []any{
				map[string]any{"message": "boom"},
			},
		})
	}))
	defer server.Close()

	client := mustLinearClient(t, map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      "token",
			"project_slug": "project",
			"endpoint":     server.URL,
		},
	})

	_, err := client.FetchIssuesByStates(context.Background(), []string{"Todo"})
	if !errors.Is(err, ErrLinearGraphQLErrors) {
		t.Fatalf("expected graphql errors mapping, got %v", err)
	}
}

func TestNewClientUsesMemoryTrackerKind(t *testing.T) {
	defer ClearMemoryIssues()
	_ = os.Unsetenv(memoryIssuesEnv)

	cfg := mustTrackerConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "memory",
		},
	})

	SetMemoryIssues([]Issue{
		{ID: "issue-1", Identifier: "MT-1", State: "In Progress"},
		{ID: "issue-2", Identifier: "MT-2", State: "Todo"},
	})

	client := NewClient(cfg)
	issues, err := client.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("memory candidate fetch failed: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("expected 2 issues, got %#v", issues)
	}

	filtered, err := client.FetchIssuesByStates(context.Background(), []string{" in progress ", "not-a-state"})
	if err != nil {
		t.Fatalf("memory state filter failed: %v", err)
	}
	if len(filtered) != 1 || filtered[0].ID != "issue-1" {
		t.Fatalf("unexpected state-filtered issues: %#v", filtered)
	}

	byIDs, err := client.FetchIssueStatesByIDs(context.Background(), []string{"issue-2"})
	if err != nil {
		t.Fatalf("memory id filter failed: %v", err)
	}
	if len(byIDs) != 1 || byIDs[0].Identifier != "MT-2" {
		t.Fatalf("unexpected id-filtered issues: %#v", byIDs)
	}
}

func TestNewClientFallsBackToLinear(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")
	cfg := mustTrackerConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      nil,
			"project_slug": "project",
			"endpoint":     "http://127.0.0.1:1",
		},
	})

	client := NewClient(cfg)
	_, err := client.FetchCandidateIssues(context.Background())
	if !errors.Is(err, ErrMissingLinearAPIToken) {
		t.Fatalf("expected linear client behavior, got %v", err)
	}
}

func TestMemoryClientWriteMethods(t *testing.T) {
	t.Parallel()

	client := NewMemoryClient(mustTrackerConfig(t, map[string]any{
		"tracker": map[string]any{"kind": "memory"},
	}))
	if err := client.CreateComment(context.Background(), "issue-1", "hello"); err != nil {
		t.Fatalf("memory create comment failed: %v", err)
	}
	if err := client.UpdateIssueState(context.Background(), "issue-1", "Done"); err != nil {
		t.Fatalf("memory update state failed: %v", err)
	}
}

func TestLinearClientCreateCommentAndUpdateIssueState(t *testing.T) {
	t.Parallel()

	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		query, _ := payload["query"].(string)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(query, "commentCreate"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"commentCreate": map[string]any{"success": true},
				},
			})
		case strings.Contains(query, "states(filter"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issue": map[string]any{
						"team": map[string]any{
							"states": map[string]any{
								"nodes": []any{map[string]any{"id": "state-1"}},
							},
						},
					},
				},
			})
		case strings.Contains(query, "issueUpdate"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issueUpdate": map[string]any{"success": true},
				},
			})
		default:
			t.Fatalf("unexpected graphql query: %s", query)
		}
	}))
	defer server.Close()

	client := mustLinearClient(t, map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      "token",
			"project_slug": "project",
			"endpoint":     server.URL,
		},
	})

	if err := client.CreateComment(context.Background(), "issue-1", "hello"); err != nil {
		t.Fatalf("create comment failed: %v", err)
	}
	if err := client.UpdateIssueState(context.Background(), "issue-1", "Done"); err != nil {
		t.Fatalf("update issue state failed: %v", err)
	}
	if callCount < 3 {
		t.Fatalf("expected at least 3 graphql calls, got %d", callCount)
	}
}

func TestLinearClientWriteFailureMappings(t *testing.T) {
	t.Parallel()

	commentFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"commentCreate": map[string]any{"success": false},
			},
		})
	}))
	defer commentFail.Close()

	commentClient := mustLinearClient(t, map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      "token",
			"project_slug": "project",
			"endpoint":     commentFail.URL,
		},
	})
	if err := commentClient.CreateComment(context.Background(), "issue-1", "hello"); !errors.Is(err, ErrCommentCreateFailed) {
		t.Fatalf("expected comment create failure, got %v", err)
	}

	stateMissing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		query, _ := payload["query"].(string)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(query, "states(filter") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"issue": map[string]any{
						"team": map[string]any{
							"states": map[string]any{
								"nodes": []any{},
							},
						},
					},
				},
			})
			return
		}
		t.Fatalf("unexpected query while checking missing state: %s", query)
	}))
	defer stateMissing.Close()

	stateClient := mustLinearClient(t, map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      "token",
			"project_slug": "project",
			"endpoint":     stateMissing.URL,
		},
	})
	if err := stateClient.UpdateIssueState(context.Background(), "issue-1", "Done"); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("expected state_not_found, got %v", err)
	}
}

func mustLinearClient(t *testing.T, cfgMap map[string]any) *linearClient {
	t.Helper()

	cfg := mustTrackerConfig(t, cfgMap)
	client, ok := NewLinearClient(cfg).(*linearClient)
	if !ok {
		t.Fatalf("expected *linearClient")
	}
	return client
}

func mustTrackerConfig(t *testing.T, cfgMap map[string]any) *config.Config {
	t.Helper()

	cfg, err := config.FromWorkflow(filepath.Join(t.TempDir(), "WORKFLOW.md"), &workflow.Definition{Config: cfgMap})
	if err != nil {
		t.Fatalf("FromWorkflow failed: %v", err)
	}
	return cfg
}
