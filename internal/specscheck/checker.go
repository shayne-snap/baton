package specscheck

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Finding struct {
	File   string
	Module string
	Name   string
	Arity  int
	Line   int
}

func FindingIdentifier(f Finding) string {
	return fmt.Sprintf("%s.%s/%d", strings.TrimSpace(f.Module), strings.TrimSpace(f.Name), f.Arity)
}

func MissingPublicSpecs(paths []string, exemptions map[string]struct{}) ([]Finding, error) {
	if exemptions == nil {
		exemptions = map[string]struct{}{}
	}

	files := make([]string, 0, 32)
	for _, path := range paths {
		collected, err := collectElixirFiles(path)
		if err != nil {
			return nil, err
		}
		files = append(files, collected...)
	}

	findings := make([]Finding, 0, 32)
	for _, file := range files {
		fileFindings, err := fileFindings(file, exemptions)
		if err != nil {
			return nil, err
		}
		findings = append(findings, fileFindings...)
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		if findings[i].Name != findings[j].Name {
			return findings[i].Name < findings[j].Name
		}
		return findings[i].Arity < findings[j].Arity
	})

	return findings, nil
}

func collectElixirFiles(path string) ([]string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return []string{}, nil
	}

	info, err := os.Stat(trimmed)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	if !info.IsDir() {
		if strings.HasSuffix(trimmed, ".ex") {
			return []string{trimmed}, nil
		}
		return []string{}, nil
	}

	files := make([]string, 0, 32)
	walkErr := filepath.WalkDir(trimmed, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".ex") {
			files = append(files, path)
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return files, nil
}

func fileFindings(path string, exemptions map[string]struct{}) ([]Finding, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	moduleName := moduleNameFromSource(string(raw))
	lines := strings.Split(string(raw), "\n")

	pendingSpecs := map[string]struct{}{}
	pendingImpl := false
	seenDefs := map[string]struct{}{}
	findings := make([]Finding, 0, 8)

	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNo := index + 1

		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if strings.HasPrefix(trimmed, "@spec ") {
			for _, id := range extractSpecIdentifiers(trimmed) {
				pendingSpecs[id] = struct{}{}
			}
			continue
		}

		if strings.HasPrefix(trimmed, "@impl") {
			pendingImpl = true
			continue
		}

		if strings.HasPrefix(trimmed, "@") {
			continue
		}

		if name, arity, ok := parseDefIdentifier(trimmed, "defp"); ok {
			_ = name
			_ = arity
			pendingSpecs = map[string]struct{}{}
			pendingImpl = false
			continue
		}

		if name, arity, ok := parseDefIdentifier(trimmed, "def"); ok {
			id := fmt.Sprintf("%s/%d", name, arity)
			if _, exists := seenDefs[id]; exists {
				pendingSpecs = map[string]struct{}{}
				pendingImpl = false
				continue
			}

			finding := Finding{
				File:   path,
				Module: moduleName,
				Name:   name,
				Arity:  arity,
				Line:   lineNo,
			}

			identifier := FindingIdentifier(finding)
			_, hasPendingSpec := pendingSpecs[id]
			_, exempt := exemptions[identifier]

			if !hasPendingSpec && !pendingImpl && !exempt {
				findings = append(findings, finding)
			}

			seenDefs[id] = struct{}{}
			pendingSpecs = map[string]struct{}{}
			pendingImpl = false
			continue
		}

		pendingSpecs = map[string]struct{}{}
		pendingImpl = false
	}

	return findings, nil
}

var modulePattern = regexp.MustCompile(`(?m)^\s*defmodule\s+([A-Za-z0-9_.]+)\s+do\b`)

func moduleNameFromSource(source string) string {
	matches := modulePattern.FindStringSubmatch(source)
	if len(matches) >= 2 {
		return matches[1]
	}
	return "Unknown"
}

func extractSpecIdentifiers(line string) []string {
	specBody := strings.TrimSpace(strings.TrimPrefix(line, "@spec"))
	if specBody == "" {
		return []string{}
	}

	head := specBody
	if idx := strings.Index(head, "::"); idx >= 0 {
		head = head[:idx]
	}
	if idx := strings.Index(head, " when "); idx >= 0 {
		head = head[:idx]
	}

	name, arity, ok := parseCallableIdentifier(strings.TrimSpace(head))
	if !ok {
		return []string{}
	}

	return []string{fmt.Sprintf("%s/%d", name, arity)}
}

func parseDefIdentifier(line string, keyword string) (string, int, bool) {
	prefix := keyword + " "
	if !strings.HasPrefix(line, prefix) {
		return "", 0, false
	}

	head := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if head == "" {
		return "", 0, false
	}

	if idx := strings.Index(head, ", do:"); idx >= 0 {
		head = head[:idx]
	}
	if idx := strings.Index(head, ",do:"); idx >= 0 {
		head = head[:idx]
	}
	if idx := strings.Index(head, " do"); idx >= 0 {
		head = head[:idx]
	}
	if idx := strings.Index(head, " when "); idx >= 0 {
		head = head[:idx]
	}

	return parseCallableIdentifier(strings.TrimSpace(head))
}

func parseCallableIdentifier(head string) (string, int, bool) {
	if head == "" {
		return "", 0, false
	}

	if open := strings.Index(head, "("); open >= 0 {
		close := strings.LastIndex(head, ")")
		if close < open {
			return "", 0, false
		}
		name := strings.TrimSpace(head[:open])
		if !validFunctionName(name) {
			return "", 0, false
		}
		args := strings.TrimSpace(head[open+1 : close])
		return name, countArgs(args), true
	}

	name := strings.TrimSpace(strings.Fields(head)[0])
	name = strings.TrimSuffix(name, ",")
	if !validFunctionName(name) {
		return "", 0, false
	}
	return name, 0, true
}

var functionNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_!?]*$`)

func validFunctionName(name string) bool {
	return functionNamePattern.MatchString(strings.TrimSpace(name))
}

func countArgs(args string) int {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return 0
	}

	arity := 1
	depthRound := 0
	depthSquare := 0
	depthCurly := 0

	for _, ch := range trimmed {
		switch ch {
		case '(':
			depthRound++
		case ')':
			if depthRound > 0 {
				depthRound--
			}
		case '[':
			depthSquare++
		case ']':
			if depthSquare > 0 {
				depthSquare--
			}
		case '{':
			depthCurly++
		case '}':
			if depthCurly > 0 {
				depthCurly--
			}
		case ',':
			if depthRound == 0 && depthSquare == 0 && depthCurly == 0 {
				arity++
			}
		}
	}

	return arity
}
