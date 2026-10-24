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

// renderKnowledgeResult converts one knowledge search page — every hit the
// backend returned, before any budget cut — into the same rendered envelope
// memory produces, so both tools emit one line format and pack through the
// same packRendered. total is the exact match count, set BEFORE packing so the
// "(3 of 9)" header is part of what the budget measures. offset is the page's
// starting rank: knowledge_search pages, so its omission line can name the
// exact offset that resumes where this page stopped.
func renderKnowledgeResult(collection string, offset int, total int64, hits []KnowledgeHit, fullBody bool) renderedResult {
	return renderedResult{
		Label:  "knowledge/" + collection,
		Total:  total,
		Hits:   renderKnowledgeHits(collection, hits, fullBody),
		Paged:  true,
		Offset: offset,
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

// renderedResult is one search response in rendered form: the hits as
// compact-line rows plus what the budget cut. It is both what packRendered
// packs and what compactLines prints, so the budget is measured on exactly the
// text that ships. Omitted counts matched hits the budget cut (zero when none
// were). Expanded carries the graph expansions that rode along beside the
// matched hits, present only when there are any (DW-6.3) and never merged
// into Hits (DW-6.2).
type renderedResult struct {
	// Label names the block this result renders as — "memory" for the fused
	// memory tiers, "knowledge/<collection>" for one collection.
	Label string
	// Total is the exact match count when the backend reports one (knowledge
	// tracks it; memory does not), so a block header can say "3 of 9".
	Total   int64
	Hits    []renderedHit
	Omitted int
	// Paged marks a source that accepts an offset (knowledge_search), and
	// Offset is this page's starting rank. Together they let the omission
	// line name the exact resume point — Offset plus the hits shown — instead
	// of a generic "narrow the query". A flag rather than sniffing Label: the
	// line's wording follows what the tool accepts, not what it is called.
	Paged           bool
	Offset          int
	Expanded        []renderedHit
	ExpandedOmitted int
}

// renderSearchResult converts one memory search answer — every hit the
// backend returned, before any budget cut — into its rendered form, ready for
// packRendered. It never mutates res or its Hits (DW-1.6): every field is
// copied into new renderedHit/renderedResult values, so the caller's original
// Hit slices (and, upstream, whatever engramclient/gRPC decoded) stay
// byte-identical after this call.
func renderSearchResult(res SearchResult) renderedResult {
	r := renderedResult{Label: "memory", Hits: renderHits(res.Hits)}
	// An expansion row is its statement alone, so one with no statement would
	// render as a blank line: such rows carry nothing and are dropped here.
	for _, h := range renderHits(res.Expanded) {
		if h.Text != "" {
			r.Expanded = append(r.Expanded, h)
		}
	}
	return r
}

// renderHits renders one block of hits, always returning a non-nil slice, so
// an empty block is an empty list rather than an absent one; whether an empty
// expansion block is shown at all is renderSearchResult's and compactLines'
// call (DW-6.3), not this function's.
func renderHits(hits []Hit) []renderedHit {
	out := make([]renderedHit, len(hits))
	for i, h := range hits {
		out[i] = renderHit(h)
	}
	return out
}

// semanticIDPrefixLen is how many leading characters of a semantic fact id a
// search row shows. Semantic ids are 64-hex sha256 digests, and their full
// column was ~12% of a typical search's bytes; 12 hex chars (48 bits) keeps a
// collision within one tenant's fact set vanishingly unlikely. memory_read
// forwards the prefix to the server unchanged, which resolves it.
const semanticIDPrefixLen = 12

// renderHit converts one packed memory Hit into its minimal line form,
// folding the tier into the id so the row is self-addressing.
func renderHit(h Hit) renderedHit {
	fields := parseFields(h.Fields)
	return renderedHit{
		ID:   h.Source + ":" + displayID(h.Source, h.ID),
		Date: dateOf(fields),
		Text: gistFor(h.Source, fields),
	}
}

// displayID is the id a search row shows for a record of source: a semantic
// id cut to semanticIDPrefixLen, every other id whole. Only semantic is
// shortened because only semantic ids are long digests the server can resolve
// by prefix; episodic ids are 20-char OpenSearch ids, and knowledge doc ids are
// harvester-chosen strings with no prefix contract at all. Byte slicing is
// safe here: semantic ids are server-generated ASCII hex.
func displayID(source, id string) string {
	if source == "semantic" && len(id) > semanticIDPrefixLen {
		return id[:semanticIDPrefixLen]
	}
	return id
}

// renderKnowledgeHits renders one block of knowledge hits, always returning a
// non-nil slice so an empty block marshals as `[]`, never `null`.
func renderKnowledgeHits(collection string, hits []KnowledgeHit, fullBody bool) []renderedHit {
	out := make([]renderedHit, len(hits))
	for i, h := range hits {
		out[i] = renderKnowledgeHit(collection, h, fullBody)
	}
	return out
}

// renderKnowledgeHit converts one knowledge hit to the same minimal shape a
// memory hit renders to, so one line format spans both sources. Its Text is
// the joined highlight fragments — the matched excerpts that already replaced
// the document body at query time. A filter-only search matches no terms and
// so carries no fragments; it falls back to the document title rather than
// rendering a blank row.
//
// A full_body search also carries no fragments, but for the opposite reason:
// the caller asked for the whole document inline, and the server put it back
// into Fields. The body lives under the collection's own text_field, a name
// this edge does not know, so the row's text is the stored document itself
// as one line of JSON — every field, the body included — rather than the
// title-only row it used to collapse to.
func renderKnowledgeHit(collection string, h KnowledgeHit, fullBody bool) renderedHit {
	fields := parseFields(h.Fields)
	text := normalizeToSingleLine(strings.Join(h.Fragments, fragmentSep))
	if text == "" && fullBody && len(fields) > 0 {
		text = normalizeToSingleLine(h.Fields)
	}
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
		// Knowledge dates are harvester-supplied, so the column is sanitized
		// like every other value on the row, and cut by rune, never by byte.
		v = normalizeToSingleLine(v)
		if r := []rune(v); len(r) > 10 {
			v = string(r[:10])
		}
		return v
	}
	return ""
}

// parseFields unmarshals a hit's fields_json into a map, tolerating empty or
// malformed input (external data that crossed the wire) by returning nil
// rather than panicking or erroring: one bad record degrades to an empty row,
// never a failed search.
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
// block: one line per matched hit (formatHitLine), then an omission line
// (omissionLine) only when hits were actually left out, so the caller still
// sees that a hit is missing rather than silently losing that signal.
//
// Graph expansions follow BELOW a one-line header, never interleaved with the
// matched hits (DW-6.2): they did not match the query, they were reached by
// traversing out of something that did, and an LLM reading these lines must be
// able to tell the difference at a glance. The header states outright that they
// are not counted against k — that one line is the entire token cost of making
// the block unambiguous, and it is emitted only when there were expansions at
// all (DW-6.3). Below it, each row is a bare statement (formatExpandedLine).
func compactLines(r renderedResult) string {
	lines := make([]string, 0, len(r.Hits)+len(r.Expanded)+3)
	lines = append(lines, blockHeader(r.Label, len(r.Hits), r.Total))
	for _, h := range r.Hits {
		lines = append(lines, formatHitLine(h))
	}
	if r.Omitted > 0 {
		lines = append(lines, omissionLine(r))
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
		lines = append(lines, formatExpandedLine(h))
	}
	return strings.Join(lines, "\n")
}

// formatExpandedLine renders one graph expansion as its statement alone — no
// id column, no date column. Both columns exist on a matched row to serve a
// next step, and an expansion supports neither: graph records have no
// drill-down (callRead refuses source "graph"; the statement already IS the
// whole record), and they carry no date. The 64-hex "graph:<id>" column was a
// quarter of a typical search's bytes for an address nothing could use. The
// header above the block already says what these rows are, so a bare statement
// cannot be mistaken for a match.
func formatExpandedLine(h renderedHit) string {
	return h.Text
}

// omissionLine is the one line a caller gets when the byte budget cut matched
// hits: how many, and the one action that actually surfaces them. It is a
// count plus an instruction and nothing else — the spill file and the
// top-omitted-facet summary it replaced were never acted on (0 of 52 sampled
// searches read an overflow_path), so they were bytes paid on every omitting
// response for nothing.
//
// The instruction must be TRUE for a budget cut. "Raise k" is not: the backend
// already returned these hits and the budget dropped them, so a larger k only
// grows the omitted count. A paged source can resume exactly where this page
// stopped — Offset plus the hits shown — so it names that offset; memory_search
// has no offset, so narrowing is its only lever.
func omissionLine(r renderedResult) string {
	if r.Paged {
		return fmt.Sprintf("... %d more omitted for size; page with offset=%d", r.Omitted, r.Offset+len(r.Hits))
	}
	return fmt.Sprintf("... %d more omitted for size; narrow the query", r.Omitted)
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
