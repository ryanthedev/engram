package embed

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"math/rand"
)

// FakeEmbedder is the deterministic Embedder used by tests and the eval
// harness — no real BGE-M3 service is required for Phase 1 (see the plan's
// embeddings note). Equal texts always embed to equal vectors, so repeated
// index/query calls are consistent. An optional fixture table maps specific
// texts to a shared fixture key so a corpus statement and its query
// paraphrases land on the same vector — the kNN signal needs that
// correlation to be meaningful in tests that use fabricated text. Text
// absent from the table falls back to its own hash: still deterministic and
// index/query-consistent, just uncorrelated with other text.
type FakeEmbedder struct {
	info     ModelInfo
	fixtures map[string]string // text -> fixture key
}

// NewFakeEmbedder returns a FakeEmbedder serving dim-dimensional vectors.
// fixtures may be nil; when non-nil, a text present as a key embeds to the
// vector for its mapped fixture key instead of its own hash.
func NewFakeEmbedder(dim int, fixtures map[string]string) *FakeEmbedder {
	return &FakeEmbedder{
		info:     ModelInfo{Model: "engram-fake-embedder", Revision: "v1", Dim: dim},
		fixtures: fixtures,
	}
}

// Embed returns one deterministic Dim-length unit vector per input text,
// index-aligned.
func (e *FakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		key := text
		if k, ok := e.fixtures[text]; ok {
			key = k
		}
		out[i] = deterministicUnitVector(key, e.info.Dim)
	}
	return out, nil
}

// Info reports the fake embedder's pinned identity and dimension.
func (e *FakeEmbedder) Info() ModelInfo { return e.info }

// deterministicUnitVector derives a reproducible unit-length vector from key:
// sha256(key) seeds a PRNG, the same approach the Phase-0 live-cluster spikes
// used for synthetic vectors (internal/spike's vec helper).
func deterministicUnitVector(key string, dim int) []float32 {
	sum := sha256.Sum256([]byte(key))
	seed := int64(binary.BigEndian.Uint64(sum[:8]))
	r := rand.New(rand.NewSource(seed))
	v := make([]float32, dim)
	var norm float64
	for i := range v {
		v[i] = float32(r.NormFloat64())
		norm += float64(v[i]) * float64(v[i])
	}
	n := float32(math.Sqrt(norm))
	for i := range v {
		v[i] /= n
	}
	return v
}
