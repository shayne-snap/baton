package statusdashboard

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"baton/internal/config"
	"baton/internal/workflow"
)

func TestSnapshotFixtures(t *testing.T) {
	baseOpts := RenderOptions{
		MaxConcurrentAgents: 10,
		LinearProjectSlug:   "project",
		ServerHost:          "127.0.0.1",
		TerminalColumns:     115,
	}

	tests := []struct {
		name     string
		fixture  string
		snapshot map[string]any
		opts     RenderOptions
		tps      float64
	}{
		{
			name:    "idle",
			fixture: "idle.snapshot.txt",
			snapshot: map[string]any{
				"running":      []any{},
				"retrying":     []any{},
				"codex_totals": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0, "seconds_running": 0},
				"rate_limits":  nil,
			},
			opts: baseOpts,
			tps:  0,
		},
		{
			name:    "idle_with_dashboard_url",
			fixture: "idle_with_dashboard_url.snapshot.txt",
			snapshot: map[string]any{
				"running":      []any{},
				"retrying":     []any{},
				"codex_totals": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0, "seconds_running": 0},
				"rate_limits":  nil,
			},
			opts: withConfiguredPort(baseOpts, 4000),
			tps:  0,
		},
		{
			name:    "super_busy",
			fixture: "super_busy.snapshot.txt",
			snapshot: map[string]any{
				"running": []any{
					runningEntry(map[string]any{
						"identifier":         "MT-101",
						"codex_total_tokens": 120_450,
						"runtime_seconds":    785,
						"turn_count":         11,
						"last_codex_event":   "turn_completed",
						"last_codex_message": turnCompletedMessage("completed"),
					}),
					runningEntry(map[string]any{
						"identifier":           "MT-102",
						"session_id":           "thread-abcdef1234567890",
						"codex_app_server_pid": "5252",
						"codex_total_tokens":   89_200,
						"runtime_seconds":      412,
						"turn_count":           4,
						"last_codex_event":     "codex/event/task_started",
						"last_codex_message":   execCommandMessage("mix test --cover"),
					}),
				},
				"retrying": []any{},
				"codex_totals": map[string]any{
					"input_tokens":    250_000,
					"output_tokens":   18_500,
					"total_tokens":    268_500,
					"seconds_running": 4_321,
				},
				"rate_limits": map[string]any{
					"limit_id": "gpt-5",
					"primary":  map[string]any{"remaining": 12_345, "limit": 20_000, "reset_in_seconds": 30},
					"secondary": map[string]any{
						"remaining": 45, "limit": 60, "reset_in_seconds": 12,
					},
					"credits": map[string]any{"has_credits": true, "balance": 9_876.5},
				},
			},
			opts: baseOpts,
			tps:  1_842.7,
		},
		{
			name:    "backoff_queue",
			fixture: "backoff_queue.snapshot.txt",
			snapshot: map[string]any{
				"running": []any{
					runningEntry(map[string]any{
						"identifier":         "MT-638",
						"state":              "retrying",
						"codex_total_tokens": 14_200,
						"runtime_seconds":    1_225,
						"turn_count":         7,
						"last_codex_event":   "notification",
						"last_codex_message": agentMessageDelta("waiting on rate-limit backoff window"),
					}),
				},
				"retrying": []any{
					retryEntry(map[string]any{"identifier": "MT-450", "attempt": 4, "due_in_ms": 1_250, "error": "rate limit exhausted"}),
					retryEntry(map[string]any{"identifier": "MT-451", "attempt": 2, "due_in_ms": 3_900, "error": "retrying after API timeout with jitter"}),
					retryEntry(map[string]any{"identifier": "MT-452", "attempt": 6, "due_in_ms": 8_100, "error": "worker crashed\nrestarting cleanly"}),
					retryEntry(map[string]any{"identifier": "MT-453", "attempt": 1, "due_in_ms": 11_000, "error": "fourth queued retry should also render after removing the top-three limit"}),
				},
				"codex_totals": map[string]any{"input_tokens": 18_000, "output_tokens": 2_200, "total_tokens": 20_200, "seconds_running": 2_700},
				"rate_limits": map[string]any{
					"limit_id": "gpt-5",
					"primary":  map[string]any{"remaining": 0, "limit": 20_000, "reset_in_seconds": 95},
					"secondary": map[string]any{
						"remaining": 0, "limit": 60, "reset_in_seconds": 45,
					},
					"credits": map[string]any{"has_credits": false},
				},
			},
			opts: baseOpts,
			tps:  15.4,
		},
		{
			name:    "credits_unlimited",
			fixture: "credits_unlimited.snapshot.txt",
			snapshot: map[string]any{
				"running": []any{
					runningEntry(map[string]any{
						"identifier":         "MT-777",
						"state":              "running",
						"codex_total_tokens": 3_200,
						"runtime_seconds":    75,
						"turn_count":         7,
						"last_codex_event":   "codex/event/token_count",
						"last_codex_message": tokenUsageMessage(90, 12, 102),
					}),
				},
				"retrying":     []any{},
				"codex_totals": map[string]any{"input_tokens": 90, "output_tokens": 12, "total_tokens": 102, "seconds_running": 75},
				"rate_limits": map[string]any{
					"limit_id":  "priority-tier",
					"primary":   map[string]any{"remaining": 100, "limit": 100, "reset_in_seconds": 1},
					"secondary": map[string]any{"remaining": 500, "limit": 500, "reset_in_seconds": 1},
					"credits":   map[string]any{"unlimited": true},
				},
			},
			opts: baseOpts,
			tps:  42,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rendered := FormatSnapshotContentForTest(tc.snapshot, tc.tps, tc.opts)
			actual := strings.TrimSpace(strings.ReplaceAll(rendered, "\x1b", "\\e"))
			expected := strings.TrimSpace(loadSnapshotFixture(t, tc.fixture))
			if actual != expected {
				t.Fatalf("snapshot mismatch\n--- actual ---\n%s\n--- expected ---\n%s", actual, expected)
			}
		})
	}
}

func TestRetryRowEscapesEscapedNewlineSequences(t *testing.T) {
	snapshot := map[string]any{
		"running": []any{},
		"retrying": []any{
			retryEntry(map[string]any{
				"identifier": "MT-980",
				"attempt":    1,
				"due_in_ms":  1500,
				"error":      "error with \\nnewline",
			}),
		},
		"codex_totals": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0, "seconds_running": 0},
	}
	rendered := FormatSnapshotContentForTest(snapshot, 0, RenderOptions{MaxConcurrentAgents: 10, LinearProjectSlug: "project"})
	plain := stripANSI(rendered)
	if !strings.Contains(plain, "error=error with newline") {
		t.Fatalf("expected sanitized retry row, got: %s", plain)
	}
	if strings.Contains(plain, "\\n") {
		t.Fatalf("expected escaped newline removed, got: %s", plain)
	}
}

func TestDashboardURLHostNormalization(t *testing.T) {
	if got := DashboardURLForTest("0.0.0.0", intPtr(0), intPtr(43123)); got != "http://127.0.0.1:43123/" {
		t.Fatalf("unexpected dashboard url: %s", got)
	}
	if got := DashboardURLForTest("::1", intPtr(4000), nil); got != "http://[::1]:4000/" {
		t.Fatalf("unexpected dashboard url: %s", got)
	}
}

func TestProjectLinkUsesTrackerKind(t *testing.T) {
	emptySnapshot := map[string]any{
		"running":      []any{},
		"retrying":     []any{},
		"codex_totals": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0, "seconds_running": 0},
	}

	jiraRendered := FormatSnapshotContentForTest(emptySnapshot, 0, RenderOptions{
		TrackerKind:    "jira",
		JiraBaseURL:    "https://example.atlassian.net",
		JiraProjectKey: "KAN",
	})
	jiraPlain := stripANSI(jiraRendered)
	if !strings.Contains(jiraPlain, "https://example.atlassian.net/jira/software/projects/KAN/issues") {
		t.Fatalf("expected jira project url, got: %s", jiraPlain)
	}

	linearRendered := FormatSnapshotContentForTest(emptySnapshot, 0, RenderOptions{
		TrackerKind:       "linear",
		LinearProjectSlug: "baton",
	})
	linearPlain := stripANSI(linearRendered)
	if !strings.Contains(linearPlain, "https://linear.app/project/baton/issues") {
		t.Fatalf("expected linear project url, got: %s", linearPlain)
	}
}

func TestRenderOfflineStatus(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderOfflineStatus(&buf); err != nil {
		t.Fatalf("render offline status failed: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "app_status=offline") {
		t.Fatalf("expected offline marker, got: %q", output)
	}
}

func TestRollingAndThrottledTPS(t *testing.T) {
	if got := RollingTPS(nil, 10_000, 0); got != 0 {
		t.Fatalf("unexpected tps: %v", got)
	}
	if got := RollingTPS([]TokenSample{{TimestampMS: 9_000, TotalTokens: 20}}, 10_000, 40); got != 20 {
		t.Fatalf("unexpected tps: %v", got)
	}
	if got := RollingTPS([]TokenSample{{TimestampMS: 4_900, TotalTokens: 10}}, 10_000, 90); got != 0 {
		t.Fatalf("unexpected tps with stale sample: %v", got)
	}

	second, value := ThrottledTPS(nil, nil, 10_000, []TokenSample{{TimestampMS: 9_000, TotalTokens: 20}}, 40)
	nextSecond, nextValue := ThrottledTPS(&second, &value, 10_500, []TokenSample{{TimestampMS: 9_000, TotalTokens: 20}}, 200)
	if nextSecond != second || nextValue != value {
		t.Fatalf("expected throttled value reuse")
	}
}

func TestFormatRateLimitsEmptyMapShowsUnavailable(t *testing.T) {
	got := stripANSI(formatRateLimits(map[string]any{}))
	if got != "unavailable" {
		t.Fatalf("expected unavailable, got %q", got)
	}
}

func TestRunningRowExpandsToTerminalWidth(t *testing.T) {
	row := FormatRunningSummaryForTest(runningEntry(map[string]any{
		"identifier":         "MT-598",
		"state":              "running",
		"codex_total_tokens": 123,
		"runtime_seconds":    15,
		"last_codex_event":   "notification",
		"last_codex_message": turnCompletedMessage("completed"),
	}), 140)

	plain := stripANSI(row)
	if len([]rune(plain)) != 140 {
		t.Fatalf("expected width 140, got %d (%q)", len([]rune(plain)), plain)
	}
	if !strings.Contains(plain, "turn completed (completed)") {
		t.Fatalf("expected humanized message in row: %q", plain)
	}
}

func TestDashboardCoalescesRapidUpdates(t *testing.T) {
	cfg := mustConfig(t)
	provider := &stubSnapshotProvider{
		snapshot: map[string]any{
			"running":  []any{},
			"retrying": []any{},
			"codex_totals": map[string]any{
				"input_tokens":    0,
				"output_tokens":   0,
				"total_tokens":    0,
				"seconds_running": 0,
			},
		},
	}
	renders := make(chan time.Time, 16)
	dashboard := New(Options{
		Provider:        provider,
		Config:          cfg,
		RefreshInterval: time.Hour,
		RenderInterval:  30 * time.Millisecond,
		RenderFn: func(string) {
			renders <- time.Now()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dashboard.Run(ctx)

	select {
	case <-renders:
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("expected initial render")
	}

	provider.setSnapshot(map[string]any{
		"running": []any{
			runningEntry(map[string]any{"identifier": "MT-1", "codex_total_tokens": 12, "last_codex_message": turnCompletedMessage("completed")}),
		},
		"retrying": []any{},
		"codex_totals": map[string]any{
			"input_tokens":    10,
			"output_tokens":   2,
			"total_tokens":    12,
			"seconds_running": 1,
		},
	})

	dashboard.NotifyUpdate()
	dashboard.NotifyUpdate()

	var second time.Time
	select {
	case second = <-renders:
	case <-time.After(300 * time.Millisecond):
		t.Fatalf("expected coalesced render")
	}
	if second.IsZero() {
		t.Fatalf("expected timestamp for coalesced render")
	}

	select {
	case <-renders:
		t.Fatalf("expected no extra immediate render")
	case <-time.After(70 * time.Millisecond):
	}
}

func TestDashboardSuppressesInitialUnavailableFrame(t *testing.T) {
	cfg := mustConfig(t)
	provider := &stubSnapshotProvider{snapshotErr: errors.New("unavailable")}
	renders := make(chan string, 16)
	dashboard := New(Options{
		Provider:        provider,
		Config:          cfg,
		RefreshInterval: time.Hour,
		RenderFn: func(content string) {
			renders <- stripANSI(content)
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dashboard.Run(ctx)

	select {
	case content := <-renders:
		t.Fatalf("expected no initial unavailable render, got %q", content)
	case <-time.After(200 * time.Millisecond):
	}

	provider.setError(nil)
	provider.setSnapshot(map[string]any{
		"running":  []any{},
		"retrying": []any{},
		"codex_totals": map[string]any{
			"input_tokens":    0,
			"output_tokens":   0,
			"total_tokens":    0,
			"seconds_running": 0,
		},
	})
	dashboard.NotifyUpdate()

	select {
	case content := <-renders:
		if strings.Contains(content, "Orchestrator snapshot unavailable") {
			t.Fatalf("expected successful snapshot render, got %q", content)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatalf("expected render after snapshot recovery")
	}
}

func TestHumanizeEventSet(t *testing.T) {
	cases := []struct {
		method   string
		payload  map[string]any
		expected string
	}{
		{"turn/started", map[string]any{"params": map[string]any{"turn": map[string]any{"id": "turn-1"}}}, "turn started"},
		{"turn/completed", map[string]any{"params": map[string]any{"turn": map[string]any{"status": "completed"}}}, "turn completed"},
		{"turn/diff/updated", map[string]any{"params": map[string]any{"diff": "line1\nline2"}}, "turn diff updated"},
		{"turn/plan/updated", map[string]any{"params": map[string]any{"plan": []any{map[string]any{"step": "a"}, map[string]any{"step": "b"}}}}, "plan updated"},
		{"thread/tokenUsage/updated", map[string]any{"params": map[string]any{"usage": map[string]any{"input_tokens": 8, "output_tokens": 3, "total_tokens": 11}}}, "thread token usage updated"},
		{"item/started", map[string]any{"params": map[string]any{"item": map[string]any{"id": "item-1234567890abcdef", "type": "commandExecution", "status": "running"}}}, "item started: command execution"},
		{"item/completed", map[string]any{"params": map[string]any{"item": map[string]any{"type": "fileChange", "status": "completed"}}}, "item completed: file change"},
		{"item/agentMessage/delta", map[string]any{"params": map[string]any{"delta": "hello"}}, "agent message streaming"},
		{"item/plan/delta", map[string]any{"params": map[string]any{"delta": "step"}}, "plan streaming"},
		{"item/reasoning/summaryTextDelta", map[string]any{"params": map[string]any{"summaryText": "thinking"}}, "reasoning summary streaming"},
		{"item/reasoning/summaryPartAdded", map[string]any{"params": map[string]any{"summaryText": "section"}}, "reasoning summary section added"},
		{"item/reasoning/textDelta", map[string]any{"params": map[string]any{"textDelta": "reason"}}, "reasoning text streaming"},
		{"item/commandExecution/outputDelta", map[string]any{"params": map[string]any{"outputDelta": "ok"}}, "command output streaming"},
		{"item/fileChange/outputDelta", map[string]any{"params": map[string]any{"outputDelta": "changed"}}, "file change output streaming"},
		{"item/commandExecution/requestApproval", map[string]any{"params": map[string]any{"parsedCmd": "git status"}}, "command approval requested (git status)"},
		{"item/fileChange/requestApproval", map[string]any{"params": map[string]any{"fileChangeCount": 2}}, "file change approval requested (2 files)"},
		{"item/tool/call", map[string]any{"params": map[string]any{"tool": "linear_graphql"}}, "dynamic tool call requested (linear_graphql)"},
		{"item/tool/requestUserInput", map[string]any{"params": map[string]any{"question": "Continue?"}}, "tool requires user input: Continue?"},
		{"codex/event/agent_message_content_delta", map[string]any{"params": map[string]any{"msg": map[string]any{"payload": map[string]any{"content": "chunk"}}}}, "agent message content streaming: chunk"},
		{"codex/event/reasoning_content_delta", map[string]any{"params": map[string]any{"msg": map[string]any{"payload": map[string]any{"text": "thinking"}}}}, "reasoning content streaming: thinking"},
		{"codex/event/exec_command_output_delta", map[string]any{"params": map[string]any{"outputDelta": "line"}}, "command output streaming"},
		{"codex/event/exec_command_end", map[string]any{"params": map[string]any{"msg": map[string]any{"exit_code": 17}}}, "command completed (exit 17)"},
		{"codex/event/mcp_tool_call_begin", map[string]any{"params": map[string]any{}}, "mcp tool call started"},
		{"codex/event/mcp_tool_call_end", map[string]any{"params": map[string]any{}}, "mcp tool call completed"},
		{"codex/event/mcp_startup_update", map[string]any{"params": map[string]any{"msg": map[string]any{"server": "linear", "status": map[string]any{"state": "ready"}}}}, "mcp startup: linear ready"},
		{"codex/event/token_count", map[string]any{"params": map[string]any{"msg": map[string]any{"payload": map[string]any{"info": map[string]any{"total_token_usage": map[string]any{"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}}}}}}, "token count update (in 3, out 2, total 5)"},
	}

	for _, tc := range cases {
		message := map[string]any{
			"event": "notification",
			"message": map[string]any{
				"method": tc.method,
				"params": tc.payload["params"],
			},
		}
		humanized := HumanizeCodexMessage(message)
		if !strings.Contains(humanized, tc.expected) {
			t.Fatalf("method %s expected %q in %q", tc.method, tc.expected, humanized)
		}
	}
}

func TestHumanizeWrapperEvents(t *testing.T) {
	completed := map[string]any{
		"event": "tool_call_completed",
		"message": map[string]any{
			"payload": map[string]any{"method": "item/tool/call", "params": map[string]any{"name": "linear_graphql"}},
		},
	}
	failed := map[string]any{
		"event": "tool_call_failed",
		"message": map[string]any{
			"payload": map[string]any{"method": "item/tool/call", "params": map[string]any{"tool": "linear_graphql"}},
		},
	}
	unsupported := map[string]any{
		"event": "unsupported_tool_call",
		"message": map[string]any{
			"payload": map[string]any{"method": "item/tool/call", "params": map[string]any{"tool": "unknown_tool"}},
		},
	}

	if got := HumanizeCodexMessage(completed); !strings.Contains(got, "dynamic tool call completed (linear_graphql)") {
		t.Fatalf("unexpected completed wrapper text: %q", got)
	}
	if got := HumanizeCodexMessage(failed); !strings.Contains(got, "dynamic tool call failed (linear_graphql)") {
		t.Fatalf("unexpected failed wrapper text: %q", got)
	}
	if got := HumanizeCodexMessage(unsupported); !strings.Contains(got, "unsupported dynamic tool call rejected (unknown_tool)") {
		t.Fatalf("unexpected unsupported wrapper text: %q", got)
	}
}

func TestTPSGraphSnapshots(t *testing.T) {
	nowMS := int64(600_000)
	currentTokens := 6_000
	samples := make([]TokenSample, 0, 24)
	for timestamp := int64(575_000); timestamp >= 0; timestamp -= 25_000 {
		samples = append(samples, TokenSample{TimestampMS: timestamp, TotalTokens: int(timestamp / 100)})
	}
	if got := TPSGraphForTest(samples, nowMS, currentTokens); got != "████████████████████████" {
		t.Fatalf("unexpected steady graph: %q", got)
	}

	ratesPerBucket := make([]int, 0, 24)
	for i := 1; i <= 24; i++ {
		ratesPerBucket = append(ratesPerBucket, i*2)
	}
	current, rampSamples := graphSamplesFromRates(ratesPerBucket)
	if got := TPSGraphForTest(rampSamples, nowMS, current); got != "▁▂▂▂▃▃▃▃▄▄▄▅▅▅▆▆▆▆▇▇▇██▅" {
		t.Fatalf("unexpected ramp graph: %q", got)
	}
}

func withConfiguredPort(opts RenderOptions, port int) RenderOptions {
	next := opts
	next.ConfiguredPort = intPtr(port)
	return next
}

func intPtr(v int) *int {
	return &v
}

func loadSnapshotFixture(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller unavailable")
	}

	paths := []string{
		filepath.Join(filepath.Dir(file), "testdata", "status_dashboard_snapshots", name),
		// Back-compat for local setups that still rely on the sibling baton repo.
		filepath.Join(filepath.Dir(file), "..", "..", "..", "baton", "elixir", "test", "fixtures", "status_dashboard_snapshots", name),
	}

	var raw []byte
	var err error
	for _, path := range paths {
		raw, err = os.ReadFile(path)
		if err == nil {
			return string(raw)
		}
	}
	t.Fatalf("read fixture failed for %q: %v (tried: %s)", name, err, strings.Join(paths, ", "))
	return ""
}

func stripANSI(value string) string {
	value = strings.ReplaceAll(value, "\x1b", "")
	value = strings.ReplaceAll(value, "[0m", "")
	value = strings.ReplaceAll(value, "[1m", "")
	value = strings.ReplaceAll(value, "[2m", "")
	value = strings.ReplaceAll(value, "[31m", "")
	value = strings.ReplaceAll(value, "[32m", "")
	value = strings.ReplaceAll(value, "[33m", "")
	value = strings.ReplaceAll(value, "[34m", "")
	value = strings.ReplaceAll(value, "[35m", "")
	value = strings.ReplaceAll(value, "[36m", "")
	value = strings.ReplaceAll(value, "[90m", "")
	return value
}

type stubSnapshotProvider struct {
	mu          sync.RWMutex
	snapshot    map[string]any
	snapshotErr error
}

func (s *stubSnapshotProvider) Snapshot(_ time.Duration) (map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.snapshotErr != nil {
		return nil, s.snapshotErr
	}
	return cloneAnyMap(s.snapshot), nil
}

func (s *stubSnapshotProvider) setSnapshot(snapshot map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot = cloneAnyMap(snapshot)
}

func (s *stubSnapshotProvider) setError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshotErr = err
}

func cloneAnyMap(input map[string]any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func mustConfig(t *testing.T) *config.Config {
	t.Helper()
	def := &workflow.Definition{
		Config: map[string]any{
			"tracker": map[string]any{
				"kind": "memory",
				"lifecycle": map[string]any{
					"backlog":      "Backlog",
					"todo":         "Todo",
					"in_progress":  "In Progress",
					"human_review": "In Review",
					"merging":      "Merging",
					"rework":       "Rework",
					"done":         "Done",
				},
			},
			"workspace": map[string]any{
				"root": t.TempDir(),
			},
		},
		PromptTemplate: "prompt",
	}
	cfg, err := config.FromWorkflow("WORKFLOW.md", def)
	if err != nil {
		t.Fatalf("build config: %v", err)
	}
	return cfg
}

func graphSamplesFromRates(ratesPerBucket []int) (int, []TokenSample) {
	bucketMS := int64(25_000)
	timestamp := int64(0)
	tokens := 0
	samples := make([]TokenSample, 0, len(ratesPerBucket)+1)
	for _, rate := range ratesPerBucket {
		nextTimestamp := timestamp + bucketMS
		nextTokens := tokens + int(int64(rate)*bucketMS/1000)
		samples = append(samples, TokenSample{TimestampMS: timestamp, TotalTokens: tokens})
		timestamp = nextTimestamp
		tokens = nextTokens
	}
	samples = append(samples, TokenSample{TimestampMS: timestamp, TotalTokens: tokens})
	return tokens, samples
}

func runningEntry(overrides map[string]any) map[string]any {
	base := map[string]any{
		"identifier":           "MT-000",
		"state":                "running",
		"runtime_kind":         "codex",
		"session_id":           "thread-1234567890",
		"codex_app_server_pid": "4242",
		"codex_total_tokens":   0,
		"runtime_seconds":      0,
		"turn_count":           1,
		"last_codex_event":     "notification",
		"last_codex_message":   turnStartedMessage(),
	}
	for k, v := range overrides {
		base[k] = v
	}
	return base
}

func retryEntry(overrides map[string]any) map[string]any {
	base := map[string]any{
		"issue_id":   "issue-1",
		"identifier": "MT-000",
		"attempt":    1,
		"due_in_ms":  1000,
		"error":      "retry scheduled",
	}
	for k, v := range overrides {
		base[k] = v
	}
	return base
}

func turnStartedMessage() map[string]any {
	return map[string]any{
		"event": "notification",
		"message": map[string]any{
			"method": "turn/started",
			"params": map[string]any{"turn": map[string]any{"id": "turn-1"}},
		},
	}
}

func turnCompletedMessage(status string) map[string]any {
	return map[string]any{
		"event": "notification",
		"message": map[string]any{
			"method": "turn/completed",
			"params": map[string]any{"turn": map[string]any{"status": status}},
		},
	}
}

func execCommandMessage(command string) map[string]any {
	return map[string]any{
		"event": "notification",
		"message": map[string]any{
			"method": "codex/event/exec_command_begin",
			"params": map[string]any{"msg": map[string]any{"command": command}},
		},
	}
}

func agentMessageDelta(delta string) map[string]any {
	return map[string]any{
		"event": "notification",
		"message": map[string]any{
			"method": "codex/event/agent_message_delta",
			"params": map[string]any{"msg": map[string]any{"payload": map[string]any{"delta": delta}}},
		},
	}
}

func tokenUsageMessage(inputTokens int, outputTokens int, totalTokens int) map[string]any {
	return map[string]any{
		"event": "notification",
		"message": map[string]any{
			"method": "thread/tokenUsage/updated",
			"params": map[string]any{
				"tokenUsage": map[string]any{
					"total": map[string]any{
						"inputTokens":  inputTokens,
						"outputTokens": outputTokens,
						"totalTokens":  totalTokens,
					},
				},
			},
		},
	}
}
