package main

// costSourceFunc adapts a plain func() float64 to telemetry.CostSource —
// main.go's cost feed is a closure over the existing ingest.CostMeter
// snapshot + pricing, so no new stateful type is needed just to satisfy the
// interface (mirrors the http.HandlerFunc adapter pattern).
type costSourceFunc func() float64

func (f costSourceFunc) CostPer1kEventsUSD() float64 { return f() }
