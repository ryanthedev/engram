//go:build integration

package server_test

// Phase-2 end-to-end integration (DW-2.1..2.4): fragment extraction through
// the REAL stack — engramclient -> gRPC -> server barricade -> live
// registry/store/retriever -> OpenSearch highlighting — plus the memory_read
// knowledge drill-down and its fail-closed authz. Uses the Phase-6
// knowledgeStack harness (scratch meta-index, unique collection names,
// full cleanup).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/ryanthedev/engram/internal/mcp"
	"github.com/ryanthedev/engram/internal/testutil"
)

// fragDocBody builds a ~5KB markdown body whose every paragraph mentions
// term, with one fenced code block holding codeToken — the DW-2.2 dirty
// case: a match inside a fence must extract without marker corruption.
func fragDocBody(term, codeToken string) string {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, "Paragraph %d discusses the %s subsystem at length, covering configuration, deployment and the trade-offs a %s integrator must weigh in production environments.\n\n", i, term, term)
	}
	fmt.Fprintf(&b, "```go\nfunc main() {\n\tresult := %s(cfg)\n\tfmt.Println(result)\n}\n```\n", codeToken)
	return b.String()
}

// fragmentSpec declares the shared collection shape for these tests.
func fragmentSpec(name string, public bool, roles ...string) mcp.CollectionSpec {
	return mcp.CollectionSpec{
		Name: name, TextField: "text", Public: public, Roles: roles,
		Mappings: map[string]mcp.FieldSpec{
			"lang": {Type: "keyword", Filterable: true},
		},
	}
}

func TestDW_2_FragmentsAndDrillDownEndToEnd(t *testing.T) {
	stack := startKnowledgeStack(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	col := scratchCollection(t, stack.base, "frag")
	if err := stack.admin.CreateCollection(ctx, fragmentSpec(col, true)); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	const codeToken = "frobnicate_widget"
	body := fragDocBody("gadget", codeToken)
	docs := make([]mcp.KnowledgeDoc, 16)
	for i := range docs {
		docs[i] = mcp.KnowledgeDoc{
			ID: fmt.Sprintf("doc-%02d", i), Title: fmt.Sprintf("Doc %02d", i),
			Text:   body,
			Fields: map[string]any{"lang": "go"},
		}
	}
	if _, err := stack.admin.KnowledgeIngest(ctx, col, "it2", "h1", docs); err != nil {
		t.Fatalf("KnowledgeIngest: %v", err)
	}
	testutil.RefreshIndex(t, stack.base, "knowledge-"+col)

	// --- DW-2.1: fragments by default, NO body in fields_json ----------
	hits, err := stack.reader.KnowledgeSearch(ctx, col, "gadget", nil, nil, 100, false)
	if err != nil {
		t.Fatalf("KnowledgeSearch(default): %v", err)
	}
	if len(hits) != 16 {
		t.Fatalf("got %d hits, want 16", len(hits))
	}
	for _, h := range hits {
		var fields map[string]any
		if err := json.Unmarshal([]byte(h.Fields), &fields); err != nil {
			t.Fatalf("fields_json does not decode: %v", err)
		}
		if _, hasBody := fields["text"]; hasBody {
			t.Fatalf("hit %s fields_json still carries the body under default suppression", h.ID)
		}
		if len(h.Fragments) == 0 {
			t.Fatalf("hit %s has no fragments under the default", h.ID)
		}
		if len(h.Fragments) > 3 {
			t.Fatalf("hit %s has %d fragments, want <= the default 3", h.ID, len(h.Fragments))
		}
		if h.Collection != col {
			t.Fatalf("hit %s collection = %q, want %q", h.ID, h.Collection, col)
		}
	}
	// The budget arithmetic behind DW-2.1: a 12-hit fragment page fits the
	// 16 KB tool budget where a single full body dominated it before.
	page, _ := json.Marshal(hits[:12])
	if len(page) > 16384 {
		t.Errorf("12 fragment hits serialize to %d bytes, want <= 16384", len(page))
	}

	// --- DW-2.2: markers off by default, even inside a code fence ------
	codeHits, err := stack.reader.KnowledgeSearch(ctx, col, codeToken, nil, nil, 1, false)
	if err != nil || len(codeHits) == 0 {
		t.Fatalf("KnowledgeSearch(%s): hits=%d err=%v", codeToken, len(codeHits), err)
	}
	foundInFence := false
	for _, frag := range codeHits[0].Fragments {
		if !strings.Contains(body, frag) {
			t.Errorf("fragment is not a verbatim substring of the stored body (marker injection?): %q", frag)
		}
		if strings.Contains(frag, codeToken+"(cfg)") {
			foundInFence = true
		}
	}
	if !foundInFence {
		t.Errorf("no fragment extracted the fenced-code match verbatim: %v", codeHits[0].Fragments)
	}

	// --- DW-2.2: per-collection tags wrap fragments in exactly them ----
	tagged := fragmentSpec(col, true)
	tagged.HighlightPreTag, tagged.HighlightPostTag = "«", "»"
	if err := stack.admin.UpdateCollection(ctx, tagged); err != nil {
		t.Fatalf("UpdateCollection(tags): %v", err)
	}
	taggedHits, err := stack.reader.KnowledgeSearch(ctx, col, codeToken, nil, nil, 1, false)
	if err != nil || len(taggedHits) == 0 {
		t.Fatalf("KnowledgeSearch(tagged): hits=%d err=%v", len(taggedHits), err)
	}
	if want := "«" + codeToken + "»"; !strings.Contains(strings.Join(taggedHits[0].Fragments, "\n"), want) {
		t.Errorf("tagged fragments missing %q: %v", want, taggedHits[0].Fragments)
	}
	// Restore markers-off for the remaining sections.
	if err := stack.admin.UpdateCollection(ctx, fragmentSpec(col, true)); err != nil {
		t.Fatalf("UpdateCollection(untag): %v", err)
	}

	// --- DW-2.2b: filter-only search -> scalars only, not an error -----
	filterHits, err := stack.reader.KnowledgeSearch(ctx, col, "",
		[]mcp.Predicate{{Field: "lang", Op: "term", Value: "go"}}, nil, 5, false)
	if err != nil {
		t.Fatalf("KnowledgeSearch(filter-only): %v", err)
	}
	if len(filterHits) == 0 {
		t.Fatal("filter-only search returned no hits")
	}
	for _, h := range filterHits {
		var fields map[string]any
		_ = json.Unmarshal([]byte(h.Fields), &fields)
		if _, hasBody := fields["text"]; hasBody {
			t.Errorf("filter-only hit %s carries the body", h.ID)
		}
		if len(h.Fragments) != 0 {
			t.Errorf("filter-only hit %s carries fragments %v, want none (no matched terms)", h.ID, h.Fragments)
		}
		if fields["lang"] != "go" {
			t.Errorf("filter-only hit %s lost its scalars: %v", h.ID, fields)
		}
	}

	// --- DW-2.3: full_body restores today's whole-body behavior --------
	fullHits, err := stack.reader.KnowledgeSearch(ctx, col, "gadget", nil, nil, 1, true)
	if err != nil || len(fullHits) == 0 {
		t.Fatalf("KnowledgeSearch(full_body): hits=%d err=%v", len(fullHits), err)
	}
	var fullFields map[string]any
	if err := json.Unmarshal([]byte(fullHits[0].Fields), &fullFields); err != nil {
		t.Fatalf("full_body fields_json does not decode: %v", err)
	}
	if fullFields["text"] != body {
		t.Errorf("full_body hit does not carry the whole stored body inline")
	}
	if len(fullHits[0].Fragments) != 0 {
		t.Errorf("full_body hit carries fragments %v, want none", fullHits[0].Fragments)
	}

	// --- DW-2.4: memory_read drill-down on a readable collection -------
	read, err := stack.reader.Read(ctx, "doc-03", col)
	if err != nil {
		t.Fatalf("Read(doc-03, %s): %v", col, err)
	}
	if read.Source != col || read.Fields["text"] != body || read.Fields["title"] != "Doc 03" {
		t.Errorf("Read returned a partial document: source=%q fields keys=%d", read.Source, len(read.Fields))
	}

	// Unknown source: self-correcting, names the vocabulary.
	_, err = stack.reader.Read(ctx, "doc-03", "no-such-collection")
	wantCode(t, err, codes.InvalidArgument, "Read(unknown source)")
	if !strings.Contains(err.Error(), "knowledge_collections") {
		t.Errorf("unknown-source error is not self-correcting: %v", err)
	}
}

// TestDW_2_4_DrillDownFailsClosedEndToEnd: an unreadable (role-gated)
// collection reads as the SAME opaque not-found as a missing doc — through
// the real auth interceptor and barricade.
func TestDW_2_4_DrillDownFailsClosedEndToEnd(t *testing.T) {
	stack := startKnowledgeStack(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	col := scratchCollection(t, stack.base, "gated")
	if err := stack.admin.CreateCollection(ctx, fragmentSpec(col, false, "curator")); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if _, err := stack.admin.KnowledgeIngest(ctx, col, "it2", "h1", []mcp.KnowledgeDoc{
		{ID: "secret-1", Text: "restricted content", Fields: map[string]any{"lang": "go"}},
	}); err != nil {
		t.Fatalf("KnowledgeIngest: %v", err)
	}
	testutil.RefreshIndex(t, stack.base, "knowledge-"+col)

	// The role-holder reads the doc.
	read, err := stack.curator.Read(ctx, "secret-1", col)
	if err != nil {
		t.Fatalf("curator Read: %v", err)
	}
	if read.Fields["text"] != "restricted content" {
		t.Errorf("curator read = %+v, want the full doc", read.Fields)
	}

	// The role-less reader is denied with the opaque not-found — for an id
	// that EXISTS and for one that doesn't, indistinguishably.
	_, denyErr := stack.reader.Read(ctx, "secret-1", col)
	wantCode(t, denyErr, codes.NotFound, "reader Read(existing id, unreadable collection)")
	_, missErr := stack.curator.Read(ctx, "never-existed", col)
	wantCode(t, missErr, codes.NotFound, "curator Read(missing id)")
	if denyErr.Error() != missErr.Error() {
		t.Errorf("denial (%q) and absence (%q) are distinguishable: existence leak", denyErr, missErr)
	}
}
