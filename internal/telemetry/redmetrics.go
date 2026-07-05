package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// RED holds the request-duration histogram every RPC call records into —
// the RED-metrics half of Phase 7's telemetry scope (domain gauges are
// Gauges; this is rate/errors/duration). One histogram with method/code
// attributes is sufficient for all three: rate and error-rate derive from
// its count (grouped by code), duration percentiles from histogram_quantile
// over its buckets — the standard Prometheus RED pattern, and exactly what
// the search-p95/ingest-availability SLO alerts
// (deploy/aws/dashboards/alerts.yml) query against.
//
// This package stays framework-agnostic (no gRPC import — DW-3.7's
// import-boundary rule forbids transport imports in business packages);
// internal/telemetrygrpc wraps Observe as an actual
// grpc.UnaryServerInterceptor, mirroring the internal/auth +
// internal/authgrpc split.
type RED struct {
	RequestDuration metric.Float64Histogram
}

// NewRED creates the RED histogram on meter.
func NewRED(meter metric.Meter) (*RED, error) {
	h, err := meter.Float64Histogram("engram_grpc_request_duration_seconds",
		metric.WithDescription("gRPC unary request duration, seconds, labeled by method and status code"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}
	return &RED{RequestDuration: h}, nil
}

// Observe records one call's duration and outcome. method/code are plain
// strings so callers never need to hand this package a transport-specific
// type (e.g. codes.Code) — the transport-layer wrapper converts.
func (r *RED) Observe(ctx context.Context, method, code string, duration time.Duration) {
	r.RequestDuration.Record(ctx, duration.Seconds(),
		metric.WithAttributes(
			attribute.String("method", method),
			attribute.String("code", code),
		))
}
