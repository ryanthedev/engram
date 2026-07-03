// Package eval is the continuous retrieval-eval harness (D9): it loads a
// gold set (query → expected doc ids) with a pre-registered train/holdout
// split and scores any retrieval.Retriever with recall@k, MRR, and nDCG@k.
// It exists from Phase 0 so retrieval quality is measured, not assumed;
// against the NullRetriever it reports 0 by construction.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/ryanthedev/engram/internal/retrieval"
)

// Split labels for gold queries. The split is assigned at generation time and
// frozen in the checked-in file — pre-registered, so holdout results can't be
// tuned against (DW-1.3 measures on SplitHoldout only).
const (
	SplitTrain   = "train"
	SplitHoldout = "holdout"
)

// Doc is one seed corpus document (indexed by Phase 1 for retrieval evals).
type Doc struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Query is one gold query: the text to search and the doc ids a correct
// retriever must return.
type Query struct {
	ID          string   `json:"id"`
	Text        string   `json:"text"`
	ExpectedIDs []string `json:"expected_ids"`
	// Split is "train" or "holdout", fixed at generation time.
	Split string `json:"split"`
	// Class tags the query type (exact-id, keyword, paraphrase, multi-term)
	// so per-class fusion losses can be analyzed (DW-1.3).
	Class string `json:"class"`
}

// GoldSet is the checked-in eval fixture: a seed corpus plus gold queries.
type GoldSet struct {
	Corpus  []Doc   `json:"corpus"`
	Queries []Query `json:"queries"`
}

// LoadGoldSet reads and validates a gold-set JSON file: non-empty corpus and
// queries, unique ids, every expected id present in the corpus, and every
// split label legal.
func LoadGoldSet(path string) (GoldSet, error) {
	var gs GoldSet
	b, err := os.ReadFile(path)
	if err != nil {
		return gs, fmt.Errorf("eval: reading gold set: %w", err)
	}
	if err := json.Unmarshal(b, &gs); err != nil {
		return gs, fmt.Errorf("eval: parsing gold set %s: %w", path, err)
	}
	if err := gs.Validate(); err != nil {
		return gs, fmt.Errorf("eval: invalid gold set %s: %w", path, err)
	}
	return gs, nil
}

// Validate checks the gold set's structural invariants.
func (gs GoldSet) Validate() error {
	if len(gs.Corpus) == 0 || len(gs.Queries) == 0 {
		return fmt.Errorf("empty corpus (%d) or queries (%d)", len(gs.Corpus), len(gs.Queries))
	}
	docs := make(map[string]bool, len(gs.Corpus))
	for _, d := range gs.Corpus {
		if d.ID == "" {
			return fmt.Errorf("corpus doc with empty id")
		}
		if docs[d.ID] {
			return fmt.Errorf("duplicate corpus id %q", d.ID)
		}
		docs[d.ID] = true
	}
	qids := make(map[string]bool, len(gs.Queries))
	for _, q := range gs.Queries {
		if q.ID == "" || len(q.ExpectedIDs) == 0 {
			return fmt.Errorf("query %q missing id or expected_ids", q.ID)
		}
		if qids[q.ID] {
			return fmt.Errorf("duplicate query id %q", q.ID)
		}
		qids[q.ID] = true
		if q.Split != SplitTrain && q.Split != SplitHoldout {
			return fmt.Errorf("query %q has unknown split %q", q.ID, q.Split)
		}
		for _, id := range q.ExpectedIDs {
			if !docs[id] {
				return fmt.Errorf("query %q expects unknown doc id %q", q.ID, id)
			}
		}
	}
	return nil
}

// SplitCounts returns the number of train and holdout queries.
func (gs GoldSet) SplitCounts() (train, holdout int) {
	for _, q := range gs.Queries {
		if q.Split == SplitHoldout {
			holdout++
		} else {
			train++
		}
	}
	return train, holdout
}

// QueryResult is one query's scored outcome.
type QueryResult struct {
	QueryID string  `json:"query_id"`
	Class   string  `json:"class"`
	Split   string  `json:"split"`
	Recall  float64 `json:"recall_at_k"`
	RR      float64 `json:"reciprocal_rank"`
	NDCG    float64 `json:"ndcg_at_k"`
	Err     string  `json:"error,omitempty"`
}

// Report aggregates a harness run.
type Report struct {
	K         int           `json:"k"`
	Split     string        `json:"split"` // "" = all queries
	Queries   int           `json:"queries"`
	RecallAtK float64       `json:"recall_at_k"`
	MRR       float64       `json:"mrr"`
	NDCGAtK   float64       `json:"ndcg_at_k"`
	PerQuery  []QueryResult `json:"per_query"`
}

// Run scores r on the gold set's queries at cutoff k. split narrows to one
// split ("" runs everything). Per-query retrieval errors are recorded and
// scored 0, not fatal — a broken retriever should read as bad recall, and
// the Phase-0 NullRetriever reports recall 0 by construction.
func Run(ctx context.Context, r retrieval.Retriever, gs GoldSet, k int, split string) (Report, error) {
	if k <= 0 {
		return Report{}, fmt.Errorf("eval: k must be positive, got %d", k)
	}
	rep := Report{K: k, Split: split}
	for _, q := range gs.Queries {
		if split != "" && q.Split != split {
			continue
		}
		qr := QueryResult{QueryID: q.ID, Class: q.Class, Split: q.Split}
		hits, err := r.Search(ctx, retrieval.Query{Text: q.Text, K: k}, retrieval.Filter{ValidOnly: true})
		if err != nil {
			qr.Err = err.Error()
		} else {
			ranked := make([]string, len(hits))
			for i, h := range hits {
				ranked[i] = h.ID
			}
			qr.Recall = RecallAtK(ranked, q.ExpectedIDs, k)
			qr.RR = ReciprocalRank(ranked, q.ExpectedIDs)
			qr.NDCG = NDCGAtK(ranked, q.ExpectedIDs, k)
		}
		rep.PerQuery = append(rep.PerQuery, qr)
		rep.RecallAtK += qr.Recall
		rep.MRR += qr.RR
		rep.NDCGAtK += qr.NDCG
	}
	rep.Queries = len(rep.PerQuery)
	if rep.Queries > 0 {
		n := float64(rep.Queries)
		rep.RecallAtK /= n
		rep.MRR /= n
		rep.NDCGAtK /= n
	}
	return rep, nil
}

// NullRetriever returns no hits for every query — the Phase-0 stand-in that
// proves the harness runs end-to-end before a real Retriever exists.
type NullRetriever struct{}

// Search implements retrieval.Retriever with an always-empty result.
func (NullRetriever) Search(context.Context, retrieval.Query, retrieval.Filter) ([]retrieval.Hit, error) {
	return nil, nil
}
