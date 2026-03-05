package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"baton/internal/app"
	"baton/internal/logging"

	"github.com/spf13/cobra"
)

const acknowledgementFlag = "i-understand-that-this-will-be-running-without-the-usual-guardrails"

type options struct {
	workflowPath string
	logsRoot     string
	logsRootSet  bool
	port         int
	portSet      bool
	acknowledged bool
}

func NewRootCommand() *cobra.Command {
	opts := &options{}

	cmd := &cobra.Command{
		Use:   "symphony [path-to-WORKFLOW.md]",
		Short: "Symphony is a local orchestration service for coding-agent runs.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return errors.New(usageMessage())
			}
			if len(args) == 1 {
				opts.workflowPath = args[0]
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.logsRootSet = cmd.Flags().Changed("logs-root")
			opts.portSet = cmd.Flags().Changed("port")
			return run(cmd.Context(), opts)
		},
		SilenceUsage: true,
	}

	cmd.Flags().StringVar(&opts.logsRoot, "logs-root", "", "Root directory for logs (default: current working directory)")
	cmd.Flags().IntVar(&opts.port, "port", -1, "Optional HTTP observability API port")
	cmd.Flags().BoolVar(&opts.acknowledged, acknowledgementFlag, false, "Acknowledge running without usual guardrails")
	cmd.AddCommand(
		newWorkspaceBeforeRemoveCommand(),
		newPRBodyCheckCommand(),
		newSpecsCheckCommand(),
	)

	return cmd
}

func run(parent context.Context, opts *options) error {
	if !opts.acknowledged {
		return errors.New(acknowledgementBanner())
	}
	if opts.logsRootSet && strings.TrimSpace(opts.logsRoot) == "" {
		return errors.New(usageMessage())
	}
	if opts.portSet && opts.port < 0 {
		return errors.New(usageMessage())
	}

	workflowPath := strings.TrimSpace(opts.workflowPath)
	if workflowPath == "" {
		workflowPath = "WORKFLOW.md"
	}
	workflowPath = filepath.Clean(workflowPath)
	expandedPath, err := filepath.Abs(workflowPath)
	if err != nil {
		return err
	}
	info, statErr := os.Stat(expandedPath)
	if statErr != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("Workflow file not found: %s", expandedPath)
	}

	logger := logging.NewDefault(logging.Options{
		LogsRoot: opts.logsRoot,
	})
	service, err := app.New(app.Options{
		WorkflowPath: expandedPath,
		LogsRoot:     opts.logsRoot,
		Port:         opts.port,
		Logger:       logger,
	})
	if err != nil {
		return fmt.Errorf("Failed to start Symphony with workflow %s: %v", expandedPath, err)
	}

	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	return service.Run(ctx)
}

func usageMessage() string {
	return "Usage: symphony [--logs-root <path>] [--port <port>] [path-to-WORKFLOW.md]"
}

func acknowledgementBanner() string {
	lines := []string{
		"This Symphony implementation is a low key engineering preview.",
		"Codex will run without any guardrails.",
		"SymphonyElixir is not a supported product and is presented as-is.",
		fmt.Sprintf("To proceed, start with `--%s` CLI argument", acknowledgementFlag),
	}

	width := 0
	for _, line := range lines {
		if len(line) > width {
			width = len(line)
		}
	}
	border := strings.Repeat("─", width+2)
	output := []string{
		"╭" + border + "╮",
		"│ " + strings.Repeat(" ", width) + " │",
	}
	for _, line := range lines {
		output = append(output, "│ "+line+strings.Repeat(" ", width-len(line))+" │")
	}
	output = append(output, "│ "+strings.Repeat(" ", width)+" │")
	output = append(output, "╰"+border+"╯")
	return "\x1b[31m\x1b[1m" + strings.Join(output, "\n") + "\x1b[0m"
}
