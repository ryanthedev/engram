package embed_test

import (
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/store"
)

// TestValidateInfoRejectsDimMismatch covers the plan's Phase-0 edge case:
// an embedding dimension that disagrees with the index template's knn_vector
// dimension must fail at startup with a clear message.
func TestValidateInfoRejectsDimMismatch(t *testing.T) {
	err := embed.ValidateInfo(embed.ModelInfo{Model: "BAAI/bge-m3", Revision: "abc123", Dim: 1536}, store.EmbeddingDim)
	if err == nil {
		t.Fatal("dim mismatch accepted")
	}
	if !strings.Contains(err.Error(), "1536") || !strings.Contains(err.Error(), "1024") {
		t.Errorf("mismatch error should name both dims, got: %v", err)
	}
}

// TestValidateInfoRejectsUnpinnedModel enforces D15's exact-artifact pin:
// empty model or revision is a startup error.
func TestValidateInfoRejectsUnpinnedModel(t *testing.T) {
	for name, info := range map[string]embed.ModelInfo{
		"no model":    {Revision: "abc123", Dim: store.EmbeddingDim},
		"no revision": {Model: "BAAI/bge-m3", Dim: store.EmbeddingDim},
	} {
		t.Run(name, func(t *testing.T) {
			if err := embed.ValidateInfo(info, store.EmbeddingDim); err == nil {
				t.Error("unpinned model accepted")
			}
		})
	}
}

// TestValidateInfoAcceptsPinnedMatch is the happy path: a pinned BGE-M3 at
// the template dimension validates.
func TestValidateInfoAcceptsPinnedMatch(t *testing.T) {
	info := embed.ModelInfo{Model: "BAAI/bge-m3", Revision: "abc123", Dim: store.EmbeddingDim}
	if err := embed.ValidateInfo(info, store.EmbeddingDim); err != nil {
		t.Fatalf("valid embedder rejected: %v", err)
	}
}
