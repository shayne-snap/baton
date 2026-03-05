package specscheck

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMissingPublicSpecsReportsMissingSpec(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "sample.ex", `defmodule Sample do
  def missing(arg), do: arg
end
`)

	findings, err := MissingPublicSpecs([]string{dir}, nil)
	if err != nil {
		t.Fatalf("MissingPublicSpecs failed: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if got := FindingIdentifier(findings[0]); got != "Sample.missing/1" {
		t.Fatalf("unexpected finding: %s", got)
	}
}

func TestMissingPublicSpecsAcceptsAdjacentSpec(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "sample.ex", `defmodule Sample do
  @spec ok(term()) :: term()
  def ok(arg), do: arg
end
`)

	findings, err := MissingPublicSpecs([]string{dir}, nil)
	if err != nil {
		t.Fatalf("MissingPublicSpecs failed: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected no findings, got %#v", findings)
	}
}

func TestMissingPublicSpecsAllowsDefpWithoutSpec(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "sample.ex", `defmodule Sample do
  def public do
    helper(:ok)
  end

  defp helper(value), do: value
end
`)

	findings, err := MissingPublicSpecs([]string{dir}, nil)
	if err != nil {
		t.Fatalf("MissingPublicSpecs failed: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if got := FindingIdentifier(findings[0]); got != "Sample.public/0" {
		t.Fatalf("unexpected finding: %s", got)
	}
}

func TestMissingPublicSpecsHonorsImplAndExemptions(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "worker.ex", `defmodule Worker do
  @impl true
  def init(state), do: {:ok, state}
end
`)
	writeModule(t, dir, "sample.ex", `defmodule Sample do
  def legacy(arg), do: arg
end
`)

	findings, err := MissingPublicSpecs([]string{dir}, map[string]struct{}{
		"Sample.legacy/1": {},
	})
	if err != nil {
		t.Fatalf("MissingPublicSpecs failed: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected no findings, got %#v", findings)
	}
}

func writeModule(t *testing.T, root string, rel string, source string) {
	t.Helper()

	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir module dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatalf("write module: %v", err)
	}
}
