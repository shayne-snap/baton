package main

import (
	"context"
	"os"

	"baton/internal/cli"
)

func main() {
	ctx := context.Background()
	cmd := cli.NewRootCommand()
	if err := cmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
