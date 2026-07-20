package cli

// White-box unit tests for vaultknowledge.go's pure helpers — the disk/model
// layer, exercised WITHOUT a gRPC stub (mirroring vault_test.go's split from
// export_test.go: assembly-level logic here, CLI-level DW-2.x end-to-end
// tests in export_test.go).

import (
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/mcp"
)

// sampleModelAndRefs returns a minimal VaultModel/VaultRefs fixture with one
// hub concept (a real file was written) and one ghost (in the graph, no
// file) — the exact distinction hubConceptIDs/resolveKnowledgeDocs must
// respect.
func sampleModelAndRefs() (VaultModel, VaultRefs) {
	model := VaultModel{
		Concepts: []Concept{
			{EntityID: "e-hub", Name: "Hub Concept", Degree: 2},
			{EntityID: "e-ghost", Name: "Ghost Concept", Degree: 1, Ghost: true},
		},
	}
	refs := VaultRefs{
		"e-hub":   {File: "Hub Concept", Display: "Hub Concept", Folder: "concepts"},
		"e-ghost": {File: "Ghost Concept", Display: "Ghost Concept", Folder: "concepts"},
	}
	return model, refs
}

func TestDecodeKnowledgeHit(t *testing.T) {
	h := mcp.Hit{ID: "kd1", Fields: `{"title":"T","text":"Body","memory_ref":"e-1","memory_ref_name":"Name"}`}
	got := decodeKnowledgeHit("col1", "text", h)
	want := knowledgeDoc{ID: "kd1", Collection: "col1", Title: "T", Text: "Body", MemoryRef: "e-1", MemoryRefName: "Name"}
	if got != want {
		t.Errorf("decodeKnowledgeHit = %+v, want %+v", got, want)
	}
}

func TestDecodeKnowledgeHit_MalformedJSONDegradesToEmpty(t *testing.T) {
	h := mcp.Hit{ID: "kd1", Fields: "not json"}
	got := decodeKnowledgeHit("col1", "text", h)
	want := knowledgeDoc{ID: "kd1", Collection: "col1"}
	if got != want {
		t.Errorf("decodeKnowledgeHit(malformed) = %+v, want zero-value fields %+v", got, want)
	}
}

func TestDecodeKnowledgeHit_EmptyTextFieldDefaultsToText(t *testing.T) {
	h := mcp.Hit{ID: "kd1", Fields: `{"text":"fallback body"}`}
	got := decodeKnowledgeHit("col1", "", h)
	if got.Text != "fallback body" {
		t.Errorf("Text = %q, want the default %q key honored when TextField is empty", got.Text, "text")
	}
}

func TestDecodeKnowledgeHit_NonStringFieldDegradesToEmpty(t *testing.T) {
	// title is a JSON number, not a string: an untrusted-shape row must
	// degrade to "", never panic on the type assertion.
	h := mcp.Hit{ID: "kd1", Fields: `{"title":42,"text":"body"}`}
	got := decodeKnowledgeHit("col1", "text", h)
	if got.Title != "" {
		t.Errorf("Title = %q, want \"\" for a non-string field", got.Title)
	}
	if got.Text != "body" {
		t.Errorf("Text = %q, want \"body\"", got.Text)
	}
}

func TestKnowledgeDocBase(t *testing.T) {
	if base, forced := knowledgeDocBase(knowledgeDoc{Title: "My Title"}); base != "My Title" || forced {
		t.Errorf("knowledgeDocBase(titled) = (%q, %v), want (\"My Title\", false)", base, forced)
	}
	if base, forced := knowledgeDocBase(knowledgeDoc{Title: ""}); base != "doc" || !forced {
		t.Errorf("knowledgeDocBase(untitled) = (%q, %v), want (\"doc\", true)", base, forced)
	}
	// A title that sanitizes to nothing (pure control characters, which
	// sanitizeFilename drops rather than substitutes) also forces the
	// fallback, same as an empty title.
	if base, forced := knowledgeDocBase(knowledgeDoc{Title: "\x00\x01\x02"}); base != "doc" || !forced {
		t.Errorf("knowledgeDocBase(all-control-chars) = (%q, %v), want (\"doc\", true)", base, forced)
	}
}

func TestHubConceptIDs_ExcludesGhosts(t *testing.T) {
	model, _ := sampleModelAndRefs()
	hubs := hubConceptIDs(model)
	if !hubs["e-hub"] {
		t.Errorf("hubConceptIDs missing the real hub: %v", hubs)
	}
	if hubs["e-ghost"] {
		t.Errorf("hubConceptIDs included a ghost: %v", hubs)
	}
}

func TestResolveKnowledgeDocs_GhostAndMissingBothUnresolved(t *testing.T) {
	model, _ := sampleModelAndRefs()
	docs := []knowledgeDoc{
		{ID: "kd1", MemoryRef: "e-hub"},
		{ID: "kd2", MemoryRef: "e-ghost"},
		{ID: "kd3", MemoryRef: "no-such-id"},
		{ID: "kd4", MemoryRef: ""},
	}
	want := map[string]string{"kd1": "e-hub", "kd2": "", "kd3": "", "kd4": ""}
	for _, r := range resolveKnowledgeDocs(docs, model) {
		if r.ConceptID != want[r.Doc.ID] {
			t.Errorf("doc %s: ConceptID = %q, want %q", r.Doc.ID, r.ConceptID, want[r.Doc.ID])
		}
	}
}

func TestResolveKnowledgeDocs_DisplayFallsBackToFileWhenTitleEmpty(t *testing.T) {
	model, _ := sampleModelAndRefs()
	resolved := resolveKnowledgeDocs([]knowledgeDoc{{ID: "kd1", Title: ""}}, model)
	if len(resolved) != 1 {
		t.Fatalf("resolved = %v, want 1 entry", resolved)
	}
	if resolved[0].Display != resolved[0].File {
		t.Errorf("Display = %q, File = %q, want an empty title to fall back to the filename", resolved[0].Display, resolved[0].File)
	}
}

func TestMemoryRefLine(t *testing.T) {
	refs := VaultRefs{"e-hub": {File: "Hub", Display: "Hub Concept", Folder: "concepts"}}
	cases := []struct {
		name string
		r    resolvedKnowledgeDoc
		want string
	}{
		{"resolved", resolvedKnowledgeDoc{ConceptID: "e-hub"}, "**Memory:** [[Hub|Hub Concept]]"},
		{"absent", resolvedKnowledgeDoc{Doc: knowledgeDoc{MemoryRef: ""}}, ""},
		{"unresolved named", resolvedKnowledgeDoc{Doc: knowledgeDoc{MemoryRef: "e-x", MemoryRefName: "Some Name"}}, "**Memory:** unresolved: Some Name"},
		{"unresolved raw id", resolvedKnowledgeDoc{Doc: knowledgeDoc{MemoryRef: "e-x"}}, "**Memory:** unresolved: e-x"},
		{"unresolved hostile name sanitized", resolvedKnowledgeDoc{Doc: knowledgeDoc{MemoryRef: "e-x", MemoryRefName: "a[[b]]c"}}, "**Memory:** unresolved: a--b--c"},
	}
	for _, c := range cases {
		if got := memoryRefLine(c.r, refs); got != c.want {
			t.Errorf("%s: memoryRefLine = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRenderKnowledgeVault_EmptyDocsIsNoOp(t *testing.T) {
	model, refs := sampleModelAndRefs()
	dir := t.TempDir()
	stats, err := renderKnowledgeVault(dir, nil, model, refs)
	if err != nil {
		t.Fatalf("renderKnowledgeVault(no docs): %v", err)
	}
	if stats != (knowledgeStats{}) {
		t.Errorf("stats = %+v, want the zero value", stats)
	}
	tree := vaultTree(t, dir)
	if len(tree) != 0 {
		t.Errorf("renderKnowledgeVault touched an empty-docs dir: %v", treeKeys(tree))
	}
}

func TestRenderKnowledgeVault_WritesNotesAndOrderedBacklinks(t *testing.T) {
	dir := t.TempDir()
	model, refs := sampleModelAndRefs()
	if err := writeVaultNote(dir, "concepts/Hub Concept.md", "# Hub Concept\n"); err != nil {
		t.Fatalf("seeding concept note: %v", err)
	}
	docs := []knowledgeDoc{
		{ID: "kd1", Title: "First", Text: "first body", MemoryRef: "e-hub"},
		{ID: "kd2", Title: "Second", Text: "second body", MemoryRef: "e-hub"},
	}
	stats, err := renderKnowledgeVault(dir, docs, model, refs)
	if err != nil {
		t.Fatalf("renderKnowledgeVault: %v", err)
	}
	if stats.Docs != 2 || stats.Backlinks != 1 {
		t.Errorf("stats = %+v, want 2 docs, 1 backlinked concept", stats)
	}

	tree := vaultTree(t, dir)
	if !strings.Contains(tree["knowledge/First.md"], "first body") {
		t.Errorf("knowledge/First.md missing its body: %v", tree["knowledge/First.md"])
	}
	if !strings.Contains(tree["knowledge/Second.md"], "second body") {
		t.Errorf("knowledge/Second.md missing its body: %v", tree["knowledge/Second.md"])
	}
	concept := tree["concepts/Hub Concept.md"]
	idx1 := strings.Index(concept, "[[First|First]]")
	idx2 := strings.Index(concept, "[[Second|Second]]")
	if idx1 == -1 || idx2 == -1 || idx1 > idx2 {
		t.Errorf("concept note = %q, want both backlinks present in doc-id order", concept)
	}
}

func TestAppendConceptBacklinks_MissingConceptFileErrors(t *testing.T) {
	dir := t.TempDir()
	ref := noteRef{File: "Nope", Display: "Nope", Folder: "concepts"}
	docs := []resolvedKnowledgeDoc{{Doc: knowledgeDoc{ID: "kd1"}, File: "kd1-note", Display: "KD1"}}
	if err := appendConceptBacklinks(dir, ref, docs); err == nil {
		t.Fatal("appendConceptBacklinks succeeded against a concept note that was never written, want an error")
	}
}
