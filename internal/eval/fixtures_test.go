package eval_test

import (
	"testing"

	"github.com/ryanthedev/engram/internal/eval"
)

// TestFixtureKeysMapsCorpusAndQueriesToDocIDs verifies FixtureKeys' contract:
// every corpus doc's text maps to its own id, and every query's text maps to
// its first expected doc id — the correlation embed.FakeEmbedder needs to
// make paraphrase queries findable by the fake kNN signal (DW-1.3).
func TestFixtureKeysMapsCorpusAndQueriesToDocIDs(t *testing.T) {
	gs := eval.GoldSet{
		Corpus: []eval.Doc{{ID: "doc-a", Text: "statement about a"}},
		Queries: []eval.Query{
			{ID: "q1", Text: "paraphrase of a", ExpectedIDs: []string{"doc-a"}},
			{ID: "q2", Text: "no expected ids"}, // ExpectedIDs empty: must be skipped, not panic
		},
	}
	m := eval.FixtureKeys(gs)
	if m["statement about a"] != "doc-a" {
		t.Errorf("corpus text -> %q, want doc-a", m["statement about a"])
	}
	if m["paraphrase of a"] != "doc-a" {
		t.Errorf("query text -> %q, want doc-a", m["paraphrase of a"])
	}
	if _, ok := m["no expected ids"]; ok {
		t.Error("query with no expected ids should not appear in the fixture map")
	}
}
