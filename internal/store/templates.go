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
	// AuthTokenTemplateName / AuthTokenIndex name the token-auth index
	// (Phase 3): one row per issued token, keyed by the token hash; the raw
	// token is never stored.
	AuthTokenTemplateName = "engram-auth-tokens"
	AuthTokenIndex        = "engram-auth-tokens-000001"
	// ACLEdgesTemplateName / ACLEdgesIndex name the ACL reachability index
	// (Phase 4): one doc per grant (user↔agent, membership, org grant), keyed
	// by a deterministic edge id so grant/revoke are idempotent. Query-time
	// enforcement reads it fresh, so deleting an edge revokes on the next call.
	ACLEdgesTemplateName = "engram-acl-edges"
	ACLEdgesIndex        = "engram-acl-edges-000001"
	// KnowledgeCollectionsTemplateName / KnowledgeCollectionsIndex name the
	// knowledge-platform collection registry meta-index (Phase 3): one doc
	// per collection, keyed by collection name — the durable source of truth
	// behind store.CollectionRegistry, so collections are created/updated at
	// runtime with no restart. Data indices are knowledge-<name>-vN behind a
	// knowledge-<name> alias; the "-" the name grammar forbids keeps them off
	// this template's knowledge-collections-* pattern (and the reserved name
	// "collections" closes the one hole).
	KnowledgeCollectionsTemplateName = "knowledge-collections"
	KnowledgeCollectionsIndex        = "knowledge-collections-000001"
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

// AuthTokenTemplateJSON is the token-auth index template (Phase 3): hashed
// tokens only, bound to (tenant_id, user_id, agent_id), TTL'd and revocable.
//
//go:embed templates/auth-tokens.json
var AuthTokenTemplateJSON []byte

// ACLEdgesTemplateJSON is the ACL reachability index template (Phase 4):
// user↔agent, membership, and org-grant edges, strict-mapped keyword fields.
//
//go:embed templates/acl-edges.json
var ACLEdgesTemplateJSON []byte

// KnowledgeCollectionsTemplateJSON is the collection-registry meta-index
// template (Phase 3): strict-mapped spec rows (name/text_field/index/version/
// public/roles/fields[]/updated_at).
//
//go:embed templates/knowledge-collections.json
var KnowledgeCollectionsTemplateJSON []byte

// RRFPipelineJSON is the RRF search pipeline (score-ranker-processor,
// rank_constant=60 — D1/D14).
//
//go:embed templates/rrf-pipeline.json
var RRFPipelineJSON []byte
