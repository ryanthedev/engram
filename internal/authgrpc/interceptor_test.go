package authgrpc

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/internal/auth"
)

// quietLogger discards expected-rejection noise from test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 4}))
}

// memStore is a minimal in-memory auth.TokenStore for barricade tests.
type memStore struct {
	mu   sync.Mutex
	byID map[string]*auth.TokenRecord
}

func newMemStore() *memStore { return &memStore{byID: map[string]*auth.TokenRecord{}} }

func (m *memStore) Put(_ context.Context, rec auth.TokenRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := rec
	m.byID[rec.ID] = &r
	return nil
}
func (m *memStore) GetByHash(_ context.Context, hash string) (auth.TokenRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.byID {
		if r.Hash == hash {
			return *r, true, nil
		}
	}
	return auth.TokenRecord{}, false, nil
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
func (m *memStore) ListByIdentity(_ context.Context, id auth.Identity) ([]auth.TokenRecord, error) {
	return nil, nil
}

// setup builds a barricade over a real Authenticator + issuer with a fixed
// clock so TTL edges are exercised deterministically.
func setup(t *testing.T) (*auth.TokenIssuer, grpc.UnaryServerInterceptor, *time.Time) {
	t.Helper()
	s := newMemStore()
	clk := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return clk }
	iss := auth.NewTokenIssuer(s, clock)
	authn := auth.NewAuthenticator(s, clock)
	interceptor := UnaryServerInterceptor(authn, quietLogger())
	return iss, interceptor, &clk
}

var testInfo = &grpc.UnaryServerInfo{FullMethod: "/engram.v1.Engram/Ingest"}

// callWith runs the interceptor with the given authorization metadata and
// returns the identity the handler saw (if any) plus the error.
func callWith(interceptor grpc.UnaryServerInterceptor, authzValue string) (auth.Identity, bool, error) {
	ctx := context.Background()
	if authzValue != "" {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(metadataKey, authzValue))
	}
	var seen auth.Identity
	var ok bool
	_, err := interceptor(ctx, nil, testInfo, func(hctx context.Context, _ any) (any, error) {
		seen, ok = IdentityFrom(hctx)
		return nil, nil
	})
	return seen, ok, err
}

func mustUnauthenticated(t *testing.T, err error) {
	t.Helper()
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
	}
	if msg := status.Convert(err).Message(); msg != "unauthenticated" {
		t.Fatalf("message = %q, want opaque %q (no detail leak)", msg, "unauthenticated")
	}
}

// TestDW_3_3_NoTokenRejected: a call with no bearer token is rejected and the
// handler never runs.
func TestDW_3_3_NoTokenRejected(t *testing.T) {
	_, interceptor, _ := setup(t)
	_, ok, err := callWith(interceptor, "")
	if ok {
		t.Fatal("handler ran without a token")
	}
	mustUnauthenticated(t, err)
}

// TestDW_3_3_MalformedRejected: a mis-schemed or garbage token is rejected
// opaquely (dirty test).
func TestDW_3_3_MalformedRejected(t *testing.T) {
	_, interceptor, _ := setup(t)
	for _, v := range []string{"Bearer ", "Basic abc", "Bearer not-a-token", "egm_raw-without-scheme"} {
		if _, ok, err := callWith(interceptor, v); ok || status.Code(err) != codes.Unauthenticated {
			t.Errorf("authz %q: ok=%v err=%v, want rejected Unauthenticated", v, ok, err)
		}
	}
}

// TestDW_3_3_ValidTokenInjectsIdentity: a valid token authenticates and the
// handler sees the bound identity.
func TestDW_3_3_ValidTokenInjectsIdentity(t *testing.T) {
	iss, interceptor, _ := setup(t)
	id := auth.Identity{TenantID: "t1", UserID: "alice", AgentID: "claude"}
	raw, _, err := iss.Issue(context.Background(), id, time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	seen, ok, err := callWith(interceptor, "Bearer "+raw)
	if err != nil {
		t.Fatalf("valid call err = %v", err)
	}
	// DeepEqual: Identity carries a Roles slice since Phase 2 of the
	// knowledge platform, so == no longer compiles.
	if !ok || !reflect.DeepEqual(seen, id) {
		t.Fatalf("handler identity = %+v (ok=%v), want %+v", seen, ok, id)
	}
}

// TestDW_3_3_ExpiredRejected: a token past its TTL is rejected opaquely (dirty
// test).
func TestDW_3_3_ExpiredRejected(t *testing.T) {
	iss, interceptor, clk := setup(t)
	raw, _, err := iss.Issue(context.Background(), auth.Identity{TenantID: "t1", UserID: "alice"}, time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	*clk = clk.Add(2 * time.Hour) // advance past expiry
	_, ok, err := callWith(interceptor, "Bearer "+raw)
	if ok {
		t.Fatal("expired token authenticated")
	}
	mustUnauthenticated(t, err)
}

// TestDW_3_3_RevokedRejectedImmediately: a revoked token is rejected on the
// next call — revocation is query-time, so ≤5 s is met with zero delay (dirty
// test).
func TestDW_3_3_RevokedRejectedImmediately(t *testing.T) {
	iss, interceptor, _ := setup(t)
	raw, handle, err := iss.Issue(context.Background(), auth.Identity{TenantID: "t1", UserID: "alice"}, time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, ok, err := callWith(interceptor, "Bearer "+raw); !ok || err != nil {
		t.Fatalf("pre-revoke call ok=%v err=%v, want authenticated", ok, err)
	}
	if err := iss.Revoke(context.Background(), handle); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	_, ok, err := callWith(interceptor, "Bearer "+raw)
	if ok {
		t.Fatal("revoked token authenticated on the next call")
	}
	mustUnauthenticated(t, err)
}

// TestExemptMethodSkipsAuth: an exempt method (e.g. a health check) runs
// without a token.
func TestExemptMethodSkipsAuth(t *testing.T) {
	s := newMemStore()
	authn := auth.NewAuthenticator(s, nil)
	interceptor := UnaryServerInterceptor(authn, quietLogger(), "/grpc.health.v1.Health/Check")
	ran := false
	_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"},
		func(context.Context, any) (any, error) { ran = true; return nil, nil })
	if err != nil || !ran {
		t.Fatalf("exempt method err=%v ran=%v, want it to run", err, ran)
	}
}
