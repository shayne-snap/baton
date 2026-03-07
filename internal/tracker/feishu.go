package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"baton/internal/config"
)

var (
	ErrMissingFeishuAuth       = errors.New("missing_feishu_auth")
	ErrFeishuAPIRequest        = errors.New("feishu_api_request")
	ErrFeishuAPIStatus         = errors.New("feishu_api_status")
	ErrFeishuUnknownPayload    = errors.New("feishu_unknown_payload")
	ErrFeishuAccessTokenFailed = errors.New("feishu_access_token_failed")
	ErrFeishuStateNotFound     = errors.New("feishu_state_not_found")
)

type feishuClient struct {
	config     *config.Config
	httpClient *http.Client

	mu            sync.Mutex
	tenantToken   string
	tenantTokenAt time.Time
}

type FeishuRequestError struct {
	Cause error
}

func (e *FeishuRequestError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %v", ErrFeishuAPIRequest, e.Cause)
}

func (e *FeishuRequestError) Unwrap() error {
	return ErrFeishuAPIRequest
}

type FeishuStatusError struct {
	Status int
	Body   any
}

func (e *FeishuStatusError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: status=%d body=%v", ErrFeishuAPIStatus, e.Status, e.Body)
}

func (e *FeishuStatusError) Unwrap() error {
	return ErrFeishuAPIStatus
}

func NewFeishuClient(cfg *config.Config) Client {
	return &feishuClient{
		config: cfg,
		httpClient: &http.Client{
			Timeout: requestTimeout,
		},
	}
}

func (c *feishuClient) FetchCandidateIssues(ctx context.Context) ([]Issue, error) {
	return c.FetchIssuesByStates(ctx, c.config.TrackerActiveStates())
}

func (c *feishuClient) FetchIssuesByStates(ctx context.Context, states []string) ([]Issue, error) {
	normalizedStates := orderedUniqueStrings(states)
	if len(normalizedStates) == 0 {
		return []Issue{}, nil
	}
	projectKey := strings.TrimSpace(c.config.FeishuProjectKey())
	if projectKey == "" {
		return nil, config.ErrMissingFeishuProjectKey
	}

	pageNum := 1
	acc := make([]Issue, 0, issuePageSize)
	for {
		payload := map[string]any{
			"project_key": projectKey,
			"page_size":   issuePageSize,
			"page_num":    pageNum,
			"states":      normalizedStates,
		}
		if assignee := strings.TrimSpace(c.config.TrackerAssignee()); assignee != "" {
			payload["assignee"] = assignee
		}

		body, err := c.requestJSON(ctx, http.MethodPost, "/open-apis/project/v1/work_item/filter", payload)
		if err != nil {
			return nil, err
		}
		pageIssues := decodeFeishuIssues(body, c.config.TrackerAssignee())
		acc = append(acc, pageIssues...)

		if !feishuHasMore(body) {
			break
		}
		pageNum++
	}
	return dedupeIssuesByID(acc), nil
}

func (c *feishuClient) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]Issue, error) {
	uniqueIDs := orderedUniqueStrings(ids)
	if len(uniqueIDs) == 0 {
		return []Issue{}, nil
	}
	projectKey := strings.TrimSpace(c.config.FeishuProjectKey())
	if projectKey == "" {
		return nil, config.ErrMissingFeishuProjectKey
	}

	payload := map[string]any{
		"project_key":   projectKey,
		"work_item_ids": uniqueIDs,
	}
	body, err := c.requestJSON(ctx, http.MethodPost, "/open-apis/project/v1/work_item/get_work_items_by_ids", payload)
	if err != nil {
		return nil, err
	}
	return decodeFeishuIssues(body, c.config.TrackerAssignee()), nil
}

func (c *feishuClient) CreateComment(ctx context.Context, issueID string, body string) error {
	issueID = strings.TrimSpace(issueID)
	commentBody := strings.TrimSpace(body)
	if issueID == "" || commentBody == "" {
		return ErrCommentCreateFailed
	}
	projectKey := strings.TrimSpace(c.config.FeishuProjectKey())
	if projectKey == "" {
		return config.ErrMissingFeishuProjectKey
	}

	payload := map[string]any{
		"project_key":  projectKey,
		"work_item_id": issueID,
		"content":      commentBody,
	}
	_, err := c.requestJSON(ctx, http.MethodPost, "/open-apis/project/v1/comment/create", payload)
	return err
}

func (c *feishuClient) UpdateIssueState(ctx context.Context, issueID string, stateName string) error {
	issueID = strings.TrimSpace(issueID)
	targetState := strings.TrimSpace(stateName)
	if issueID == "" || targetState == "" {
		return ErrIssueUpdateFailed
	}
	projectKey := strings.TrimSpace(c.config.FeishuProjectKey())
	if projectKey == "" {
		return config.ErrMissingFeishuProjectKey
	}

	payload := map[string]any{
		"project_key":  projectKey,
		"work_item_id": issueID,
		"state":        targetState,
	}
	body, err := c.requestJSON(ctx, http.MethodPost, "/open-apis/project/v1/work_item/update_state_flow", payload)
	if err != nil {
		return err
	}
	if !feishuStateUpdated(body) {
		return ErrFeishuStateNotFound
	}
	return nil
}

func (c *feishuClient) AddLink(ctx context.Context, issueID string, linkURL string, title string) error {
	linkURL = strings.TrimSpace(linkURL)
	if linkURL == "" {
		return ErrLinkAddFailed
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = linkURL
	}
	return c.CreateComment(ctx, issueID, fmt.Sprintf("Added link: [%s](%s)", title, linkURL))
}

func (c *feishuClient) requestJSON(ctx context.Context, method string, path string, payload any) (map[string]any, error) {
	base := strings.TrimRight(strings.TrimSpace(c.config.FeishuBaseURL()), "/")
	if base == "" {
		return nil, config.ErrMissingFeishuBaseURL
	}

	token, err := c.tenantAccessToken(ctx)
	if err != nil {
		return nil, err
	}

	var reader io.Reader
	if payload != nil {
		rawPayload, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return nil, &FeishuRequestError{Cause: marshalErr}
		}
		reader = bytes.NewReader(rawPayload)
	}

	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return nil, &FeishuRequestError{Cause: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &FeishuRequestError{Cause: err}
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &FeishuRequestError{Cause: err}
	}
	if len(rawBody) == 0 {
		rawBody = []byte("{}")
	}
	decoded := decodeJSONOrString(rawBody)

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &FeishuStatusError{Status: resp.StatusCode, Body: decoded}
	}

	bodyMap, ok := decoded.(map[string]any)
	if !ok {
		return nil, ErrFeishuUnknownPayload
	}
	if !feishuEnvelopeSuccess(bodyMap) {
		return nil, &FeishuStatusError{
			Status: resp.StatusCode,
			Body:   bodyMap,
		}
	}
	return bodyMap, nil
}

func (c *feishuClient) tenantAccessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	cachedToken := c.tenantToken
	cachedAt := c.tenantTokenAt
	c.mu.Unlock()

	if strings.TrimSpace(cachedToken) != "" && time.Since(cachedAt) < 90*time.Minute {
		return cachedToken, nil
	}

	appID := strings.TrimSpace(c.config.FeishuAppID())
	appSecret := strings.TrimSpace(c.config.FeishuAppSecret())
	if appID == "" || appSecret == "" {
		return "", ErrMissingFeishuAuth
	}

	base := strings.TrimRight(strings.TrimSpace(c.config.FeishuBaseURL()), "/")
	if base == "" {
		return "", config.ErrMissingFeishuBaseURL
	}
	payload := map[string]any{
		"app_id":     appID,
		"app_secret": appSecret,
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return "", &FeishuRequestError{Cause: err}
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		base+"/open-apis/auth/v3/tenant_access_token/internal",
		bytes.NewReader(rawPayload),
	)
	if err != nil {
		return "", &FeishuRequestError{Cause: err}
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", &FeishuRequestError{Cause: err}
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", &FeishuRequestError{Cause: err}
	}
	decoded := decodeJSONOrString(rawBody)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", &FeishuStatusError{Status: resp.StatusCode, Body: decoded}
	}
	bodyMap, ok := decoded.(map[string]any)
	if !ok {
		return "", ErrFeishuUnknownPayload
	}
	if !feishuEnvelopeSuccess(bodyMap) {
		return "", ErrFeishuAccessTokenFailed
	}

	token := strings.TrimSpace(stringValue(bodyMap["tenant_access_token"]))
	if token == "" {
		data, _ := bodyMap["data"].(map[string]any)
		token = strings.TrimSpace(stringValue(data["tenant_access_token"]))
	}
	if token == "" {
		return "", ErrFeishuAccessTokenFailed
	}

	c.mu.Lock()
	c.tenantToken = token
	c.tenantTokenAt = time.Now().UTC()
	c.mu.Unlock()

	return token, nil
}

func feishuEnvelopeSuccess(body map[string]any) bool {
	if body == nil {
		return false
	}
	code := strings.TrimSpace(stringValue(body["code"]))
	if code == "" {
		switch typed := body["code"].(type) {
		case float64:
			code = strconv.Itoa(int(typed))
		case int:
			code = strconv.Itoa(typed)
		}
	}
	return code == "" || code == "0"
}

func decodeFeishuIssues(body map[string]any, configuredAssignee string) []Issue {
	data, _ := body["data"].(map[string]any)
	rawIssues := feishuIssueNodes(data)
	filter := normalizeAssigneeMatchValue(configuredAssignee)
	out := make([]Issue, 0, len(rawIssues))
	for _, raw := range rawIssues {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		id := strings.TrimSpace(feishuFirstString(item, "work_item_id", "id", "item_id"))
		identifier := strings.TrimSpace(feishuFirstString(item, "work_item_key", "identifier", "key", "name"))
		if identifier == "" {
			identifier = id
		}
		title := strings.TrimSpace(feishuFirstString(item, "title", "summary", "name"))
		state := strings.TrimSpace(feishuStateName(item))
		assigneeID, assigneeName, assigneeEmail := feishuAssigneeIdentity(item)

		issue := Issue{
			ID:               id,
			Identifier:       identifier,
			Title:            title,
			Description:      strings.TrimSpace(feishuDescription(item["description"])),
			Priority:         parsePriority(feishuPriorityValue(item["priority"])),
			State:            state,
			BranchName:       strings.TrimSpace(feishuFirstString(item, "branch_name")),
			URL:              strings.TrimSpace(feishuFirstString(item, "url")),
			AssigneeID:       assigneeID,
			Labels:           feishuLabels(item),
			AssignedToWorker: feishuAssigneeMatches(filter, assigneeID, assigneeName, assigneeEmail),
			CreatedAt:        parseDateTime(feishuFirstString(item, "created_at", "createdAt")),
			UpdatedAt:        parseDateTime(feishuFirstString(item, "updated_at", "updatedAt")),
		}
		if issue.ID == "" && issue.Identifier == "" {
			continue
		}
		out = append(out, issue)
	}
	return out
}

func feishuIssueNodes(data map[string]any) []any {
	if data == nil {
		return []any{}
	}
	for _, key := range []string{"work_items", "items", "list", "records"} {
		if nodes, ok := data[key].([]any); ok {
			return nodes
		}
	}
	return []any{}
}

func feishuStateName(item map[string]any) string {
	state, _ := item["state"].(map[string]any)
	if state != nil {
		for _, key := range []string{"name", "state_name", "display_name", "key"} {
			if value := strings.TrimSpace(stringValue(state[key])); value != "" {
				return value
			}
		}
	}
	return strings.TrimSpace(feishuFirstString(item, "state_name", "state"))
}

func feishuDescription(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		if content := strings.TrimSpace(stringValue(typed["text"])); content != "" {
			return content
		}
		return strings.TrimSpace(stringValue(typed["content"]))
	default:
		return ""
	}
}

func feishuPriorityValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"value", "id"} {
			if content := strings.TrimSpace(stringValue(typed[key])); content != "" {
				if parsed, err := strconv.Atoi(content); err == nil {
					return float64(parsed)
				}
			}
		}
		return nil
	default:
		return value
	}
}

func feishuAssigneeIdentity(item map[string]any) (string, string, string) {
	for _, key := range []string{"assignee", "owner"} {
		assignee, _ := item[key].(map[string]any)
		if assignee == nil {
			continue
		}
		id := strings.TrimSpace(feishuFirstString(assignee, "user_key", "open_id", "id", "account_id"))
		name := strings.TrimSpace(feishuFirstString(assignee, "name", "display_name"))
		email := strings.TrimSpace(feishuFirstString(assignee, "email"))
		if id != "" || name != "" || email != "" {
			return id, name, email
		}
	}
	return "", "", ""
}

func feishuAssigneeMatches(configured string, id string, name string, email string) bool {
	if configured == "" {
		return true
	}
	if configured == "me" {
		return strings.TrimSpace(id) != ""
	}
	candidates := []string{
		normalizeAssigneeMatchValue(id),
		normalizeAssigneeMatchValue(name),
		normalizeAssigneeMatchValue(email),
	}
	for _, candidate := range candidates {
		if candidate != "" && candidate == configured {
			return true
		}
	}
	return false
}

func feishuLabels(item map[string]any) []string {
	value, exists := item["labels"]
	if !exists {
		value = item["tags"]
	}
	rawLabels, ok := value.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(rawLabels))
	for _, raw := range rawLabels {
		switch typed := raw.(type) {
		case string:
			label := strings.ToLower(strings.TrimSpace(typed))
			if label != "" {
				out = append(out, label)
			}
		case map[string]any:
			label := strings.ToLower(strings.TrimSpace(feishuFirstString(typed, "name", "label")))
			if label != "" {
				out = append(out, label)
			}
		}
	}
	return out
}

func feishuFirstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		value := strings.TrimSpace(stringValue(values[key]))
		if value != "" {
			return value
		}
	}
	return ""
}

func feishuHasMore(body map[string]any) bool {
	data, _ := body["data"].(map[string]any)
	if data == nil {
		return false
	}
	switch typed := data["has_more"].(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

func feishuStateUpdated(body map[string]any) bool {
	data, _ := body["data"].(map[string]any)
	if data == nil {
		return true
	}
	if updated, ok := data["updated"].(bool); ok {
		return updated
	}
	if status := strings.TrimSpace(stringValue(data["status"])); status != "" {
		return strings.EqualFold(status, "ok") || strings.EqualFold(status, "success")
	}
	return true
}

func dedupeIssuesByID(issues []Issue) []Issue {
	seen := map[string]struct{}{}
	out := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		key := strings.TrimSpace(issue.ID)
		if key == "" {
			key = strings.TrimSpace(issue.Identifier)
		}
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, issue)
	}
	return out
}
