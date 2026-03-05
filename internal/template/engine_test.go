package template

import "testing"

func TestLiquidEngineStrictVariableRendering(t *testing.T) {
	t.Parallel()

	engine := NewLiquidEngine()
	_, err := engine.Render("Work on {{ missing.ticket_id }}", map[string]any{
		"issue": map[string]any{"identifier": "MT-1"},
	})
	if err == nil {
		t.Fatal("expected strict variable rendering error")
	}
}

func TestLiquidEngineTemplateParseError(t *testing.T) {
	t.Parallel()

	engine := NewLiquidEngine()
	_, err := engine.Render("{% if issue.identifier %}", map[string]any{
		"issue": map[string]any{"identifier": "MT-1"},
	})
	if err == nil {
		t.Fatal("expected template parse error")
	}
}
