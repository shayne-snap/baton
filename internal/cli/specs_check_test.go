package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpecsCheckUsesDefaultLibPath(t *testing.T) {
	inTempSpecsProject(t, func(root string) {
		writeSpecsModule(t, root, "lib/sample.ex", `defmodule Sample do
  @spec ok(term()) :: term()
  def ok(arg), do: arg
end
`)

		cmd := newSpecsCheckCommand()
		stdout, stderr, err := executeCLICommand(cmd, []string{})
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if !strings.Contains(stdout, "specs.check: all public functions have @spec or exemption") {
			t.Fatalf("unexpected stdout: %q", stdout)
		}
		if stderr != "" {
			t.Fatalf("expected empty stderr, got %q", stderr)
		}
	})
}

func TestSpecsCheckRaisesOnMissingSpecInExplicitPath(t *testing.T) {
	inTempSpecsProject(t, func(root string) {
		writeSpecsModule(t, root, "src/sample.ex", `defmodule Sample do
  def missing(arg), do: arg
end
`)

		cmd := newSpecsCheckCommand()
		_, stderr, err := executeCLICommand(cmd, []string{"--paths", "src"})
		if err == nil {
			t.Fatal("expected missing spec error")
		}
		if !strings.Contains(err.Error(), "specs.check failed with 1 missing @spec declaration") {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(stderr, "src/sample.ex:2 missing @spec for Sample.missing/1") {
			t.Fatalf("unexpected stderr: %q", stderr)
		}
	})
}

func TestSpecsCheckLoadsExemptionsFile(t *testing.T) {
	inTempSpecsProject(t, func(root string) {
		writeSpecsModule(t, root, "lib/sample.ex", `defmodule Sample do
  def legacy(arg), do: arg
end
`)
		exemptionsPath := filepath.Join(root, "config", "specs_exemptions.txt")
		if err := os.MkdirAll(filepath.Dir(exemptionsPath), 0o755); err != nil {
			t.Fatalf("mkdir config: %v", err)
		}
		if err := os.WriteFile(exemptionsPath, []byte("# existing exemptions\n\nSample.legacy/1\n"), 0o644); err != nil {
			t.Fatalf("write exemptions: %v", err)
		}

		cmd := newSpecsCheckCommand()
		stdout, stderr, err := executeCLICommand(cmd, []string{"--paths", "lib", "--exemptions-file", "config/specs_exemptions.txt"})
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if !strings.Contains(stdout, "specs.check: all public functions have @spec or exemption") {
			t.Fatalf("unexpected stdout: %q", stdout)
		}
		if stderr != "" {
			t.Fatalf("expected empty stderr, got %q", stderr)
		}
	})
}

func inTempSpecsProject(t *testing.T, fn func(root string)) {
	t.Helper()

	root := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir temp project: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(original)
	})

	fn(root)
}

func writeSpecsModule(t *testing.T, root string, relPath string, source string) {
	t.Helper()

	path := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir module dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatalf("write module: %v", err)
	}
}
