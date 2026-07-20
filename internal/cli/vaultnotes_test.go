package cli

// White-box tests for the Phase 3 note renderers: event notes (full
// sanitized prose, UTC-foldered path, concept footer) and concept notes
// (claim fact-sheet, folded provenance quotes, related-concept links). The
// DW-tagged tests map 1:1 onto the plan's Done-When items; the untagged
// tests cover edge cases and the security barricade the implementation
// surfaced beyond the DW floor. assertNoHazards (sanitize_test.go) and tp
// (vaultmodel_test.go) are reused as-is.

import (
	"strings"
	"testing"
)

// bodyAfterFrontmatter strips the leading YAML frontmatter block so hazard
// sweeps check only the rendered body -- our own frontmatter intentionally
// contains literal "---" delimiters, which are not the injection surface
// under test.
func bodyAfterFrontmatter(t *testing.T, content string) string {
	t.Helper()
	if !strings.HasPrefix(content, "---\n") {
		t.Fatalf("note does not start with a frontmatter block:\n%s", content)
	}
	rest := content[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatalf("frontmatter block is not terminated:\n%s", content)
	}
	return rest[end+len("\n---\n"):]
}

func TestDW_3_1_EventNoteBodyPathAndFooter(t *testing.T) {
	ev := Event{
		EventID:    "ev-1",
		Title:      "Alice meets Bob",
		Body:       "Alice met Bob in the park.\nThey talked for an hour.",
		OccurredAt: tp(t, "2026-01-15T10:00:00Z"),
		ConceptIDs: []string{"c-alice", "c-bob"},
	}
	refs := VaultRefs{
		"ev-1":    {File: "2026-01-15 alice-meets-bob", Display: "Alice meets Bob", Folder: "events/2026"},
		"c-alice": {File: "Alice", Display: "Alice", Folder: "concepts"},
		"c-bob":   {File: "Bob", Display: "Bob", Folder: "concepts"},
	}

	relPath, content := renderEvent(ev, refs)

	if want := "events/2026/2026-01-15 alice-meets-bob.md"; relPath != want {
		t.Errorf("relPath = %q, want %q", relPath, want)
	}
	if !strings.Contains(content, "engram_id: ev-1") {
		t.Errorf("content missing engram_id frontmatter: %q", content)
	}
	if !strings.Contains(content, "# Alice meets Bob") {
		t.Errorf("content missing H1: %q", content)
	}
	if !strings.Contains(content, "Alice met Bob in the park.") || !strings.Contains(content, "They talked for an hour.") {
		t.Errorf("content missing full sanitized body: %q", content)
	}
	if !strings.Contains(content, "**Concepts:**") {
		t.Errorf("content missing concepts footer: %q", content)
	}
	if !strings.Contains(content, "[[Alice|Alice]]") || !strings.Contains(content, "[[Bob|Bob]]") {
		t.Errorf("content missing resolved concept links: %q", content)
	}
	// Footer links sorted by ref.File: Alice < Bob.
	if strings.Index(content, "[[Alice|Alice]]") > strings.Index(content, "[[Bob|Bob]]") {
		t.Errorf("concept footer links not sorted by ref.File: %q", content)
	}
}

func TestDW_3_1_EventNoteUndatedFolder(t *testing.T) {
	ev := Event{EventID: "ev-2", Title: "Undated thing", Body: "no time on this one"}
	refs := VaultRefs{"ev-2": {File: "undated-thing", Display: "Undated thing", Folder: "events/undated"}}

	relPath, _ := renderEvent(ev, refs)

	if want := "events/undated/undated-thing.md"; relPath != want {
		t.Errorf("relPath = %q, want %q (nil OccurredAt -> undated bucket)", relPath, want)
	}
}

func TestDW_3_1_EventNoteUTCFoldering(t *testing.T) {
	// 23:30 on 2026-01-01 in -05:00 is 04:30 on 2026-01-02 UTC. renderEvent
	// must not recompute this itself -- it trusts refs, which Phase 2's
	// buildVaultRefs already folded via .UTC().
	ev := Event{EventID: "ev-3", Title: "Late night event"}
	refs := VaultRefs{"ev-3": {File: "2026-01-02 late-night-event", Display: "Late night event", Folder: "events/2026"}}

	relPath, _ := renderEvent(ev, refs)

	if want := "events/2026/2026-01-02 late-night-event.md"; relPath != want {
		t.Errorf("relPath = %q, want %q (UTC-foldered day, not local)", relPath, want)
	}
}

func TestDW_3_1_EventNoteNoConceptsOmitsFooter(t *testing.T) {
	ev := Event{EventID: "ev-4", Title: "Lonely event", Body: "no concepts here"}
	refs := VaultRefs{"ev-4": {File: "lonely-event", Display: "Lonely event", Folder: "events/undated"}}

	_, content := renderEvent(ev, refs)

	if strings.Contains(content, "**Concepts:**") {
		t.Errorf("content has a concepts footer with zero ConceptIDs: %q", content)
	}
}

func TestDW_3_1_EventNoteSanitizesHostileBody(t *testing.T) {
	ev := Event{
		EventID: "ev-5",
		Title:   "Hostile event",
		Body:    "---\ntitle: forged\n---\n> [!danger] pwned\n![[evil.png]]\n<iframe src=evil></iframe>\nclick obsidian://open?x",
	}
	refs := VaultRefs{"ev-5": {File: "hostile-event", Display: "Hostile event", Folder: "events/undated"}}

	_, content := renderEvent(ev, refs)

	assertNoHazards(t, bodyAfterFrontmatter(t, content))
	if !strings.Contains(content, "forged") {
		t.Errorf("hostile body should stay legible (transform-not-reject): %q", content)
	}
}

func TestDW_3_2_ConceptNoteClaimsOldestFirstWithQuote(t *testing.T) {
	c := Concept{
		EntityID: "c-1",
		Name:     "Alice Smith",
		Degree:   2,
		Claims: []Claim{
			{Statement: "Alice joined in 2027", ValidAt: tp(t, "2027-01-01T00:00:00Z"), EdgeID: "ed-2", SourceEventID: "ev-9"},
			{Statement: "Alice was born in 2000", ValidAt: tp(t, "2000-01-01T00:00:00Z"), EdgeID: "ed-1", SourceEventID: "ev-1"},
		},
		RelatedIDs: []string{"c-2"},
	}
	refs := VaultRefs{
		"c-1":  {File: "Alice Smith", Display: "Alice Smith", Folder: "concepts"},
		"c-2":  {File: "Bob", Display: "Bob", Folder: "concepts"},
		"ev-1": {File: "2000-01-01 born", Display: "Born", Folder: "events/2000"},
		"ev-9": {File: "2027-01-01 joined", Display: "Joined", Folder: "events/2027"},
	}
	events := map[string]Event{
		"ev-1": {EventID: "ev-1", Body: "Alice was born in a small hospital downtown."},
		"ev-9": {EventID: "ev-9", Body: "Alice signed the paperwork and joined the team."},
	}

	_, content := renderConcept(c, refs, events)

	if !strings.Contains(content, "## What we've learned") {
		t.Errorf("content missing claims heading: %q", content)
	}
	born := strings.Index(content, "Alice was born in 2000")
	joined := strings.Index(content, "Alice joined in 2027")
	if born == -1 || joined == -1 || born > joined {
		t.Errorf("claims not rendered oldest-first: born@%d joined@%d in %q", born, joined, content)
	}
	if !strings.Contains(content, "> [!quote]- Source: [[2000-01-01 born|Born]]") {
		t.Errorf("content missing the born-claim's attributed callout title: %q", content)
	}
	if !strings.Contains(content, "> [!quote]- Source: [[2027-01-01 joined|Joined]]") {
		t.Errorf("content missing the joined-claim's attributed callout title: %q", content)
	}
	// The callout body must be the SOURCE EVENT'S OWN PROSE (the "receipts"),
	// not merely a link to it.
	if !strings.Contains(content, "> Alice was born in a small hospital downtown.") {
		t.Errorf("content missing the born-claim's quoted source-event prose: %q", content)
	}
	if !strings.Contains(content, "> Alice signed the paperwork and joined the team.") {
		t.Errorf("content missing the joined-claim's quoted source-event prose: %q", content)
	}
}

func TestDW_3_2_ConceptNoteClaimTieBreaksOnEdgeID(t *testing.T) {
	// Same ValidAt (including both nil): EdgeID breaks the tie.
	c := Concept{
		EntityID: "c-1",
		Name:     "X",
		Degree:   2,
		Claims: []Claim{
			{Statement: "second claim text", EdgeID: "ed-b"},
			{Statement: "first claim text", EdgeID: "ed-a"},
		},
	}
	refs := VaultRefs{"c-1": {File: "X", Display: "X", Folder: "concepts"}}

	_, content := renderConcept(c, refs, nil)

	first := strings.Index(content, "first claim text")
	second := strings.Index(content, "second claim text")
	if first == -1 || second == -1 || first > second {
		t.Errorf("claims with equal (nil) ValidAt not tie-broken by EdgeID: %q", content)
	}
}

func TestDW_3_2_ConceptNoteClaimWithoutSourceQuote(t *testing.T) {
	c := Concept{
		EntityID: "c-1",
		Name:     "X",
		Degree:   2,
		Claims:   []Claim{{Statement: "an orphaned claim", EdgeID: "ed-1", SourceEventID: ""}},
	}
	refs := VaultRefs{"c-1": {File: "X", Display: "X", Folder: "concepts"}}

	_, content := renderConcept(c, refs, nil)

	if !strings.Contains(content, "an orphaned claim") {
		t.Errorf("content missing the statement: %q", content)
	}
	if strings.Contains(content, "[!quote]") {
		t.Errorf("claim with no SourceEventID should render Statement alone, no callout: %q", content)
	}
}

func TestDW_3_2_ConceptNoteClaimSourceEventNotInEventsMap(t *testing.T) {
	// SourceEventID set and present in refs, but the event itself is absent
	// from the events map (e.g. dropped upstream) -- same "Statement alone"
	// degradation as an empty SourceEventID: no body text to quote.
	c := Concept{
		EntityID: "c-1",
		Name:     "X",
		Degree:   2,
		Claims:   []Claim{{Statement: "unlinkable claim", EdgeID: "ed-1", SourceEventID: "ev-missing"}},
	}
	refs := VaultRefs{
		"c-1":        {File: "X", Display: "X", Folder: "concepts"},
		"ev-missing": {File: "missing-event", Display: "Missing Event", Folder: "events/undated"},
	}

	_, content := renderConcept(c, refs, map[string]Event{}) // events map does NOT contain ev-missing

	if !strings.Contains(content, "unlinkable claim") {
		t.Errorf("content missing the statement: %q", content)
	}
	if strings.Contains(content, "[!quote]") {
		t.Errorf("claim whose source event isn't in the events map should render Statement alone: %q", content)
	}
}

func TestDW_3_2_ConceptNoteClaimSourceEventNotInRefs(t *testing.T) {
	// SourceEventID set and present in the events map, but absent from refs
	// (should not happen given the Phase 2 contract, but defensively -- no
	// ref means no link to attribute the quote to, so render Statement alone).
	c := Concept{
		EntityID: "c-1",
		Name:     "X",
		Degree:   2,
		Claims:   []Claim{{Statement: "unlinkable claim", EdgeID: "ed-1", SourceEventID: "ev-missing"}},
	}
	refs := VaultRefs{"c-1": {File: "X", Display: "X", Folder: "concepts"}}
	events := map[string]Event{"ev-missing": {EventID: "ev-missing", Body: "some prose"}}

	_, content := renderConcept(c, refs, events)

	if !strings.Contains(content, "unlinkable claim") || strings.Contains(content, "[!quote]") {
		t.Errorf("claim whose source event isn't in refs should render Statement alone: %q", content)
	}
}

func TestDW_3_2_ConceptNoteZeroClaimsHubListsRelated(t *testing.T) {
	c := Concept{
		EntityID:   "c-1",
		Name:       "Hub With No Claims",
		Degree:     2,
		Claims:     nil,
		RelatedIDs: []string{"c-2", "c-3"},
	}
	refs := VaultRefs{
		"c-1": {File: "Hub With No Claims", Display: "Hub With No Claims", Folder: "concepts"},
		"c-2": {File: "Neighbor A", Display: "Neighbor A", Folder: "concepts"},
		"c-3": {File: "Neighbor B", Display: "Neighbor B", Folder: "concepts"},
	}

	_, content := renderConcept(c, refs, nil)

	if strings.Contains(content, "What we've learned") {
		t.Errorf("zero-claim concept should omit the claims section entirely: %q", content)
	}
	if !strings.Contains(content, "## Related concepts") {
		t.Errorf("content missing related-concepts heading: %q", content)
	}
	if !strings.Contains(content, "[[Neighbor A|Neighbor A]]") || !strings.Contains(content, "[[Neighbor B|Neighbor B]]") {
		t.Errorf("content missing related concept links: %q", content)
	}
}

func TestDW_3_2_ConceptNoteSanitizesHostileStatement(t *testing.T) {
	c := Concept{
		EntityID: "c-1",
		Name:     "X",
		Degree:   2,
		Claims: []Claim{{
			Statement: "---\nforged: yes\n---\n> [!danger] gotcha\n<script>x</script>",
			EdgeID:    "ed-1",
		}},
	}
	refs := VaultRefs{"c-1": {File: "X", Display: "X", Folder: "concepts"}}

	_, content := renderConcept(c, refs, nil)

	assertNoHazards(t, bodyAfterFrontmatter(t, content))
	if !strings.Contains(content, "forged: yes") {
		t.Errorf("hostile statement should stay legible (transform-not-reject): %q", content)
	}
}

func TestDW_3_2_ConceptNoteSanitizesHostileSourceEventProse(t *testing.T) {
	// The quoted callout body is the source event's RAW prose -- it must go
	// through sanitizeBody (then quoteBlock) exactly like the statement does.
	c := Concept{
		EntityID: "c-1",
		Name:     "X",
		Degree:   2,
		Claims:   []Claim{{Statement: "benign statement", EdgeID: "ed-1", SourceEventID: "ev-1"}},
	}
	refs := VaultRefs{
		"c-1":  {File: "X", Display: "X", Folder: "concepts"},
		"ev-1": {File: "evt", Display: "Evt", Folder: "events/undated"},
	}
	events := map[string]Event{
		"ev-1": {EventID: "ev-1", Body: "---\nforged: yes\n---\n> [!danger] gotcha\n<script>x</script>\n![[evil.png]]"},
	}

	_, content := renderConcept(c, refs, events)

	if !strings.Contains(content, "> [!quote]- Source: [[evt|Evt]]") {
		t.Errorf("content missing the callout's attribution title: %q", content)
	}
	// The attribution title line is OUR OWN trusted literal and legitimately
	// contains "> [!" -- strip it before sweeping so the hazard detectors
	// check only the quoted (attacker-controlled) body, not our own syntax.
	body := strings.Replace(bodyAfterFrontmatter(t, content), "> [!quote]- Source: [[evt|Evt]]\n", "", 1)
	assertNoHazards(t, body)
	if !strings.Contains(content, "forged: yes") {
		t.Errorf("hostile source-event prose should stay legible (transform-not-reject): %q", content)
	}
}

func TestConceptNoteNameNeverRawOnlyCleanedDisplay(t *testing.T) {
	// The H1 must come from refs' Display (already cleanInline'd), never the
	// raw Concept.Name -- proven by making them diverge.
	c := Concept{EntityID: "c-1", Name: "raw [ugly] | name", Degree: 2}
	refs := VaultRefs{"c-1": {File: "safe-name", Display: "cleaned-safe-name", Folder: "concepts"}}

	_, content := renderConcept(c, refs, nil)

	if strings.Contains(content, "raw [ugly] | name") {
		t.Errorf("raw Concept.Name leaked into output instead of the resolved Display: %q", content)
	}
	if !strings.Contains(content, "# cleaned-safe-name") {
		t.Errorf("H1 should use refs' Display: %q", content)
	}
}

func TestConceptNoteHostileAliasSurvivesYAMLEncodingIntact(t *testing.T) {
	// A hostile alias attempting frontmatter breakout must be safely
	// YAML-encoded (quoted/escaped), never hand-interpolated as a raw string
	// that could terminate the frontmatter block early.
	hostile := "evil\n---\nengram_id: hacked"
	c := Concept{EntityID: "c-1", Name: "X", Degree: 2, Aliases: []string{hostile}}
	refs := VaultRefs{"c-1": {File: "X", Display: "X", Folder: "concepts"}}

	_, content := renderConcept(c, refs, nil)

	fmEnd := strings.Index(content[4:], "---") + 4 // skip the opening "---\n" delimiter
	if fmEnd < 4 {
		t.Fatalf("no closing frontmatter delimiter found: %q", content)
	}
	frontmatter := content[:fmEnd]
	if !strings.Contains(frontmatter, "engram_id: c-1") {
		t.Errorf("hostile alias corrupted the frontmatter block -- engram_id not found before the true closing delimiter: %q", frontmatter)
	}
}

func TestDW_3_3_GhostConceptsResolveAsLinksNotFiles(t *testing.T) {
	hub := Concept{
		EntityID:   "c-hub",
		Name:       "Hub",
		Degree:     2,
		RelatedIDs: []string{"c-ghost", "c-hub2"},
	}
	hub2 := Concept{EntityID: "c-hub2", Name: "Hub2", Degree: 2, RelatedIDs: []string{"c-hub"}}
	ghost := Concept{EntityID: "c-ghost", Name: "Ghost", Degree: 1, Ghost: true, RelatedIDs: []string{"c-hub"}}
	model := VaultModel{Concepts: []Concept{hub, hub2, ghost}}
	refs := VaultRefs{
		"c-hub":   {File: "Hub", Display: "Hub", Folder: "concepts"},
		"c-hub2":  {File: "Hub2", Display: "Hub2", Folder: "concepts"},
		"c-ghost": {File: "Ghost", Display: "Ghost", Folder: "concepts"},
	}

	// The Phase-5 discipline this test simulates: only render non-ghost
	// concepts to files.
	rendered := map[string]string{}
	for _, c := range model.Concepts {
		if c.Ghost {
			continue
		}
		path, content := renderConcept(c, refs, nil)
		rendered[path] = content
	}

	if len(rendered) != 2 {
		t.Fatalf("rendered %d concept files, want 2 (only degree>=2 hubs, ghost excluded): %v", len(rendered), rendered)
	}
	if _, ok := rendered["concepts/Ghost.md"]; ok {
		t.Errorf("a ghost concept must never get its own file: %v", rendered)
	}
	hubContent := rendered["concepts/Hub.md"]
	if !strings.Contains(hubContent, "[[Ghost|Ghost]]") {
		t.Errorf("hub's related-concepts list must still resolve the ghost as a valid link target: %q", hubContent)
	}
}

func TestDW_3_4_EventNoteByteIdenticalAcrossRuns(t *testing.T) {
	ev := Event{
		EventID:    "ev-1",
		Title:      "Repeatable event",
		Body:       "some prose",
		OccurredAt: tp(t, "2026-03-04T12:00:00Z"),
		ConceptIDs: []string{"c-1", "c-2"},
	}
	refs := VaultRefs{
		"ev-1": {File: "2026-03-04 repeatable-event", Display: "Repeatable event", Folder: "events/2026"},
		"c-1":  {File: "Alpha", Display: "Alpha", Folder: "concepts"},
		"c-2":  {File: "Beta", Display: "Beta", Folder: "concepts"},
	}

	path1, content1 := renderEvent(ev, refs)
	path2, content2 := renderEvent(ev, refs)

	if path1 != path2 || content1 != content2 {
		t.Errorf("renderEvent not byte-identical across runs:\nrun1: %q %q\nrun2: %q %q", path1, content1, path2, content2)
	}
}

func TestDW_3_4_ConceptNoteByteIdenticalAcrossRuns(t *testing.T) {
	c := Concept{
		EntityID: "c-1",
		Name:     "Repeatable concept",
		Degree:   2,
		Claims: []Claim{
			{Statement: "claim one", EdgeID: "ed-1", ValidAt: tp(t, "2026-01-01T00:00:00Z"), SourceEventID: "ev-1"},
			{Statement: "claim two", EdgeID: "ed-2", ValidAt: tp(t, "2026-06-01T00:00:00Z")},
		},
		RelatedIDs: []string{"c-2"},
		Aliases:    []string{"Alt Name"},
	}
	refs := VaultRefs{
		"c-1":  {File: "Repeatable concept", Display: "Repeatable concept", Folder: "concepts"},
		"c-2":  {File: "Neighbor", Display: "Neighbor", Folder: "concepts"},
		"ev-1": {File: "2026-01-01 evt", Display: "Evt", Folder: "events/2026"},
	}
	events := map[string]Event{"ev-1": {EventID: "ev-1", Body: "the source event's prose"}}

	path1, content1 := renderConcept(c, refs, events)
	path2, content2 := renderConcept(c, refs, events)

	if path1 != path2 || content1 != content2 {
		t.Errorf("renderConcept not byte-identical across runs:\nrun1: %q %q\nrun2: %q %q", path1, content1, path2, content2)
	}
}

func TestEventNoteEmptySlugFallsBackViaRefs(t *testing.T) {
	// renderEvent does not recompute slugs -- it trusts whatever Phase 2
	// assigned, including the id-derived fallback for an unsanitizable title.
	ev := Event{EventID: "ev-abc123", Title: ""}
	refs := VaultRefs{"ev-abc123": {File: "event (ev-abc12)", Display: "ev-abc123", Folder: "events/undated"}}

	relPath, content := renderEvent(ev, refs)

	if want := "events/undated/event (ev-abc12).md"; relPath != want {
		t.Errorf("relPath = %q, want %q", relPath, want)
	}
	if !strings.Contains(content, "# ev-abc123") {
		t.Errorf("H1 should use the fallback Display: %q", content)
	}
}

func TestEventNoteConceptFooterSkipsUnresolvableID(t *testing.T) {
	// Defensive: an id absent from refs must not produce a malformed link.
	ev := Event{EventID: "ev-1", ConceptIDs: []string{"c-missing", "c-1"}}
	refs := VaultRefs{
		"ev-1": {File: "ev1", Display: "Ev1", Folder: "events/undated"},
		"c-1":  {File: "Alpha", Display: "Alpha", Folder: "concepts"},
	}

	_, content := renderEvent(ev, refs)

	if strings.Contains(content, "[[|") {
		t.Errorf("unresolved concept id produced a malformed link: %q", content)
	}
	if !strings.Contains(content, "[[Alpha|Alpha]]") {
		t.Errorf("resolvable concept link missing: %q", content)
	}
}
