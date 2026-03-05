package tracker

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"

	"baton/internal/config"
)

const memoryIssuesEnv = "BATON_MEMORY_TRACKER_ISSUES_JSON"

type memoryClient struct {
	config *config.Config
}

type memoryIssueStore struct {
	mu     sync.RWMutex
	issues []Issue
}

var globalMemoryIssueStore memoryIssueStore

func NewMemoryClient(cfg *config.Config) Client {
	return &memoryClient{config: cfg}
}

func SetMemoryIssues(issues []Issue) {
	globalMemoryIssueStore.mu.Lock()
	defer globalMemoryIssueStore.mu.Unlock()

	cloned := make([]Issue, len(issues))
	copy(cloned, issues)
	globalMemoryIssueStore.issues = cloned
}

func ClearMemoryIssues() {
	globalMemoryIssueStore.mu.Lock()
	defer globalMemoryIssueStore.mu.Unlock()
	globalMemoryIssueStore.issues = nil
}

func (c *memoryClient) FetchCandidateIssues(_ context.Context) ([]Issue, error) {
	return configuredMemoryIssues(), nil
}

func (c *memoryClient) FetchIssuesByStates(_ context.Context, states []string) ([]Issue, error) {
	normalizedStates := normalizedStateSet(states)
	if len(normalizedStates) == 0 {
		return []Issue{}, nil
	}

	issues := configuredMemoryIssues()
	filtered := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		if _, ok := normalizedStates[normalizeIssueState(issue.State)]; ok {
			filtered = append(filtered, issue)
		}
	}
	return filtered, nil
}

func (c *memoryClient) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]Issue, error) {
	wantedIDs := map[string]struct{}{}
	for _, id := range ids {
		trimmed := strings.TrimSpace(id)
		if trimmed != "" {
			wantedIDs[trimmed] = struct{}{}
		}
	}
	if len(wantedIDs) == 0 {
		return []Issue{}, nil
	}

	issues := configuredMemoryIssues()
	filtered := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		if _, ok := wantedIDs[issue.ID]; ok {
			filtered = append(filtered, issue)
		}
	}
	return filtered, nil
}

func (c *memoryClient) CreateComment(_ context.Context, _ string, _ string) error {
	return nil
}

func (c *memoryClient) UpdateIssueState(_ context.Context, _ string, _ string) error {
	return nil
}

func configuredMemoryIssues() []Issue {
	if fromEnv, ok := memoryIssuesFromEnv(); ok {
		return fromEnv
	}

	globalMemoryIssueStore.mu.RLock()
	defer globalMemoryIssueStore.mu.RUnlock()

	cloned := make([]Issue, len(globalMemoryIssueStore.issues))
	copy(cloned, globalMemoryIssueStore.issues)
	return cloned
}

func memoryIssuesFromEnv() ([]Issue, bool) {
	raw := strings.TrimSpace(os.Getenv(memoryIssuesEnv))
	if raw == "" {
		return nil, false
	}

	var entries []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return []Issue{}, true
	}

	issues := make([]Issue, 0, len(entries))
	for _, entry := range entries {
		var issue Issue
		if err := json.Unmarshal(entry, &issue); err != nil {
			continue
		}
		issues = append(issues, issue)
	}

	return issues, true
}

func normalizedStateSet(states []string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, state := range states {
		normalized := normalizeIssueState(state)
		if normalized == "" {
			continue
		}
		set[normalized] = struct{}{}
	}
	return set
}

func normalizeIssueState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}
