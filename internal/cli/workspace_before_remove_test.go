package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestWorkspaceBeforeRemovePrintsHelp(t *testing.T) {
	cmd := newWorkspaceBeforeRemoveCommand()
	stdout, _, err := executeCommand(cmd, []string{"--help"})
	if err != nil {
		t.Fatalf("execute help: %v", err)
	}
	if !strings.Contains(stdout, "mix workspace.before_remove") {
		t.Fatalf("expected help output, got %q", stdout)
	}
}

func TestWorkspaceBeforeRemoveInvalidOption(t *testing.T) {
	cmd := newWorkspaceBeforeRemoveCommand()
	_, _, err := executeCommand(cmd, []string{"--wat"})
	if err == nil {
		t.Fatal("expected invalid option error")
	}
	if !strings.Contains(err.Error(), "Invalid option(s):") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWorkspaceBeforeRemoveNoOpWhenBranchUnavailable(t *testing.T) {
	t.Setenv("PATH", "")
	cmd := newWorkspaceBeforeRemoveCommand()
	stdout, stderr, err := executeCommand(cmd, []string{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("expected no output, got stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestWorkspaceBeforeRemoveUsesCurrentBranchAndClosesPRs(t *testing.T) {
	withFakeBinaries(t, map[string]string{
		"gh": `#!/bin/sh
printf '%s\n' "$*" >> "$GH_LOG"
if [ "$1" = "auth" ] && [ "$2" = "status" ]; then exit 0; fi
if [ "$1" = "pr" ] && [ "$2" = "list" ]; then printf '101\n102\n'; exit 0; fi
if [ "$1" = "pr" ] && [ "$2" = "close" ] && [ "$3" = "101" ]; then exit 0; fi
if [ "$1" = "pr" ] && [ "$2" = "close" ] && [ "$3" = "102" ]; then printf 'boom\n' >&2; exit 17; fi
exit 99
`,
		"git": `#!/bin/sh
printf 'feature/workpad\n'
exit 0
`,
	}, func(logPath string) {
		cmd := newWorkspaceBeforeRemoveCommand()
		stdout, stderr, err := executeCommand(cmd, []string{})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if !strings.Contains(stdout, "Closed PR #101 for branch feature/workpad") {
			t.Fatalf("unexpected stdout: %q", stdout)
		}
		if !strings.Contains(stderr, "Failed to close PR #102 for branch feature/workpad") {
			t.Fatalf("unexpected stderr: %q", stderr)
		}

		raw, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("read log: %v", err)
		}
		log := string(raw)
		if !strings.Contains(log, "pr list --repo openai/baton --head feature/workpad --state open --json number --jq .[].number") {
			t.Fatalf("missing list command in log: %q", log)
		}
		if !strings.Contains(log, "pr close 101 --repo openai/baton") {
			t.Fatalf("missing close 101 in log: %q", log)
		}
		if !strings.Contains(log, "pr close 102 --repo openai/baton") {
			t.Fatalf("missing close 102 in log: %q", log)
		}
	})
}

func TestWorkspaceBeforeRemoveNoOpWhenGHAuthUnavailable(t *testing.T) {
	withFakeBinaries(t, map[string]string{
		"gh": `#!/bin/sh
printf '%s\n' "$*" >> "$GH_LOG"
if [ "$1" = "auth" ] && [ "$2" = "status" ]; then exit 1; fi
exit 99
`,
	}, func(logPath string) {
		cmd := newWorkspaceBeforeRemoveCommand()
		stdout, stderr, err := executeCommand(cmd, []string{"--branch", "feature/no-auth"})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if stdout != "" || stderr != "" {
			t.Fatalf("expected no output, got stdout=%q stderr=%q", stdout, stderr)
		}

		raw, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("read log: %v", err)
		}
		log := string(raw)
		if !strings.Contains(log, "auth status") {
			t.Fatalf("missing auth call in log: %q", log)
		}
		if strings.Contains(log, "pr list") {
			t.Fatalf("unexpected pr list call in log: %q", log)
		}
	})
}

func executeCommand(cmd *cobra.Command, args []string) (string, string, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func withFakeBinaries(t *testing.T, scripts map[string]string, fn func(logPath string)) {
	t.Helper()

	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	logPath := filepath.Join(root, "gh.log")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin dir: %v", err)
	}
	if err := os.WriteFile(logPath, []byte(""), 0o644); err != nil {
		t.Fatalf("write gh.log: %v", err)
	}

	for name, script := range scripts {
		path := filepath.Join(binDir, name)
		content := script
		if !strings.HasPrefix(content, "#!/bin/sh\n") {
			content = "#!/bin/sh\n" + strings.TrimSpace(content) + "\n"
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}

	t.Setenv("GH_LOG", logPath)
	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+":"+originalPath)
	fn(logPath)
}
