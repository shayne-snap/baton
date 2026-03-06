package cli

import (
	"errors"
	"os"
	"strings"

	"baton/internal/mcpserver"

	"github.com/spf13/cobra"
)

const (
	linearAPIKeyEnv   = "BATON_LINEAR_API_KEY"
	linearEndpointEnv = "BATON_LINEAR_ENDPOINT"
)

func newMCPLinearServerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "mcp-linear-server",
		Short:  "Run Baton's Linear MCP server over stdio.",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			apiKey := envOrEmpty(linearAPIKeyEnv)
			if apiKey == "" {
				return errors.New("missing BATON_LINEAR_API_KEY")
			}
			endpoint := envOrDefault(linearEndpointEnv, "https://api.linear.app/graphql")
			return mcpserver.ServeLinearStdio(cmd.Context(), endpoint, apiKey)
		},
	}
	return cmd
}

func envOrEmpty(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

func envOrDefault(key string, fallback string) string {
	value := envOrEmpty(key)
	if value == "" {
		return fallback
	}
	return value
}
