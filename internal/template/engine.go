package template

import (
	"fmt"

	"github.com/osteele/liquid"
)

type Engine interface {
	Render(templateText string, vars map[string]any) (string, error)
}

type liquidEngine struct {
	engine *liquid.Engine
}

func NewLiquidEngine() Engine {
	engine := liquid.NewEngine()
	engine.StrictVariables()
	return &liquidEngine{
		engine: engine,
	}
}

func (e *liquidEngine) Render(templateText string, vars map[string]any) (string, error) {
	tpl, err := e.engine.ParseString(templateText)
	if err != nil {
		return "", fmt.Errorf("template_parse_error: %w template=%q", err, templateText)
	}

	rendered, err := tpl.RenderString(vars)
	if err != nil {
		return "", fmt.Errorf("template_render_error: %w", err)
	}

	return rendered, nil
}
