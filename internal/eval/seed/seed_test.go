package seed_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/eval"
	"github.com/ryanthedev/engram/internal/eval/seed"
)

// fakeEmbedder is a canned embed.Embedder test double.
type fakeEmbedder struct {
	vecs [][]float32
	err  error
}

func (f fakeEmbedder) Embed(context.Context, []string) ([][]float32, error) { return f.vecs, f.err }
func (f fakeEmbedder) Info() embed.ModelInfo {
	return embed.ModelInfo{Model: "m", Revision: "r", Dim: 4}
}

// TestCorpusIndexesEachDocAtLiteralID proves the harness-seeding contract:
// every corpus doc is PUT under its own literal id (bypassing the
// content-addressed write protocol), so eval.Query.ExpectedIDs match
// Retriever Hit.ID directly.
func TestCorpusIndexesEachDocAtLiteralID(t *testing.T) {
	var mu sync.Mutex
	var puts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			mu.Lock()
			puts = append(puts, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_refresh"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	gs := eval.GoldSet{Corpus: []eval.Doc{{ID: "doc-a", Text: "a"}, {ID: "doc-b", Text: "b"}}}
	embedder := fakeEmbedder{vecs: [][]float32{{1, 2, 3, 4}, {5, 6, 7, 8}}}
	if err := seed.Corpus(context.Background(), srv.Client(), srv.URL, "idx", gs, embedder, "t1", "agent-1"); err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	if len(puts) != 2 {
		t.Fatalf("got %d PUTs, want 2", len(puts))
	}
	if puts[0] != "/idx/_doc/doc-a" || puts[1] != "/idx/_doc/doc-b" {
		t.Errorf("PUT paths = %v, want literal doc ids under idx", puts)
	}
}

// TestCorpusPropagatesEmbedderError ensures a failed embedding call aborts
// seeding with a clear error instead of indexing zero-vector docs.
func TestCorpusPropagatesEmbedderError(t *testing.T) {
	gs := eval.GoldSet{Corpus: []eval.Doc{{ID: "doc-a", Text: "a"}}}
	err := seed.Corpus(context.Background(), http.DefaultClient, "http://unused.invalid", "idx", gs, fakeEmbedder{err: errors.New("boom")}, "", "")
	if err == nil {
		t.Fatal("want the embedder error propagated")
	}
}

// TestCorpusRejectsVectorCountMismatch guards against a misbehaving
// embedder silently truncating output.
func TestCorpusRejectsVectorCountMismatch(t *testing.T) {
	gs := eval.GoldSet{Corpus: []eval.Doc{{ID: "doc-a", Text: "a"}, {ID: "doc-b", Text: "b"}}}
	err := seed.Corpus(context.Background(), http.DefaultClient, "http://unused.invalid", "idx", gs, fakeEmbedder{vecs: [][]float32{{1}}}, "", "")
	if err == nil {
		t.Fatal("want a vector-count-mismatch error")
	}
}

// TestCorpusEmptyCorpusNoOp proves an empty gold set issues no HTTP calls.
func TestCorpusEmptyCorpusNoOp(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	if err := seed.Corpus(context.Background(), srv.Client(), srv.URL, "idx", eval.GoldSet{}, fakeEmbedder{}, "", ""); err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	if called {
		t.Error("empty corpus should not issue any HTTP call")
	}
}
