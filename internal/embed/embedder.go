// Package embed defines the Embedder seam over the co-located embedding
// service (BGE-M3, 1024-dim — D15). The embedding model, dimension, and
// revision are part of the index contract: a dimension mismatch against the
// knn_vector mapping is a startup error, not a runtime surprise.
package embed

import (
	"context"
	"fmt"
)

// ModelInfo identifies the embedding model an Embedder serves. Dim must match
// the index template's knn_vector dimension; Model and Revision pin the exact
// artifact so silently-swapped models are detectable.
type ModelInfo struct {
	Model    string
	Revision string
	Dim      int
}

// Embedder produces dense vectors for texts. Implementations wrap the
// co-located BGE-M3 service (D15: query-embed budget ≤ 50 ms inside the read
// SLA); the episodic enrichment job and the query path both consume this seam.
type Embedder interface {
	// Embed returns one Dim-length vector per input text, index-aligned.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Info reports the pinned model, revision, and dimension; validated at
	// startup against the index templates via ValidateInfo.
	Info() ModelInfo
}

// ValidateInfo is the startup guard for the embedding contract: it rejects an
// embedder whose dimension differs from the index template's knn_vector
// dimension (wantDim), or whose model/revision are unpinned (empty).
func ValidateInfo(info ModelInfo, wantDim int) error {
	if info.Model == "" || info.Revision == "" {
		return fmt.Errorf("embed: model artifact not pinned (model=%q revision=%q); D15 requires an exact artifact + revision", info.Model, info.Revision)
	}
	if info.Dim != wantDim {
		return fmt.Errorf("embed: dimension mismatch: embedder %q serves %d-dim vectors but the index template expects %d — refusing to start", info.Model, info.Dim, wantDim)
	}
	return nil
}
