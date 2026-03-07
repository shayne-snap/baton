package tracker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"baton/internal/config"
)

var (
	ErrMissingJiraAuth         = errors.New("missing_jira_auth")
	ErrUnsupportedJiraAuthType = errors.New("unsupported_jira_auth_type")
	ErrJiraAPIRequest          = errors.New("jira_api_request")
	ErrJiraAPIStatus           = errors.New("jira_api_status")
	ErrJiraUnknownPayload      = errors.New("jira_unknown_payload")
	ErrJiraTransitionNotFound  = errors.New("jira_transition_not_found")
	ErrJiraCurrentUserNotFound = errors.New("jira_current_user_not_found")
)

type jiraClient struct {
	config     *config.Config
	httpClient *http.Client
}

type JiraRequestError struct {
	Cause error
}

func (e *JiraRequestError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %v", ErrJiraAPIRequest, e.Cause)
}

func (e *JiraRequestError) Unwrap() error {
	return ErrJiraAPIRequest
}

type JiraStatusError struct {
	Status int
	Body   any
}

func (e *JiraStatusError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: status=%d body=%v", ErrJiraAPIStatus, e.Status, e.Body)
}

func (e *JiraStatusError) Unwrap() error {
	return ErrJiraAPIStatus
}

func NewJiraClient(cfg *config.Config) Client {
	return &jiraClient{
		config: cfg,
		httpClient: &http.Client{
			Timeout: requestTimeout,
		},
	}
}

func (c *jiraClient) FetchCandidateIssues(ctx context.Context) ([]Issue, error) {
	activeStates := c.config.TrackerActiveStates()
	jql, err := c.buildCandidateJQL(ctx, activeStates)
	if err != nil {
		return nil, err
	}
	return c.searchIssues(ctx, jql)
}

func (c *jiraClient) FetchIssuesByStates(ctx context.Context, states []string) ([]Issue, error) {
	normalizedStates := orderedUniqueStrings(states)
	if len(normalizedStates) == 0 {
		return []Issue{}, nil
	}

	projectKey := c.config.JiraProjectKey()
	if strings.TrimSpace(projectKey) == "" {
		return nil, config.ErrMissingJiraProjectKey
	}

	jql := fmt.Sprintf("project = %s AND %s ORDER BY priority DESC, created ASC", quoteJQLValue(projectKey), jiraStatusClause(normalizedStates))
	return c.searchIssues(ctx, jql)
}

func (c *jiraClient) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]Issue, error) {
	uniqueIDs := orderedUniqueStrings(ids)
	if len(uniqueIDs) == 0 {
		return []Issue{}, nil
	}

	projectKey := c.config.JiraProjectKey()
	if strings.TrimSpace(projectKey) == "" {
		return nil, config.ErrMissingJiraProjectKey
	}

	quoted := make([]string, 0, len(uniqueIDs))
	for _, id := range uniqueIDs {
		quoted = append(quoted, quoteJQLValue(id))
	}
	jql := fmt.Sprintf("project = %s AND key in (%s)", quoteJQLValue(projectKey), strings.Join(quoted, ", "))
	return c.searchIssues(ctx, jql)
}

func (c *jiraClient) CreateComment(ctx context.Context, issueID string, body string) error {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return ErrCommentCreateFailed
	}
	payload := map[string]any{
		"body": adfDoc(strings.TrimSpace(body)),
	}
	_, err := c.requestJSON(ctx, http.MethodPost, "/rest/api/3/issue/"+url.PathEscape(issueID)+"/comment", payload)
	return err
}

func (c *jiraClient) UpdateIssueState(ctx context.Context, issueID string, stateName string) error {
	issueID = strings.TrimSpace(issueID)
	targetState := strings.TrimSpace(stateName)
	if issueID == "" || targetState == "" {
		return ErrIssueUpdateFailed
	}

	transitionsResp, err := c.requestJSON(ctx, http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(issueID)+"/transitions", nil)
	if err != nil {
		return err
	}
	transitions, _ := transitionsResp["transitions"].([]any)

	transitionID := ""
	for _, raw := range transitions {
		transition, _ := raw.(map[string]any)
		to, _ := transition["to"].(map[string]any)
		if strings.EqualFold(strings.TrimSpace(stringValue(to["name"])), targetState) {
			transitionID = strings.TrimSpace(stringValue(transition["id"]))
			break
		}
	}
	if transitionID == "" {
		return ErrJiraTransitionNotFound
	}

	payload := map[string]any{
		"transition": map[string]any{
			"id": transitionID,
		},
	}
	_, err = c.requestJSON(ctx, http.MethodPost, "/rest/api/3/issue/"+url.PathEscape(issueID)+"/transitions", payload)
	return err
}

func (c *jiraClient) AddLink(ctx context.Context, issueID string, linkURL string, title string) error {
	issueID = strings.TrimSpace(issueID)
	linkURL = strings.TrimSpace(linkURL)
	if issueID == "" || linkURL == "" {
		return ErrLinkAddFailed
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = linkURL
	}
	payload := map[string]any{
		"object": map[string]any{
			"url":   linkURL,
			"title": title,
		},
	}
	_, err := c.requestJSON(ctx, http.MethodPost, "/rest/api/3/issue/"+url.PathEscape(issueID)+"/remotelink", payload)
	return err
}

func (c *jiraClient) buildCandidateJQL(ctx context.Context, activeStates []string) (string, error) {
	projectKey := c.config.JiraProjectKey()
	if strings.TrimSpace(projectKey) == "" {
		return "", config.ErrMissingJiraProjectKey
	}

	base := strings.TrimSpace(c.config.JiraJQL())
	if base == "" {
		base = fmt.Sprintf("project = %s ORDER BY priority DESC, created ASC", quoteJQLValue(projectKey))
	}

	clauses := []string{jiraStatusClause(activeStates)}
	assignee := strings.TrimSpace(c.config.TrackerAssignee())
	if assignee != "" {
		assigneeClause, err := c.assigneeJQLClause(ctx, assignee)
		if err != nil {
			return "", err
		}
		if assigneeClause != "" {
			clauses = append(clauses, assigneeClause)
		}
	}
	return appendJQLClauses(base, clauses), nil
}

func (c *jiraClient) assigneeJQLClause(ctx context.Context, assignee string) (string, error) {
	normalized := normalizeAssigneeMatchValue(assignee)
	if normalized == "" {
		return "", nil
	}
	if normalized == "me" {
		_, err := c.currentUserAccountID(ctx)
		if err != nil {
			return "", err
		}
		return "assignee = currentUser()", nil
	}
	return "assignee = " + quoteJQLValue(assignee), nil
}

func (c *jiraClient) searchIssues(ctx context.Context, jql string) ([]Issue, error) {
	if strings.TrimSpace(c.config.JiraBaseURL()) == "" {
		return nil, config.ErrMissingJiraBaseURL
	}
	if strings.TrimSpace(c.config.JiraEmail()) == "" || strings.TrimSpace(c.config.JiraAPIToken()) == "" {
		return nil, ErrMissingJiraAuth
	}
	if !strings.EqualFold(strings.TrimSpace(c.config.JiraAuthType()), "email_api_token") {
		return nil, ErrUnsupportedJiraAuthType
	}

	acc := make([]Issue, 0, issuePageSize)
	nextPageToken := ""
	for {
		payload := map[string]any{
			"jql":        jql,
			"maxResults": issuePageSize,
			"fields": []any{
				"summary",
				"description",
				"priority",
				"status",
				"assignee",
				"labels",
				"created",
				"updated",
			},
		}
		if strings.TrimSpace(nextPageToken) != "" {
			payload["nextPageToken"] = nextPageToken
		}
		resp, err := c.requestJSON(ctx, http.MethodPost, "/rest/api/3/search/jql", payload)
		if err != nil {
			return nil, err
		}

		rawIssues, _ := resp["issues"].([]any)
		if len(rawIssues) == 0 {
			break
		}

		issues := decodeJiraIssues(rawIssues, c.config.JiraBaseURL(), c.config.TrackerAssignee())
		acc = append(acc, issues...)

		token := strings.TrimSpace(stringValue(resp["nextPageToken"]))
		if token == "" {
			break
		}
		nextPageToken = token
	}
	return acc, nil
}

func (c *jiraClient) currentUserAccountID(ctx context.Context) (string, error) {
	body, err := c.requestJSON(ctx, http.MethodGet, "/rest/api/3/myself", nil)
	if err != nil {
		return "", err
	}
	accountID := strings.TrimSpace(stringValue(body["accountId"]))
	if accountID == "" {
		return "", ErrJiraCurrentUserNotFound
	}
	return accountID, nil
}

func (c *jiraClient) requestJSON(ctx context.Context, method string, path string, payload any) (map[string]any, error) {
	base := strings.TrimRight(strings.TrimSpace(c.config.JiraBaseURL()), "/")
	if base == "" {
		return nil, config.ErrMissingJiraBaseURL
	}

	fullURL := base + path
	var reader io.Reader
	if payload != nil {
		rawPayload, err := json.Marshal(payload)
		if err != nil {
			return nil, &JiraRequestError{Cause: err}
		}
		reader = bytes.NewReader(rawPayload)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
	if err != nil {
		return nil, &JiraRequestError{Cause: err}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	email := strings.TrimSpace(c.config.JiraEmail())
	token := strings.TrimSpace(c.config.JiraAPIToken())
	if email == "" || token == "" {
		return nil, ErrMissingJiraAuth
	}
	authHeader := base64.StdEncoding.EncodeToString([]byte(email + ":" + token))
	req.Header.Set("Authorization", "Basic "+authHeader)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &JiraRequestError{Cause: err}
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &JiraRequestError{Cause: err}
	}
	if len(rawBody) == 0 {
		rawBody = []byte("{}")
	}
	decoded := decodeJSONOrString(rawBody)

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &JiraStatusError{Status: resp.StatusCode, Body: decoded}
	}

	bodyMap, ok := decoded.(map[string]any)
	if !ok {
		return nil, ErrJiraUnknownPayload
	}
	return bodyMap, nil
}

func decodeJiraIssues(rawIssues []any, baseURL string, configuredAssignee string) []Issue {
	filter := normalizeAssigneeMatchValue(configuredAssignee)
	out := make([]Issue, 0, len(rawIssues))
	for _, raw := range rawIssues {
		issue, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		key := strings.TrimSpace(stringValue(issue["key"]))
		id := key
		if id == "" {
			id = strings.TrimSpace(stringValue(issue["id"]))
		}
		fields, _ := issue["fields"].(map[string]any)
		assignee, _ := fields["assignee"].(map[string]any)
		assigneeAccountID := strings.TrimSpace(stringValue(assignee["accountId"]))
		assigneeDisplayName := strings.TrimSpace(stringValue(assignee["displayName"]))
		assigneeEmail := strings.TrimSpace(stringValue(assignee["emailAddress"]))
		state := nestedString(fields, "status", "name")
		priority := parseJiraPriority(fields["priority"])

		out = append(out, Issue{
			ID:               id,
			Identifier:       key,
			Title:            strings.TrimSpace(stringValue(fields["summary"])),
			Description:      decodeADFText(fields["description"]),
			Priority:         priority,
			State:            strings.TrimSpace(state),
			URL:              jiraIssueURL(baseURL, key),
			AssigneeID:       assigneeAccountID,
			Labels:           normalizeJiraLabels(fields["labels"]),
			AssignedToWorker: jiraAssigneeMatches(filter, assigneeAccountID, assigneeDisplayName, assigneeEmail),
			CreatedAt:        parseDateTime(fields["created"]),
			UpdatedAt:        parseDateTime(fields["updated"]),
		})
	}
	return out
}

func jiraIssueURL(baseURL string, key string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" || strings.TrimSpace(key) == "" {
		return ""
	}
	return base + "/browse/" + key
}

func normalizeJiraLabels(value any) []string {
	rawLabels, ok := value.([]any)
	if !ok || len(rawLabels) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(rawLabels))
	for _, raw := range rawLabels {
		label := strings.ToLower(strings.TrimSpace(stringValue(raw)))
		if label != "" {
			out = append(out, label)
		}
	}
	return out
}

func jiraAssigneeMatches(configured string, accountID string, displayName string, email string) bool {
	if configured == "" {
		return true
	}
	if configured == "me" {
		return strings.TrimSpace(accountID) != ""
	}
	candidates := []string{
		normalizeAssigneeMatchValue(accountID),
		normalizeAssigneeMatchValue(displayName),
		normalizeAssigneeMatchValue(email),
	}
	for _, candidate := range candidates {
		if candidate != "" && candidate == configured {
			return true
		}
	}
	return false
}

func parseJiraPriority(value any) *int {
	priority, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	id := strings.TrimSpace(stringValue(priority["id"]))
	if id == "" {
		return nil
	}
	parsed, err := strconv.Atoi(id)
	if err != nil {
		return nil
	}
	return &parsed
}

func decodeADFText(value any) string {
	root, ok := value.(map[string]any)
	if !ok {
		return strings.TrimSpace(stringValue(value))
	}
	content, _ := root["content"].([]any)
	parts := make([]string, 0, 8)
	walkADF(content, &parts)
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func walkADF(nodes []any, parts *[]string) {
	for _, raw := range nodes {
		node, _ := raw.(map[string]any)
		if node == nil {
			continue
		}
		if text := strings.TrimSpace(stringValue(node["text"])); text != "" {
			*parts = append(*parts, text)
		}
		child, _ := node["content"].([]any)
		if len(child) > 0 {
			walkADF(child, parts)
		}
	}
}

func adfDoc(body string) map[string]any {
	body = strings.TrimSpace(body)
	if body == "" {
		body = " "
	}
	paragraphs := strings.Split(body, "\n")
	content := make([]any, 0, len(paragraphs))
	for _, paragraph := range paragraphs {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph == "" {
			continue
		}
		content = append(content, map[string]any{
			"type": "paragraph",
			"content": []any{
				map[string]any{
					"type": "text",
					"text": paragraph,
				},
			},
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "paragraph"})
	}
	return map[string]any{
		"type":    "doc",
		"version": 1,
		"content": content,
	}
}

func jiraStatusClause(states []string) string {
	normalized := orderedUniqueStrings(states)
	quoted := make([]string, 0, len(normalized))
	for _, state := range normalized {
		quoted = append(quoted, quoteJQLValue(state))
	}
	return "status in (" + strings.Join(quoted, ", ") + ")"
}

func appendJQLClauses(base string, clauses []string) string {
	filtered := make([]string, 0, len(clauses))
	for _, clause := range clauses {
		clause = strings.TrimSpace(clause)
		if clause != "" {
			filtered = append(filtered, clause)
		}
	}

	base = strings.TrimSpace(base)
	if base == "" {
		return strings.Join(filtered, " AND ")
	}
	orderBy := ""
	upper := strings.ToUpper(base)
	if idx := strings.Index(upper, " ORDER BY "); idx >= 0 {
		orderBy = strings.TrimSpace(base[idx:])
		base = strings.TrimSpace(base[:idx])
	}
	if len(filtered) > 0 {
		base = "(" + base + ") AND " + strings.Join(filtered, " AND ")
	}
	if orderBy != "" {
		base = strings.TrimSpace(base + " " + orderBy)
	}
	return base
}

func quoteJQLValue(value string) string {
	escaped := strings.ReplaceAll(strings.TrimSpace(value), `"`, `\"`)
	return `"` + escaped + `"`
}
