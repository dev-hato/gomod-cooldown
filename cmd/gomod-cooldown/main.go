// gomod-cooldown filters Go module version discovery for a child command.
package main

import (
	"context"
	"os"

	"github.com/dev-hato/gomod-cooldown/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), cli.Invocation{
		Args:   os.Args[1:],
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}))
}
