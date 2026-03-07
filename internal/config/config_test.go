package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"baton/internal/workflow"
)

func TestConfigDefaultsAndValidation(t *testing.T) {
	t.Parallel()

	cfg := mustConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "linear",
			"linear": map[string]any{
				"api_key":      nil,
				"project_slug": nil,
			},
		},
		"polling": map[string]any{
			"interval_ms": nil,
		},
		"codex": map[string]any{
			"command": nil,
		},
	}, "")

	if got := cfg.PollIntervalMS(); got != 30_000 {
		t.Fatalf("poll interval default mismatch: %d", got)
	}
	assertStringSliceEqual(t, cfg.LinearActiveStates(), []string{"Todo", "In Progress"})
	assertStringSliceEqual(t, cfg.LinearTerminalStates(), []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"})
	if got := cfg.AgentMaxTurns(); got != 20 {
		t.Fatalf("agent max turns default mismatch: %d", got)
	}
	if got := cfg.CodexCommand(); got != "codex app-server" {
		t.Fatalf("codex command default mismatch: %q", got)
	}

	cfg = mustConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "linear",
			"linear": map[string]any{
				"api_key":      "token",
				"project_slug": nil,
			},
		},
	}, "")
	err := cfg.Validate()
	assertValidationCode(t, err, ErrMissingLinearProjectSlug)

	cfg = mustConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "linear",
			"linear": map[string]any{
				"api_key":      "token",
				"project_slug": "project",
			},
		},
		"codex": map[string]any{
			"command": "",
		},
	}, "")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected validate ok, got %v", err)
	}

	cfg = mustConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "linear",
			"linear": map[string]any{
				"api_key":      "token",
				"project_slug": "project",
			},
		},
		"codex": map[string]any{"approval_policy": "definitely-not-valid"},
	}, "")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected validate ok for free-form string approval policy, got %v", err)
	}

	cfg = mustConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "linear",
			"linear": map[string]any{
				"api_key":      "token",
				"project_slug": "project",
			},
		},
		"codex": map[string]any{"thread_sandbox": "unsafe-ish"},
	}, "")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected validate ok for free-form string thread sandbox, got %v", err)
	}

	cfg = mustConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "linear",
			"linear": map[string]any{
				"api_key":      "token",
				"project_slug": "project",
			},
		},
		"codex": map[string]any{"turn_sandbox_policy": map[string]any{"type": "workspaceWrite"}},
	}, "")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected validate ok for object-form turn sandbox policy, got %v", err)
	}

	cfg = mustConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "linear",
			"linear": map[string]any{
				"api_key":      "token",
				"project_slug": "project",
			},
		},
		"codex": map[string]any{"approval_policy": 123},
	}, "")
	assertValidationCode(t, cfg.Validate(), ErrInvalidCodexApproval)

	cfg = mustConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "linear",
			"linear": map[string]any{
				"api_key":      "token",
				"project_slug": "project",
			},
		},
		"codex": map[string]any{"thread_sandbox": 123},
	}, "")
	assertValidationCode(t, cfg.Validate(), ErrInvalidCodexThreadSandbox)

	cfg = mustConfig(t, map[string]any{
		"tracker": map[string]any{"kind": 123},
	}, "")
	err = cfg.Validate()
	assertValidationCode(t, err, ErrUnsupportedTrackerKind)
	if verr := new(ValidationError); errors.As(err, &verr) {
		if verr.Value != "123" {
			t.Fatalf("expected unsupported kind value to be %q, got %#v", "123", verr.Value)
		}
	}

	cfg = mustConfig(t, map[string]any{
		"tracker": map[string]any{"kind": "memory"},
		"agent_runtime": map[string]any{
			"kind": "opencode",
			"opencode": map[string]any{
				"command": "opencode serve --port 0",
				"permission": []any{
					map[string]any{"permission": "*", "pattern": "*", "action": "allow"},
				},
			},
		},
	}, "")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected opencode validate ok, got %v", err)
	}
	if got := cfg.OpencodeCommand(); got != "opencode serve --port 0" {
		t.Fatalf("opencode command mismatch: %q", got)
	}
	assertMapListEqual(t, cfg.OpencodePermissionRules(), []map[string]any{{"permission": "*", "pattern": "*", "action": "allow"}})

	cfg = mustConfig(t, map[string]any{
		"tracker": map[string]any{"kind": "memory"},
		"agent_runtime": map[string]any{
			"kind": "opencode",
			"opencode": map[string]any{
				"command": "",
			},
		},
	}, "")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected opencode blank command to use default, got %v", err)
	}
	if got := cfg.OpencodeCommand(); got != "opencode serve" {
		t.Fatalf("expected default opencode command, got %q", got)
	}
}

func TestConfigJiraAndFeishuValidation(t *testing.T) {
	t.Parallel()

	jiraCfg := mustConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "jira",
			"jira": map[string]any{
				"base_url":    "https://example.atlassian.net",
				"project_key": "BAT",
				"auth": map[string]any{
					"type":      "email_api_token",
					"email":     "bot@example.com",
					"api_token": "jira-token",
				},
			},
		},
	}, "")
	if err := jiraCfg.Validate(); err != nil {
		t.Fatalf("expected valid jira config, got %v", err)
	}

	badJira := mustConfig(t, map[string]any{
		"tracker": map[string]any{"kind": "jira"},
	}, "")
	assertValidationCode(t, badJira.Validate(), ErrMissingJiraBaseURL)

	feishuCfg := mustConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "feishu",
			"feishu": map[string]any{
				"base_url":    "https://open.feishu.cn",
				"project_key": "BAT",
				"app_id":      "app-id",
				"app_secret":  "app-secret",
			},
		},
	}, "")
	if err := feishuCfg.Validate(); err != nil {
		t.Fatalf("expected valid feishu config, got %v", err)
	}

	badFeishu := mustConfig(t, map[string]any{
		"tracker": map[string]any{"kind": "feishu"},
	}, "")
	assertValidationCode(t, badFeishu.Validate(), ErrMissingFeishuBaseURL)

	prevBaseURL := os.Getenv("FEISHU_BASE_URL")
	prevProjectKey := os.Getenv("FEISHU_PROJECT_KEY")
	prevAppID := os.Getenv("FEISHU_APP_ID")
	prevAppSecret := os.Getenv("FEISHU_APP_SECRET")
	defer restoreEnv("FEISHU_BASE_URL", prevBaseURL)
	defer restoreEnv("FEISHU_PROJECT_KEY", prevProjectKey)
	defer restoreEnv("FEISHU_APP_ID", prevAppID)
	defer restoreEnv("FEISHU_APP_SECRET", prevAppSecret)
	if err := os.Setenv("FEISHU_BASE_URL", "https://open.feishu.cn"); err != nil {
		t.Fatalf("set FEISHU_BASE_URL: %v", err)
	}
	if err := os.Setenv("FEISHU_PROJECT_KEY", "BAT"); err != nil {
		t.Fatalf("set FEISHU_PROJECT_KEY: %v", err)
	}
	if err := os.Setenv("FEISHU_APP_ID", "app-id-from-env"); err != nil {
		t.Fatalf("set FEISHU_APP_ID: %v", err)
	}
	if err := os.Setenv("FEISHU_APP_SECRET", "app-secret-from-env"); err != nil {
		t.Fatalf("set FEISHU_APP_SECRET: %v", err)
	}
	envFeishuCfg := mustConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "feishu",
			"feishu": map[string]any{
				"base_url":    "$FEISHU_BASE_URL",
				"project_key": "$FEISHU_PROJECT_KEY",
				"app_id":      "$FEISHU_APP_ID",
				"app_secret":  "$FEISHU_APP_SECRET",
			},
		},
	}, "")
	if err := envFeishuCfg.Validate(); err != nil {
		t.Fatalf("expected env-backed feishu config to validate, got %v", err)
	}
	if got := envFeishuCfg.FeishuBaseURL(); got != "https://open.feishu.cn" {
		t.Fatalf("unexpected feishu base url from env: %q", got)
	}
	if got := envFeishuCfg.FeishuProjectKey(); got != "BAT" {
		t.Fatalf("unexpected feishu project key from env: %q", got)
	}
	if got := envFeishuCfg.FeishuAppID(); got != "app-id-from-env" {
		t.Fatalf("unexpected feishu app id from env: %q", got)
	}
	if got := envFeishuCfg.FeishuAppSecret(); got != "app-secret-from-env" {
		t.Fatalf("unexpected feishu app secret from env: %q", got)
	}
}

func TestClaudeCodeRuntimeConfigDefaultsAndValidation(t *testing.T) {
	t.Parallel()

	cfg := mustConfig(t, map[string]any{
		"tracker": map[string]any{"kind": "memory"},
		"agent_runtime": map[string]any{
			"kind":       "claudecode",
			"claudecode": map[string]any{},
		},
	}, "")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected claudecode validate ok, got %v", err)
	}
	if got := cfg.AgentRuntimeKind(); got != "claudecode" {
		t.Fatalf("unexpected runtime kind: %q", got)
	}
	if got := cfg.ClaudeCodeCommand(); got != "claude" {
		t.Fatalf("unexpected default Claude command: %q", got)
	}
	if got := cfg.ClaudeCodePermissionMode(); got != "dontAsk" {
		t.Fatalf("unexpected default permission mode: %q", got)
	}
	if !cfg.ClaudeCodeMCPStrict() {
		t.Fatal("expected strict MCP config by default")
	}
	if !cfg.ClaudeCodeSessionPersistence() {
		t.Fatal("expected session persistence by default")
	}

	cfg = mustConfig(t, map[string]any{
		"tracker": map[string]any{"kind": "memory"},
		"agent_runtime": map[string]any{
			"kind": "claudecode",
			"claudecode": map[string]any{
				"command":              "claude --verbose",
				"permission_mode":      "acceptEdits",
				"allowed_tools":        "Read,Write",
				"disallowed_tools":     []any{"Bash"},
				"model":                "claude-sonnet-4-5",
				"append_system_prompt": "Follow tracker state exactly.",
				"mcp_strict":           false,
				"session_persistence":  false,
			},
		},
	}, "")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected explicit claudecode config validate ok, got %v", err)
	}
	if got := cfg.ClaudeCodeCommand(); got != "claude --verbose" {
		t.Fatalf("unexpected configured command: %q", got)
	}
	if got := cfg.ClaudeCodePermissionMode(); got != "acceptEdits" {
		t.Fatalf("unexpected permission mode: %q", got)
	}
	assertStringSliceEqual(t, cfg.ClaudeCodeAllowedTools(), []string{"Read", "Write"})
	assertStringSliceEqual(t, cfg.ClaudeCodeDisallowedTools(), []string{"Bash"})
	if got := cfg.ClaudeCodeModel(); got != "claude-sonnet-4-5" {
		t.Fatalf("unexpected model: %q", got)
	}
	if got := cfg.ClaudeCodeAppendSystemPrompt(); got != "Follow tracker state exactly." {
		t.Fatalf("unexpected append system prompt: %q", got)
	}
	if cfg.ClaudeCodeMCPStrict() {
		t.Fatal("expected strict MCP override to be false")
	}
	if cfg.ClaudeCodeSessionPersistence() {
		t.Fatal("expected session persistence override to be false")
	}

	badCfg := mustConfig(t, map[string]any{
		"tracker": map[string]any{"kind": "memory"},
		"agent_runtime": map[string]any{
			"kind": "claudecode",
			"claudecode": map[string]any{
				"permission_mode": "definitely-not-valid",
			},
		},
	}, "")
	assertValidationCode(t, badCfg.Validate(), ErrInvalidClaudeCodePermission)
}

func TestConfigRejectsLegacyTrackerLayout(t *testing.T) {
	t.Parallel()

	cfg := mustConfigRaw(t, map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      "token",
			"project_slug": "project",
		},
	}, "")
	assertValidationCode(t, cfg.Validate(), ErrMissingTrackerLifecycleState)
}

func TestConfigEnvResolution(t *testing.T) {
	t.Parallel()

	const tokenValue = "test-linear-api-key"
	prev := os.Getenv("LINEAR_API_KEY")
	defer restoreEnv("LINEAR_API_KEY", prev)
	if err := os.Setenv("LINEAR_API_KEY", tokenValue); err != nil {
		t.Fatalf("set env: %v", err)
	}

	cfg := mustConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "linear",
			"linear": map[string]any{
				"api_key":      nil,
				"project_slug": "project",
			},
		},
	}, "")

	if got := cfg.LinearAPIToken(); got != tokenValue {
		t.Fatalf("linear token env fallback mismatch: %q", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected validate ok, got %v", err)
	}

	cfg = mustConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "linear",
			"linear": map[string]any{
				"api_key":      "$LINEAR_API_KEY",
				"project_slug": "project",
			},
		},
	}, "")

	if got := cfg.LinearAPIToken(); got != tokenValue {
		t.Fatalf("linear token $VAR resolution mismatch: %q", got)
	}
}

func TestWorkspaceRootPathResolution(t *testing.T) {
	t.Parallel()

	defaultRoot := filepath.Join(os.TempDir(), "baton_workspaces")
	cfg := mustConfig(t, map[string]any{"tracker": map[string]any{"kind": "memory"}}, "")
	if got := cfg.WorkspaceRoot(); got != defaultRoot {
		t.Fatalf("default workspace root mismatch: %q", got)
	}

	cfg = mustConfig(t, map[string]any{
		"tracker": map[string]any{"kind": "memory"},
		"workspace": map[string]any{
			"root": "relative-root",
		},
	}, "")
	if got := cfg.WorkspaceRoot(); got != "relative-root" {
		t.Fatalf("expected bare relative root to be preserved, got %q", got)
	}

	const envName = "BATON_TEST_WORKSPACE_ROOT"
	prev := os.Getenv(envName)
	defer restoreEnv(envName, prev)
	if err := os.Setenv(envName, filepath.Join(t.TempDir(), "workspaces")); err != nil {
		t.Fatalf("set env: %v", err)
	}

	cfg = mustConfig(t, map[string]any{
		"tracker": map[string]any{"kind": "memory"},
		"workspace": map[string]any{
			"root": "$" + envName,
		},
	}, "")
	if got := cfg.WorkspaceRoot(); got == defaultRoot || got == "" {
		t.Fatalf("expected resolved workspace root, got %q", got)
	}
}

func TestStateLimitAndCSVParsing(t *testing.T) {
	t.Parallel()

	cfg := mustConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "memory",
			"routing": map[string]any{
				"active_states": "Todo,  Review,",
			},
		},
		"agent": map[string]any{
			"max_concurrent_agents": 10,
			"max_concurrent_agents_by_state": map[string]any{
				" todo ":      1,
				"In Progress": "4",
				"In Review":   2,
				"bad":         0,
			},
		},
	}, "")

	assertStringSliceEqual(t, cfg.LinearActiveStates(), []string{"Todo", "Review"})
	if got := cfg.MaxConcurrentAgentsForState("Todo"); got != 1 {
		t.Fatalf("expected todo limit 1, got %d", got)
	}
	if got := cfg.MaxConcurrentAgentsForState("In Progress"); got != 4 {
		t.Fatalf("expected in progress limit 4, got %d", got)
	}
	if got := cfg.MaxConcurrentAgentsForState("In Review"); got != 2 {
		t.Fatalf("expected in review limit 2, got %d", got)
	}
	if got := cfg.MaxConcurrentAgentsForState("Closed"); got != 10 {
		t.Fatalf("expected fallback global limit 10, got %d", got)
	}

	cfg = mustConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "memory",
			"routing": map[string]any{
				"active_states": ",",
			},
		},
	}, "")
	assertStringSliceEqual(t, cfg.LinearActiveStates(), []string{"Todo", "In Progress"})
}

func TestTrackerAssigneeExplicitEmptyDisablesEnvFallback(t *testing.T) {
	t.Setenv("BATON_ASSIGNEE", "me")
	t.Setenv("LINEAR_ASSIGNEE", "legacy-me")

	cfg := mustConfig(t, map[string]any{
		"tracker": map[string]any{
			"kind": "jira",
			"routing": map[string]any{
				"assignee": "",
			},
			"jira": map[string]any{
				"base_url":    "https://example.atlassian.net",
				"project_key": "KAN",
				"auth": map[string]any{
					"type":      "email_api_token",
					"email":     "jira@example.com",
					"api_token": "token",
				},
			},
			"lifecycle": map[string]any{
				"backlog":      "Backlog",
				"todo":         "To Do",
				"in_progress":  "In Progress",
				"human_review": "In Review",
				"merging":      "Ready to Merge",
				"rework":       "Rework",
				"done":         "Done",
			},
		},
	}, "")

	if got := cfg.TrackerAssignee(); got != "" {
		t.Fatalf("expected explicit empty assignee to disable fallback, got %q", got)
	}
}

func TestWorkflowPromptFallbackAndCodexDefaults(t *testing.T) {
	t.Parallel()

	cfg := mustConfig(t, map[string]any{"tracker": map[string]any{"kind": "memory"}}, "   \n")
	if cfg.WorkflowPrompt() == "" {
		t.Fatal("expected default workflow prompt when workflow body is blank")
	}

	policy, ok := cfg.CodexApprovalPolicy().(map[string]any)
	if !ok {
		t.Fatalf("expected default approval policy map, got %#v", cfg.CodexApprovalPolicy())
	}
	if _, ok := policy["reject"]; !ok {
		t.Fatalf("expected reject policy in default approval policy: %#v", policy)
	}
	if got := cfg.CodexThreadSandbox(); got != "workspace-write" {
		t.Fatalf("default codex thread sandbox mismatch: %q", got)
	}
	turnSandbox := cfg.CodexTurnSandboxPolicy("")
	if turnSandbox["type"] != "workspaceWrite" {
		t.Fatalf("default turn sandbox type mismatch: %#v", turnSandbox["type"])
	}
	if !cfg.ObservabilityEnabled() {
		t.Fatal("expected observability to be enabled by default")
	}
	if got := cfg.ObservabilityRefreshMS(); got != 1_000 {
		t.Fatalf("default observability refresh mismatch: %d", got)
	}
	if got := cfg.ObservabilityRenderIntervalMS(); got != 16 {
		t.Fatalf("default observability render interval mismatch: %d", got)
	}

	cfg = mustConfig(t, map[string]any{
		"tracker": map[string]any{"kind": "memory"},
		"codex": map[string]any{
			"approval_policy": "",
		},
	}, "")
	if _, ok := cfg.CodexApprovalPolicy().(map[string]any); !ok {
		t.Fatalf("expected fallback approval policy map for invalid empty string")
	}
	assertValidationCode(t, cfg.Validate(), ErrInvalidCodexApproval)

	cfg = mustConfig(t, map[string]any{
		"tracker": map[string]any{"kind": "memory"},
		"observability": map[string]any{
			"dashboard_enabled":  false,
			"refresh_ms":         250,
			"render_interval_ms": 32,
		},
	}, "")
	if cfg.ObservabilityEnabled() {
		t.Fatal("expected observability disabled override")
	}
	if cfg.ObservabilityRefreshMS() != 250 || cfg.ObservabilityRenderIntervalMS() != 32 {
		t.Fatalf("unexpected observability override values refresh=%d render=%d", cfg.ObservabilityRefreshMS(), cfg.ObservabilityRenderIntervalMS())
	}
}

func TestReloadFromDiskKeepsLastKnownGoodOnInvalidWorkflow(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")

	initial := "---\ntracker:\n  kind: memory\n  lifecycle:\n    backlog: Backlog\n    todo: Todo\n    in_progress: In Progress\n    human_review: In Review\n    merging: Merging\n    rework: Rework\n    done: Done\npolling:\n  interval_ms: 30000\n---\ninitial prompt\n"
	if err := os.WriteFile(workflowPath, []byte(initial), 0o644); err != nil {
		t.Fatalf("write initial workflow: %v", err)
	}

	definition, err := workflow.LoadFile(workflowPath)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	cfg, err := FromWorkflow(workflowPath, definition)
	if err != nil {
		t.Fatalf("FromWorkflow failed: %v", err)
	}

	updated := "---\ntracker:\n  kind: memory\n  lifecycle:\n    backlog: Backlog\n    todo: Todo\n    in_progress: In Progress\n    human_review: In Review\n    merging: Merging\n    rework: Rework\n    done: Done\npolling:\n  interval_ms: 1200\n---\nupdated prompt\n"
	if err := os.WriteFile(workflowPath, []byte(updated), 0o644); err != nil {
		t.Fatalf("write updated workflow: %v", err)
	}

	if err := cfg.ReloadFromDisk(); err != nil {
		t.Fatalf("reload updated workflow: %v", err)
	}
	if got := cfg.PollIntervalMS(); got != 1200 {
		t.Fatalf("expected updated poll interval 1200, got %d", got)
	}
	if got := cfg.WorkflowPrompt(); got != "updated prompt" {
		t.Fatalf("expected updated prompt, got %q", got)
	}

	invalid := "---\ntracker: [\n---\nbroken\n"
	if err := os.WriteFile(workflowPath, []byte(invalid), 0o644); err != nil {
		t.Fatalf("write invalid workflow: %v", err)
	}

	if err := cfg.ReloadFromDisk(); err == nil {
		t.Fatalf("expected reload failure for invalid workflow")
	}

	if got := cfg.PollIntervalMS(); got != 1200 {
		t.Fatalf("expected last known good poll interval after failed reload, got %d", got)
	}
	if got := cfg.WorkflowPrompt(); got != "updated prompt" {
		t.Fatalf("expected last known good prompt after failed reload, got %q", got)
	}
}

func mustConfig(t *testing.T, configMap map[string]any, prompt string) *Config {
	t.Helper()
	cfg, err := FromWorkflow(
		filepath.Join(t.TempDir(), "WORKFLOW.md"),
		&workflow.Definition{
			Config:         withRequiredTrackerDefaults(configMap),
			PromptTemplate: prompt,
		},
	)
	if err != nil {
		t.Fatalf("FromWorkflow failed: %v", err)
	}
	return cfg
}

func mustConfigRaw(t *testing.T, configMap map[string]any, prompt string) *Config {
	t.Helper()
	cfg, err := FromWorkflow(
		filepath.Join(t.TempDir(), "WORKFLOW.md"),
		&workflow.Definition{
			Config:         configMap,
			PromptTemplate: prompt,
		},
	)
	if err != nil {
		t.Fatalf("FromWorkflow failed: %v", err)
	}
	return cfg
}

func withRequiredTrackerDefaults(configMap map[string]any) map[string]any {
	merged := map[string]any{}
	for key, value := range configMap {
		merged[key] = value
	}

	trackerRaw, _ := merged["tracker"].(map[string]any)
	if trackerRaw == nil {
		trackerRaw = map[string]any{}
	}
	tracker := cloneTestMap(trackerRaw)
	if _, ok := tracker["lifecycle"]; !ok {
		tracker["lifecycle"] = map[string]any{
			"backlog":      "Backlog",
			"todo":         "Todo",
			"in_progress":  "In Progress",
			"human_review": "In Review",
			"merging":      "Merging",
			"rework":       "Rework",
			"done":         "Done",
		}
	}
	if _, ok := tracker["routing"]; !ok {
		tracker["routing"] = map[string]any{
			"active_states":   []any{"Todo", "In Progress"},
			"terminal_states": []any{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"},
		}
	}
	merged["tracker"] = tracker
	return merged
}

func cloneTestMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func assertValidationCode(t *testing.T, err error, expectedCode error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected validation error %v, got nil", expectedCode)
	}
	verr := new(ValidationError)
	if !errors.As(err, &verr) {
		t.Fatalf("expected ValidationError, got %T (%v)", err, err)
	}
	if !errors.Is(err, expectedCode) {
		t.Fatalf("expected code %v, got %v", expectedCode, err)
	}
}

func assertStringSliceEqual(t *testing.T, got []string, expected []string) {
	t.Helper()
	if len(got) != len(expected) {
		t.Fatalf("length mismatch: got=%v expected=%v", got, expected)
	}
	for idx := range got {
		if got[idx] != expected[idx] {
			t.Fatalf("value mismatch at %d: got=%v expected=%v", idx, got, expected)
		}
	}
}

func assertMapListEqual(t *testing.T, got []map[string]any, expected []map[string]any) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got rules: %v", err)
	}
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatalf("marshal expected rules: %v", err)
	}
	if string(gotJSON) != string(expectedJSON) {
		t.Fatalf("rule mismatch: got=%s expected=%s", gotJSON, expectedJSON)
	}
}

func restoreEnv(name string, prev string) {
	if prev == "" {
		_ = os.Unsetenv(name)
		return
	}
	_ = os.Setenv(name, prev)
}
