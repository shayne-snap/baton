package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const prTemplateFixture = `#### Context

<!-- Why is this change needed? -->

#### TL;DR

*<!-- A short summary -->*

#### Summary

- <!-- Summary bullet -->

#### Alternatives

- <!-- Alternative bullet -->

#### Test Plan

- [ ] <!-- Test checkbox -->
`

const prValidBodyFixture = `#### Context

Context text.

#### TL;DR

Short summary.

#### Summary

- First change.

#### Alternatives

- Alternative considered.

#### Test Plan

- [x] Ran targeted checks.
`

func TestPRBodyCheckPrintsHelp(t *testing.T) {
	cmd := newPRBodyCheckCommand()
	stdout, _, err := executeCLICommand(cmd, []string{"--help"})
	if err != nil {
		t.Fatalf("execute help: %v", err)
	}
	if !strings.Contains(stdout, "mix pr_body.check --file /path/to/pr_body.md") {
		t.Fatalf("unexpected help output: %q", stdout)
	}
}

func TestPRBodyCheckFailsOnInvalidOptions(t *testing.T) {
	cmd := newPRBodyCheckCommand()
	_, _, err := executeCLICommand(cmd, []string{"lint", "--wat"})
	if err == nil {
		t.Fatal("expected invalid options error")
	}
	if !strings.Contains(err.Error(), "Invalid option(s):") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPRBodyCheckFailsWithoutFileOption(t *testing.T) {
	cmd := newPRBodyCheckCommand()
	_, _, err := executeCLICommand(cmd, []string{})
	if err == nil {
		t.Fatal("expected missing file error")
	}
	if !strings.Contains(err.Error(), "Missing required option --file") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPRBodyCheckFailsWhenTemplateMissing(t *testing.T) {
	inTempRepo(t, func(root string) {
		bodyPath := filepath.Join(root, "body.md")
		if err := os.WriteFile(bodyPath, []byte(prValidBodyFixture), 0o644); err != nil {
			t.Fatalf("write body: %v", err)
		}

		cmd := newPRBodyCheckCommand()
		_, _, err := executeCLICommand(cmd, []string{"--file", bodyPath})
		if err == nil {
			t.Fatal("expected missing template error")
		}
		if !strings.Contains(err.Error(), "Unable to read PR template") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestPRBodyCheckFailsOnInvalidBody(t *testing.T) {
	inTempRepo(t, func(root string) {
		writePRTemplate(t, root, prTemplateFixture)
		bodyPath := filepath.Join(root, "body.md")
		invalidBody := strings.Replace(prValidBodyFixture, "#### Alternatives\n\n- Alternative considered.\n\n", "", 1)
		if err := os.WriteFile(bodyPath, []byte(invalidBody), 0o644); err != nil {
			t.Fatalf("write body: %v", err)
		}

		cmd := newPRBodyCheckCommand()
		_, stderr, err := executeCLICommand(cmd, []string{"--file", bodyPath})
		if err == nil {
			t.Fatal("expected invalid body error")
		}
		if !strings.Contains(err.Error(), "PR body format invalid") {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(stderr, "Missing required heading: #### Alternatives") {
			t.Fatalf("unexpected stderr: %q", stderr)
		}
	})
}

func TestPRBodyCheckPassesForValidBody(t *testing.T) {
	inTempRepo(t, func(root string) {
		writePRTemplate(t, root, prTemplateFixture)
		bodyPath := filepath.Join(root, "body.md")
		if err := os.WriteFile(bodyPath, []byte(prValidBodyFixture), 0o644); err != nil {
			t.Fatalf("write body: %v", err)
		}

		cmd := newPRBodyCheckCommand()
		stdout, stderr, err := executeCLICommand(cmd, []string{"--file", bodyPath})
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if !strings.Contains(stdout, "PR body format OK") {
			t.Fatalf("unexpected stdout: %q", stdout)
		}
		if stderr != "" {
			t.Fatalf("expected empty stderr, got %q", stderr)
		}
	})
}

func executeCLICommand(cmd *cobra.Command, args []string) (string, string, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func inTempRepo(t *testing.T, fn func(root string)) {
	t.Helper()

	root := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir temp repo: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(original)
	})

	fn(root)
}

func writePRTemplate(t *testing.T, root string, content string) {
	t.Helper()

	templateDir := filepath.Join(root, ".github")
	if err := os.MkdirAll(templateDir, 0o755); err != nil {
		t.Fatalf("mkdir template dir: %v", err)
	}
	templatePath := filepath.Join(templateDir, "pull_request_template.md")
	if err := os.WriteFile(templatePath, []byte(content), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}
}
