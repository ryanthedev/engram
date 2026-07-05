package telemetrygrpc_test

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/internal/telemetry"
	"github.com/ryanthedev/engram/internal/telemetrygrpc"
)

// TestUnaryServerInterceptor_RecordsOKAndErrorCalls proves the interceptor
// wraps a handler transparently (result/error pass through unchanged) while
// recording one RED observation per call, tagged with the actual resulting
// gRPC status code.
func TestUnaryServerInterceptor_RecordsOKAndErrorCalls(t *testing.T) {
	provider, err := telemetry.NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	red, err := telemetry.NewRED(provider.Meter("test"))
	if err != nil {
		t.Fatalf("NewRED: %v", err)
	}
	interceptor := telemetrygrpc.UnaryServerInterceptor(red)

	okHandler := func(ctx context.Context, req any) (any, error) { return "ok", nil }
	errHandler := func(ctx context.Context, req any) (any, error) {
		return nil, status.Error(codes.NotFound, "missing")
	}

	resp, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/engram.v1.Engram/Ingest"}, okHandler)
	if err != nil || resp != "ok" {
		t.Fatalf("ok call = (%v, %v), want (\"ok\", nil)", resp, err)
	}

	_, err = interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/engram.v1.Engram/Ingest"}, errHandler)
	if err == nil || status.Code(err) != codes.NotFound {
		t.Fatalf("error call code = %v, want NotFound", status.Code(err))
	}

	rec := httptest.NewRecorder()
	provider.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	if got := countLabeled(body, `code="OK"`); got != 1 {
		t.Errorf("OK observations = %v, want 1:\n%s", got, body)
	}
	if got := countLabeled(body, `code="NotFound"`); got != 1 {
		t.Errorf("NotFound observations = %v, want 1:\n%s", got, body)
	}
}

func countLabeled(body, labelSubstr string) float64 {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "engram_grpc_request_duration_seconds_count") && strings.Contains(line, labelSubstr) {
			fields := strings.Fields(line)
			if v, err := strconv.ParseFloat(fields[len(fields)-1], 64); err == nil {
				return v
			}
		}
	}
	return 0
}
