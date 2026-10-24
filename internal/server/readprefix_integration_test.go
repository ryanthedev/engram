//go:build integration

package server_test

// Read by semantic id prefix against a REAL cluster, a real OpenSearchStore and
// the real acl.Filter clause — the one property fakes cannot prove: a fact the
// caller cannot read that SHARES the prefix is never returned and never
// counted, so it can neither leak through the ambiguity error nor turn a
// prefix that is unique among the caller's readable facts into an ambiguity.

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/server"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

// reachEdges is an acl.EdgeSource granting each user a fixed agent set.
type reachEdges map[string]acl.Reach

func (r reachEdges) Reachability(_ context.Context, id auth.Identity) (acl.Reach, error) {
	return r[id.UserID], nil
}

// padID builds a 64-hex id from a prefix and a filler digit.
func padID(prefix string, fill byte) string {
	return prefix + strings.Repeat(string(fill), 64-len(prefix))
}

func TestReadPrefixEndToEnd(t *testing.T) {
	ctx := context.Background()
	base := testutil.OpenSearchURL()
	if _, err := store.Apply(ctx, testutil.HTTPClient, base); err != nil {
		t.Fatalf("applying cluster contract: %v", err)
	}
	semIdx := testutil.ScratchIndexName("semantic", t.Name())
	testutil.DeleteIndex(t, base, semIdx)
	t.Cleanup(func() { testutil.DeleteIndex(t, base, semIdx) })
	testutil.CreateScratchIndex(t, base, semIdx)

	st := store.NewOpenSearchStore(testutil.HTTPClient, base, store.WithSemanticIndex(semIdx))
	// Caller u1/a1 reaches only a1; private facts owned by a9 are unreadable.
	filter := acl.NewFilter(reachEdges{"u1": {Agents: []string{"a1"}}}, slog.Default())
	svc := &server.Server{Auditor: st, ACL: filter, ACLClause: filter, FactPrefix: st}

	var (
		readable     = padID("c0ffee00", 'a') // a1, t1: the caller's
		unreadable   = padID("c0ffee00", 'b') // a9, t1: same prefix, not the caller's
		crossTenant  = padID("c0ffee00", 'c') // a1, t2: same prefix, other tenant
		onlyHidden   = padID("d00d0000", 'b') // a9, t1: the prefix's only match is unreadable
		twinA, twinB = padID("e1e1e1e1", '1'), padID("e1e1e1e1", '2')
	)
	write := func(id, tenant, owner, subject string) {
		t.Helper()
		f := memory.SemanticFact{
			Subject: subject, Predicate: "p", Object: "o", Statement: subject + " p o",
			TenantID: tenant, Scope: acl.ScopePrivate, OwnerAgentID: owner, ExtractorVersion: "v1",
			ValidAt: time.Unix(1000, 0).UTC(), CreatedAt: time.Unix(1000, 0).UTC(),
		}
		f.ContentKey = memory.ContentKey(f.TenantID, f.Subject, f.Predicate, f.Object)
		if err := st.Create(ctx, id, f); err != nil {
			t.Fatalf("creating %s: %v", id, err)
		}
	}
	write(readable, "t1", "a1", "s-readable")
	write(unreadable, "t1", "a9", "s-unreadable")
	write(crossTenant, "t2", "a1", "s-cross")
	write(onlyHidden, "t1", "a9", "s-hidden")
	write(twinA, "t1", "a1", "s-twin-a")
	write(twinB, "t1", "a1", "s-twin-b")
	testutil.RefreshIndex(t, base, semIdx) // the seek has search visibility

	read := func(id string) (*engrampb.ReadResponse, error) {
		return svc.Read(authedCtx("t1", "u1", "a1"), &engrampb.ReadRequest{Id: id, Source: "semantic"})
	}

	t.Run("unique among readable resolves despite hidden siblings", func(t *testing.T) {
		resp, err := read("c0ffee00")
		if err != nil {
			t.Fatalf("Read(c0ffee00): %v — an unreadable or cross-tenant sibling must not make the prefix ambiguous", err)
		}
		if got := resp.GetFact().GetId(); got != readable {
			t.Errorf("resolved to %q, want the caller's own %q", got, readable)
		}
	})
	t.Run("only match unreadable is the opaque not-found", func(t *testing.T) {
		_, err := read("d00d0000")
		if status.Code(err) != codes.NotFound || status.Convert(err).Message() != "record not found" {
			t.Errorf("err = %v, want the opaque NOT_FOUND", err)
		}
	})
	t.Run("two readable matches is a self-correcting ambiguity", func(t *testing.T) {
		_, err := read("e1e1e1e1")
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "matches 2 records; use more characters") {
			t.Errorf("err = %v, want InvalidArgument naming 2 matches", err)
		}
		// One more character disambiguates.
		resp, err := read("e1e1e1e12")
		if err != nil || resp.GetFact().GetId() != twinB {
			t.Errorf("Read(e1e1e1e12) = %v, %v; want %s", resp.GetFact().GetId(), err, twinB)
		}
	})
	t.Run("no match at all is the opaque not-found", func(t *testing.T) {
		if _, err := read("0123abcd"); status.Code(err) != codes.NotFound {
			t.Errorf("err = %v, want NOT_FOUND", err)
		}
	})
	t.Run("full ids keep the exact path and its verdicts", func(t *testing.T) {
		if resp, err := read(readable); err != nil || resp.GetFact().GetId() != readable {
			t.Errorf("Read(full readable) = %v, %v", resp.GetFact().GetId(), err)
		}
		for _, id := range []string{unreadable, crossTenant} {
			if _, err := read(id); status.Code(err) != codes.NotFound {
				t.Errorf("Read(full %s) err = %v, want NOT_FOUND", id, err)
			}
		}
	})
	t.Run("another tenant resolves only its own", func(t *testing.T) {
		resp, err := svc.Read(authedCtx("t2", "u1", "a1"), &engrampb.ReadRequest{Id: "c0ffee00", Source: "semantic"})
		if err != nil || resp.GetFact().GetId() != crossTenant {
			t.Errorf("t2 Read(c0ffee00) = %v, %v; want %s", resp.GetFact().GetId(), err, crossTenant)
		}
	})
}
