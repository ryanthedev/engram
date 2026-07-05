// Package telemetrygrpc is the gRPC transport edge for internal/telemetry's
// RED metrics (Phase 7) — the same internal/auth + internal/authgrpc split
// applies here: internal/telemetry stays framework-agnostic (OTel/Prometheus
// only), and this package is the one place that imports gRPC to wrap it as
// a real interceptor. Allowlisted in internal/importlint's transport-edge
// list alongside internal/authgrpc (DW-3.7).
package telemetrygrpc

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/internal/telemetry"
)

// UnaryServerInterceptor records every unary RPC's duration and resulting
// status code into red — mount it alongside the auth interceptor (order
// does not matter for metrics; it runs regardless of auth outcome, since an
// auth rejection is itself an observable RED event, not something to hide
// from the metric).
func UnaryServerInterceptor(red *telemetry.RED) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		red.Observe(ctx, info.FullMethod, status.Code(err).String(), time.Since(start))
		return resp, err
	}
}
