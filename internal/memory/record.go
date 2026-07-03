package memory

import "time"

// Episodic is a T1 episodic record: one raw agent event, appended synchronously
// on Ingest. The episodic index doubles as the async write queue (outbox, D12):
// the append IS the enqueue, and the outbox fields below track scan-and-claim
// worker state.
type Episodic struct {
	// EventID is the client-supplied idempotency identity (D13). Required on
	// every Ingest; it keys the extraction ledger together with TenantID and
	// the extractor version.
	EventID string `json:"event_id"`

	// Tenancy / provenance fields (D16). Present from Phase 0 on every record;
	// dumb single-team values until Phase 4 turns on enforcement.
	TenantID     string   `json:"tenant_id"`
	TeamID       string   `json:"team_id"`
	Scope        string   `json:"scope"`
	OwnerAgentID string   `json:"owner_agent_id"`
	SourceIDs    []string `json:"source_ids,omitempty"`

	// Kind classifies the event (e.g. "tool_result", "conversation", "task").
	Kind string `json:"kind"`
	// Text is the raw event payload; the BM25 retrieval target.
	Text string `json:"text"`
	// TextEmbedding is the 1024-dim BGE-M3 embedding (D15). Appends are
	// text-first: this is nil until the Phase-1 enrichment job fills it.
	TextEmbedding []float32 `json:"text_embedding,omitempty"`

	// OccurredAt is real-world event time, stamped from the event content.
	OccurredAt time.Time `json:"occurred_at"`
	// CreatedAt is system append time; stamped by the writer, never the agent.
	CreatedAt time.Time `json:"created_at"`

	// Outbox fields (D12): scan-and-claim worker state.
	// ProcessedAt is nil until the async worker completes the event.
	ProcessedAt *time.Time `json:"processed_at,omitempty"`
	// ClaimLeaseUntil is the current worker lease expiry; nil when unclaimed.
	ClaimLeaseUntil *time.Time `json:"claim_lease_until,omitempty"`
	// Attempts counts processing attempts; bounded before dead-lettering.
	Attempts int `json:"attempts"`
	// DeadLettered marks the event as permanently failed (with the reason);
	// dead-lettered events are excluded from outbox scans.
	DeadLettered     bool   `json:"dead_lettered,omitempty"`
	DeadLetterReason string `json:"dead_letter_reason,omitempty"`
}

// SemanticFact is a T2 semantic record: one extracted fact, written
// bi-temporally (D3) under the write protocol (D10/D11). Facts are never
// hard-deleted; contradictions close the valid-time interval of the
// predecessor and link the successor via Supersedes.
type SemanticFact struct {
	// Subject, Predicate, Object are the normalized triple; together with
	// TenantID they define ContentKey (D11).
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	Object    string `json:"object"`
	// Statement is the natural-language rendering of the fact; the BM25
	// retrieval target.
	Statement string `json:"statement"`
	// FactEmbedding is the 1024-dim BGE-M3 embedding of Statement (D15).
	FactEmbedding []float32 `json:"fact_embedding,omitempty"`

	// ContentKey = sha256(tenant_id·subject·predicate·object) — D11. Stored
	// for duplicate detection; compute with ContentKey().
	ContentKey string `json:"content_key"`
	// Supersedes is the doc _id of the predecessor this fact replaces
	// (UPDATE) or retracts (INVALIDATE); "" for ADD. The version chain is
	// this explicit link, not content-key equality (D11).
	Supersedes string `json:"supersedes,omitempty"`
	// ExtractorVersion identifies the extraction pipeline version; part of
	// the ledger key (D13).
	ExtractorVersion string `json:"extractor_version"`

	// Tenancy / provenance fields (D16). SourceIDs carries the episodic
	// event IDs this fact was extracted from (provenance-as-ACL seam, D6).
	TenantID     string   `json:"tenant_id"`
	TeamID       string   `json:"team_id"`
	Scope        string   `json:"scope"`
	OwnerAgentID string   `json:"owner_agent_id"`
	SourceIDs    []string `json:"source_ids,omitempty"`

	// Bi-temporal timestamps (D3). Valid time [ValidAt, InvalidAt) is
	// real-world truth; transaction time [CreatedAt, ExpiredAt) is system
	// record currency. Nil InvalidAt/ExpiredAt mean open intervals.
	ValidAt   time.Time  `json:"valid_at"`
	InvalidAt *time.Time `json:"invalid_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiredAt *time.Time `json:"expired_at,omitempty"`
	// InvalidatedTxAt audit-stamps the one permitted in-place mutation:
	// closing InvalidAt (documented deviation in the plan's bi-temporal
	// semantics).
	InvalidatedTxAt *time.Time `json:"invalidated_tx_at,omitempty"`
}
