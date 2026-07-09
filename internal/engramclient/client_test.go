package engramclient_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/authgrpc"
	"github.com/ryanthedev/engram/internal/engramclient"
	"github.com/ryanthedev/engram/internal/graph"
	"github.com/ryanthedev/engram/internal/server"
)

// fakeVerifier resolves exactly one token to one identity — everything else
// is rejected, so the REAL authgrpc interceptor exercises both barricade
// outcomes over a real TCP connection.
type fakeVerifier struct {
	token string
	id    auth.Identity
}

func (f fakeVerifier) Verify(_ context.Context, raw string) (auth.Identity, error) {
	if raw == f.token {
		return f.id, nil
	}
	return auth.Identity{}, errors.New("unknown token")
}

// startExportServer runs an engram gRPC server on a loopback port behind the
// production auth interceptor, backed by a MemBackend graph seeded with
// nEntities entities for tenant t1. Returns the dial address.
func startExportServer(t *testing.T, nEntities int) string {
	t.Helper()
	backend := graph.NewMemBackend()
	now := time.Unix(1000, 0).UTC()
	for i := 0; i < nEntities; i++ {
		e := graph.Entity{
			ID: fmt.Sprintf("e%05d", i), TenantID: "t1", Scope: "private",
			OwnerAgentID: "a1", Name: fmt.Sprintf("Entity %d", i),
			MentionCount: 1, ValidAt: now, CreatedAt: now,
		}
		if err := backend.PutEntity(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	grpcServer := grpc.NewServer(grpc.ChainUnaryInterceptor(
		authgrpc.UnaryServerInterceptor(fakeVerifier{token: "good-token", id: auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"}}, quiet),
	))
	engrampb.RegisterEngramServer(grpcServer, &server.Server{Exporter: backend})
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)
	return lis.Addr().String()
}

// TestDW_2_5_ClientExportReturnsPageAndCursor: engramclient.Export pages the
// caller's graph over a real authenticated connection — a bounded first page
// with an advancing cursor, then exhaustion with an empty one.
func TestDW_2_5_ClientExportReturnsPageAndCursor(t *testing.T) {
	addr := startExportServer(t, 501) // one past the 500 scan batch
	c, err := engramclient.Dial(addr, "good-token")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	page1, err := c.Export(context.Background(), "")
	if err != nil {
		t.Fatalf("Export page 1: %v", err)
	}
	if len(page1.GetEntities()) != 500 || page1.GetNextCursor() == "" {
		t.Fatalf("page 1: %d entities, cursor %q — want 500 + advancing cursor", len(page1.GetEntities()), page1.GetNextCursor())
	}

	page2, err := c.Export(context.Background(), page1.GetNextCursor())
	if err != nil {
		t.Fatalf("Export page 2: %v", err)
	}
	if len(page2.GetEntities()) != 1 || page2.GetNextCursor() != "" {
		t.Fatalf("page 2: %d entities, cursor %q — want the last entity + terminal cursor", len(page2.GetEntities()), page2.GetNextCursor())
	}
}

// TestDW_2_5_ClientExportUnauthenticatedRejected: the EXISTING interceptor
// rejects an Export with a bad (or missing) token before the handler runs —
// opaquely, with codes.Unauthenticated.
func TestDW_2_5_ClientExportUnauthenticatedRejected(t *testing.T) {
	addr := startExportServer(t, 1)
	for name, token := range map[string]string{"wrong token": "wrong-token", "empty token": ""} {
		c, err := engramclient.Dial(addr, token)
		if err != nil {
			t.Fatalf("%s: Dial: %v", name, err)
		}
		_, err = c.Export(context.Background(), "")
		if status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s: code = %v, want Unauthenticated", name, status.Code(err))
		}
		_ = c.Close()
	}
}
