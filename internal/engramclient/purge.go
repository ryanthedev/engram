// Memory-purge client method: the thin gRPC passthrough behind
// `engram purge`, mirroring KnowledgeDelete's shape in knowledge.go. It is
// pure translation — no client-side policy — because the server barricade is
// the authority on authorization, the id cap, and the per-tier semantics.

package engramclient

import (
	"context"

	"github.com/ryanthedev/engram/api/engrampb"
)

// PurgeResult reports what a MemoryPurge call removed, per memory tier. It is
// declared here (rather than in internal/mcp, which holds the DTOs shared with
// the MCP tool surface) precisely because purge has NO MCP tool: the type has
// exactly one consumer, the CLI.
type PurgeResult struct {
	// Episodic counts raw episodic docs HARD-deleted. It can exceed one per
	// event id — a retried Ingest appends a second doc under the same
	// event_id, and the purge takes every one of them.
	Episodic int64 `json:"episodic"`
	// Ledger counts extraction-ledger rows HARD-deleted, so a corrected
	// re-ingest of the same event_id re-extracts instead of short-circuiting.
	Ledger int64 `json:"ledger"`
	// Semantic counts facts SOFT-deleted (expired_at stamped). They vanish
	// from search immediately but remain visible to `engram audit`.
	Semantic int64 `json:"semantic"`
	// DryRun echoes the server: true means nothing was mutated.
	DryRun bool `json:"dry_run"`
}

// MemoryPurge erases the named events from the caller's tenant and reports
// the per-tier totals. The tenant is never sent: the server pins it from the
// bearer token. dryRun asks the server to count without mutating anything.
//
// A caller lacking the memory-admin role gets PermissionDenied (classify it
// with IsPermissionDenied); a server without purge wiring gets Unimplemented.
func (c *Client) MemoryPurge(ctx context.Context, eventIDs []string, dryRun bool) (PurgeResult, error) {
	resp, err := c.api.MemoryPurge(ctx, &engrampb.MemoryPurgeRequest{
		EventIds: eventIDs,
		DryRun:   dryRun,
	})
	if err != nil {
		return PurgeResult{}, err
	}
	return PurgeResult{
		Episodic: resp.GetEpisodic(),
		Ledger:   resp.GetLedger(),
		Semantic: resp.GetSemantic(),
		DryRun:   resp.GetDryRun(),
	}, nil
}
