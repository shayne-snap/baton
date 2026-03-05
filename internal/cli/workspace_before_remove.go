package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

const defaultWorkspaceBeforeRemoveRepo = "openai/symphony"

const workspaceBeforeRemoveHelp = `Closes open pull requests for the current Git branch.

This task is intended for use from the ` + "`before_remove`" + ` workspace hook.

Usage:

    mix workspace.before_remove
    mix workspace.before_remove --branch feature/my-branch
    mix workspace.before_remove --repo openai/symphony
`

type workspaceBeforeRemoveOptions struct {
	branch  string
	repo    string
	help    bool
	invalid []invalidOption
}

func newWorkspaceBeforeRemoveCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "workspace.before_remove",
		Short:              "Close open GitHub PRs for the current branch before workspace removal",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := parseWorkspaceBeforeRemoveArgs(args)
			if opts.help {
				_, err := fmt.Fprint(cmd.OutOrStdout(), workspaceBeforeRemoveHelp)
				return err
			}
			if len(opts.invalid) > 0 {
				return fmt.Errorf("Invalid option(s): %s", formatInvalidOptions(opts.invalid))
			}

			repo := strings.TrimSpace(opts.repo)
			if repo == "" {
				repo = defaultWorkspaceBeforeRemoveRepo
			}

			branch := strings.TrimSpace(opts.branch)
			if branch == "" {
				branch = currentGitBranch()
			}
			if strings.TrimSpace(branch) == "" {
				return nil
			}

			if !commandAvailable("gh") {
				return nil
			}

			if _, _, err := runCommand("gh", "auth", "status"); err != nil {
				return nil
			}

			numbers, err := listOpenPullRequestNumbers(repo, branch)
			if err != nil {
				return nil
			}

			for _, number := range numbers {
				if err := closePullRequest(cmd, repo, branch, number); err != nil {
					return err
				}
			}

			return nil
		},
	}

	return cmd
}

func parseWorkspaceBeforeRemoveArgs(args []string) workspaceBeforeRemoveOptions {
	out := workspaceBeforeRemoveOptions{}

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch {
		case arg == "--help" || arg == "-h":
			out.help = true
		case arg == "--branch":
			if i+1 >= len(args) {
				out.invalid = append(out.invalid, invalidOption{Option: arg})
				continue
			}
			i++
			out.branch = args[i]
		case strings.HasPrefix(arg, "--branch="):
			out.branch = strings.TrimPrefix(arg, "--branch=")
		case arg == "--repo":
			if i+1 >= len(args) {
				out.invalid = append(out.invalid, invalidOption{Option: arg})
				continue
			}
			i++
			out.repo = args[i]
		case strings.HasPrefix(arg, "--repo="):
			out.repo = strings.TrimPrefix(arg, "--repo=")
		case strings.HasPrefix(arg, "-"):
			out.invalid = append(out.invalid, invalidOption{Option: arg})
		default:
			// Ignore positional argv to mirror OptionParser.parse behavior.
		}
	}

	return out
}

func listOpenPullRequestNumbers(repo string, branch string) ([]string, error) {
	output, _, err := runCommand(
		"gh",
		"pr",
		"list",
		"--repo",
		repo,
		"--head",
		branch,
		"--state",
		"open",
		"--json",
		"number",
		"--jq",
		".[].number",
	)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(output, "\n")
	numbers := make([]string, 0, len(lines))
	for _, line := range lines {
		number := strings.TrimSpace(line)
		if number != "" {
			numbers = append(numbers, number)
		}
	}
	return numbers, nil
}

func closePullRequest(cmd *cobra.Command, repo string, branch string, number string) error {
	output, status, err := runCommand(
		"gh",
		"pr",
		"close",
		number,
		"--repo",
		repo,
		"--comment",
		closingComment(branch),
	)
	if err == nil {
		_, writeErr := fmt.Fprintf(cmd.OutOrStdout(), "Closed PR #%s for branch %s\n", number, branch)
		return writeErr
	}

	message := fmt.Sprintf("Failed to close PR #%s for branch %s: exit %d", number, branch, status)
	trimmedOutput := strings.TrimSpace(output)
	if trimmedOutput != "" {
		message = fmt.Sprintf("%s output=%q", message, trimmedOutput)
	}
	_, writeErr := fmt.Fprintln(cmd.ErrOrStderr(), message)
	return writeErr
}

func closingComment(branch string) string {
	return fmt.Sprintf(
		"Closing because the Linear issue for branch %s entered a terminal state without merge.",
		branch,
	)
}

func currentGitBranch() string {
	output, _, err := runCommand("git", "branch", "--show-current")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

func commandAvailable(command string) bool {
	_, err := exec.LookPath(command)
	return err == nil
}

func runCommand(command string, args ...string) (string, int, error) {
	path, err := exec.LookPath(command)
	if err != nil {
		return "", -1, err
	}

	cmd := exec.Command(path, args...)
	var buffer bytes.Buffer
	cmd.Stdout = &buffer
	cmd.Stderr = &buffer
	err = cmd.Run()
	if err == nil {
		return buffer.String(), 0, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return buffer.String(), exitErr.ExitCode(), err
	}
	return buffer.String(), -1, err
}

type invalidOption struct {
	Option string
	Value  *string
}

func formatInvalidOptions(options []invalidOption) string {
	parts := make([]string, 0, len(options))
	for _, option := range options {
		value := "nil"
		if option.Value != nil {
			value = strconv.Quote(*option.Value)
		}
		parts = append(parts, fmt.Sprintf("{%s, %s}", strconv.Quote(option.Option), value))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
