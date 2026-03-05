package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultLogFileUsesCurrentWorkingDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd failed: %v", err)
	}
	expected := filepath.Join(cwd, "log", "symphony.log")
	if got := DefaultLogFile(""); got != expected {
		t.Fatalf("unexpected default log file path: got=%s expected=%s", got, expected)
	}
}

func TestDefaultLogFileUnderCustomRoot(t *testing.T) {
	root := "/tmp/symphony-logs"
	expected := "/tmp/symphony-logs/log/symphony.log"
	if got := DefaultLogFile(root); got != expected {
		t.Fatalf("unexpected custom log file path: got=%s expected=%s", got, expected)
	}
}

func TestRotatingFileWriterRotatesBySize(t *testing.T) {
	temp := t.TempDir()
	path := filepath.Join(temp, "log", "symphony.log")

	writer, err := NewRotatingFileWriter(path, 32, 3)
	if err != nil {
		t.Fatalf("new rotating writer failed: %v", err)
	}
	defer writer.Close()

	for i := 0; i < 10; i++ {
		if _, err := writer.Write([]byte(strings.Repeat("x", 12))); err != nil {
			t.Fatalf("write failed: %v", err)
		}
	}

	for _, check := range []string{
		path,
		path + ".1",
		path + ".2",
		path + ".3",
	} {
		if _, err := os.Stat(check); err != nil {
			t.Fatalf("expected rotated file %s to exist: %v", check, err)
		}
	}
}
