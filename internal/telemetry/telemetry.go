// Package telemetry instruments the running engram-server for production
// operation (Phase 7): a Prometheus-scrapeable metrics endpoint carrying RED
// metrics (via the standard OTel/Prometheus toolchain) plus the domain gauges
// DW-7.8 requires — outbox/worker lag, repair-sweep backlog + convergence
// age, DLQ depth, experience-gate verdict rates, and extraction cost per 1k
// events — and the cost-control primitives (budget alarm + extraction
// kill-switch, DW-7.6).
//
// This package depends only on the OTel/Prometheus toolchain and the small
// duck-typed source interfaces in recorder.go; it is never imported back by
// internal/store, internal/worker, or internal/experience — those packages'
// existing types (OpenSearchStore, Sweeper, experience.Store) structurally
// satisfy the source interfaces without any import of this package,
// preserving the inward-pointing dependency rule (business packages don't
// know about telemetry).
package telemetry

import (
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Provider bundles an OTel MeterProvider backed by a Prometheus exporter with
// the HTTP handler that serves its scrape endpoint. Each Provider owns a
// private Prometheus registry, so multiple Providers (e.g. one per test) never
// collide on process-global metric registration.
type Provider struct {
	mp       *sdkmetric.MeterProvider
	registry *prometheus.Registry
}

// NewProvider builds a Provider with its own Prometheus registry — safe to
// construct more than once per process (each test gets an isolated registry;
// production wires exactly one).
func NewProvider() (*Provider, error) {
	registry := prometheus.NewRegistry()
	exporter, err := otelprometheus.New(otelprometheus.WithRegisterer(registry))
	if err != nil {
		return nil, fmt.Errorf("telemetry: building prometheus exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	return &Provider{mp: mp, registry: registry}, nil
}

// Meter returns the named OTel meter instruments are created from.
func (p *Provider) Meter(name string) metric.Meter {
	return p.mp.Meter(name)
}

// Handler returns the HTTP handler that serves the Prometheus scrape
// endpoint (mount at /metrics).
func (p *Provider) Handler() http.Handler {
	return promhttp.HandlerFor(p.registry, promhttp.HandlerOpts{})
}
