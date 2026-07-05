package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ryanthedev/engram/deploy/aws/sizing"
	"github.com/ryanthedev/engram/internal/store"
)

// ramReport is the DW-7.3 evidence: measured off-heap vector RAM (from the
// real k-NN plugin stats API) compared against the SQfp16 sizing formula,
// plus the circuit-breaker headroom check.
type ramReport struct {
	SeedDocs                   int     `json:"seed_docs"`
	EstimatedBytes             float64 `json:"estimated_bytes_sqfp16_formula"`
	MeasuredBytes              float64 `json:"measured_bytes_knn_stats"`
	WithinTolerance20Pct       bool    `json:"within_20pct_tolerance"`
	CircuitBreakerUsagePercent float64 `json:"circuit_breaker_usage_percent"`
	CircuitBreakerLimitConfig  string  `json:"circuit_breaker_limit_config"`
	UnderEightyPercentBreaker  bool    `json:"under_80pct_breaker_limit"`
}

// measureVectorRAM computes the SQfp16 estimate for numVectors (dim=1024,
// m=16 — Engram's pinned embedding dim and the semantic template's HNSW
// degree) and compares it against the real cluster's k-NN plugin stats for
// THIS RUN'S index specifically (_plugins/_knn/stats.nodes.*.
// indices_in_cache[index].graph_memory_usage, reported in KiB) — scoped per
// index rather than the node-wide total, since a shared dev cluster can
// carry other indices' graphs in cache too. The circuit-breaker usage
// percentage, however, IS necessarily node-wide (that's what the real
// breaker trips on), so it is read separately.
func measureVectorRAM(ctx context.Context, client *http.Client, baseURL, index string, numVectors int) (ramReport, error) {
	estimated := sizing.EstimatedVectorRAMBytes(numVectors, store.EmbeddingDim, 16, 1)

	measuredKB, usagePct, err := knnStats(ctx, client, baseURL, index)
	if err != nil {
		return ramReport{SeedDocs: numVectors, EstimatedBytes: estimated}, err
	}
	measuredBytes := measuredKB * 1024

	limitConfig, err := circuitBreakerLimit(ctx, client, baseURL)
	if err != nil {
		limitConfig = "unknown (" + err.Error() + ")"
	}

	return ramReport{
		SeedDocs:                   numVectors,
		EstimatedBytes:             estimated,
		MeasuredBytes:              measuredBytes,
		WithinTolerance20Pct:       sizing.WithinTolerance(measuredBytes, estimated, 20),
		CircuitBreakerUsagePercent: usagePct,
		CircuitBreakerLimitConfig:  limitConfig,
		UnderEightyPercentBreaker:  usagePct <= 80.0,
	}, nil
}

// knnStats sums index's graph_memory_usage (KiB) across nodes' per-index
// cache breakdown, and takes the max node-wide
// graph_memory_usage_percentage — the DW-7.3 "measured" side (see
// measureVectorRAM's doc comment for why these are scoped differently).
func knnStats(ctx context.Context, client *http.Client, baseURL, index string) (indexKB, maxNodeUsagePct float64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/_plugins/_knn/stats", nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("querying _plugins/_knn/stats: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("_plugins/_knn/stats: unexpected status %d", resp.StatusCode)
	}
	var decoded struct {
		Nodes map[string]struct {
			GraphMemoryUsagePercentage float64 `json:"graph_memory_usage_percentage"`
			IndicesInCache             map[string]struct {
				GraphMemoryUsage float64 `json:"graph_memory_usage"`
			} `json:"indices_in_cache"`
		} `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return 0, 0, fmt.Errorf("decoding _plugins/_knn/stats: %w", err)
	}
	for _, n := range decoded.Nodes {
		if entry, ok := n.IndicesInCache[index]; ok {
			indexKB += entry.GraphMemoryUsage
		}
		if n.GraphMemoryUsagePercentage > maxNodeUsagePct {
			maxNodeUsagePct = n.GraphMemoryUsagePercentage
		}
	}
	return indexKB, maxNodeUsagePct, nil
}

// circuitBreakerLimit reads the configured knn.memory.circuit_breaker.limit
// (default "50%") for the report's context field.
func circuitBreakerLimit(ctx context.Context, client *http.Client, baseURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+"/_cluster/settings?include_defaults=true&filter_path=**.knn.memory.circuit_breaker.limit", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", err
	}
	for _, section := range []string{"persistent", "transient", "defaults"} {
		if v, ok := dig(decoded, section, "knn", "memory", "circuit_breaker", "limit"); ok {
			return fmt.Sprintf("%v", v), nil
		}
	}
	return "", fmt.Errorf("limit not found in cluster settings response")
}

func dig(m map[string]any, keys ...string) (any, bool) {
	var cur any = m
	for _, k := range keys {
		asMap, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = asMap[k]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}
