// Command schemagen writes the JSON Schema of `bothways status --json`.
// Run it through `go generate ./internal/cli`.
package main

import (
	"fmt"
	"os"

	"github.com/CoinkWang/bothways/internal/cli"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: schemagen <output file>")
		os.Exit(2)
	}
	data, err := cli.StatusSchema()
	if err == nil {
		err = os.WriteFile(os.Args[1], data, 0o644)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "schemagen: %v\n", err)
		os.Exit(1)
	}
}
