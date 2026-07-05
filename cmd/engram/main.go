// Command engram is the Engram CLI: token administration and the
// ingest/search/status client surface over engramd. See `engram help`.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ryanthedev/engram/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}
