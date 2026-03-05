package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"baton/internal/agent"
	"baton/internal/codex"
	"baton/internal/config"
	"baton/internal/tracker"
	"baton/internal/workflow"

	"github.com/rs/zerolog"
)

type trackerStub struct {
	fetchCandidateIssuesFn  func(context.Context) ([]tracker.Issue, error)
	fetchIssuesByStatesFn   func(context.Context, []string) ([]tracker.Issue, error)
	fetchIssueStatesByIDsFn func(context.Context, []string) ([]tracker.Issue, error)
}

func (s *trackerStub) FetchCandidateIssues(ctx context.Context) ([]tracker.Issue, error) {
	if s.fetchCandidateIssuesFn != nil {
		return s.fetchCandidateIssuesFn(ctx)
	}
	return []tracker.Issue{}, nil
}

func (s *trackerStub) FetchIssuesByStates(ctx context.Context, states []string) ([]tracker.Issue, error) {
	if s.fetchIssuesByStatesFn != nil {
		return s.fetchIssuesByStatesFn(ctx, states)
	}
	return []tracker.Issue{}, nil
}

func (s *trackerStub) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]tracker.Issue, error) {
	if s.fetchIssueStatesByIDsFn != nil {
		return s.fetchIssueStatesByIDsFn(ctx, ids)
	}
	return []tracker.Issue{}, nil
}

func (s *trackerStub) CreateComment(context.Context, string, string) error {
	return nil
}

func (s *trackerStub) UpdateIssueState(context.Context, string, string) error {
	return nil
}

type workspaceStub struct {
	removedIdentifiers []string
}

func (s *workspaceStub) CreateForIssue(context.Context, tracker.Issue) (string, error) {
	return "", errors.New("not implemented")
}

func (s *workspaceStub) Remove(context.Context, string) error {
	return nil
}

func (s *workspaceStub) RemoveIssueWorkspaces(_ context.Context, identifier string) error {
	s.removedIdentifiers = append(s.removedIdentifiers, identifier)
	return nil
}

func (s *workspaceStub) RunBeforeRunHook(context.Context, string, tracker.Issue) error {
	return nil
}

func (s *workspaceStub) RunAfterRunHook(context.Context, string, tracker.Issue) {}

type runnerStub struct{}

func (runnerStub) Run(context.Context, tracker.Issue, agent.RunOptions) error { return nil }

func TestSortIssuesForDispatchPriorityThenCreated(t *testing.T) {
	o, _ := newTestOrchestrator(t, nil)

	priority1 := 1
	priority2 := 2

	oldest := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	newer := oldest.Add(30 * time.Minute)
	newest := oldest.Add(2 * time.Hour)

	issues := []tracker.Issue{
		{ID: "c", Identifier: "MT-3", Title: "third", State: "Todo", Priority: &priority2, CreatedAt: &oldest, AssignedToWorker: true},
		{ID: "d", Identifier: "MT-4", Title: "fourth", State: "Todo", Priority: nil, CreatedAt: &oldest, AssignedToWorker: true},
		{ID: "b", Identifier: "MT-2", Title: "second", State: "Todo", Priority: &priority1, CreatedAt: &newer, AssignedToWorker: true},
		{ID: "a", Identifier: "MT-1", Title: "first", State: "Todo", Priority: &priority1, CreatedAt: &oldest, AssignedToWorker: true},
		{ID: "e", Identifier: "MT-5", Title: "fifth", State: "Todo", Priority: &priority2, CreatedAt: &newest, AssignedToWorker: true},
	}

	o.sortIssuesForDispatch(issues)

	got := []string{issues[0].ID, issues[1].ID, issues[2].ID, issues[3].ID, issues[4].ID}
	want := []string{"a", "b", "c", "e", "d"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected sort order at index %d: got %v want %v", i, got, want)
		}
	}
}

func TestShouldDispatchIssueTodoBlockedByNonTerminal(t *testing.T) {
	o, _ := newTestOrchestrator(t, nil)
	state := newRuntimeState(o.config)

	activeStates := activeStateSet(o.config)
	terminalStates := terminalStateSet(o.config)

	blocked := tracker.Issue{
		ID:               "issue-1",
		Identifier:       "MT-1",
		Title:            "blocked",
		State:            "Todo",
		AssignedToWorker: true,
		BlockedBy: []tracker.BlockerRef{
			{ID: "blk-1", Identifier: "MT-0", State: "In Progress"},
		},
	}
	if o.shouldDispatchIssue(blocked, state, activeStates, terminalStates) {
		t.Fatalf("todo issue with non-terminal blockers should not dispatch")
	}

	unblocked := blocked
	unblocked.BlockedBy = []tracker.BlockerRef{{ID: "blk-1", Identifier: "MT-0", State: "Done"}}
	if !o.shouldDispatchIssue(unblocked, state, activeStates, terminalStates) {
		t.Fatalf("todo issue with terminal blockers should dispatch")
	}
}

func TestReconcileRunningIssuesUpdatesActiveState(t *testing.T) {
	tr := &trackerStub{}
	o, _ := newTestOrchestrator(t, tr)

	issueID := "issue-3"
	state := newRuntimeState(o.config)
	state.claimed[issueID] = struct{}{}
	state.running[issueID] = &runningEntry{
		identifier: "MT-557",
		issue: tracker.Issue{
			ID:               issueID,
			Identifier:       "MT-557",
			Title:            "active",
			State:            "Todo",
			AssignedToWorker: true,
		},
		startedAt: time.Now().UTC(),
	}

	tr.fetchIssueStatesByIDsFn = func(_ context.Context, ids []string) ([]tracker.Issue, error) {
		if len(ids) != 1 || ids[0] != issueID {
			t.Fatalf("unexpected issue id refresh payload: %v", ids)
		}
		return []tracker.Issue{{
			ID:               issueID,
			Identifier:       "MT-557",
			Title:            "active",
			State:            "In Progress",
			AssignedToWorker: true,
		}}, nil
	}

	o.reconcileRunningIssues(context.Background(), state)

	entry, ok := state.running[issueID]
	if !ok {
		t.Fatalf("expected issue to keep running")
	}
	if entry.issue.State != "In Progress" {
		t.Fatalf("expected refreshed state In Progress, got %q", entry.issue.State)
	}
	if _, ok := state.claimed[issueID]; !ok {
		t.Fatalf("expected claim to remain")
	}
}

func TestReconcileRunningIssuesStopsNonActiveWithoutCleanup(t *testing.T) {
	tr := &trackerStub{}
	o, ws := newTestOrchestrator(t, tr)

	issueID := "issue-1"
	canceled := false
	state := newRuntimeState(o.config)
	state.claimed[issueID] = struct{}{}
	state.running[issueID] = &runningEntry{
		identifier: "MT-555",
		issue: tracker.Issue{
			ID:               issueID,
			Identifier:       "MT-555",
			Title:            "queued",
			State:            "Todo",
			AssignedToWorker: true,
		},
		startedAt: time.Now().UTC(),
		cancel: func() {
			canceled = true
		},
	}

	tr.fetchIssueStatesByIDsFn = func(_ context.Context, _ []string) ([]tracker.Issue, error) {
		return []tracker.Issue{{
			ID:               issueID,
			Identifier:       "MT-555",
			Title:            "queued",
			State:            "Backlog",
			AssignedToWorker: true,
		}}, nil
	}

	o.reconcileRunningIssues(context.Background(), state)

	if _, ok := state.running[issueID]; ok {
		t.Fatalf("expected running entry removed")
	}
	if _, ok := state.claimed[issueID]; ok {
		t.Fatalf("expected claim removed")
	}
	if !canceled {
		t.Fatalf("expected running worker cancellation")
	}
	if len(ws.removedIdentifiers) != 0 {
		t.Fatalf("expected workspace to stay for non-active reconcile, got cleanup calls: %v", ws.removedIdentifiers)
	}
}

func TestReconcileRunningIssuesStopsTerminalAndCleansWorkspace(t *testing.T) {
	tr := &trackerStub{}
	o, ws := newTestOrchestrator(t, tr)

	issueID := "issue-2"
	state := newRuntimeState(o.config)
	state.claimed[issueID] = struct{}{}
	state.running[issueID] = &runningEntry{
		identifier: "MT-556",
		issue: tracker.Issue{
			ID:               issueID,
			Identifier:       "MT-556",
			Title:            "done",
			State:            "In Progress",
			AssignedToWorker: true,
		},
		startedAt: time.Now().UTC(),
	}

	tr.fetchIssueStatesByIDsFn = func(_ context.Context, _ []string) ([]tracker.Issue, error) {
		return []tracker.Issue{{
			ID:               issueID,
			Identifier:       "MT-556",
			Title:            "done",
			State:            "Closed",
			AssignedToWorker: true,
		}}, nil
	}

	o.reconcileRunningIssues(context.Background(), state)

	if _, ok := state.running[issueID]; ok {
		t.Fatalf("expected running entry removed")
	}
	if _, ok := state.claimed[issueID]; ok {
		t.Fatalf("expected claim removed")
	}
	if len(ws.removedIdentifiers) != 1 || ws.removedIdentifiers[0] != "MT-556" {
		t.Fatalf("expected terminal cleanup for MT-556, got %v", ws.removedIdentifiers)
	}
}

func TestHandleWorkerDoneNormalSchedulesContinuationRetry(t *testing.T) {
	o, _ := newTestOrchestrator(t, nil)

	issueID := "issue-resume"
	state := newRuntimeState(o.config)
	state.claimed[issueID] = struct{}{}
	state.running[issueID] = &runningEntry{
		identifier: "MT-558",
		issue: tracker.Issue{
			ID:         issueID,
			Identifier: "MT-558",
			Title:      "resume",
			State:      "In Progress",
		},
		startedAt: time.Now().UTC(),
	}

	o.handleWorkerDone(state, workerDone{issueID: issueID, err: nil})

	if _, ok := state.running[issueID]; ok {
		t.Fatalf("expected running entry removed")
	}
	if _, ok := state.completed[issueID]; !ok {
		t.Fatalf("expected issue marked completed")
	}
	retry := state.retryAttempts[issueID]
	if retry == nil {
		t.Fatalf("expected retry scheduled")
	}
	defer retry.timer.Stop()

	if retry.attempt != 1 {
		t.Fatalf("expected continuation attempt=1, got %d", retry.attempt)
	}
	assertDueInRange(t, retry.dueAt, 500*time.Millisecond, 1100*time.Millisecond)
}

func TestHandleWorkerDoneAbnormalIncrementsRetryAttempt(t *testing.T) {
	o, _ := newTestOrchestrator(t, nil)

	issueID := "issue-crash"
	state := newRuntimeState(o.config)
	state.claimed[issueID] = struct{}{}
	state.running[issueID] = &runningEntry{
		identifier:   "MT-559",
		retryAttempt: 2,
		issue: tracker.Issue{
			ID:         issueID,
			Identifier: "MT-559",
			Title:      "crash",
			State:      "In Progress",
		},
		startedAt: time.Now().UTC(),
	}

	o.handleWorkerDone(state, workerDone{issueID: issueID, err: errors.New("boom")})

	retry := state.retryAttempts[issueID]
	if retry == nil {
		t.Fatalf("expected retry scheduled")
	}
	defer retry.timer.Stop()

	if retry.attempt != 3 {
		t.Fatalf("expected retry attempt=3, got %d", retry.attempt)
	}
	if !strings.Contains(retry.err, "agent exited: boom") {
		t.Fatalf("expected retry error to include agent exit reason, got %q", retry.err)
	}
	assertDueInRange(t, retry.dueAt, 39*time.Second, 41*time.Second)
}

func TestHandleWorkerDoneFirstAbnormalUsesAttemptOne(t *testing.T) {
	o, _ := newTestOrchestrator(t, nil)

	issueID := "issue-crash-initial"
	state := newRuntimeState(o.config)
	state.claimed[issueID] = struct{}{}
	state.running[issueID] = &runningEntry{
		identifier: "MT-560",
		issue: tracker.Issue{
			ID:         issueID,
			Identifier: "MT-560",
			Title:      "crash",
			State:      "In Progress",
		},
		startedAt: time.Now().UTC(),
	}

	o.handleWorkerDone(state, workerDone{issueID: issueID, err: errors.New("boom")})

	retry := state.retryAttempts[issueID]
	if retry == nil {
		t.Fatalf("expected retry scheduled")
	}
	defer retry.timer.Stop()

	if retry.attempt != 1 {
		t.Fatalf("expected retry attempt=1, got %d", retry.attempt)
	}
	assertDueInRange(t, retry.dueAt, 9*time.Second, 11*time.Second)
}

func TestSnapshotPayloadReflectsLatestCodexState(t *testing.T) {
	o, _ := newTestOrchestrator(t, nil)

	issueID := "issue-snapshot"
	startedAt := time.Now().UTC().Add(-5 * time.Second)
	state := newRuntimeState(o.config)
	state.claimed[issueID] = struct{}{}
	state.running[issueID] = &runningEntry{
		identifier: "MT-188",
		issue: tracker.Issue{
			ID:         issueID,
			Identifier: "MT-188",
			Title:      "Snapshot",
			State:      "In Progress",
		},
		startedAt: startedAt,
	}

	now := time.Now().UTC()
	o.handleWorkerUpdate(state, workerUpdate{
		issueID: issueID,
		update: codex.Update{
			Event:     "session_started",
			Timestamp: now,
			Payload: map[string]any{
				"session_id": "thread-live-turn-live",
			},
		},
	})
	o.handleWorkerUpdate(state, workerUpdate{
		issueID: issueID,
		update: codex.Update{
			Event:     "notification",
			Timestamp: now,
			Payload: map[string]any{
				"method": "some-event",
			},
		},
	})

	snapshot := o.snapshotPayload(state)
	running, ok := snapshot["running"].([]map[string]any)
	if !ok || len(running) != 1 {
		t.Fatalf("expected one running row, got %#v", snapshot["running"])
	}

	row := running[0]
	if row["issue_id"] != issueID {
		t.Fatalf("unexpected issue_id: %#v", row["issue_id"])
	}
	if row["session_id"] != "thread-live-turn-live" {
		t.Fatalf("unexpected session_id: %#v", row["session_id"])
	}
	if row["turn_count"] != 1 {
		t.Fatalf("unexpected turn_count: %#v", row["turn_count"])
	}
	if row["last_codex_timestamp"] != now {
		t.Fatalf("unexpected last_codex_timestamp: %#v", row["last_codex_timestamp"])
	}

	message, ok := row["last_codex_message"].(map[string]any)
	if !ok {
		t.Fatalf("expected last_codex_message map, got %#v", row["last_codex_message"])
	}
	if message["event"] != "notification" {
		t.Fatalf("unexpected message event: %#v", message["event"])
	}
	if runtimeSeconds, ok := row["runtime_seconds"].(int); !ok || runtimeSeconds < 0 {
		t.Fatalf("expected non-negative runtime_seconds, got %#v", row["runtime_seconds"])
	}
}

func TestSnapshotPayloadTracksTokenTotalsAndRateLimits(t *testing.T) {
	o, _ := newTestOrchestrator(t, nil)

	issueID := "issue-rate-limit"
	state := newRuntimeState(o.config)
	state.claimed[issueID] = struct{}{}
	state.running[issueID] = &runningEntry{
		identifier: "MT-201",
		issue: tracker.Issue{
			ID:         issueID,
			Identifier: "MT-201",
			Title:      "Usage",
			State:      "In Progress",
		},
		startedAt: time.Now().UTC(),
	}

	rateLimits := map[string]any{
		"limit_id":  "codex",
		"primary":   map[string]any{"remaining": 90, "limit": 100},
		"secondary": nil,
		"credits":   map[string]any{"has_credits": false},
	}

	o.handleWorkerUpdate(state, workerUpdate{
		issueID: issueID,
		update: codex.Update{
			Event:             "notification",
			Timestamp:         time.Now().UTC(),
			CodexAppServerPID: "4242",
			Payload: map[string]any{
				"method": "thread/tokenUsage/updated",
				"params": map[string]any{
					"tokenUsage": map[string]any{
						"total": map[string]any{
							"inputTokens":  12,
							"outputTokens": 4,
							"totalTokens":  16,
						},
					},
				},
				"rate_limits": rateLimits,
			},
		},
	})

	snapshot := o.snapshotPayload(state)
	running := snapshot["running"].([]map[string]any)
	row := running[0]

	if row["codex_app_server_pid"] != "4242" {
		t.Fatalf("unexpected app-server pid: %#v", row["codex_app_server_pid"])
	}
	if row["codex_input_tokens"] != 12 || row["codex_output_tokens"] != 4 || row["codex_total_tokens"] != 16 {
		t.Fatalf("unexpected running token totals: %#v", row)
	}

	totals, ok := snapshot["codex_totals"].(map[string]any)
	if !ok {
		t.Fatalf("expected codex_totals map, got %#v", snapshot["codex_totals"])
	}
	if totals["input_tokens"] != 12 || totals["output_tokens"] != 4 || totals["total_tokens"] != 16 {
		t.Fatalf("unexpected aggregate token totals: %#v", totals)
	}
	if !reflect.DeepEqual(snapshot["rate_limits"], rateLimits) {
		t.Fatalf("unexpected rate_limits payload: %#v", snapshot["rate_limits"])
	}
}

func TestSnapshotPayloadIncludesPollingStatus(t *testing.T) {
	o, _ := newTestOrchestrator(t, nil)
	state := newRuntimeState(o.config)
	state.pollInterval = 30 * time.Second
	state.pollCheckInProgress = false
	state.nextPollDueAt = time.Now().UTC().Add(4 * time.Second)

	snapshot := o.snapshotPayload(state)
	polling, ok := snapshot["polling"].(map[string]any)
	if !ok {
		t.Fatalf("expected polling map, got %#v", snapshot["polling"])
	}
	if polling["checking?"] != false {
		t.Fatalf("expected checking? false, got %#v", polling["checking?"])
	}

	dueIn, ok := polling["next_poll_in_ms"].(int64)
	if !ok {
		t.Fatalf("expected next_poll_in_ms int64, got %T (%#v)", polling["next_poll_in_ms"], polling["next_poll_in_ms"])
	}
	if dueIn < 0 || dueIn > 4_000 {
		t.Fatalf("unexpected next_poll_in_ms=%d", dueIn)
	}

	state.pollCheckInProgress = true
	state.nextPollDueAt = time.Time{}
	snapshot = o.snapshotPayload(state)
	polling = snapshot["polling"].(map[string]any)
	if polling["checking?"] != true {
		t.Fatalf("expected checking? true, got %#v", polling["checking?"])
	}
	if polling["next_poll_in_ms"] != nil {
		t.Fatalf("expected next_poll_in_ms=nil while checking, got %#v", polling["next_poll_in_ms"])
	}
}

func TestHandleRefreshRequestCoalescing(t *testing.T) {
	o, _ := newTestOrchestrator(t, nil)
	state := newRuntimeState(o.config)

	queued := 0
	state.pollCheckInProgress = false
	state.nextPollDueAt = time.Now().UTC().Add(30 * time.Second)

	response := o.handleRefreshRequest(state, func() { queued++ })
	if response["queued"] != true {
		t.Fatalf("expected queued=true, got %#v", response["queued"])
	}
	if response["coalesced"] != false {
		t.Fatalf("expected coalesced=false, got %#v", response["coalesced"])
	}
	if queued != 1 {
		t.Fatalf("expected queue callback once, got %d", queued)
	}
	ops, ok := response["operations"].([]string)
	if !ok || len(ops) != 2 || ops[0] != "poll" || ops[1] != "reconcile" {
		t.Fatalf("unexpected operations payload: %#v", response["operations"])
	}

	response = o.handleRefreshRequest(state, func() { queued++ })
	if response["coalesced"] != true {
		t.Fatalf("expected coalesced=true for already-due refresh, got %#v", response["coalesced"])
	}
	if queued != 1 {
		t.Fatalf("expected no extra queueing for coalesced refresh, got %d", queued)
	}

	state.pollCheckInProgress = true
	state.nextPollDueAt = time.Now().UTC().Add(5 * time.Second)
	response = o.handleRefreshRequest(state, func() { queued++ })
	if response["coalesced"] != true {
		t.Fatalf("expected coalesced=true while polling, got %#v", response["coalesced"])
	}
	if queued != 1 {
		t.Fatalf("expected no queueing while polling, got %d", queued)
	}
}

func TestSnapshotAndRefreshUnavailableWhenNotRunning(t *testing.T) {
	o, _ := newTestOrchestrator(t, nil)
	if _, err := o.Snapshot(10 * time.Millisecond); !errors.Is(err, ErrOrchestratorUnavailable) {
		t.Fatalf("expected snapshot unavailable error, got %v", err)
	}
	if _, err := o.RequestRefresh(); !errors.Is(err, ErrOrchestratorUnavailable) {
		t.Fatalf("expected refresh unavailable error, got %v", err)
	}
}

func TestSnapshotAndRefreshAvailableWhenRunning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	workflowBody := "---\ntracker:\n  kind: memory\npolling:\n  interval_ms: 30000\n---\nprompt\n"
	if err := os.WriteFile(workflowPath, []byte(workflowBody), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	definition, err := workflow.LoadFile(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	cfg, err := config.FromWorkflow(workflowPath, definition)
	if err != nil {
		t.Fatalf("config.FromWorkflow failed: %v", err)
	}

	o := New(cfg, &trackerStub{}, &workspaceStub{}, runnerStub{}, zerolog.New(testWriter{t: t}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- o.Run(ctx)
	}()
	defer func() {
		cancel()
		select {
		case err := <-doneCh:
			if err != nil {
				t.Fatalf("orchestrator returned error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for orchestrator shutdown")
		}
	}()

	var refresh map[string]any
	for range 20 {
		refresh, err = o.RequestRefresh()
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("request refresh failed: %v", err)
	}
	if refresh["queued"] != true {
		t.Fatalf("expected queued=true, got %#v", refresh["queued"])
	}

	snapshot, err := o.Snapshot(2 * time.Second)
	if err != nil {
		t.Fatalf("snapshot failed: %v", err)
	}
	if _, ok := snapshot["running"]; !ok {
		t.Fatalf("snapshot missing running list: %#v", snapshot)
	}
	if _, ok := snapshot["polling"]; !ok {
		t.Fatalf("snapshot missing polling metadata: %#v", snapshot)
	}
}

func newTestOrchestrator(t *testing.T, tr *trackerStub) (*Orchestrator, *workspaceStub) {
	t.Helper()

	if tr == nil {
		tr = &trackerStub{}
	}
	ws := &workspaceStub{}
	cfg := testConfig(t, nil)

	logger := zerolog.New(testWriter{t: t})
	return New(cfg, tr, ws, runnerStub{}, logger), ws
}

func testConfig(t *testing.T, extra map[string]any) *config.Config {
	t.Helper()

	raw := map[string]any{
		"tracker": map[string]any{
			"kind":            "memory",
			"active_states":   []string{"Todo", "In Progress", "In Review"},
			"terminal_states": []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"},
		},
		"polling": map[string]any{
			"interval_ms": 30_000,
		},
		"agent": map[string]any{
			"max_concurrent_agents": 10,
			"max_retry_backoff_ms":  300_000,
		},
		"codex": map[string]any{
			"command":          "codex app-server",
			"stall_timeout_ms": 300_000,
		},
	}

	for key, value := range extra {
		raw[key] = value
	}

	cfg, err := config.FromWorkflow("WORKFLOW.md", &workflow.Definition{
		Config:         raw,
		PromptTemplate: "prompt",
	})
	if err != nil {
		t.Fatalf("config.FromWorkflow failed: %v", err)
	}
	return cfg
}

func assertDueInRange(t *testing.T, dueAt time.Time, minRemaining, maxRemaining time.Duration) {
	t.Helper()
	remaining := time.Until(dueAt)
	if remaining < minRemaining || remaining > maxRemaining {
		t.Fatalf("retry due time outside expected range: got %s want [%s, %s]", remaining, minRemaining, maxRemaining)
	}
}

type testWriter struct {
	t *testing.T
}

func (w testWriter) Write(p []byte) (n int, err error) {
	if w.t != nil {
		w.t.Logf("%s", strings.TrimSpace(string(p)))
	}
	return len(p), nil
}
