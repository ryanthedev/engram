// This file holds the memory-tier purge gRPC handler: the request barricade
// for MemoryPurge, the one sanctioned way to erase an ingested event from
// append-only memory. It follows knowledge.go's barricade ordering exactly
// (documented at internal/server/knowledge.go:1-14): resolve the verified
// Identity from the context, AUTHORIZE FIRST — so an unauthorized caller
// learns nothing about its own arguments, not even that they were malformed —
// then validate every request field at this edge, and only then call the inner
// seam, which may assume validated input.
//
// Deliberately NOT exposed as an MCP tool (internal/mcp/tools.go is
// unchanged): purge is destructive and irreversible on the episodic tier, and
// an LLM caller should not be able to reach it by reasoning its way there. The
// operator surface is `engram purge` (internal/cli/purge.go), which is
// dry-run by default and requires an explicit --confirm.

package server

import (
	"context"
	"log/slog"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/authgrpc"
	"github.com/ryanthedev/engram/internal/store"
)

// RoleMemoryAdmin is the role a token must hold to purge memory-tier data.
//
// The memory tier otherwise has no role model at all — reads are gated by the
// tenant pin plus acl.CanRead/ScopeGuard, and writes by the ingest scope
// guard, none of which consult Identity.Roles. Roles ARE carried on memory
// tokens, though (auth.Identity.Roles, minted by `engram token create
// --roles`), so gating purge on one costs no new machinery and keeps the
// blast radius off every ordinary ingest token. The role name is distinct
// from the knowledge platform's "admin" (RoleKnowledgeAdmin) on purpose:
// holding knowledge-admin must not confer the ability to erase memory.
const RoleMemoryAdmin = "memory-admin"

// maxPurgeEventIDs caps how many events one MemoryPurge call may name. The
// handler issues three by-query requests per id, so an unbounded list is both
// a long-running request and a large accidental blast radius from one typo in
// a generated argument list. A bulk migration that needs more is re-run in
// batches — deliberately a conscious act per batch.
const maxPurgeEventIDs = 500

// MemoryPurger erases one event's memory-tier footprint (consumer-defined
// seam; *store.OpenSearchStore satisfies it). It is declared here rather than
// widened onto store.Store on purpose: store.Store is implemented by the
// worker's and server's test fakes, and adding a method there would break
// every one of them for a capability neither the worker nor any other handler
// needs. dryRun counts without mutating; see store.PurgeEvent for the per-tier
// hard/soft semantics and its two documented limitations (graph and experience
// tiers untouched).
type MemoryPurger interface {
	PurgeEvent(ctx context.Context, tenantID, eventID string, dryRun bool) (store.PurgeCounts, error)
}

// MemoryPurge implements engrampb.EngramServer: it erases the named events
// from the caller's own tenant and reports per-tier totals.
//
// The tenant comes exclusively from the verified Identity the auth barricade
// injected — MemoryPurgeRequest carries no tenant field, so a token can never
// reach another tenant's data even by lying. Authorization runs before
// argument validation (knowledge.go's ordering), so an unauthorized caller
// gets an opaque PermissionDenied and no oracle for whether its ids were even
// well-formed.
//
// Purging an event that does not exist is success with zero counts, not
// NOT_FOUND: purge is idempotent by construction, and reporting absence would
// hand a caller an existence oracle over ids it may never have written.
func (s *Server) MemoryPurge(ctx context.Context, req *engrampb.MemoryPurgeRequest) (*engrampb.MemoryPurgeResponse, error) {
	if s.Purger == nil {
		// Mirrors the Auditor precedent: an unwired seam answers UNIMPLEMENTED
		// rather than panicking on a nil interface.
		return nil, status.Error(codes.Unimplemented, "memory purge is not configured")
	}
	id, _ := authgrpc.IdentityFrom(ctx)
	if s.KnowledgeAuth.AuthorizeWrite(id, RoleMemoryAdmin) != nil {
		// knowledgeauth fails closed on an absent/invalid identity, so this
		// also covers the "interceptor not wired" case. Opaque by design: no
		// role oracle.
		return nil, status.Error(codes.PermissionDenied, "not authorized to purge memory")
	}
	eventIDs, err := validatePurgeEventIDs(req.GetEventIds())
	if err != nil {
		return nil, err
	}

	resp := &engrampb.MemoryPurgeResponse{DryRun: req.GetDryRun()}
	for _, eventID := range eventIDs {
		counts, err := s.Purger.PurgeEvent(ctx, id.TenantID, eventID, req.GetDryRun())
		// Counts are accumulated BEFORE the error check: a mid-list failure
		// has already mutated the tiers for the events it got through, and the
		// operator needs those numbers to know where the run stopped. They are
		// logged below even on the error path, since the gRPC error carries no
		// response body.
		resp.Episodic += counts.Episodic
		resp.Ledger += counts.Ledger
		resp.Semantic += counts.Semantic
		if err != nil {
			logPurge(ctx, id.TenantID, eventIDs, resp, err)
			return nil, status.Errorf(codes.Internal, "purging event %q: %v", eventID, err)
		}
	}
	logPurge(ctx, id.TenantID, eventIDs, resp, nil)
	return resp, nil
}

// validatePurgeEventIDs enforces the request contract at the barricade: at
// least one id, at most maxPurgeEventIDs, and no blank entry. A blank id is
// rejected rather than skipped (the same refusal internal/server/knowledge.go
// applies to an empty doc id) — it almost always means an unset variable in
// the caller's argument list, and silently dropping it would let a purge
// report success while missing the row the operator meant.
//
// Surviving ids pass through VERBATIM, never trimmed: event_id is an opaque
// client-supplied string, so " ev-1 " and "ev-1" are different events, and
// normalizing here would quietly purge one while the caller named the other.
// TrimSpace is used only to detect blankness, never to rewrite a value.
func validatePurgeEventIDs(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, status.Error(codes.InvalidArgument, "event_ids must name at least one event")
	}
	if len(raw) > maxPurgeEventIDs {
		return nil, status.Errorf(codes.InvalidArgument, "event_ids carries %d ids; at most %d may be purged per call", len(raw), maxPurgeEventIDs)
	}
	for i, eventID := range raw {
		if strings.TrimSpace(eventID) == "" {
			return nil, status.Errorf(codes.InvalidArgument, "event_ids[%d] is empty", i)
		}
	}
	return raw, nil
}

// logPurge writes the single structured audit line every purge leaves behind,
// success or failure. A purge is the only operation in the system that
// destroys memory-tier data, so the record of who erased what must not depend
// on the caller keeping the response.
func logPurge(ctx context.Context, tenantID string, eventIDs []string, resp *engrampb.MemoryPurgeResponse, err error) {
	args := []any{
		"tenant_id", tenantID,
		"event_ids", eventIDs,
		"dry_run", resp.GetDryRun(),
		"episodic_deleted", resp.GetEpisodic(),
		"ledger_deleted", resp.GetLedger(),
		"semantic_expired", resp.GetSemantic(),
	}
	if err != nil {
		slog.ErrorContext(ctx, "memory purge failed", append(args, "error", err)...)
		return
	}
	slog.InfoContext(ctx, "memory purge", args...)
}
