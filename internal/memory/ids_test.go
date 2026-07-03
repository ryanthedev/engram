package memory_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/testutil"
)

// TestDW_0_8_ContentKeyAndDocIDContract verifies the D11 ID scheme: content
// keys and doc _ids are deterministic, unambiguous across field boundaries,
// valid_at-sensitive, and timezone-canonical.
func TestDW_0_8_ContentKeyAndDocIDContract(t *testing.T) {
	k1 := memory.ContentKey("t1", "svc", "owns", "team-a")
	k2 := memory.ContentKey("t1", "svc", "owns", "team-a")
	if k1 != k2 {
		t.Fatalf("ContentKey not deterministic: %s != %s", k1, k2)
	}
	if len(k1) != 64 {
		t.Fatalf("ContentKey should be hex sha256 (64 chars), got %d", len(k1))
	}

	// Field boundaries must be unambiguous: ("a","bc") vs ("ab","c").
	if memory.ContentKey("t", "a", "bc", "o") == memory.ContentKey("t", "ab", "c", "o") {
		t.Fatal("ContentKey collides across field boundaries — separator missing")
	}

	// Different objects (an UPDATE changes the object) yield different keys:
	// the version chain cannot be key equality, it must be the supersedes link.
	if memory.ContentKey("t1", "svc", "owns", "team-a") == memory.ContentKey("t1", "svc", "owns", "team-b") {
		t.Fatal("ContentKey ignores object")
	}

	validAt := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	id1 := memory.FactDocID(k1, validAt)
	id2 := memory.FactDocID(k1, validAt)
	if id1 != id2 {
		t.Fatalf("FactDocID not deterministic — identical concurrent extractions would NOT collide")
	}
	if memory.FactDocID(k1, validAt.Add(time.Second)) == id1 {
		t.Fatal("FactDocID ignores valid_at")
	}

	// Timezone canonicalization: same instant, different zone → same _id.
	nyc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("loading tz: %v", err)
	}
	if memory.FactDocID(k1, validAt.In(nyc)) != id1 {
		t.Fatal("FactDocID is not timezone-canonical (must format as UTC)")
	}

	// Ledger key (D13): deterministic, extractor-version-sensitive.
	lk := memory.LedgerKey{TenantID: "t1", EventID: "ev-1", ExtractorVersion: "x1"}
	if lk.DocID() != (memory.LedgerKey{TenantID: "t1", EventID: "ev-1", ExtractorVersion: "x1"}).DocID() {
		t.Fatal("LedgerKey.DocID not deterministic")
	}
	if lk.DocID() == (memory.LedgerKey{TenantID: "t1", EventID: "ev-1", ExtractorVersion: "x2"}).DocID() {
		t.Fatal("LedgerKey.DocID ignores extractor_version — bumped reprocess would not re-claim")
	}
}

// TestDW_0_8_StructsCarryChainAndLedgerFields verifies the record structs
// reflect the ID & idempotency contract: the chain fields on SemanticFact,
// the four bi-temporal timestamps, and the outbox + tenancy fields on
// Episodic (D11/D13/D16).
func TestDW_0_8_StructsCarryChainAndLedgerFields(t *testing.T) {
	requireFields(t, reflect.TypeOf(memory.SemanticFact{}), map[string]string{
		"ContentKey":       "content_key",
		"Supersedes":       "supersedes,omitempty",
		"ExtractorVersion": "extractor_version",
		"ValidAt":          "valid_at",
		"InvalidAt":        "invalid_at,omitempty",
		"CreatedAt":        "created_at",
		"ExpiredAt":        "expired_at,omitempty",
		"InvalidatedTxAt":  "invalidated_tx_at,omitempty",
		"TenantID":         "tenant_id",
		"TeamID":           "team_id",
		"Scope":            "scope",
		"OwnerAgentID":     "owner_agent_id",
		"SourceIDs":        "source_ids,omitempty",
	})
	requireFields(t, reflect.TypeOf(memory.Episodic{}), map[string]string{
		"EventID":         "event_id",
		"ProcessedAt":     "processed_at,omitempty",
		"ClaimLeaseUntil": "claim_lease_until,omitempty",
		"Attempts":        "attempts",
		"TenantID":        "tenant_id",
		"TeamID":          "team_id",
		"Scope":           "scope",
		"OwnerAgentID":    "owner_agent_id",
		"SourceIDs":       "source_ids,omitempty",
	})
	requireFields(t, reflect.TypeOf(memory.LedgerKey{}), map[string]string{
		"TenantID":         "tenant_id",
		"EventID":          "event_id",
		"ExtractorVersion": "extractor_version",
	})
}

// TestDW_0_8_ContractDocumentedInCode verifies the D11/D13 contract prose
// lives in the package documentation (doc.go), covering the content-key
// scheme, doc-_id format, supersedes chain, and claim-first ledger.
func TestDW_0_8_ContractDocumentedInCode(t *testing.T) {
	root := testutil.RepoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "internal", "memory", "doc.go"))
	if err != nil {
		t.Fatalf("reading doc.go: %v", err)
	}
	doc := string(b)
	for _, phrase := range []string{
		"content_key = sha256(tenant_id · subject · predicate · object)",
		"_id = sha256(content_key · valid_at)",
		"op_type=create",
		"supersedes",
		"NOT content-key equality",
		"(tenant_id, event_id, extractor_version)",
		"BEFORE",
		"cached extraction",
	} {
		if !strings.Contains(doc, phrase) {
			t.Errorf("ID & idempotency contract missing from doc.go: %q", phrase)
		}
	}
}

func requireFields(t *testing.T, typ reflect.Type, want map[string]string) {
	t.Helper()
	for name, tag := range want {
		f, ok := typ.FieldByName(name)
		if !ok {
			t.Errorf("%s missing field %s", typ.Name(), name)
			continue
		}
		if got := f.Tag.Get("json"); got != tag {
			t.Errorf("%s.%s json tag = %q, want %q", typ.Name(), name, got, tag)
		}
	}
}
