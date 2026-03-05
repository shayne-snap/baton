package cli

import (
	"fmt"
	"os"
	"strings"

	"baton/internal/specscheck"

	"github.com/spf13/cobra"
)

type specsCheckOptions struct {
	paths          []string
	exemptionsFile string
}

func newSpecsCheckCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "specs.check",
		Short:              "Fails when public functions in lib/ are missing adjacent @specs",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := parseSpecsCheckArgs(args)
			if len(opts.paths) == 0 {
				opts.paths = []string{"lib"}
			}

			exemptions, err := loadSpecsExemptions(opts.exemptionsFile)
			if err != nil {
				return err
			}

			findings, err := specscheck.MissingPublicSpecs(opts.paths, exemptions)
			if err != nil {
				return err
			}

			if len(findings) == 0 {
				_, writeErr := fmt.Fprintln(
					cmd.OutOrStdout(),
					"specs.check: all public functions have @spec or exemption",
				)
				return writeErr
			}

			for _, finding := range findings {
				_, _ = fmt.Fprintf(
					cmd.ErrOrStderr(),
					"%s:%d missing @spec for %s\n",
					finding.File,
					finding.Line,
					specscheck.FindingIdentifier(finding),
				)
			}

			return fmt.Errorf(
				"specs.check failed with %d missing @spec declaration(s)",
				len(findings),
			)
		},
	}

	return cmd
}

func parseSpecsCheckArgs(args []string) specsCheckOptions {
	out := specsCheckOptions{paths: []string{}}

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch {
		case arg == "--help" || arg == "-h":
			// Mirrors Elixir task behavior where no explicit help switch exists.
			// Keep this as no-op to avoid diverging with Cobra defaults.
		case arg == "--paths":
			if i+1 >= len(args) {
				continue
			}
			i++
			out.paths = append(out.paths, args[i])
		case strings.HasPrefix(arg, "--paths="):
			out.paths = append(out.paths, strings.TrimPrefix(arg, "--paths="))
		case arg == "--exemptions-file":
			if i+1 >= len(args) {
				continue
			}
			i++
			out.exemptionsFile = args[i]
		case strings.HasPrefix(arg, "--exemptions-file="):
			out.exemptionsFile = strings.TrimPrefix(arg, "--exemptions-file=")
		default:
			// Ignore invalid switches/argv to mirror OptionParser.parse with ignored invalid values.
		}
	}

	return out
}

func loadSpecsExemptions(path string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return out, nil
	}

	raw, err := os.ReadFile(trimmed)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}

	for _, line := range strings.Split(string(raw), "\n") {
		candidate := strings.TrimSpace(line)
		if candidate == "" || strings.HasPrefix(candidate, "#") {
			continue
		}
		out[candidate] = struct{}{}
	}

	return out, nil
}
