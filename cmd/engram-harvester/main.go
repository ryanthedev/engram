package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/internal/harvester"
	_ "github.com/ryanthedev/engram/internal/harvester/sources"
)

type stringSlice []string

func (s *stringSlice) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ",")
}

func (s *stringSlice) Set(value string) error {
	parts := strings.Split(value, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}

func main() {
	// Flags
	manifestPath := flag.String("manifest", "", "path to harvest manifest YAML file (required)")
	var collections stringSlice
	flag.Var(&collections, "collection", "repeatable or comma-separated list of collections to harvest")
	var sources stringSlice
	flag.Var(&sources, "source", "repeatable or comma-separated list of source types to harvest")
	addr := flag.String("addr", ":7070", "engram gRPC server address")
	batchSize := flag.Int("batch-size", 500, "batch size for ingesting documents")
	timeout := flag.Duration("timeout", 6*time.Hour, "overall run deadline timeout")
	_ = flag.Bool("once", false, "no-op: a run is always one-shot")
	_ = flag.Bool("backfill", false, "no-op: a run is always one-shot")

	flag.Parse()

	logger := slog.Default()

	if *manifestPath == "" {
		slog.Error("harvester: --manifest flag is required")
		flag.Usage()
		os.Exit(1)
	}

	// 1. Load manifest file
	manifestData, err := os.ReadFile(*manifestPath)
	if err != nil {
		slog.Error("harvester: failed to read manifest file", "path", *manifestPath, "error", err)
		os.Exit(1)
	}

	m, err := harvester.LoadManifest(manifestData)
	if err != nil {
		slog.Error("harvester: failed to load manifest", "error", err)
		os.Exit(1)
	}

	// 2. Validate filters exist in manifest BEFORE dialing
	if err := harvester.ValidateFilters(m, collections, sources); err != nil {
		slog.Error("harvester: filter validation failed", "error", err)
		os.Exit(1)
	}

	// 3. Read token from env ONLY
	token := os.Getenv("ENGRAM_HARVESTER_TOKEN")
	if token == "" {
		slog.Error("harvester: ENGRAM_HARVESTER_TOKEN environment variable is required but empty")
		os.Exit(1)
	}

	// 4. Dial engram
	ec, err := harvester.DialEngram(*addr, token)
	if err != nil {
		slog.Error("harvester: dialing engram failed", "addr", *addr, "error", err)
		os.Exit(1)
	}
	defer func() {
		if closer, ok := ec.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	// 5. Build context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// 6. Run harvester orchestration
	opts := harvester.RunOptions{
		Collections: collections,
		Sources:     sources,
		BatchSize:   *batchSize,
		Logger:      logger,
	}

	totalIndexed, totalDeleted, err := harvester.Run(ctx, ec, m, opts)
	if err != nil {
		// Inspect if it is an auth error or permission denied
		var grpcStatus interface{ GRPCStatus() *status.Status }
		if errors.As(err, &grpcStatus) {
			st := grpcStatus.GRPCStatus()
			if st.Code() == codes.PermissionDenied || st.Code() == codes.Unauthenticated {
				slog.Error("harvester: authentication failed or permission denied: check your ENGRAM_HARVESTER_TOKEN", "error", err)
				os.Exit(1)
			}
		}

		errMsg := strings.ToLower(err.Error())
		if strings.Contains(errMsg, "permissiondenied") || strings.Contains(errMsg, "unauthenticated") || strings.Contains(errMsg, "permission denied") {
			slog.Error("harvester: authentication failed or permission denied: check your ENGRAM_HARVESTER_TOKEN", "error", err)
			os.Exit(1)
		}

		slog.Error("harvester: execution completed with errors", "indexed", totalIndexed, "deleted", totalDeleted, "error", err)
		os.Exit(1)
	}

	slog.Info("harvester: execution completed successfully", "indexed", totalIndexed, "deleted", totalDeleted)
	os.Exit(0)
}
