// Package sizing implements the GREENFIELD-BUILD-PLAN's SQfp16 off-heap
// vector RAM formula, shared by cmd/engram-deploy (to size the OpenSearch
// domain) and cmd/engram-loadtest (to verify measured RAM against it,
// DW-7.3).
package sizing

// BytesPerVectorSQfp16 estimates off-heap native memory per HNSW vector
// under SQfp16 scalar quantization: 1.1*(2*dim + 8*m) bytes/vector (the
// plan's §Backbone config formula; the 1.1 factor is Faiss/Lucene HNSW graph
// link overhead). dim is the embedding dimension (1024, D15/BGE-M3); m is
// the HNSW graph degree (16, per internal/store's semantic template).
func BytesPerVectorSQfp16(dim, m int) float64 {
	return 1.1 * (2*float64(dim) + 8*float64(m))
}

// EstimatedVectorRAMBytes estimates total off-heap vector RAM for a corpus
// of numVectors at the given dimension/HNSW-degree, replicated replicas
// times (each replica carries a full copy of the index in-memory graph).
func EstimatedVectorRAMBytes(numVectors, dim, m, replicas int) float64 {
	if replicas < 1 {
		replicas = 1
	}
	return BytesPerVectorSQfp16(dim, m) * float64(numVectors) * float64(replicas)
}

// WithinTolerance reports whether measured is within tolerancePct of
// estimated (DW-7.3: "matches the SQfp16 sizing formula within 20%").
func WithinTolerance(measured, estimated, tolerancePct float64) bool {
	if estimated == 0 {
		return measured == 0
	}
	diff := measured - estimated
	if diff < 0 {
		diff = -diff
	}
	return diff/estimated <= tolerancePct/100
}
