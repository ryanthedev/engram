// Package authgrpc is the gRPC transport barricade: a unary interceptor that
// authenticates every call's bearer token, injects the resolved Identity into
// the context, and rejects everything else — so the service handlers and the
// business packages behind them may assume an Identity is present without
// re-validating (cc-defensive-programming: one barricade at the process edge,
// assertions inside it). It is transport-adjacent by design and is one of the
// few packages permitted to import gRPC (the depguard rule excludes it).
package authgrpc

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/internal/auth"
)

// metadataKey is the gRPC metadata key carrying the bearer token (gRPC
// lowercases header keys). Value form: "Bearer <raw-token>".
const metadataKey = "authorization"

// bearerPrefix is the scheme in the authorization metadata value.
const bearerPrefix = "Bearer "

// identityCtxKey is the private context key under which the barricade stores
// the verified Identity. Private type so no other package can forge it.
type identityCtxKey struct{}

// Verifier is the read half of auth the interceptor depends on
// (consumer-defined seam — *auth.Authenticator satisfies it). Kept minimal so
// an SSO verifier can slot in behind the same barricade later (D17).
type Verifier interface {
	Verify(ctx context.Context, raw string) (auth.Identity, error)
}

// errUnauthenticated is the single opaque status returned for every rejection
// reason — the caller learns only "unauthenticated", never whether the token
// was malformed, unknown, expired, or revoked (no oracle / no detail leak).
var errUnauthenticated = status.Error(codes.Unauthenticated, "unauthenticated")

// UnaryServerInterceptor returns a grpc.UnaryServerInterceptor that verifies
// the bearer token on every call, injects the Identity, and otherwise rejects
// with an opaque Unauthenticated. The real rejection reason is logged
// server-side only. Methods in the exempt set (e.g. health checks) skip auth.
func UnaryServerInterceptor(v Verifier, logger *slog.Logger, exempt ...string) grpc.UnaryServerInterceptor {
	if logger == nil {
		logger = slog.Default()
	}
	skip := make(map[string]bool, len(exempt))
	for _, m := range exempt {
		skip[m] = true
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if skip[info.FullMethod] {
			return handler(ctx, req)
		}
		raw, err := bearerToken(ctx)
		if err != nil {
			logger.WarnContext(ctx, "auth rejected: no bearer token", "method", info.FullMethod, "reason", err)
			return nil, errUnauthenticated
		}
		id, err := v.Verify(ctx, raw)
		if err != nil {
			// Log the typed reason for operators; never return it to the caller.
			logger.WarnContext(ctx, "auth rejected", "method", info.FullMethod, "reason", err)
			return nil, errUnauthenticated
		}
		return handler(WithIdentity(ctx, id), req)
	}
}

// bearerToken extracts the raw token from the authorization metadata. A
// missing/blank/mis-schemed header is an error (mapped to Unauthenticated by
// the caller). This is the input-validation step of the barricade.
func bearerToken(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", errors.New("no request metadata")
	}
	vals := md.Get(metadataKey)
	if len(vals) == 0 {
		return "", errors.New("no authorization metadata")
	}
	v := strings.TrimSpace(vals[0])
	if !strings.HasPrefix(v, bearerPrefix) {
		return "", errors.New("authorization is not a Bearer token")
	}
	raw := strings.TrimSpace(strings.TrimPrefix(v, bearerPrefix))
	if raw == "" {
		return "", errors.New("empty bearer token")
	}
	return raw, nil
}

// WithIdentity returns ctx carrying id — used by the interceptor and by
// in-process callers/tests that bypass the transport.
func WithIdentity(ctx context.Context, id auth.Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// IdentityFrom returns the Identity the barricade injected, if present.
// Handlers past the barricade may assert ok is true; a missing identity there
// is a programming error (the interceptor was not wired), not a client fault.
func IdentityFrom(ctx context.Context) (auth.Identity, bool) {
	id, ok := ctx.Value(identityCtxKey{}).(auth.Identity)
	return id, ok
}
