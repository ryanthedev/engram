package enrich_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/enrich"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
)

// fakeStore is an enrich.Store test double.
type fakeStore struct {
	pending  []store.Unembedded
	setCalls map[string][]float32
	findErr  error
	setErr   error
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
