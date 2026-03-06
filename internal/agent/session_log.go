package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"baton/internal/logging"
	"baton/internal/runtime"
)

type sessionLogWriter struct {
	mu sync.Mutex

	file       *os.File
	latestFile *os.File
}

func newSessionLogWriter(logsRoot string, issueIdentifier string) *sessionLogWriter {
	if strings.TrimSpace(logsRoot) == "" {
		return &sessionLogWriter{}
	}

	logsDir := logging.CodexIssueLogsDir(logsRoot, issueIdentifier)
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return &sessionLogWriter{}
	}

	now := time.Now().UTC().Format("20060102T150405Z")
	sessionPath := filepath.Join(logsDir, "session-"+now+".log")
	latestPath := filepath.Join(logsDir, "latest.log")

	file, err := os.OpenFile(sessionPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return &sessionLogWriter{}
	}
	latestFile, latestErr := os.OpenFile(latestPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if latestErr != nil {
		_ = file.Close()
		return &sessionLogWriter{}
	}

	return &sessionLogWriter{
		file:       file,
		latestFile: latestFile,
	}
}

func (w *sessionLogWriter) WriteUpdate(update runtime.Update) {
	if w == nil {
		return
	}

	line := map[string]any{
		"timestamp": update.Timestamp.UTC().Format(time.RFC3339Nano),
		"event":     update.Event,
		"payload":   update.Payload,
		"raw":       update.Raw,
		"decision":  update.Decision,
	}
	appServerPID := strings.TrimSpace(update.AppServerPID)
	if appServerPID == "" {
		appServerPID = strings.TrimSpace(update.CodexAppServerPID)
	}
	if appServerPID != "" {
		line["codex_app_server_pid"] = appServerPID
	}

	encoded, err := json.Marshal(line)
	if err != nil {
		return
	}
	encoded = append(encoded, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file != nil {
		_, _ = w.file.Write(encoded)
	}
	if w.latestFile != nil {
		_, _ = w.latestFile.Write(encoded)
	}
}

func (w *sessionLogWriter) Close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	if w.latestFile != nil {
		_ = w.latestFile.Close()
		w.latestFile = nil
	}
}
