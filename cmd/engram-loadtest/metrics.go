package main

import (
	"io"
	"net/http"
	"regexp"
	"strconv"
)

// gaugesOfInterest are the DW-7.8 domain gauges this tool proves move
// during its own run (excluding the gate/cost gauges, which need the
// experience/graph stages and a real cost feed this tool intentionally
// doesn't wire — see main.go's doc comment on scope).
var gaugesOfInterest = []string{
	"engram_outbox_backlog",
	"engram_outbox_lag_seconds",
	"engram_repair_backlog",
	"engram_repair_convergence_age_seconds",
	"engram_dlq_depth",
}

// scrapeGauges fetches addr's /metrics endpoint and extracts each gauge of
// interest's current value, for embedding in the load-test report as
// before/after evidence. Best-effort: a scrape failure yields an empty map
// rather than aborting the run.
func scrapeGauges(addr string) map[string]float64 {
	out := map[string]float64{}
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return out
	}
	for _, name := range gaugesOfInterest {
		re := regexp.MustCompile(regexp.QuoteMeta(name) + `(?:\{[^}]*\})?\s+(-?[0-9.eE+]+)\n`)
		if m := re.FindStringSubmatch(string(body) + "\n"); m != nil {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				out[name] = v
			}
		}
	}
	return out
}
