package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"baton/internal/agent"
	"baton/internal/config"
	"baton/internal/runtime"
	"baton/internal/tracker"
	"baton/internal/workspace"

	"github.com/rs/zerolog"
)

const (
	continuationRetryDelay = 1 * time.Second
	failureRetryBaseDelay  = 10 * time.Second
	pollTransitionDelay    = 20 * time.Millisecond
)

var (
	ErrOrchestratorUnavailable = errors.New("orchestrator_unavailable")
	ErrSnapshotTimeout         = errors.New("snapshot_timeout")
)

type Orchestrator struct {
	config    *config.Config
	tracker   tracker.Client
	workspace workspace.Manager
	runner    agent.Runner
	logger    zerolog.Logger

	workerDoneCh   chan workerDone
	workerUpdateCh chan workerUpdate
	retryIssueCh   chan string
	snapshotReqCh  chan snapshotRequest
	refreshReqCh   chan refreshRequest
	stopCh         chan struct{}

	closeOnce sync.Once

	runMu    sync.RWMutex
	isActive bool
}

type runtimeState struct {
	pollInterval        time.Duration
	maxConcurrentAgents int
	nextPollDueAt       time.Time
	pollCheckInProgress bool

	running       map[string]*runningEntry
	claimed       map[string]struct{}
	retryAttempts map[string]*retryEntry
	completed     map[string]struct{}

	codexTotals     codexTotals
	codexRateLimits map[string]any
}

type runningEntry struct {
	issue       tracker.Issue
	identifier  string
	startedAt   time.Time
	runtimeKind string

	sessionID         string
	codexAppServerPID string
	lastCodexEvent    string
	lastCodexMessage  map[string]any
	lastCodexAt       time.Time
	turnCount         int

	codexInputTokens   int
	codexOutputTokens  int
	codexTotalTokens   int
	lastReportedInput  int
	lastReportedOutput int
	lastReportedTotal  int
	retryAttempt       int

	cancel context.CancelFunc
}

type retryEntry struct {
	attempt    int
	timer      *time.Timer
	dueAt      time.Time
	identifier string
	err        string
}

type retryMetadata struct {
	identifier string
	err        string
	delayType  retryDelayType
}

type retryDelayType string

const (
	retryDelayContinuation retryDelayType = "continuation"
	retryDelayFailure      retryDelayType = "failure"
)

type codexTotals struct {
	InputTokens    int
	OutputTokens   int
	TotalTokens    int
	SecondsRunning int
}

type workerDone struct {
	issueID string
	err     error
}

type workerUpdate struct {
	issueID string
	update  runtime.Update
}

type tokenDelta struct {
	inputTokens  int
	outputTokens int
	totalTokens  int

	inputReported  int
	outputReported int
	totalReported  int
}

type snapshotRequest struct {
	reply chan map[string]any
}

type refreshRequest struct {
	reply chan map[string]any
}

func New(
	cfg *config.Config,
	client tracker.Client,
	workspaceManager workspace.Manager,
	runner agent.Runner,
	logger zerolog.Logger,
) *Orchestrator {
	return &Orchestrator{
		config:         cfg,
		tracker:        client,
		workspace:      workspaceManager,
		runner:         runner,
		logger:         logger,
		workerDoneCh:   make(chan workerDone, 512),
		workerUpdateCh: make(chan workerUpdate, 1024),
		retryIssueCh:   make(chan string, 512),
		snapshotReqCh:  make(chan snapshotRequest, 32),
		refreshReqCh:   make(chan refreshRequest, 32),
		stopCh:         make(chan struct{}),
	}
}

func (o *Orchestrator) Snapshot(timeout time.Duration) (map[string]any, error) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if !o.running() {
		return nil, ErrOrchestratorUnavailable
	}

	req := snapshotRequest{reply: make(chan map[string]any, 1)}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case o.snapshotReqCh <- req:
	case <-timer.C:
		return nil, ErrSnapshotTimeout
	}

	select {
	case snapshot := <-req.reply:
		return snapshot, nil
	case <-timer.C:
		return nil, ErrSnapshotTimeout
	}
}

func (o *Orchestrator) RequestRefresh() (map[string]any, error) {
	if !o.running() {
		return nil, ErrOrchestratorUnavailable
	}

	req := refreshRequest{reply: make(chan map[string]any, 1)}
	select {
	case o.refreshReqCh <- req:
	case <-o.stopCh:
		return nil, ErrOrchestratorUnavailable
	}

	select {
	case payload := <-req.reply:
		return payload, nil
	case <-o.stopCh:
		return nil, ErrOrchestratorUnavailable
	}
}

func (o *Orchestrator) running() bool {
	o.runMu.RLock()
	defer o.runMu.RUnlock()
	return o.isActive
}

func (o *Orchestrator) setRunning(active bool) {
	o.runMu.Lock()
	defer o.runMu.Unlock()
	o.isActive = active
}

func (o *Orchestrator) Run(ctx context.Context) error {
	if err := o.config.Validate(); err != nil {
		return err
	}

	state := newRuntimeState(o.config)
	o.setRunning(true)
	defer o.setRunning(false)

	o.logger.Info().Msg("orchestrator started")
	o.runTerminalWorkspaceCleanup(ctx)

	tickTimer := time.NewTimer(0)
	defer tickTimer.Stop()
	pollTimer := time.NewTimer(time.Hour)
	if !pollTimer.Stop() {
		select {
		case <-pollTimer.C:
		default:
		}
	}
	defer pollTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			o.logger.Info().Msg("orchestrator stopping")
			o.shutdown(state)
			return nil
		case <-tickTimer.C:
			o.refreshRuntimeConfig(state)
			state.pollCheckInProgress = true
			state.nextPollDueAt = time.Time{}
			resetTimer(pollTimer, pollTransitionDelay)
		case <-pollTimer.C:
			o.runPollCycle(ctx, state)
			state.pollCheckInProgress = false
			state.nextPollDueAt = time.Now().UTC().Add(state.pollInterval)
			resetTimer(tickTimer, state.pollInterval)
		case req := <-o.snapshotReqCh:
			req.reply <- o.snapshotPayload(state)
		case req := <-o.refreshReqCh:
			req.reply <- o.handleRefreshRequest(state, func() {
				resetTimer(tickTimer, 0)
			})
		case done := <-o.workerDoneCh:
			o.handleWorkerDone(state, done)
		case update := <-o.workerUpdateCh:
			o.handleWorkerUpdate(state, update)
		case issueID := <-o.retryIssueCh:
			o.handleRetryIssue(ctx, state, issueID)
		}
	}
}

func newRuntimeState(cfg *config.Config) *runtimeState {
	return &runtimeState{
		pollInterval:        pollDuration(cfg),
		maxConcurrentAgents: cfg.MaxConcurrentAgents(),
		nextPollDueAt:       time.Now().UTC(),
		running:             map[string]*runningEntry{},
		claimed:             map[string]struct{}{},
		retryAttempts:       map[string]*retryEntry{},
		completed:           map[string]struct{}{},
		codexTotals:         codexTotals{},
	}
}

func (o *Orchestrator) runPollCycle(ctx context.Context, state *runtimeState) {
	state = o.reconcileRunningIssues(ctx, state)

	if err := o.config.Validate(); err != nil {
		o.logValidationError(err)
		return
	}

	issues, err := o.tracker.FetchCandidateIssues(ctx)
	if err != nil {
		o.logger.Error().Err(err).Msg("failed to fetch candidate issues")
		return
	}

	o.sortIssuesForDispatch(issues)
	activeStates := activeStateSet(o.config)
	terminalStates := terminalStateSet(o.config)

	for _, issue := range issues {
		if o.availableSlots(state) <= 0 {
			break
		}
		if !o.shouldDispatchIssue(issue, state, activeStates, terminalStates) {
			continue
		}
		o.dispatchIssue(ctx, state, issue, nil)
	}
}

func (o *Orchestrator) snapshotPayload(state *runtimeState) map[string]any {
	now := time.Now().UTC()
	nowUnixMS := now.UnixMilli()

	running := make([]map[string]any, 0, len(state.running))
	for issueID, metadata := range state.running {
		runtimeSeconds := int(now.Sub(metadata.startedAt).Seconds())
		if runtimeSeconds < 0 {
			runtimeSeconds = 0
		}

		var lastTimestamp any
		if !metadata.lastCodexAt.IsZero() {
			lastTimestamp = metadata.lastCodexAt
		}

		running = append(running, map[string]any{
			"issue_id":             issueID,
			"identifier":           metadata.identifier,
			"state":                metadata.issue.State,
			"runtime_kind":         metadata.runtimeKind,
			"session_id":           metadata.sessionID,
			"codex_app_server_pid": metadata.codexAppServerPID,
			"codex_input_tokens":   metadata.codexInputTokens,
			"codex_output_tokens":  metadata.codexOutputTokens,
			"codex_total_tokens":   metadata.codexTotalTokens,
			"turn_count":           metadata.turnCount,
			"started_at":           metadata.startedAt,
			"last_codex_timestamp": lastTimestamp,
			"last_codex_message":   metadata.lastCodexMessage,
			"last_codex_event":     metadata.lastCodexEvent,
			"runtime_seconds":      runtimeSeconds,
		})
	}
	sort.Slice(running, func(i, j int) bool {
		return fmt.Sprintf("%v", running[i]["issue_id"]) < fmt.Sprintf("%v", running[j]["issue_id"])
	})

	retrying := make([]map[string]any, 0, len(state.retryAttempts))
	for issueID, retry := range state.retryAttempts {
		dueIn := retry.dueAt.UnixMilli() - nowUnixMS
		if dueIn < 0 {
			dueIn = 0
		}
		retrying = append(retrying, map[string]any{
			"issue_id":   issueID,
			"attempt":    retry.attempt,
			"due_in_ms":  dueIn,
			"identifier": retry.identifier,
			"error":      retry.err,
		})
	}
	sort.Slice(retrying, func(i, j int) bool {
		return fmt.Sprintf("%v", retrying[i]["issue_id"]) < fmt.Sprintf("%v", retrying[j]["issue_id"])
	})

	totals := map[string]any{
		"input_tokens":    state.codexTotals.InputTokens,
		"output_tokens":   state.codexTotals.OutputTokens,
		"total_tokens":    state.codexTotals.TotalTokens,
		"seconds_running": liveRuntimeSeconds(state, now),
	}

	var nextPollIn any
	if !state.nextPollDueAt.IsZero() {
		dueIn := state.nextPollDueAt.UnixMilli() - nowUnixMS
		if dueIn < 0 {
			dueIn = 0
		}
		nextPollIn = dueIn
	}

	return map[string]any{
		"running":      running,
		"retrying":     retrying,
		"codex_totals": totals,
		"rate_limits":  state.codexRateLimits,
		"polling": map[string]any{
			"checking?":        state.pollCheckInProgress,
			"next_poll_in_ms":  nextPollIn,
			"poll_interval_ms": int(state.pollInterval.Milliseconds()),
		},
	}
}

func (o *Orchestrator) handleRefreshRequest(state *runtimeState, queueNow func()) map[string]any {
	now := time.Now().UTC()
	alreadyDue := !state.nextPollDueAt.IsZero() && !state.nextPollDueAt.After(now)
	coalesced := state.pollCheckInProgress || alreadyDue
	if !coalesced {
		state.nextPollDueAt = now
		if queueNow != nil {
			queueNow()
		}
	}

	return map[string]any{
		"queued":       true,
		"coalesced":    coalesced,
		"requested_at": now,
		"operations":   []string{"poll", "reconcile"},
	}
}

func (o *Orchestrator) reconcileRunningIssues(ctx context.Context, state *runtimeState) *runtimeState {
	o.reconcileStalledRunningIssues(ctx, state)

	if len(state.running) == 0 {
		return state
	}

	runningIDs := make([]string, 0, len(state.running))
	for issueID := range state.running {
		runningIDs = append(runningIDs, issueID)
	}

	issues, err := o.tracker.FetchIssueStatesByIDs(ctx, runningIDs)
	if err != nil {
		o.logger.Debug().Err(err).Msg("failed to refresh running issue states; keeping active workers")
		return state
	}

	activeStates := activeStateSet(o.config)
	terminalStates := terminalStateSet(o.config)
	for _, issue := range issues {
		switch {
		case terminalIssueState(issue.State, terminalStates):
			o.logger.Info().Str("issue_id", issue.ID).Str("issue_identifier", issue.Identifier).Str("state", issue.State).Msg("issue moved to terminal state; stopping active agent")
			o.terminateRunningIssue(ctx, state, issue.ID, true)
		case !o.issueRoutableToWorker(issue):
			o.logger.Info().Str("issue_id", issue.ID).Str("issue_identifier", issue.Identifier).Str("assignee_id", issue.AssigneeID).Msg("issue no longer routed to this worker; stopping active agent")
			o.terminateRunningIssue(ctx, state, issue.ID, false)
		case activeIssueState(issue.State, activeStates):
			if entry, ok := state.running[issue.ID]; ok {
				entry.issue = issue
			}
		default:
			o.logger.Info().Str("issue_id", issue.ID).Str("issue_identifier", issue.Identifier).Str("state", issue.State).Msg("issue moved to non-active state; stopping active agent")
			o.terminateRunningIssue(ctx, state, issue.ID, false)
		}
	}

	return state
}

func (o *Orchestrator) reconcileStalledRunningIssues(ctx context.Context, state *runtimeState) {
	timeout := time.Duration(o.config.CodexStallTimeoutMS()) * time.Millisecond
	if timeout <= 0 || len(state.running) == 0 {
		return
	}

	now := time.Now().UTC()
	for issueID, entry := range state.running {
		lastAt := entry.startedAt
		if !entry.lastCodexAt.IsZero() {
			lastAt = entry.lastCodexAt
		}
		if now.Sub(lastAt) <= timeout {
			continue
		}

		elapsed := now.Sub(lastAt)
		o.logger.Warn().
			Str("issue_id", issueID).
			Str("issue_identifier", entry.identifier).
			Str("session_id", safeSessionID(entry.sessionID)).
			Int64("elapsed_ms", elapsed.Milliseconds()).
			Msg("issue stalled; restarting with backoff")

		next := nextRetryAttemptFromRunning(entry)
		o.terminateRunningIssue(ctx, state, issueID, false)
		o.scheduleIssueRetry(state, issueID, next, retryMetadata{
			identifier: entry.identifier,
			err:        fmt.Sprintf("stalled for %dms without codex activity", elapsed.Milliseconds()),
			delayType:  retryDelayFailure,
		})
	}
}

func (o *Orchestrator) handleWorkerDone(state *runtimeState, done workerDone) {
	entry, ok := state.running[done.issueID]
	if !ok {
		return
	}

	delete(state.running, done.issueID)
	o.recordSessionCompletionTotals(state, entry)

	if done.err == nil {
		state.completed[done.issueID] = struct{}{}
		o.scheduleIssueRetry(state, done.issueID, intPtr(1), retryMetadata{
			identifier: entry.identifier,
			delayType:  retryDelayContinuation,
		})
		o.logger.Info().
			Str("issue_id", done.issueID).
			Str("issue_identifier", fallbackIdentifier(done.issueID, entry.identifier)).
			Str("session_id", safeSessionID(entry.sessionID)).
			Msg("agent task completed; scheduling active-state continuation check")
		return
	}

	next := nextRetryAttemptFromRunning(entry)
	o.scheduleIssueRetry(state, done.issueID, next, retryMetadata{
		identifier: entry.identifier,
		err:        "agent exited: " + done.err.Error(),
		delayType:  retryDelayFailure,
	})
	o.logger.Warn().
		Str("issue_id", done.issueID).
		Str("issue_identifier", fallbackIdentifier(done.issueID, entry.identifier)).
		Str("session_id", safeSessionID(entry.sessionID)).
		Err(done.err).
		Msg("agent task exited; scheduling retry")
}

func (o *Orchestrator) handleWorkerUpdate(state *runtimeState, update workerUpdate) {
	entry, ok := state.running[update.issueID]
	if !ok {
		return
	}

	delta := extractTokenDelta(entry, update.update)
	prevSessionID := entry.sessionID
	nextSessionID := sessionIDForUpdate(prevSessionID, update.update)

	entry.lastCodexAt = update.update.Timestamp
	entry.lastCodexEvent = update.update.Event
	entry.lastCodexMessage = summarizeUpdate(update.update)
	entry.sessionID = nextSessionID
	entry.codexAppServerPID = codexPIDForUpdate(entry.codexAppServerPID, update.update)
	entry.turnCount = turnCountForUpdate(entry.turnCount, prevSessionID, update.update)

	entry.codexInputTokens += delta.inputTokens
	entry.codexOutputTokens += delta.outputTokens
	entry.codexTotalTokens += delta.totalTokens
	entry.lastReportedInput = max(entry.lastReportedInput, delta.inputReported)
	entry.lastReportedOutput = max(entry.lastReportedOutput, delta.outputReported)
	entry.lastReportedTotal = max(entry.lastReportedTotal, delta.totalReported)

	state.codexTotals.InputTokens = max(0, state.codexTotals.InputTokens+delta.inputTokens)
	state.codexTotals.OutputTokens = max(0, state.codexTotals.OutputTokens+delta.outputTokens)
	state.codexTotals.TotalTokens = max(0, state.codexTotals.TotalTokens+delta.totalTokens)

	if rateLimits := extractRateLimits(update.update); rateLimits != nil {
		state.codexRateLimits = rateLimits
	}
}

func (o *Orchestrator) handleRetryIssue(ctx context.Context, state *runtimeState, issueID string) {
	retry, ok := state.retryAttempts[issueID]
	if !ok {
		return
	}
	delete(state.retryAttempts, issueID)

	issues, err := o.tracker.FetchCandidateIssues(ctx)
	if err != nil {
		o.logger.Warn().
			Str("issue_id", issueID).
			Str("issue_identifier", fallbackIdentifier(issueID, retry.identifier)).
			Err(err).
			Msg("retry poll failed")
		o.scheduleIssueRetry(state, issueID, intPtr(retry.attempt+1), retryMetadata{
			identifier: fallbackIdentifier(issueID, retry.identifier),
			err:        "retry poll failed: " + err.Error(),
			delayType:  retryDelayFailure,
		})
		return
	}

	issue, found := findIssueByID(issues, issueID)
	if !found {
		o.releaseIssueClaim(state, issueID)
		return
	}

	terminalStates := terminalStateSet(o.config)
	if terminalIssueState(issue.State, terminalStates) {
		o.cleanupIssueWorkspace(ctx, issue.Identifier)
		o.releaseIssueClaim(state, issueID)
		return
	}

	activeStates := activeStateSet(o.config)
	if !o.retryCandidateIssue(issue, activeStates, terminalStates) {
		o.releaseIssueClaim(state, issueID)
		return
	}

	if o.dispatchSlotsAvailable(state, issue) {
		o.dispatchIssue(ctx, state, issue, intPtr(retry.attempt))
		return
	}

	o.scheduleIssueRetry(state, issueID, intPtr(retry.attempt+1), retryMetadata{
		identifier: issue.Identifier,
		err:        "no available orchestrator slots",
		delayType:  retryDelayFailure,
	})
}

func (o *Orchestrator) dispatchIssue(ctx context.Context, state *runtimeState, issue tracker.Issue, attempt *int) {
	revalidatedIssue, ok := o.revalidateIssueForDispatch(ctx, issue)
	if !ok {
		return
	}

	issue = revalidatedIssue

	runCtx, cancel := context.WithCancel(ctx)
	entry := &runningEntry{
		issue:        issue,
		identifier:   issue.Identifier,
		startedAt:    time.Now().UTC(),
		runtimeKind:  o.config.AgentRuntimeKind(),
		retryAttempt: normalizeRetryAttempt(attempt),
		cancel:       cancel,
	}

	state.running[issue.ID] = entry
	state.claimed[issue.ID] = struct{}{}

	if existingRetry, ok := state.retryAttempts[issue.ID]; ok {
		if existingRetry.timer != nil {
			existingRetry.timer.Stop()
		}
		delete(state.retryAttempts, issue.ID)
	}

	o.logger.Info().
		Str("issue_id", issue.ID).
		Str("issue_identifier", issue.Identifier).
		Any("attempt", attemptValue(attempt)).
		Msg("dispatching issue to agent")

	go o.runIssueWorker(runCtx, issue, attempt)
}

func (o *Orchestrator) runIssueWorker(ctx context.Context, issue tracker.Issue, attempt *int) {
	err := o.runner.Run(ctx, issue, agent.RunOptions{
		Attempt: attempt,
		OnRuntimeUpdate: func(issueID string, update runtime.Update) {
			select {
			case o.workerUpdateCh <- workerUpdate{issueID: issueID, update: update}:
			case <-o.stopCh:
			}
		},
	})

	select {
	case o.workerDoneCh <- workerDone{issueID: issue.ID, err: err}:
	case <-o.stopCh:
	}
}

func (o *Orchestrator) revalidateIssueForDispatch(ctx context.Context, issue tracker.Issue) (tracker.Issue, bool) {
	if strings.TrimSpace(issue.ID) == "" {
		return issue, true
	}

	issues, err := o.tracker.FetchIssueStatesByIDs(ctx, []string{issue.ID})
	if err != nil {
		o.logger.Warn().Err(err).Str("issue_id", issue.ID).Str("issue_identifier", issue.Identifier).Msg("skipping dispatch; issue refresh failed")
		return issue, false
	}
	if len(issues) == 0 {
		o.logger.Info().Str("issue_id", issue.ID).Str("issue_identifier", issue.Identifier).Msg("skipping dispatch; issue no longer active or visible")
		return issue, false
	}

	refreshed := issues[0]
	activeStates := activeStateSet(o.config)
	terminalStates := terminalStateSet(o.config)
	if !o.retryCandidateIssue(refreshed, activeStates, terminalStates) {
		o.logger.Info().
			Str("issue_id", refreshed.ID).
			Str("issue_identifier", refreshed.Identifier).
			Str("state", refreshed.State).
			Int("blocked_by", len(refreshed.BlockedBy)).
			Msg("skipping stale dispatch after issue refresh")
		return refreshed, false
	}

	return refreshed, true
}

func (o *Orchestrator) shouldDispatchIssue(issue tracker.Issue, state *runtimeState, activeStates map[string]struct{}, terminalStates map[string]struct{}) bool {
	if !o.candidateIssue(issue, activeStates, terminalStates) {
		return false
	}
	if todoIssueBlockedByNonTerminal(issue, terminalStates) {
		return false
	}
	if _, ok := state.claimed[issue.ID]; ok {
		return false
	}
	if _, ok := state.running[issue.ID]; ok {
		return false
	}
	if o.availableSlots(state) <= 0 {
		return false
	}
	if !o.stateSlotsAvailable(issue, state.running) {
		return false
	}
	return true
}

func (o *Orchestrator) retryCandidateIssue(issue tracker.Issue, activeStates map[string]struct{}, terminalStates map[string]struct{}) bool {
	if !o.candidateIssue(issue, activeStates, terminalStates) {
		return false
	}
	return !todoIssueBlockedByNonTerminal(issue, terminalStates)
}

func (o *Orchestrator) candidateIssue(issue tracker.Issue, activeStates map[string]struct{}, terminalStates map[string]struct{}) bool {
	if strings.TrimSpace(issue.ID) == "" || strings.TrimSpace(issue.Identifier) == "" || strings.TrimSpace(issue.Title) == "" || strings.TrimSpace(issue.State) == "" {
		return false
	}
	if !o.issueRoutableToWorker(issue) {
		return false
	}
	if !activeIssueState(issue.State, activeStates) {
		return false
	}
	if terminalIssueState(issue.State, terminalStates) {
		return false
	}
	return true
}

func (o *Orchestrator) issueRoutableToWorker(issue tracker.Issue) bool {
	if issue.AssignedToWorker {
		return true
	}
	return strings.TrimSpace(o.config.LinearAssignee()) == ""
}

func (o *Orchestrator) stateSlotsAvailable(issue tracker.Issue, running map[string]*runningEntry) bool {
	limit := o.config.MaxConcurrentAgentsForState(issue.State)
	used := runningIssueCountForState(running, issue.State)
	return limit > used
}

func (o *Orchestrator) dispatchSlotsAvailable(state *runtimeState, issue tracker.Issue) bool {
	return o.availableSlots(state) > 0 && o.stateSlotsAvailable(issue, state.running)
}

func (o *Orchestrator) availableSlots(state *runtimeState) int {
	return max(state.maxConcurrentAgents-len(state.running), 0)
}

func (o *Orchestrator) sortIssuesForDispatch(issues []tracker.Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		left := issues[i]
		right := issues[j]

		leftPriority := priorityRank(left.Priority)
		rightPriority := priorityRank(right.Priority)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}

		leftCreated := issueCreatedAtSortKey(left.CreatedAt)
		rightCreated := issueCreatedAtSortKey(right.CreatedAt)
		if leftCreated != rightCreated {
			return leftCreated < rightCreated
		}

		leftKey := strings.TrimSpace(left.Identifier)
		if leftKey == "" {
			leftKey = strings.TrimSpace(left.ID)
		}
		rightKey := strings.TrimSpace(right.Identifier)
		if rightKey == "" {
			rightKey = strings.TrimSpace(right.ID)
		}
		return leftKey < rightKey
	})
}

func (o *Orchestrator) terminateRunningIssue(ctx context.Context, state *runtimeState, issueID string, cleanupWorkspace bool) {
	entry, ok := state.running[issueID]
	if !ok {
		o.releaseIssueClaim(state, issueID)
		return
	}

	o.recordSessionCompletionTotals(state, entry)

	if cleanupWorkspace {
		o.cleanupIssueWorkspace(ctx, entry.identifier)
	}

	if entry.cancel != nil {
		entry.cancel()
	}

	delete(state.running, issueID)
	delete(state.claimed, issueID)
	if retry, ok := state.retryAttempts[issueID]; ok {
		if retry.timer != nil {
			retry.timer.Stop()
		}
		delete(state.retryAttempts, issueID)
	}
}

func (o *Orchestrator) cleanupIssueWorkspace(ctx context.Context, identifier string) {
	if strings.TrimSpace(identifier) == "" {
		return
	}
	if err := o.workspace.RemoveIssueWorkspaces(ctx, identifier); err != nil {
		o.logger.Warn().Err(err).Str("issue_identifier", identifier).Msg("workspace cleanup failed")
	}
}

func (o *Orchestrator) scheduleIssueRetry(state *runtimeState, issueID string, attempt *int, metadata retryMetadata) {
	prev := state.retryAttempts[issueID]
	nextAttempt := 1
	if prev != nil && prev.attempt > 0 {
		nextAttempt = prev.attempt + 1
	}
	if attempt != nil && *attempt > 0 {
		nextAttempt = *attempt
	}

	identifier := strings.TrimSpace(metadata.identifier)
	if identifier == "" {
		if prev != nil {
			identifier = strings.TrimSpace(prev.identifier)
		}
	}
	if identifier == "" {
		identifier = issueID
	}

	errText := strings.TrimSpace(metadata.err)
	if errText == "" && prev != nil {
		errText = prev.err
	}

	if prev != nil && prev.timer != nil {
		prev.timer.Stop()
	}

	delay := retryDelay(nextAttempt, metadata.delayType, o.config.MaxRetryBackoffMS())
	dueAt := time.Now().Add(delay)
	timer := time.AfterFunc(delay, func() {
		select {
		case o.retryIssueCh <- issueID:
		case <-o.stopCh:
		}
	})

	state.retryAttempts[issueID] = &retryEntry{
		attempt:    nextAttempt,
		timer:      timer,
		dueAt:      dueAt,
		identifier: identifier,
		err:        errText,
	}

	log := o.logger.Warn().
		Str("issue_id", issueID).
		Str("issue_identifier", identifier).
		Int("attempt", nextAttempt).
		Dur("delay", delay)
	if errText != "" {
		log = log.Str("error", errText)
	}
	log.Msg("retry scheduled")
}

func (o *Orchestrator) releaseIssueClaim(state *runtimeState, issueID string) {
	delete(state.claimed, issueID)
	if retry, ok := state.retryAttempts[issueID]; ok {
		if retry.timer != nil {
			retry.timer.Stop()
		}
		delete(state.retryAttempts, issueID)
	}
}

func retryDelay(attempt int, delayType retryDelayType, maxBackoffMS int) time.Duration {
	if delayType == retryDelayContinuation && attempt == 1 {
		return continuationRetryDelay
	}

	if attempt <= 0 {
		attempt = 1
	}
	if maxBackoffMS <= 0 {
		maxBackoffMS = 300_000
	}

	power := attempt - 1
	if power > 10 {
		power = 10
	}

	delay := failureRetryBaseDelay * time.Duration(1<<power)
	maxDelay := time.Duration(maxBackoffMS) * time.Millisecond
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

func (o *Orchestrator) recordSessionCompletionTotals(state *runtimeState, entry *runningEntry) {
	if entry == nil {
		return
	}
	seconds := int(time.Since(entry.startedAt).Seconds())
	if seconds < 0 {
		seconds = 0
	}
	state.codexTotals.SecondsRunning = max(0, state.codexTotals.SecondsRunning+seconds)
}

func liveRuntimeSeconds(state *runtimeState, now time.Time) int {
	total := max(0, state.codexTotals.SecondsRunning)
	for _, entry := range state.running {
		seconds := int(now.Sub(entry.startedAt).Seconds())
		if seconds < 0 {
			seconds = 0
		}
		total += seconds
	}
	if total < 0 {
		return 0
	}
	return total
}

func (o *Orchestrator) runTerminalWorkspaceCleanup(ctx context.Context) {
	issues, err := o.tracker.FetchIssuesByStates(ctx, o.config.LinearTerminalStates())
	if err != nil {
		o.logger.Warn().Err(err).Msg("skipping startup terminal workspace cleanup; failed to fetch terminal issues")
		return
	}
	for _, issue := range issues {
		o.cleanupIssueWorkspace(ctx, issue.Identifier)
	}
}

func (o *Orchestrator) refreshRuntimeConfig(state *runtimeState) {
	if err := o.config.ReloadFromDisk(); err != nil {
		o.logger.Error().Err(err).Str("workflow_path", o.config.WorkflowFilePath()).Msg("failed to reload workflow; keeping last known good configuration")
	}
	state.pollInterval = pollDuration(o.config)
	state.maxConcurrentAgents = o.config.MaxConcurrentAgents()
}

func (o *Orchestrator) shutdown(state *runtimeState) {
	o.closeOnce.Do(func() {
		close(o.stopCh)
	})

	for _, entry := range state.running {
		if entry.cancel != nil {
			entry.cancel()
		}
	}

	for _, retry := range state.retryAttempts {
		if retry.timer != nil {
			retry.timer.Stop()
		}
	}
}

func (o *Orchestrator) logValidationError(err error) {
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		o.logger.Error().Err(err).Msg("dispatch validation failed")
		return
	}

	switch {
	case errors.Is(validationErr.Code, config.ErrMissingLinearAPIToken):
		o.logger.Error().Msg("Linear API token missing in WORKFLOW.md")
	case errors.Is(validationErr.Code, config.ErrMissingLinearProjectSlug):
		o.logger.Error().Msg("Linear project slug missing in WORKFLOW.md")
	case errors.Is(validationErr.Code, config.ErrMissingTrackerKind):
		o.logger.Error().Msg("Tracker kind missing in WORKFLOW.md")
	case errors.Is(validationErr.Code, config.ErrUnsupportedTrackerKind):
		o.logger.Error().Any("kind", validationErr.Value).Msg("unsupported tracker kind in WORKFLOW.md")
	case errors.Is(validationErr.Code, config.ErrUnsupportedAgentRuntime):
		o.logger.Error().Any("kind", validationErr.Value).Msg("unsupported agent_runtime.kind in WORKFLOW.md")
	case errors.Is(validationErr.Code, config.ErrMissingCodexCommand):
		o.logger.Error().Msg("Codex command missing in WORKFLOW.md")
	case errors.Is(validationErr.Code, config.ErrInvalidCodexApproval):
		o.logger.Error().Any("value", validationErr.Value).Msg("invalid codex.approval_policy in WORKFLOW.md")
	case errors.Is(validationErr.Code, config.ErrInvalidCodexThreadSandbox):
		o.logger.Error().Any("value", validationErr.Value).Msg("invalid codex.thread_sandbox in WORKFLOW.md")
	case errors.Is(validationErr.Code, config.ErrInvalidCodexTurnSandbox):
		o.logger.Error().Any("value", validationErr.Value).Msg("invalid codex.turn_sandbox_policy in WORKFLOW.md")
	default:
		o.logger.Error().Err(err).Msg("dispatch validation failed")
	}
}

func activeStateSet(cfg *config.Config) map[string]struct{} {
	set := map[string]struct{}{}
	for _, state := range cfg.LinearActiveStates() {
		normalized := normalizeIssueState(state)
		if normalized != "" {
			set[normalized] = struct{}{}
		}
	}
	return set
}

func terminalStateSet(cfg *config.Config) map[string]struct{} {
	set := map[string]struct{}{}
	for _, state := range cfg.LinearTerminalStates() {
		normalized := normalizeIssueState(state)
		if normalized != "" {
			set[normalized] = struct{}{}
		}
	}
	return set
}

func activeIssueState(stateName string, activeStates map[string]struct{}) bool {
	_, ok := activeStates[normalizeIssueState(stateName)]
	return ok
}

func terminalIssueState(stateName string, terminalStates map[string]struct{}) bool {
	_, ok := terminalStates[normalizeIssueState(stateName)]
	return ok
}

func normalizeIssueState(stateName string) string {
	return strings.ToLower(strings.TrimSpace(stateName))
}

func todoIssueBlockedByNonTerminal(issue tracker.Issue, terminalStates map[string]struct{}) bool {
	if normalizeIssueState(issue.State) != "todo" {
		return false
	}
	for _, blocker := range issue.BlockedBy {
		if !terminalIssueState(blocker.State, terminalStates) {
			return true
		}
	}
	return false
}

func runningIssueCountForState(running map[string]*runningEntry, issueState string) int {
	normalized := normalizeIssueState(issueState)
	count := 0
	for _, entry := range running {
		if normalizeIssueState(entry.issue.State) == normalized {
			count++
		}
	}
	return count
}

func priorityRank(priority *int) int {
	if priority != nil && *priority >= 1 && *priority <= 4 {
		return *priority
	}
	return 5
}

func issueCreatedAtSortKey(ts *time.Time) int64 {
	if ts == nil {
		return 1<<63 - 1
	}
	return ts.UTC().UnixMicro()
}

func findIssueByID(issues []tracker.Issue, issueID string) (tracker.Issue, bool) {
	for _, issue := range issues {
		if issue.ID == issueID {
			return issue, true
		}
	}
	return tracker.Issue{}, false
}

func normalizeRetryAttempt(attempt *int) int {
	if attempt == nil {
		return 0
	}
	if *attempt > 0 {
		return *attempt
	}
	return 0
}

func nextRetryAttemptFromRunning(entry *runningEntry) *int {
	if entry == nil || entry.retryAttempt <= 0 {
		return nil
	}
	next := entry.retryAttempt + 1
	return &next
}

func safeSessionID(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return "n/a"
	}
	return sessionID
}

func pollDuration(cfg *config.Config) time.Duration {
	intervalMS := cfg.PollIntervalMS()
	if intervalMS <= 0 {
		intervalMS = 30_000
	}
	return time.Duration(intervalMS) * time.Millisecond
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

func intPtr(v int) *int {
	return &v
}

func attemptValue(attempt *int) any {
	if attempt == nil {
		return nil
	}
	return *attempt
}

func fallbackIdentifier(issueID string, identifier string) string {
	if strings.TrimSpace(identifier) != "" {
		return identifier
	}
	return issueID
}

func summarizeUpdate(update runtime.Update) map[string]any {
	return map[string]any{
		"event":     update.Event,
		"message":   firstNonNil(update.Payload, update.Raw),
		"timestamp": update.Timestamp,
	}
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			if text, ok := value.(string); ok {
				if strings.TrimSpace(text) == "" {
					continue
				}
			}
			return value
		}
	}
	return nil
}

func sessionIDForUpdate(existing string, update runtime.Update) string {
	if payload, ok := update.Payload.(map[string]any); ok {
		if sessionID := stringValue(payload["session_id"]); strings.TrimSpace(sessionID) != "" {
			return sessionID
		}
	}
	if strings.TrimSpace(existing) != "" {
		return existing
	}
	return ""
}

func codexPIDForUpdate(existing string, update runtime.Update) string {
	pid := strings.TrimSpace(update.AppServerPID)
	if pid == "" {
		pid = strings.TrimSpace(update.CodexAppServerPID)
	}
	if pid != "" {
		return pid
	}
	return existing
}

func turnCountForUpdate(existing int, currentSessionID string, update runtime.Update) int {
	if update.Event != "session_started" {
		if existing >= 0 {
			return existing
		}
		return 0
	}

	nextSessionID := sessionIDForUpdate(currentSessionID, update)
	if strings.TrimSpace(nextSessionID) == "" || nextSessionID == currentSessionID {
		if existing >= 0 {
			return existing
		}
		return 0
	}
	if existing < 0 {
		return 1
	}
	return existing + 1
}

func extractTokenDelta(entry *runningEntry, update runtime.Update) tokenDelta {
	usage := extractTokenUsage(update)

	input := computeTokenDelta(entry.lastReportedInput, usage, tokenFieldInput)
	output := computeTokenDelta(entry.lastReportedOutput, usage, tokenFieldOutput)
	total := computeTokenDelta(entry.lastReportedTotal, usage, tokenFieldTotal)

	return tokenDelta{
		inputTokens:    input.delta,
		outputTokens:   output.delta,
		totalTokens:    total.delta,
		inputReported:  input.reported,
		outputReported: output.reported,
		totalReported:  total.reported,
	}
}

type tokenField int

const (
	tokenFieldInput tokenField = iota
	tokenFieldOutput
	tokenFieldTotal
)

type computedDelta struct {
	delta    int
	reported int
}

func computeTokenDelta(prevReported int, usage map[string]any, field tokenField) computedDelta {
	nextTotal := getTokenUsage(usage, field)
	if nextTotal < 0 {
		return computedDelta{delta: 0, reported: prevReported}
	}
	if nextTotal < prevReported {
		return computedDelta{delta: 0, reported: prevReported}
	}
	return computedDelta{delta: nextTotal - prevReported, reported: nextTotal}
}

func extractTokenUsage(update runtime.Update) map[string]any {
	payloads := []any{update.Payload}
	for _, payload := range payloads {
		if usage := absoluteTokenUsageFromPayload(payload); usage != nil {
			return usage
		}
	}
	for _, payload := range payloads {
		if usage := turnCompletedUsageFromPayload(payload); usage != nil {
			return usage
		}
	}
	return map[string]any{}
}

func absoluteTokenUsageFromPayload(payload any) map[string]any {
	data, ok := payload.(map[string]any)
	if !ok {
		return nil
	}

	paths := [][]any{
		{"params", "msg", "payload", "info", "total_token_usage"},
		{"params", "msg", "info", "total_token_usage"},
		{"params", "tokenUsage", "total"},
		{"tokenUsage", "total"},
	}

	for _, path := range paths {
		if value, ok := mapAtPath(data, path).(map[string]any); ok && integerTokenMap(value) {
			return value
		}
	}
	return nil
}

func turnCompletedUsageFromPayload(payload any) map[string]any {
	data, ok := payload.(map[string]any)
	if !ok {
		return nil
	}

	method := stringValue(firstNonNil(data["method"]))
	if method != "turn/completed" && method != "turn_completed" {
		return nil
	}

	if direct, ok := data["usage"].(map[string]any); ok && integerTokenMap(direct) {
		return direct
	}
	if params, ok := data["params"].(map[string]any); ok {
		if usage, ok := params["usage"].(map[string]any); ok && integerTokenMap(usage) {
			return usage
		}
	}
	return nil
}

func extractRateLimits(update runtime.Update) map[string]any {
	return rateLimitsFromPayload(update.Payload)
}

func rateLimitsFromPayload(payload any) map[string]any {
	switch data := payload.(type) {
	case map[string]any:
		if direct, ok := data["rate_limits"].(map[string]any); ok && rateLimitsMap(direct) {
			return direct
		}
		if rateLimitsMap(data) {
			return data
		}
		for _, value := range data {
			if extracted := rateLimitsFromPayload(value); extracted != nil {
				return extracted
			}
		}
	case []any:
		for _, value := range data {
			if extracted := rateLimitsFromPayload(value); extracted != nil {
				return extracted
			}
		}
	}
	return nil
}

func rateLimitsMap(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	limitID := firstNonNil(payload["limit_id"], payload["limit_name"])
	if limitID == nil {
		return false
	}
	for _, key := range []string{"primary", "secondary", "credits"} {
		if _, ok := payload[key]; ok {
			return true
		}
	}
	return false
}

func integerTokenMap(payload map[string]any) bool {
	for _, field := range []string{
		"input_tokens", "output_tokens", "total_tokens",
		"prompt_tokens", "completion_tokens",
		"inputTokens", "outputTokens", "totalTokens",
		"promptTokens", "completionTokens",
	} {
		if _, ok := mapIntegerValue(payload, field); ok {
			return true
		}
	}
	return false
}

func getTokenUsage(usage map[string]any, field tokenField) int {
	if usage == nil {
		return -1
	}
	var names []string
	switch field {
	case tokenFieldInput:
		names = []string{"input_tokens", "prompt_tokens", "input", "promptTokens", "inputTokens"}
	case tokenFieldOutput:
		names = []string{"output_tokens", "completion_tokens", "output", "completion", "outputTokens", "completionTokens"}
	case tokenFieldTotal:
		names = []string{"total_tokens", "total", "totalTokens"}
	default:
		return -1
	}

	for _, name := range names {
		if value, ok := mapIntegerValue(usage, name); ok {
			return value
		}
	}
	return -1
}

func mapAtPath(payload map[string]any, path []any) any {
	var current any = payload
	for _, segment := range path {
		nextMap, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		key := fmt.Sprintf("%v", segment)
		value, ok := nextMap[key]
		if !ok {
			return nil
		}
		current = value
	}
	return current
}

func mapIntegerValue(payload map[string]any, field string) (int, bool) {
	value, ok := payload[field]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case int:
		if typed < 0 {
			return 0, false
		}
		return typed, true
	case int64:
		if typed < 0 {
			return 0, false
		}
		return int(typed), true
	case float64:
		if typed < 0 {
			return 0, false
		}
		return int(typed), true
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		n, err := strconv.Atoi(trimmed)
		if err != nil || n < 0 {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}
