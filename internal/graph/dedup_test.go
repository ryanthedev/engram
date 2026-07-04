package graph

import (
	"context"
	"errors"
	"testing"
)

// unit vector helpers for deterministic embedding fixtures.
func vec(vals ...float32) []float32 { return vals }

// fakeJudge lets tests control the tie-break outcome and observe whether it
// was invoked.
type fakeJudge struct {
	same   bool
	reason string
	err    error
	calls  int
}

func (j *fakeJudge) SameEntity(context.Context, Candidate, Candidate) (bool, string, error) {
	j.calls++
	return j.same, j.reason, j.err
}

func mustDeduper(t *testing.T, j Judge) Deduper {
	t.Helper()
	d, err := NewDeduper(j)
	if err != nil {
		t.Fatalf("NewDeduper: %v", err)
	}
	return d
}

func TestNewDeduper_NilJudgeErrors(t *testing.T) {
	if _, err := NewDeduper(nil); err == nil {
		t.Fatal("NewDeduper(nil) should error: a dedup routine with no tie-break judge can never resolve the ambiguous band")
	}
}

// TestDW_6_3_IdenticalMentionAlwaysMerges: repeated ingestion of the exact
// same (name, context) always merges into the sole existing candidate
// without touching the judge — cosine sim of an identical embedding is 1.0,
// well above the merge threshold. This is the mechanism DW-6.3's "10x
// re-ingest keeps entity count flat" relies on.
func TestDW_6_3_IdenticalMentionAlwaysMerges(t *testing.T) {
	j := &fakeJudge{}
	d := mustDeduper(t, j)
	same := vec(1, 0, 0, 0)
	existing := []Candidate{{ID: "e1", Name: "acme-svc", Embedding: same}}

	for i := 0; i < 10; i++ {
		dec, err := d.Decide(context.Background(), Candidate{Name: "acme-svc", Embedding: same}, existing)
		if err != nil {
			t.Fatalf("iteration %d: Decide: %v", i, err)
		}
		if !dec.Merge || dec.MatchID != "e1" {
			t.Fatalf("iteration %d: got Merge=%v MatchID=%q, want merge into e1", i, dec.Merge, dec.MatchID)
		}
	}
	if j.calls != 0 {
		t.Errorf("judge called %d times; identical mentions should never need a tie-break", j.calls)
	}
}

// TestDW_6_3_NoCandidatesAlwaysNew: an empty candidate list is always a new
// entity, deterministically, no judge call.
func TestDW_6_3_NoCandidatesAlwaysNew(t *testing.T) {
	j := &fakeJudge{}
	d := mustDeduper(t, j)
	dec, err := d.Decide(context.Background(), Candidate{Name: "brand-new"}, nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.Merge {
		t.Fatal("no candidates should never merge")
	}
	if j.calls != 0 {
		t.Errorf("judge called with no candidates to compare against")
	}
}

// TestHomonym_SameNameDifferentEntityStaysUnmerged: two mentions share an
// EXACT name ("Jordan") but have unrelated contexts (embedding vectors
// pointing in very different directions) — the embedding signal must
// dominate a perfect lexical match and keep them distinct WITHOUT ever
// invoking the judge (the edge case's documented resolution: "embedding
// distance ... before LLM-judge").
func TestHomonym_SameNameDifferentEntityStaysUnmerged(t *testing.T) {
	j := &fakeJudge{}
	d := mustDeduper(t, j)
	countryJordan := Candidate{ID: "country", Name: "Jordan", Embedding: vec(1, 0, 0, 0)}
	athleteMention := Candidate{Name: "Jordan", Embedding: vec(0, 1, 0, 0)} // orthogonal: unrelated context

	dec, err := d.Decide(context.Background(), athleteMention, []Candidate{countryJordan})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.Merge {
		t.Fatalf("homonym with orthogonal context merged into %q; want distinct", dec.MatchID)
	}
	if dec.LexSim != 1.0 {
		t.Errorf("LexSim = %v, want 1.0 (identical surface name)", dec.LexSim)
	}
	if j.calls != 0 {
		t.Errorf("judge called %d times; embedding distance alone should have settled the homonym case", j.calls)
	}
}

// TestAmbiguousBand_InvokesJudgeAndHonorsVerdict: a combined score in the
// middle band defers to the judge, and the judge's verdict (both true and
// false) is honored.
func TestAmbiguousBand_InvokesJudgeAndHonorsVerdict(t *testing.T) {
	// embedSim=0.6, lexSim=1.0 (partial name overlap forced via a shared
	// prefix token) -> combined = 0.7*0.6 + 0.3*1.0 = 0.72, inside
	// (0.45, 0.75).
	a := vec(1, 0, 0, 0)
	b := vec(0.6, 0.8, 0, 0) // cosine(a,b) = 0.6

	for _, tc := range []struct {
		name string
		same bool
	}{
		{"judge says same", true},
		{"judge says different", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &fakeJudge{same: tc.same, reason: "test"}
			d := mustDeduper(t, j)
			existing := Candidate{ID: "e1", Name: "widget", Embedding: a}
			mention := Candidate{Name: "widget", Embedding: b}

			dec, err := d.Decide(context.Background(), mention, []Candidate{existing})
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if j.calls != 1 {
				t.Fatalf("judge calls = %d, want exactly 1 for an ambiguous-band decision", j.calls)
			}
			if !dec.UsedJudge {
				t.Error("UsedJudge should be true")
			}
			if dec.Merge != tc.same {
				t.Errorf("Merge = %v, want %v (judge's verdict)", dec.Merge, tc.same)
			}
			if tc.same && dec.MatchID != "e1" {
				t.Errorf("MatchID = %q, want e1", dec.MatchID)
			}
		})
	}
}

// TestAmbiguousBand_JudgeErrorDefaultsToDistinct: a judge failure resolves to
// "not the same" (fail-SAFE — see Judge's doc comment: over-splitting is
// cheap to undo on the next mention, over-merging is not), never to Merge.
func TestAmbiguousBand_JudgeErrorDefaultsToDistinct(t *testing.T) {
	j := &fakeJudge{err: errors.New("judge unreachable")}
	d := mustDeduper(t, j)
	a := vec(1, 0, 0, 0)
	b := vec(0.6, 0.8, 0, 0)
	existing := Candidate{ID: "e1", Name: "widget", Embedding: a}
	mention := Candidate{Name: "widget", Embedding: b}

	dec, err := d.Decide(context.Background(), mention, []Candidate{existing})
	if err != nil {
		t.Fatalf("Decide should not surface the judge's error as a routine error: %v", err)
	}
	if dec.Merge {
		t.Fatal("judge error must never resolve to Merge")
	}
}

// TestDecide_PicksBestCombinedScoreAmongMultipleCandidates ensures the
// routine compares ALL candidates and merges into the strongest match, not
// just the first.
func TestDecide_PicksBestCombinedScoreAmongMultipleCandidates(t *testing.T) {
	j := &fakeJudge{}
	d := mustDeduper(t, j)
	mention := Candidate{Name: "acme-svc", Embedding: vec(1, 0, 0, 0)}
	candidates := []Candidate{
		{ID: "weak", Name: "acme-svc", Embedding: vec(0, 1, 0, 0)},   // orthogonal: bad match
		{ID: "strong", Name: "acme-svc", Embedding: vec(1, 0, 0, 0)}, // identical: best match
	}
	dec, err := d.Decide(context.Background(), mention, candidates)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !dec.Merge || dec.MatchID != "strong" {
		t.Fatalf("got Merge=%v MatchID=%q, want merge into 'strong'", dec.Merge, dec.MatchID)
	}
}

func TestCosineSimilarity(t *testing.T) {
	cases := []struct {
		name     string
		a, b     []float32
		wantSign int // -1, 0, 1 relative to 0 (avoid float exactness assumptions beyond the orthogonal/identical cases)
	}{
		{"identical", vec(1, 0), vec(1, 0), 1},
		{"orthogonal", vec(1, 0), vec(0, 1), 0},
		{"empty a", nil, vec(1, 0), 0},
		{"length mismatch", vec(1, 0, 0), vec(1, 0), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cosineSimilarity(tc.a, tc.b)
			switch tc.wantSign {
			case 1:
				if got < 0.999 {
					t.Errorf("cosineSimilarity(%v,%v) = %v, want ~1", tc.a, tc.b, got)
				}
			case 0:
				if got != 0 {
					t.Errorf("cosineSimilarity(%v,%v) = %v, want 0", tc.a, tc.b, got)
				}
			}
		})
	}
}

func TestRuleJudge(t *testing.T) {
	rj := RuleJudge{}
	cases := []struct {
		name string
		m, e Candidate
		want bool
	}{
		{"high overlap + high sim", Candidate{Name: "acme corp", Embedding: vec(1, 0)}, Candidate{Name: "acme corp inc", Embedding: vec(0.9, 0.436)}, true},
		{"no overlap", Candidate{Name: "acme corp", Embedding: vec(1, 0)}, Candidate{Name: "widget inc", Embedding: vec(1, 0)}, false},
		{"overlap but low sim", Candidate{Name: "acme corp", Embedding: vec(1, 0)}, Candidate{Name: "acme corp", Embedding: vec(0, 1)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			same, reason, err := rj.SameEntity(context.Background(), tc.m, tc.e)
			if err != nil {
				t.Fatalf("SameEntity: %v", err)
			}
			if same != tc.want {
				t.Errorf("SameEntity = %v (%s), want %v", same, reason, tc.want)
			}
		})
	}
}
