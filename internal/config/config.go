package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"baton/internal/workflow"
)

var (
	ErrMissingTrackerKind           = errors.New("missing_tracker_kind")
	ErrMissingTrackerLifecycleState = errors.New("missing_tracker_lifecycle_state")
	ErrMissingLinearAPIToken        = errors.New("missing_linear_api_token")
	ErrMissingLinearProjectSlug     = errors.New("missing_linear_project_slug")
	ErrMissingJiraBaseURL           = errors.New("missing_jira_base_url")
	ErrMissingJiraProjectKey        = errors.New("missing_jira_project_key")
	ErrMissingJiraAuthType          = errors.New("missing_jira_auth_type")
	ErrMissingJiraEmail             = errors.New("missing_jira_email")
	ErrMissingJiraAPIToken          = errors.New("missing_jira_api_token")
	ErrMissingFeishuBaseURL         = errors.New("missing_feishu_base_url")
	ErrMissingFeishuProjectKey      = errors.New("missing_feishu_project_key")
	ErrMissingFeishuAppID           = errors.New("missing_feishu_app_id")
	ErrMissingFeishuAppSecret       = errors.New("missing_feishu_app_secret")
	ErrUnsupportedAgentRuntime      = errors.New("unsupported_agent_runtime")
	ErrMissingCodexCommand          = errors.New("missing_codex_command")
	ErrMissingOpencodeCommand       = errors.New("missing_opencode_command")
	ErrInvalidClaudeCodePermission  = errors.New("invalid_claudecode_permission_mode")
	ErrInvalidCodexApproval         = errors.New("invalid_codex_approval_policy")
	ErrInvalidCodexThreadSandbox    = errors.New("invalid_codex_thread_sandbox")
	ErrInvalidCodexTurnSandbox      = errors.New("invalid_codex_turn_sandbox_policy")
	ErrUnsupportedTrackerKind       = errors.New("unsupported_tracker_kind")
)

const (
	defaultLinearEndpoint     = "https://api.linear.app/graphql"
	defaultPollIntervalMS     = 30_000
	defaultHookTimeoutMS      = 60_000
	defaultMaxConcurrent      = 10
	defaultMaxTurns           = 20
	defaultMaxRetryBackoffMS  = 300_000
	defaultCodexCommand       = "codex app-server"
	defaultOpencodeCommand    = "opencode serve"
	defaultClaudeCodeCommand  = "claude"
	defaultCodexTurnTimeoutMS = 3_600_000
	defaultCodexReadTimeoutMS = 5_000
	defaultCodexStallTimeout  = 300_000
	defaultDashboardEnabled   = true
	defaultDashboardRefreshMS = 1_000
	defaultDashboardRenderMS  = 16
	defaultServerHost         = "127.0.0.1"
	defaultClaudePermission   = "dontAsk"
)

var (
	defaultActiveStates   = []string{"Todo", "In Progress"}
	defaultTerminalStates = []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"}
	defaultApprovalPolicy = map[string]any{
		"reject": map[string]any{
			"sandbox_approval": true,
			"rules":            true,
			"mcp_elicitations": true,
		},
	}
	defaultThreadSandbox = "workspace-write"
	defaultPrompt        = `You are working on a Linear issue.

Identifier: {{ issue.identifier }}
Title: {{ issue.title }}

Body:
{% if issue.description %}
{{ issue.description }}
{% else %}
No description provided.
{% endif %}`
)

var envReferencePattern = regexp.MustCompile(`^\$[A-Za-z_][A-Za-z0-9_]*$`)

type ValidationError struct {
	Code  error
	Value any
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ""
	}
	if e.Value == nil {
		return e.Code.Error()
	}
	return fmt.Sprintf("%s: %v", e.Code.Error(), e.Value)
}

func (e *ValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Code
}

type Config struct {
	mu sync.RWMutex

	WorkflowPath string
	RawConfig    map[string]any

	Tracker       TrackerConfig
	Polling       PollingConfig
	Agent         AgentConfig
	AgentRuntime  AgentRuntimeConfig
	Hooks         HooksConfig
	Observability ObservabilityConfig
	Server        ServerConfig

	promptTemplate string
}

type TrackerConfig struct {
	Kind      string
	Routing   TrackerRoutingConfig
	Lifecycle TrackerLifecycleConfig
	Linear    TrackerLinearConfig
	Jira      TrackerJiraConfig
	Feishu    TrackerFeishuConfig
}

type TrackerRoutingConfig struct {
	Assignee string
	Active   []string
	Terminal []string
}

type TrackerLifecycleConfig struct {
	Backlog     string
	Todo        string
	InProgress  string
	HumanReview string
	Merging     string
	Rework      string
	Done        string
}

type TrackerLinearConfig struct {
	Endpoint    string
	APIKey      string
	ProjectSlug string
}

type TrackerJiraConfig struct {
	BaseURL    string
	ProjectKey string
	JQL        string
	Auth       TrackerJiraAuthConfig
}

type TrackerJiraAuthConfig struct {
	Type     string
	Email    string
	APIToken string
}

type TrackerFeishuConfig struct {
	BaseURL    string
	ProjectKey string
	AppID      string
	AppSecret  string
}

type PollingConfig struct {
	IntervalMS int
}

type AgentConfig struct {
	MaxConcurrentAgents        int
	MaxTurns                   int
	MaxRetryBackoffMS          int
	MaxConcurrentAgentsByState map[string]int
}

type AgentRuntimeConfig struct {
	Kind       string
	Codex      CodexConfig
	Opencode   OpencodeConfig
	ClaudeCode ClaudeCodeConfig
}

type CodexConfig struct {
	Command        string
	ApprovalPolicy any
	ThreadSandbox  any
	TurnSandbox    any
	TurnTimeoutMS  int
	ReadTimeoutMS  int
	StallTimeoutMS int
}

type OpencodeConfig struct {
	Command    string
	Permission []map[string]any
}

type ClaudeCodeConfig struct {
	Command            string
	PermissionMode     string
	AllowedTools       []string
	DisallowedTools    []string
	Model              string
	AppendSystemPrompt string
	MCPStrict          bool
	SessionPersistence bool
}

type HooksConfig struct {
	AfterCreate  string
	BeforeRun    string
	AfterRun     string
	BeforeRemove string
	TimeoutMS    int
}

type ObservabilityConfig struct {
	DashboardEnabled bool
	RefreshMS        int
	RenderIntervalMS int
}

type ServerConfig struct {
	Host string
	Port *int
}

type CodexRuntimeSettings struct {
	ApprovalPolicy    any
	ThreadSandbox     string
	TurnSandboxPolicy map[string]any
}

func FromWorkflow(path string, definition *workflow.Definition) (*Config, error) {
	if definition == nil {
		return nil, fmt.Errorf("nil workflow definition")
	}

	cleanPath := filepath.Clean(path)
	raw := normalizeMap(definition.Config)
	agentRuntimeRaw, _ := getPath(raw, "agent_runtime").(map[string]any)
	codexRaw := codexConfigSource(raw)
	opencodeRaw, _ := getPath(agentRuntimeRaw, "opencode").(map[string]any)
	claudeCodeRaw, _ := getPath(agentRuntimeRaw, "claudecode").(map[string]any)

	cfg := &Config{
		WorkflowPath:   cleanPath,
		RawConfig:      raw,
		promptTemplate: strings.TrimSpace(definition.PromptTemplate),
		Tracker: TrackerConfig{
			Kind: normalizeTrackerKind(scalarString(getPath(raw, "tracker", "kind"))),
			Routing: TrackerRoutingConfig{
				Assignee: scalarString(getPath(raw, "tracker", "routing", "assignee")),
				Active:   parseCSV(getPath(raw, "tracker", "routing", "active_states"), defaultActiveStates),
				Terminal: parseCSV(getPath(raw, "tracker", "routing", "terminal_states"), defaultTerminalStates),
			},
			Lifecycle: TrackerLifecycleConfig{
				Backlog:     scalarString(getPath(raw, "tracker", "lifecycle", "backlog")),
				Todo:        scalarString(getPath(raw, "tracker", "lifecycle", "todo")),
				InProgress:  scalarString(getPath(raw, "tracker", "lifecycle", "in_progress")),
				HumanReview: scalarString(getPath(raw, "tracker", "lifecycle", "human_review")),
				Merging:     scalarString(getPath(raw, "tracker", "lifecycle", "merging")),
				Rework:      scalarString(getPath(raw, "tracker", "lifecycle", "rework")),
				Done:        scalarString(getPath(raw, "tracker", "lifecycle", "done")),
			},
			Linear: TrackerLinearConfig{
				Endpoint:    withDefault(scalarString(getPath(raw, "tracker", "linear", "endpoint")), defaultLinearEndpoint),
				APIKey:      binaryValue(getPath(raw, "tracker", "linear", "api_key")),
				ProjectSlug: scalarString(getPath(raw, "tracker", "linear", "project_slug")),
			},
			Jira: TrackerJiraConfig{
				BaseURL:    scalarString(getPath(raw, "tracker", "jira", "base_url")),
				ProjectKey: scalarString(getPath(raw, "tracker", "jira", "project_key")),
				JQL:        scalarString(getPath(raw, "tracker", "jira", "jql")),
				Auth: TrackerJiraAuthConfig{
					Type:     scalarString(getPath(raw, "tracker", "jira", "auth", "type")),
					Email:    binaryValue(getPath(raw, "tracker", "jira", "auth", "email")),
					APIToken: binaryValue(getPath(raw, "tracker", "jira", "auth", "api_token")),
				},
			},
			Feishu: TrackerFeishuConfig{
				BaseURL:    scalarString(getPath(raw, "tracker", "feishu", "base_url")),
				ProjectKey: scalarString(getPath(raw, "tracker", "feishu", "project_key")),
				AppID:      binaryValue(getPath(raw, "tracker", "feishu", "app_id")),
				AppSecret:  binaryValue(getPath(raw, "tracker", "feishu", "app_secret")),
			},
		},
		Polling: PollingConfig{
			IntervalMS: intWithDefault(getPath(raw, "polling", "interval_ms"), defaultPollIntervalMS),
		},
		Agent: AgentConfig{
			MaxConcurrentAgents:        intWithDefault(getPath(raw, "agent", "max_concurrent_agents"), defaultMaxConcurrent),
			MaxTurns:                   positiveIntWithDefault(getPath(raw, "agent", "max_turns"), defaultMaxTurns),
			MaxRetryBackoffMS:          positiveIntWithDefault(getPath(raw, "agent", "max_retry_backoff_ms"), defaultMaxRetryBackoffMS),
			MaxConcurrentAgentsByState: parseStateLimits(getPath(raw, "agent", "max_concurrent_agents_by_state")),
		},
		AgentRuntime: AgentRuntimeConfig{
			Kind: normalizeAgentRuntimeKind(scalarString(getPath(agentRuntimeRaw, "kind"))),
			Codex: CodexConfig{
				Command:        commandWithDefault(getPath(codexRaw, "command"), defaultCodexCommand),
				ApprovalPolicy: getPath(codexRaw, "approval_policy"),
				ThreadSandbox:  getPath(codexRaw, "thread_sandbox"),
				TurnSandbox:    getPath(codexRaw, "turn_sandbox_policy"),
				TurnTimeoutMS:  intWithDefault(getPath(codexRaw, "turn_timeout_ms"), defaultCodexTurnTimeoutMS),
				ReadTimeoutMS:  intWithDefault(getPath(codexRaw, "read_timeout_ms"), defaultCodexReadTimeoutMS),
				StallTimeoutMS: intWithDefault(getPath(codexRaw, "stall_timeout_ms"), defaultCodexStallTimeout),
			},
			Opencode: OpencodeConfig{
				Command:    commandWithDefault(getPath(opencodeRaw, "command"), defaultOpencodeCommand),
				Permission: parseMapList(getPath(opencodeRaw, "permission")),
			},
			ClaudeCode: ClaudeCodeConfig{
				Command:            commandWithDefault(getPath(claudeCodeRaw, "command"), defaultClaudeCodeCommand),
				PermissionMode:     withDefault(scalarString(getPath(claudeCodeRaw, "permission_mode")), defaultClaudePermission),
				AllowedTools:       parseCSV(getPath(claudeCodeRaw, "allowed_tools"), nil),
				DisallowedTools:    parseCSV(getPath(claudeCodeRaw, "disallowed_tools"), nil),
				Model:              scalarString(getPath(claudeCodeRaw, "model")),
				AppendSystemPrompt: scalarString(getPath(claudeCodeRaw, "append_system_prompt")),
				MCPStrict:          boolWithDefault(getPath(claudeCodeRaw, "mcp_strict"), true),
				SessionPersistence: boolWithDefault(getPath(claudeCodeRaw, "session_persistence"), true),
			},
		},
		Hooks: HooksConfig{
			AfterCreate:  hookValue(getPath(raw, "hooks", "after_create")),
			BeforeRun:    hookValue(getPath(raw, "hooks", "before_run")),
			AfterRun:     hookValue(getPath(raw, "hooks", "after_run")),
			BeforeRemove: hookValue(getPath(raw, "hooks", "before_remove")),
			TimeoutMS:    positiveIntWithDefault(getPath(raw, "hooks", "timeout_ms"), defaultHookTimeoutMS),
		},
		Observability: ObservabilityConfig{
			DashboardEnabled: boolWithDefault(getPath(raw, "observability", "dashboard_enabled"), defaultDashboardEnabled),
			RefreshMS:        intWithDefault(getPath(raw, "observability", "refresh_ms"), defaultDashboardRefreshMS),
			RenderIntervalMS: intWithDefault(getPath(raw, "observability", "render_interval_ms"), defaultDashboardRenderMS),
		},
		Server: ServerConfig{
			Host: withDefault(scalarString(getPath(raw, "server", "host")), defaultServerHost),
			Port: optionalNonNegativeInt(getPath(raw, "server", "port")),
		},
	}

	return cfg, nil
}

func (c *Config) Validate() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	switch kind := c.Tracker.Kind; kind {
	case "":
		return &ValidationError{Code: ErrMissingTrackerKind}
	case "linear", "jira", "feishu", "memory":
	default:
		return &ValidationError{Code: ErrUnsupportedTrackerKind, Value: kind}
	}

	lifecycle := c.Tracker.Lifecycle
	if strings.TrimSpace(lifecycle.Backlog) == "" {
		return &ValidationError{Code: ErrMissingTrackerLifecycleState, Value: "backlog"}
	}
	if strings.TrimSpace(lifecycle.Todo) == "" {
		return &ValidationError{Code: ErrMissingTrackerLifecycleState, Value: "todo"}
	}
	if strings.TrimSpace(lifecycle.InProgress) == "" {
		return &ValidationError{Code: ErrMissingTrackerLifecycleState, Value: "in_progress"}
	}
	if strings.TrimSpace(lifecycle.HumanReview) == "" {
		return &ValidationError{Code: ErrMissingTrackerLifecycleState, Value: "human_review"}
	}
	if strings.TrimSpace(lifecycle.Merging) == "" {
		return &ValidationError{Code: ErrMissingTrackerLifecycleState, Value: "merging"}
	}
	if strings.TrimSpace(lifecycle.Rework) == "" {
		return &ValidationError{Code: ErrMissingTrackerLifecycleState, Value: "rework"}
	}
	if strings.TrimSpace(lifecycle.Done) == "" {
		return &ValidationError{Code: ErrMissingTrackerLifecycleState, Value: "done"}
	}

	if c.Tracker.Kind == "linear" {
		token := normalizeSecret(resolveEnvValue(c.Tracker.Linear.APIKey, os.Getenv("LINEAR_API_KEY")))
		if token == "" {
			return &ValidationError{Code: ErrMissingLinearAPIToken}
		}
		if strings.TrimSpace(c.Tracker.Linear.ProjectSlug) == "" {
			return &ValidationError{Code: ErrMissingLinearProjectSlug}
		}
	}
	if c.Tracker.Kind == "jira" {
		if strings.TrimSpace(c.Tracker.Jira.BaseURL) == "" {
			return &ValidationError{Code: ErrMissingJiraBaseURL}
		}
		if strings.TrimSpace(c.Tracker.Jira.ProjectKey) == "" {
			return &ValidationError{Code: ErrMissingJiraProjectKey}
		}
		if strings.TrimSpace(c.Tracker.Jira.Auth.Type) == "" {
			return &ValidationError{Code: ErrMissingJiraAuthType}
		}
		email := normalizeSecret(resolveEnvValue(c.Tracker.Jira.Auth.Email, os.Getenv("JIRA_EMAIL")))
		if email == "" {
			return &ValidationError{Code: ErrMissingJiraEmail}
		}
		token := normalizeSecret(resolveEnvValue(c.Tracker.Jira.Auth.APIToken, os.Getenv("JIRA_API_TOKEN")))
		if token == "" {
			return &ValidationError{Code: ErrMissingJiraAPIToken}
		}
	}
	if c.Tracker.Kind == "feishu" {
		baseURL := strings.TrimSpace(resolveEnvValue(c.Tracker.Feishu.BaseURL, os.Getenv("FEISHU_BASE_URL")))
		if baseURL == "" {
			return &ValidationError{Code: ErrMissingFeishuBaseURL}
		}
		projectKey := strings.TrimSpace(resolveEnvValue(c.Tracker.Feishu.ProjectKey, os.Getenv("FEISHU_PROJECT_KEY")))
		if projectKey == "" {
			return &ValidationError{Code: ErrMissingFeishuProjectKey}
		}
		appID := normalizeSecret(resolveEnvValue(c.Tracker.Feishu.AppID, os.Getenv("FEISHU_APP_ID")))
		if appID == "" {
			return &ValidationError{Code: ErrMissingFeishuAppID}
		}
		appSecret := normalizeSecret(resolveEnvValue(c.Tracker.Feishu.AppSecret, os.Getenv("FEISHU_APP_SECRET")))
		if appSecret == "" {
			return &ValidationError{Code: ErrMissingFeishuAppSecret}
		}
	}

	if kind := c.AgentRuntime.Kind; kind != "" && kind != "codex" && kind != "opencode" && kind != "claudecode" {
		return &ValidationError{Code: ErrUnsupportedAgentRuntime, Value: kind}
	}

	switch c.AgentRuntimeKind() {
	case "codex":
		if _, err := c.codexRuntimeSettingsLocked(""); err != nil {
			return err
		}
		if strings.TrimSpace(c.AgentRuntime.Codex.Command) == "" {
			return &ValidationError{Code: ErrMissingCodexCommand}
		}
	case "opencode":
		if strings.TrimSpace(c.AgentRuntime.Opencode.Command) == "" {
			return &ValidationError{Code: ErrMissingOpencodeCommand}
		}
	case "claudecode":
		if !validClaudePermissionMode(c.AgentRuntime.ClaudeCode.PermissionMode) {
			return &ValidationError{Code: ErrInvalidClaudeCodePermission, Value: c.AgentRuntime.ClaudeCode.PermissionMode}
		}
	}

	return nil
}

func (c *Config) ReloadFromDisk() error {
	path := c.WorkflowFilePath()
	definition, err := workflow.LoadFile(path)
	if err != nil {
		return err
	}
	return c.ReplaceFromWorkflow(path, definition)
}

func (c *Config) ReplaceFromWorkflow(path string, definition *workflow.Definition) error {
	next, err := FromWorkflow(path, definition)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.copyFromLocked(next)
	return nil
}

func (c *Config) WorkflowFilePath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.WorkflowPath
}

func (c *Config) TrackerKind() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Tracker.Kind
}

func (c *Config) LinearEndpoint() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Tracker.Linear.Endpoint
}

func (c *Config) LinearProjectSlug() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return strings.TrimSpace(c.Tracker.Linear.ProjectSlug)
}

func (c *Config) LinearActiveStates() []string {
	return c.TrackerActiveStates()
}

func (c *Config) TrackerActiveStates() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneStrings(c.Tracker.Routing.Active)
}

func (c *Config) LinearTerminalStates() []string {
	return c.TrackerTerminalStates()
}

func (c *Config) TrackerTerminalStates() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneStrings(c.Tracker.Routing.Terminal)
}

func (c *Config) TrackerLifecycle() TrackerLifecycleConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Tracker.Lifecycle
}

func (c *Config) PollIntervalMS() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Polling.IntervalMS
}

func (c *Config) MaxConcurrentAgents() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Agent.MaxConcurrentAgents
}

func (c *Config) AgentMaxTurns() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Agent.MaxTurns
}

func (c *Config) MaxRetryBackoffMS() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Agent.MaxRetryBackoffMS
}

func (c *Config) CodexCommand() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AgentRuntime.Codex.Command
}

func (c *Config) CodexTurnTimeoutMS() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AgentRuntime.Codex.TurnTimeoutMS
}

func (c *Config) CodexReadTimeoutMS() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AgentRuntime.Codex.ReadTimeoutMS
}

func (c *Config) OpencodeCommand() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AgentRuntime.Opencode.Command
}

func (c *Config) OpencodePermissionRules() []map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneMapList(c.AgentRuntime.Opencode.Permission)
}

func (c *Config) ClaudeCodeCommand() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AgentRuntime.ClaudeCode.Command
}

func (c *Config) ClaudeCodePermissionMode() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AgentRuntime.ClaudeCode.PermissionMode
}

func (c *Config) ClaudeCodeAllowedTools() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneStrings(c.AgentRuntime.ClaudeCode.AllowedTools)
}

func (c *Config) ClaudeCodeDisallowedTools() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneStrings(c.AgentRuntime.ClaudeCode.DisallowedTools)
}

func (c *Config) ClaudeCodeModel() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AgentRuntime.ClaudeCode.Model
}

func (c *Config) ClaudeCodeAppendSystemPrompt() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AgentRuntime.ClaudeCode.AppendSystemPrompt
}

func (c *Config) ClaudeCodeMCPStrict() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AgentRuntime.ClaudeCode.MCPStrict
}

func (c *Config) ClaudeCodeSessionPersistence() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AgentRuntime.ClaudeCode.SessionPersistence
}

func (c *Config) HookTimeoutMS() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Hooks.TimeoutMS
}

func (c *Config) HookAfterCreate() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Hooks.AfterCreate
}

func (c *Config) HookBeforeRun() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Hooks.BeforeRun
}

func (c *Config) HookAfterRun() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Hooks.AfterRun
}

func (c *Config) HookBeforeRemove() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Hooks.BeforeRemove
}

func (c *Config) ServerHost() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Server.Host
}

func (c *Config) ServerPort() *int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.Server.Port == nil {
		return nil
	}
	port := *c.Server.Port
	return &port
}

func (c *Config) ObservabilityEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Observability.DashboardEnabled
}

func (c *Config) ObservabilityRefreshMS() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Observability.RefreshMS
}

func (c *Config) ObservabilityRenderIntervalMS() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Observability.RenderIntervalMS
}

func (c *Config) CodexStallTimeoutMS() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.AgentRuntime.Codex.StallTimeoutMS < 0 {
		return 0
	}
	return c.AgentRuntime.Codex.StallTimeoutMS
}

func (c *Config) AgentRuntimeKind() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if strings.TrimSpace(c.AgentRuntime.Kind) == "" {
		return "codex"
	}
	return c.AgentRuntime.Kind
}

func (c *Config) MaxConcurrentAgentsForState(stateName string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	normalized := normalizeIssueState(stateName)
	if normalized == "" {
		return c.Agent.MaxConcurrentAgents
	}
	limit, ok := c.Agent.MaxConcurrentAgentsByState[normalized]
	if !ok {
		return c.Agent.MaxConcurrentAgents
	}
	return limit
}

func (c *Config) LinearAPIToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	value := resolveEnvValue(c.Tracker.Linear.APIKey, os.Getenv("LINEAR_API_KEY"))
	return normalizeSecret(value)
}

func (c *Config) LinearAssignee() string {
	return c.TrackerAssignee()
}

func (c *Config) TrackerAssignee() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if rawAssignee := getPath(c.RawConfig, "tracker", "routing", "assignee"); rawAssignee != nil {
		explicit := strings.TrimSpace(scalarString(rawAssignee))
		if explicit == "" {
			return ""
		}
	}
	value := resolveEnvValue(c.Tracker.Routing.Assignee, os.Getenv("BATON_ASSIGNEE"))
	if strings.TrimSpace(value) == "" {
		value = resolveEnvValue(c.Tracker.Routing.Assignee, os.Getenv("LINEAR_ASSIGNEE"))
	}
	return normalizeSecret(value)
}

func (c *Config) JiraBaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return strings.TrimSpace(c.Tracker.Jira.BaseURL)
}

func (c *Config) JiraProjectKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return strings.TrimSpace(c.Tracker.Jira.ProjectKey)
}

func (c *Config) JiraJQL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return strings.TrimSpace(c.Tracker.Jira.JQL)
}

func (c *Config) JiraAuthType() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return strings.TrimSpace(c.Tracker.Jira.Auth.Type)
}

func (c *Config) JiraEmail() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	value := resolveEnvValue(c.Tracker.Jira.Auth.Email, os.Getenv("JIRA_EMAIL"))
	return normalizeSecret(value)
}

func (c *Config) JiraAPIToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	value := resolveEnvValue(c.Tracker.Jira.Auth.APIToken, os.Getenv("JIRA_API_TOKEN"))
	return normalizeSecret(value)
}

func (c *Config) FeishuBaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	value := resolveEnvValue(c.Tracker.Feishu.BaseURL, os.Getenv("FEISHU_BASE_URL"))
	return strings.TrimSpace(value)
}

func (c *Config) FeishuProjectKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	value := resolveEnvValue(c.Tracker.Feishu.ProjectKey, os.Getenv("FEISHU_PROJECT_KEY"))
	return strings.TrimSpace(value)
}

func (c *Config) FeishuAppID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	value := resolveEnvValue(c.Tracker.Feishu.AppID, os.Getenv("FEISHU_APP_ID"))
	return normalizeSecret(value)
}

func (c *Config) FeishuAppSecret() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	value := resolveEnvValue(c.Tracker.Feishu.AppSecret, os.Getenv("FEISHU_APP_SECRET"))
	return normalizeSecret(value)
}

func (c *Config) WorkspaceRoot() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	defaultRoot := filepath.Join(os.TempDir(), "baton_workspaces")
	return resolvePathValue(c.RawConfig, defaultRoot)
}

func (c *Config) WorkflowPrompt() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if strings.TrimSpace(c.promptTemplate) == "" {
		return defaultPrompt
	}
	return c.promptTemplate
}

func (c *Config) CodexApprovalPolicy() any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	policy, err := c.resolveCodexApprovalPolicyLocked()
	if err != nil {
		return cloneMap(defaultApprovalPolicy)
	}
	return policy
}

func (c *Config) CodexThreadSandbox() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	sandbox, err := c.resolveCodexThreadSandboxLocked()
	if err != nil {
		return defaultThreadSandbox
	}
	return sandbox
}

func (c *Config) CodexTurnSandboxPolicy(workspace string) map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	policy, err := c.resolveCodexTurnSandboxPolicyLocked(workspace)
	if err != nil {
		return c.defaultCodexTurnSandboxPolicyLocked(workspace)
	}
	return policy
}

func (c *Config) CodexRuntimeSettings(workspace string) (*CodexRuntimeSettings, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.codexRuntimeSettingsLocked(workspace)
}

func (c *Config) codexRuntimeSettingsLocked(workspace string) (*CodexRuntimeSettings, error) {
	approvalPolicy, err := c.resolveCodexApprovalPolicyLocked()
	if err != nil {
		return nil, err
	}
	threadSandbox, err := c.resolveCodexThreadSandboxLocked()
	if err != nil {
		return nil, err
	}
	turnSandboxPolicy, err := c.resolveCodexTurnSandboxPolicyLocked(workspace)
	if err != nil {
		return nil, err
	}

	return &CodexRuntimeSettings{
		ApprovalPolicy:    approvalPolicy,
		ThreadSandbox:     threadSandbox,
		TurnSandboxPolicy: turnSandboxPolicy,
	}, nil
}

func (c *Config) resolveCodexApprovalPolicyLocked() (any, error) {
	switch value := c.AgentRuntime.Codex.ApprovalPolicy.(type) {
	case nil:
		return cloneMap(defaultApprovalPolicy), nil
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return nil, &ValidationError{Code: ErrInvalidCodexApproval, Value: value}
		}
		return trimmed, nil
	case map[string]any:
		return value, nil
	default:
		return nil, &ValidationError{Code: ErrInvalidCodexApproval, Value: value}
	}
}

func (c *Config) resolveCodexThreadSandboxLocked() (string, error) {
	switch value := c.AgentRuntime.Codex.ThreadSandbox.(type) {
	case nil:
		return defaultThreadSandbox, nil
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return "", &ValidationError{Code: ErrInvalidCodexThreadSandbox, Value: value}
		}
		return trimmed, nil
	default:
		return "", &ValidationError{Code: ErrInvalidCodexThreadSandbox, Value: value}
	}
}

func (c *Config) resolveCodexTurnSandboxPolicyLocked(workspace string) (map[string]any, error) {
	switch value := c.AgentRuntime.Codex.TurnSandbox.(type) {
	case nil:
		return c.defaultCodexTurnSandboxPolicyLocked(workspace), nil
	case map[string]any:
		return value, nil
	default:
		return nil, &ValidationError{Code: ErrInvalidCodexTurnSandbox, Value: value}
	}
}

func (c *Config) defaultCodexTurnSandboxPolicyLocked(workspace string) map[string]any {
	writableRoot := strings.TrimSpace(workspace)
	if writableRoot == "" {
		defaultRoot := filepath.Join(os.TempDir(), "baton_workspaces")
		writableRoot = resolvePathValue(c.RawConfig, defaultRoot)
	}
	writableRoot = filepath.Clean(writableRoot)
	if strings.HasPrefix(writableRoot, "~") {
		writableRoot = expandHomeDir(writableRoot)
	}
	if strings.Contains(writableRoot, "/") || strings.Contains(writableRoot, "\\") {
		writableRoot = filepath.Clean(expandPathMaybe(writableRoot))
	}

	return map[string]any{
		"type":                "workspaceWrite",
		"writableRoots":       []any{writableRoot},
		"readOnlyAccess":      map[string]any{"type": "fullAccess"},
		"networkAccess":       false,
		"excludeTmpdirEnvVar": false,
		"excludeSlashTmp":     false,
	}
}

func (c *Config) copyFromLocked(next *Config) {
	c.WorkflowPath = next.WorkflowPath
	c.RawConfig = next.RawConfig
	c.Tracker = next.Tracker
	c.Polling = next.Polling
	c.Agent = next.Agent
	c.AgentRuntime = next.AgentRuntime
	c.Hooks = next.Hooks
	c.Observability = next.Observability
	c.Server = next.Server
	c.promptTemplate = next.promptTemplate
}

func normalizeTrackerKind(value string) string {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	return trimmed
}

func normalizeAgentRuntimeKind(value string) string {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	if trimmed == "" {
		return "codex"
	}
	return trimmed
}

func validClaudePermissionMode(value string) bool {
	switch strings.TrimSpace(value) {
	case "acceptEdits", "bypassPermissions", "default", "delegate", "dontAsk", "plan":
		return true
	default:
		return false
	}
}

func codexConfigSource(raw map[string]any) map[string]any {
	agentRuntimeRaw, _ := getPath(raw, "agent_runtime").(map[string]any)
	if codexRaw, ok := getPath(agentRuntimeRaw, "codex").(map[string]any); ok && codexRaw != nil {
		return codexRaw
	}
	if legacyRaw, ok := getPath(raw, "codex").(map[string]any); ok && legacyRaw != nil {
		return legacyRaw
	}
	return map[string]any{}
}

func normalizeIssueState(value string) string {
	return strings.TrimSpace(strings.ToLower(value))
}

func parseCSV(value any, defaultValue []string) []string {
	switch v := value.(type) {
	case nil:
		return cloneStrings(defaultValue)
	case string:
		parts := strings.Split(v, ",")
		normalized := make([]string, 0, len(parts))
		for _, part := range parts {
			trimmed := strings.TrimSpace(part)
			if trimmed != "" {
				normalized = append(normalized, trimmed)
			}
		}
		if len(normalized) == 0 {
			return cloneStrings(defaultValue)
		}
		return normalized
	case []any:
		normalized := make([]string, 0, len(v))
		for _, item := range v {
			itemValue := scalarString(item)
			if strings.TrimSpace(itemValue) != "" {
				normalized = append(normalized, strings.TrimSpace(itemValue))
			}
		}
		if len(normalized) == 0 {
			return cloneStrings(defaultValue)
		}
		return normalized
	default:
		return cloneStrings(defaultValue)
	}
}

func parseStateLimits(value any) map[string]int {
	rawMap, ok := value.(map[string]any)
	if !ok {
		return map[string]int{}
	}
	result := map[string]int{}
	for rawState, rawLimit := range rawMap {
		limit, ok := parsePositiveInt(rawLimit)
		if !ok {
			continue
		}
		result[normalizeIssueState(rawState)] = limit
	}
	return result
}

func parseMapList(value any) []map[string]any {
	rawList, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]map[string]any, 0, len(rawList))
	for _, item := range rawList {
		rawMap, ok := item.(map[string]any)
		if !ok || rawMap == nil {
			continue
		}
		result = append(result, cloneConfigMap(rawMap))
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func commandWithDefault(value any, defaultValue string) string {
	stringValue := scalarString(value)
	if strings.TrimSpace(stringValue) == "" {
		return defaultValue
	}
	return strings.TrimSpace(stringValue)
}

func hookValue(value any) string {
	switch v := value.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return ""
		}
		return strings.TrimRight(v, "\n")
	default:
		return ""
	}
}

func intWithDefault(value any, defaultValue int) int {
	parsed, ok := parseInt(value)
	if !ok {
		return defaultValue
	}
	return parsed
}

func positiveIntWithDefault(value any, defaultValue int) int {
	parsed, ok := parsePositiveInt(value)
	if !ok {
		return defaultValue
	}
	return parsed
}

func optionalNonNegativeInt(value any) *int {
	parsed, ok := parseNonNegativeInt(value)
	if !ok {
		return nil
	}
	return &parsed
}

func boolWithDefault(value any, defaultValue bool) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true":
			return true
		case "false":
			return false
		default:
			return defaultValue
		}
	default:
		return defaultValue
	}
}

func parseInt(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func parsePositiveInt(value any) (int, bool) {
	parsed, ok := parseInt(value)
	if !ok || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

func parseNonNegativeInt(value any) (int, bool) {
	parsed, ok := parseInt(value)
	if !ok || parsed < 0 {
		return 0, false
	}
	return parsed, true
}

func binaryValue(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func scalarString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

func withDefault(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func getPath(raw map[string]any, path ...string) any {
	current := any(raw)
	for _, segment := range path {
		mapped, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		next, exists := mapped[segment]
		if !exists {
			return nil
		}
		current = next
	}
	return current
}

func normalizeMap(input map[string]any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	normalized := make(map[string]any, len(input))
	for key, value := range input {
		normalized[fmt.Sprint(key)] = normalizeValue(value)
	}
	return normalized
}

func normalizeValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return normalizeMap(v)
	case map[any]any:
		converted := make(map[string]any, len(v))
		for key, inner := range v {
			converted[fmt.Sprint(key)] = normalizeValue(inner)
		}
		return converted
	case []any:
		result := make([]any, 0, len(v))
		for _, item := range v {
			result = append(result, normalizeValue(item))
		}
		return result
	default:
		return v
	}
}

func cloneMapList(input []map[string]any) []map[string]any {
	if len(input) == 0 {
		return nil
	}
	result := make([]map[string]any, 0, len(input))
	for _, item := range input {
		result = append(result, cloneConfigMap(item))
	}
	return result
}

func cloneConfigMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = cloneConfigValue(value)
	}
	return result
}

func cloneConfigValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return cloneConfigMap(v)
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			result[i] = cloneConfigValue(item)
		}
		return result
	default:
		return v
	}
}

func resolveEnvValue(value string, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	if !envReferencePattern.MatchString(trimmed) {
		return trimmed
	}
	envName := strings.TrimPrefix(trimmed, "$")
	resolved, exists := os.LookupEnv(envName)
	if !exists {
		return fallback
	}
	return resolved
}

func resolvePathValue(raw map[string]any, defaultValue string) string {
	value := binaryValue(getPath(raw, "workspace", "root"))
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return defaultValue
	}

	if envReferencePattern.MatchString(trimmed) {
		envName := strings.TrimPrefix(trimmed, "$")
		resolved, exists := os.LookupEnv(envName)
		if !exists {
			return defaultValue
		}
		trimmed = strings.TrimSpace(resolved)
		if trimmed == "" {
			return defaultValue
		}
	}

	return expandPathMaybe(trimmed)
}

func expandPathMaybe(path string) string {
	if strings.Contains(path, "://") {
		return path
	}
	if strings.HasPrefix(path, "~") {
		path = expandHomeDir(path)
	}
	if strings.Contains(path, "/") || strings.Contains(path, "\\") {
		return filepath.Clean(path)
	}
	return path
}

func expandHomeDir(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return path
}

func normalizeSecret(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	return trimmed
}

func cloneMap(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func cloneStrings(input []string) []string {
	output := make([]string, len(input))
	copy(output, input)
	return output
}
