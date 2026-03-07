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
	"unicode/utf8"

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

func TestNormalizeBranchNameSanitizesNonASCIISegments(t *testing.T) {
	t.Parallel()

	got := normalizeBranchName("linnana9808/bac-7-非英文分支名转英文分支名。", "BAC-7")
	if got != "linnana9808/bac-7" {
		t.Fatalf("expected sanitized branch name, got %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("expected valid utf8 output, got %q", got)
	}
	for _, r := range got {
		if r > 127 {
			t.Fatalf("expected ASCII-only branch name, got %q", got)
		}
	}
}

func TestNormalizeBranchNameFallsBackToIdentifierWhenNoASCIIRemains(t *testing.T) {
	t.Parallel()

	got := normalizeBranchName("纯中文分支", "BAC-7")
	if got != "BAC-7" {
		t.Fatalf("expected identifier fallback, got %q", got)
	}
}

func TestNormalizeBranchNamePreservesSafeEnglishBranchNames(t *testing.T) {
	t.Parallel()

	got := normalizeBranchName("linnana9808/bac-7-fix-readme_v2", "BAC-7")
	if got != "linnana9808/bac-7-fix-readme_v2" {
		t.Fatalf("expected safe branch name to stay unchanged, got %q", got)
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

func TestNewClientUsesJiraTrackerKind(t *testing.T) {
	t.Setenv("BATON_ASSIGNEE", "me")

	var capturedJQL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/myself":
			_ = json.NewEncoder(w).Encode(map[string]any{"accountId": "jira-user-1"})
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/search/jql":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode search payload: %v", err)
			}
			capturedJQL = stringValue(payload["jql"])
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issues": []any{
					map[string]any{
						"id":  "10001",
						"key": "BAC-15",
						"fields": map[string]any{
							"summary": "Implement jira client",
							"description": map[string]any{
								"type":    "doc",
								"version": 1,
								"content": []any{
									map[string]any{
										"type": "paragraph",
										"content": []any{
											map[string]any{"type": "text", "text": "desc"},
										},
									},
								},
							},
							"status":   map[string]any{"name": "In Progress"},
							"assignee": map[string]any{"accountId": "jira-user-1"},
							"labels":   []any{"backend"},
							"created":  "2026-01-01T00:00:00.000+0000",
							"updated":  "2026-01-01T01:00:00.000+0000",
						},
					},
				},
				"total": 1,
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	cfg := mustTrackerConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "jira",
			"jira": map[string]any{
				"base_url":    server.URL,
				"project_key": "BAC",
				"auth": map[string]any{
					"type":      "email_api_token",
					"email":     "jira@example.com",
					"api_token": "jira-token",
				},
			},
			"routing": map[string]any{
				"assignee":        "$BATON_ASSIGNEE",
				"active_states":   []any{"To Do", "In Progress"},
				"terminal_states": []any{"Done"},
			},
		},
	})

	client := NewClient(cfg)
	issues, err := client.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("jira candidate fetch failed: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %#v", issues)
	}
	if issues[0].ID != "BAC-15" || issues[0].Identifier != "BAC-15" {
		t.Fatalf("unexpected issue identity: %#v", issues[0])
	}
	if issues[0].State != "In Progress" {
		t.Fatalf("unexpected issue state: %#v", issues[0])
	}
	if !strings.Contains(strings.ToLower(capturedJQL), "status in") || !strings.Contains(capturedJQL, "assignee = currentUser()") {
		t.Fatalf("unexpected candidate jql: %q", capturedJQL)
	}
}

func TestNewClientUsesFeishuTrackerKind(t *testing.T) {
	t.Setenv("BATON_ASSIGNEE", "feishu-user-1")

	var filterAuthHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token-1",
				"expire":              7200,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/open-apis/project/v1/work_item/filter":
			filterAuthHeader = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"items": []any{
						map[string]any{
							"work_item_id":  "wi-1",
							"work_item_key": "BAC-88",
							"title":         "Implement Feishu client",
							"description":   "desc",
							"state":         map[string]any{"name": "In Progress"},
							"assignee":      map[string]any{"user_key": "feishu-user-1"},
							"labels":        []any{"backend"},
							"created_at":    "2026-01-01T00:00:00Z",
							"updated_at":    "2026-01-01T01:00:00Z",
						},
					},
					"has_more": false,
				},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	cfg := mustTrackerConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "feishu",
			"routing": map[string]any{
				"assignee":        "$BATON_ASSIGNEE",
				"active_states":   []any{"To Do", "In Progress"},
				"terminal_states": []any{"Done"},
			},
			"feishu": map[string]any{
				"base_url":    server.URL,
				"project_key": "BAC",
				"app_id":      "app-id",
				"app_secret":  "app-secret",
			},
		},
	})

	client := NewClient(cfg)
	issues, err := client.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("feishu candidate fetch failed: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %#v", issues)
	}
	if issues[0].ID != "wi-1" || issues[0].Identifier != "BAC-88" {
		t.Fatalf("unexpected issue identity: %#v", issues[0])
	}
	if issues[0].State != "In Progress" {
		t.Fatalf("unexpected issue state: %#v", issues[0])
	}
	if !issues[0].AssignedToWorker {
		t.Fatalf("expected issue to be assigned to worker: %#v", issues[0])
	}
	if got := strings.TrimSpace(filterAuthHeader); got != "Bearer tenant-token-1" {
		t.Fatalf("unexpected filter auth header: %q", got)
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
	if err := client.AddLink(context.Background(), "issue-1", "https://example.com/pr/1", "PR #1"); err != nil {
		t.Fatalf("memory add link failed: %v", err)
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

func TestJiraClientCreateCommentUpdateIssueStateAndAddLink(t *testing.T) {
	t.Parallel()

	commentCalled := false
	updateCalled := false
	linkCalled := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/issue/BAC-99/comment":
			commentCalled = true
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode comment payload: %v", err)
			}
			body, _ := payload["body"].(map[string]any)
			if stringValue(body["type"]) != "doc" {
				t.Fatalf("expected ADF doc body, got %#v", payload["body"])
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "comment-1"})
		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/issue/BAC-99/transitions":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"transitions": []any{
					map[string]any{
						"id": "31",
						"to": map[string]any{"name": "Done"},
					},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/issue/BAC-99/transitions":
			updateCalled = true
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode transition payload: %v", err)
			}
			transition, _ := payload["transition"].(map[string]any)
			if stringValue(transition["id"]) != "31" {
				t.Fatalf("expected transition id 31, got %#v", payload)
			}
			w.WriteHeader(http.StatusNoContent)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/issue/BAC-99/remotelink":
			linkCalled = true
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode remotelink payload: %v", err)
			}
			object, _ := payload["object"].(map[string]any)
			if got := stringValue(object["url"]); got != "https://github.com/openai/openai/pull/10" {
				t.Fatalf("unexpected link url: %#v", payload)
			}
			if got := stringValue(object["title"]); got != "PR #10" {
				t.Fatalf("unexpected link title: %#v", payload)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "10000"})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewJiraClient(mustTrackerConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "jira",
			"jira": map[string]any{
				"base_url":    server.URL,
				"project_key": "BAC",
				"auth": map[string]any{
					"type":      "email_api_token",
					"email":     "jira@example.com",
					"api_token": "jira-token",
				},
			},
		},
	}))

	if err := client.CreateComment(context.Background(), "BAC-99", "hello"); err != nil {
		t.Fatalf("create comment failed: %v", err)
	}
	if err := client.UpdateIssueState(context.Background(), "BAC-99", "Done"); err != nil {
		t.Fatalf("update issue state failed: %v", err)
	}
	if err := client.AddLink(context.Background(), "BAC-99", "https://github.com/openai/openai/pull/10", "PR #10"); err != nil {
		t.Fatalf("add link failed: %v", err)
	}
	if !commentCalled {
		t.Fatal("expected comment endpoint to be called")
	}
	if !updateCalled {
		t.Fatal("expected update state endpoint to be called")
	}
	if !linkCalled {
		t.Fatal("expected remotelink endpoint to be called")
	}
}

func TestFeishuClientCreateCommentUpdateIssueStateAndAddLink(t *testing.T) {
	t.Parallel()

	commentCalled := false
	updateCalled := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code":                0,
				"msg":                 "ok",
				"tenant_access_token": "tenant-token-feishu",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/open-apis/project/v1/comment/create":
			commentCalled = true
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode comment payload: %v", err)
			}
			if got := strings.TrimSpace(stringValue(payload["work_item_id"])); got != "BAC-99" {
				t.Fatalf("unexpected comment issue id: %#v", payload)
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"id": "comment-1"}})
		case r.Method == http.MethodPost && r.URL.Path == "/open-apis/project/v1/work_item/update_state_flow":
			updateCalled = true
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode update payload: %v", err)
			}
			if got := strings.TrimSpace(stringValue(payload["state"])); got != "Done" {
				t.Fatalf("unexpected state payload: %#v", payload)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"updated": true}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewFeishuClient(mustTrackerConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "feishu",
			"feishu": map[string]any{
				"base_url":    server.URL,
				"project_key": "BAC",
				"app_id":      "app-id",
				"app_secret":  "app-secret",
			},
		},
	}))

	if err := client.CreateComment(context.Background(), "BAC-99", "hello"); err != nil {
		t.Fatalf("create comment failed: %v", err)
	}
	if err := client.UpdateIssueState(context.Background(), "BAC-99", "Done"); err != nil {
		t.Fatalf("update issue state failed: %v", err)
	}
	if err := client.AddLink(context.Background(), "BAC-99", "https://github.com/openai/openai/pull/10", "PR #10"); err != nil {
		t.Fatalf("add link failed: %v", err)
	}
	if !commentCalled {
		t.Fatal("expected comment endpoint to be called")
	}
	if !updateCalled {
		t.Fatal("expected state update endpoint to be called")
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

	cfgMap = withTrackerTestDefaults(cfgMap)
	cfg, err := config.FromWorkflow(filepath.Join(t.TempDir(), "WORKFLOW.md"), &workflow.Definition{Config: cfgMap})
	if err != nil {
		t.Fatalf("FromWorkflow failed: %v", err)
	}
	return cfg
}

func withTrackerTestDefaults(cfgMap map[string]any) map[string]any {
	merged := map[string]any{}
	for key, value := range cfgMap {
		merged[key] = value
	}
	trackerRaw, _ := merged["tracker"].(map[string]any)
	if trackerRaw == nil {
		return merged
	}
	trackerCopy := map[string]any{}
	for key, value := range trackerRaw {
		trackerCopy[key] = value
	}
	if _, ok := trackerCopy["lifecycle"]; !ok {
		trackerCopy["lifecycle"] = map[string]any{
			"backlog":      "Backlog",
			"todo":         "Todo",
			"in_progress":  "In Progress",
			"human_review": "In Review",
			"merging":      "Merging",
			"rework":       "Rework",
			"done":         "Done",
		}
	}
	if _, ok := trackerCopy["routing"]; !ok {
		routing := map[string]any{
			"active_states":   []any{"Todo", "In Progress"},
			"terminal_states": []any{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"},
		}
		if assignee, ok := trackerCopy["assignee"]; ok {
			routing["assignee"] = assignee
			delete(trackerCopy, "assignee")
		}
		if activeStates, ok := trackerCopy["active_states"]; ok {
			routing["active_states"] = activeStates
			delete(trackerCopy, "active_states")
		}
		if terminalStates, ok := trackerCopy["terminal_states"]; ok {
			routing["terminal_states"] = terminalStates
			delete(trackerCopy, "terminal_states")
		}
		trackerCopy["routing"] = routing
	}
	if _, ok := trackerCopy["linear"]; !ok {
		linear := map[string]any{}
		if endpoint, ok := trackerCopy["endpoint"]; ok {
			linear["endpoint"] = endpoint
			delete(trackerCopy, "endpoint")
		}
		if apiKey, ok := trackerCopy["api_key"]; ok {
			linear["api_key"] = apiKey
			delete(trackerCopy, "api_key")
		}
		if projectSlug, ok := trackerCopy["project_slug"]; ok {
			linear["project_slug"] = projectSlug
			delete(trackerCopy, "project_slug")
		}
		if len(linear) > 0 {
			trackerCopy["linear"] = linear
		}
	}
	merged["tracker"] = trackerCopy
	return merged
}
