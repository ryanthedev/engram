package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// defaultRequestK is how many hits memory_search asks the Backend for when
// the caller doesn't specify k. It is deliberately larger than any single
// response is expected to carry — the byte-budget packer below needs real
// candidates to choose from — and stays at or under Phase 1's MaxK=100, which
// the retrieval layer clamps to regardless.
const defaultRequestK = 50

// searchBudgetBytesEnv is the env var overriding the memory_search response
// byte budget; searchBudgetBytesDefault is its fallback, safely below the
// smallest plausible client cap (~25K tokens / Claude Code's
// MAX_MCP_OUTPUT_TOKENS).
const (
	searchBudgetBytesEnv     = "ENGRAM_MCP_SEARCH_BUDGET_BYTES"
	searchBudgetBytesDefault = 16384
)

// facetFields are the per-hit fields eligible for top-facet computation over
// omitted hits, in the fixed order used for both tie-breaking and hint text.
var facetFields = []string{"subject", "predicate", "kind"}

// searchResult is the memory_search tool-result envelope: a budget-packed
// page of hits plus what got left out. Omitted/OmittedFacets/Hint are
// present only when hits were actually omitted (DW-2.2). OverflowPath is set
// by the caller (see spill.go) only after the full slim result set has been
// durably spilled to disk (DW-3.1) — it is also the shape spilled to that
// file, where OverflowPath itself is always zero-value/omitted.
type searchResult struct {
	Hits          []Hit             `json:"hits"`
	Omitted       int               `json:"omitted,omitempty"`
	OmittedFacets map[string]string `json:"omitted_facets,omitempty"`
	Hint          string            `json:"hint,omitempty"`
	OverflowPath  string            `json:"overflow_path,omitempty"`
}

// searchByteBudget returns the configured memory_search response byte
// budget: ENGRAM_MCP_SEARCH_BUDGET_BYTES parsed as a positive integer, or
// searchBudgetBytesDefault when unset or invalid. The env var crosses the
// process boundary as external input, so it is validated and defaulted, never
// asserted (DW-2.3).
func searchByteBudget() int {
	raw := os.Getenv(searchBudgetBytesEnv)
	if raw == "" {
		return searchBudgetBytesDefault
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return searchBudgetBytesDefault
	}
	return n
}

// packSearchResult packs hits into a searchResult whose full serialized size
// stays within budgetBytes, always keeping at least one hit when hits is
// non-empty (DW-2.4) even if that hit alone exceeds the budget. Never
// panics — a zero-hit input produces an empty, non-nil page.
//
// It starts by trying to keep every hit (the common, cheap case: one
// marshal, no omission). If the full result overflows the budget, it moves
// the lowest-ranked packed hit into the remainder, recomputes the *real*
// envelope (facets + hint) over that remainder, and remeasures the *actual*
// serialized bytes of the whole candidate result — repeating until it fits
// or only one hit is left, which is force-kept unconditionally. Measuring
// the true serialized candidate each time (rather than a size estimate)
// is deliberate: an estimate can drift from what's actually emitted.
func packSearchResult(hits []Hit, budgetBytes int) searchResult {
	packed := make([]Hit, len(hits))
	copy(packed, hits)
	for len(packed) > 1 && !searchResultFits(packed, hits[len(packed):], budgetBytes) {
		packed = packed[:len(packed)-1]
	}
	return buildSearchResult(packed, hits[len(packed):])
}

// searchResultFits reports whether the full serialized searchResult built
// from packed+remainder is at or under budgetBytes. A marshal failure (never
// expected for these types) is treated as "does not fit" — the safe default
// that keeps the packer shrinking rather than emitting something unverified.
func searchResultFits(packed, remainder []Hit, budgetBytes int) bool {
	b, err := json.Marshal(buildSearchResult(packed, remainder))
	return err == nil && len(b) <= budgetBytes
}

// buildSearchResult assembles the envelope from a packed page and its
// remainder: omitted/omitted_facets/hint are populated only when hits were
// actually left out (DW-2.2).
func buildSearchResult(packed, remainder []Hit) searchResult {
	result := searchResult{Hits: packed}
	if len(remainder) == 0 {
		return result
	}
	result.Omitted = len(remainder)
	result.OmittedFacets = topFacets(remainder)
	result.Hint = refineHint(result.Omitted, result.OmittedFacets)
	return result
}

// topFacets computes the single most common value per facetFields field
// among hits, skipping hits whose Fields is missing, malformed, or lacks the
// field. Ties are broken by first-encountered order among hits, which are
// already in the backend's stable rank order (DW-2.5).
func topFacets(hits []Hit) map[string]string {
	counts := make(map[string]map[string]int, len(facetFields))
	firstSeen := make(map[string][]string, len(facetFields))
	for _, h := range hits {
		var fields map[string]any
		if err := json.Unmarshal([]byte(h.Fields), &fields); err != nil {
			continue // malformed/absent Fields: this hit contributes no facets
		}
		for _, field := range facetFields {
			v, ok := fields[field].(string)
			if !ok || v == "" {
				continue
			}
			if counts[field] == nil {
				counts[field] = map[string]int{}
			}
			if counts[field][v] == 0 {
				firstSeen[field] = append(firstSeen[field], v)
			}
			counts[field][v]++
		}
	}
	out := map[string]string{}
	for _, field := range facetFields {
		values := counts[field]
		if len(values) == 0 {
			continue
		}
		best, bestCount := "", 0
		for _, v := range firstSeen[field] { // first-seen order breaks ties
			if values[v] > bestCount {
				best, bestCount = v, values[v]
			}
		}
		out[field] = best
	}
	return out
}

// refineHint builds a short, deterministic hint describing what got omitted
// and the top facet values the caller can narrow by — the chosen
// cap-plus-refine-hint paging model (no next-page cursor).
func refineHint(omitted int, facets map[string]string) string {
	hint := fmt.Sprintf("%d more hit(s) omitted to stay within the response size budget; narrow your query", omitted)
	parts := make([]string, 0, len(facetFields))
	for _, field := range facetFields { // fixed order: deterministic hint text
		if v, ok := facets[field]; ok {
			parts = append(parts, fmt.Sprintf("%s=%q", field, v))
		}
	}
	if len(parts) == 0 {
		return hint + "."
	}
	return fmt.Sprintf("%s (top omitted %s).", hint, strings.Join(parts, ", "))
}
