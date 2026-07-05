package telemetry

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRED_RecordsDurationAndCode proves Observe records one histogram
// observation per call, tagged with the method and the given result code —
// the transport-agnostic core internal/telemetrygrpc's interceptor wraps.
func TestRED_RecordsDurationAndCode(t *testing.T) {
	p, err := NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	red, err := NewRED(p.Meter("test"))
	if err != nil {
		t.Fatalf("NewRED: %v", err)
	}

	red.Observe(context.Background(), "/engram.v1.Engram/Search", "OK", 5*time.Millisecond)
	red.Observe(context.Background(), "/engram.v1.Engram/Search", "InvalidArgument", time.Millisecond)

	body := scrape(t, p)
	if !strings.Contains(body, "engram_grpc_request_duration_seconds") {
		t.Fatalf("histogram not found in scrape:\n%s", body)
	}

	// One _count series per (method, code) combination; each call is
	// tagged with its own resulting code, so OK and InvalidArgument each
	// get exactly one observation.
	if got := countSeriesWithLabel(body, "engram_grpc_request_duration_seconds_count", `code="OK"`); got != 1 {
		t.Errorf("OK-coded observations = %v, want 1:\n%s", got, body)
	}
	if got := countSeriesWithLabel(body, "engram_grpc_request_duration_seconds_count", `code="InvalidArgument"`); got != 1 {
		t.Errorf("InvalidArgument-coded observations = %v, want 1:\n%s", got, body)
	}
}

// countSeriesWithLabel returns the _count value of the exposition line
// starting with metric and containing labelSubstr, or 0 if no such line
// exists.
func countSeriesWithLabel(body, metric, labelSubstr string) float64 {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, metric) && strings.Contains(line, labelSubstr) {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
			if err == nil {
				return v
			}
		}
	}
	return 0
}
