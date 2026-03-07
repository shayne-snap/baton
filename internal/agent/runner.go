package agent

import (
	"context"
	"fmt"
	"strings"

	"baton/internal/codex"
	"baton/internal/config"
	"baton/internal/prompt"
	"baton/internal/runtime"
	claudecoderuntime "baton/internal/runtime/claudecode"
	"baton/internal/runtime/codexruntime"
	opencoderuntime "baton/internal/runtime/opencode"
	"baton/internal/tracker"
	"baton/internal/workspace"
)

type IssueStateFetcher func(ctx context.Context, ids []string) ([]tracker.Issue, error)
type RuntimeUpdateHandler func(issueID string, update runtime.Update)

type RunOptions struct {
	Attempt           *int
	MaxTurns          int
	IssueStateFetcher IssueStateFetcher
	OnRuntimeUpdate   RuntimeUpdateHandler
	OnCodexUpdate     func(issueID string, update codex.Update)
}

type Runner interface {
	Run(ctx context.Context, issue tracker.Issue, opts RunOptions) error
}

type runner struct {
	config        *config.Config
	workspace     workspace.Manager
	tracker       tracker.Client
	agentRuntime  runtime.Runtime
	promptBuilder *prompt.Builder
	logsRoot      string
}

type RunnerOptions struct {
	LogsRoot string
}

func NewRunner(cfg *config.Config, manager workspace.Manager, trackerClient tracker.Client, options ...RunnerOptions) Runner {
	var opts RunnerOptions
	if len(options) > 0 {
		opts = options[0]
	}
	return &runner{
		config:        cfg,
		workspace:     manager,
		tracker:       trackerClient,
		agentRuntime:  newAgentRuntime(cfg),
		promptBuilder: prompt.NewBuilder(cfg),
		logsRoot:      opts.LogsRoot,
	}
}

func newAgentRuntime(cfg *config.Config) runtime.Runtime {
	switch cfg.AgentRuntimeKind() {
	case "codex":
		return codexruntime.New(cfg)
	case "opencode":
		return opencoderuntime.New(cfg)
	case "claudecode":
		return claudecoderuntime.New(cfg)
	default:
		return codexruntime.New(cfg)
	}
}

func (r *runner) Run(ctx context.Context, issue tracker.Issue, opts RunOptions) error {
	workspacePath, err := r.workspace.CreateForIssue(ctx, issue)
	if err != nil {
		return fmt.Errorf("agent run failed for issue_id=%s issue_identifier=%s: %w", issue.ID, issue.Identifier, err)
	}

	defer r.workspace.RunAfterRunHook(ctx, workspacePath, issue)

	if err := r.workspace.RunBeforeRunHook(ctx, workspacePath, issue); err != nil {
		return fmt.Errorf("agent run failed for issue_id=%s issue_identifier=%s: %w", issue.ID, issue.Identifier, err)
	}

	maxTurns := opts.MaxTurns
	if maxTurns <= 0 {
		maxTurns = r.config.AgentMaxTurns()
	}

	issueStateFetcher := opts.IssueStateFetcher
	if issueStateFetcher == nil {
		issueStateFetcher = r.tracker.FetchIssueStatesByIDs
	}

	session, err := r.agentRuntime.StartSession(workspacePath)
	if err != nil {
		return fmt.Errorf("agent run failed for issue_id=%s issue_identifier=%s: %w", issue.ID, issue.Identifier, err)
	}
	defer r.agentRuntime.StopSession(session)

	sessionLogger := newSessionLogWriter(r.logsRoot, r.config.AgentRuntimeKind(), issue.Identifier)
	defer sessionLogger.Close()

	currentIssue := issue
	for turnNumber := 1; turnNumber <= maxTurns; turnNumber++ {
		promptText, err := r.buildTurnPrompt(currentIssue, opts, turnNumber, maxTurns)
		if err != nil {
			return fmt.Errorf("agent run failed for issue_id=%s issue_identifier=%s: %w", issue.ID, issue.Identifier, err)
		}

		_, err = r.agentRuntime.RunTurn(session, promptText, currentIssue, runtime.RunTurnOptions{
			OnMessage: func(update runtime.Update) {
				sessionLogger.WriteUpdate(update)
				if strings.TrimSpace(currentIssue.ID) == "" {
					return
				}
				if opts.OnRuntimeUpdate != nil {
					opts.OnRuntimeUpdate(currentIssue.ID, update)
				}
				if opts.OnCodexUpdate != nil {
					opts.OnCodexUpdate(currentIssue.ID, codex.Update(update))
				}
			},
		})
		if err != nil {
			return fmt.Errorf("agent run failed for issue_id=%s issue_identifier=%s: %w", issue.ID, issue.Identifier, err)
		}

		nextIssue, shouldContinue, err := r.continueWithIssue(ctx, currentIssue, issueStateFetcher)
		if err != nil {
			return fmt.Errorf("agent run failed for issue_id=%s issue_identifier=%s: %w", issue.ID, issue.Identifier, err)
		}
		currentIssue = nextIssue

		if !shouldContinue {
			return nil
		}
	}

	return nil
}

func (r *runner) continueWithIssue(
	ctx context.Context,
	issue tracker.Issue,
	fetcher IssueStateFetcher,
) (tracker.Issue, bool, error) {
	if strings.TrimSpace(issue.ID) == "" {
		return issue, false, nil
	}
	issues, err := fetcher(ctx, []string{issue.ID})
	if err != nil {
		return issue, false, fmt.Errorf("issue_state_refresh_failed: %w", err)
	}
	if len(issues) == 0 {
		return issue, false, nil
	}

	refreshed := issues[0]
	if r.activeIssueState(refreshed.State) {
		return refreshed, true, nil
	}
	return refreshed, false, nil
}

func (r *runner) activeIssueState(stateName string) bool {
	normalized := normalizeIssueState(stateName)
	for _, state := range r.config.LinearActiveStates() {
		if normalizeIssueState(state) == normalized {
			return true
		}
	}
	return false
}

func normalizeIssueState(stateName string) string {
	return strings.ToLower(strings.TrimSpace(stateName))
}

func (r *runner) buildTurnPrompt(issue tracker.Issue, opts RunOptions, turnNumber int, maxTurns int) (string, error) {
	if turnNumber == 1 {
		return r.promptBuilder.BuildPrompt(issue, opts.Attempt)
	}

	return fmt.Sprintf(`Continuation guidance:

- The previous agent turn completed normally, but the tracker issue is still in an active state.
- This is continuation turn #%d of %d for the current agent run.
- Resume from the current workspace and workpad state instead of restarting from scratch.
- The original task instructions and prior turn context are already present in this thread, so do not restate them before acting.
- Focus on the remaining ticket work and do not end the turn while the issue stays active unless you are truly blocked.
`, turnNumber, maxTurns), nil
}
