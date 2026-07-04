package authgrpc

import (
	"context"

	"google.golang.org/grpc/credentials"
)

// BearerCreds is a grpc.PerRPCCredentials that attaches a raw bearer token to
// every outbound call — the client half of the barricade, used by the CLI and
// the MCP server's gRPC client. Marked insecure-transport-tolerant because the
// local stack runs plaintext; production terminates TLS at the ingress and
// this rides inside it.
type BearerCreds struct {
	// Token is the raw bearer token; sent as "authorization: Bearer <token>".
	Token string
	// RequireTLS, when true, refuses to send the token over a non-TLS channel.
	RequireTLS bool
}

var _ credentials.PerRPCCredentials = BearerCreds{}

// GetRequestMetadata returns the authorization header for the call.
func (c BearerCreds) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{metadataKey: bearerPrefix + c.Token}, nil
}

// RequireTransportSecurity reports whether the token may only ride a secure
// channel.
func (c BearerCreds) RequireTransportSecurity() bool { return c.RequireTLS }
