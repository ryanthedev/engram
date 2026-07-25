// Package enrich runs the episodic embedding-enrichment job (D15): episodic
// append is text-first, so a newly ingested event is BM25-searchable
// immediately but not yet kNN-searchable. This background job polls for
// episodic docs whose text_embedding is still unset, embeds their Text, and
// fills it in — the dev-harness lag budget is <=30s (DW-1.1).
package enrich

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/store"
)

// Store is the minimal write-side capability the enrichment job needs: find
// episodic docs missing an embedding, and fill one in. OpenSearchStore
// satisfies this structurally; defining the seam here (rather than
// depending on the full store.Store interface) keeps the job decoupled from
// write-path methods it never calls.
type Store interface {
	// FindUnembedded returns up to limit episodic docs still missing
	// text_embedding, oldest first.
	FindUnembedded(ctx context.Context, limit int) ([]store.Unembedded, error)
	// SetTextEmbedding fills text_embedding on the episodic doc at docID.
	SetTextEmbedding(ctx context.Context, docID string, vec []float32) error
	// FindUnembeddedFacts returns up to limit semantic docs still missing
	// fact_embedding, oldest first.
	FindUnembeddedFacts(ctx context.Context, limit int) ([]store.UnembeddedFact, error)
	// SetFactEmbedding fills fact_embedding on the semantic doc at docID.
	SetFactEmbedding(ctx context.Context, docID string, vec []float32) error
}

// Job runs the episodic embedding-enrichment loop: poll for text-only
// episodic docs, embed their Text, and fill text_embedding so the kNN clause
// can find them (D15).
type Job struct {
	Store    Store
	Embedder embed.Embedder
	// Logger receives tick outcomes; defaults to slog.Default() when nil.
	Logger *slog.Logger
}

// Tick runs one enrichment pass over BOTH vector tiers: up to batch episodic
// docs missing text_embedding, then up to batch semantic docs missing
// fact_embedding. It returns the total count enriched.
//
// Semantic is normally embedded inline at extraction, so its half is a no-op
// in steady state. It runs anyway because it is the only path that can rebuild
// semantic vectors without re-running extraction — which is what makes a model
// swap a matter of clearing a field rather than replaying every event through
// an LLM.
// The two tiers are deliberately independent: a failure in one must not stop
// the other. They share nothing but an embedder, their backlogs are unrelated,
// and the scans are oldest-first — so returning early on an episodic error
// meant one unembeddable page of episodic docs starved the semantic tier
// forever, since every subsequent tick re-fetched that same page and bailed
// before reaching semantic at all. Both halves run; both errors are reported.
func (j *Job) Tick(ctx context.Context, batch int) (int, error) {
	episodic, episodicErr := j.tickEpisodic(ctx, batch)
	semantic, semanticErr := j.tickSemantic(ctx, batch)
	return episodic + semantic, errors.Join(episodicErr, semanticErr)
}

// tickSemantic embeds one page of semantic statements missing fact_embedding.
func (j *Job) tickSemantic(ctx context.Context, batch int) (int, error) {
	pending, err := j.Store.FindUnembeddedFacts(ctx, batch)
	if err != nil {
		return 0, fmt.Errorf("enrich: scanning for unembedded facts: %w", err)
	}
	if len(pending) == 0 {
		return 0, nil
	}
	statements := make([]string, len(pending))
	for i, p := range pending {
		statements[i] = p.Statement
	}
	vectors, err := j.Embedder.Embed(ctx, statements)
	if err != nil {
		return 0, fmt.Errorf("enrich: embedding %d statements: %w", len(statements), err)
	}
	enriched := 0
	for i, p := range pending {
		if err := j.Store.SetFactEmbedding(ctx, p.DocID, vectors[i]); err != nil {
			return enriched, fmt.Errorf("enrich: setting fact_embedding for %s: %w", p.DocID, err)
		}
		enriched++
	}
	return enriched, nil
}

// tickEpisodic embeds one page of episodic texts missing text_embedding.
func (j *Job) tickEpisodic(ctx context.Context, batch int) (int, error) {
	pending, err := j.Store.FindUnembedded(ctx, batch)
	if err != nil {
		return 0, fmt.Errorf("enrich: scanning for unembedded docs: %w", err)
	}
	if len(pending) == 0 {
		return 0, nil
	}
	texts := make([]string, len(pending))
	for i, p := range pending {
		texts[i] = p.Rec.Text
	}
	vectors, err := j.Embedder.Embed(ctx, texts)
	if err != nil {
		return 0, fmt.Errorf("enrich: embedding %d texts: %w", len(texts), err)
	}
	enriched := 0
	for i, p := range pending {
		if err := j.Store.SetTextEmbedding(ctx, p.DocID, vectors[i]); err != nil {
			return enriched, fmt.Errorf("enrich: setting text_embedding for %s: %w", p.DocID, err)
		}
		enriched++
	}
	return enriched, nil
}

// Run polls Tick every interval until ctx is done. A failed tick is logged,
// not fatal — a transient error must not kill the background job.
func (j *Job) Run(ctx context.Context, interval time.Duration, batch int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := j.Tick(ctx, batch)
			if err != nil {
				j.logger().ErrorContext(ctx, "enrichment tick failed", "error", err)
			}
			// Reported even alongside an error: now that the tiers are
			// independent, a tick can half-fail, and "one tier is stuck" looks
			// exactly like "everything is stuck" if progress goes unlogged.
			if n > 0 {
				j.logger().InfoContext(ctx, "enrichment tick", "enriched", n)
			}
		}
	}
}

func (j *Job) logger() *slog.Logger {
	if j.Logger != nil {
		return j.Logger
	}
	return slog.Default()
}
