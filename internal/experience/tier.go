package experience

import (
	"context"
	"log/slog"

	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/retrieval"
)

// Tier exposes ADMITTED experiences as a gated retrieval tier
// (retrieval.TierSource), registered via the Phase-4 RegisterTier seam from
// cmd/engram-server/stages_experience.go. It searches only the admitted,
// non-soft-expired index — quarantined and pruned experiences are never
// reachable through it. Every hit carries the experience's provenance/scope
// fields, so the MultiRetriever re-verifies each one through the ACL predicate
// (defense in depth): the tier never widens what a caller may read.
type Tier struct {
	store  *Store
	logger *slog.Logger
}

var _ retrieval.TierSource = (*Tier)(nil)

// NewTier returns a tier over the gated store. logger nil uses
// slog.Default().
func NewTier(store *Store, logger *slog.Logger) *Tier {
	if logger == nil {
		logger = slog.Default()
	}
	return &Tier{store: store, logger: logger}
}

// Search implements retrieval.TierSource. It scopes results to the caller's
// tenant, returns admitted experiences as hits, and best-effort increments each
// returned experience's retrieval count (the prune / following-correlation
// signal). The MultiRetriever re-checks every hit through the ACL, so returning
// a hit here is never itself an authorization decision.
func (t *Tier) Search(ctx context.Context, id auth.Identity, q retrieval.Query) ([]retrieval.Hit, error) {
	if id.TenantID == "" {
		return nil, nil // no tenant, nothing to search (fail-closed)
	}
	exps, err := t.store.SearchAdmitted(ctx, id.TenantID, q.Text, q.K)
	if err != nil {
		return nil, err
	}
	hits := make([]retrieval.Hit, 0, len(exps))
	for _, exp := range exps {
		t.store.recordRetrieval(ctx, exp)
		hits = append(hits, retrieval.Hit{
			ID:     exp.Fingerprint,
			Score:  exp.Utility, // utility as the tier's relevance proxy
			Source: "experience",
			Fields: map[string]any{
				// Provenance/scope — required so filterAuthorized re-verifies.
				"tenant_id":      exp.TenantID,
				"team_id":        exp.TeamID,
				"scope":          exp.Scope,
				"owner_agent_id": exp.OwnerAgentID,
				// Content.
				"statement":       exp.DistilledSkill,
				"distilled_skill": exp.DistilledSkill,
				"task":            exp.Task,
				"utility":         exp.Utility,
			},
		})
	}
	return hits, nil
}

// retrievalBumper is the optional Backend capability the tier uses to feed the
// retrieval-count signal. Backends that implement it get retrieval accounting;
// those that don't degrade gracefully (no accounting, never an error).
type retrievalBumper interface {
	BumpRetrieval(ctx context.Context, tenantID, fingerprint string) error
}

// recordRetrieval bumps an experience's retrieval count best-effort; a failure
// is logged, never fatal to the query (a search must not fail because an
// accounting write did).
func (s *Store) recordRetrieval(ctx context.Context, exp Experience) {
	b, ok := s.backend.(retrievalBumper)
	if !ok {
		return
	}
	if err := b.BumpRetrieval(ctx, exp.TenantID, exp.Fingerprint); err != nil {
		s.logger.WarnContext(ctx, "experience retrieval-count bump failed", "fingerprint", exp.Fingerprint, "err", err)
	}
}
