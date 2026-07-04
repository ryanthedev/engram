//go:build e2e

// Package e2e is the end-to-end test harness driving the full local stack
// through MCP, the CLI, and gRPC. It is the foundation every later phase
// extends: a phase adds a scenario pack in its own file (RegisterScenario from
// an init) with ZERO edits to this core (DW-3.6) — the harness discovers
// registered scenarios and runs them.
package e2e

import "sort"

// Scenario is one named end-to-end check. Run receives a booted Harness and
// returns an error to fail the scenario. Scenarios must be independent and
// idempotent — the harness gives each run a unique tenant so they never
// collide, even sharing one cluster.
type Scenario struct {
	// Name identifies the scenario in test output (unique).
	Name string
	// Run executes the scenario against a booted harness.
	Run func(h *Harness) error
}

// registry holds every scenario registered at init time. It is package-private
// so packs register through RegisterScenario, never by mutating it directly —
// that indirection is what keeps the core edit-free as phases add packs.
var registry []Scenario

// RegisterScenario adds a scenario to the pack. Call it from an init() in a
// scenario file; the core needs no change to pick it up. Duplicate names
// panic, catching accidental copy-paste.
func RegisterScenario(s Scenario) {
	for _, existing := range registry {
		if existing.Name == s.Name {
			panic("e2e: duplicate scenario registered: " + s.Name)
		}
	}
	registry = append(registry, s)
}

// Scenarios returns the registered scenarios in stable name order.
func Scenarios() []Scenario {
	out := append([]Scenario(nil), registry...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
