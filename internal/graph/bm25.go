package graph

import (
	"math"
	"strings"
)

// bm25 parameters (Okapi BM25 defaults — Robertson/Zaragoza's canonical
// choices; not exposed as config since this is an internal lexical-similarity
// signal, not a tunable search feature).
const (
	bm25K1 = 1.5
	bm25B  = 0.75
)

// tokenize lowercases and splits on non-alphanumeric runs — good enough for
// short entity names/aliases (not a general-purpose analyzer).
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
}

// bm25Corpus precomputes the document-frequency statistics BM25 needs across
// a small candidate set (the entities CandidateEntities returned for one
// mention's NameKey — always small, so no index is needed).
type bm25Corpus struct {
	docs      [][]string
	docFreq   map[string]int
	avgDocLen float64
}

// newBM25Corpus builds corpus statistics over docs (already-tokenized).
func newBM25Corpus(docs [][]string) bm25Corpus {
	c := bm25Corpus{docs: docs, docFreq: map[string]int{}}
	var totalLen int
	for _, d := range docs {
		totalLen += len(d)
		seen := map[string]bool{}
		for _, t := range d {
			if !seen[t] {
				seen[t] = true
				c.docFreq[t]++
			}
		}
	}
	if len(docs) > 0 {
		c.avgDocLen = float64(totalLen) / float64(len(docs))
	}
	return c
}

// score computes the Okapi BM25 relevance of queryTokens against one
// document (doc, already tokenized) within this corpus. A doc absent from
// the corpus (avgDocLen == 0) scores 0.
func (c bm25Corpus) score(queryTokens, doc []string) float64 {
	if len(doc) == 0 || c.avgDocLen == 0 {
		return 0
	}
	tf := map[string]int{}
	for _, t := range doc {
		tf[t]++
	}
	n := float64(len(c.docs))
	var total float64
	for _, qt := range queryTokens {
		f := float64(tf[qt])
		if f == 0 {
			continue
		}
		df := float64(c.docFreq[qt])
		// idf with the +0.5/+1 smoothing that keeps it non-negative for
		// small corpora (Robertson-Sparck Jones lower-bounded variant).
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		denom := f + bm25K1*(1-bm25B+bm25B*float64(len(doc))/c.avgDocLen)
		total += idf * (f * (bm25K1 + 1) / denom)
	}
	return total
}

// normalizedBM25 returns bm25 scores for query against every doc in docs,
// min-max normalized into [0,1] so they combine predictably with the
// cosine-similarity signal (bm25's raw scale is corpus-size-dependent and
// unbounded above). An all-zero score set (no lexical overlap anywhere)
// stays all zero rather than being divided by a zero range.
func normalizedBM25(query string, docs []string) []float64 {
	qTokens := tokenize(query)
	tokenized := make([][]string, len(docs))
	for i, d := range docs {
		tokenized[i] = tokenize(d)
	}
	corpus := newBM25Corpus(tokenized)
	raw := make([]float64, len(docs))
	var maxScore float64
	for i, d := range tokenized {
		raw[i] = corpus.score(qTokens, d)
		if raw[i] > maxScore {
			maxScore = raw[i]
		}
	}
	if maxScore == 0 {
		return raw // all zero: no lexical overlap with any candidate
	}
	out := make([]float64, len(raw))
	for i, s := range raw {
		out[i] = s / maxScore
	}
	return out
}
