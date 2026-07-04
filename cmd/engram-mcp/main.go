// Command engram-mcp is the Engram MCP server: it speaks the Model Context
// Protocol over stdio (JSON-RPC 2.0) and maps memory_ingest / memory_search /
// memory_status onto the engramd gRPC API. An agent host (e.g. Claude Code)
// spawns it and drives tools over stdin/stdout.
//
// Registration (see docs/mcp.md):
//
//	claude mcp add engram -- env ENGRAM_TOKEN=<raw> engram-mcp -addr localhost:7070
//
// The bearer token is read from ENGRAM_TOKEN (or -token); it authenticates
// every gRPC call. Logs go to stderr so they never corrupt the stdio JSON-RPC
// stream on stdout.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ryanthedev/engram/internal/engramclient"
	"github.com/ryanthedev/engram/internal/mcp"
)

func main() {
	addr := flag.String("addr", envOr("ENGRAM_ADDR", "localhost:7070"), "engramd gRPC address")
	token := flag.String("token", os.Getenv("ENGRAM_TOKEN"), "bearer token (or set ENGRAM_TOKEN)")
	flag.Parse()

	// Structured logs to stderr — stdout is the JSON-RPC channel.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if *token == "" {
		logger.Warn("no ENGRAM_TOKEN set; every gRPC call will be rejected until a token is provided")
	}

	client, err := engramclient.Dial(*addr, *token)
	if err != nil {
		fmt.Fprintln(os.Stderr, "engram-mcp:", err)
		os.Exit(1)
	}
	defer client.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("engram-mcp serving on stdio", "addr", *addr)
	srv := mcp.NewServer(client)
	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "engram-mcp:", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
