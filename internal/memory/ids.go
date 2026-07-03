package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// sep joins ID components. The ASCII unit separator cannot appear in
// normalized field values, so the hash input is unambiguous ("a"+"bc" never
// collides with "ab"+"c").
const sep = "\x1f"

// ContentKey returns the D11 content key for a fact:
// sha256(tenant_id·subject·predicate·object), hex-encoded. It identifies the
// assertion's identity for duplicate detection; successive versions of a
// chain usually have DIFFERENT content keys (an UPDATE changes the object).
func ContentKey(tenantID, subject, predicate, object string) string {
	return hexSum(tenantID, subject, predicate, object)
}

// FactDocID returns the D11 semantic doc _id: sha256(content_key·valid_at),
// hex-encoded, with validAt canonicalized as UTC RFC3339Nano. Indexed with
// op_type=create, identical concurrent extractions produce the same _id and
// collide (409) instead of duplicating.
func FactDocID(contentKey string, validAt time.Time) string {
	return hexSum(contentKey, validAt.UTC().Format(time.RFC3339Nano))
}

// LedgerKey is the D13 idempotency identity for one extraction run:
// (tenant_id, event_id, extractor_version). It is claimed with op_type=create
// BEFORE extraction; bumping ExtractorVersion deliberately re-extracts.
type LedgerKey struct {
	TenantID         string `json:"tenant_id"`
	EventID          string `json:"event_id"`
	ExtractorVersion string `json:"extractor_version"`
}

// DocID returns the ledger entry's doc _id:
// sha256(tenant_id·event_id·extractor_version), hex-encoded.
func (k LedgerKey) DocID() string {
	return hexSum(k.TenantID, k.EventID, k.ExtractorVersion)
}

func hexSum(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, sep)))
	return hex.EncodeToString(h[:])
}
