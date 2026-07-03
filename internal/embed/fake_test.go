package embed_test

import (
	"context"
	"math"
	"testing"

	"github.com/ryanthedev/engram/internal/embed"
)

// TestFakeEmbedderIsDeterministic proves the same text always embeds to the
// same vector — index-time and query-time calls must agree for kNN to be
// meaningful.
func TestFakeEmbedderIsDeterministic(t *testing.T) {
	e := embed.NewFakeEmbedder(1024, nil)
	v1, err := e.Embed(context.Background(), []string{"orders-svc connection pool leak"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	v2, err := e.Embed(context.Background(), []string{"orders-svc connection pool leak"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(v1[0]) != 1024 || len(v2[0]) != 1024 {
		t.Fatalf("wrong dim: %d, %d", len(v1[0]), len(v2[0]))
	}
	for i := range v1[0] {
		if v1[0][i] != v2[0][i] {
			t.Fatalf("vectors differ at %d: %v != %v (not deterministic)", i, v1[0][i], v2[0][i])
		}
	}
}

// TestFakeEmbedderVectorsAreUnitLength matches the real embedder contract
// (BGE-M3 vectors are L2-normalized — the semantic template uses
// space_type=innerproduct, D15).
func TestFakeEmbedderVectorsAreUnitLength(t *testing.T) {
	e := embed.NewFakeEmbedder(64, nil)
	vecs, err := e.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	for _, v := range vecs {
		var norm float64
		for _, x := range v {
			norm += float64(x) * float64(x)
		}
		if math.Abs(math.Sqrt(norm)-1.0) > 1e-4 {
			t.Errorf("vector norm = %v, want ~1.0", math.Sqrt(norm))
		}
	}
}

// TestFakeEmbedderDistinctTextsDiffer proves unregistered texts don't
// collide (would make kNN vacuous).
func TestFakeEmbedderDistinctTextsDiffer(t *testing.T) {
	e := embed.NewFakeEmbedder(32, nil)
	vecs, err := e.Embed(context.Background(), []string{"payments-api timeout", "checkout latency regression"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	same := true
	for i := range vecs[0] {
		if vecs[0][i] != vecs[1][i] {
			same = false
			break
		}
	}
	if same {
		t.Error("distinct unregistered texts embedded to identical vectors")
	}
}

// TestFakeEmbedderFixtureCorrelation proves the fixture table makes distinct
// texts (a corpus statement and a query paraphrase) share a vector when
// mapped to the same fixture key — the property DW-1.3's fake kNN signal
// depends on.
func TestFakeEmbedderFixtureCorrelation(t *testing.T) {
	fixtures := map[string]string{
		"payments-api has a 2500ms timeout":      "fact-pay-timeout",
		"how long before payments gives up":      "fact-pay-timeout",
		"totally unrelated text about something": "fact-other",
	}
	e := embed.NewFakeEmbedder(16, fixtures)
	vecs, err := e.Embed(context.Background(), []string{
		"payments-api has a 2500ms timeout",
		"how long before payments gives up",
		"totally unrelated text about something",
	})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	for i := range vecs[0] {
		if vecs[0][i] != vecs[1][i] {
			t.Fatalf("fixture-mapped texts diverge at %d: %v != %v", i, vecs[0][i], vecs[1][i])
		}
	}
	same := true
	for i := range vecs[0] {
		if vecs[0][i] != vecs[2][i] {
			same = false
			break
		}
	}
	if same {
		t.Error("texts mapped to different fixture keys embedded identically")
	}
}

// TestFakeEmbedderInfoIsPinned proves the fake embedder passes the D15
// startup validation it exists to unblock in tests.
func TestFakeEmbedderInfoIsPinned(t *testing.T) {
	e := embed.NewFakeEmbedder(1024, nil)
	if err := embed.ValidateInfo(e.Info(), 1024); err != nil {
		t.Errorf("fake embedder failed its own dimension validation: %v", err)
	}
}
