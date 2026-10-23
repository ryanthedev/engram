package mcp

import (
	"os"
	"strconv"
)

// defaultRequestK is how many hits memory_search and knowledge_search ask the
// Backend for when the caller doesn't specify k. 10 is what callers actually
// ask for (50 of 52 sampled searches passed k, typically 5-10), and about what
// the 6144-byte rendered budget holds of typical rows. The old 50 was sized to
// give a fat-JSON packer candidates to choose from; with the budget measured on
// rendered text, every extra candidate past what fits is fetched, ranked,
// rendered and then cut — server work and an omission line for nothing.
const defaultRequestK = 10

// searchBudgetBytesEnv is the env var overriding the search response byte
// budget; searchBudgetBytesDefault is its fallback.
//
// The budget bounds the RENDERED text the tool returns — the exact bytes that
// land in the caller's context — not any serialized intermediate. 6144 is
// what the old 16384-byte budget actually shipped: it measured the JSON of
// each fat hit (its whole fields_json record included), so it bound when the
// rendered lines were only ~5-6KB. Measuring the real output at 6144 keeps
// today's effective cap while making the number mean what it says.
const (
	searchBudgetBytesEnv     = "ENGRAM_MCP_SEARCH_BUDGET_BYTES"
	searchBudgetBytesDefault = 6144
)

// searchByteBudget returns the configured search response byte budget:
// ENGRAM_MCP_SEARCH_BUDGET_BYTES parsed as a positive integer, or
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

// packRendered cuts a fully rendered result down until the text compactLines
// produces from it is at most budgetBytes long, and records what it cut in
// Omitted / ExpandedOmitted. It packs the RENDERED form on purpose: the budget
// exists to bound the bytes a caller's context pays for, so the only honest
// measure is the exact text the tool returns. Measuring anything upstream of
// rendering — the old packer marshaled each fat Hit, fields_json and all —
// over-counts by however much the render drops, which made "omitted" fire on
// pages that would have fit two to three times over.
//
// Each fit check renders the whole candidate and measures it (never an
// estimate that can drift from what is emitted): the header, the omission
// line, and the graph header all change length as the counts change, and
// summing per-line sizes would have to re-derive every one of them.
//
// Two passes, in a fixed order (DW-6.4):
//
//   - Matched hits pack first, against the whole budget, with the expansion
//     block set aside — so an expansion can never shrink the matched page. The
//     lowest-ranked hit goes first. At least one hit is always kept when any
//     exist (DW-2.4): that floor is unconditional, and the budget is
//     best-effort below it.
//   - Expansions then fill only what the matched page left, with a ZERO floor:
//     under a tight budget the whole block vanishes. They are bonus context
//     reached from the hits, not matches. The kept expansions are a prefix of
//     the hop-ordered input (nearest first), so the dropped ones are the
//     furthest.
//
// The budget does not bound the graph header that reports dropped expansions:
// when not one expansion fits, the header still ships, because a caller
// silently missing its neighbors is worse than a response one line over — the
// same best-effort-at-the-floor posture as the one-hit floor. The overshoot is
// that one header line (under ~100 bytes), and only on a request that opted in
// with expand; reserving room for it in the matched pass would instead let a
// header evict a match.
func packRendered(r renderedResult, budgetBytes int) renderedResult {
	hits, expanded := r.Hits, r.Expanded
	r.Expanded, r.ExpandedOmitted = nil, 0

	kept := len(hits)
	for kept > 1 && !renderFits(withHits(r, hits, kept), budgetBytes) {
		kept--
	}
	r = withHits(r, hits, kept)

	keptExpanded := len(expanded)
	for keptExpanded > 0 && !renderFits(withExpanded(r, expanded, keptExpanded), budgetBytes) {
		keptExpanded--
	}
	return withExpanded(r, expanded, keptExpanded)
}

// withHits returns r carrying the first kept of hits and the Omitted count
// that choice produces. hits[:kept] of an empty input stays a non-nil empty
// slice whenever hits is, so a zero-hit page still renders its header.
func withHits(r renderedResult, hits []renderedHit, kept int) renderedResult {
	r.Hits = hits[:kept]
	r.Omitted = len(hits) - kept
	return r
}

// withExpanded returns r carrying the first kept of expanded and the
// ExpandedOmitted count that choice produces. Zero kept leaves Expanded nil, so
// an empty block stays absent rather than present-and-empty (DW-6.3).
func withExpanded(r renderedResult, expanded []renderedHit, kept int) renderedResult {
	r.Expanded = nil
	if kept > 0 {
		r.Expanded = expanded[:kept]
	}
	r.ExpandedOmitted = len(expanded) - kept
	return r
}

// renderFits reports whether r's compact-line text is at most budgetBytes.
func renderFits(r renderedResult, budgetBytes int) bool {
	return len(compactLines(r)) <= budgetBytes
}
