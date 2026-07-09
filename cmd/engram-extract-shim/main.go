package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", ":8088", "listen address")
	backendName := flag.String("backend", envOr("SHIM_BACKEND", "agy"), "extraction backend: agy, codex, claude, or ensemble (agy+codex reconciled by a claude-sonnet judge)")
	model := flag.String("model", envOr("SHIM_MODEL", ""), "backend-specific model override (defaults to each backend's cheap-model preset)")
	timeout := flag.Duration("timeout", envDurationOr("SHIM_TIMEOUT", 60*time.Second), "per-call timeout for the backend CLI")
	flag.Parse()

	backend, err := newBackend(*backendName, *model)
	if err != nil {
		fmt.Fprintln(os.Stderr, "engram-extract-shim:", err)
		os.Exit(1)
	}

	shim := &Shim{Backend: backend, Timeout: *timeout}
	slog.Info("engram-extract-shim listening", "addr", *addr, "backend", *backendName, "timeout", *timeout)
	if err := http.ListenAndServe(*addr, shim.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, "engram-extract-shim:", err)
		os.Exit(1)
	}
}

// envOr returns the environment variable's value, or fallback when unset —
// flags always take precedence since flag.String's default is evaluated
// once at flag-definition time from the env.
func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

// envDurationOr parses the environment variable as a duration, or returns
// fallback when unset or unparsable (a bad env value degrades to the
// documented default rather than crashing at startup).
func envDurationOr(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
