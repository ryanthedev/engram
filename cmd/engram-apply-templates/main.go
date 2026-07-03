// Command engram-apply-templates idempotently applies Engram's cluster
// contract (index templates, concrete indices, RRF search pipeline) to a dev
// OpenSearch. It refuses any cluster off the pinned 3.1 line (D14).
// Re-running against an up-to-date cluster is a no-op.
//
// Usage:
//
//	go run ./cmd/engram-apply-templates [-url http://localhost:9200]
//
// The URL defaults to $ENGRAM_OPENSEARCH_URL, then http://localhost:9200.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/ryanthedev/engram/internal/store"
)

func main() {
	def := os.Getenv("ENGRAM_OPENSEARCH_URL")
	if def == "" {
		def = "http://localhost:9200"
	}
	url := flag.String("url", def, "OpenSearch base URL")
	flag.Parse()

	client := &http.Client{Timeout: store.DefaultTimeout}
	res, err := store.Apply(context.Background(), client, *url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("cluster %s (OpenSearch %s)\n", *url, res.Version)
	for name, action := range res.Actions {
		fmt.Printf("  %-28s %s\n", name, action)
	}
	if res.Changed() {
		fmt.Println("result: resources created")
	} else {
		fmt.Println("result: no-op (already up to date)")
	}
}
