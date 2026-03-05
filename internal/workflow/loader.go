package workflow

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	ErrMissingWorkflowFile        = errors.New("missing_workflow_file")
	ErrWorkflowFrontMatterNotAMap = errors.New("workflow_front_matter_not_a_map")
)

type ParseError struct {
	Cause error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("workflow_parse_error: %v", e.Cause)
}

func (e *ParseError) Unwrap() error {
	return e.Cause
}

type Definition struct {
	Config         map[string]any
	PromptTemplate string
}

func LoadFile(path string) (*Definition, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s: %v", ErrMissingWorkflowFile, path, err)
		}
		return nil, fmt.Errorf("read workflow file: %w", err)
	}

	return Parse(raw)
}

func Parse(raw []byte) (*Definition, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return &Definition{
			Config:         map[string]any{},
			PromptTemplate: "",
		}, nil
	}

	config, body, err := splitFrontMatter(string(raw))
	if err != nil {
		return nil, err
	}

	return &Definition{
		Config:         config,
		PromptTemplate: strings.TrimSpace(body),
	}, nil
}

func splitFrontMatter(raw string) (map[string]any, string, error) {
	lines := strings.Split(raw, "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return map[string]any{}, raw, nil
	}

	front := make([]string, 0, len(lines))
	prompt := []string{}
	foundClosingDelimiter := false
	for idx := 1; idx < len(lines); idx++ {
		if lines[idx] == "---" {
			foundClosingDelimiter = true
			if idx+1 < len(lines) {
				prompt = lines[idx+1:]
			}
			break
		}
		front = append(front, lines[idx])
	}
	if !foundClosingDelimiter {
		prompt = []string{}
	}

	yamlText := strings.TrimSpace(strings.Join(front, "\n"))
	if yamlText == "" {
		return map[string]any{}, strings.Join(prompt, "\n"), nil
	}

	var decoded any
	if err := yaml.Unmarshal([]byte(yamlText), &decoded); err != nil {
		return nil, "", &ParseError{Cause: err}
	}

	configMap, ok := normalizeToStringKeyMap(decoded)
	if !ok {
		return nil, "", ErrWorkflowFrontMatterNotAMap
	}

	return configMap, strings.Join(prompt, "\n"), nil
}

func normalizeToStringKeyMap(input any) (map[string]any, bool) {
	switch v := input.(type) {
	case nil:
		return map[string]any{}, true
	case map[string]any:
		return normalizeNestedMap(v), true
	case map[any]any:
		converted := make(map[string]any, len(v))
		for key, value := range v {
			converted[fmt.Sprint(key)] = normalizeNestedValue(value)
		}
		return converted, true
	default:
		return nil, false
	}
}

func normalizeNestedMap(input map[string]any) map[string]any {
	normalized := make(map[string]any, len(input))
	for key, value := range input {
		normalized[key] = normalizeNestedValue(value)
	}
	return normalized
}

func normalizeNestedValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return normalizeNestedMap(typed)
	case map[any]any:
		converted := make(map[string]any, len(typed))
		for key, v := range typed {
			converted[fmt.Sprint(key)] = normalizeNestedValue(v)
		}
		return converted
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, normalizeNestedValue(item))
		}
		return result
	default:
		return typed
	}
}
