package mcp

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// leadSnippetRunes is the max rune length of an episodic gist: a lead
// snippet, not the full body — enough to decide read/don't-read without
// spending context on the ~1KB record memory_read (Phase 2) exists to fetch.
// A named constant per the plan (Produces), not a magic number.
const leadSnippetRunes = 200

// hitDisplayFields maps each hit source to the fields (beyond id/source/
// score/gist) a compact-line result exposes, in header order. Episodic's fat
// `text` is deliberately absent here — it becomes the gist (truncated),
// never a raw display field. Mirrors internal/retrieval/opensearch.go's
// allowedFields, minus the field that became the gist for each source.
var hitDisplayFields = map[string][]string{
	"episodic": {"kind", "occurred_at", "event_id"},
	"semantic": {"subject", "predicate", "object", "valid_at"},
	"graph":    {"subject", "predicate", "object", "hop"},
}

// renderedHit is one memory_search result: just enough to decide whether the
// full record (fetched via Phase 2's memory_read) is worth the tokens. ID +
// Source together are the addressing contract memory_read consumes, passed
// through from the packed Hit unmodified so they always round-trip. Fields
// is already the source's display allowlist, un-nested from fields_json into
// a real object (a free win: it was already necessary to compute Gist from
// parsed fields).
type renderedHit struct {
	ID     string         `json:"id"`
	Source string         `json:"source"`
	Score  float64        `json:"score"`
	Gist   string         `json:"gist"`
	Fields map[string]any `json:"fields,omitempty"`
}

// renderedResult is the rendered memory_search envelope: mirrors
// searchResult's shape and its omitted/omitted_facets/hint/overflow_path
// gating (present only when hits were actually left out), but with hits
// rendered to their compact-line form. Expanded mirrors the same gating: the
// graph expansions that rode along beside the matched hits, present only when
// there are any (DW-6.3) and never merged into Hits (DW-6.2).
type renderedResult struct {
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

// renderHit converts one packed Hit into its compact-line result: id+source
// pass through unmodified (the round-trippable addressing pair), Fields is
// parsed and projected to the source's display allowlist, and Gist is the
// source-appropriate one-line summary.
func renderHit(h Hit) renderedHit {
	fields := parseFields(h.Fields)
	return renderedHit{
		ID:     h.ID,
		Source: h.Source,
		Score:  h.Score,
		Gist:   gistFor(h.Source, fields),
		Fields: displayFields(h.Source, fields),
	}
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

// displayFields projects fields to source's key display fields (hitDisplayFields),
// dropping absent/nil entries rather than inventing them. Returns nil when
// there is nothing to show (unregistered source, or no fields present) so it
// marshals as an absent/omitted field, not an empty object.
func displayFields(source string, fields map[string]any) map[string]any {
	keys := hitDisplayFields[source]
	if len(keys) == 0 {
		return nil
	}
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, present := fields[k]; present && v != nil {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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

// formatScore renders a score as a fixed 3-decimal string: stable and
// compact regardless of the float's actual precision, so repeated renders of
// the same score are byte-identical.
func formatScore(score float64) string {
	return strconv.FormatFloat(score, 'f', 3, 64)
}

// formatHitLine renders one renderedHit as a single tab-separated line:
// id, source, score, gist, then any key display fields as key=value tokens.
// id and source are copied through verbatim; every other value passes
// through normalizeToSingleLine (via Gist, or here for display fields), so
// the first two tab-separated tokens are always exactly (id, source) with no
// possible ambiguity from hit content (DW-1.4).
func formatHitLine(h renderedHit) string {
	parts := []string{h.ID, h.Source, formatScore(h.Score), h.Gist}
	for _, k := range hitDisplayFields[h.Source] {
		v, ok := h.Fields[k]
		if !ok {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, normalizeToSingleLine(fmt.Sprintf("%v", v))))
	}
	return strings.Join(parts, "\t")
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
	lines := make([]string, 0, len(r.Hits)+len(r.Expanded)+2)
	for _, h := range r.Hits {
		lines = append(lines, formatHitLine(h))
	}
	if r.Omitted > 0 {
		lines = append(lines, fmt.Sprintf("... %s", r.Hint))
	}
	if len(r.Expanded) == 0 {
		return strings.Join(lines, "\n")
	}
	header := fmt.Sprintf("-- expanded: %d graph hit(s) reached from the matches above; context only, not counted against k", len(r.Expanded))
	if r.ExpandedOmitted > 0 {
		header += fmt.Sprintf(" (%d more dropped to stay within the response size budget)", r.ExpandedOmitted)
	}
	lines = append(lines, header+" --")
	for _, h := range r.Expanded {
		lines = append(lines, formatHitLine(h))
	}
	return strings.Join(lines, "\n")
}
