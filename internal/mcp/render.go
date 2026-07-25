package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// leadSnippetRunes is the max rune length of an episodic gist: a lead
// snippet, not the full body — enough to decide read/don't-read without
// spending context on the ~1KB record memory_read (Phase 2) exists to fetch.
// A named constant per the plan (Produces), not a magic number.
const leadSnippetRunes = 200

// renderKnowledgeResult converts a budget-packed knowledge search into the
// same rendered envelope memory produces, so both tools emit one line format.
func renderKnowledgeResult(collection string, result searchResult[KnowledgeHit]) renderedResult {
	return renderedResult{
		Label:         "knowledge/" + collection,
		Total:         result.Total,
		Hits:          renderKnowledgeHits(collection, result.Hits),
		Omitted:       result.Omitted,
		OmittedFacets: result.OmittedFacets,
		Hint:          result.Hint,
		OverflowPath:  result.OverflowPath,
	}
}

// renderedHit is one search result — memory or knowledge — in the minimal
// form a caller needs to decide whether the full record is worth the tokens.
// Three fields, deliberately: an address, a date, and the matched text. The
// full record is one memory_read away.
type renderedHit struct {
	// ID is self-addressing: "<source>:<record id>" for a memory tier,
	// "<collection>:<doc id>" for knowledge. One field rather than an
	// (id, source) pair because the source cannot live in a block header:
	// the memory block mixes tiers in one ranked list, and memory_read needs
	// the EXACT tier — "memory" is not a valid source. A caller hands this
	// string straight back to memory_read, which splits it itself.
	ID string `json:"id"`
	// Date is the hit's own date (YYYY-MM-DD), not the harvest date: the one
	// signal a text excerpt cannot carry. Empty when the record has none —
	// graph hits have no date field at all, and not every harvested document
	// declares one.
	Date string `json:"date,omitempty"`
	// Text is the whole payload: an episodic lead snippet, a semantic/graph
	// statement, or a knowledge document's matched fragments. Everything else
	// a record holds is a memory_read away, deliberately — search decides
	// "drill or not", it does not answer.
	Text string `json:"text"`
}

// renderedResult is the rendered memory_search envelope: mirrors
// searchResult's shape and its omitted/omitted_facets/hint/overflow_path
// gating (present only when hits were actually left out), but with hits
// rendered to their compact-line form. Expanded mirrors the same gating: the
// graph expansions that rode along beside the matched hits, present only when
// there are any (DW-6.3) and never merged into Hits (DW-6.2).
type renderedResult struct {
	// Label names the block this result renders as — "memory" for the fused
	// memory tiers, "knowledge/<collection>" for one collection.
	Label string `json:"label"`
	// Total is the exact match count when the backend reports one (knowledge
	// tracks it; memory does not), so a block header can say "3 of 9".
	Total           int64             `json:"total,omitempty"`
	Hits            []renderedHit     `json:"hits"`
	Omitted         int               `json:"omitted,omitempty"`
	OmittedFacets   map[string]string `json:"omitted_facets,omitempty"`
	Hint            string            `json:"hint,omitempty"`
	OverflowPath    string            `json:"overflow_path,omitempty"`
	Expanded        []renderedHit     `json:"expanded,omitempty"`
	ExpandedOmitted int               `json:"expanded_omitted,omitempty"`
}

// renderSearchResult converts a packed searchResult into its rendered form.
// It never mutates result or the Hits it packed (DW-1.6): every field is
// copied into new renderedHit/renderedResult values, so the caller's
// original Hit slice (and, upstream, whatever engramclient/gRPC decoded)
// stays byte-identical after this call.
func renderSearchResult(result searchResult[Hit]) renderedResult {
	return renderedResult{
		Label:           "memory",
		Total:           result.Total,
		Hits:            renderHits(result.Hits),
		Omitted:         result.Omitted,
		OmittedFacets:   result.OmittedFacets,
		Hint:            result.Hint,
		OverflowPath:    result.OverflowPath,
		Expanded:        renderHits(result.Expanded),
		ExpandedOmitted: result.ExpandedOmitted,
	}
}

// renderHits renders one block of hits, always returning a non-nil slice so an
// empty matched block still marshals as `"hits": []` (never `null`) exactly as
// it did before there was a second block. The `expanded` block needs no such
// care: its omitempty tag drops it whenever it is zero-LENGTH, nil or not
// (DW-6.3).
func renderHits(hits []Hit) []renderedHit {
	out := make([]renderedHit, len(hits))
	for i, h := range hits {
		out[i] = renderHit(h)
	}
	return out
}

// renderHit converts one packed memory Hit into its minimal line form,
// folding the tier into the id so the row is self-addressing.
func renderHit(h Hit) renderedHit {
	fields := parseFields(h.Fields)
	return renderedHit{
		ID:   h.Source + ":" + h.ID,
		Date: dateOf(fields),
		Text: gistFor(h.Source, fields),
	}
}

// renderKnowledgeHits renders one block of knowledge hits, always returning a
// non-nil slice so an empty block marshals as `[]`, never `null`.
func renderKnowledgeHits(collection string, hits []KnowledgeHit) []renderedHit {
	out := make([]renderedHit, len(hits))
	for i, h := range hits {
		out[i] = renderKnowledgeHit(collection, h)
	}
	return out
}

// renderKnowledgeHit converts one knowledge hit to the same minimal shape a
// memory hit renders to, so one line format spans both sources. Its Text is
// the joined highlight fragments — the matched excerpts that already replaced
// the document body at query time. A filter-only search matches no terms and
// so carries no fragments; it falls back to the document title rather than
// rendering a blank row.
func renderKnowledgeHit(collection string, h KnowledgeHit) renderedHit {
	fields := parseFields(h.Fields)
	text := normalizeToSingleLine(strings.Join(h.Fragments, fragmentSep))
	if text == "" {
		title, _ := fields["title"].(string)
		text = normalizeToSingleLine(title)
	}
	return renderedHit{
		ID:   collection + ":" + h.ID,
		Date: dateOf(fields),
		Text: text,
	}
}

// fragmentSep joins a hit's separate highlight fragments into one line. A
// pilcrow, not a space: the fragments are discontiguous excerpts, and running
// them together would read as continuous prose that the document never
// contained.
const fragmentSep = " ¶ "

// dateFieldNames are the field names dateOf will accept as a hit's date, in
// precedence order. Memory tiers are fixed (occurred_at/valid_at); knowledge
// collections declare their own vocabulary at runtime, so the knowledge names
// here are a convention, not a contract — a collection that dates its
// documents under some other key renders with an empty date rather than a
// wrong one.
var dateFieldNames = []string{"occurred_at", "valid_at", "date", "published", "created_at", "updated_at"}

// dateOf extracts a hit's date as YYYY-MM-DD. It reads the first populated
// entry of dateFieldNames and truncates to the date component — a timestamp's
// time-of-day is noise at search-scan altitude. Absent or non-string values
// yield "", which renders as an empty column rather than a fabricated date.
func dateOf(fields map[string]any) string {
	for _, name := range dateFieldNames {
		v, ok := fields[name].(string)
		if !ok || v == "" {
			continue
		}
		if len(v) >= 10 {
			return v[:10]
		}
		return v
	}
	return ""
}

// parseFields unmarshals a hit's fields_json into a map, tolerating empty or
// malformed input (external data that crossed the wire) by returning nil
// rather than panicking or erroring — mirrors budget.go's topFacets handling
// of the same data.
func parseFields(fieldsJSON string) map[string]any {
	if fieldsJSON == "" {
		return nil
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
		return nil
	}
	return fields
}

// gistFor computes the one-line "aboutness" summary for a hit: episodic's
// gist is a truncated lead snippet of its fat text (the only tier that is
// genuinely two-phase); every other tier's statement already IS the whole
// memory, so its gist is the untruncated statement (DW-1.5, content
// unchanged). A source without a "statement" field falls back to treating
// text as its gist, for forward-compat with an unregistered tier source.
func gistFor(source string, fields map[string]any) string {
	if source == "episodic" {
		text, _ := fields["text"].(string)
		return leadSnippet(text, leadSnippetRunes)
	}
	if statement, ok := fields["statement"].(string); ok {
		return normalizeToSingleLine(statement)
	}
	text, _ := fields["text"].(string)
	return leadSnippet(text, leadSnippetRunes)
}

// normalizeToSingleLine collapses s to one line: newlines, tabs, and every
// other control character become a single space, and runs of resulting
// whitespace collapse to one — so a compact-line result never breaks its
// line, carries binary noise, or (critically) contains a literal tab that
// could be mistaken for the line's own field delimiter. Leading/trailing
// whitespace is trimmed.
func normalizeToSingleLine(s string) string {
	var b strings.Builder
	lastWasSpace := false
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || unicode.IsControl(r) {
			r = ' '
		}
		if r == ' ' {
			if lastWasSpace {
				continue
			}
			lastWasSpace = true
		} else {
			lastWasSpace = false
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// leadSnippet returns a single-line, rune-safe lead snippet of text capped
// at maxRunes runes. Normalization happens first so the cap always lands on
// visible content, never mid-control-sequence. Truncation slices the
// []rune form (never raw bytes), so a multi-byte rune — e.g. an emoji body —
// is always kept whole or dropped whole, never split (DW-1.3). An ellipsis
// is appended only when truncation actually removed something, so text that
// already fit is returned unmodified — no dangling ellipsis (DW-1.7).
func leadSnippet(text string, maxRunes int) string {
	normalized := normalizeToSingleLine(text)
	runes := []rune(normalized)
	if len(runes) <= maxRunes {
		return normalized
	}
	return string(runes[:maxRunes]) + "…"
}

// formatHitLine renders one renderedHit as a single tab-separated line:
// id, source, score, gist, then any key display fields as key=value tokens.
// id and source are copied through verbatim; every other value passes
// through normalizeToSingleLine (via Gist, or here for display fields), so
// the first two tab-separated tokens are always exactly (id, source) with no
// possible ambiguity from hit content (DW-1.4).
func formatHitLine(h renderedHit) string {
	// Three tab-separated columns, grep's shape: address, date, matched text.
	// Score is deliberately absent — the list is already ranked, so position
	// carries it, and RRF's 1/(60+rank) renders as a column of near-identical
	// numbers that reads as a bug. The date column is emitted even when empty
	// so column positions never shift between rows.
	//
	// ID is sanitized here, not just the content: knowledge document ids are
	// harvester-supplied (store/knowledge.go only checks non-empty), so a tab
	// inside one would otherwise split a row into phantom columns. Memory ids
	// are server-generated and already safe; this costs nothing and closes the
	// case for both.
	return strings.Join([]string{
		normalizeToSingleLine(h.ID),
		h.Date,
		h.Text,
	}, "\t")
}

// compactLines renders a renderedResult's full memory_search text-content
// block: one line per matched hit (formatHitLine), then an omission summary
// line only when hits were actually left out — mirroring buildSearchResult's
// (budget.go) omitted/hint gating, so the caller still sees why a hit is
// missing rather than silently losing that signal to the format change.
//
// Graph expansions follow BELOW a one-line header, never interleaved with the
// matched hits (DW-6.2): they did not match the query, they were reached by
// traversing out of something that did, and an LLM reading these lines must be
// able to tell the difference at a glance. The header states outright that they
// are not counted against k — that one line is the entire token cost of making
// the block unambiguous, and it is emitted only when expansions survived the
// budget (DW-6.3).
func compactLines(r renderedResult) string {
	lines := make([]string, 0, len(r.Hits)+len(r.Expanded)+3)
	lines = append(lines, blockHeader(r.Label, len(r.Hits), r.Total))
	for _, h := range r.Hits {
		lines = append(lines, formatHitLine(h))
	}
	if r.Omitted > 0 {
		// The spill path rides in the text, not just a dropped structured
		// field: with no structuredContent to carry it, a path left out here
		// is a written file nobody can reach.
		omission := "... " + r.Hint
		if r.OverflowPath != "" {
			omission += " overflow_path=" + r.OverflowPath
		}
		lines = append(lines, omission)
	}
	// A header is still owed when the budget dropped EVERY expansion: with no
	// structuredContent to carry expanded_omitted, returning early here would
	// silently erase the fact that neighbors existed at all.
	if len(r.Expanded) == 0 && r.ExpandedOmitted == 0 {
		return strings.Join(lines, "\n")
	}
	header := fmt.Sprintf("graph (%d, reached from the hits above; not counted against k", len(r.Expanded))
	if r.ExpandedOmitted > 0 {
		header += fmt.Sprintf("; %d more dropped for size", r.ExpandedOmitted)
	}
	lines = append(lines, header+")")
	for _, h := range r.Expanded {
		lines = append(lines, formatHitLine(h))
	}
	return strings.Join(lines, "\n")
}

// blockHeader labels one source's block, ripgrep-style: the source is named
// once above its rows instead of repeated on every line. It carries the page
// size and, when the backend reports one, the exact total — so "3 of 9" tells
// a caller there is more to page without spending a row on it.
func blockHeader(label string, shown int, total int64) string {
	if total > int64(shown) {
		return fmt.Sprintf("%s (%d of %d)", label, shown, total)
	}
	return fmt.Sprintf("%s (%d)", label, shown)
}
