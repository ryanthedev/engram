package server_test

// Barricade tests for the MemoryPurge handler: role gating, the tenant pin,
// argument validation, and the unwired-seam case. The MemoryPurger seam is
// faked here (consumer-defined interface); the live-cluster behaviour — the
// tombstone, the replay duplicates, the ledger row — is covered by
// purge_integration_test.go.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/server"
	"github.com/ryanthedev/engram/internal/store"
)

// fakePurger records what the handler asked for and answers with fixed
// counts, so a test can assert on the tenant/event/dry-run arguments the
// barricade derived rather than on any storage behaviour.
type fakePurger struct {
	tenants  []string
	events   []string
	dryRuns  []bool
	counts   store.PurgeCounts
	errAfter int // return an error on the (1-based) Nth call; 0 = never
	calls    int
}

func (p *fakePurger) PurgeEvent(_ context.Context, tenantID, eventID string, dryRun bool) (store.PurgeCounts, error) {
	p.calls++
	p.tenants = append(p.tenants, tenantID)
	p.events = append(p.events, eventID)
	p.dryRuns = append(p.dryRuns, dryRun)
	if p.errAfter != 0 && p.calls == p.errAfter {
		return p.counts, errors.New("fake purger: boom")
	}
	return p.counts, nil
}

// purgeServer wires a Server with only the purge seam configured — the same
// minimal shape a handler test needs, mirroring knowledgeServer.
func purgeServer(p *fakePurger) *server.Server { return &server.Server{Purger: p} }

// TestMemoryPurgeRoleGating: only a token holding memory-admin may purge.
// Every other caller — including one holding the knowledge platform's admin
// role — is denied, and the seam is never reached.
func TestMemoryPurgeRoleGating(t *testing.T) {
	tests := []struct {
		name     string
		ctx      context.Context
		wantCode codes.Code
	}{
		{"memory-admin is allowed", identityCtx(server.RoleMemoryAdmin), codes.OK},
		{"no roles is denied", identityCtx(), codes.PermissionDenied},
		{"an unrelated role is denied", identityCtx("harvester"), codes.PermissionDenied},
		{"knowledge admin does not confer memory purge", identityCtx(server.RoleKnowledgeAdmin), codes.PermissionDenied},
		{"an unauthenticated caller is denied", context.Background(), codes.PermissionDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakePurger{}
			_, err := purgeServer(p).MemoryPurge(tt.ctx, &engrampb.MemoryPurgeRequest{EventIds: []string{"ev-1"}})
			if got := status.Code(err); got != tt.wantCode {
				t.Fatalf("MemoryPurge = %v (code %v), want %v", err, got, tt.wantCode)
			}
			if tt.wantCode != codes.OK && p.calls != 0 {
				t.Errorf("the purger was reached %d time(s) despite denial", p.calls)
			}
		})
	}
}

// TestMemoryPurgeAuthorizesBeforeValidating pins knowledge.go's barricade
// ordering: an unauthorized caller sending garbage arguments gets
// PermissionDenied, never an InvalidArgument that would confirm its request
// shape was even parsed.
func TestMemoryPurgeAuthorizesBeforeValidating(t *testing.T) {
	p := &fakePurger{}
	_, err := purgeServer(p).MemoryPurge(identityCtx("reader"), &engrampb.MemoryPurgeRequest{EventIds: nil})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Errorf("MemoryPurge(unauthorized + empty ids) = %v (code %v), want PermissionDenied", err, got)
	}
}

// TestMemoryPurgePinsTenantFromIdentity: the request carries no tenant field
// at all, and the handler must pass the token's tenant to every call. This is
// the cross-tenant guard — there is no request field a caller could use to
// reach elsewhere.
func TestMemoryPurgePinsTenantFromIdentity(t *testing.T) {
	p := &fakePurger{counts: store.PurgeCounts{Episodic: 2, Ledger: 1, Semantic: 3}}
	resp, err := purgeServer(p).MemoryPurge(identityCtx(server.RoleMemoryAdmin),
		&engrampb.MemoryPurgeRequest{EventIds: []string{"ev-1", "ev-2"}})
	if err != nil {
		t.Fatalf("MemoryPurge: %v", err)
	}
	for i, tenant := range p.tenants {
		if tenant != "t1" { // identityCtx mints TenantID "t1"
			t.Errorf("call %d used tenant %q, want the identity's t1", i, tenant)
		}
	}
	if strings.Join(p.events, ",") != "ev-1,ev-2" {
		t.Errorf("purged events = %v, want [ev-1 ev-2] in order", p.events)
	}
	// Counts sum across every requested id.
	if resp.GetEpisodic() != 4 || resp.GetLedger() != 2 || resp.GetSemantic() != 6 {
		t.Errorf("summed counts = (%d, %d, %d), want (4, 2, 6)", resp.GetEpisodic(), resp.GetLedger(), resp.GetSemantic())
	}
}

// TestMemoryPurgeDryRunPassesThroughAndEchoes: the handler forwards dry_run
// verbatim to the seam and echoes it on the response, so a caller can never
// misread a preview as a completed purge.
func TestMemoryPurgeDryRunPassesThroughAndEchoes(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		p := &fakePurger{}
		resp, err := purgeServer(p).MemoryPurge(identityCtx(server.RoleMemoryAdmin),
			&engrampb.MemoryPurgeRequest{EventIds: []string{"ev-1"}, DryRun: dryRun})
		if err != nil {
			t.Fatalf("MemoryPurge(dryRun=%v): %v", dryRun, err)
		}
		if len(p.dryRuns) != 1 || p.dryRuns[0] != dryRun {
			t.Errorf("seam received dryRun=%v, want %v", p.dryRuns, dryRun)
		}
		if resp.GetDryRun() != dryRun {
			t.Errorf("response DryRun = %v, want %v", resp.GetDryRun(), dryRun)
		}
	}
}

// TestMemoryPurgeArgumentValidation: an authorized caller's malformed
// arguments are INVALID_ARGUMENT and nothing is purged. A blank id is
// rejected, not skipped — it almost always means an unset variable, and
// dropping it would report success while missing the row that was meant.
func TestMemoryPurgeArgumentValidation(t *testing.T) {
	tooMany := make([]string, 501)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("ev-%d", i)
	}
	tests := []struct {
		name string
		ids  []string
	}{
		{"no ids at all", nil},
		{"empty id list", []string{}},
		{"a blank id", []string{"ev-1", ""}},
		{"a whitespace-only id", []string{"   "}},
		{"over the per-call cap", tooMany},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakePurger{}
			_, err := purgeServer(p).MemoryPurge(identityCtx(server.RoleMemoryAdmin),
				&engrampb.MemoryPurgeRequest{EventIds: tt.ids})
			if got := status.Code(err); got != codes.InvalidArgument {
				t.Fatalf("MemoryPurge(%v) = %v (code %v), want InvalidArgument", tt.ids, err, got)
			}
			if p.calls != 0 {
				t.Errorf("the purger was reached %d time(s) despite invalid arguments", p.calls)
			}
		})
	}
	// Exactly at the cap is accepted: the boundary is inclusive.
	p := &fakePurger{}
	if _, err := purgeServer(p).MemoryPurge(identityCtx(server.RoleMemoryAdmin),
		&engrampb.MemoryPurgeRequest{EventIds: tooMany[:500]}); err != nil {
		t.Errorf("MemoryPurge with exactly 500 ids: %v", err)
	}
}

// TestMemoryPurgeUnconfiguredIsUnimplemented mirrors the Auditor precedent: a
// Server without purge wiring answers UNIMPLEMENTED rather than panicking on
// a nil seam — and does so before any authorization decision is even needed.
func TestMemoryPurgeUnconfiguredIsUnimplemented(t *testing.T) {
	svc := &server.Server{}
	_, err := svc.MemoryPurge(identityCtx(server.RoleMemoryAdmin), &engrampb.MemoryPurgeRequest{EventIds: []string{"ev-1"}})
	if got := status.Code(err); got != codes.Unimplemented {
		t.Errorf("MemoryPurge on an unwired server = %v (code %v), want Unimplemented", err, got)
	}
}

// TestMemoryPurgeStoreErrorIsInternal: a seam failure part-way through a
// multi-id purge surfaces as Internal naming the event that failed. The
// events before it were already mutated — that is why the handler logs the
// running counts on the error path (the gRPC error carries no body).
func TestMemoryPurgeStoreErrorIsInternal(t *testing.T) {
	p := &fakePurger{counts: store.PurgeCounts{Episodic: 1}, errAfter: 2}
	_, err := purgeServer(p).MemoryPurge(identityCtx(server.RoleMemoryAdmin),
		&engrampb.MemoryPurgeRequest{EventIds: []string{"ev-1", "ev-2", "ev-3"}})
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("MemoryPurge with a failing seam = %v (code %v), want Internal", err, got)
	}
	if !strings.Contains(err.Error(), "ev-2") {
		t.Errorf("error %q does not name the failing event id", err)
	}
	if p.calls != 2 {
		t.Errorf("purger calls = %d, want 2 (the run stops at the failure)", p.calls)
	}
}
