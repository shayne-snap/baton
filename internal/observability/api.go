package observability

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"baton/internal/logging"
	"baton/internal/orchestrator"
	"baton/internal/statusdashboard"
)

const defaultSnapshotTimeout = 15 * time.Second

type snapshotOrchestrator interface {
	Snapshot(timeout time.Duration) (map[string]any, error)
	RequestRefresh() (map[string]any, error)
}

type HandlerOptions struct {
	Orchestrator    snapshotOrchestrator
	SnapshotTimeout time.Duration
	WorkspaceRoot   string
	LogsRoot        string
}

type Handler struct {
	orchestrator    snapshotOrchestrator
	snapshotTimeout time.Duration
	workspaceRoot   string
	logsRoot        string
}

func NewHandler(opts HandlerOptions) http.Handler {
	timeout := opts.SnapshotTimeout
	if timeout <= 0 {
		timeout = defaultSnapshotTimeout
	}

	return &Handler{
		orchestrator:    opts.Orchestrator,
		snapshotTimeout: timeout,
		workspaceRoot:   opts.WorkspaceRoot,
		logsRoot:        opts.LogsRoot,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case path == "/":
		h.methodNotAllowed(w)
		return
	case path == "/api/v1/state":
		if r.Method != http.MethodGet {
			h.methodNotAllowed(w)
			return
		}
		h.state(w)
		return
	case path == "/api/v1/refresh":
		if r.Method != http.MethodPost {
			h.methodNotAllowed(w)
			return
		}
		h.refresh(w)
		return
	default:
		if issueIdentifier, ok := issueIdentifierFromPath(path); ok {
			if r.Method != http.MethodGet {
				h.methodNotAllowed(w)
				return
			}
			h.issue(w, issueIdentifier)
			return
		}
		h.notFound(w)
	}
}

func issueIdentifierFromPath(path string) (string, bool) {
	if !strings.HasPrefix(path, "/api/v1/") {
		return "", false
	}

	rest := strings.TrimPrefix(path, "/api/v1/")
	rest = strings.TrimSpace(rest)
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

func (h *Handler) state(w http.ResponseWriter) {
	generatedAt := iso8601(time.Now().UTC())

	snapshot, err := h.orchestrator.Snapshot(h.snapshotTimeout)
	if err != nil {
		if errors.Is(err, orchestrator.ErrSnapshotTimeout) {
			writeJSON(w, http.StatusOK, map[string]any{
				"generated_at": generatedAt,
				"error": map[string]any{
					"code":    "snapshot_timeout",
					"message": "Snapshot timed out",
				},
			})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"generated_at": generatedAt,
			"error": map[string]any{
				"code":    "snapshot_unavailable",
				"message": "Snapshot unavailable",
			},
		})
		return
	}

	running := mapSlice(snapshot["running"])
	retrying := mapSlice(snapshot["retrying"])

	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at": generatedAt,
		"counts": map[string]any{
			"running":  len(running),
			"retrying": len(retrying),
		},
		"running":      projectRunningEntries(running),
		"retrying":     projectRetryEntries(retrying),
		"codex_totals": snapshot["codex_totals"],
		"rate_limits":  snapshot["rate_limits"],
	})
}

func (h *Handler) issue(w http.ResponseWriter, issueIdentifier string) {
	snapshot, err := h.orchestrator.Snapshot(h.snapshotTimeout)
	if err != nil {
		h.errorResponse(w, http.StatusNotFound, "issue_not_found", "Issue not found")
		return
	}

	running := findByIdentifier(mapSlice(snapshot["running"]), issueIdentifier)
	retry := findByIdentifier(mapSlice(snapshot["retrying"]), issueIdentifier)
	if running == nil && retry == nil {
		h.errorResponse(w, http.StatusNotFound, "issue_not_found", "Issue not found")
		return
	}

	issueID := stringValue(field(running, retry, "issue_id"))
	retryAttempt := intValue(mapValue(retry, "attempt"))
	if retryAttempt < 0 {
		retryAttempt = 0
	}

	payload := map[string]any{
		"issue_identifier": issueIdentifier,
		"issue_id":         issueID,
		"status":           issueStatus(running, retry),
		"workspace": map[string]any{
			"path": filepath.Join(h.workspaceRoot, issueIdentifier),
		},
		"attempts": map[string]any{
			"restart_count":         max(retryAttempt-1, 0),
			"current_retry_attempt": retryAttempt,
		},
		"running":       runningIssuePayload(running),
		"retry":         retryIssuePayload(retry),
		"logs":          map[string]any{"codex_session_logs": h.codexSessionLogs(issueIdentifier)},
		"recent_events": recentEventsPayload(running),
		"last_error":    mapValue(retry, "error"),
		"tracked":       map[string]any{},
	}

	writeJSON(w, http.StatusOK, payload)
}

func (h *Handler) refresh(w http.ResponseWriter) {
	payload, err := h.orchestrator.RequestRefresh()
	if err != nil {
		h.errorResponse(w, http.StatusServiceUnavailable, "orchestrator_unavailable", "Orchestrator is unavailable")
		return
	}

	response := cloneMap(payload)
	if requestedAt, ok := response["requested_at"].(time.Time); ok {
		response["requested_at"] = iso8601(requestedAt)
	}

	writeJSON(w, http.StatusAccepted, response)
}

func (h *Handler) methodNotAllowed(w http.ResponseWriter) {
	h.errorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
}

func (h *Handler) notFound(w http.ResponseWriter) {
	h.errorResponse(w, http.StatusNotFound, "not_found", "Route not found")
}

func (h *Handler) errorResponse(w http.ResponseWriter, status int, code string, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func projectRunningEntries(entries []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		out = append(out, map[string]any{
			"issue_id":         stringValue(entry["issue_id"]),
			"issue_identifier": stringValue(entry["identifier"]),
			"state":            stringValue(entry["state"]),
			"session_id":       stringValue(entry["session_id"]),
			"turn_count":       max(intValue(entry["turn_count"]), 0),
			"last_event":       normalizeEvent(entry["last_codex_event"]),
			"last_message":     summarizeMessage(entry["last_codex_message"]),
			"started_at":       toISO8601(entry["started_at"]),
			"last_event_at":    toISO8601(entry["last_codex_timestamp"]),
			"tokens": map[string]any{
				"input_tokens":  max(intValue(entry["codex_input_tokens"]), 0),
				"output_tokens": max(intValue(entry["codex_output_tokens"]), 0),
				"total_tokens":  max(intValue(entry["codex_total_tokens"]), 0),
			},
		})
	}
	return out
}

func projectRetryEntries(entries []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	now := time.Now().UTC()
	for _, entry := range entries {
		out = append(out, map[string]any{
			"issue_id":         stringValue(entry["issue_id"]),
			"issue_identifier": stringValue(entry["identifier"]),
			"attempt":          max(intValue(entry["attempt"]), 0),
			"due_at":           dueAtISO8601(entry["due_in_ms"], now),
			"error":            mapValue(entry, "error"),
		})
	}
	return out
}

func runningIssuePayload(entry map[string]any) any {
	if entry == nil {
		return nil
	}
	return map[string]any{
		"session_id":    stringValue(entry["session_id"]),
		"turn_count":    max(intValue(entry["turn_count"]), 0),
		"state":         stringValue(entry["state"]),
		"started_at":    toISO8601(entry["started_at"]),
		"last_event":    normalizeEvent(entry["last_codex_event"]),
		"last_message":  summarizeMessage(entry["last_codex_message"]),
		"last_event_at": toISO8601(entry["last_codex_timestamp"]),
		"tokens": map[string]any{
			"input_tokens":  max(intValue(entry["codex_input_tokens"]), 0),
			"output_tokens": max(intValue(entry["codex_output_tokens"]), 0),
			"total_tokens":  max(intValue(entry["codex_total_tokens"]), 0),
		},
	}
}

func retryIssuePayload(entry map[string]any) any {
	if entry == nil {
		return nil
	}
	return map[string]any{
		"attempt": max(intValue(entry["attempt"]), 0),
		"due_at":  dueAtISO8601(entry["due_in_ms"], time.Now().UTC()),
		"error":   mapValue(entry, "error"),
	}
}

func recentEventsPayload(running map[string]any) []map[string]any {
	if running == nil {
		return []map[string]any{}
	}

	at := toISO8601(running["last_codex_timestamp"])
	if at == nil {
		return []map[string]any{}
	}

	return []map[string]any{
		{
			"at":      at,
			"event":   normalizeEvent(running["last_codex_event"]),
			"message": summarizeMessage(running["last_codex_message"]),
		},
	}
}

func dueAtISO8601(dueInValue any, now time.Time) any {
	dueIn := intValue(dueInValue)
	if dueIn < 0 {
		return nil
	}
	return iso8601(now.Add(time.Duration(dueIn/1000) * time.Second))
}

func summarizeMessage(raw any) any {
	return statusdashboard.HumanizeCodexMessage(raw)
}

func issueStatus(running map[string]any, retry map[string]any) string {
	if running != nil {
		return "running"
	}
	if retry != nil {
		return "retrying"
	}
	return "running"
}

func field(primary map[string]any, fallback map[string]any, key string) any {
	if value := mapValue(primary, key); value != nil {
		return value
	}
	return mapValue(fallback, key)
}

func findByIdentifier(entries []map[string]any, identifier string) map[string]any {
	for _, entry := range entries {
		if stringValue(entry["identifier"]) == identifier {
			return entry
		}
	}
	return nil
}

func mapSlice(value any) []map[string]any {
	switch typed := value.(type) {
	case nil:
		return []map[string]any{}
	case []map[string]any:
		return typed
	case []any:
		result := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			if mapped, ok := item.(map[string]any); ok {
				result = append(result, mapped)
			}
		}
		return result
	default:
		return []map[string]any{}
	}
}

func mapValue(value map[string]any, key string) any {
	if value == nil {
		return nil
	}
	return value[key]
}

func normalizeEvent(value any) any {
	if value == nil {
		return nil
	}
	return fmt.Sprint(value)
}

func toISO8601(value any) any {
	switch typed := value.(type) {
	case time.Time:
		if typed.IsZero() {
			return nil
		}
		return iso8601(typed)
	case *time.Time:
		if typed == nil || typed.IsZero() {
			return nil
		}
		return iso8601(*typed)
	default:
		return nil
	}
}

func iso8601(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return -1
	}
}

func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func (h *Handler) codexSessionLogs(issueIdentifier string) []any {
	logsDir := logging.CodexIssueLogsDir(h.logsRoot, issueIdentifier)

	entries := make([]any, 0, 4)
	latestPath := filepath.Join(logsDir, "latest.log")
	if fileExists(latestPath) {
		entries = append(entries, map[string]any{
			"label": "latest",
			"path":  latestPath,
			"url":   nil,
		})
	}

	matches, err := filepath.Glob(filepath.Join(logsDir, "session-*.log"))
	if err != nil || len(matches) == 0 {
		return entries
	}
	sort.Strings(matches)
	for i := len(matches) - 1; i >= 0 && len(entries) < 4; i-- {
		path := matches[i]
		entries = append(entries, map[string]any{
			"label": filepath.Base(path),
			"path":  path,
			"url":   nil,
		})
	}

	return entries
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func writeJSON(w http.ResponseWriter, status int, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
