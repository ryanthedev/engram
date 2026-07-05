package graph

import "testing"

func TestBM25_ExactMatchScoresHighestAmongCandidates(t *testing.T) {
	docs := []string{"bob smith", "robert jones", "alice cooper"}
	scores := normalizedBM25("bob smith", docs)
	if scores[0] <= scores[1] || scores[0] <= scores[2] {
		t.Fatalf("exact match doc should score highest: %v", scores)
	}
	if scores[0] != 1.0 {
		t.Errorf("top score should normalize to 1.0, got %v", scores[0])
	}
}

func TestBM25_PartialOverlapBeatsNoOverlap(t *testing.T) {
	docs := []string{"bob jones", "alice cooper"}
	scores := normalizedBM25("bob smith", docs)
	if scores[0] <= scores[1] {
		t.Fatalf("partial token overlap should outscore no overlap: %v", scores)
	}
}

func TestBM25_NoOverlapIsAllZero(t *testing.T) {
	scores := normalizedBM25("xyz", []string{"alice", "bob"})
	for i, s := range scores {
		if s != 0 {
			t.Errorf("score[%d] = %v, want 0 (no lexical overlap anywhere)", i, s)
		}
	}
}

func TestBM25_SingleCandidateNormalizesToOne(t *testing.T) {
	// A degenerate but real case: exactly one candidate returned (e.g. the
	// homonym scenario, where NameKey-adjacent recall found only one prior
	// entity under this name) — its own score is always the max, so it
	// normalizes to 1.0 regardless of the raw BM25 magnitude. This is
	// intentional: lexical similarity alone cannot distinguish homonyms: the
	// embedding signal must carry that weight (see dedup_test.go).
	scores := normalizedBM25("jordan", []string{"jordan"})
	if len(scores) != 1 || scores[0] != 1.0 {
		t.Fatalf("single-candidate score = %v, want [1.0]", scores)
	}
}

func TestBM25_EmptyDocsList(t *testing.T) {
	scores := normalizedBM25("anything", nil)
	if len(scores) != 0 {
		t.Fatalf("empty docs should yield no scores, got %v", scores)
	}
}
