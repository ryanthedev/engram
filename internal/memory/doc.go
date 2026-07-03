// Package memory defines Engram's core record types and the ID & idempotency
// contract (plan decisions D11/D13). It is the innermost ring: it imports only
// the standard library, and every other package's seams are expressed in these
// types.
//
// # ID & idempotency contract (D11 / D13)
//
// Content key:
//
//	content_key = sha256(tenant_id · subject · predicate · object)
//
// stored on every SemanticFact and used for duplicate detection. Fields are
// joined with the ASCII unit separator (0x1F) so the key is unambiguous.
//
// Semantic doc _id:
//
//	_id = sha256(content_key · valid_at)
//
// with valid_at canonicalized as UTC RFC3339Nano. Documents are indexed with
// op_type=create, so identical concurrent extractions collide (HTTP 409)
// instead of duplicating; the loser re-reads and re-reconciles.
//
// Version chain: the supersedes field is an explicit link to the predecessor
// doc _id, set by the reconciler on UPDATE/INVALIDATE. The chain is this link,
// NOT content-key equality — an UPDATE usually changes the object, so
// successive versions have different content keys.
//
// Extraction ledger (claim-first, D13): idempotency identity is the ledger key
// (tenant_id, event_id, extractor_version), claimed with op_type=create BEFORE
// extraction. The extraction output is persisted into the ledger entry before
// any semantic write, so a retry resumes from the cached extraction and never
// re-calls the LLM (extraction is nondeterministic; re-extraction would orphan
// near-duplicate facts).
//
// # Bi-temporal semantics (D3)
//
// Valid time (real world) is the half-open interval [valid_at, invalid_at);
// invalid_at == nil means still valid. Transaction time (system) is
// [created_at, expired_at); expired_at == nil means current record. Closing
// invalid_at is the one permitted in-place mutation and is audit-stamped with
// invalidated_tx_at. Writers stamp system time; agents never set it.
//
// Language note: Go is the locked implementation language (D0, confirmed in
// Phase 0 of the walking-skeleton plan).
package memory
