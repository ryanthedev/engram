package enrich_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/enrich"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
)

// fakeStore is an enrich.Store test double. The semantic half defaults to
// empty, so every pre-existing episodic test exercises exactly its original
// behavior: Tick's semantic pass finds nothing and contributes nothing.
type fakeStore struct {
	pending      []store.Unembedded
	pendingFacts []store.UnembeddedFact
	setCalls     map[string][]float32
	factCalls    map[string][]float32
	findErr      error
	setErr       error
	findFactErr  error
	setFactErr   error
}

func (f *fakeStore) FindUnembeddedFacts(_ context.Context, limit int) ([]store.UnembeddedFact, error) {
	if f.findFactErr != nil {
		return nil, f.findFactErr
	}
	if limit < len(f.pendingFacts) {
		return f.pendingFacts[:limit], nil
	}
	return f.pendingFacts, nil
}

func (f *fakeStore) SetFactEmbedding(_ context.Context, docID string, vec []float32) error {
	if f.setFactErr != nil {
		return f.setFactErr
	}
	if f.factCalls == nil {
		f.factCalls = map[string][]float32{}
	}
	f.factCalls[docID] = vec
	return nil
}

func (f *fakeStore) FindUnembedded(_ context.Context, limit int) ([]store.Unembedded, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	if limit < len(f.pending) {
		return f.pending[:limit], nil
	}
	return f.pending, nil
}

func (f *fakeStore) SetTextEmbedding(_ context.Context, docID string, vec []float32) error {
	if f.setErr != nil {
		return f.setErr
	}
	if f.setCalls == nil {
		f.setCalls = map[string][]float32{}
	}
	f.setCalls[docID] = vec
	return nil
}

var _ enrich.Store = (*fakeStore)(nil)

// TestJobTickEmbedsAndFillsPending covers the enrichment job's main path: it
// embeds every pending doc's Text and fills text_embedding for each.
func TestJobTickEmbedsAndFillsPending(t *testing.T) {
	fs := &fakeStore{pending: []store.Unembedded{
		{DocID: "d1", Rec: memory.Episodic{Text: "hello"}},
		{DocID: "d2", Rec: memory.Episodic{Text: "world"}},
	}}
	j := &enrich.Job{Store: fs, Embedder: embed.NewFakeEmbedder(8, nil)}
	n, err := j.Tick(context.Background(), 10)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 2 {
		t.Errorf("enriched = %d, want 2", n)
	}
	if len(fs.setCalls) != 2 {
		t.Errorf("SetTextEmbedding called %d times, want 2", len(fs.setCalls))
	}
	if len(fs.setCalls["d1"]) != 8 {
		t.Errorf("vector dim = %d, want 8", len(fs.setCalls["d1"]))
	}
}

// TestJobTickNoPendingIsNoOp proves an empty backlog does nothing and
// succeeds.
func TestJobTickNoPendingIsNoOp(t *testing.T) {
	fs := &fakeStore{}
	j := &enrich.Job{Store: fs, Embedder: embed.NewFakeEmbedder(8, nil)}
	n, err := j.Tick(context.Background(), 10)
	if err != nil || n != 0 {
		t.Fatalf("Tick = (%d, %v), want (0, nil)", n, err)
	}
}

// TestJobTickPropagatesFindError surfaces a scan failure rather than
// swallowing it.
func TestJobTickPropagatesFindError(t *testing.T) {
	fs := &fakeStore{findErr: errors.New("boom")}
	j := &enrich.Job{Store: fs, Embedder: embed.NewFakeEmbedder(8, nil)}
	if _, err := j.Tick(context.Background(), 10); err == nil {
		t.Fatal("want the FindUnembedded error propagated")
	}
}

// TestJobTickPropagatesSetError surfaces a write failure rather than
// swallowing it.
func TestJobTickPropagatesSetError(t *testing.T) {
	fs := &fakeStore{
		pending: []store.Unembedded{{DocID: "d1", Rec: memory.Episodic{Text: "x"}}},
		setErr:  errors.New("boom"),
	}
	j := &enrich.Job{Store: fs, Embedder: embed.NewFakeEmbedder(8, nil)}
	if _, err := j.Tick(context.Background(), 10); err == nil {
		t.Fatal("want the SetTextEmbedding error propagated")
	}
}

// TestJobRunStopsOnContextCancel proves the background loop has a clear
// lifecycle owner: canceling ctx stops it.
func TestJobRunStopsOnContextCancel(t *testing.T) {
	fs := &fakeStore{}
	j := &enrich.Job{Store: fs, Embedder: embed.NewFakeEmbedder(8, nil)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		j.Run(ctx, time.Millisecond, 10)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// TestTickEmbedsSemanticFacts: the semantic half of a tick fills
// fact_embedding for statements missing it, index-aligned, and its count adds
// to the episodic half's. This is the path a model swap depends on — clearing
// fact_embedding must rebuild semantic vectors WITHOUT re-running extraction,
// which would spend LLM calls reproducing facts that are already correct.
func TestTickEmbedsSemanticFacts(t *testing.T) {
	fs := &fakeStore{
		pending: []store.Unembedded{
			{DocID: "ep-1", Rec: memory.Episodic{Text: "deploy key rotates weekly"}},
		},
		pendingFacts: []store.UnembeddedFact{
			{DocID: "sem-1", Statement: "alice knows bob"},
			{DocID: "sem-2", Statement: "the grid focuses windows"},
		},
	}
	job := enrich.Job{Store: fs, Embedder: embed.NewFakeEmbedder(8, nil)}

	n, err := job.Tick(context.Background(), 10)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 3 {
		t.Errorf("enriched = %d, want 3 (1 episodic + 2 semantic)", n)
	}
	if len(fs.factCalls) != 2 {
		t.Fatalf("fact_embedding written for %d docs, want 2: %v", len(fs.factCalls), fs.factCalls)
	}
	for _, id := range []string{"sem-1", "sem-2"} {
		if len(fs.factCalls[id]) != 8 {
			t.Errorf("%s embedding has %d dims, want 8", id, len(fs.factCalls[id]))
		}
	}
	// Index alignment: distinct statements must not collapse to one vector.
	if fmt.Sprint(fs.factCalls["sem-1"]) == fmt.Sprint(fs.factCalls["sem-2"]) {
		t.Error("both facts got the same vector — statements were not embedded index-aligned")
	}
}

// TestTickSemanticFailureDoesNotLoseEpisodicWork: when the semantic half
// fails, the episodic writes that already succeeded are still reported. A tick
// that returned 0 on a late failure would make a partially-drained backlog
// look like a no-op in the logs.
func TestTickSemanticFailureDoesNotLoseEpisodicWork(t *testing.T) {
	fs := &fakeStore{
		pending:     []store.Unembedded{{DocID: "ep-1", Rec: memory.Episodic{Text: "x"}}},
		findFactErr: errors.New("opensearch down"),
	}
	job := enrich.Job{Store: fs, Embedder: embed.NewFakeEmbedder(8, nil)}

	n, err := job.Tick(context.Background(), 10)
	if err == nil {
		t.Fatal("Tick succeeded despite a semantic scan failure")
	}
	if n != 1 {
		t.Errorf("enriched = %d, want 1: the episodic write that landed must still be counted", n)
	}
	if len(fs.setCalls) != 1 {
		t.Errorf("episodic writes = %d, want 1", len(fs.setCalls))
	}
}

// TestTickEpisodicFailureDoesNotStarveSemantic is the mirror of the above, and
// the one that bit in practice: Tick used to return as soon as the episodic
// half errored, so the semantic half never ran at all. Because both scans are
// oldest-first, the next tick re-fetched the same unembeddable episodic page
// and bailed at the same place — semantic could never drain, no matter how
// many facts were waiting or how cheap they were to embed. A full corpus of
// semantic vectors stayed empty behind one bad page of episodic docs.
func TestTickEpisodicFailureDoesNotStarveSemantic(t *testing.T) {
	fs := &fakeStore{
		findErr:      errors.New("embed service timeout"),
		pendingFacts: []store.UnembeddedFact{{DocID: "sem-1", Statement: "a"}, {DocID: "sem-2", Statement: "b"}},
	}
	job := enrich.Job{Store: fs, Embedder: embed.NewFakeEmbedder(8, nil)}

	n, err := job.Tick(context.Background(), 10)
	if err == nil {
		t.Fatal("Tick succeeded despite an episodic scan failure")
	}
	if n != 2 {
		t.Errorf("enriched = %d, want 2: semantic must drain even while episodic is failing", n)
	}
	if len(fs.factCalls) != 2 {
		t.Errorf("semantic writes = %d, want 2", len(fs.factCalls))
	}
}

// TestTickReportsBothTierErrors guards the aggregation: when both halves fail,
// neither error may be swallowed, or a debugging session chases one stuck tier
// while the other is equally stuck and invisible.
func TestTickReportsBothTierErrors(t *testing.T) {
	episodicErr := errors.New("episodic scan exploded")
	semanticErr := errors.New("semantic scan exploded")
	fs := &fakeStore{findErr: episodicErr, findFactErr: semanticErr}
	job := enrich.Job{Store: fs, Embedder: embed.NewFakeEmbedder(8, nil)}

	_, err := job.Tick(context.Background(), 10)
	if !errors.Is(err, episodicErr) {
		t.Errorf("episodic error not reported: %v", err)
	}
	if !errors.Is(err, semanticErr) {
		t.Errorf("semantic error not reported: %v", err)
	}
}
