package cli

// White-box tests for topic-map clustering + MOC rendering (Phase 4):
// deterministic connected-components discovery, size-bounded misc bucketing,
// collision-safe filename assignment (including the reserved "misc-"
// namespace), map content (members/timeline/out-links), and the empty-graph
// edge case.

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// chainConcepts builds n concepts wired into a single connected chain
// (c00-c01-c02-...): a deterministic, easy-to-reason-about "one big
// component" fixture. Degree/Ghost/RelatedIDs are kept internally
// consistent with how assembleConcepts would have produced them.
func chainConcepts(n int, idPrefixStr string) []Concept {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s%03d", idPrefixStr, i)
	}
	concepts := make([]Concept, n)
	for i, id := range ids {
		var related []string
		if i > 0 {
			related = append(related, ids[i-1])
		}
		if i < n-1 {
			related = append(related, ids[i+1])
		}
		sort.Strings(related)
		concepts[i] = Concept{
			EntityID:   id,
			Name:       fmt.Sprintf("Chain Concept %03d", i),
			Degree:     len(related),
			RelatedIDs: related,
			Ghost:      len(related) < hubMinDegree,
		}
	}
	return concepts
}

// isolatedConcepts builds n concepts with no edges at all: each is its own
// singleton connected component (a mostly-disconnected-graph fixture).
func isolatedConcepts(n int, idPrefixStr string) []Concept {
	concepts := make([]Concept, n)
	for i := 0; i < n; i++ {
		concepts[i] = Concept{
			EntityID: fmt.Sprintf("%s%03d", idPrefixStr, i),
			Name:     fmt.Sprintf("Solo %s %03d", idPrefixStr, i),
			Degree:   0,
			Ghost:    true,
		}
	}
	return concepts
}

func clusterByKind(t *testing.T, clusters []Cluster, kind string) []Cluster {
	t.Helper()
	var out []Cluster
	for _, c := range clusters {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// --- DW-4.4: empty graph -----------------------------------------------

func TestDW_4_4_EmptyGraphNoClusters(t *testing.T) {
	clusters := clusterConcepts(VaultModel{})
	if len(clusters) != 0 {
		t.Fatalf("got %d clusters for an empty model, want 0", len(clusters))
	}
}

// --- DW-4.2: size-bounded misc buckets + large components -------------

func TestDW_4_2_LargeComponentKeepsOwnMapNoSplit(t *testing.T) {
	model := VaultModel{Concepts: chainConcepts(10, "c")}
	clusters := clusterConcepts(model)

	concept := clusterByKind(t, clusters, "concept")
	misc := clusterByKind(t, clusters, "misc")
	if len(concept) != 1 {
		t.Fatalf("got %d concept clusters, want 1 (no artificial split of a single large component)", len(concept))
	}
	if len(misc) != 0 {
		t.Fatalf("got %d misc clusters, want 0 (nothing sub-threshold)", len(misc))
	}
	if len(concept[0].Members) != 10 {
		t.Errorf("cluster has %d members, want all 10", len(concept[0].Members))
	}
}

func TestDW_4_2_ManyTinyComponentsBoundedMiscBuckets(t *testing.T) {
	model := VaultModel{Concepts: isolatedConcepts(120, "s")}
	clusters := clusterConcepts(model)

	if concept := clusterByKind(t, clusters, "concept"); len(concept) != 0 {
		t.Fatalf("got %d concept clusters from 120 isolated concepts, want 0 (every component is a singleton, sub-threshold)", len(concept))
	}
	misc := clusterByKind(t, clusters, "misc")
	if len(misc) != 3 {
		t.Fatalf("got %d misc clusters for 120 concepts at cap 50, want 3 (ceil(120/50)) — never per-node explosion, never one giant bucket", len(misc))
	}
	total := 0
	for _, c := range misc {
		total += len(c.Members)
	}
	if total != 120 {
		t.Errorf("misc buckets hold %d concepts total, want 120 (none dropped)", total)
	}
}

func TestDW_4_2_MiscBucketCapNeverExceeded(t *testing.T) {
	model := VaultModel{Concepts: isolatedConcepts(237, "x")}
	for _, c := range clusterByKind(t, clusterConcepts(model), "misc") {
		if len(c.Members) > miscBucketCap {
			t.Errorf("misc cluster %q has %d members, want <= %d", c.RelPath, len(c.Members), miscBucketCap)
		}
	}
}

// --- DW-4.1: determinism + collision suffixing --------------------------

func TestDW_4_1_DeterministicAcrossRuns(t *testing.T) {
	concepts := append(chainConcepts(5, "big"), isolatedConcepts(30, "tiny")...)
	events := []Event{
		{EventID: "ev1", Title: "First", OccurredAt: tp(t, "2026-01-01T00:00:00Z"), ConceptIDs: []string{"big000"}},
		{EventID: "ev2", Title: "Second", OccurredAt: tp(t, "2026-01-02T00:00:00Z"), ConceptIDs: []string{"big001"}},
	}
	model := VaultModel{Concepts: concepts, Events: events}
	refs := buildVaultRefs(events, concepts)

	c1 := clusterConcepts(model)
	c2 := clusterConcepts(model)
	if !reflect.DeepEqual(c1, c2) {
		t.Fatalf("clusterConcepts is not deterministic across runs on an identical model:\nrun1=%+v\nrun2=%+v", c1, c2)
	}

	for i := range c1 {
		p1, b1 := renderMap(c1[i], refs)
		p2, b2 := renderMap(c2[i], refs)
		if p1 != p2 || b1 != b2 {
			t.Fatalf("renderMap not byte-identical across runs for cluster %d:\nrun1 path=%q\nrun2 path=%q", i, p1, p2)
		}
	}
}

func TestDW_4_1_TitleCollisionSuffixed(t *testing.T) {
	a := chainConcepts(3, "a")
	a[0].Name = "Widget" // a's highest-degree member (middle of the chain would be highest, force it onto index 0 explicitly below)
	b := chainConcepts(3, "b")
	b[0].Name = "Widget" // same sanitized base as a's top concept

	// Make index 0 the unambiguous top of each 3-chain by giving it extra degree.
	a[0].RelatedIDs = append(a[0].RelatedIDs, "a-ghost")
	a[0].Degree = len(a[0].RelatedIDs)
	b[0].RelatedIDs = append(b[0].RelatedIDs, "b-ghost")
	b[0].Degree = len(b[0].RelatedIDs)
	// The extra neighbor must resolve to a real concept so it doesn't dangle.
	a = append(a, Concept{EntityID: "a-ghost", Name: "A Ghost", Degree: 1, RelatedIDs: []string{"a000"}, Ghost: true})
	b = append(b, Concept{EntityID: "b-ghost", Name: "B Ghost", Degree: 1, RelatedIDs: []string{"b000"}, Ghost: true})

	model := VaultModel{Concepts: append(a, b...)}
	clusters := clusterConcepts(model)
	concept := clusterByKind(t, clusters, "concept")
	if len(concept) != 2 {
		t.Fatalf("got %d concept clusters, want 2 (a-component and b-component, joined only through a shared literal name, not a shared edge)", len(concept))
	}
	if concept[0].Title != "Widget" || concept[1].Title != "Widget" {
		t.Fatalf("titles = %q, %q; want both \"Widget\"", concept[0].Title, concept[1].Title)
	}
	if concept[0].RelPath == concept[1].RelPath {
		t.Fatalf("both Widget clusters resolved to the SAME path %q — homonym silently clobbered instead of suffixed", concept[0].RelPath)
	}
	for _, c := range concept {
		if !strings.Contains(c.RelPath, " (") {
			t.Errorf("cluster %q title collided but its path %q carries no collision suffix", c.Title, c.RelPath)
		}
	}
}

func TestDW_4_1_ConceptTitleCollidesWithMiscPrefixReserved(t *testing.T) {
	// 60 isolated concepts -> ceil(60/50) = 2 misc buckets: misc-01 (50), misc-02 (10).
	tiny := isolatedConcepts(60, "t")
	// A 4-node component whose top concept's name sanitizes to "misc-01" —
	// same base as the real bucket.
	big := chainConcepts(4, "z")
	big[0].Name = "Misc-01"
	big[0].RelatedIDs = append(big[0].RelatedIDs, "z-extra")
	big[0].Degree = len(big[0].RelatedIDs)
	big = append(big, Concept{EntityID: "z-extra", Name: "Extra", Degree: 1, RelatedIDs: []string{"z000"}, Ghost: true})

	model := VaultModel{Concepts: append(tiny, big...)}
	clusters := clusterConcepts(model)

	misc := clusterByKind(t, clusters, "misc")
	if len(misc) != 2 {
		t.Fatalf("got %d misc clusters, want 2", len(misc))
	}
	var realMiscPath string
	for _, m := range misc {
		if m.MiscIndex == 1 {
			realMiscPath = m.RelPath
		}
	}
	if realMiscPath != "maps/misc-01.md" {
		t.Fatalf("real misc bucket #1 path = %q, want the canonical unsuffixed \"maps/misc-01.md\" (a misc bucket must never be bumped by a colliding concept map)", realMiscPath)
	}

	concept := clusterByKind(t, clusters, "concept")
	if len(concept) != 1 {
		t.Fatalf("got %d concept clusters, want 1", len(concept))
	}
	if concept[0].RelPath == "maps/misc-01.md" {
		t.Fatalf("concept cluster titled %q clobbered the real misc-01 bucket's path", concept[0].Title)
	}
	if !strings.Contains(concept[0].RelPath, " (") {
		t.Errorf("concept cluster whose title sanitizes to a reserved misc- name must be suffixed, got %q", concept[0].RelPath)
	}
}

// --- DW-4.3: map content --------------------------------------------------

func TestDW_4_3_MapContentMembersTimelineOutLinks(t *testing.T) {
	// m1/m2/m3 form one 3-member component. "ext" is referenced only by m1
	// (an asymmetric edge — Phase 2's assembleConcepts never actually
	// produces one, since it always adds both directions, but renderMap
	// must not crash or drop data if a future producer ever did) so it
	// forms its own singleton component instead of merging with m1/m2/m3,
	// making it a legitimate cross-cluster out-link target from m1's
	// perspective.
	concepts := []Concept{
		{EntityID: "m1", Name: "Member One", Degree: 2, RelatedIDs: []string{"m2", "ext"}},
		{EntityID: "m2", Name: "Member Two", Degree: 2, RelatedIDs: []string{"m1", "m3"}},
		{EntityID: "m3", Name: "Member Three", Degree: 1, RelatedIDs: []string{"m2"}},
		// "ext" is asymmetrically referenced by m1 only, so it forms its
		// OWN singleton component (never pulled into m1/m2/m3's component)
		// while still being a valid cross-cluster out-link target from m1's
		// perspective. This models the intentionally-defensive case
		// (findComponents never crashes or panics on it) without violating
		// determinism.
		{EntityID: "ext", Name: "External Concept", Degree: 0},
	}
	events := []Event{
		{EventID: "ev-b", Title: "Later Event", OccurredAt: tp(t, "2026-02-01T00:00:00Z"), ConceptIDs: []string{"m1"}},
		{EventID: "ev-a", Title: "Earlier Event", OccurredAt: tp(t, "2026-01-01T00:00:00Z"), ConceptIDs: []string{"m2"}},
	}
	model := VaultModel{Concepts: concepts, Events: events}
	refs := buildVaultRefs(events, concepts)

	clusters := clusterConcepts(model)
	concept := clusterByKind(t, clusters, "concept")
	if len(concept) != 1 {
		t.Fatalf("got %d concept clusters, want 1 (m1/m2/m3 form one component of size 3)", len(concept))
	}
	cl := concept[0]
	if want := []string{"m1", "m2", "m3"}; !equalStrings(cl.Members, want) {
		t.Fatalf("Members = %v, want %v", cl.Members, want)
	}
	if want := []string{"ext"}; !equalStrings(cl.OutLinkIDs, want) {
		t.Fatalf("OutLinkIDs = %v, want %v (ext is outside the m1/m2/m3 cluster)", cl.OutLinkIDs, want)
	}

	_, content := renderMap(cl, refs)

	if !strings.Contains(content, "## Concepts") {
		t.Errorf("content missing member section:\n%s", content)
	}
	for _, id := range cl.Members {
		if !strings.Contains(content, refs[id].File) {
			t.Errorf("content missing member link for %s (file %q):\n%s", id, refs[id].File, content)
		}
	}

	if !strings.Contains(content, "## Timeline") {
		t.Errorf("content missing timeline section:\n%s", content)
	}
	earlierIdx := strings.Index(content, "2026-01-01")
	laterIdx := strings.Index(content, "2026-02-01")
	if earlierIdx == -1 || laterIdx == -1 || earlierIdx > laterIdx {
		t.Errorf("timeline not chronological (earlier at %d, later at %d):\n%s", earlierIdx, laterIdx, content)
	}

	if !strings.Contains(content, "## Cross-cluster links") {
		t.Errorf("content missing cross-cluster out-links section:\n%s", content)
	}
	if !strings.Contains(content, refs["ext"].File) {
		t.Errorf("content missing out-link to %q (file %q):\n%s", "ext", refs["ext"].File, content)
	}
}

func TestDW_4_3_TitleIsHighestDegreeMemberIDTieBreak(t *testing.T) {
	// Three members forming a triangle: m1<->m2, m2<->m3, m3<->m1 all
	// tied at Degree 2. Smallest id must win the tie.
	concepts := []Concept{
		{EntityID: "m1", Name: "First", Degree: 2, RelatedIDs: []string{"m2", "m3"}},
		{EntityID: "m2", Name: "Second", Degree: 2, RelatedIDs: []string{"m1", "m3"}},
		{EntityID: "m3", Name: "Third", Degree: 2, RelatedIDs: []string{"m1", "m2"}},
	}
	model := VaultModel{Concepts: concepts}
	clusters := clusterConcepts(model)
	if len(clusters) != 1 {
		t.Fatalf("got %d clusters, want 1", len(clusters))
	}
	if clusters[0].TopConceptID != "m1" {
		t.Errorf("TopConceptID = %q, want %q (smallest id wins an equal-degree tie)", clusters[0].TopConceptID, "m1")
	}
	if clusters[0].Title != "First" {
		t.Errorf("Title = %q, want %q", clusters[0].Title, "First")
	}
}

func TestDW_4_3_FilenameSanitized(t *testing.T) {
	concepts := []Concept{
		{EntityID: "m1", Name: `Weird/Name*Test"?`, Degree: 2, RelatedIDs: []string{"m2", "m3"}},
		{EntityID: "m2", Name: "Second", Degree: 1, RelatedIDs: []string{"m1"}},
		{EntityID: "m3", Name: "Third", Degree: 1, RelatedIDs: []string{"m1"}},
	}
	model := VaultModel{Concepts: concepts}
	clusters := clusterConcepts(model)
	if len(clusters) != 1 {
		t.Fatalf("got %d clusters, want 1", len(clusters))
	}
	rel := clusters[0].RelPath
	for _, illegal := range []string{"/", "*", `"`, "?"} {
		// "maps/" itself contains one legal '/', so check only the filename
		// portion after the directory prefix.
		if strings.Contains(strings.TrimPrefix(rel, "maps/"), illegal) {
			t.Errorf("RelPath %q still carries un-sanitized character %q", rel, illegal)
		}
	}
	if !strings.HasSuffix(rel, ".md") {
		t.Errorf("RelPath %q does not end in .md", rel)
	}
}

// --- Beyond the DW floor: edge cases surfaced during implementation ------

func TestMissingRefSkipsSilently(t *testing.T) {
	cl := Cluster{
		Kind:       "concept",
		Title:      "Orphan",
		RelPath:    "maps/orphan.md",
		Members:    []string{"ghost-id"},
		Timeline:   []timelineEntry{{EventID: "ghost-event"}},
		OutLinkIDs: []string{"ghost-out"},
	}
	_, content := renderMap(cl, VaultRefs{}) // empty refs: nothing resolves
	if strings.Contains(content, "[[") {
		t.Errorf("expected no wikilinks when every id is unresolvable, got:\n%s", content)
	}
}

func TestEmptyTimelineAndOutLinksOmitSections(t *testing.T) {
	cl := Cluster{Kind: "concept", Title: "Solo Map", RelPath: "maps/solo.md", Members: []string{"m1"}}
	refs := VaultRefs{"m1": noteRef{File: "m1-file", Display: "M1", Folder: "concepts"}}
	_, content := renderMap(cl, refs)
	if strings.Contains(content, "## Timeline") {
		t.Errorf("expected no Timeline section when Timeline is empty:\n%s", content)
	}
	if strings.Contains(content, "## Cross-cluster links") {
		t.Errorf("expected no Cross-cluster links section when OutLinkIDs is empty:\n%s", content)
	}
}

func TestClusterConceptsIncludesGhostsAsMembers(t *testing.T) {
	// A 3-node component where two members are ghosts (degree < hubMinDegree)
	// must still be clustered as a whole — ghosts are part of the concept
	// graph even though Phase 3 never gives them their own note file.
	concepts := chainConcepts(3, "g")
	if !concepts[0].Ghost || !concepts[2].Ghost {
		t.Fatalf("test fixture assumption broken: chain endpoints should be ghosts (degree 1)")
	}
	model := VaultModel{Concepts: concepts}
	clusters := clusterConcepts(model)
	if len(clusters) != 1 || len(clusters[0].Members) != 3 {
		t.Fatalf("got clusters=%+v, want one cluster with all 3 members (ghosts included)", clusters)
	}
}

func TestBuildTimelineTieBreaksByEventID(t *testing.T) {
	same := tp(t, "2026-03-01T00:00:00Z")
	events := []Event{
		{EventID: "ev-z", OccurredAt: same, ConceptIDs: []string{"m1"}},
		{EventID: "ev-a", OccurredAt: same, ConceptIDs: []string{"m1"}},
	}
	conceptToEvents, eventByID := indexEvents(events)
	timeline := buildTimeline([]string{"m1"}, conceptToEvents, eventByID)
	if len(timeline) != 2 || timeline[0].EventID != "ev-a" || timeline[1].EventID != "ev-z" {
		t.Errorf("timeline = %+v, want ev-a before ev-z on an exact time tie", timeline)
	}
}

func TestDigitWidthMinimumTwo(t *testing.T) {
	if got := digitWidth(0); got != 2 {
		t.Errorf("digitWidth(0) = %d, want 2", got)
	}
	if got := digitWidth(9); got != 2 {
		t.Errorf("digitWidth(9) = %d, want 2", got)
	}
	if got := digitWidth(150); got != 3 {
		t.Errorf("digitWidth(150) = %d, want 3", got)
	}
}

// --- Coverage-closing: branches no earlier fixture happened to exercise ---

// TestFilenameFallback_EmptyNameUsesMapBase drives assignClusterFilenames's
// base=="" fallback: a top concept whose Name sanitizes away to nothing
// (here, a name that is entirely trimmed punctuation) must still get a
// deterministic, collision-suffixed filename rather than an empty one.
func TestFilenameFallback_EmptyNameUsesMapBase(t *testing.T) {
	conceptByID := map[string]Concept{
		"z1": {EntityID: "z1", Name: "...", Degree: 0}, // sanitizeFilename("...") == ""
	}
	clusters := []*Cluster{
		{Kind: "concept", Key: "z1", TopConceptID: "z1"},
	}
	assignClusterFilenames(clusters, conceptByID, 2)

	if clusters[0].RelPath == "maps/map.md" {
		t.Errorf("empty-name fallback is always forced through the suffix path, got the bare unsuffixed fallback %q", clusters[0].RelPath)
	}
	if !strings.HasPrefix(clusters[0].RelPath, "maps/map (") {
		t.Errorf("RelPath = %q, want the \"map\" fallback base with a forced suffix", clusters[0].RelPath)
	}
}

// TestFilenameCollision_ThreeWayForcesExtendedSuffix drives the residual-
// clash loop's extended ("-N") fallback branch. Real clusterConcepts output
// never repeats a Key (component-smallest-id and misc-index are both
// globally unique by construction), so a THIRD cluster sharing another's
// exact Key is a deliberately pathological/adversarial fixture — it proves
// assignClusterFilenames still terminates deterministically with distinct
// names rather than looping forever or clobbering, which the plan's "no
// silent clobber" requirement demands even of hostile/malformed input.
func TestFilenameCollision_ThreeWayForcesExtendedSuffix(t *testing.T) {
	conceptByID := map[string]Concept{
		"c1": {EntityID: "c1", Name: "Widget"},
		"c2": {EntityID: "c2", Name: "Widget"},
		"c3": {EntityID: "c3", Name: "Widget"},
	}
	clusters := []*Cluster{
		{Kind: "concept", Key: "aaaaaaaa1111", TopConceptID: "c1"},
		{Kind: "concept", Key: "aaaaaaaa2222", TopConceptID: "c2"},
		{Kind: "concept", Key: "aaaaaaaa2222", TopConceptID: "c3"}, // identical Key to c2's cluster, on purpose
	}
	assignClusterFilenames(clusters, conceptByID, 2)

	seen := make(map[string]bool, len(clusters))
	extendedSuffixCount := 0
	for _, cl := range clusters {
		if seen[cl.RelPath] {
			t.Fatalf("duplicate RelPath %q across colliding clusters — silently clobbered instead of disambiguated: %+v", cl.RelPath, clusters)
		}
		seen[cl.RelPath] = true
		if strings.Contains(cl.RelPath, "-12)") {
			extendedSuffixCount++
		}
	}
	if extendedSuffixCount != 1 {
		t.Fatalf("want exactly one cluster to escalate to the extended \"-N\" suffix fallback, got %d among %+v", extendedSuffixCount, clusters)
	}
}

// TestRenderMap_UndatedTimelineEntry drives renderMap's nil-OccurredAt
// timeline branch: an event with no known time still gets its link
// rendered on the timeline, just without a date prefix.
func TestRenderMap_UndatedTimelineEntry(t *testing.T) {
	cl := Cluster{
		Kind:     "concept",
		Title:    "Undated Cluster",
		RelPath:  "maps/undated-cluster.md",
		Members:  []string{"m1"},
		Timeline: []timelineEntry{{EventID: "ev-undated", OccurredAt: nil}},
	}
	refs := VaultRefs{
		"m1":         {File: "m1-file", Display: "M1", Folder: "concepts"},
		"ev-undated": {File: "ev-undated-file", Display: "Undated Event", Folder: "events/undated"},
	}
	_, content := renderMap(cl, refs)

	if !strings.Contains(content, "[[ev-undated-file|Undated Event]]") {
		t.Fatalf("content missing the undated event's link:\n%s", content)
	}
	if strings.Contains(content, "— [[ev-undated-file") {
		t.Errorf("an undated timeline entry must not carry a date prefix:\n%s", content)
	}
}
