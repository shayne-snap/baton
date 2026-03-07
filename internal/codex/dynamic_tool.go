package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"baton/internal/config"
	"baton/internal/tracker"
)

const LinearGraphQLTool = "linear_graphql"
const TrackerGetIssueTool = "tracker_get_issue"
const TrackerUpdateStateTool = "tracker_update_state"
const TrackerUpsertWorkpadCommentTool = "tracker_upsert_workpad_comment"
const TrackerAddLinkTool = "tracker_add_link"

const linearGraphQLTool = LinearGraphQLTool
const trackerGetIssueTool = TrackerGetIssueTool
const trackerUpdateStateTool = TrackerUpdateStateTool
const trackerUpsertWorkpadCommentTool = TrackerUpsertWorkpadCommentTool
const trackerAddLinkTool = TrackerAddLinkTool

var (
	errMissingQuery          = errors.New("missing_query")
	errInvalidArguments      = errors.New("invalid_arguments")
	errInvalidVariables      = errors.New("invalid_variables")
	errInvalidOperationCount = errors.New("invalid_operation_count")
	errMissingIssueID        = errors.New("missing_issue_id")
	errMissingState          = errors.New("missing_state")
	errMissingBody           = errors.New("missing_body")
	errMissingURL            = errors.New("missing_url")
	errIssueNotFound         = errors.New("issue_not_found")
)

type linearStatusError struct {
	Status int
}

func (e *linearStatusError) Error() string {
	return fmt.Sprintf("linear_api_status: %d", e.Status)
}

type linearRequestError struct {
	Reason any
}

func (e *linearRequestError) Error() string {
	return fmt.Sprintf("linear_api_request: %v", e.Reason)
}

func ToolSpecs() []map[string]any {
	return []map[string]any{
		{
			"name":        trackerGetIssueTool,
			"description": "Fetch a tracker issue by `issue_id`.",
			"inputSchema": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []any{"issue_id"},
				"properties": map[string]any{
					"issue_id": map[string]any{
						"type":        "string",
						"description": "Tracker issue id.",
					},
				},
			},
		},
		{
			"name":        trackerUpdateStateTool,
			"description": "Update tracker issue state.",
			"inputSchema": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []any{"issue_id", "state"},
				"properties": map[string]any{
					"issue_id": map[string]any{
						"type":        "string",
						"description": "Tracker issue id.",
					},
					"state": map[string]any{
						"type":        "string",
						"description": "Target tracker state name.",
					},
				},
			},
		},
		{
			"name":        trackerUpsertWorkpadCommentTool,
			"description": "Upsert the tracker workpad comment for an issue.",
			"inputSchema": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []any{"issue_id", "body"},
				"properties": map[string]any{
					"issue_id": map[string]any{
						"type":        "string",
						"description": "Tracker issue id.",
					},
					"body": map[string]any{
						"type":        "string",
						"description": "Comment body text.",
					},
				},
			},
		},
		{
			"name":        trackerAddLinkTool,
			"description": "Add a link to the issue workpad comment.",
			"inputSchema": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []any{"issue_id", "url"},
				"properties": map[string]any{
					"issue_id": map[string]any{
						"type":        "string",
						"description": "Tracker issue id.",
					},
					"url": map[string]any{
						"type":        "string",
						"description": "URL to attach to the issue.",
					},
					"title": map[string]any{
						"type":        "string",
						"description": "Optional link title.",
					},
				},
			},
		},
	}
}

type DynamicToolExecutor struct {
	config *config.Config
	client *http.Client
}

func NewDynamicToolExecutor(cfg *config.Config) *DynamicToolExecutor {
	return &DynamicToolExecutor{
		config: cfg,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (e *DynamicToolExecutor) Execute(ctx context.Context, tool string, arguments any) map[string]any {
	switch tool {
	case trackerGetIssueTool:
		return e.executeTrackerGetIssue(ctx, arguments)
	case trackerUpdateStateTool:
		return e.executeTrackerUpdateState(ctx, arguments)
	case trackerUpsertWorkpadCommentTool:
		return e.executeTrackerUpsertWorkpadComment(ctx, arguments)
	case trackerAddLinkTool:
		return e.executeTrackerAddLink(ctx, arguments)
	default:
		return failureResponse(map[string]any{
			"error": map[string]any{
				"message":        fmt.Sprintf("Unsupported dynamic tool: %q.", tool),
				"supportedTools": supportedToolNames(),
			},
		})
	}
}

func supportedToolNames() []any {
	return []any{
		trackerGetIssueTool,
		trackerUpdateStateTool,
		trackerUpsertWorkpadCommentTool,
		trackerAddLinkTool,
	}
}

func (e *DynamicToolExecutor) executeLinearGraphQL(ctx context.Context, arguments any) map[string]any {
	query, variables, err := normalizeLinearGraphQLArguments(arguments)
	if err != nil {
		return failureResponse(toolErrorPayload(err))
	}

	token := e.config.LinearAPIToken()
	if strings.TrimSpace(token) == "" {
		return failureResponse(toolErrorPayload(config.ErrMissingLinearAPIToken))
	}

	payload := map[string]any{
		"query":     query,
		"variables": variables,
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return failureResponse(toolErrorPayload(err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.config.LinearEndpoint(), bytes.NewReader(rawPayload))
	if err != nil {
		return failureResponse(toolErrorPayload(&linearRequestError{Reason: err}))
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return failureResponse(toolErrorPayload(&linearRequestError{Reason: err}))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return failureResponse(toolErrorPayload(err))
	}

	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		decoded = strings.TrimSpace(string(body))
	}

	if resp.StatusCode != http.StatusOK {
		return failureResponse(toolErrorPayload(&linearStatusError{Status: resp.StatusCode}))
	}

	responseMap, ok := decoded.(map[string]any)
	if !ok {
		return failureResponse(toolErrorPayload(map[string]any{
			"message": "Linear GraphQL response is not a JSON object.",
			"body":    decoded,
		}))
	}

	if errors, ok := responseMap["errors"].([]any); ok && len(errors) > 0 {
		return map[string]any{
			"success": false,
			"contentItems": []any{
				map[string]any{
					"type": "inputText",
					"text": encodePayload(responseMap),
				},
			},
		}
	}

	return map[string]any{
		"success": true,
		"contentItems": []any{
			map[string]any{
				"type": "inputText",
				"text": encodePayload(responseMap),
			},
		},
	}
}

func (e *DynamicToolExecutor) executeTrackerGetIssue(ctx context.Context, arguments any) map[string]any {
	issueID, _, _, err := parseTrackerCommonArguments(arguments)
	if err != nil {
		return failureResponse(trackerToolErrorPayload(err))
	}
	client := tracker.NewClient(e.config)
	issues, err := client.FetchIssueStatesByIDs(ctx, []string{issueID})
	if err != nil {
		return failureResponse(trackerToolErrorPayload(err))
	}
	if len(issues) == 0 {
		return failureResponse(trackerToolErrorPayload(errIssueNotFound))
	}
	return successResponse(map[string]any{"issue": issuePayload(issues[0])})
}

func (e *DynamicToolExecutor) executeTrackerUpdateState(ctx context.Context, arguments any) map[string]any {
	issueID, state, _, err := parseTrackerCommonArguments(arguments)
	if err != nil {
		return failureResponse(trackerToolErrorPayload(err))
	}
	if strings.TrimSpace(state) == "" {
		return failureResponse(trackerToolErrorPayload(errMissingState))
	}
	client := tracker.NewClient(e.config)
	if err := client.UpdateIssueState(ctx, issueID, strings.TrimSpace(state)); err != nil {
		return failureResponse(trackerToolErrorPayload(err))
	}
	return successResponse(map[string]any{
		"issue_id": issueID,
		"state":    strings.TrimSpace(state),
		"updated":  true,
	})
}

func (e *DynamicToolExecutor) executeTrackerUpsertWorkpadComment(ctx context.Context, arguments any) map[string]any {
	issueID, _, body, err := parseTrackerCommonArguments(arguments)
	if err != nil {
		return failureResponse(trackerToolErrorPayload(err))
	}
	if strings.TrimSpace(body) == "" {
		return failureResponse(trackerToolErrorPayload(errMissingBody))
	}
	client := tracker.NewClient(e.config)
	if err := client.CreateComment(ctx, issueID, body); err != nil {
		return failureResponse(trackerToolErrorPayload(err))
	}
	return successResponse(map[string]any{
		"issue_id": issueID,
		"updated":  true,
	})
}

func (e *DynamicToolExecutor) executeTrackerAddLink(ctx context.Context, arguments any) map[string]any {
	params, ok := arguments.(map[string]any)
	if !ok || params == nil {
		return failureResponse(trackerToolErrorPayload(errInvalidArguments))
	}
	issueID := strings.TrimSpace(stringFromMap(params, "issue_id"))
	if issueID == "" {
		return failureResponse(trackerToolErrorPayload(errMissingIssueID))
	}
	url := strings.TrimSpace(stringFromMap(params, "url"))
	if url == "" {
		return failureResponse(trackerToolErrorPayload(errMissingURL))
	}
	title := strings.TrimSpace(stringFromMap(params, "title"))
	if title == "" {
		title = url
	}
	client := tracker.NewClient(e.config)
	if err := client.AddLink(ctx, issueID, url, title); err != nil {
		return failureResponse(trackerToolErrorPayload(err))
	}
	return successResponse(map[string]any{
		"issue_id": issueID,
		"url":      url,
		"title":    title,
		"updated":  true,
	})
}

func parseTrackerCommonArguments(arguments any) (string, string, string, error) {
	params, ok := arguments.(map[string]any)
	if !ok || params == nil {
		return "", "", "", errInvalidArguments
	}
	issueID := strings.TrimSpace(stringFromMap(params, "issue_id"))
	if issueID == "" {
		return "", "", "", errMissingIssueID
	}
	return issueID, stringFromMap(params, "state"), stringFromMap(params, "body"), nil
}

func stringFromMap(values map[string]any, key string) string {
	typed, _ := values[key].(string)
	return typed
}

func issuePayload(issue tracker.Issue) map[string]any {
	priority := any(nil)
	if issue.Priority != nil {
		priority = *issue.Priority
	}
	return map[string]any{
		"id":                 issue.ID,
		"identifier":         issue.Identifier,
		"title":              issue.Title,
		"description":        issue.Description,
		"state":              issue.State,
		"url":                issue.URL,
		"branch_name":        issue.BranchName,
		"assignee_id":        issue.AssigneeID,
		"labels":             issue.LabelNames(),
		"assigned_to_worker": issue.AssignedToWorker,
		"priority":           priority,
	}
}

func normalizeLinearGraphQLArguments(arguments any) (string, map[string]any, error) {
	switch typed := arguments.(type) {
	case string:
		query := strings.TrimSpace(typed)
		if query == "" {
			return "", nil, errMissingQuery
		}
		if !hasExactlyOneGraphQLOperation(query) {
			return "", nil, errInvalidOperationCount
		}
		return query, map[string]any{}, nil
	case map[string]any:
		rawQuery, _ := typed["query"].(string)
		query := strings.TrimSpace(rawQuery)
		if query == "" {
			return "", nil, errMissingQuery
		}
		if !hasExactlyOneGraphQLOperation(query) {
			return "", nil, errInvalidOperationCount
		}
		if rawVariables, ok := typed["variables"]; ok && rawVariables != nil {
			variables, ok := rawVariables.(map[string]any)
			if !ok {
				return "", nil, errInvalidVariables
			}
			return query, variables, nil
		}
		return query, map[string]any{}, nil
	default:
		return "", nil, errInvalidArguments
	}
}

func hasExactlyOneGraphQLOperation(query string) bool {
	text := strings.TrimSpace(query)
	if text == "" {
		return false
	}

	const (
		stateNormal = iota
		stateComment
		stateString
		stateBlockString
	)

	state := stateNormal
	braceDepth := 0
	expectSelectionSet := false
	opCount := 0

	for i := 0; i < len(text); i++ {
		ch := text[i]

		switch state {
		case stateComment:
			if ch == '\n' || ch == '\r' {
				state = stateNormal
			}
			continue
		case stateString:
			if ch == '\\' && i+1 < len(text) {
				i++
				continue
			}
			if ch == '"' {
				state = stateNormal
			}
			continue
		case stateBlockString:
			if i+2 < len(text) && text[i] == '"' && text[i+1] == '"' && text[i+2] == '"' {
				state = stateNormal
				i += 2
			}
			continue
		}

		if ch == '#' {
			state = stateComment
			continue
		}
		if i+2 < len(text) && text[i] == '"' && text[i+1] == '"' && text[i+2] == '"' {
			state = stateBlockString
			i += 2
			continue
		}
		if ch == '"' {
			state = stateString
			continue
		}

		if braceDepth == 0 {
			if isGraphQLIgnoredByte(ch) {
				continue
			}

			if isGraphQLNameStart(ch) {
				start := i
				for i+1 < len(text) && isGraphQLNameContinue(text[i+1]) {
					i++
				}
				token := strings.ToLower(text[start : i+1])
				switch token {
				case "query", "mutation", "subscription":
					opCount++
					if opCount > 1 {
						return false
					}
					expectSelectionSet = true
				case "fragment":
					expectSelectionSet = true
				}
				continue
			}

			if ch == '{' {
				if expectSelectionSet {
					expectSelectionSet = false
				} else {
					opCount++
					if opCount > 1 {
						return false
					}
				}
			}
		}

		if ch == '{' {
			braceDepth++
		} else if ch == '}' && braceDepth > 0 {
			braceDepth--
			if braceDepth == 0 {
				expectSelectionSet = false
			}
		}
	}

	return opCount == 1
}

func isGraphQLIgnoredByte(ch byte) bool {
	switch ch {
	case ' ', '\t', '\n', '\r', '\f', ',', 0xEF, 0xBB, 0xBF:
		return true
	default:
		return false
	}
}

func isGraphQLNameStart(ch byte) bool {
	return ch == '_' || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z')
}

func isGraphQLNameContinue(ch byte) bool {
	return isGraphQLNameStart(ch) || (ch >= '0' && ch <= '9')
}

func toolErrorPayload(reason any) map[string]any {
	err, _ := reason.(error)
	switch {
	case errors.Is(err, errMissingQuery):
		return map[string]any{
			"error": map[string]any{
				"message": "`linear_graphql` requires a non-empty `query` string.",
			},
		}
	case errors.Is(err, errInvalidArguments):
		return map[string]any{
			"error": map[string]any{
				"message": "`linear_graphql` expects either a GraphQL query string or an object with `query` and optional `variables`.",
			},
		}
	case errors.Is(err, errInvalidVariables):
		return map[string]any{
			"error": map[string]any{
				"message": "`linear_graphql.variables` must be a JSON object when provided.",
			},
		}
	case errors.Is(err, errInvalidOperationCount):
		return map[string]any{
			"error": map[string]any{
				"message": "`linear_graphql.query` must contain exactly one GraphQL operation.",
			},
		}
	case errors.Is(err, config.ErrMissingLinearAPIToken):
		return map[string]any{
			"error": map[string]any{
				"message": "Baton is missing Linear auth. Set `tracker.linear.api_key` in `WORKFLOW.md` or export `LINEAR_API_KEY`.",
			},
		}
	default:
		statusErr := new(linearStatusError)
		if errors.As(err, &statusErr) {
			return map[string]any{
				"error": map[string]any{
					"message": fmt.Sprintf("Linear GraphQL request failed with HTTP %d.", statusErr.Status),
					"status":  statusErr.Status,
				},
			}
		}
		requestErr := new(linearRequestError)
		if errors.As(err, &requestErr) {
			return map[string]any{
				"error": map[string]any{
					"message": "Linear GraphQL request failed before receiving a successful response.",
					"reason":  fmt.Sprint(requestErr.Reason),
				},
			}
		}
		return map[string]any{
			"error": map[string]any{
				"message": "Linear GraphQL tool execution failed.",
				"reason":  fmt.Sprint(reason),
			},
		}
	}
}

func trackerToolErrorPayload(reason any) map[string]any {
	err, _ := reason.(error)
	switch {
	case errors.Is(err, errInvalidArguments):
		return map[string]any{
			"error": map[string]any{
				"code":    "invalid_arguments",
				"message": "Tracker tools require a JSON object argument.",
			},
		}
	case errors.Is(err, errMissingIssueID):
		return map[string]any{
			"error": map[string]any{
				"code":    "invalid_arguments",
				"message": "`issue_id` is required.",
			},
		}
	case errors.Is(err, errMissingState):
		return map[string]any{
			"error": map[string]any{
				"code":    "invalid_arguments",
				"message": "`state` is required.",
			},
		}
	case errors.Is(err, errMissingBody):
		return map[string]any{
			"error": map[string]any{
				"code":    "invalid_arguments",
				"message": "`body` is required.",
			},
		}
	case errors.Is(err, errMissingURL):
		return map[string]any{
			"error": map[string]any{
				"code":    "invalid_arguments",
				"message": "`url` is required.",
			},
		}
	case errors.Is(err, errIssueNotFound):
		return map[string]any{
			"error": map[string]any{
				"code":    "not_found",
				"message": "Issue not found.",
			},
		}
	case errors.Is(err, tracker.ErrStateNotFound):
		return map[string]any{
			"error": map[string]any{
				"code":    "not_found",
				"message": "Target state not found for this issue.",
			},
		}
	case errors.Is(err, tracker.ErrMissingLinearAPIToken):
		return map[string]any{
			"error": map[string]any{
				"code":    "permission",
				"message": "Missing tracker credentials for Linear.",
			},
		}
	default:
		return map[string]any{
			"error": map[string]any{
				"code":    "execution_error",
				"message": "Tracker tool execution failed.",
				"reason":  fmt.Sprint(reason),
			},
		}
	}
}

func successResponse(payload map[string]any) map[string]any {
	return map[string]any{
		"success": true,
		"contentItems": []any{
			map[string]any{
				"type": "inputText",
				"text": encodePayload(payload),
			},
		},
	}
}

func failureResponse(payload map[string]any) map[string]any {
	return map[string]any{
		"success": false,
		"contentItems": []any{
			map[string]any{
				"type": "inputText",
				"text": encodePayload(payload),
			},
		},
	}
}

func encodePayload(payload any) string {
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Sprint(payload)
	}
	return string(encoded)
}
