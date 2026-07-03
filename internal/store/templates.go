package store

import _ "embed"

// The checked-in JSON below is the single source of truth for the cluster
// contract: the apply tool PUTs these exact bytes, and the DW-0.3 contract
// tests parse the same bytes. Editing a mapping means editing the JSON.

// EmbeddingDim is the pinned knn_vector dimension (BGE-M3, D15). Embedders
// are validated against it at startup (embed.ValidateInfo).
const EmbeddingDim = 1024

// PinnedVersionPrefix is the only OpenSearch version line Engram runs against
// (D14: pinned 3.1 exactly — one code path, no fallback).
const PinnedVersionPrefix = "3.1."

// Resource names on the cluster.
const (
	// EpisodicTemplateName / EpisodicIndex name the T1 index template and
	// its concrete dev index.
	EpisodicTemplateName = "engram-episodic"
	EpisodicIndex        = "engram-episodic-000001"
	// SemanticTemplateName / SemanticIndex name the T2 index template and
	// its concrete dev index.
	SemanticTemplateName = "engram-semantic"
	SemanticIndex        = "engram-semantic-000001"
	// LedgerTemplateName / LedgerIndex name the extraction-ledger index
	// (D13): one row per (tenant_id, event_id, extractor_version), claimed
	// with op_type=create before extraction.
	LedgerTemplateName = "engram-ledger"
	LedgerIndex        = "engram-ledger-000001"
	// RRFPipelineName is the hybrid-search fusion pipeline (D1).
	RRFPipelineName = "engram-rrf"
)

// EpisodicTemplateJSON is the T1 episodic index template (outbox + tenancy
// fields included — D12/D16).
//
//go:embed templates/episodic.json
var EpisodicTemplateJSON []byte

// SemanticTemplateJSON is the T2 semantic index template (bi-temporal +
// chain + tenancy fields — D3/D11/D16).
//
//go:embed templates/semantic.json
var SemanticTemplateJSON []byte

// LedgerTemplateJSON is the extraction-ledger index template (claim-first
// idempotency rows with the cached extraction — D13).
//
//go:embed templates/ledger.json
var LedgerTemplateJSON []byte

// RRFPipelineJSON is the RRF search pipeline (score-ranker-processor,
// rank_constant=60 — D1/D14).
//
//go:embed templates/rrf-pipeline.json
var RRFPipelineJSON []byte
