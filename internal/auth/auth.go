// Package auth is the token-authentication barricade for every client
// surface (gRPC, MCP, CLI). It issues opaque 256-bit bearer tokens, stores
// only their salted-free SHA-256 hash (the raw token is shown exactly once at
// issuance), and verifies a presented token by hashing it, looking the hash
// up, and constant-time-comparing — then returns the bound Identity. Tokens
// are TTL'd and revocable; revocation is query-time (no cache), so it bites
// on the next call. TokenIssuer is the OIDC seam (D17): SSO later swaps the
// issuer without touching Verify or the gRPC barricade.
//
// Security posture (cc-defensive-programming): the token is external input at
// a trust boundary, so every rejection reason is a typed sentinel here, but
// the transport layer collapses them to one opaque "unauthenticated" so no
// detail leaks to the caller. All comparison of secret-derived material is
// constant-time.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Identity is the principal a token is bound to (D16/D17): the tenant, the
// human user, and the acting agent. Internal packages assume an Identity is
// present once past the barricade; provenance and (Phase 4) ACL are computed
// from it.
type Identity struct {
	TenantID string
	UserID   string
	AgentID  string
}

// Valid reports whether the identity carries the minimum binding a token may
// be issued for: a tenant and a user. AgentID may be empty (a user-scoped
// token not yet bound to a specific agent).
func (id Identity) Valid() bool {
	return id.TenantID != "" && id.UserID != ""
}

// identityCtxKey is the private context key under which a verified Identity is
// stored. It lives here (not in the transport layer) so business packages —
// which may not import gRPC — can still read the barricade-injected Identity
// without crossing the transport boundary. The type is private so no other
// package can forge or overwrite the value.
type identityCtxKey struct{}

// ContextWithIdentity returns ctx carrying id. The gRPC barricade calls this
// after verifying a token; in-process callers and tests use it to simulate an
// authenticated context. This is the single writer of the identity ctx value.
func ContextWithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// IdentityFromContext returns the Identity injected by the barricade, if any.
// A business package (e.g. the store's write-guard path) reads it to authorize
// a request without importing the transport layer; ok=false means no verified
// identity is present (an in-process/worker call), which callers treat as
// "skip client-scoped authorization", never as "authorized".
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityCtxKey{}).(Identity)
	return id, ok
}

// String renders the identity for logs (never includes a secret).
func (id Identity) String() string {
	return fmt.Sprintf("tenant=%s user=%s agent=%s", id.TenantID, id.UserID, id.AgentID)
}

// Typed rejection reasons. Verify returns exactly one; the transport barricade
// maps all of them to a single opaque error so callers cannot distinguish
// "unknown" from "expired" from "revoked" (no oracle). Tests assert on these
// to prove each rejection path (DW-3.3).
var (
	// ErrTokenMalformed means the presented string is not a well-formed Engram
	// token (empty, wrong prefix, or undecodable) — rejected before any lookup.
	ErrTokenMalformed = errors.New("auth: malformed token")
	// ErrTokenUnknown means a well-formed token whose hash is not on record.
	ErrTokenUnknown = errors.New("auth: unknown token")
	// ErrTokenExpired means a known token past its TTL.
	ErrTokenExpired = errors.New("auth: token expired")
	// ErrTokenRevoked means a known token explicitly revoked.
	ErrTokenRevoked = errors.New("auth: token revoked")
)

// tokenPrefix namespaces raw tokens so a malformed/foreign string is rejected
// cheaply and so leaked tokens are greppable in logs/secret scanners.
const tokenPrefix = "egm_"

// rawTokenBytes is the entropy per token (256 bits, the plan's requirement).
const rawTokenBytes = 32

// TokenRecord is one stored token: its public handle, the SHA-256 hash of the
// raw token (the only representation persisted), the bound identity, and its
// lifecycle timestamps. It never contains the raw token.
type TokenRecord struct {
	// ID is the public handle (hash prefix) shown at issuance and in `token
	// list`; it names a token for revocation without exposing the secret.
	ID        string     `json:"id"`
	Hash      string     `json:"hash"`
	TenantID  string     `json:"tenant_id"`
	UserID    string     `json:"user_id"`
	AgentID   string     `json:"agent_id"`
	IssuedAt  time.Time  `json:"issued_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	Revoked   bool       `json:"revoked"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// Identity returns the principal this record is bound to.
func (r TokenRecord) Identity() Identity {
	return Identity{TenantID: r.TenantID, UserID: r.UserID, AgentID: r.AgentID}
}

// TokenStore is the persistence seam (consumer-defined). OpenSearch backs it
// in production (internal/store.AuthTokenStore); tests use an in-memory fake.
// Implementations MUST store only the hash — never the raw token — and MUST
// key lookups on the hash so Verify is a single point read.
type TokenStore interface {
	// Put persists a new token record (its hash is the storage key).
	Put(ctx context.Context, rec TokenRecord) error
	// GetByHash returns the record for hash; ok=false when none exists.
	GetByHash(ctx context.Context, hash string) (rec TokenRecord, ok bool, err error)
	// Revoke marks the token with the given public handle revoked and reports
	// whether it existed. Revocation is durable and query-time (no cache), so
	// it takes effect on the next Verify.
	Revoke(ctx context.Context, id string) (found bool, err error)
	// ListByIdentity returns every token bound to id (revoked and live),
	// newest first — the `token list` surface. Records carry no raw secret.
	ListByIdentity(ctx context.Context, id Identity) ([]TokenRecord, error)
}

// Authenticator verifies presented tokens against the store. It is the read
// half of the barricade; the gRPC interceptor is its one caller in the
// request path.
type Authenticator struct {
	store TokenStore
	now   func() time.Time
}

// NewAuthenticator returns an Authenticator over store. clock may be nil
// (defaults to time.Now); tests inject a fixed clock to exercise TTL edges.
func NewAuthenticator(store TokenStore, clock func() time.Time) *Authenticator {
	if clock == nil {
		clock = time.Now
	}
	return &Authenticator{store: store, now: clock}
}

// Verify authenticates a raw bearer token and returns its bound Identity, or
// a typed rejection. It always hashes the input before any lookup (so the
// unknown-token path costs the same as the known-token path), looks the hash
// up as the storage key, constant-time-compares the stored hash, then checks
// revocation and TTL. The raw token is never logged or stored.
func (a *Authenticator) Verify(ctx context.Context, raw string) (Identity, error) {
	hash, err := hashToken(raw)
	if err != nil {
		return Identity{}, err
	}
	rec, ok, err := a.store.GetByHash(ctx, hash)
	if err != nil {
		return Identity{}, fmt.Errorf("auth: verifying token: %w", err)
	}
	if !ok {
		return Identity{}, ErrTokenUnknown
	}
	// Constant-time compare of the full digest: defends against a store that
	// returns a prefix/near match and keeps verification timing independent of
	// how many leading bytes match.
	if subtle.ConstantTimeCompare([]byte(rec.Hash), []byte(hash)) != 1 {
		return Identity{}, ErrTokenUnknown
	}
	if rec.Revoked {
		return Identity{}, ErrTokenRevoked
	}
	if !rec.ExpiresAt.IsZero() && !a.now().Before(rec.ExpiresAt) {
		return Identity{}, ErrTokenExpired
	}
	return rec.Identity(), nil
}

// TokenIssuer mints, revokes, and lists tokens. It is the OIDC seam (D17): an
// SSO integration later provides an alternate issuer while Verify and the
// barricade stay unchanged.
type TokenIssuer struct {
	store TokenStore
	now   func() time.Time
}

// NewTokenIssuer returns a TokenIssuer over store. clock may be nil.
func NewTokenIssuer(store TokenStore, clock func() time.Time) *TokenIssuer {
	if clock == nil {
		clock = time.Now
	}
	return &TokenIssuer{store: store, now: clock}
}

// Issue mints a token bound to id with lifetime ttl and returns the raw token
// exactly once — it is never recoverable afterward (only the hash is stored).
// A non-positive ttl means no expiry. The returned handle (rec.ID) names the
// token for later revocation.
func (t *TokenIssuer) Issue(ctx context.Context, id Identity, ttl time.Duration) (rawToken string, handle string, err error) {
	if !id.Valid() {
		return "", "", fmt.Errorf("auth: cannot issue a token for an incomplete identity (%s)", id)
	}
	raw, err := generateRawToken()
	if err != nil {
		return "", "", err
	}
	hash := sha256Hex(raw)
	now := t.now().UTC()
	rec := TokenRecord{
		ID:       hash[:16], // public handle: hash prefix, not the secret
		Hash:     hash,
		TenantID: id.TenantID,
		UserID:   id.UserID,
		AgentID:  id.AgentID,
		IssuedAt: now,
	}
	if ttl > 0 {
		rec.ExpiresAt = now.Add(ttl)
	}
	if err := t.store.Put(ctx, rec); err != nil {
		return "", "", fmt.Errorf("auth: storing issued token: %w", err)
	}
	return raw, rec.ID, nil
}

// Revoke revokes the token named by its public handle. It reports ErrTokenUnknown
// if no such token exists. Revocation is durable and effective on the next
// Verify (no cache to invalidate — the ≤5 s SLO is met immediately).
func (t *TokenIssuer) Revoke(ctx context.Context, handle string) error {
	found, err := t.store.Revoke(ctx, handle)
	if err != nil {
		return fmt.Errorf("auth: revoking token %s: %w", handle, err)
	}
	if !found {
		return ErrTokenUnknown
	}
	return nil
}

// List returns every token bound to id (the `token list` surface). Records
// carry the handle, identity, and lifecycle state — never a raw secret.
func (t *TokenIssuer) List(ctx context.Context, id Identity) ([]TokenRecord, error) {
	return t.store.ListByIdentity(ctx, id)
}

// generateRawToken returns a fresh prefixed 256-bit token, URL-safe base64.
func generateRawToken() (string, error) {
	buf := make([]byte, rawTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: reading entropy for token: %w", err)
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashToken validates the token's shape and returns the SHA-256 hex of the
// whole raw string. A malformed token is rejected before any store lookup.
func hashToken(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, tokenPrefix) {
		return "", ErrTokenMalformed
	}
	body := strings.TrimPrefix(raw, tokenPrefix)
	decoded, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(decoded) != rawTokenBytes {
		return "", ErrTokenMalformed
	}
	return sha256Hex(raw), nil
}

// sha256Hex returns the hex-encoded SHA-256 of s.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
