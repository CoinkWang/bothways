// Command bothways is a variant worktree orchestrator.
package main

import (
	"os"

	"github.com/CoinkWang/bothways/internal/cli"
)

func main() {
	os.Exit(cli.Main())
}
