package graph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ryanthedev/engram/internal/embed"
)

// ErrNotFound is returned when a requested entity does not exist.
var ErrNotFound = errors.New("graph: not found")

// Backend is the persistence port Store depends on: entities and edges
// behind a narrow interface, mirroring internal/experience's Backend split.
// MemBackend backs unit tests; OpenSearchBackend backs production and the
// e2e/integration tiers (dependency inversion — the arrow points from
// infrastructure to this package).
type Backend interface {
	// CandidateEntities returns live entities that might match name for
	// tenant — a bounded, possibly-fuzzy lookup (NOT an exact-match
	// guarantee: it may return zero, one, or several entities, including
	// homonyms sharing a normalized name). Deduper.Decide adjudicates the
	// result; the backend's job is recall, not precision.
	CandidateEntities(ctx context.Context, tenantID, name string) ([]Entity, error)
	// PutEntity upserts e keyed by its ID.
	PutEntity(ctx context.Context, e Entity) error
	// GetEntity fetches one entity by id.
	GetEntity(ctx context.Context, tenantID, id string) (Entity, bool, error)
	// CountEntities returns the number of LIVE entities for tenant — the
	// entity-count-stability metric (DW-6.3).
	CountEntities(ctx context.Context, tenantID string) (int, error)

	// PutEdge upserts e keyed by its ID.
	PutEdge(ctx context.Context, e Edge) error
	// GetEdge fetches one edge by id (the UpsertEdge idempotency read).
	GetEdge(ctx context.Context, tenantID, id string) (Edge, bool, error)
	// Neighbors returns every LIVE edge touching entityID in either
	// direction (the traversal primitive GraphExpander's BFS uses).
	Neighbors(ctx context.Context, tenantID, entityID string) ([]Edge, error)
}

// Store is the deep module callers use: it hides the candidate-lookup +
// dedup-decide + upsert-or-merge choreography behind two intent methods
// (UpsertMention, UpsertEdge) plus the read paths GraphExpander and the
// decision-gate metric need.
type Store struct {
	backend  Backend
	dedup    Deduper
	embedder embed.Embedder // optional: nil degrades dedup to lexical-only
	logger   *slog.Logger
	now      func() time.Time
	// nameKeyedDedup switches the dedup-embedding INPUT from the fact
	// context to the mention's normalized name alone. See
	// WithNameKeyedDedup — production wiring must never set this.
	nameKeyedDedup bool
}

// StoreOption configures optional Store behavior beyond the required
// constructor arguments (mirrors ExpanderOption/BackendOption's convention
// in this package).
type StoreOption func(*Store)

// WithNameKeyedDedup makes Store embed each mention's normalized NAME alone
// for the dedup-similarity signal, instead of the fact's full context.
//
// Why this exists: the deterministic dev/e2e embedder (embed.FakeEmbedder)
// is a literal hash of its input string — sha256(text) seeds a PRNG that
// produces a unit vector. There is no partial or fuzzy similarity for a hash
// function: two input strings that differ by even one token hash to
// effectively uncorrelated vectors; only byte-identical input reliably
// yields equal vectors. The production default (embedding the fact's full
// context) means the SAME entity mentioned in two different facts always
// gets two uncorrelated vectors under FakeEmbedder, indistinguishable from
// a true homonym — Deduper.Decide (correctly, and unchanged by this option)
// never merges them, so the local/dev stack cannot demonstrate multi-hop
// connect-the-dots at all. Embedding the normalized name alone makes every
// mention of the same entity hash identically, so they cluster and merge
// through Decide's EXISTING weighted rule — Decide itself is not touched.
//
// This is a deliberate, DOCUMENTED trade, not a free lunch: with this
// option enabled, the dev embedder also merges real homonyms (same name,
// different real-world entity), because it no longer sees ANY
// disambiguating context. That is an ACCEPTED dev-fixture limitation, not a
// production regression — homonym separation is fundamentally a
// real-semantic-embedder property (a real embedder given full context
// distinguishes "Jordan the country" from "Jordan the athlete" by meaning;
// a hash function given only "jordan" cannot distinguish anything). callers
// MUST only enable this for a deterministic/fake embedder; production
// wiring (a real embedder) must leave it off so real-embedder homonym
// separation is preserved exactly as before.
func WithNameKeyedDedup() StoreOption {
	return func(s *Store) { s.nameKeyedDedup = true }
}

// NewStore returns a Store over backend and dedup. embedder may be nil
// (dedup then relies on the lexical signal alone — degraded, not fatal,
// matching the worker's optional-embedder convention). logger nil uses
// slog.Default(). opts is empty in production; WithNameKeyedDedup is the
// dev/e2e-only exception (see its doc comment).
func NewStore(backend Backend, dedup Deduper, embedder embed.Embedder, logger *slog.Logger, opts ...StoreOption) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Store{backend: backend, dedup: dedup, embedder: embedder, logger: logger, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// UpsertMention resolves m to a canonical entity: fetch name-key candidates,
// run the ONE dedup decision, then either merge into the winning candidate
// (bump MentionCount, union aliases, extend SourceIDs, never touching
// unrelated entities) or create a new entity. It is idempotent under
// at-least-once replay: a repeated mention with identical Context always
// re-finds and re-merges into the same entity rather than growing the count
// (DW-6.1 / DW-6.3).
func (s *Store) UpsertMention(ctx context.Context, m Mention) (entityID string, dec Decision, err error) {
	if m.TenantID == "" || strings.TrimSpace(m.Name) == "" {
		return "", Decision{}, fmt.Errorf("graph: mention requires a tenant and a non-empty name")
	}
	vec := s.embed(ctx, m)

	existing, err := s.backend.CandidateEntities(ctx, m.TenantID, m.Name)
	if err != nil {
		return "", Decision{}, fmt.Errorf("graph: fetching candidates for %q: %w", m.Name, err)
	}
	candidates := make([]Candidate, 0, len(existing))
	byID := make(map[string]Entity, len(existing))
	for _, e := range existing {
		if !e.Live() {
			continue
		}
		// (tenant, scope) merge boundary (plan-review finding): a
		// same-normalized-name entity in a DIFFERENT scope (e.g. a
		// private note's "Acme" vs a team's "Acme") is never a merge
		// candidate for this mention. Enforced HERE, before Decide
		// ever sees the candidate set, so Decide's signature/contract
		// (Candidate carries no Scope) stays untouched — Decide simply
		// never learns the scope-excluded candidates existed.
		if e.Scope != m.Scope {
			continue
		}
		byID[e.ID] = e
		candidates = append(candidates, Candidate{ID: e.ID, Name: e.Name, Aliases: e.Aliases, Embedding: e.Embedding})
	}

	dec, err = s.dedup.Decide(ctx, Candidate{Name: m.Name, Context: m.Context, Embedding: vec}, candidates)
	if err != nil {
		return "", Decision{}, fmt.Errorf("graph: dedup decision for %q: %w", m.Name, err)
	}
	s.logger.InfoContext(ctx, "graph dedup decision", "name", m.Name, "merge", dec.Merge,
		"match_id", dec.MatchID, "combined", dec.Combined, "embed_sim", dec.EmbedSim, "lex_sim", dec.LexSim,
		"used_judge", dec.UsedJudge, "reason", dec.Reason)

	if dec.Merge {
		matched := byID[dec.MatchID]
		merged := mergeEntity(matched, m)
		if err := s.backend.PutEntity(ctx, merged); err != nil {
			return "", dec, fmt.Errorf("graph: merging entity %s: %w", matched.ID, err)
		}
		return matched.ID, dec, nil
	}

	now := s.now().UTC()
	e := Entity{
		ID:           newEntityID(m.TenantID, Fingerprint(m.TenantID, m.Name), m.SourceID),
		NameKey:      Fingerprint(m.TenantID, m.Name),
		TenantID:     m.TenantID,
		TeamID:       m.TeamID,
		Scope:        m.Scope,
		OwnerAgentID: m.OwnerAgentID,
		Name:         m.Name,
		Embedding:    vec,
		SourceIDs:    []string{m.SourceID},
		MentionCount: 1,
		ValidAt:      now,
		CreatedAt:    now,
	}
	if err := s.backend.PutEntity(ctx, e); err != nil {
		return "", dec, fmt.Errorf("graph: creating entity %q: %w", m.Name, err)
	}
	return e.ID, dec, nil
}

// mergeEntity folds a new mention into an already-resolved entity (the
// LiCoMemory hyperlink-not-duplicate pattern: the mention becomes an alias,
// nothing is deleted or physically combined). The canonical Name, ID,
// CreatedAt, and Embedding are preserved; only accounting fields grow.
func mergeEntity(existing Entity, m Mention) Entity {
	merged := existing
	merged.MentionCount++
	if !strings.EqualFold(existing.Name, m.Name) && !containsFold(existing.Aliases, m.Name) {
		merged.Aliases = append(append([]string{}, existing.Aliases...), m.Name)
	}
	if !containsStr(existing.SourceIDs, m.SourceID) {
		merged.SourceIDs = append(append([]string{}, existing.SourceIDs...), m.SourceID)
	}
	return merged
}

// UpsertEdge upserts the relation described by spec (From -[Predicate]-> To).
// Idempotent: the same (tenant, from, predicate, to) always lands the same
// doc, so re-ingesting an identical fact is a no-op on the edge count
// (DW-6.1/6.3).
func (s *Store) UpsertEdge(ctx context.Context, spec EdgeSpec) (string, error) {
	id := edgeFingerprint(spec.TenantID, spec.FromEntityID, spec.Predicate, spec.ToEntityID)
	existing, ok, err := s.backend.GetEdge(ctx, spec.TenantID, id)
	if err != nil {
		return "", fmt.Errorf("graph: reading existing edge %s: %w", id, err)
	}
	now := s.now().UTC()
	e := Edge{
		ID: id, TenantID: spec.TenantID, TeamID: spec.TeamID, Scope: spec.Scope, OwnerAgentID: spec.OwnerAgentID,
		FromEntityID: spec.FromEntityID, ToEntityID: spec.ToEntityID, Predicate: spec.Predicate, Statement: spec.Statement,
		SourceIDs: []string{spec.SourceID}, ValidAt: spec.ValidAt, CreatedAt: now,
	}
	if ok {
		e.CreatedAt = existing.CreatedAt
		e.ValidAt = existing.ValidAt
		if !containsStr(existing.SourceIDs, spec.SourceID) {
			e.SourceIDs = append(append([]string{}, existing.SourceIDs...), spec.SourceID)
		} else {
			e.SourceIDs = existing.SourceIDs
		}
	}
	if err := s.backend.PutEdge(ctx, e); err != nil {
		return "", fmt.Errorf("graph: upserting edge %s: %w", id, err)
	}
	return id, nil
}

// GetEntity fetches one entity by id.
func (s *Store) GetEntity(ctx context.Context, tenantID, id string) (Entity, bool, error) {
	return s.backend.GetEntity(ctx, tenantID, id)
}

// Neighbors returns every live edge touching entityID.
func (s *Store) Neighbors(ctx context.Context, tenantID, entityID string) ([]Edge, error) {
	return s.backend.Neighbors(ctx, tenantID, entityID)
}

// CountEntities returns the number of live entities for tenant.
func (s *Store) CountEntities(ctx context.Context, tenantID string) (int, error) {
	return s.backend.CountEntities(ctx, tenantID)
}

// embed returns the dedup-similarity embedding for m: the fact's full
// context by default (production — preserves real-embedder homonym
// separation via context), or m's normalized name alone when
// nameKeyedDedup is enabled (dev/e2e only — see WithNameKeyedDedup). Returns
// nil if no embedder is configured or embedding fails (degraded, not
// fatal — dedup falls back to the lexical signal alone).
func (s *Store) embed(ctx context.Context, m Mention) []float32 {
	text := m.Context
	if s.nameKeyedDedup {
		text = normalizeName(m.Name)
	}
	if s.embedder == nil || strings.TrimSpace(text) == "" {
		return nil
	}
	vecs, err := s.embedder.Embed(ctx, []string{text})
	if err != nil || len(vecs) == 0 {
		s.logger.WarnContext(ctx, "graph: mention embedding failed; dedup degraded to lexical-only", "err", err)
		return nil
	}
	return vecs[0]
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func containsFold(xs []string, want string) bool {
	for _, x := range xs {
		if strings.EqualFold(x, want) {
			return true
		}
	}
	return false
}

// MemBackend is the in-memory Backend for unit tests. Safe for concurrent
// use. CandidateEntities does token-overlap prefiltering (bounded) rather
// than an exact NameKey match, so tests exercise the same "may return
// several, possibly-homonymous candidates" contract the OpenSearch backend
// provides.
type MemBackend struct {
	mu       sync.Mutex
	entities map[string]Entity // key: id
	edges    map[string]Edge   // key: id
}

var _ Backend = (*MemBackend)(nil)

// NewMemBackend returns an empty in-memory backend.
func NewMemBackend() *MemBackend {
	return &MemBackend{entities: map[string]Entity{}, edges: map[string]Edge{}}
}

// CandidateEntities returns live, tenant-scoped entities whose NameKey
// matches OR whose name/alias tokens overlap the query name — a superset a
// real fuzzy-match query would also return, bounded to 20.
func (m *MemBackend) CandidateEntities(_ context.Context, tenantID, name string) ([]Entity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := Fingerprint(tenantID, name)
	var out []Entity
	for _, e := range m.entities {
		if e.TenantID != tenantID || !e.Live() {
			continue
		}
		if e.NameKey == key || tokenOverlapRatio(name, e.Name) > 0 {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > 20 {
		out = out[:20]
	}
	return out, nil
}

// PutEntity implements Backend.
func (m *MemBackend) PutEntity(_ context.Context, e Entity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entities[e.ID] = e
	return nil
}

// GetEntity implements Backend.
func (m *MemBackend) GetEntity(_ context.Context, tenantID, id string) (Entity, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entities[id]
	if !ok || e.TenantID != tenantID {
		return Entity{}, false, nil
	}
	return e, true, nil
}

// CountEntities implements Backend.
func (m *MemBackend) CountEntities(_ context.Context, tenantID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, e := range m.entities {
		if e.TenantID == tenantID && e.Live() {
			n++
		}
	}
	return n, nil
}

// PutEdge implements Backend.
func (m *MemBackend) PutEdge(_ context.Context, e Edge) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.edges[e.ID] = e
	return nil
}

// GetEdge implements Backend.
func (m *MemBackend) GetEdge(_ context.Context, tenantID, id string) (Edge, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.edges[id]
	if !ok || e.TenantID != tenantID {
		return Edge{}, false, nil
	}
	return e, true, nil
}

// Neighbors implements Backend: every live edge touching entityID, either
// direction.
func (m *MemBackend) Neighbors(_ context.Context, tenantID, entityID string) ([]Edge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Edge
	for _, e := range m.edges {
		if e.TenantID != tenantID || !e.Live() {
			continue
		}
		if e.FromEntityID == entityID || e.ToEntityID == entityID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
