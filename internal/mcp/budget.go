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

// memoryFacetFields are the per-hit fields eligible for top-facet computation
// over omitted memory_search hits, in the fixed order used for both
// tie-breaking and hint text. Knowledge collections declare their own field
// vocabulary at runtime, so knowledge_search passes nil instead (no facets in
// v1) — which is why the packer takes facet fields as a parameter rather than
// reading this var directly.
var memoryFacetFields = []string{"subject", "predicate", "kind"}

// packable is the hit shape the byte-budget packer works over: memory Hit
// and KnowledgeHit pack identically except for their serialized form (which
// the packer measures by real marshal, never by estimate) and where their
// stored fields live for facet computation — which is all fieldsJSON hides.
type packable interface {
	fieldsJSON() string
}

// searchResult is the memory_search and knowledge_search tool-result
// envelope: a budget-packed
// page of hits plus what got left out. Omitted/OmittedFacets/Hint are
// present only when hits were actually omitted (DW-2.2). OverflowPath is set
// by the caller (see spill.go) only after the full slim result set has been
// durably spilled to disk (DW-3.1) — it is also the shape spilled to that
// file, where OverflowPath itself is always zero-value/omitted.
//
// Expanded is memory_search's second, subordinate block: the graph expansions
// that rode along beside the matched hits. It is packed only into whatever
// budget the matched hits left over (packExpanded), so it can never evict a
// match, and it is omitted entirely when empty — an absent block, not an empty
// one (DW-6.3). ExpandedOmitted reports how many expansions the budget dropped
// so the loss is visible rather than silent. knowledge_search never sets
// either.
type searchResult[H packable] struct {
	Hits []H `json:"hits"`
	// Total is the exact match count across every page (OpenSearch
	// track_total_hits, never a capped estimate) — set only by
	// knowledge_search's offset paging, after packAndSpill returns.
	// memory_search never sets it, so its zero value stays omitted from the
	// JSON (memory_search carries no paging concept).
	Total           int64             `json:"total,omitempty"`
	Omitted         int               `json:"omitted,omitempty"`
	OmittedFacets   map[string]string `json:"omitted_facets,omitempty"`
	Hint            string            `json:"hint,omitempty"`
	OverflowPath    string            `json:"overflow_path,omitempty"`
	Expanded        []H               `json:"expanded,omitempty"`
	ExpandedOmitted int               `json:"expanded_omitted,omitempty"`
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
// — INCLUDING the overflow_path field the caller (tools.go) attaches after a
// spill — stays within budgetBytes, always keeping at least one hit when
// hits is non-empty (DW-2.4) even if that hit plus the reserved
// overflow_path headroom still exceeds the budget: the one-hit floor is
// unconditional, budget is best-effort below it. Never panics — a zero-hit
// input produces an empty, non-nil page.
//
// It starts by trying to keep every hit (the common, cheap case: one
// marshal, no omission, no overflow_path ever attached). If the full result
// overflows the budget, it moves the lowest-ranked packed hit into the
// remainder, recomputes the *real* envelope (facets + hint) over that
// remainder, and remeasures the *actual* serialized bytes of the whole
// candidate result — repeating until it fits or only one hit is left, which
// is force-kept unconditionally. From the first shrink onward, remainder is
// non-empty, meaning a spill (and therefore an overflow_path field) will be
// attempted, so each fit check also reserves headroom for that field (DW-2.1;
// see searchResultFits). Measuring the true serialized candidate each time
// (rather than a size estimate) is deliberate: an estimate can drift from
// what's actually emitted.
// facetFields selects the per-hit fields eligible for omitted-hit facet
// computation (memoryFacetFields for memory_search, nil for knowledge_search).
func packSearchResult[H packable](hits []H, budgetBytes int, facetFields []string) searchResult[H] {
	packed := make([]H, len(hits))
	copy(packed, hits)
	for len(packed) > 1 && !searchResultFits(packed, hits[len(packed):], budgetBytes, facetFields) {
		packed = packed[:len(packed)-1]
	}
	return buildSearchResult(packed, hits[len(packed):], facetFields)
}

// packExpanded appends as many graph expansions to an ALREADY-PACKED result as
// the budget it did not spend will hold, and reports the rest as
// ExpandedOmitted (DW-6.4). It is deliberately the second, subordinate pass:
//
//   - Matched hits are packed first, against the whole budget, by
//     packSearchResult — which never sees Expanded and so can never shrink the
//     matched page to make room for an expansion. Expansions live strictly on
//     the leftovers, which makes them structurally the first thing dropped
//     under pressure and incapable of evicting a direct match.
//   - Unlike the matched page, expansions have a ZERO floor (no one-hit
//     minimum): under a tight budget the entire block vanishes. Expansion is
//     bonus context, and every expanded hit is a token cost on the caller.
//   - It runs AFTER the caller's spill has attached the real OverflowPath, so
//     the marshal measured here is the true final response — no headroom
//     reservation is needed (contrast searchResultFits, which must reserve for
//     a path that does not exist yet).
//
// Dropped expansions are NOT spilled: the spill file is the matched-hit escape
// hatch (memory_read drills the rest), and an expansion's seed hit is already
// in the response. They are counted instead, so the omission is visible.
//
// Order is preserved: the kept expansions are a prefix of expanded, which
// arrives hop-ordered (nearest first), so the ones dropped are the furthest.
//
// The one thing the budget does not bound is the ExpandedOmitted counter
// itself: when even zero expansions do not fit, the ~24-byte count is still
// emitted, because a caller silently missing its expansions is worse than a
// response 24 bytes over. This is the same best-effort-at-the-floor posture
// packSearchResult already takes with its unconditional one-hit floor.
func packExpanded[H packable](result searchResult[H], expanded []H, budgetBytes int) searchResult[H] {
	kept := len(expanded)
	for kept > 0 && !withExpandedFits(result, expanded, kept, budgetBytes) {
		kept--
	}
	if kept > 0 {
		result.Expanded = expanded[:kept]
	}
	result.ExpandedOmitted = len(expanded) - kept
	return result
}

// withExpandedFits reports whether result carrying the first kept of expanded —
// and the ExpandedOmitted count that choice actually produces — serializes at
// or under budgetBytes. Like searchResultFits it measures the REAL marshal of
// the exact value that would be emitted (counter included: an omitted count is
// itself bytes on the wire), never an estimate, and treats a marshal failure —
// never expected for these types — as "does not fit", the safe default that
// keeps the caller shrinking rather than emitting something unverified.
func withExpandedFits[H packable](result searchResult[H], expanded []H, kept, budgetBytes int) bool {
	candidate := result
	candidate.Expanded = expanded[:kept]
	candidate.ExpandedOmitted = len(expanded) - kept
	b, err := json.Marshal(candidate)
	return err == nil && len(b) <= budgetBytes
}

// searchResultFits reports whether the full serialized searchResult built
// from packed+remainder is at or under budgetBytes. When remainder is
// non-empty, hits are being omitted and the caller will attempt a spill, so
// the real response will carry an overflow_path field that does not exist
// yet at packing time (the spill file's name isn't chosen until later). To
// keep the budget bound honest for that field too, the candidate reserves
// headroom by setting OverflowPath to maxSpillPath() — an upper bound on the
// real spill path's length — before marshaling, so a fit here guarantees the
// real (equal-or-shorter) overflow_path still fits (DW-2.1). A marshal
// failure (never expected for these types) is treated as "does not fit" —
// the safe default that keeps the packer shrinking rather than emitting
// something unverified.
func searchResultFits[H packable](packed, remainder []H, budgetBytes int, facetFields []string) bool {
	candidate := buildSearchResult(packed, remainder, facetFields)
	if len(remainder) > 0 {
		candidate.OverflowPath = maxSpillPath() // reserve real-field headroom, not an estimate
	}
	b, err := json.Marshal(candidate)
	return err == nil && len(b) <= budgetBytes
}

// buildSearchResult assembles the envelope from a packed page and its
// remainder: omitted/omitted_facets/hint are populated only when hits were
// actually left out (DW-2.2). Hint is built optimistically assuming
// overflow_path WILL end up set (overflowPathSet=true): at this point
// (pack time) the caller's spill hasn't run yet, so "will it succeed" is
// unknown; assuming success matches the worst-case (longer) text already
// reserved for in searchResultFits' budget headroom. If the real spill
// later fails, the caller (tools.go) downgrades the hint via a second
// refineHint call with overflowPathSet=false so it never dangles a path
// that was never written (DW-3.2).
func buildSearchResult[H packable](packed, remainder []H, facetFields []string) searchResult[H] {
	result := searchResult[H]{Hits: packed}
	if len(remainder) == 0 {
		return result
	}
	result.Omitted = len(remainder)
	result.OmittedFacets = topFacets(remainder, facetFields)
	result.Hint = refineHint(result.Omitted, result.OmittedFacets, facetFields, true)
	return result
}

// topFacets computes the single most common value per facetFields field
// among hits, skipping hits whose Fields is missing, malformed, or lacks the
// field. Ties are broken by first-encountered order among hits, which are
// already in the backend's stable rank order (DW-2.5).
func topFacets[H packable](hits []H, facetFields []string) map[string]string {
	counts := make(map[string]map[string]int, len(facetFields))
	firstSeen := make(map[string][]string, len(facetFields))
	for _, h := range hits {
		var fields map[string]any
		if err := json.Unmarshal([]byte(h.fieldsJSON()), &fields); err != nil {
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

// refineHint builds a short, deterministic hint describing what got omitted,
// the top facet values the caller can narrow by, and where to get the rest —
// the chosen cap-plus-refine-hint paging model (no next-page cursor). It
// names engram's own sanctioned escape hatches explicitly: overflow_path for
// the full omitted set (DW-3.1) and memory_read(id, source) to drill a
// single hit's full body (DW-3.3). This wording is a direct response to the
// motivating incident — an agent that, lacking this pointer, invented its
// own path to a full result set (a jq probe over Claude Code's private
// tool-results cache) instead of reading engram's own spill.
//
// facetFields selects which fields' top values the hint lists (in that fixed
// order for determinism): memoryFacetFields for memory_search, nil for
// knowledge_search.
//
// overflowPathSet must reflect the REAL, final state of the response's
// overflow_path field, never merely "omission happened": omission alone
// only means a spill will be attempted, not that it will succeed. Passing
// true when overflow_path will end up unset (or vice versa) makes the hint
// lie (DW-3.2) — see buildSearchResult and tools.go's callSearch for the two
// call sites and why each passes the value it does.
func refineHint(omitted int, facets map[string]string, facetFields []string, overflowPathSet bool) string {
	hint := fmt.Sprintf("%d more hit(s) omitted to stay within the response size budget; narrow your query", omitted)
	parts := make([]string, 0, len(facetFields))
	for _, field := range facetFields { // fixed order: deterministic hint text
		if v, ok := facets[field]; ok {
			parts = append(parts, fmt.Sprintf("%s=%q", field, v))
		}
	}
	if len(parts) > 0 {
		hint = fmt.Sprintf("%s (top omitted %s)", hint, strings.Join(parts, ", "))
	}
	if overflowPathSet {
		hint += "; read the full omitted set from overflow_path"
	}
	return hint + ", or drill one hit's full body with memory_read(id, source)."
}
