package cli

// White-box tests for the vault model assembly (Phase 2): hub/ghost rule,
// normalized-name collapse, event dedupe, the source_ids → event_id claim
// join, and byte-identical determinism of the model + VaultRefs.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/engramclient"
)

func tp(t *testing.T, s string) *time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad fixture time %q: %v", s, err)
	}
	return &ts
}

func conceptByID(t *testing.T, m VaultModel, id string) Concept {
	t.Helper()
	for _, c := range m.Concepts {
		if c.EntityID == id {
			return c
		}
	}
	t.Fatalf("concept %q not in model (have %d concepts)", id, len(m.Concepts))
	return Concept{}
}

func TestDW_2_3_HubRuleGhostBelowDegree2(t *testing.T) {
	entities := []engramclient.ExportEntity{
		{ID: "e-a", Name: "alpha"},
		{ID: "e-b", Name: "beta"},
		{ID: "e-c", Name: "gamma"},
		{ID: "e-d", Name: "delta"},
	}
	edges := []engramclient.ExportEdge{
		{ID: "ed1", FromEntityID: "e-a", ToEntityID: "e-b", Statement: "alpha relates beta"},
		{ID: "ed2", FromEntityID: "e-a", ToEntityID: "e-c", Statement: "alpha relates gamma"},
		{ID: "ed3", FromEntityID: "e-a", ToEntityID: "e-b", Statement: "alpha relates beta again"}, // duplicate endpoint pair
		{ID: "ed4", FromEntityID: "e-a", ToEntityID: "e-a", Statement: "self loop"},
		{ID: "ed5", FromEntityID: "e-a", ToEntityID: "e-x", Statement: "endpoint not exported"},
	}
	m, _ := buildVaultModel(nil, entities, edges)

	a := conceptByID(t, m, "e-a")
	if a.Degree != 2 || a.Ghost {
		t.Errorf("alpha: Degree=%d Ghost=%v, want Degree=2 Ghost=false (distinct endpoints only: duplicate pair, self-loop, unknown endpoint all excluded)", a.Degree, a.Ghost)
	}
	if want := []string{"e-b", "e-c"}; !equalStrings(a.RelatedIDs, want) {
		t.Errorf("alpha RelatedIDs = %v, want %v", a.RelatedIDs, want)
	}
	// The unknown-endpoint edge still contributes its Statement as a claim.
	if len(a.Claims) != 5 {
		t.Errorf("alpha has %d claims, want 5 (all incident edges incl. self-loop and unknown endpoint)", len(a.Claims))
	}
	for id, wantDeg := range map[string]int{"e-b": 1, "e-c": 1, "e-d": 0} {
		c := conceptByID(t, m, id)
		if c.Degree != wantDeg || !c.Ghost {
			t.Errorf("%s: Degree=%d Ghost=%v, want Degree=%d Ghost=true", id, c.Degree, c.Ghost, wantDeg)
		}
	}
}

func TestDW_2_3_NormalizedNameCollapse(t *testing.T) {
	variant := `  "alice   SMITH" `
	entities := []engramclient.ExportEntity{
		{ID: "e-2", Name: variant, Aliases: []string{"Ally"}, SourceIDs: []string{"ev-2"}},
		{ID: "e-1", Name: "Alice Smith", Aliases: []string{"Al"}, SourceIDs: []string{"ev-1"}},
		{ID: "e-3", Name: "Bob"},
	}
	edges := []engramclient.ExportEdge{
		{ID: "ed1", FromEntityID: "e-2", ToEntityID: "e-3", Statement: "alice knows bob"},
	}
	m, refs := buildVaultModel(nil, entities, edges)

	if len(m.Concepts) != 2 {
		t.Fatalf("got %d concepts, want 2 (alice variants collapsed + bob)", len(m.Concepts))
	}
	alice := conceptByID(t, m, "e-1") // canonical = smallest member id
	if alice.Name != "Alice Smith" {
		t.Errorf("canonical name = %q, want %q", alice.Name, "Alice Smith")
	}
	for _, want := range []string{"Al", "Ally", variant} {
		if !containsString(alice.Aliases, want) {
			t.Errorf("aliases %v missing %q", alice.Aliases, want)
		}
	}
	// The variant member's edge resolves through the canonical id.
	if want := []string{"e-3"}; !equalStrings(alice.RelatedIDs, want) {
		t.Errorf("alice RelatedIDs = %v, want %v (edge from merged member e-2)", alice.RelatedIDs, want)
	}
	if len(alice.Claims) != 1 || alice.Claims[0].Statement != "alice knows bob" {
		t.Errorf("alice claims = %+v, want the merged member's claim", alice.Claims)
	}
	if _, ok := refs["e-2"]; ok {
		t.Errorf("merged-away entity e-2 must not get its own ref")
	}
	if _, ok := refs["e-1"]; !ok {
		t.Errorf("canonical concept e-1 missing from VaultRefs")
	}
}

func TestNormalizeConceptName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"lowercase", "Alice Smith", "alice smith"},
		{"internal whitespace collapsed", " alice \t  smith ", "alice smith"},
		{"surrounding quotes stripped", `"Alice Smith"`, "alice smith"},
		{"surrounding punctuation stripped", "(alice smith!)", "alice smith"},
		{"internal punctuation preserved", "u.s. army", "u.s. army"},
		{"empty stays empty", "  '' ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeConceptName(tt.in); got != tt.want {
				t.Errorf("normalizeConceptName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDW_2_3_SharedSourceIDDoesNotMerge(t *testing.T) {
	entities := []engramclient.ExportEntity{
		{ID: "e-1", Name: "alpha", SourceIDs: []string{"ev-1"}},
		{ID: "e-2", Name: "beta", SourceIDs: []string{"ev-1"}},
	}
	m, _ := buildVaultModel(nil, entities, nil)
	if len(m.Concepts) != 2 {
		t.Fatalf("distinct-named entities sharing a source_id merged: got %d concepts, want 2", len(m.Concepts))
	}
}

func TestDW_2_3_EmptyNamesNeverCollapseTogether(t *testing.T) {
	entities := []engramclient.ExportEntity{
		{ID: "e-1", Name: ""},
		{ID: "e-2", Name: `""`}, // normalizes to empty too
	}
	m, _ := buildVaultModel(nil, entities, nil)
	if len(m.Concepts) != 2 {
		t.Fatalf("entities with empty normalized names fused: got %d concepts, want 2", len(m.Concepts))
	}
}

func TestDW_2_3_DuplicateEventIDsDedupeDeterministically(t *testing.T) {
	early := engramclient.ExportEpisodic{EventID: "ev-1", Kind: "note", Text: "earlier", OccurredAt: tp(t, "2026-07-01T00:00:00Z")}
	late := engramclient.ExportEpisodic{EventID: "ev-1", Kind: "note", Text: "later", OccurredAt: tp(t, "2026-07-02T00:00:00Z")}
	undated := engramclient.ExportEpisodic{EventID: "ev-1", Kind: "note", Text: "undated"}

	tests := []struct {
		name     string
		in       []engramclient.ExportEpisodic
		wantBody string
	}{
		{"earliest wins", []engramclient.ExportEpisodic{late, early}, "earlier"},
		{"order independent", []engramclient.ExportEpisodic{early, late}, "earlier"},
		{"nil OccurredAt sorts last", []engramclient.ExportEpisodic{undated, late}, "later"},
		{"same time ties break on kind", []engramclient.ExportEpisodic{
			{EventID: "ev-1", Kind: "zeta", Text: "kz", OccurredAt: early.OccurredAt},
			{EventID: "ev-1", Kind: "alpha", Text: "ka", OccurredAt: early.OccurredAt},
		}, "ka"},
		{"same time and kind ties break on text", []engramclient.ExportEpisodic{
			{EventID: "ev-1", Kind: "note", Text: "bbb", OccurredAt: early.OccurredAt},
			{EventID: "ev-1", Kind: "note", Text: "aaa", OccurredAt: early.OccurredAt},
		}, "aaa"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := buildVaultModel(tt.in, nil, nil)
			if len(m.Events) != 1 {
				t.Fatalf("got %d events, want 1", len(m.Events))
			}
			if m.Events[0].Body != tt.wantBody {
				t.Errorf("dedupe winner body = %q, want %q", m.Events[0].Body, tt.wantBody)
			}
		})
	}

	t.Run("empty event_id skipped", func(t *testing.T) {
		m, refs := buildVaultModel([]engramclient.ExportEpisodic{{EventID: "", Text: "orphan"}}, nil, nil)
		if len(m.Events) != 0 || len(refs) != 0 {
			t.Errorf("empty-event_id record produced Events=%d refs=%d, want 0/0", len(m.Events), len(refs))
		}
	})
}

func TestDW_2_4_ClaimJoinSourceIDsToEventID(t *testing.T) {
	episodics := []engramclient.ExportEpisodic{
		{EventID: "ev-1", Text: "the source prose", OccurredAt: tp(t, "2026-07-01T09:00:00Z")},
	}
	entities := []engramclient.ExportEntity{
		{ID: "e-1", Name: "alpha", SourceIDs: []string{"ev-1"}},
		{ID: "e-2", Name: "beta"},
	}
	edges := []engramclient.ExportEdge{
		// Smallest source id WITH an exported event wins (ev-0 absent, ev-1 present).
		{ID: "ed1", FromEntityID: "e-1", ToEntityID: "e-2", Statement: "s1", SourceIDs: []string{"ev-0", "ev-1"}, ValidAt: tp(t, "2026-07-02T00:00:00Z")},
		// No exported event at all: id kept, claim survives (renders quote-less).
		{ID: "ed2", FromEntityID: "e-1", ToEntityID: "e-2", Statement: "s2", SourceIDs: []string{"ev-9"}, ValidAt: tp(t, "2026-07-01T00:00:00Z")},
		// No source ids: empty provenance, claim still present.
		{ID: "ed3", FromEntityID: "e-1", ToEntityID: "e-2", Statement: "s3", ValidAt: tp(t, "2026-07-01T00:00:00Z")},
	}
	m, _ := buildVaultModel(episodics, entities, edges)

	a := conceptByID(t, m, "e-1")
	if len(a.Claims) != 3 {
		t.Fatalf("got %d claims, want 3 (join is total — absent events drop nothing)", len(a.Claims))
	}
	// Sorted by ValidAt then EdgeID: ed2(07-01), ed3(07-01), ed1(07-02).
	wantOrder := []struct{ edge, src string }{{"ed2", "ev-9"}, {"ed3", ""}, {"ed1", "ev-1"}}
	for i, want := range wantOrder {
		got := a.Claims[i]
		if got.EdgeID != want.edge || got.SourceEventID != want.src {
			t.Errorf("claim[%d] = {EdgeID:%q SourceEventID:%q}, want {%q %q}", i, got.EdgeID, got.SourceEventID, want.edge, want.src)
		}
	}
	// Claims attach to BOTH resolvable endpoints.
	if b := conceptByID(t, m, "e-2"); len(b.Claims) != 3 {
		t.Errorf("to-endpoint got %d claims, want 3", len(b.Claims))
	}
	// ev-9 is not an exported event — the quote-less contract.
	for _, ev := range m.Events {
		if ev.EventID == "ev-9" {
			t.Errorf("ev-9 must not exist as an Event")
		}
	}
	// Entity source_ids → event join: ev-1 lists the concept (ghosts included).
	if len(m.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(m.Events))
	}
	if want := []string{"e-1"}; !equalStrings(m.Events[0].ConceptIDs, want) {
		t.Errorf("event ConceptIDs = %v, want %v", m.Events[0].ConceptIDs, want)
	}
}

func TestDW_2_4_ByteIdenticalAcrossRunsAndPermutations(t *testing.T) {
	episodics := []engramclient.ExportEpisodic{
		{EventID: "ev-2", Kind: "note", Text: "second event\nbody", OccurredAt: tp(t, "2026-07-02T12:00:00Z")},
		{EventID: "ev-1", Kind: "note", Text: "first event", OccurredAt: tp(t, "2026-07-01T12:00:00Z")},
		{EventID: "ev-3", Kind: "note", Text: "undated event"},
		{EventID: "ev-1", Kind: "note", Text: "duplicate later", OccurredAt: tp(t, "2026-07-05T12:00:00Z")},
	}
	entities := []engramclient.ExportEntity{
		{ID: "e-1", Name: "Widget", Aliases: []string{"w"}, SourceIDs: []string{"ev-1"}},
		{ID: "e-2", Name: "widget", SourceIDs: []string{"ev-2"}}, // collapses into e-1
		{ID: "e-3", Name: "Gadget", SourceIDs: []string{"ev-2"}},
		{ID: "e-4", Name: "Sprocket"},
	}
	edges := []engramclient.ExportEdge{
		{ID: "ed-1", FromEntityID: "e-1", ToEntityID: "e-3", Statement: "widget uses gadget", SourceIDs: []string{"ev-1"}, ValidAt: tp(t, "2026-07-01T13:00:00Z")},
		{ID: "ed-2", FromEntityID: "e-2", ToEntityID: "e-4", Statement: "widget contains sprocket", SourceIDs: []string{"ev-2"}, ValidAt: tp(t, "2026-07-02T13:00:00Z")},
		{ID: "ed-3", FromEntityID: "e-3", ToEntityID: "e-4", Statement: "gadget beside sprocket", SourceIDs: []string{"ev-gone"}},
	}

	encode := func(m VaultModel, r VaultRefs) string {
		t.Helper()
		mb, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal model: %v", err)
		}
		rb, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal refs: %v", err)
		}
		return string(mb) + "\n" + string(rb)
	}

	m1, r1 := buildVaultModel(episodics, entities, edges)
	m2, r2 := buildVaultModel(episodics, entities, edges)
	if a, b := encode(m1, r1), encode(m2, r2); a != b {
		t.Errorf("two runs over identical input differ:\n%s\nvs\n%s", a, b)
	}

	// Input order must not leak: reversed slices produce identical bytes.
	m3, r3 := buildVaultModel(reversed(episodics), reversed(entities), reversed(edges))
	if a, b := encode(m1, r1), encode(m3, r3); a != b {
		t.Errorf("permuted input changed output:\n%s\nvs\n%s", a, b)
	}
}

func TestVaultRefsFoldersFilesAndCollisions(t *testing.T) {
	episodics := []engramclient.ExportEpisodic{
		{EventID: "ev-1", Text: "Standup notes", OccurredAt: tp(t, "2026-07-19T10:00:00Z")},
		{EventID: "ev-2", Text: "Widget"}, // undated; title collides with the concept below
		{EventID: "ev-3", Text: ""},       // no legible title at all
	}
	entities := []engramclient.ExportEntity{
		{ID: "e-1", Name: "Widget"},
		{ID: "e-2", Name: "Lone"}, // degree 0 → ghost, still a link target
	}
	m, refs := buildVaultModel(episodics, entities, nil)

	dated := refs["ev-1"]
	if dated.Folder != "events/2026" || dated.File != "2026-07-19 Standup notes" {
		t.Errorf("dated event ref = %+v, want Folder events/2026, File %q", dated, "2026-07-19 Standup notes")
	}
	if got := refs["ev-2"].Folder; got != "events/undated" {
		t.Errorf("undated event Folder = %q, want events/undated", got)
	}
	if got := refs["e-1"].Folder; got != "concepts" {
		t.Errorf("concept Folder = %q, want concepts", got)
	}

	// "Widget" event vs "Widget" concept: global case-insensitive homonyms —
	// BOTH suffixed, files unique.
	evFile, coFile := refs["ev-2"].File, refs["e-1"].File
	if strings.EqualFold(evFile, coFile) {
		t.Errorf("homonym files not disambiguated: event %q vs concept %q", evFile, coFile)
	}
	for id, f := range map[string]string{"ev-2": evFile, "e-1": coFile} {
		if !strings.Contains(f, "(") {
			t.Errorf("%s file %q lacks a collision suffix", id, f)
		}
	}

	// Titleless event: id-derived fallback, never an empty filename.
	if ref := refs["ev-3"]; ref.File == "" || ref.Display == "" {
		t.Errorf("titleless event ref = %+v, want non-empty File and Display", ref)
	}

	// Ghost concept is still a ref (link target), and files are globally
	// unique case-insensitively.
	ghost := conceptByID(t, m, "e-2")
	if !ghost.Ghost {
		t.Fatalf("fixture drift: e-2 should be a ghost")
	}
	if _, ok := refs["e-2"]; !ok {
		t.Errorf("ghost concept missing from VaultRefs")
	}
	seen := make(map[string]string)
	for id, ref := range refs {
		key := strings.ToLower(ref.File)
		if other, dup := seen[key]; dup {
			t.Errorf("file %q assigned to both %s and %s", ref.File, other, id)
		}
		seen[key] = id
	}
}

func TestEventTitleDerivation(t *testing.T) {
	tests := []struct {
		name string
		text string
		id   string
		want string
	}{
		{"first non-blank line", "\n\nActual title line\nrest of body", "ev-1", "Actual title line"},
		{"inline hazards cleaned", "[[Sneaky]] | title\tx", "ev-1", "--Sneaky-- - title x"},
		{"empty text falls back to id", "", "event-abcdefgh-rest", "event-ab"},
		{"whitespace-only falls back to id", " \n\t\n", "event-abcdefgh-rest", "event-ab"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := eventTitle(tt.text, tt.id); got != tt.want {
				t.Errorf("eventTitle(%q, %q) = %q, want %q", tt.text, tt.id, got, tt.want)
			}
		})
	}
	t.Run("long first line capped", func(t *testing.T) {
		got := eventTitle(strings.Repeat("x", 200), "ev-1")
		if len([]rune(got)) != maxEventTitleRunes {
			t.Errorf("title length = %d runes, want %d", len([]rune(got)), maxEventTitleRunes)
		}
	})
}

func TestEmptyIDEntitySkipped(t *testing.T) {
	entities := []engramclient.ExportEntity{
		{ID: "", Name: "nameless but named"},
		{ID: "e-1", Name: "kept"},
	}
	m, refs := buildVaultModel(nil, entities, nil)
	if len(m.Concepts) != 1 || m.Concepts[0].EntityID != "e-1" {
		t.Errorf("concepts = %+v, want only e-1 (empty-id entity is unlinkable and skipped)", m.Concepts)
	}
	if _, ok := refs[""]; ok || len(refs) != 1 {
		t.Errorf("refs = %v, want exactly the e-1 ref", refs)
	}
}

func TestVaultRefsResidualClashExtendsSuffix(t *testing.T) {
	// Concept and event are homonyms sharing the same 8-char id prefix, so
	// both first-choice suffixed names clash ("Widget (abcdefgh)"); a third
	// candidate literally named like the extended full-id suffix forces the
	// terminal counter fallback too.
	episodics := []engramclient.ExportEpisodic{
		{EventID: "abcdefgh-ev", Text: "Widget"},
	}
	entities := []engramclient.ExportEntity{
		{ID: "abcdefgh-en", Name: "Widget"},
		{ID: "aaa-mimic", Name: "Widget (abcdefgh-ev)"}, // crafted mimic of the extended name
	}
	_, refs := buildVaultModel(episodics, entities, nil)

	files := map[string]string{}
	seen := map[string]string{}
	for id, ref := range refs {
		files[id] = ref.File
		key := strings.ToLower(ref.File)
		if other, dup := seen[key]; dup {
			t.Errorf("file %q assigned to both %s and %s", ref.File, other, id)
		}
		seen[key] = id
	}
	if len(refs) != 3 {
		t.Fatalf("got %d refs %v, want 3", len(refs), files)
	}
	if got := files["abcdefgh-en"]; got != "Widget (abcdefgh)" {
		t.Errorf("first sorted homonym file = %q, want %q", got, "Widget (abcdefgh)")
	}
	// The event's extended-prefix name is taken by the mimic, so it must
	// land on the counter fallback — still deterministic, still unique.
	if got := files["abcdefgh-ev"]; got != "Widget (abcdefgh-ev-12)" {
		t.Errorf("residual-clash file = %q, want %q", got, "Widget (abcdefgh-ev-12)")
	}
}

func TestVaultRefsForcedFallbackNames(t *testing.T) {
	// Titles/names whose every rune is stripped by sanitizeFilename fall
	// back to kind-named bases with a forced id suffix.
	episodics := []engramclient.ExportEpisodic{
		{EventID: "ev-dots", Text: "..."}, // cleanInline keeps dots; sanitizeFilename trims them all
	}
	entities := []engramclient.ExportEntity{
		{ID: "e-dots", Name: ". . ."},
	}
	_, refs := buildVaultModel(episodics, entities, nil)
	if got := refs["ev-dots"].File; got != "event (ev-dots)" {
		t.Errorf("forced event file = %q, want %q", got, "event (ev-dots)")
	}
	if got := refs["e-dots"].File; got != "concept (e-dots)" {
		t.Errorf("forced concept file = %q, want %q", got, "concept (e-dots)")
	}
}

func TestVaultRefsCrossKindIDCollision(t *testing.T) {
	// Pathological: one id used by both an event and an entity. The sorted
	// (id, folder) order makes the concept ("concepts" < "events/...") win
	// the map slot deterministically; the second candidate is skipped, not
	// silently clobbered mid-map.
	episodics := []engramclient.ExportEpisodic{
		{EventID: "shared-1", Text: "event title"},
	}
	entities := []engramclient.ExportEntity{
		{ID: "shared-1", Name: "Concepto"},
	}
	m, refs := buildVaultModel(episodics, entities, nil)
	if len(m.Events) != 1 || len(m.Concepts) != 1 {
		t.Fatalf("model lost records: %d events, %d concepts", len(m.Events), len(m.Concepts))
	}
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1 (one slot per id)", len(refs))
	}
	if got := refs["shared-1"].Folder; got != "concepts" {
		t.Errorf("collided ref Folder = %q, want concepts (first in sorted (id, folder) order)", got)
	}
}

// --- small fixture helpers ---

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func reversed[T any](in []T) []T {
	out := make([]T, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}
