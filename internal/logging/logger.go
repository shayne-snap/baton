package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const (
	defaultLogRelativePath = "log/symphony.log"
	defaultMaxBytes        = int64(10 * 1024 * 1024)
	defaultMaxFiles        = 5
)

type Options struct {
	LogsRoot string
	MaxBytes int64
	MaxFiles int
}

func DefaultLogFile(logsRoot string) string {
	root := ResolveLogsRoot(logsRoot)
	return filepath.Join(root, defaultLogRelativePath)
}

func ResolveLogsRoot(logsRoot string) string {
	root := strings.TrimSpace(logsRoot)
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}
		root = cwd
	}
	return root
}

func SafePathComponent(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "issue"
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '.' || r == '_' || r == '-':
			return r
		default:
			return '_'
		}
	}, trimmed)
	safe = strings.Trim(safe, "_")
	if safe == "" {
		return "issue"
	}
	return safe
}

func CodexIssueLogsDir(logsRoot string, issueIdentifier string) string {
	return filepath.Join(ResolveLogsRoot(logsRoot), "log", "codex", SafePathComponent(issueIdentifier))
}

func NewDefault(opts Options) zerolog.Logger {
	output := newLogOutput(opts)
	return zerolog.New(output).
		With().
		Timestamp().
		Logger().
		Level(zerolog.InfoLevel)
}

func SetGlobalDefaults() {
	zerolog.TimeFieldFormat = time.RFC3339
}

func newLogOutput(opts Options) io.Writer {
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = defaultMaxFiles
	}

	path := DefaultLogFile(opts.LogsRoot)
	writer, err := NewRotatingFileWriter(path, maxBytes, maxFiles)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to configure rotating log sink at %s: %v; falling back to stdout\n", path, err)
		return os.Stdout
	}
	return writer
}

type RotatingFileWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	maxFiles int

	file *os.File
	size int64
}

func NewRotatingFileWriter(path string, maxBytes int64, maxFiles int) (*RotatingFileWriter, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("log path is required")
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	if maxFiles <= 0 {
		maxFiles = defaultMaxFiles
	}

	writer := &RotatingFileWriter{
		path:     filepath.Clean(path),
		maxBytes: maxBytes,
		maxFiles: maxFiles,
	}
	if err := writer.openFile(); err != nil {
		return nil, err
	}
	return writer, nil
}

func (w *RotatingFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		if err := w.openFile(); err != nil {
			return 0, err
		}
	}

	if w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *RotatingFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	w.size = 0
	return err
}

func (w *RotatingFileWriter) openFile() error {
	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return err
	}

	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *RotatingFileWriter) rotate() error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return err
		}
		w.file = nil
	}

	oldest := fmt.Sprintf("%s.%d", w.path, w.maxFiles)
	_ = os.Remove(oldest)

	for i := w.maxFiles - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.path, i)
		dst := fmt.Sprintf("%s.%d", w.path, i+1)
		if _, err := os.Stat(src); err == nil {
			if err := os.Rename(src, dst); err != nil {
				return err
			}
		}
	}

	if _, err := os.Stat(w.path); err == nil {
		if err := os.Rename(w.path, fmt.Sprintf("%s.1", w.path)); err != nil {
			return err
		}
	}

	return w.openFile()
}
