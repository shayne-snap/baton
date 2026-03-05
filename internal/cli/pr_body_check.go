package cli

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

var prBodyTemplatePaths = []string{
	".github/pull_request_template.md",
	"../.github/pull_request_template.md",
}

const prBodyCheckHelp = `Validates a PR description markdown file against the structure and expectations
implied by the repository pull request template.

Usage:

    mix pr_body.check --file /path/to/pr_body.md
`

var prHeadingPattern = regexp.MustCompile(`(?m)^#{4,6}\s+.+$`)
var prBulletPattern = regexp.MustCompile(`(?m)^- `)
var prTemplateCheckboxPattern = regexp.MustCompile(`(?m)^- \[ \] `)
var prBodyCheckboxPattern = regexp.MustCompile(`(?m)^- \[[ xX]\] `)

type prBodyCheckOptions struct {
	file    string
	help    bool
	invalid []invalidOption
}

func newPRBodyCheckCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "pr_body.check",
		Short:              "Validate PR body format against the repository PR template",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := parsePRBodyCheckArgs(args)
			if opts.help {
				_, err := fmt.Fprint(cmd.OutOrStdout(), prBodyCheckHelp)
				return err
			}
			if len(opts.invalid) > 0 {
				return fmt.Errorf("Invalid option(s): %s", formatInvalidOptions(opts.invalid))
			}
			if strings.TrimSpace(opts.file) == "" {
				return fmt.Errorf("Missing required option --file")
			}

			templatePath, template, err := readPRTemplate()
			if err != nil {
				return err
			}

			bodyRaw, err := os.ReadFile(opts.file)
			if err != nil {
				return fmt.Errorf("Unable to read %s: %v", opts.file, err)
			}
			body := string(bodyRaw)

			headings := prHeadingPattern.FindAllString(template, -1)
			if len(headings) == 0 {
				return fmt.Errorf("No markdown headings found in %s", templatePath)
			}

			errors := lintPRBody(template, body, headings)
			if len(errors) == 0 {
				_, writeErr := fmt.Fprintln(cmd.OutOrStdout(), "PR body format OK")
				return writeErr
			}

			for _, issue := range errors {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "ERROR: %s\n", issue)
			}
			return fmt.Errorf("PR body format invalid. Read `%s` and follow it precisely.", templatePath)
		},
	}

	return cmd
}

func parsePRBodyCheckArgs(args []string) prBodyCheckOptions {
	out := prBodyCheckOptions{}

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch {
		case arg == "--help" || arg == "-h":
			out.help = true
		case arg == "--file":
			if i+1 >= len(args) {
				out.invalid = append(out.invalid, invalidOption{Option: arg})
				continue
			}
			i++
			out.file = args[i]
		case strings.HasPrefix(arg, "--file="):
			out.file = strings.TrimPrefix(arg, "--file=")
		case strings.HasPrefix(arg, "-"):
			out.invalid = append(out.invalid, invalidOption{Option: arg})
		default:
			// Ignore positional argv to mirror OptionParser.parse behavior.
		}
	}

	return out
}

func readPRTemplate() (string, string, error) {
	for _, path := range prBodyTemplatePaths {
		raw, err := os.ReadFile(path)
		if err == nil {
			return path, string(raw), nil
		}
	}
	return "", "", fmt.Errorf(
		"Unable to read PR template from any of: %s",
		strings.Join(prBodyTemplatePaths, ", "),
	)
}

func lintPRBody(template string, body string, headings []string) []string {
	errors := make([]string, 0, 8)
	errors = checkRequiredHeadings(errors, body, headings)
	errors = checkHeadingOrder(errors, body, headings)
	errors = checkNoPlaceholders(errors, body)
	errors = checkSectionsFromTemplate(errors, template, body, headings)
	return errors
}

func checkRequiredHeadings(errors []string, body string, headings []string) []string {
	for _, heading := range headings {
		if headingPosition(body, heading) < 0 {
			errors = append(errors, fmt.Sprintf("Missing required heading: %s", heading))
		}
	}
	return errors
}

func checkHeadingOrder(errors []string, body string, headings []string) []string {
	positions := make([]int, 0, len(headings))
	for _, heading := range headings {
		if pos := headingPosition(body, heading); pos >= 0 {
			positions = append(positions, pos)
		}
	}
	for i := 1; i < len(positions); i++ {
		if positions[i] < positions[i-1] {
			return append(errors, "Required headings are out of order.")
		}
	}
	return errors
}

func checkNoPlaceholders(errors []string, body string) []string {
	if strings.Contains(body, "<!--") {
		errors = append(
			errors,
			"PR description still contains template placeholder comments (<!-- ... -->).",
		)
	}
	return errors
}

func checkSectionsFromTemplate(
	errors []string,
	template string,
	body string,
	headings []string,
) []string {
	for _, heading := range headings {
		templateSection := captureHeadingSection(template, heading, headings)
		bodySection := captureHeadingSection(body, heading, headings)

		if bodySection == nil {
			continue
		}
		if strings.TrimSpace(*bodySection) == "" {
			errors = append(errors, fmt.Sprintf("Section cannot be empty: %s", heading))
			continue
		}

		if templateSection != nil &&
			prBulletPattern.MatchString(*templateSection) &&
			!prBulletPattern.MatchString(*bodySection) {
			errors = append(errors, fmt.Sprintf("Section must include at least one bullet item: %s", heading))
		}
		if templateSection != nil &&
			prTemplateCheckboxPattern.MatchString(*templateSection) &&
			!prBodyCheckboxPattern.MatchString(*bodySection) {
			errors = append(errors, fmt.Sprintf("Section must include at least one checkbox item: %s", heading))
		}
	}
	return errors
}

func headingPosition(body string, heading string) int {
	return strings.Index(body, heading)
}

func captureHeadingSection(doc string, heading string, headings []string) *string {
	headingIndex := strings.Index(doc, heading)
	if headingIndex < 0 {
		return nil
	}

	sectionStart := headingIndex + len(heading)
	if sectionStart+2 > len(doc) {
		empty := ""
		return &empty
	}
	if doc[sectionStart:sectionStart+2] != "\n\n" {
		return nil
	}

	content := doc[sectionStart+2:]
	if offset := nextHeadingOffset(content, heading, headings); offset >= 0 {
		section := content[:offset]
		return &section
	}
	return &content
}

func nextHeadingOffset(content string, heading string, headings []string) int {
	minOffset := -1
	for _, marker := range headingsAfter(heading, headings) {
		offset := strings.Index(content, marker)
		if offset < 0 {
			continue
		}
		if minOffset < 0 || offset < minOffset {
			minOffset = offset
		}
	}
	return minOffset
}

func headingsAfter(current string, headings []string) []string {
	out := make([]string, 0, len(headings))
	for _, heading := range headings {
		if heading == current {
			continue
		}
		out = append(out, "\n"+heading)
	}
	return out
}
