package eval

// FixtureKeys derives a text -> fixture-key map from a gold set: every
// corpus doc's text maps to its own doc id, and every query's text maps to
// its first expected doc id. Feeding this to embed.NewFakeEmbedder makes a
// corpus statement and its gold queries (exact-id / keyword / paraphrase /
// multi-term) embed to the SAME vector, so the fake kNN signal is meaningful
// for retrieval evals without a real embedding model (DW-1.3).
func FixtureKeys(gs GoldSet) map[string]string {
	m := make(map[string]string, len(gs.Corpus)+len(gs.Queries))
	for _, d := range gs.Corpus {
		m[d.Text] = d.ID
	}
	for _, q := range gs.Queries {
		if len(q.ExpectedIDs) > 0 {
			m[q.Text] = q.ExpectedIDs[0]
		}
	}
	return m
}
