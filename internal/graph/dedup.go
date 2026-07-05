package graph

import (
	"context"
	"fmt"
	"math"
)

// Weights tunes how the embedding-similarity and lexical (BM25) signals
// combine into Deduper's single score. They sum to 1 by convention (Decide
// does not enforce it — a caller supplying unnormalized weights gets a
// proportionally scaled score, which only shifts where the threshold bands
// fall, never the ordering of candidates).
type Weights struct {
	Embed float64
	Lex   float64
}

// DefaultWeights favors the embedding (context) signal over raw lexical
// overlap — the homonym edge case needs embedding distance to win over a
// perfect name match (see the design memo).
var DefaultWeights = Weights{Embed: 0.7, Lex: 0.3}

// Candidate is one side of a dedup comparison: a mention's (or an existing
// entity's) name, disambiguating context, and context embedding.
type Candidate struct {
	ID        string // empty for the incoming mention; the entity id for an existing candidate
	Name      string
	Aliases   []string
	Context   string
	Embedding []float32
}

// Decision is Deduper.Decide's verdict for one incoming mention.
type Decision struct {
	// Merge reports whether the mention resolves to MatchID (an existing
	// entity) rather than creating a new one.
	Merge   bool
	MatchID string
	Reason  string
	// EmbedSim / LexSim / Combined are the winning candidate's scores —
	// exposed for the DW-6.3 "dedup decisions logged" requirement and for
	// tests, not consumed by callers as control flow.
	EmbedSim, LexSim, Combined float64
	UsedJudge                  bool
}

// Judge is the tie-break seam for mentions whose combined score lands in the
// ambiguous middle band — too similar to confidently split, not similar
// enough to confidently merge. Mirrors experience.Gatekeeper's shape:
// RuleJudge is the deterministic fixture judge, HTTPJudge an OpenAI-compatible
// LLM, config-selected.
type Judge interface {
	// SameEntity reports whether mention and existing are the same
	// real-world entity. A non-nil error means the judge itself failed; the
	// caller (Deduper.Decide) treats that as "not the same" (fail-SAFE, not
	// fail-closed: an over-split entity is cheap to re-merge on the next
	// mention, while an over-merged one conflates two identities and is
	// comparatively expensive to unwind — the opposite trade-off from an
	// ACL/security gate).
	SameEntity(ctx context.Context, mention, existing Candidate) (same bool, reason string, err error)
}

// Deduper is the ONE dedup decision routine (functional cohesion: "decide
// whether this mention is the same entity as its best existing candidate").
// It combines three signals — cosine similarity of context embeddings, a
// BM25-style lexical score over candidate names/aliases, and (only when
// those two disagree) a Judge tie-break — but callers see exactly one
// operation and one verdict; the signal combination is entirely hidden.
type Deduper struct {
	Judge Judge
	// MergeThreshold: combined score at/above this merges without a judge
	// call. SplitThreshold: combined score at/below this is distinct
	// without a judge call. Between them is the ambiguous band the Judge
	// tie-breaks.
	MergeThreshold, SplitThreshold float64
	Weights                        Weights
}

// NewDeduper returns a Deduper with the plan's default thresholds
// (0.75 merge / 0.45 split) and DefaultWeights. judge must not be nil — a
// Deduper with no tie-break judge could never resolve the ambiguous band, so
// construction fails loudly (mirrors experience.ErrNoGate).
func NewDeduper(judge Judge) (Deduper, error) {
	if judge == nil {
		return Deduper{}, fmt.Errorf("graph: a dedup Judge is required (no tie-break for the ambiguous band)")
	}
	return Deduper{Judge: judge, MergeThreshold: 0.75, SplitThreshold: 0.45, Weights: DefaultWeights}, nil
}

// Decide scores mention against every candidate (existing entities sharing
// a name-lookup key) and returns the single verdict: merge into the
// best-scoring candidate, or create a new entity. An empty candidates list
// always yields a new-entity decision (nothing to merge into).
func (d Deduper) Decide(ctx context.Context, mention Candidate, candidates []Candidate) (Decision, error) {
	if len(candidates) == 0 {
		return Decision{Merge: false, Reason: "no existing candidates"}, nil
	}

	lexScores := normalizedBM25(mention.Name, candidateDocs(candidates))
	best := -1
	var bestCombined, bestEmbed, bestLex float64
	for i, c := range candidates {
		embedSim := cosineSimilarity(mention.Embedding, c.Embedding)
		lexSim := lexScores[i]
		combined := d.Weights.Embed*embedSim + d.Weights.Lex*lexSim
		if best == -1 || combined > bestCombined {
			best, bestCombined, bestEmbed, bestLex = i, combined, embedSim, lexSim
		}
	}
	winner := candidates[best]

	switch {
	case bestCombined >= d.MergeThreshold:
		return Decision{
			Merge: true, MatchID: winner.ID, Reason: "combined score above merge threshold",
			EmbedSim: bestEmbed, LexSim: bestLex, Combined: bestCombined,
		}, nil
	case bestCombined <= d.SplitThreshold:
		return Decision{
			Merge: false, Reason: "combined score at/below split threshold",
			EmbedSim: bestEmbed, LexSim: bestLex, Combined: bestCombined,
		}, nil
	default:
		// Ambiguous band: tie-break via judge. A judge error or "not the
		// same" verdict both resolve to a NEW entity (fail-safe: see Judge's
		// doc comment on why this direction, not fail-closed).
		same, reason, err := d.Judge.SameEntity(ctx, mention, winner)
		dec := Decision{EmbedSim: bestEmbed, LexSim: bestLex, Combined: bestCombined, UsedJudge: true}
		if err != nil {
			dec.Reason = fmt.Sprintf("judge failed, defaulting to distinct: %v", err)
			return dec, nil
		}
		dec.Merge = same
		if same {
			dec.MatchID = winner.ID
			dec.Reason = "judge tie-break: " + reason
		} else {
			dec.Reason = "judge tie-break: " + reason
		}
		return dec, nil
	}
}

// candidateDocs renders each candidate's lexical document (name + aliases,
// space-joined) for the BM25 corpus.
func candidateDocs(candidates []Candidate) []string {
	out := make([]string, len(candidates))
	for i, c := range candidates {
		doc := c.Name
		for _, a := range c.Aliases {
			doc += " " + a
		}
		out[i] = doc
	}
	return out
}

// cosineSimilarity returns the cosine similarity of a and b, in [-1,1]. A
// missing embedding on either side (nil, or length mismatch) returns 0 —
// dedup degrades to lexical-only rather than erroring, matching the
// worker's "degraded, not fatal" embedding convention.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// RuleJudge is the deterministic, fixture-driven Judge: it reproduces a
// reasonable "same entity" call from token overlap and embedding similarity
// alone (no live model), so dedup tie-breaks are reproducible in unit tests
// and the local e2e stack — the same role RuleGatekeeper plays for the
// experience write-gate.
type RuleJudge struct{}

var _ Judge = RuleJudge{}

// SameEntity implements Judge deterministically: same entity when the names
// share at least half their tokens AND the context embeddings are at least
// moderately similar (>=0.35 cosine) — a heuristic tie-break for the
// ambiguous band only (Decide already resolved the clear cases before ever
// calling this).
func (RuleJudge) SameEntity(_ context.Context, mention, existing Candidate) (bool, string, error) {
	overlap := tokenOverlapRatio(mention.Name, existing.Name)
	sim := cosineSimilarity(mention.Embedding, existing.Embedding)
	if overlap >= 0.5 && sim >= 0.35 {
		return true, fmt.Sprintf("rule judge: token overlap %.2f, embed sim %.2f", overlap, sim), nil
	}
	return false, fmt.Sprintf("rule judge: token overlap %.2f, embed sim %.2f below bar", overlap, sim), nil
}

// tokenOverlapRatio returns the Jaccard overlap of a and b's token sets.
func tokenOverlapRatio(a, b string) float64 {
	ta, tb := tokenize(a), tokenize(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	setA := map[string]bool{}
	for _, t := range ta {
		setA[t] = true
	}
	setB := map[string]bool{}
	for _, t := range tb {
		setB[t] = true
	}
	var inter int
	for t := range setA {
		if setB[t] {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
