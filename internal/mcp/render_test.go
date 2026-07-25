package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// episodicHit builds a Hit whose Fields is a realistic episodic-tier
// fields_json blob (per allowedFields: text/kind/occurred_at/event_id),
// mirroring budget_test.go's semanticHit for the episodic tier.
func episodicHit(id, text, kind, occurredAt, eventID string) Hit {
	fields := map[string]any{
		"text":        text,
		"kind":        kind,
		"occurred_at": occurredAt,
		"event_id":    eventID,
	}
	b, _ := json.Marshal(fields)
	return Hit{ID: id, Score: 0.5, Source: "episodic", Fields: string(b)}
}

// graphHit builds a Hit whose Fields is a realistic graph-tier fields_json
// blob (per allowedFields: statement/subject/predicate/object/hop).
func graphHit(id, statement, subject, predicate, object string, hop int) Hit {
	fields := map[string]any{
		"statement": statement,
		"subject":   subject,
		"predicate": predicate,
		"object":    object,
		"hop":       hop,
	}
	b, _ := json.Marshal(fields)
	return Hit{ID: id, Score: 0.7, Source: "graph", Fields: string(b)}
}

// parseIDSource recovers the (id, source) pair from one compact-line result,
// exactly as a caller/Phase 2 would: split on tabs, take the first two
// tokens. Fails the test if the line doesn't have at least two fields.
// parseIDSource recovers the (id, source) pair from a result line's first
// column, which is the self-addressing "<source>:<id>" form memory_read
// consumes. Splitting on the FIRST colon mirrors callRead exactly, so this
// helper proves the same round trip the tool performs.
func parseIDSource(t *testing.T, line string) (id, source string) {
	t.Helper()
	parts := strings.SplitN(line, "\t", 3)
	if len(parts) < 3 {
		t.Fatalf("line has fewer than 3 tab-separated fields: %q", line)
	}
	source, id, found := strings.Cut(parts[0], ":")
	if !found {
		t.Fatalf("first column %q is not a self-addressing \"<source>:<id>\" pair: %q", parts[0], line)
	}
	return id, source
}

// TestDW_1_1_MultiHitSearchReturnsCompactLineText: a multi-hit memory_search
// call's content[0].text is compact-line text, not escaped fields_json JSON
// — it fails to parse as JSON, carries no fields_json/escaped-quote residue,
// and has exactly one line per hit.
func TestDW_1_1_MultiHitSearchReturnsCompactLineText(t *testing.T) {
	hits := []Hit{
		episodicHit("ep-1", "the deploy key rotates weekly", "note", "2026-07-01T00:00:00Z", "evt-1"),
		semanticHit("sem-1", "alice", "knows", "bob", 5),
		graphHit("gr-1", "alice manages the infra team", "alice", "manages", "infra-team", 1),
	}
	text, decoded := searchViaWire(t, &fixedHitsBackend{hits: hits}, map[string]any{"query": "q"})

	var probe any
	if err := json.Unmarshal([]byte(text), &probe); err == nil {
		t.Fatalf("content text parses as JSON, want compact-line text: %s", text)
	}
	if strings.Contains(text, "fields_json") {
		t.Errorf("content text still mentions fields_json: %q", text)
	}
	// One block header ("memory (N)") plus one line per hit — the ripgrep
	// grouping: the source is named once above its rows, never per row.
	lines := strings.Split(text, "\n")
	if len(lines) != len(hits)+1 {
		t.Fatalf("got %d lines, want %d (a block header plus one per hit): %q", len(lines), len(hits)+1, text)
	}
	if !strings.HasPrefix(lines[0], "memory (") {
		t.Errorf("first line = %q, want the memory block header", lines[0])
	}

	gotHits, _ := decoded["hits"].([]any)
	if len(gotHits) != len(hits) {
		t.Errorf("structuredContent hits = %d, want %d", len(gotHits), len(hits))
	}
}

// TestDW_1_2_EpisodicLeadSnippetNormalizedAndCapped: episodic text renders
// as a normalized single-line lead snippet within the length cap — short
// text is returned unmodified (no truncation, no dangling ellipsis), long
// text is capped at leadSnippetRunes runes plus the ellipsis marker.
func TestDW_1_2_EpisodicLeadSnippetNormalizedAndCapped(t *testing.T) {
	t.Run("short text under cap", func(t *testing.T) {
		short := "the deploy key rotates weekly"
		h := episodicHit("ep-1", short, "note", "2026-07-01T00:00:00Z", "evt-1")
		rh := renderHit(h)
		if rh.Text != short {
			t.Errorf("Text = %q, want unmodified %q", rh.Text, short)
		}
		if strings.HasSuffix(rh.Text, "…") {
			t.Errorf("Text has a dangling ellipsis on text under the cap: %q", rh.Text)
		}
	})

	t.Run("long text over cap", func(t *testing.T) {
		long := strings.Repeat("a", leadSnippetRunes+50)
		h := episodicHit("ep-2", long, "note", "2026-07-01T00:00:00Z", "evt-2")
		rh := renderHit(h)
		if !strings.HasSuffix(rh.Text, "…") {
			t.Errorf("Text over the cap missing ellipsis: %q", rh.Text)
		}
		textRunes := []rune(rh.Text)
		if len(textRunes) != leadSnippetRunes+1 { // +1 for the ellipsis rune
			t.Errorf("Text rune length = %d, want %d (cap + ellipsis)", len(textRunes), leadSnippetRunes+1)
		}
		if strings.Contains(rh.Text, "\n") {
			t.Errorf("Text is not single-line: %q", rh.Text)
		}
	})
}

// TestDW_1_3_RuneSafeTruncationOnEmojiBody (dirty): an episodic body padded
// so a multibyte emoji run straddles the truncation boundary must never be
// split mid-rune — the rendered gist stays valid UTF-8 with no replacement
// character, regardless of exactly where the cut lands.
func TestDW_1_3_RuneSafeTruncationOnEmojiBody(t *testing.T) {
	for pad := leadSnippetRunes - 3; pad <= leadSnippetRunes+3; pad++ {
		pad := pad
		t.Run(fmt.Sprintf("pad=%d", pad), func(t *testing.T) {
			body := strings.Repeat("a", pad) + "👻🕯️👻🕯️👻🕯️"
			h := episodicHit("ep-ghost", body, "note", "2026-07-01T00:00:00Z", "evt-ghost")
			rh := renderHit(h)

			if !utf8.ValidString(rh.Text) {
				t.Fatalf("Text is not valid UTF-8 for pad=%d: %q", pad, rh.Text)
			}
			if strings.ContainsRune(rh.Text, '�') {
				t.Errorf("Text contains the UTF-8 replacement character (split rune) for pad=%d: %q", pad, rh.Text)
			}
		})
	}
}

// TestDW_1_4_IDSourceRoundTrip: every result line's first two tab-separated
// fields parse back to exactly the original (id, source) pair, across all
// three tiers.
func TestDW_1_4_IDSourceRoundTrip(t *testing.T) {
	hits := []Hit{
		episodicHit("ep-round-1", "some text", "note", "2026-07-01T00:00:00Z", "evt-1"),
		semanticHit("sem-round-2", "alice", "knows", "bob", 5),
		graphHit("gr-round-3", "alice manages infra", "alice", "manages", "infra", 2),
	}
	for _, h := range hits {
		line := formatHitLine(renderHit(h))
		gotID, gotSource := parseIDSource(t, line)
		if gotID != h.ID || gotSource != h.Source {
			t.Errorf("round trip = (%q, %q), want (%q, %q) for line %q", gotID, gotSource, h.ID, h.Source, line)
		}
	}
}

// TestDW_1_4_IDSourceRoundTripSurvivesAdversarialContent: a statement/text
// body containing embedded tab and newline characters (an attempt to forge
// extra delimiter fields) must not corrupt the id+source round trip — the
// content is normalized before it ever reaches the line.
func TestDW_1_4_IDSourceRoundTripSurvivesAdversarialContent(t *testing.T) {
	evil := "fake\tid\tsource\ninjection"
	hits := []Hit{
		episodicHit("ep-evil", evil, "note", "2026-07-01T00:00:00Z", "evt-evil"),
		semanticHit("sem-evil", evil, evil, evil, 0),
	}
	for _, h := range hits {
		line := formatHitLine(renderHit(h))
		gotID, gotSource := parseIDSource(t, line)
		if gotID != h.ID || gotSource != h.Source {
			t.Errorf("round trip = (%q, %q), want (%q, %q) for adversarial line %q", gotID, gotSource, h.ID, h.Source, line)
		}
	}
}

// TestDW_1_5_SemanticGraphGistIsFullStatement: semantic/graph hits render
// their full statement as the line text — untruncated, since for these tiers
// the statement already IS the whole memory — under a self-addressing id.
func TestDW_1_5_SemanticGraphGistIsFullStatement(t *testing.T) {
	t.Run("semantic", func(t *testing.T) {
		h := semanticHit("sem-1", "alice", "knows", "bob", 1)
		rh := renderHit(h)
		if rh.Text == "" {
			t.Fatal("Text is empty for a semantic hit")
		}
		if rh.ID != "semantic:sem-1" {
			t.Errorf("ID = %q, want the self-addressing pair %q", rh.ID, "semantic:sem-1")
		}
	})

	t.Run("graph", func(t *testing.T) {
		statement := "alice manages the infra team"
		h := graphHit("gr-1", statement, "alice", "manages", "infra-team", 2)
		rh := renderHit(h)
		if rh.Text != statement {
			t.Errorf("Text = %q, want the untruncated statement %q", rh.Text, statement)
		}
		if rh.ID != "graph:gr-1" {
			t.Errorf("ID = %q, want the self-addressing pair %q", rh.ID, "graph:gr-1")
		}
	})
}

// TestDW_1_6_BackendHitsNotMutated: the Hit slice a Backend.Search
// implementation returns is byte-identical after a full memory_search call
// — rendering never mutates the caller's Fields (which still carries the
// full untruncated text/statement) in place. This is the gRPC/engramclient
// full-fidelity guarantee (DW-1.6), verified at the seam this phase can
// reach (internal/mcp is the file-scope boundary; api/engrampb and
// engramclient are untouched by this phase's diff).
func TestDW_1_6_BackendHitsNotMutated(t *testing.T) {
	fatText := "the deploy key rotates weekly. " + strings.Repeat("x", 1500) + " END-OF-BODY-MARKER"
	original := episodicHit("ep-fat", fatText, "note", "2026-07-01T00:00:00Z", "evt-fat")
	backend := &fixedHitsBackend{hits: []Hit{original}}

	_, decoded := searchViaWire(t, backend, map[string]any{"query": "q"})

	// The backend's own slice must be untouched.
	if backend.hits[0].Fields != original.Fields {
		t.Fatalf("Backend's Hit.Fields mutated by rendering:\n got  %q\n want %q", backend.hits[0].Fields, original.Fields)
	}
	if backend.hits[0].ID != original.ID || backend.hits[0].Score != original.Score || backend.hits[0].Source != original.Source {
		t.Fatalf("Backend's Hit mutated by rendering: got %+v, want %+v", backend.hits[0], original)
	}

	// The rendered gist is truncated (proves rendering happened) while the
	// source-of-truth Fields string above still carries the full body,
	// including the marker that a truncated snippet would have dropped.
	gotHits, _ := decoded["hits"].([]any)
	if len(gotHits) != 1 {
		t.Fatalf("rendered hits = %d, want 1", len(gotHits))
	}
	if row := fmt.Sprint(gotHits[0]); strings.Contains(row, "END-OF-BODY-MARKER") {
		t.Errorf("rendered row contains the tail marker, want it truncated: %q", row)
	}
	if !strings.Contains(backend.hits[0].Fields, "END-OF-BODY-MARKER") {
		t.Fatal("Backend's source Fields lost the tail marker — full text was not preserved untruncated")
	}
}

// TestDW_1_7_EmptyShortNewlineTextRendersCleanly (dirty): empty, sub-cap,
// and newline/tab-laden episodic text all render without panicking and
// without a dangling ellipsis.
func TestDW_1_7_EmptyShortNewlineTextRendersCleanly(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		wantEll bool
	}{
		{"empty text", "", false},
		{"sub-limit text", "short note", false},
		{"newline and tab laden text", "line one\nline two\tindented\r\nline three", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := episodicHit("ep-dirty", tc.text, "note", "2026-07-01T00:00:00Z", "evt-dirty")

			var rh renderedHit
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("renderHit panicked on %q: %v", tc.text, r)
					}
				}()
				rh = renderHit(h)
			}()

			if strings.Contains(rh.Text, "\n") || strings.Contains(rh.Text, "\t") || strings.Contains(rh.Text, "\r") {
				t.Errorf("Text not single-line for %q: %q", tc.text, rh.Text)
			}
			if hasEllipsis := strings.HasSuffix(rh.Text, "…"); hasEllipsis != tc.wantEll {
				t.Errorf("Text ellipsis = %v, want %v for %q -> %q", hasEllipsis, tc.wantEll, tc.text, rh.Text)
			}

			// The full hit-line render (and the top-level compact renderer)
			// must also survive without panicking.
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("formatHitLine panicked on %q: %v", tc.text, r)
					}
				}()
				_ = formatHitLine(rh)
			}()
		})
	}
}

// TestRenderHitMissingOrMalformedFieldsNoPanic: a hit whose Fields is empty
// or unparseable, or whose display fields are simply absent, renders without
// panicking and with an empty (not invented) gist/fields — the "hits with
// missing display fields" edge case named in the plan's Scope, beyond the
// DW floor.
func TestRenderHitMissingOrMalformedFieldsNoPanic(t *testing.T) {
	tests := []Hit{
		{ID: "no-fields", Source: "episodic", Score: 0.1, Fields: ""},
		{ID: "bad-json", Source: "episodic", Score: 0.1, Fields: "not json"},
		{ID: "no-fields-semantic", Source: "semantic", Score: 0.1, Fields: ""},
		{ID: "unregistered-source", Source: "experience", Score: 0.1, Fields: `{"text":"hello"}`},
	}
	for _, h := range tests {
		t.Run(h.ID, func(t *testing.T) {
			var rh renderedHit
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("renderHit panicked on %+v: %v", h, r)
					}
				}()
				rh = renderHit(h)
			}()
			if want := h.Source + ":" + h.ID; rh.ID != want {
				t.Errorf("renderHit lost identity: got %+v, want id=%s", rh, want)
			}
		})
	}
}

// TestCompactLinesOmissionFooterOnlyWhenOmitted: the omission summary
// footer line appears only when hits were actually left out — mirroring
// buildSearchResult's gating in budget.go, so the format change doesn't
// silently drop that signal.
func TestCompactLinesOmissionFooterOnlyWhenOmitted(t *testing.T) {
	fit := renderedResult{Hits: []renderedHit{{ID: "episodic:a", Text: "g"}}}
	if got := compactLines(fit); strings.Contains(got, "...") {
		t.Errorf("compactLines with no omission has a footer: %q", got)
	}

	omitted := renderedResult{
		Hits:    []renderedHit{{ID: "episodic:a", Text: "g"}},
		Omitted: 3,
		Hint:    "3 more hit(s) omitted to stay within the response size budget; narrow your query.",
	}
	got := compactLines(omitted)
	if !strings.Contains(got, "3 more hit(s) omitted") {
		t.Errorf("compactLines with omission missing the hint text: %q", got)
	}
}
