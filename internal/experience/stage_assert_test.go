package experience

import (
	"github.com/ryanthedev/engram/internal/worker"
)

// var _ worker.Stage confirms DistillStage satisfies the D20 seam at compile
// time without making the production package depend on internal/worker (the
// internal/graph stage_test.go precedent). It is half of the DW-1.1 assertion:
// the seam carries []ingest.FactOutcome, and this fails to compile if
// DistillStage.Process drifts from it.
var _ worker.Stage = (*DistillStage)(nil)
