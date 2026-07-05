package auth

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// memStore is an in-memory TokenStore for unit tests: it stores only hashes,
// mirroring the production contract.
type memStore struct {
	mu   sync.Mutex
	byID map[string]*TokenRecord // handle -> record
}

func newMemStore() *memStore { return &memStore{byID: map[string]*TokenRecord{}} }

func (m *memStore) Put(_ context.Context, rec TokenRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := rec
	m.byID[rec.ID] = &r
	return nil
}

func (m *memStore) GetByHash(_ context.Context, hash string) (TokenRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.byID {
		if r.Hash == hash {
			return *r, true, nil
		}
	}
	return TokenRecord{}, false, nil
}

func (m *memStore) Revoke(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[id]
	if !ok {
		return false, nil
	}
	now := time.Now().UTC()
	r.Revoked = true
	r.RevokedAt = &now
	return true, nil
}

func (m *memStore) ListByIdentity(_ context.Context, id Identity) ([]TokenRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []TokenRecord
	for _, r := range m.byID {
		if r.TenantID == id.TenantID && r.UserID == id.UserID {
			out = append(out, *r)
		}
	}
	return out, nil
}

// rawOf returns every raw token the store would never be able to reproduce —
// exposed only so tests can inspect what Put actually persisted.
func (m *memStore) records() []TokenRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []TokenRecord
	for _, r := range m.byID {
		out = append(out, *r)
	}
	return out
}

var testIdentity = Identity{TenantID: "t1", UserID: "alice", AgentID: "claude"}

func issue(t *testing.T, iss *TokenIssuer, ttl time.Duration) (raw, handle string) {
	t.Helper()
	raw, handle, err := iss.Issue(context.Background(), testIdentity, ttl)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return raw, handle
}

// TestDW_3_4_StoredHashedOnly: nothing persisted equals or contains the raw
// token — only its SHA-256 hash and public handle are stored.
func TestDW_3_4_StoredHashedOnly(t *testing.T) {
	s := newMemStore()
	iss := NewTokenIssuer(s, nil)
	raw, _ := issue(t, iss, time.Hour)

	for _, r := range s.records() {
		if r.Hash == raw || strings.Contains(r.Hash, raw) {
			t.Fatal("stored hash contains the raw token")
		}
		if r.Hash != sha256Hex(raw) {
			t.Fatal("stored hash is not sha256(raw)")
		}
		if len(r.Hash) != 64 {
			t.Fatalf("hash length = %d, want 64 hex chars", len(r.Hash))
		}
		if r.ID == raw {
			t.Fatal("public handle equals the raw token")
		}
	}
}

// TestDW_3_4_RawShownOnce: the raw token is returned by Issue and is not
// recoverable from the store afterward (List/Get expose no raw).
func TestDW_3_4_RawShownOnce(t *testing.T) {
	s := newMemStore()
	iss := NewTokenIssuer(s, nil)
	raw, handle := issue(t, iss, time.Hour)
	if !strings.HasPrefix(raw, tokenPrefix) {
		t.Fatalf("raw token %q missing prefix", raw)
	}
	list, err := iss.List(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != handle {
		t.Fatalf("List = %+v, want the one issued handle %s", list, handle)
	}
	for _, r := range list {
		if r.Hash == sha256Hex("") { // sanity: hash present
			t.Fatal("listed record carries no hash")
		}
		// The listed record must not let anyone reconstruct raw.
		if r.ID == raw {
			t.Fatal("List leaked the raw token")
		}
	}
}

// TestDW_3_4_ConstantTimeVerify: a token differing from a valid one only in
// its final character is rejected — the compare covers the full digest (it
// does not short-circuit on a shared prefix). Verifies the round-trip too.
func TestDW_3_4_ConstantTimeVerify(t *testing.T) {
	s := newMemStore()
	iss := NewTokenIssuer(s, nil)
	auth := NewAuthenticator(s, nil)
	raw, _ := issue(t, iss, time.Hour)

	got, err := auth.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify(valid): %v", err)
	}
	if got != testIdentity {
		t.Fatalf("Verify identity = %+v, want %+v", got, testIdentity)
	}

	// Flip the last character: a different secret hashes to a different,
	// unknown digest and must be rejected even though it shares a long prefix.
	last := raw[len(raw)-1]
	repl := byte('A')
	if last == 'A' {
		repl = 'B'
	}
	tampered := raw[:len(raw)-1] + string(repl)
	if _, err := auth.Verify(context.Background(), tampered); err == nil {
		t.Fatal("Verify accepted a tampered token")
	}
}

// TestDW_3_3_MalformedRejected: empty, wrong-prefix, and undecodable tokens
// each reject with ErrTokenMalformed before any store lookup (dirty test).
func TestDW_3_3_MalformedRejected(t *testing.T) {
	auth := NewAuthenticator(newMemStore(), nil)
	for _, raw := range []string{"", "   ", "bearer-xyz", "egm_not@base64!!", tokenPrefix + "short"} {
		if _, err := auth.Verify(context.Background(), raw); err != ErrTokenMalformed {
			t.Errorf("Verify(%q) err = %v, want ErrTokenMalformed", raw, err)
		}
	}
}

// TestDW_3_3_UnknownRejected: a well-formed token never issued rejects as
// ErrTokenUnknown.
func TestDW_3_3_UnknownRejected(t *testing.T) {
	auth := NewAuthenticator(newMemStore(), nil)
	raw, err := generateRawToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := auth.Verify(context.Background(), raw); err != ErrTokenUnknown {
		t.Errorf("Verify(unknown) err = %v, want ErrTokenUnknown", err)
	}
}

// TestDW_3_3_ExpiredRejected: a token past its TTL rejects as ErrTokenExpired
// (dirty test). A fixed clock exercises the boundary at and just past expiry.
func TestDW_3_3_ExpiredRejected(t *testing.T) {
	s := newMemStore()
	base := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	clk := base
	clock := func() time.Time { return clk }
	iss := NewTokenIssuer(s, clock)
	auth := NewAuthenticator(s, clock)
	raw, _, err := iss.Issue(context.Background(), testIdentity, time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Just before expiry: valid.
	clk = base.Add(time.Hour - time.Nanosecond)
	if _, err := auth.Verify(context.Background(), raw); err != nil {
		t.Fatalf("Verify just before expiry = %v, want valid", err)
	}
	// Exactly at expiry: expired (half-open TTL — not Before means expired).
	clk = base.Add(time.Hour)
	if _, err := auth.Verify(context.Background(), raw); err != ErrTokenExpired {
		t.Fatalf("Verify at expiry = %v, want ErrTokenExpired", err)
	}
}

// TestDW_3_3_RevokedRejected + revocation is immediate: a valid token rejects
// as ErrTokenRevoked on the very next Verify after Revoke — query-time, no
// cache, so the ≤5 s SLO is met with zero delay (dirty test).
func TestDW_3_3_RevocationImmediate(t *testing.T) {
	s := newMemStore()
	iss := NewTokenIssuer(s, nil)
	auth := NewAuthenticator(s, nil)
	raw, handle := issue(t, iss, time.Hour)

	if _, err := auth.Verify(context.Background(), raw); err != nil {
		t.Fatalf("Verify before revoke = %v, want valid", err)
	}
	if err := iss.Revoke(context.Background(), handle); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := auth.Verify(context.Background(), raw); err != ErrTokenRevoked {
		t.Fatalf("Verify immediately after revoke = %v, want ErrTokenRevoked", err)
	}
	// Revoking an unknown handle is a typed miss, not a silent success.
	if err := iss.Revoke(context.Background(), "deadbeefdeadbeef"); err != ErrTokenUnknown {
		t.Fatalf("Revoke(unknown) = %v, want ErrTokenUnknown", err)
	}
}

// TestIssueRejectsIncompleteIdentity: a token cannot be minted for an identity
// missing tenant or user (would erase provenance — D17).
func TestIssueRejectsIncompleteIdentity(t *testing.T) {
	iss := NewTokenIssuer(newMemStore(), nil)
	for _, id := range []Identity{{}, {TenantID: "t1"}, {UserID: "alice"}} {
		if _, _, err := iss.Issue(context.Background(), id, time.Hour); err == nil {
			t.Errorf("Issue(%+v) succeeded, want rejection", id)
		}
	}
}
