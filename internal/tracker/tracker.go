package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"baton/internal/config"
)

const (
	issuePageSize  = 50
	requestTimeout = 30 * time.Second
)

var (
	ErrMissingLinearAPIToken       = errors.New("missing_linear_api_token")
	ErrMissingLinearProjectSlug    = errors.New("missing_linear_project_slug")
	ErrMissingLinearViewerIdentity = errors.New("missing_linear_viewer_identity")
	ErrLinearAPIRequest            = errors.New("linear_api_request")
	ErrLinearAPIStatus             = errors.New("linear_api_status")
	ErrLinearGraphQLErrors         = errors.New("linear_graphql_errors")
	ErrLinearUnknownPayload        = errors.New("linear_unknown_payload")
	ErrLinearMissingEndCursor      = errors.New("linear_missing_end_cursor")
	ErrCommentCreateFailed         = errors.New("comment_create_failed")
	ErrIssueUpdateFailed           = errors.New("issue_update_failed")
	ErrStateNotFound               = errors.New("state_not_found")
)

const linearQuery = `
query SymphonyLinearPoll($projectSlug: String!, $stateNames: [String!]!, $first: Int!, $relationFirst: Int!, $after: String) {
  issues(filter: {project: {slugId: {eq: $projectSlug}}, state: {name: {in: $stateNames}}}, first: $first, after: $after) {
    nodes {
      id
      identifier
      title
      description
      priority
      state { name }
      branchName
      url
      assignee { id }
      labels { nodes { name } }
      inverseRelations(first: $relationFirst) {
        nodes {
          type
          issue { id identifier state { name } }
        }
      }
      createdAt
      updatedAt
    }
    pageInfo { hasNextPage endCursor }
  }
}`

const linearQueryByIDs = `
query SymphonyLinearIssuesById($ids: [ID!]!, $first: Int!, $relationFirst: Int!) {
  issues(filter: {id: {in: $ids}}, first: $first) {
    nodes {
      id
      identifier
      title
      description
      priority
      state { name }
      branchName
      url
      assignee { id }
      labels { nodes { name } }
      inverseRelations(first: $relationFirst) {
        nodes {
          type
          issue { id identifier state { name } }
        }
      }
      createdAt
      updatedAt
    }
  }
}`

const viewerQuery = `
query SymphonyLinearViewer {
  viewer { id }
}`

const createCommentMutation = `
mutation SymphonyCreateComment($issueId: String!, $body: String!) {
  commentCreate(input: {issueId: $issueId, body: $body}) {
    success
  }
}`

const updateStateMutation = `
mutation SymphonyUpdateIssueState($issueId: String!, $stateId: String!) {
  issueUpdate(id: $issueId, input: {stateId: $stateId}) {
    success
  }
}`

const stateLookupQuery = `
query SymphonyResolveStateId($issueId: String!, $stateName: String!) {
  issue(id: $issueId) {
    team {
      states(filter: {name: {eq: $stateName}}, first: 1) {
        nodes {
          id
        }
      }
    }
  }
}`

type BlockerRef struct {
	ID         string
	Identifier string
	State      string
}

type Issue struct {
	ID               string
	Identifier       string
	Title            string
	Description      string
	Priority         *int
	State            string
	BranchName       string
	URL              string
	AssigneeID       string
	BlockedBy        []BlockerRef
	Labels           []string
	AssignedToWorker bool
	CreatedAt        *time.Time
	UpdatedAt        *time.Time
}

func (i Issue) LabelNames() []string {
	labels := make([]string, len(i.Labels))
	copy(labels, i.Labels)
	return labels
}

type Client interface {
	FetchCandidateIssues(ctx context.Context) ([]Issue, error)
	FetchIssuesByStates(ctx context.Context, states []string) ([]Issue, error)
	FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]Issue, error)
	CreateComment(ctx context.Context, issueID string, body string) error
	UpdateIssueState(ctx context.Context, issueID string, stateName string) error
}

type linearClient struct {
	config     *config.Config
	httpClient *http.Client
}

type GraphQLRequestError struct {
	Cause error
}

func (e *GraphQLRequestError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %v", ErrLinearAPIRequest, e.Cause)
}

func (e *GraphQLRequestError) Unwrap() error {
	return ErrLinearAPIRequest
}

type GraphQLStatusError struct {
	Status int
	Body   any
}

func (e *GraphQLStatusError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: status=%d body=%v", ErrLinearAPIStatus, e.Status, e.Body)
}

func (e *GraphQLStatusError) Unwrap() error {
	return ErrLinearAPIStatus
}

type GraphQLErrorsError struct {
	Errors any
}

func (e *GraphQLErrorsError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %v", ErrLinearGraphQLErrors, e.Errors)
}

func (e *GraphQLErrorsError) Unwrap() error {
	return ErrLinearGraphQLErrors
}

func NewLinearClient(cfg *config.Config) Client {
	return &linearClient{
		config: cfg,
		httpClient: &http.Client{
			Timeout: requestTimeout,
		},
	}
}

func (c *linearClient) FetchCandidateIssues(ctx context.Context) ([]Issue, error) {
	if c.config.LinearAPIToken() == "" {
		return nil, ErrMissingLinearAPIToken
	}
	if c.config.LinearProjectSlug() == "" {
		return nil, ErrMissingLinearProjectSlug
	}

	assigneeFilter, err := c.routingAssigneeFilter(ctx)
	if err != nil {
		return nil, err
	}

	return c.fetchByStatesPaged(
		ctx,
		c.config.LinearProjectSlug(),
		c.config.LinearActiveStates(),
		assigneeFilter,
	)
}

func (c *linearClient) FetchIssuesByStates(ctx context.Context, states []string) ([]Issue, error) {
	normalizedStates := orderedUniqueStrings(states)
	if len(normalizedStates) == 0 {
		return []Issue{}, nil
	}
	if c.config.LinearAPIToken() == "" {
		return nil, ErrMissingLinearAPIToken
	}
	if c.config.LinearProjectSlug() == "" {
		return nil, ErrMissingLinearProjectSlug
	}

	return c.fetchByStatesPaged(ctx, c.config.LinearProjectSlug(), normalizedStates, nil)
}

func (c *linearClient) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]Issue, error) {
	uniqueIDs := orderedUniqueStrings(ids)
	if len(uniqueIDs) == 0 {
		return []Issue{}, nil
	}

	assigneeFilter, err := c.routingAssigneeFilter(ctx)
	if err != nil {
		return nil, err
	}

	first := min(len(uniqueIDs), issuePageSize)
	body, err := c.graphql(ctx, linearQueryByIDs, map[string]any{
		"ids":           uniqueIDs,
		"first":         first,
		"relationFirst": issuePageSize,
	}, "")
	if err != nil {
		return nil, err
	}

	return decodeLinearResponse(body, assigneeFilter)
}

func (c *linearClient) CreateComment(ctx context.Context, issueID string, body string) error {
	response, err := c.graphql(ctx, createCommentMutation, map[string]any{
		"issueId": issueID,
		"body":    body,
	}, "")
	if err != nil {
		return err
	}
	if boolPath(response, "data", "commentCreate", "success") {
		return nil
	}
	return ErrCommentCreateFailed
}

func (c *linearClient) UpdateIssueState(ctx context.Context, issueID string, stateName string) error {
	stateLookup, err := c.graphql(ctx, stateLookupQuery, map[string]any{
		"issueId":   issueID,
		"stateName": stateName,
	}, "")
	if err != nil {
		return err
	}
	stateID := stringPath(stateLookup, "data", "issue", "team", "states", "nodes", "0", "id")
	if strings.TrimSpace(stateID) == "" {
		return ErrStateNotFound
	}

	updateResp, err := c.graphql(ctx, updateStateMutation, map[string]any{
		"issueId": issueID,
		"stateId": stateID,
	}, "")
	if err != nil {
		return err
	}
	if boolPath(updateResp, "data", "issueUpdate", "success") {
		return nil
	}
	return ErrIssueUpdateFailed
}

func (c *linearClient) fetchByStatesPaged(
	ctx context.Context,
	projectSlug string,
	stateNames []string,
	assigneeFilter *assigneeFilter,
) ([]Issue, error) {
	after := ""
	acc := make([][]Issue, 0, 4)

	for {
		var afterValue any
		if after == "" {
			afterValue = nil
		} else {
			afterValue = after
		}

		body, err := c.graphql(ctx, linearQuery, map[string]any{
			"projectSlug":   projectSlug,
			"stateNames":    stateNames,
			"first":         issuePageSize,
			"relationFirst": issuePageSize,
			"after":         afterValue,
		}, "")
		if err != nil {
			return nil, err
		}

		issues, pageInfo, err := decodeLinearPageResponse(body, assigneeFilter)
		if err != nil {
			return nil, err
		}
		acc = append(acc, issues)

		nextCursor, done, err := nextPageCursor(pageInfo)
		if err != nil {
			return nil, err
		}
		if done {
			return mergeIssuePages(acc), nil
		}
		after = nextCursor
	}
}

func (c *linearClient) graphql(
	ctx context.Context,
	query string,
	variables map[string]any,
	operationName string,
) (map[string]any, error) {
	token := c.config.LinearAPIToken()
	if token == "" {
		return nil, ErrMissingLinearAPIToken
	}

	payload := map[string]any{
		"query":     query,
		"variables": variables,
	}
	if strings.TrimSpace(operationName) != "" {
		payload["operationName"] = strings.TrimSpace(operationName)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &GraphQLRequestError{Cause: err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.LinearEndpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, &GraphQLRequestError{Cause: err}
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &GraphQLRequestError{Cause: err}
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &GraphQLRequestError{Cause: err}
	}

	decoded := decodeJSONOrString(rawBody)
	if resp.StatusCode != http.StatusOK {
		return nil, &GraphQLStatusError{
			Status: resp.StatusCode,
			Body:   decoded,
		}
	}

	bodyMap, ok := decoded.(map[string]any)
	if !ok {
		return nil, ErrLinearUnknownPayload
	}
	return bodyMap, nil
}

type pageInfo struct {
	HasNextPage bool
	EndCursor   string
}

func decodeLinearPageResponse(body map[string]any, filter *assigneeFilter) ([]Issue, pageInfo, error) {
	data, _ := body["data"].(map[string]any)
	issuesEnvelope, _ := data["issues"].(map[string]any)
	nodes, nodesOK := issuesEnvelope["nodes"].([]any)
	pageInfoMap, pageInfoOK := issuesEnvelope["pageInfo"].(map[string]any)

	if nodesOK && pageInfoOK {
		issues, err := decodeLinearNodes(nodes, filter)
		if err != nil {
			return nil, pageInfo{}, err
		}
		return issues, pageInfo{
			HasNextPage: boolValue(pageInfoMap["hasNextPage"]),
			EndCursor:   stringValue(pageInfoMap["endCursor"]),
		}, nil
	}

	issues, err := decodeLinearResponse(body, filter)
	if err != nil {
		return nil, pageInfo{}, err
	}
	return issues, pageInfo{}, nil
}

func decodeLinearResponse(body map[string]any, filter *assigneeFilter) ([]Issue, error) {
	if errorsPayload, ok := body["errors"]; ok {
		return nil, &GraphQLErrorsError{Errors: errorsPayload}
	}

	data, ok := body["data"].(map[string]any)
	if !ok {
		return nil, ErrLinearUnknownPayload
	}
	issuesEnvelope, ok := data["issues"].(map[string]any)
	if !ok {
		return nil, ErrLinearUnknownPayload
	}
	nodes, ok := issuesEnvelope["nodes"].([]any)
	if !ok {
		return nil, ErrLinearUnknownPayload
	}

	return decodeLinearNodes(nodes, filter)
}

func decodeLinearNodes(nodes []any, filter *assigneeFilter) ([]Issue, error) {
	out := make([]Issue, 0, len(nodes))
	for _, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		issue := normalizeIssue(node, filter)
		out = append(out, issue)
	}
	return out, nil
}

func nextPageCursor(info pageInfo) (string, bool, error) {
	if info.HasNextPage {
		if strings.TrimSpace(info.EndCursor) == "" {
			return "", false, ErrLinearMissingEndCursor
		}
		return info.EndCursor, false, nil
	}
	return "", true, nil
}

func mergeIssuePages(pages [][]Issue) []Issue {
	total := 0
	for _, page := range pages {
		total += len(page)
	}
	merged := make([]Issue, 0, total)
	for _, page := range pages {
		merged = append(merged, page...)
	}
	return merged
}

type assigneeFilter struct {
	matchValues map[string]struct{}
}

func (c *linearClient) routingAssigneeFilter(ctx context.Context) (*assigneeFilter, error) {
	assignee := c.config.LinearAssignee()
	if assignee == "" {
		return nil, nil
	}
	return c.buildAssigneeFilter(ctx, assignee)
}

func (c *linearClient) buildAssigneeFilter(ctx context.Context, assignee string) (*assigneeFilter, error) {
	normalized := normalizeAssigneeMatchValue(assignee)
	if normalized == "" {
		return nil, nil
	}
	if normalized == "me" {
		body, err := c.graphql(ctx, viewerQuery, map[string]any{}, "")
		if err != nil {
			return nil, err
		}
		data, _ := body["data"].(map[string]any)
		viewer, _ := data["viewer"].(map[string]any)
		viewerID := normalizeAssigneeMatchValue(stringValue(viewer["id"]))
		if viewerID == "" {
			return nil, ErrMissingLinearViewerIdentity
		}
		return &assigneeFilter{
			matchValues: map[string]struct{}{viewerID: {}},
		}, nil
	}

	return &assigneeFilter{
		matchValues: map[string]struct{}{normalized: {}},
	}, nil
}

func normalizeIssue(issue map[string]any, filter *assigneeFilter) Issue {
	assignee, _ := issue["assignee"].(map[string]any)
	assigneeID := stringValue(assignee["id"])

	return Issue{
		ID:               stringValue(issue["id"]),
		Identifier:       stringValue(issue["identifier"]),
		Title:            stringValue(issue["title"]),
		Description:      stringValue(issue["description"]),
		Priority:         parsePriority(issue["priority"]),
		State:            nestedString(issue, "state", "name"),
		BranchName:       stringValue(issue["branchName"]),
		URL:              stringValue(issue["url"]),
		AssigneeID:       assigneeID,
		BlockedBy:        extractBlockers(issue),
		Labels:           extractLabels(issue),
		AssignedToWorker: assignedToWorker(assigneeID, filter),
		CreatedAt:        parseDateTime(issue["createdAt"]),
		UpdatedAt:        parseDateTime(issue["updatedAt"]),
	}
}

func assignedToWorker(assigneeID string, filter *assigneeFilter) bool {
	if filter == nil {
		return true
	}
	normalized := normalizeAssigneeMatchValue(assigneeID)
	if normalized == "" {
		return false
	}
	_, ok := filter.matchValues[normalized]
	return ok
}

func normalizeAssigneeMatchValue(value string) string {
	return strings.TrimSpace(value)
}

func extractLabels(issue map[string]any) []string {
	labelsEnvelope, _ := issue["labels"].(map[string]any)
	nodes, _ := labelsEnvelope["nodes"].([]any)
	out := make([]string, 0, len(nodes))
	for _, rawNode := range nodes {
		node, ok := rawNode.(map[string]any)
		if !ok {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(stringValue(node["name"])))
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

func extractBlockers(issue map[string]any) []BlockerRef {
	inverseRelations, _ := issue["inverseRelations"].(map[string]any)
	nodes, _ := inverseRelations["nodes"].([]any)
	out := make([]BlockerRef, 0, len(nodes))
	for _, rawRelation := range nodes {
		relation, ok := rawRelation.(map[string]any)
		if !ok {
			continue
		}
		relationType := strings.ToLower(strings.TrimSpace(stringValue(relation["type"])))
		if relationType != "blocks" {
			continue
		}
		blockerIssue, _ := relation["issue"].(map[string]any)
		out = append(out, BlockerRef{
			ID:         stringValue(blockerIssue["id"]),
			Identifier: stringValue(blockerIssue["identifier"]),
			State:      nestedString(blockerIssue, "state", "name"),
		})
	}
	return out
}

func parseDateTime(value any) *time.Time {
	raw := strings.TrimSpace(stringValue(value))
	if raw == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil
	}
	return &parsed
}

func parsePriority(value any) *int {
	switch typed := value.(type) {
	case int:
		v := typed
		return &v
	case int64:
		v := int(typed)
		return &v
	case float64:
		if typed == float64(int(typed)) {
			v := int(typed)
			return &v
		}
		return nil
	default:
		return nil
	}
}

func orderedUniqueStrings(input []string) []string {
	out := make([]string, 0, len(input))
	seen := map[string]struct{}{}
	for _, value := range input {
		normalized := strings.TrimSpace(value)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

func decodeJSONOrString(raw []byte) any {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err == nil {
		return decoded
	}
	return strings.TrimSpace(string(raw))
}

func nestedString(root map[string]any, path ...string) string {
	current := any(root)
	for _, segment := range path {
		currentMap, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current, ok = currentMap[segment]
		if !ok {
			return ""
		}
	}
	return stringValue(current)
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return ""
	}
}

func boolValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	default:
		return false
	}
}

func boolPath(root map[string]any, segments ...string) bool {
	value, ok := mapPath(root, segments...)
	if !ok {
		return false
	}
	return boolValue(value)
}

func stringPath(root map[string]any, segments ...string) string {
	value, ok := mapPath(root, segments...)
	if !ok {
		return ""
	}
	return stringValue(value)
}

func mapPath(root map[string]any, segments ...string) (any, bool) {
	var current any = root
	for _, segment := range segments {
		switch typed := current.(type) {
		case map[string]any:
			next, exists := typed[segment]
			if !exists {
				return nil, false
			}
			current = next
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(typed) {
				return nil, false
			}
			current = typed[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func min(a int, b int) int {
	return slices.Min([]int{a, b})
}
