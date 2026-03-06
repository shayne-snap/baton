package statusdashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"baton/internal/config"
)

const (
	defaultSnapshotTimeout  = 15 * time.Second
	defaultRefreshInterval  = 1 * time.Second
	defaultRenderInterval   = 16 * time.Millisecond
	startupUnavailableGrace = 3 * time.Second
	minimumIdleRerender     = 1 * time.Second
	throughputWindowMS      = int64(5_000)
	throughputGraphWindowMS = int64(10 * 60 * 1000)
	throughputGraphColumns  = 24

	runningIDWidth           = 8
	runningStageWidth        = 14
	runningPIDWidth          = 8
	runningAgeWidth          = 12
	runningTokensWidth       = 10
	runningSessionWidth      = 14
	runningEventDefaultWidth = 44
	runningEventMinWidth     = 12
	runningRowChromeWidth    = 10
	defaultTerminalColumns   = 115
)

var (
	sparklineBlocks  = []string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}
	ansiRegex        = regexp.MustCompile(`\x1B\[[0-9;]*[A-Za-z]`)
	ansiShortRegex   = regexp.MustCompile(`\x1B.`)
	controlByteRegex = regexp.MustCompile(`[\x00-\x1F\x7F]`)
	whitespaceRegex  = regexp.MustCompile(`\s+`)
)

const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiBlue    = "\x1b[34m"
	ansiCyan    = "\x1b[36m"
	ansiDim     = "\x1b[2m"
	ansiGreen   = "\x1b[32m"
	ansiRed     = "\x1b[31m"
	ansiOrange  = "\x1b[33m"
	ansiYellow  = "\x1b[33m"
	ansiMagenta = "\x1b[35m"
	ansiGray    = "\x1b[90m"
)

type SnapshotProvider interface {
	Snapshot(timeout time.Duration) (map[string]any, error)
}

type Options struct {
	Provider        SnapshotProvider
	Config          *config.Config
	Output          io.Writer
	RenderFn        func(string)
	SnapshotTimeout time.Duration
	RefreshInterval time.Duration
	RenderInterval  time.Duration
	BoundPortFn     func() *int
}

type Dashboard struct {
	provider        SnapshotProvider
	cfg             *config.Config
	output          io.Writer
	snapshotTimeout time.Duration
	refreshInterval time.Duration
	renderInterval  time.Duration
	boundPortFn     func() *int
	renderFn        func(string)
	refreshCh       chan struct{}

	tokenSamples            []TokenSample
	hasLastTPSSecond        bool
	lastTPSSecond           int64
	hasLastTPSValue         bool
	lastTPSValue            float64
	lastRenderedContent     string
	lastRenderedAt          time.Time
	lastSnapshotFingerprint string
	pendingContent          string
	pendingFingerprint      string
	flushTimer              *time.Timer
	flushTimerCh            <-chan time.Time
	startedAt               time.Time
}

type TokenSample struct {
	TimestampMS int64
	TotalTokens int
}

type RenderOptions struct {
	MaxConcurrentAgents int
	LinearProjectSlug   string
	ServerHost          string
	ConfiguredPort      *int
	BoundPort           *int
	TerminalColumns     int
}

func New(opts Options) *Dashboard {
	output := opts.Output
	if output == nil {
		output = os.Stdout
	}
	timeout := opts.SnapshotTimeout
	if timeout <= 0 {
		timeout = defaultSnapshotTimeout
	}
	refresh := opts.RefreshInterval
	if refresh <= 0 {
		refresh = defaultRefreshInterval
	}
	renderInterval := opts.RenderInterval
	if renderInterval <= 0 {
		renderInterval = defaultRenderInterval
	}
	return &Dashboard{
		provider:        opts.Provider,
		cfg:             opts.Config,
		output:          output,
		snapshotTimeout: timeout,
		refreshInterval: refresh,
		renderInterval:  renderInterval,
		boundPortFn:     opts.BoundPortFn,
		renderFn:        opts.RenderFn,
		refreshCh:       make(chan struct{}, 1),
		tokenSamples:    []TokenSample{},
	}
}

func (d *Dashboard) Run(ctx context.Context) {
	if d == nil || d.provider == nil || d.cfg == nil {
		return
	}

	d.startedAt = time.Now().UTC()
	d.renderTick(d.startedAt)
	ticker := time.NewTicker(d.refreshInterval)
	defer ticker.Stop()
	defer d.stopFlushTimer()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			d.renderTick(now.UTC())
		case <-d.refreshCh:
			d.renderTick(time.Now().UTC())
		case now := <-d.flushTimerCh:
			d.flushPending(now.UTC())
		}
	}
}

func (d *Dashboard) RenderOfflineStatus() error {
	return RenderOfflineStatus(d.output)
}

func (d *Dashboard) NotifyUpdate() {
	if d == nil || d.refreshCh == nil {
		return
	}
	select {
	case d.refreshCh <- struct{}{}:
	default:
	}
}

func (d *Dashboard) renderTick(now time.Time) {
	snapshotData, fingerprint := d.snapshotData(now)
	if snapshotData == nil && d.lastRenderedAt.IsZero() {
		startedAt := d.startedAt
		if startedAt.IsZero() {
			startedAt = now
			d.startedAt = startedAt
		}
		if now.Sub(startedAt) < startupUnavailableGrace {
			return
		}
	}
	currentTokens := snapshotTotalTokens(snapshotData)

	var lastSecond *int64
	if d.hasLastTPSSecond {
		lastSecond = &d.lastTPSSecond
	}
	var lastValue *float64
	if d.hasLastTPSValue {
		lastValue = &d.lastTPSValue
	}
	nextSecond, tps := ThrottledTPS(lastSecond, lastValue, now.UnixMilli(), d.tokenSamples, currentTokens)
	d.lastTPSSecond = nextSecond
	d.hasLastTPSSecond = true
	d.lastTPSValue = tps
	d.hasLastTPSValue = true

	if fingerprint == d.lastSnapshotFingerprint && !d.lastRenderedAt.IsZero() && now.Sub(d.lastRenderedAt) < minimumIdleRerender {
		return
	}

	content := FormatSnapshotContentForTest(snapshotData, tps, d.renderOptions())
	if content == d.lastRenderedContent {
		d.lastSnapshotFingerprint = fingerprint
		return
	}

	if !d.lastRenderedAt.IsZero() && now.Sub(d.lastRenderedAt) < d.renderInterval {
		d.pendingContent = content
		d.pendingFingerprint = fingerprint
		d.scheduleFlush(now)
		return
	}

	d.renderNow(content, fingerprint, now)
}

func (d *Dashboard) renderNow(content string, fingerprint string, now time.Time) {
	if d.renderFn != nil {
		d.renderFn(content)
	} else {
		renderToTerminal(d.output, content)
	}
	d.lastRenderedAt = now
	d.lastRenderedContent = content
	d.lastSnapshotFingerprint = fingerprint
	d.pendingContent = ""
	d.pendingFingerprint = ""
	d.stopFlushTimer()
}

func (d *Dashboard) flushPending(now time.Time) {
	if strings.TrimSpace(d.pendingContent) == "" {
		d.stopFlushTimer()
		return
	}
	d.renderNow(d.pendingContent, d.pendingFingerprint, now)
}

func (d *Dashboard) scheduleFlush(now time.Time) {
	delay := d.renderInterval
	if !d.lastRenderedAt.IsZero() {
		remaining := d.renderInterval - now.Sub(d.lastRenderedAt)
		if remaining > 0 {
			delay = remaining
		} else {
			delay = time.Millisecond
		}
	}
	if delay <= 0 {
		delay = time.Millisecond
	}

	if d.flushTimer != nil {
		return
	}

	d.flushTimer = time.NewTimer(delay)
	d.flushTimerCh = d.flushTimer.C
}

func (d *Dashboard) stopFlushTimer() {
	if d.flushTimer == nil {
		return
	}
	if !d.flushTimer.Stop() {
		select {
		case <-d.flushTimer.C:
		default:
		}
	}
	d.flushTimer = nil
	d.flushTimerCh = nil
}

func (d *Dashboard) snapshotData(now time.Time) (map[string]any, string) {
	snapshot, err := d.provider.Snapshot(d.snapshotTimeout)
	if err != nil {
		d.tokenSamples = pruneSamples(d.tokenSamples, now.UnixMilli())
		return nil, "unavailable"
	}

	data := map[string]any{
		"running":      mapSlice(snapshot["running"]),
		"retrying":     mapSlice(snapshot["retrying"]),
		"codex_totals": mapOf(snapshot["codex_totals"]),
		"rate_limits":  snapshot["rate_limits"],
		"polling":      mapOf(snapshot["polling"]),
	}

	currentTokens := intValue(mapOf(data["codex_totals"])["total_tokens"])
	d.tokenSamples = updateTokenSamples(d.tokenSamples, now.UnixMilli(), max(currentTokens, 0))

	encoded, _ := json.Marshal(data)
	return data, string(encoded)
}

func (d *Dashboard) renderOptions() RenderOptions {
	opts := RenderOptions{
		MaxConcurrentAgents: d.cfg.MaxConcurrentAgents(),
		LinearProjectSlug:   d.cfg.LinearProjectSlug(),
		ServerHost:          d.cfg.ServerHost(),
		ConfiguredPort:      d.cfg.ServerPort(),
	}
	if d.boundPortFn != nil {
		opts.BoundPort = d.boundPortFn()
	}
	return opts
}

func RenderOfflineStatus(output io.Writer) error {
	if output == nil {
		output = os.Stdout
	}
	content := strings.Join([]string{
		colorize("╭─ BATON STATUS", ansiBold),
		colorize("│ app_status=offline", ansiRed),
		closingBorder(),
	}, "\n")
	renderToTerminal(output, content)
	return nil
}

func FormatSnapshotContentForTest(snapshotData map[string]any, tps float64, opts RenderOptions) string {
	if snapshotData == nil {
		lines := []string{
			colorize("╭─ BATON STATUS", ansiBold),
			colorize("│ Orchestrator snapshot unavailable", ansiRed),
			colorize("│ Throughput: ", ansiBold) + colorize(fmt.Sprintf("%s tps", formatTPS(tps)), ansiCyan),
		}
		lines = append(lines, formatProjectLinkLines(opts)...)
		lines = append(lines, formatProjectRefreshLine(nil), closingBorder())
		return strings.Join(lines, "\n")
	}

	running := mapSlice(snapshotData["running"])
	retrying := mapSlice(snapshotData["retrying"])
	codexTotals := mapOf(snapshotData["codex_totals"])
	rateLimits := snapshotData["rate_limits"]
	polling := mapOf(snapshotData["polling"])
	terminalColumns := opts.TerminalColumns
	runningEventWidth := runningEventWidth(terminalColumns)

	lines := []string{
		colorize("╭─ BATON STATUS", ansiBold),
		colorize("│ Agents: ", ansiBold) + colorize(fmt.Sprintf("%d", len(running)), ansiGreen) + colorize("/", ansiGray) + colorize(fmt.Sprintf("%d", opts.MaxConcurrentAgents), ansiGray),
		colorize("│ Throughput: ", ansiBold) + colorize(fmt.Sprintf("%s tps", formatTPS(tps)), ansiCyan),
		colorize("│ Runtime: ", ansiBold) + colorize(formatRuntimeSeconds(intValue(codexTotals["seconds_running"])), ansiMagenta),
		colorize("│ Tokens: ", ansiBold) +
			colorize("in "+formatCount(codexTotals["input_tokens"]), ansiYellow) +
			colorize(" | ", ansiGray) +
			colorize("out "+formatCount(codexTotals["output_tokens"]), ansiYellow) +
			colorize(" | ", ansiGray) +
			colorize("total "+formatCount(codexTotals["total_tokens"]), ansiYellow),
		colorize("│ Rate Limits: ", ansiBold) + formatRateLimits(rateLimits),
	}

	lines = append(lines, formatProjectLinkLines(opts)...)
	lines = append(lines, formatProjectRefreshLine(polling))
	lines = append(lines,
		colorize("├─ Running", ansiBold),
		"│",
		runningTableHeaderRow(runningEventWidth),
		runningTableSeparatorRow(runningEventWidth),
	)
	lines = append(lines, formatRunningRows(running, runningEventWidth)...)
	if len(running) > 0 {
		lines = append(lines, "│")
	}
	lines = append(lines, colorize("├─ Backoff queue", ansiBold), "│")
	lines = append(lines, formatRetryRows(retrying)...)
	lines = append(lines, closingBorder())

	return strings.Join(lines, "\n")
}

func FormatRunningSummaryForTest(runningEntry map[string]any, terminalColumns int) string {
	return formatRunningSummary(runningEntry, runningEventWidth(terminalColumns))
}

func DashboardURLForTest(host string, configuredPort *int, boundPort *int) string {
	return dashboardURL(host, configuredPort, boundPort)
}

func RollingTPS(samples []TokenSample, nowMS int64, currentTokens int) float64 {
	window := append([]TokenSample{{TimestampMS: nowMS, TotalTokens: currentTokens}}, samples...)
	window = pruneSamples(window, nowMS)
	if len(window) < 2 {
		return 0.0
	}
	first := window[len(window)-1]
	elapsedMS := nowMS - first.TimestampMS
	deltaTokens := max(0, currentTokens-first.TotalTokens)
	if elapsedMS <= 0 {
		return 0.0
	}
	return float64(deltaTokens) / (float64(elapsedMS) / 1000.0)
}

func ThrottledTPS(lastSecond *int64, lastValue *float64, nowMS int64, tokenSamples []TokenSample, currentTokens int) (int64, float64) {
	second := nowMS / 1000
	if lastSecond != nil && *lastSecond == second && lastValue != nil {
		return second, *lastValue
	}
	return second, RollingTPS(tokenSamples, nowMS, currentTokens)
}

func TPSGraphForTest(samples []TokenSample, nowMS int64, currentTokens int) string {
	bucketMS := throughputGraphWindowMS / throughputGraphColumns
	activeBucketStart := (nowMS / bucketMS) * bucketMS
	graphWindowStart := activeBucketStart - int64(throughputGraphColumns-1)*bucketMS

	series := append([]TokenSample{{TimestampMS: nowMS, TotalTokens: currentTokens}}, samples...)
	series = pruneGraphSamples(series, nowMS)
	sort.Slice(series, func(i, j int) bool { return series[i].TimestampMS < series[j].TimestampMS })

	type rateSample struct {
		TimestampMS int64
		TPS         float64
	}
	rates := make([]rateSample, 0, max(len(series)-1, 0))
	for i := 0; i+1 < len(series); i++ {
		start := series[i]
		end := series[i+1]
		elapsed := end.TimestampMS - start.TimestampMS
		delta := max(0, end.TotalTokens-start.TotalTokens)
		tps := 0.0
		if elapsed > 0 {
			tps = float64(delta) / (float64(elapsed) / 1000.0)
		}
		rates = append(rates, rateSample{TimestampMS: end.TimestampMS, TPS: tps})
	}

	bucketed := make([]float64, 0, throughputGraphColumns)
	for idx := 0; idx < throughputGraphColumns; idx++ {
		bucketStart := graphWindowStart + int64(idx)*bucketMS
		bucketEnd := bucketStart + bucketMS
		lastBucket := idx == throughputGraphColumns-1

		sum := 0.0
		count := 0
		for _, item := range rates {
			inBucket := item.TimestampMS >= bucketStart && item.TimestampMS < bucketEnd
			if lastBucket {
				inBucket = item.TimestampMS >= bucketStart && item.TimestampMS <= bucketEnd
			}
			if inBucket {
				sum += item.TPS
				count++
			}
		}
		if count == 0 {
			bucketed = append(bucketed, 0.0)
		} else {
			bucketed = append(bucketed, sum/float64(count))
		}
	}

	maxTPS := 0.0
	for _, value := range bucketed {
		if value > maxTPS {
			maxTPS = value
		}
	}

	var b strings.Builder
	for _, value := range bucketed {
		index := 0
		if maxTPS > 0 {
			index = int(math.Round(float64(len(sparklineBlocks)-1) * (value / maxTPS)))
			if index < 0 {
				index = 0
			}
			if index >= len(sparklineBlocks) {
				index = len(sparklineBlocks) - 1
			}
		}
		b.WriteString(sparklineBlocks[index])
	}
	return b.String()
}

func FormatTimestampForTest(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format("2006-01-02 15:04:05Z")
}

func HumanizeCodexMessage(message any) string {
	if message == nil {
		return "no codex message yet"
	}
	if mapped, ok := message.(map[string]any); ok {
		if rawMessage, exists := mapped["message"]; exists {
			payload := unwrapCodexMessagePayload(mapOf(rawMessage))
			event := anyToString(mapped["event"])
			if eventText := humanizeCodexEvent(event, mapped, payload); eventText != "" {
				return truncate(eventText, 140)
			}
			return truncate(humanizeCodexPayload(payload), 140)
		}

		payload := unwrapCodexMessagePayload(mapped)
		return truncate(humanizeCodexPayload(payload), 140)
	}
	if text, ok := message.(string); ok {
		return truncate(inlineText(text), 140)
	}
	return truncate(humanizeCodexPayload(message), 140)
}

func humanizeCodexEvent(event string, message map[string]any, payload any) string {
	switch event {
	case "session_started":
		sessionID := anyToString(mapValue(payload, []string{"session_id"}))
		if sessionID != "" {
			return "session started (" + sessionID + ")"
		}
		return "session started"
	case "turn_input_required":
		return "turn blocked: waiting for user input"
	case "approval_auto_approved":
		method := anyToString(mapValue(payload, []string{"method"}))
		if method == "" {
			method = anyToString(mapPath(message, "payload", "method"))
		}
		decision := anyToString(message["decision"])
		base := "approval request auto-approved"
		if method != "" {
			base = humanizeCodexMethod(method, payload) + " (auto-approved)"
		}
		if decision != "" {
			return base + ": " + decision
		}
		return base
	case "tool_input_auto_answered":
		base := humanizeCodexMethod("item/tool/requestUserInput", payload)
		if base == "" {
			base = "tool input auto-answered"
		} else {
			base += " (auto-answered)"
		}
		answer := anyToString(message["answer"])
		if answer != "" {
			return base + ": " + inlineText(answer)
		}
		return base
	case "tool_call_completed":
		return humanizeDynamicToolEvent("dynamic tool call completed", payload)
	case "tool_call_failed":
		return humanizeDynamicToolEvent("dynamic tool call failed", payload)
	case "unsupported_tool_call":
		return humanizeDynamicToolEvent("unsupported dynamic tool call rejected", payload)
	case "turn_ended_with_error":
		return "turn ended with error: " + formatReason(message)
	case "startup_failed":
		return "startup failed: " + formatReason(message)
	case "turn_failed":
		return humanizeCodexMethod("turn/failed", payload)
	case "turn_cancelled":
		return "turn cancelled"
	case "malformed":
		return "malformed JSON event from codex"
	default:
		return ""
	}
}

func humanizeDynamicToolEvent(prefix string, payload any) string {
	tool := anyToString(mapPath(payload, "params", "tool"))
	if tool == "" {
		tool = anyToString(mapPath(payload, "params", "name"))
	}
	if tool == "" {
		return prefix
	}
	return fmt.Sprintf("%s (%s)", prefix, tool)
}

func formatReason(message map[string]any) string {
	reason := anyToString(mapValue(message, []string{"reason"}))
	if reason != "" {
		return reason
	}
	reason = anyToString(mapPath(message, "payload", "reason"))
	if reason != "" {
		return reason
	}
	return "unknown"
}

func unwrapCodexMessagePayload(message map[string]any) any {
	if method := anyToString(mapValue(message, []string{"method"})); method != "" {
		return message
	}
	if sessionID := anyToString(mapValue(message, []string{"session_id"})); sessionID != "" {
		return message
	}
	if reason := anyToString(mapValue(message, []string{"reason"})); reason != "" {
		return message
	}
	if payload, ok := message["payload"]; ok {
		return payload
	}
	return message
}

func humanizeCodexPayload(payload any) string {
	mapped, ok := payload.(map[string]any)
	if !ok {
		if text, ok := payload.(string); ok {
			return inlineText(text)
		}
		return inlineText(fmt.Sprint(payload))
	}

	method := anyToString(mapValue(mapped, []string{"method"}))
	if method != "" {
		return humanizeCodexMethod(method, mapped)
	}

	sessionID := anyToString(mapValue(mapped, []string{"session_id"}))
	if sessionID != "" {
		return "session started (" + sessionID + ")"
	}

	if errValue, ok := mapped["error"]; ok {
		return "error: " + formatErrorValue(errValue)
	}

	return inlineText(formatStructured(mapped))
}

func humanizeCodexMethod(method string, payload any) string {
	switch method {
	case "thread/started":
		threadID := anyToString(mapPath(payload, "params", "thread", "id"))
		if threadID != "" {
			return "thread started (" + threadID + ")"
		}
		return "thread started"
	case "turn/started":
		turnID := anyToString(mapPath(payload, "params", "turn", "id"))
		if turnID != "" {
			return "turn started (" + turnID + ")"
		}
		return "turn started"
	case "turn/completed":
		status := anyToString(mapPath(payload, "params", "turn", "status"))
		if status == "" {
			status = "completed"
		}
		usage := firstNonNil(
			mapPath(payload, "params", "usage"),
			mapPath(payload, "params", "tokenUsage"),
			mapValue(payload, []string{"usage"}),
		)
		usageText := formatUsageCounts(usage)
		if usageText == "" {
			return "turn completed (" + status + ")"
		}
		return "turn completed (" + status + ") (" + usageText + ")"
	case "turn/failed":
		errText := anyToString(mapPath(payload, "params", "error", "message"))
		if errText == "" {
			return "turn failed"
		}
		return "turn failed: " + errText
	case "turn/cancelled":
		return "turn cancelled"
	case "turn/diff/updated":
		diff := anyToString(mapPath(payload, "params", "diff"))
		if diff == "" {
			return "turn diff updated"
		}
		lineCount := len(strings.Split(strings.TrimSpace(diff), "\n"))
		if strings.TrimSpace(diff) == "" {
			lineCount = 0
		}
		return fmt.Sprintf("turn diff updated (%d lines)", lineCount)
	case "turn/plan/updated":
		steps, _ := mapPath(payload, "params", "plan").([]any)
		if steps == nil {
			steps, _ = mapPath(payload, "params", "steps").([]any)
		}
		if steps == nil {
			steps, _ = mapPath(payload, "params", "items").([]any)
		}
		if steps != nil {
			return fmt.Sprintf("plan updated (%d steps)", len(steps))
		}
		return "plan updated"
	case "thread/tokenUsage/updated":
		usage := firstNonNil(
			mapPath(payload, "params", "tokenUsage", "total"),
			mapValue(payload, []string{"usage"}),
		)
		usageText := formatUsageCounts(usage)
		if usageText == "" {
			return "thread token usage updated"
		}
		return "thread token usage updated (" + usageText + ")"
	case "item/started":
		return humanizeItemLifecycle("started", payload)
	case "item/completed":
		return humanizeItemLifecycle("completed", payload)
	case "item/agentMessage/delta":
		return humanizeStreamingEvent("agent message streaming", payload)
	case "item/plan/delta":
		return humanizeStreamingEvent("plan streaming", payload)
	case "item/reasoning/summaryTextDelta":
		return humanizeStreamingEvent("reasoning summary streaming", payload)
	case "item/reasoning/summaryPartAdded":
		return humanizeStreamingEvent("reasoning summary section added", payload)
	case "item/reasoning/textDelta":
		return humanizeStreamingEvent("reasoning text streaming", payload)
	case "item/commandExecution/outputDelta":
		return humanizeStreamingEvent("command output streaming", payload)
	case "item/fileChange/outputDelta":
		return humanizeStreamingEvent("file change output streaming", payload)
	case "item/commandExecution/requestApproval":
		cmd := normalizedCommand(mapPath(payload, "params", "parsedCmd"), mapPath(payload, "params", "command"), mapPath(payload, "params", "args"))
		if cmd == "" {
			return "command approval requested"
		}
		return "command approval requested (" + cmd + ")"
	case "item/fileChange/requestApproval":
		count := intValue(mapPath(payload, "params", "fileChangeCount"))
		if count <= 0 {
			return "file change approval requested"
		}
		return fmt.Sprintf("file change approval requested (%d files)", count)
	case "item/tool/call":
		tool := anyToString(firstNonNil(
			mapPath(payload, "params", "tool"),
			mapPath(payload, "params", "name"),
		))
		if tool == "" {
			return "dynamic tool call requested"
		}
		return "dynamic tool call requested (" + tool + ")"
	case "item/tool/requestUserInput":
		question := anyToString(mapPath(payload, "params", "question"))
		if question == "" {
			return "tool requires user input"
		}
		return "tool requires user input: " + inlineText(question)
	case "tool/requestUserInput":
		return humanizeCodexMethod("item/tool/requestUserInput", payload)
	default:
		if strings.HasPrefix(method, "codex/event/") {
			suffix := strings.TrimPrefix(method, "codex/event/")
			return humanizeCodexWrapperEvent(suffix, payload)
		}
		return inlineText(formatStructured(payload))
	}
}

func humanizeItemLifecycle(verb string, payload any) string {
	item := mapOf(mapPath(payload, "params", "item"))
	itemType := humanizeItemType(anyToString(item["type"]))
	status := humanizeStatus(anyToString(item["status"]))
	itemID := shortID(anyToString(item["id"]))

	parts := []string{fmt.Sprintf("item %s: %s", verb, itemType)}
	if status != "" {
		parts = append(parts, "("+status+")")
	}
	if itemID != "" {
		parts = append(parts, "["+itemID+"]")
	}
	return strings.Join(parts, " ")
}

func humanizeStreamingEvent(prefix string, payload any) string {
	delta := extractDeltaPreview(payload)
	if delta == nil {
		return prefix
	}
	return prefix + ": " + *delta
}

func humanizeReasoningUpdate(payload any) string {
	reason := extractReasoningFocus(payload)
	if reason == nil {
		return "reasoning update"
	}
	return "reasoning update: " + *reason
}

func humanizeExecCommandBegin(payload any) string {
	command := firstNonNil(
		mapPath(payload, "params", "msg", "command"),
		mapPath(payload, "params", "msg", "parsed_cmd"),
	)
	normalized := normalizedCommand(command)
	if normalized == "" {
		return "command started"
	}
	return normalized
}

func humanizeExecCommandEnd(payload any) string {
	exitCode := parseInteger(firstNonNil(
		mapPath(payload, "params", "msg", "exit_code"),
		mapPath(payload, "params", "msg", "exitCode"),
	))
	if exitCode == nil {
		return "command completed"
	}
	return fmt.Sprintf("command completed (exit %d)", *exitCode)
}

func humanizeCodexWrapperEvent(suffix string, payload any) string {
	switch suffix {
	case "mcp_startup_update":
		server := anyToString(mapPath(payload, "params", "msg", "server"))
		if server == "" {
			server = "mcp"
		}
		state := anyToString(mapPath(payload, "params", "msg", "status", "state"))
		if state == "" {
			state = "updated"
		}
		return fmt.Sprintf("mcp startup: %s %s", server, state)
	case "mcp_startup_complete":
		return "mcp startup complete"
	case "task_started":
		return "task started"
	case "user_message":
		return "user message received"
	case "item_started":
		itemType := wrapperPayloadType(payload)
		if itemType == "token_count" {
			return humanizeCodexWrapperEvent("token_count", payload)
		}
		if itemType != "" {
			return fmt.Sprintf("item started (%s)", humanizeItemType(itemType))
		}
		return "item started"
	case "item_completed":
		itemType := wrapperPayloadType(payload)
		if itemType == "token_count" {
			return humanizeCodexWrapperEvent("token_count", payload)
		}
		if itemType != "" {
			return fmt.Sprintf("item completed (%s)", humanizeItemType(itemType))
		}
		return "item completed"
	case "agent_message_delta":
		return humanizeStreamingEvent("agent message streaming", payload)
	case "agent_message_content_delta":
		return humanizeStreamingEvent("agent message content streaming", payload)
	case "agent_reasoning_delta":
		return humanizeStreamingEvent("reasoning streaming", payload)
	case "reasoning_content_delta":
		return humanizeStreamingEvent("reasoning content streaming", payload)
	case "agent_reasoning_section_break":
		return "reasoning section break"
	case "agent_reasoning":
		return humanizeReasoningUpdate(payload)
	case "turn_diff":
		return "turn diff updated"
	case "exec_command_begin":
		return humanizeExecCommandBegin(payload)
	case "exec_command_end":
		return humanizeExecCommandEnd(payload)
	case "exec_command_output_delta":
		return "command output streaming"
	case "mcp_tool_call_begin":
		return "mcp tool call started"
	case "mcp_tool_call_end":
		return "mcp tool call completed"
	case "token_count":
		usage := extractFirstPath(payload, tokenUsagePaths())
		usageText := formatUsageCounts(usage)
		if usageText == "" {
			return "token count update"
		}
		return "token count update (" + usageText + ")"
	default:
		msgType := anyToString(mapPath(payload, "params", "msg", "type"))
		if msgType != "" {
			return fmt.Sprintf("%s (%s)", suffix, msgType)
		}
		return suffix
	}
}

func wrapperPayloadType(payload any) string {
	return anyToString(mapPath(payload, "params", "msg", "payload", "type"))
}

func extractDeltaPreview(payload any) *string {
	value := extractFirstPath(payload, deltaPaths())
	text := anyToString(value)
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	preview := inlineText(trimmed)
	return &preview
}

func extractReasoningFocus(payload any) *string {
	value := extractFirstPath(payload, reasoningFocusPaths())
	text := anyToString(value)
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	focus := inlineText(trimmed)
	return &focus
}

func extractFirstPath(payload any, paths [][]string) any {
	for _, path := range paths {
		if value := mapPath(payload, path...); value != nil {
			return value
		}
	}
	return nil
}

func tokenUsagePaths() [][]string {
	return [][]string{
		{"params", "msg", "payload", "info", "total_token_usage"},
		{"params", "msg", "info", "total_token_usage"},
		{"params", "tokenUsage", "total"},
	}
}

func deltaPaths() [][]string {
	return [][]string{
		{"params", "delta"},
		{"params", "msg", "delta"},
		{"params", "textDelta"},
		{"params", "msg", "textDelta"},
		{"params", "outputDelta"},
		{"params", "msg", "outputDelta"},
		{"params", "text"},
		{"params", "msg", "text"},
		{"params", "summaryText"},
		{"params", "msg", "summaryText"},
		{"params", "msg", "content"},
		{"params", "msg", "payload", "delta"},
		{"params", "msg", "payload", "textDelta"},
		{"params", "msg", "payload", "outputDelta"},
		{"params", "msg", "payload", "text"},
		{"params", "msg", "payload", "summaryText"},
		{"params", "msg", "payload", "content"},
	}
}

func reasoningFocusPaths() [][]string {
	return [][]string{
		{"params", "reason"},
		{"params", "summaryText"},
		{"params", "summary"},
		{"params", "text"},
		{"params", "msg", "reason"},
		{"params", "msg", "summaryText"},
		{"params", "msg", "summary"},
		{"params", "msg", "text"},
		{"params", "msg", "payload", "reason"},
		{"params", "msg", "payload", "summaryText"},
		{"params", "msg", "payload", "summary"},
		{"params", "msg", "payload", "text"},
	}
}

func formatUsageCounts(usage any) string {
	mapped := mapOf(usage)
	if len(mapped) == 0 {
		return ""
	}
	in := parseInteger(firstNonNil(
		mapped["input_tokens"],
		mapped["prompt_tokens"],
		mapped["inputTokens"],
		mapped["promptTokens"],
	))
	out := parseInteger(firstNonNil(
		mapped["output_tokens"],
		mapped["completion_tokens"],
		mapped["outputTokens"],
		mapped["completionTokens"],
	))
	total := parseInteger(firstNonNil(mapped["total_tokens"], mapped["total"], mapped["totalTokens"]))
	if in == nil && out == nil && total == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if in != nil {
		parts = append(parts, "in "+formatCount(*in))
	}
	if out != nil {
		parts = append(parts, "out "+formatCount(*out))
	}
	if total != nil {
		parts = append(parts, "total "+formatCount(*total))
	}
	return strings.Join(parts, ", ")
}

func formatErrorValue(value any) string {
	if value == nil {
		return "unknown"
	}
	if text, ok := value.(string); ok {
		if strings.TrimSpace(text) == "" {
			return "unknown"
		}
		return inlineText(text)
	}
	return inlineText(formatStructured(value))
}

func formatProjectLinkLines(opts RenderOptions) []string {
	projectPart := colorize("n/a", ansiGray)
	if strings.TrimSpace(opts.LinearProjectSlug) != "" {
		projectPart = colorize(linearProjectURL(strings.TrimSpace(opts.LinearProjectSlug)), ansiCyan)
	}
	lines := []string{
		colorize("│ Project: ", ansiBold) + projectPart,
	}
	if url := dashboardURL(opts.ServerHost, opts.ConfiguredPort, opts.BoundPort); url != "" {
		lines = append(lines, colorize("│ Dashboard: ", ansiBold)+colorize(url, ansiCyan))
	}
	return lines
}

func formatProjectRefreshLine(polling map[string]any) string {
	if polling != nil {
		if checking, ok := polling["checking?"].(bool); ok && checking {
			return colorize("│ Next refresh: ", ansiBold) + colorize("checking now…", ansiCyan)
		}
		if dueIn, ok := polling["next_poll_in_ms"]; ok {
			due := intValue(dueIn)
			if due >= 0 {
				seconds := (max(due, 0) + 999) / 1000
				return colorize("│ Next refresh: ", ansiBold) + colorize(fmt.Sprintf("%ds", seconds), ansiCyan)
			}
		}
	}
	return colorize("│ Next refresh: ", ansiBold) + colorize("n/a", ansiGray)
}

func linearProjectURL(projectSlug string) string {
	return "https://linear.app/project/" + projectSlug + "/issues"
}

func dashboardURL(host string, configuredPort *int, boundPort *int) string {
	if configuredPort == nil && boundPort == nil {
		return ""
	}
	port := 0
	if boundPort != nil {
		port = *boundPort
	} else if configuredPort != nil {
		port = *configuredPort
	}
	if port <= 0 {
		return ""
	}
	return fmt.Sprintf("http://%s:%d/", dashboardURLHost(host), port)
}

func dashboardURLHost(host string) string {
	trimmed := strings.TrimSpace(host)
	switch trimmed {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	}
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		return trimmed
	}
	if strings.Contains(trimmed, ":") {
		return "[" + trimmed + "]"
	}
	return trimmed
}

func renderToTerminal(output io.Writer, content string) {
	if output == nil {
		output = os.Stdout
	}
	_, _ = io.WriteString(output, "\x1b[H\x1b[2J")
	_, _ = io.WriteString(output, normalizeStatusLines(content))
	_, _ = io.WriteString(output, "\n")
}

func updateTokenSamples(samples []TokenSample, nowMS int64, totalTokens int) []TokenSample {
	samples = append([]TokenSample{{TimestampMS: nowMS, TotalTokens: totalTokens}}, samples...)
	return pruneGraphSamples(samples, nowMS)
}

func pruneSamples(samples []TokenSample, nowMS int64) []TokenSample {
	minTimestamp := nowMS - throughputWindowMS
	out := make([]TokenSample, 0, len(samples))
	for _, sample := range samples {
		if sample.TimestampMS >= minTimestamp {
			out = append(out, sample)
		}
	}
	return out
}

func pruneGraphSamples(samples []TokenSample, nowMS int64) []TokenSample {
	minTimestamp := nowMS - max(throughputWindowMS, throughputGraphWindowMS)
	out := make([]TokenSample, 0, len(samples))
	for _, sample := range samples {
		if sample.TimestampMS >= minTimestamp {
			out = append(out, sample)
		}
	}
	return out
}

func formatRunningRows(running []map[string]any, runningEventWidth int) []string {
	if len(running) == 0 {
		return []string{
			"│  " + colorize("No active agents", ansiGray),
			"│",
		}
	}

	sort.SliceStable(running, func(i, j int) bool {
		return strings.TrimSpace(anyToString(running[i]["identifier"])) < strings.TrimSpace(anyToString(running[j]["identifier"]))
	})

	rows := make([]string, 0, len(running))
	for _, entry := range running {
		rows = append(rows, formatRunningSummary(entry, runningEventWidth))
	}
	return rows
}

func formatRunningSummary(runningEntry map[string]any, runningEventWidth int) string {
	issue := formatCell(orDefault(anyToString(runningEntry["identifier"]), "unknown"), runningIDWidth, false)
	state := orDefault(anyToString(runningEntry["state"]), "unknown")
	stateDisplay := formatCell(state, runningStageWidth, false)
	session := formatCell(compactSessionID(anyToString(runningEntry["session_id"])), runningSessionWidth, false)
	pid := formatCell(orDefault(anyToString(runningEntry["codex_app_server_pid"]), "n/a"), runningPIDWidth, false)
	totalTokens := max(intValue(runningEntry["codex_total_tokens"]), 0)
	runtimeSeconds := max(intValue(runningEntry["runtime_seconds"]), 0)
	turnCount := max(intValue(runningEntry["turn_count"]), 0)
	age := formatCell(formatRuntimeAndTurns(runtimeSeconds, turnCount), runningAgeWidth, false)
	event := anyToString(runningEntry["last_codex_event"])
	eventLabel := formatCell(HumanizeCodexMessage(runningEntry["last_codex_message"]), runningEventWidth, false)
	tokens := formatCell(formatCount(totalTokens), runningTokensWidth, true)

	statusColor := ansiBlue
	switch event {
	case "", "none":
		statusColor = ansiRed
	case "codex/event/token_count":
		statusColor = ansiYellow
	case "codex/event/task_started":
		statusColor = ansiGreen
	case "turn_completed", "turn/completed":
		statusColor = ansiMagenta
	}

	parts := []string{
		"│ ",
		statusDot(statusColor),
		" ",
		colorize(issue, ansiCyan),
		" ",
		colorize(stateDisplay, statusColor),
		" ",
		colorize(pid, ansiYellow),
		" ",
		colorize(age, ansiMagenta),
		" ",
		colorize(tokens, ansiYellow),
		" ",
		colorize(session, ansiCyan),
		" ",
		colorize(eventLabel, statusColor),
	}
	return strings.Join(parts, "")
}

func formatRetryRows(retrying []map[string]any) []string {
	if len(retrying) == 0 {
		return []string{"│  " + colorize("No queued retries", ansiGray)}
	}
	sort.SliceStable(retrying, func(i, j int) bool {
		return intValue(retrying[i]["due_in_ms"]) < intValue(retrying[j]["due_in_ms"])
	})
	rows := make([]string, 0, len(retrying))
	for _, entry := range retrying {
		rows = append(rows, formatRetrySummary(entry))
	}
	return rows
}

func formatRetrySummary(retryEntry map[string]any) string {
	issueID := orDefault(anyToString(retryEntry["issue_id"]), "unknown")
	identifier := orDefault(anyToString(retryEntry["identifier"]), issueID)
	attempt := max(intValue(retryEntry["attempt"]), 0)
	dueInMS := max(intValue(retryEntry["due_in_ms"]), 0)
	errorSuffix := formatRetryError(retryEntry["error"])

	return "│  " + colorize("↻", ansiOrange) + " " +
		colorize(identifier, ansiRed) + " " +
		colorize(fmt.Sprintf("attempt=%d", attempt), ansiYellow) +
		colorize(" in ", ansiDim) +
		colorize(nextInWords(dueInMS), ansiCyan) +
		errorSuffix
}

func nextInWords(dueInMS int) string {
	secs := dueInMS / 1000
	millis := dueInMS % 1000
	return fmt.Sprintf("%d.%03ds", secs, millis)
}

func formatRetryError(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	sanitized := text
	sanitized = strings.ReplaceAll(sanitized, "\\r\\n", " ")
	sanitized = strings.ReplaceAll(sanitized, "\\r", " ")
	sanitized = strings.ReplaceAll(sanitized, "\\n", " ")
	sanitized = strings.ReplaceAll(sanitized, "\r\n", " ")
	sanitized = strings.ReplaceAll(sanitized, "\r", " ")
	sanitized = strings.ReplaceAll(sanitized, "\n", " ")
	sanitized = whitespaceRegex.ReplaceAllString(sanitized, " ")
	sanitized = strings.TrimSpace(sanitized)
	if sanitized == "" {
		return ""
	}
	return " " + colorize("error="+truncate(sanitized, 96), ansiDim)
}

func formatRuntimeSeconds(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	mins := seconds / 60
	secs := seconds % 60
	return fmt.Sprintf("%dm %ds", mins, secs)
}

func formatRuntimeAndTurns(seconds int, turnCount int) string {
	if turnCount > 0 {
		return fmt.Sprintf("%s / %d", formatRuntimeSeconds(seconds), turnCount)
	}
	return formatRuntimeSeconds(seconds)
}

func formatCount(value any) string {
	switch typed := value.(type) {
	case nil:
		return "0"
	case int:
		return groupThousands(strconv.Itoa(typed))
	case int64:
		return groupThousands(strconv.FormatInt(typed, 10))
	case float64:
		return groupThousands(strconv.Itoa(int(typed)))
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return "0"
		}
		if parsed, err := strconv.Atoi(trimmed); err == nil {
			return groupThousands(strconv.Itoa(parsed))
		}
		return trimmed
	default:
		return fmt.Sprint(value)
	}
}

func runningTableHeaderRow(runningEventWidth int) string {
	header := strings.Join([]string{
		formatCell("ID", runningIDWidth, false),
		formatCell("STAGE", runningStageWidth, false),
		formatCell("PID", runningPIDWidth, false),
		formatCell("AGE / TURN", runningAgeWidth, false),
		formatCell("TOKENS", runningTokensWidth, false),
		formatCell("SESSION", runningSessionWidth, false),
		formatCell("EVENT", runningEventWidth, false),
	}, " ")
	return "│   " + colorize(header, ansiGray)
}

func runningTableSeparatorRow(runningEventWidth int) string {
	separatorWidth := runningIDWidth + runningStageWidth + runningPIDWidth + runningAgeWidth + runningTokensWidth + runningSessionWidth + runningEventWidth + 6
	return "│   " + colorize(strings.Repeat("─", separatorWidth), ansiGray)
}

func runningEventWidth(terminalColumns int) int {
	if terminalColumns <= 0 {
		terminalColumns = detectTerminalColumns()
	}
	return max(runningEventMinWidth, terminalColumns-fixedRunningWidth()-runningRowChromeWidth)
}

func fixedRunningWidth() int {
	return runningIDWidth + runningStageWidth + runningPIDWidth + runningAgeWidth + runningTokensWidth + runningSessionWidth
}

func detectTerminalColumns() int {
	value := strings.TrimSpace(os.Getenv("COLUMNS"))
	if value == "" {
		return fixedRunningWidth() + runningRowChromeWidth + runningEventDefaultWidth
	}
	columns, err := strconv.Atoi(value)
	if err != nil || columns <= 0 {
		return defaultTerminalColumns
	}
	return columns
}

func formatCell(value string, width int, right bool) string {
	normalized := strings.ReplaceAll(value, "\n", " ")
	normalized = whitespaceRegex.ReplaceAllString(normalized, " ")
	normalized = strings.TrimSpace(normalized)
	normalized = truncatePlain(normalized, width)
	if right {
		return fmt.Sprintf("%*s", width, normalized)
	}
	return fmt.Sprintf("%-*s", width, normalized)
}

func truncatePlain(value string, width int) string {
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 3 {
		return string(runes[:max(width, 0)])
	}
	return string(runes[:width-3]) + "..."
}

func compactSessionID(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return "n/a"
	}
	runes := []rune(sessionID)
	if len(runes) <= 10 {
		return sessionID
	}
	prefix := string(runes[:4])
	suffix := string(runes[len(runes)-6:])
	return prefix + "..." + suffix
}

func groupThousands(value string) string {
	sign := ""
	unsigned := value
	if strings.HasPrefix(value, "-") {
		sign = "-"
		unsigned = strings.TrimPrefix(value, "-")
	}
	if unsigned == "" {
		return sign
	}

	runes := []rune(unsigned)
	out := make([]rune, 0, len(runes)+len(runes)/3)
	for i, r := range runes {
		out = append(out, r)
		remaining := len(runes) - i - 1
		if remaining > 0 && remaining%3 == 0 {
			out = append(out, ',')
		}
	}
	return sign + string(out)
}

func formatTPS(value float64) string {
	return groupThousands(strconv.Itoa(int(value)))
}

func formatRateLimits(rateLimits any) string {
	if rateLimits == nil {
		return colorize("unavailable", ansiGray)
	}
	mapped := mapOf(rateLimits)
	if len(mapped) == 0 {
		return colorize("unavailable", ansiGray)
	}

	limitID := anyToString(firstNonNil(
		mapped["limit_id"],
		mapped["limit_name"],
	))
	if limitID == "" {
		limitID = "unknown"
	}
	primary := formatRateLimitBucket(firstNonNil(mapped["primary"]))
	secondary := formatRateLimitBucket(firstNonNil(mapped["secondary"]))
	credits := formatRateLimitCredits(firstNonNil(mapped["credits"]))

	return colorize(limitID, ansiYellow) +
		colorize(" | ", ansiGray) +
		colorize("primary "+primary, ansiCyan) +
		colorize(" | ", ansiGray) +
		colorize("secondary "+secondary, ansiCyan) +
		colorize(" | ", ansiGray) +
		colorize(credits, ansiGreen)
}

func formatRateLimitBucket(bucket any) string {
	mapped := mapOf(bucket)
	if len(mapped) == 0 {
		if bucket == nil {
			return "n/a"
		}
		return fmt.Sprint(bucket)
	}
	remaining := parseInteger(firstNonNil(mapped["remaining"]))
	limit := parseInteger(firstNonNil(mapped["limit"]))
	reset := firstNonNil(
		mapped["reset_in_seconds"],
		mapped["resetInSeconds"],
		mapped["reset_at"],
		mapped["resetAt"],
		mapped["resets_at"],
		mapped["resetsAt"],
	)

	base := "n/a"
	switch {
	case remaining != nil && limit != nil:
		base = fmt.Sprintf("%s/%s", formatCount(*remaining), formatCount(*limit))
	case remaining != nil:
		base = "remaining " + formatCount(*remaining)
	case limit != nil:
		base = "limit " + formatCount(*limit)
	default:
		base = truncate(formatStructured(mapped), 40)
	}

	if reset == nil {
		return base
	}
	return base + " reset " + formatResetValue(reset)
}

func formatRateLimitCredits(credits any) string {
	mapped := mapOf(credits)
	if len(mapped) == 0 {
		if credits == nil {
			return "credits n/a"
		}
		return "credits " + fmt.Sprint(credits)
	}
	if boolValue(mapped["unlimited"]) {
		return "credits unlimited"
	}
	hasCredits := boolValue(mapped["has_credits"])
	balance := firstNonNil(mapped["balance"])
	if hasCredits {
		if balance != nil {
			return "credits " + formatNumber(balance)
		}
		return "credits available"
	}
	return "credits none"
}

func formatResetValue(value any) string {
	if n := parseInteger(value); n != nil {
		return formatCount(*n) + "s"
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func formatNumber(value any) string {
	switch typed := value.(type) {
	case int:
		return formatCount(typed)
	case int64:
		return formatCount(typed)
	case float64:
		return fmt.Sprintf("%.2f", typed)
	default:
		return fmt.Sprint(value)
	}
}

func statusDot(colorCode string) string {
	return colorize("●", colorCode)
}

func snapshotTotalTokens(snapshotData map[string]any) int {
	if snapshotData == nil {
		return 0
	}
	codexTotals := mapOf(snapshotData["codex_totals"])
	return max(intValue(codexTotals["total_tokens"]), 0)
}

func normalizeStatusLines(content string) string {
	return content
}

func closingBorder() string {
	return "╰─"
}

func colorize(value string, code string) string {
	return code + value + ansiReset
}

func parseInteger(value any) *int {
	switch typed := value.(type) {
	case int:
		v := typed
		return &v
	case int64:
		v := int(typed)
		return &v
	case float64:
		v := int(typed)
		return &v
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil
		}
		n, err := strconv.Atoi(trimmed)
		if err != nil {
			return nil
		}
		return &n
	default:
		return nil
	}
}

func normalizedCommand(values ...any) string {
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			if inline := inlineText(typed); inline != "" {
				return inline
			}
		case map[string]any:
			binaryCommand := firstNonNil(
				typed["parsedCmd"],
				typed["command"],
				typed["cmd"],
			)
			args := firstNonNil(
				typed["args"],
				typed["argv"],
			)
			if normalized := normalizedCommand(binaryCommand, args); normalized != "" {
				return normalized
			}
		case []any:
			parts := make([]string, 0, len(typed))
			allStrings := true
			for _, item := range typed {
				text, ok := item.(string)
				if !ok {
					allStrings = false
					break
				}
				parts = append(parts, text)
			}
			if allStrings && len(parts) > 0 {
				return inlineText(strings.Join(parts, " "))
			}
		case []string:
			if len(typed) > 0 {
				return inlineText(strings.Join(typed, " "))
			}
		}
	}
	return ""
}

func humanizeItemType(kind string) string {
	if strings.TrimSpace(kind) == "" {
		return "item"
	}
	re := regexp.MustCompile(`([a-z0-9])([A-Z])`)
	normalized := re.ReplaceAllString(kind, `$1 $2`)
	normalized = strings.ReplaceAll(normalized, "_", " ")
	normalized = strings.ReplaceAll(normalized, "/", " ")
	normalized = strings.ToLower(strings.TrimSpace(normalized))
	if normalized == "" {
		return "item"
	}
	return normalized
}

func humanizeStatus(status string) string {
	normalized := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(status, "_", " "), "-", " ")))
	return normalized
}

func shortID(id string) string {
	runes := []rune(id)
	if len(runes) > 12 {
		return string(runes[:12])
	}
	return id
}

func inlineText(text string) string {
	value := strings.ReplaceAll(text, "\n", " ")
	value = whitespaceRegex.ReplaceAllString(value, " ")
	value = strings.TrimSpace(value)
	value = sanitizeANSIAndControlBytes(value)
	return truncate(value, 80)
}

func sanitizeANSIAndControlBytes(value string) string {
	value = ansiRegex.ReplaceAllString(value, "")
	value = ansiShortRegex.ReplaceAllString(value, "")
	value = controlByteRegex.ReplaceAllString(value, "")
	return value
}

func mapPath(payload any, path ...string) any {
	current := payload
	for _, key := range path {
		mapped, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		value, ok := mapped[key]
		if !ok {
			return nil
		}
		current = value
	}
	return current
}

func mapValue(payload any, keys []string) any {
	mapped := mapOf(payload)
	if len(mapped) == 0 {
		return nil
	}
	for _, key := range keys {
		if value, ok := mapped[key]; ok {
			return value
		}
	}
	return nil
}

func mapOf(value any) map[string]any {
	if mapped, ok := value.(map[string]any); ok {
		return mapped
	}
	return map[string]any{}
}

func mapSlice(value any) []map[string]any {
	switch typed := value.(type) {
	case nil:
		return []map[string]any{}
	case []map[string]any:
		return typed
	case []any:
		out := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			if mapped, ok := item.(map[string]any); ok {
				out = append(out, mapped)
			}
		}
		return out
	default:
		return []map[string]any{}
	}
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return -1
		}
		return n
	default:
		return -1
	}
}

func boolValue(value any) bool {
	typed, ok := value.(bool)
	return ok && typed
}

func anyToString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return ""
	}
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value == nil {
			continue
		}
		if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
			continue
		}
		return value
	}
	return nil
}

func formatStructured(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

func truncate(value string, maxChars int) string {
	runes := []rune(value)
	if len(runes) > maxChars {
		return string(runes[:maxChars]) + "..."
	}
	return value
}

func orDefault(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
