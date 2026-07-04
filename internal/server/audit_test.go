package server_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/authgrpc"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/server"
	"github.com/ryanthedev/engram/internal/store"
)

type fakeAuditor struct {
	target   store.VersionedFact
	versions []store.VersionedFact
	ok       bool
	err      error
}

func (f fakeAuditor) AuditFact(context.Context, string) (store.VersionedFact, []store.VersionedFact, bool, error) {
	return f.target, f.versions, f.ok, f.err
}

type fakeReadAuthz struct{ allow bool }

func (f fakeReadAuthz) CanRead(context.Context, auth.Identity, acl.Record) (bool, error) {
	return f.allow, nil
}

func factVF(id, tenant, owner string) store.VersionedFact {
	return store.VersionedFact{ID: id, Fact: memory.SemanticFact{
		Subject: "svc", Predicate: "owner", Object: "team-a", Statement: "svc owner team-a",
		TenantID: tenant, Scope: acl.ScopeTeam, TeamID: "teamX", OwnerAgentID: owner,
		SourceIDs: []string{"ev-1"}, ExtractorVersion: "v1",
		ValidAt: time.Unix(1000, 0).UTC(), CreatedAt: time.Unix(1000, 0).UTC(),
	}}
}

func authedCtx(tenant, user, agent string) context.Context {
	return authgrpc.WithIdentity(context.Background(), auth.Identity{TenantID: tenant, UserID: user, AgentID: agent})
}

// TestServerAuditAssemblesProvenanceAndVersions: an authorized audit returns
// the target's provenance and its full version list (DW-4.6, server layer).
func TestServerAuditAssemblesProvenanceAndVersions(t *testing.T) {
	target := factVF("v2", "t1", "a1")
	svc := &server.Server{
		Auditor: fakeAuditor{target: target, versions: []store.VersionedFact{factVF("v1", "t1", "a1"), target}, ok: true},
		ACL:     fakeReadAuthz{allow: true},
	}
	resp, err := svc.Audit(authedCtx("t1", "u1", "a1"), &engrampb.AuditRequest{Id: "v2"})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if resp.GetProvenance().GetOwnerAgentId() != "a1" || resp.GetProvenance().GetScope() != acl.ScopeTeam {
		t.Errorf("provenance = %+v", resp.GetProvenance())
	}
	if len(resp.GetProvenance().GetSourceIds()) != 1 || resp.GetProvenance().GetSourceIds()[0] != "ev-1" {
		t.Errorf("provenance source_ids = %v", resp.GetProvenance().GetSourceIds())
	}
	if len(resp.GetVersions()) != 2 {
		t.Fatalf("versions = %d, want 2", len(resp.GetVersions()))
	}
	if resp.GetVersions()[0].GetId() != "v1" || resp.GetVersions()[1].GetId() != "v2" {
		t.Errorf("version order = %s,%s", resp.GetVersions()[0].GetId(), resp.GetVersions()[1].GetId())
	}
}

// TestAuditFailClosed verifies every denial path returns an opaque NotFound (no
// existence oracle) rather than leaking the fact or a distinguishable error.
func TestAuditFailClosed(t *testing.T) {
	target := factVF("v2", "t1", "a1")
	cases := []struct {
		name string
		svc  *server.Server
		ctx  context.Context
		code codes.Code
	}{
		{
			"unauthorized reader",
			&server.Server{Auditor: fakeAuditor{target: target, ok: true}, ACL: fakeReadAuthz{allow: false}},
			authedCtx("t1", "u2", "a2"), codes.NotFound,
		},
		{
			"cross-tenant",
			&server.Server{Auditor: fakeAuditor{target: target, ok: true}, ACL: fakeReadAuthz{allow: true}},
			authedCtx("t-other", "u1", "a1"), codes.NotFound,
		},
		{
			"unknown id",
			&server.Server{Auditor: fakeAuditor{ok: false}, ACL: fakeReadAuthz{allow: true}},
			authedCtx("t1", "u1", "a1"), codes.NotFound,
		},
		{
			"empty id",
			&server.Server{Auditor: fakeAuditor{ok: true, target: target}},
			authedCtx("t1", "u1", "a1"), codes.InvalidArgument,
		},
		{
			"unconfigured",
			&server.Server{},
			authedCtx("t1", "u1", "a1"), codes.Unimplemented,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := "v2"
			if tc.name == "empty id" {
				id = ""
			}
			_, err := tc.svc.Audit(tc.ctx, &engrampb.AuditRequest{Id: id})
			if status.Code(err) != tc.code {
				t.Fatalf("Audit err = %v (code %s), want %s", err, status.Code(err), tc.code)
			}
		})
	}
}
