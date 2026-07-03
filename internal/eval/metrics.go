package eval

import "math"

// RecallAtK returns |relevant ∩ ranked[:k]| / |relevant|.
func RecallAtK(ranked, relevant []string, k int) float64 {
	if len(relevant) == 0 {
		return 0
	}
	rel := toSet(relevant)
	found := 0
	for _, id := range top(ranked, k) {
		if rel[id] {
			found++
			rel[id] = false // count each relevant doc once
		}
	}
	return float64(found) / float64(len(relevant))
}

// ReciprocalRank returns 1/rank of the first relevant hit, 0 if none.
func ReciprocalRank(ranked, relevant []string) float64 {
	rel := toSet(relevant)
	for i, id := range ranked {
		if rel[id] {
			return 1 / float64(i+1)
		}
	}
	return 0
}

// NDCGAtK returns the normalized discounted cumulative gain at cutoff k with
// binary relevance: DCG over ranked[:k] divided by the ideal DCG.
func NDCGAtK(ranked, relevant []string, k int) float64 {
	if len(relevant) == 0 {
		return 0
	}
	rel := toSet(relevant)
	dcg := 0.0
	for i, id := range top(ranked, k) {
		if rel[id] {
			dcg += 1 / math.Log2(float64(i)+2)
		}
	}
	ideal := 0.0
	n := min(len(relevant), k)
	for i := 0; i < n; i++ {
		ideal += 1 / math.Log2(float64(i)+2)
	}
	return dcg / ideal
}

func top(ranked []string, k int) []string {
	if len(ranked) > k {
		return ranked[:k]
	}
	return ranked
}

func toSet(ids []string) map[string]bool {
	s := make(map[string]bool, len(ids))
	for _, id := range ids {
		s[id] = true
	}
	return s
}
