package mcp

import (
	"encoding/json"
	"os"
	"strconv"
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

// searchResult is the memory_search and knowledge_search tool-result
// envelope: a budget-packed page of hits plus how many got left out. Omitted
// is present only when hits were actually omitted (DW-2.2), and it is the
// whole omission signal: a count, not a spill file or a facet summary. The
// old overflow_path spill was never read by a caller (0 of 52 sampled
// searches), so it cost every omitting response its bytes for nothing.
//
// Expanded is memory_search's second, subordinate block: the graph expansions
// that rode along beside the matched hits. It is packed only into whatever
// budget the matched hits left over (packExpanded), so it can never evict a
// match, and it is omitted entirely when empty — an absent block, not an empty
// one (DW-6.3). ExpandedOmitted reports how many expansions the budget dropped
// so the loss is visible rather than silent. knowledge_search never sets
// either.
type searchResult[H any] struct {
	Hits []H `json:"hits"`
	// Total is the exact match count across every page (OpenSearch
	// track_total_hits, never a capped estimate) — set only by
	// knowledge_search's offset paging, after packing returns.
	// memory_search never sets it, so its zero value stays omitted from the
	// JSON (memory_search carries no paging concept).
	Total           int64 `json:"total,omitempty"`
	Omitted         int   `json:"omitted,omitempty"`
	Expanded        []H   `json:"expanded,omitempty"`
	ExpandedOmitted int   `json:"expanded_omitted,omitempty"`
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
// non-empty (DW-2.4) even if that hit alone exceeds the budget: the one-hit
// floor is unconditional, budget is best-effort below it. Never panics — a
// zero-hit input produces an empty, non-nil page.
//
// It starts by trying to keep every hit (the common, cheap case: one
// marshal, no omission). If the full result overflows the budget, it moves
// the lowest-ranked packed hit into the remainder and remeasures the *actual*
// serialized bytes of the whole candidate result — repeating until it fits or
// only one hit is left, which is force-kept unconditionally. Measuring the
// true serialized candidate each time (rather than a size estimate) is
// deliberate: an estimate can drift from what's actually emitted.
func packSearchResult[H any](hits []H, budgetBytes int) searchResult[H] {
	packed := make([]H, len(hits))
	copy(packed, hits)
	for len(packed) > 1 && !searchResultFits(packed, len(hits)-len(packed), budgetBytes) {
		packed = packed[:len(packed)-1]
	}
	return searchResult[H]{Hits: packed, Omitted: len(hits) - len(packed)}
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
//
// Order is preserved: the kept expansions are a prefix of expanded, which
// arrives hop-ordered (nearest first), so the ones dropped are the furthest.
//
// The one thing the budget does not bound is the ExpandedOmitted counter
// itself: when even zero expansions do not fit, the ~24-byte count is still
// emitted, because a caller silently missing its expansions is worse than a
// response 24 bytes over. This is the same best-effort-at-the-floor posture
// packSearchResult already takes with its unconditional one-hit floor.
func packExpanded[H any](result searchResult[H], expanded []H, budgetBytes int) searchResult[H] {
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
func withExpandedFits[H any](result searchResult[H], expanded []H, kept, budgetBytes int) bool {
	candidate := result
	candidate.Expanded = expanded[:kept]
	candidate.ExpandedOmitted = len(expanded) - kept
	b, err := json.Marshal(candidate)
	return err == nil && len(b) <= budgetBytes
}

// searchResultFits reports whether the full serialized searchResult carrying
// packed (with omitted hits left out) is at or under budgetBytes. A marshal
// failure (never expected for these types) is treated as "does not fit" — the
// safe default that keeps the packer shrinking rather than emitting something
// unverified.
func searchResultFits[H any](packed []H, omitted, budgetBytes int) bool {
	b, err := json.Marshal(searchResult[H]{Hits: packed, Omitted: omitted})
	return err == nil && len(b) <= budgetBytes
}
