package cli

import (
	"errors"
	"strings"

	"baton/internal/config"
	"baton/internal/mcpserver"

	"github.com/spf13/cobra"
)

const (
	trackerKindEnv     = "BATON_TRACKER_KIND"
	trackerAssigneeEnv = "BATON_TRACKER_ASSIGNEE"

	jiraBaseURLEnv    = "BATON_JIRA_BASE_URL"
	jiraProjectKeyEnv = "BATON_JIRA_PROJECT_KEY"
	jiraJQLEnv        = "BATON_JIRA_JQL"
	jiraAuthTypeEnv   = "BATON_JIRA_AUTH_TYPE"
	jiraEmailEnv      = "BATON_JIRA_EMAIL"
	jiraAPITokenEnv   = "BATON_JIRA_API_TOKEN"

	feishuBaseURLEnv    = "BATON_FEISHU_BASE_URL"
	feishuProjectKeyEnv = "BATON_FEISHU_PROJECT_KEY"
	feishuAppIDEnv      = "BATON_FEISHU_APP_ID"
	feishuAppSecretEnv  = "BATON_FEISHU_APP_SECRET"
)

func newMCPTrackerServerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "mcp-tracker-server",
		Short:  "Run Baton's tracker MCP server over stdio.",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			kind := strings.ToLower(envOrEmpty(trackerKindEnv))
			switch kind {
			case "linear":
				apiKey := envOrEmpty(linearAPIKeyEnv)
				if apiKey == "" {
					return errors.New("missing linear tracker environment for mcp-tracker-server")
				}
				cfg := &config.Config{
					Tracker: config.TrackerConfig{
						Kind: "linear",
						Routing: config.TrackerRoutingConfig{
							Assignee: envOrEmpty(trackerAssigneeEnv),
						},
						Linear: config.TrackerLinearConfig{
							Endpoint: envOrDefault(linearEndpointEnv, "https://api.linear.app/graphql"),
							APIKey:   apiKey,
						},
					},
				}
				return mcpserver.ServeTrackerStdio(cmd.Context(), cfg)
			case "jira":
				baseURL := envOrEmpty(jiraBaseURLEnv)
				projectKey := envOrEmpty(jiraProjectKeyEnv)
				email := envOrEmpty(jiraEmailEnv)
				apiToken := envOrEmpty(jiraAPITokenEnv)
				if baseURL == "" || projectKey == "" || email == "" || apiToken == "" {
					return errors.New("missing jira tracker environment for mcp-tracker-server")
				}
				authType := envOrDefault(jiraAuthTypeEnv, "email_api_token")
				cfg := &config.Config{
					Tracker: config.TrackerConfig{
						Kind: "jira",
						Routing: config.TrackerRoutingConfig{
							Assignee: envOrEmpty(trackerAssigneeEnv),
						},
						Jira: config.TrackerJiraConfig{
							BaseURL:    baseURL,
							ProjectKey: projectKey,
							JQL:        envOrEmpty(jiraJQLEnv),
							Auth: config.TrackerJiraAuthConfig{
								Type:     authType,
								Email:    email,
								APIToken: apiToken,
							},
						},
					},
				}
				return mcpserver.ServeTrackerStdio(cmd.Context(), cfg)
			case "feishu":
				baseURL := envOrEmpty(feishuBaseURLEnv)
				projectKey := envOrEmpty(feishuProjectKeyEnv)
				appID := envOrEmpty(feishuAppIDEnv)
				appSecret := envOrEmpty(feishuAppSecretEnv)
				if baseURL == "" || projectKey == "" || appID == "" || appSecret == "" {
					return errors.New("missing feishu tracker environment for mcp-tracker-server")
				}
				cfg := &config.Config{
					Tracker: config.TrackerConfig{
						Kind: "feishu",
						Routing: config.TrackerRoutingConfig{
							Assignee: envOrEmpty(trackerAssigneeEnv),
						},
						Feishu: config.TrackerFeishuConfig{
							BaseURL:    baseURL,
							ProjectKey: projectKey,
							AppID:      appID,
							AppSecret:  appSecret,
						},
					},
				}
				return mcpserver.ServeTrackerStdio(cmd.Context(), cfg)
			default:
				return errors.New("unsupported or missing BATON_TRACKER_KIND for mcp-tracker-server")
			}
		},
	}
	return cmd
}
