// Command engram-seed-knowledge seeds the curated_notes knowledge collection
// with a fixed demo-doc set mapped to real rtd memory entity ids, proving out
// the knowledge<->memory soft foreign key this prototype adds end-to-end.
// Idempotent: re-running tolerates an already-existing collection and
// upserts the same docs in place.
//
// Requires a token bearing the admin (or harvester) role -- mint one via:
//
//	engram token create --tenant T --user U --roles admin
//
// Usage:
//
//	go run ./cmd/engram-seed-knowledge [-addr HOST:PORT] [-token TOK]
//
// -addr/-token fall back to the ENGRAM_ADDR/ENGRAM_TOKEN environment
// variables when omitted, matching every other engram CLI command.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ryanthedev/engram/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.RunSeedKnowledge(ctx, os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "engram-seed-knowledge:", err)
		os.Exit(1)
	}
}
