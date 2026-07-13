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
	// CountAllEntities returns the number of LIVE entities across ALL
	// tenants — Phase 3's durable graph telemetry signal (the all-tenant
	// DW-6.3 stability signal wired onto /metrics), distinct from
	// CountEntities' per-tenant scope.
	CountAllEntities(ctx context.Context) (int, error)

	// PutEdge upserts e keyed by its ID.
	PutEdge(ctx context.Context, e Edge) error
	// GetEdge fetches one edge by id (the UpsertEdge idempotency read).
	GetEdge(ctx context.Context, tenantID, id string) (Edge, bool, error)
	// Neighbors returns every LIVE edge touching entityID in either
	// direction (the traversal primitive GraphExpander's BFS uses).
	Neighbors(ctx context.Context, tenantID, entityID string) ([]Edge, error)

	// ScanEntities returns one page (ascending id order) of LIVE entities
	// for tenantID, resuming after cursor. A zero Cursor starts at the
	// beginning; a zero returned next Cursor means the tier is exhausted —
	// this page was the last one. Unlike CandidateEntities/Neighbors, Scan
	// never silently truncates: repeated calls with the advancing cursor
	// eventually visit every live entity in tenantID exactly once. An
	// empty or not-yet-created index returns an empty page and a zero
	// cursor, never an error.
	ScanEntities(ctx context.Context, tenantID string, cursor Cursor) (items []Entity, next Cursor, err error)
	// ScanEdges is ScanEntities' edge-tier counterpart: one page of LIVE
	// edges (InvalidAt==nil && ExpiredAt==nil) for tenantID, same cursor
	// contract.
	ScanEdges(ctx context.Context, tenantID string, cursor Cursor) (items []Edge, next Cursor, err error)
}

// Cursor is an opaque pagination token returned by Scan{Entities,Edges} and
// passed back in to resume. The zero value means "start from the
// beginning" when passed in, and "no more pages" when returned as next —
// callers never construct or inspect a non-zero Cursor themselves, they
// only round-trip the value a previous Scan call handed back. Shared by
// both entity and edge scans because both tiers sort on the same kind of
// field: a deterministic, non-empty id (see graph.go's newEntityID /
// edgeFingerprint) that alone is sufficient as a total-order tie-break —
// no realistic need for a multi-field sort key has been identified.
type Cursor struct {
	// after is the last id seen on the previous page (ascending order);
	// the next page's query resumes strictly after this id. Entity/Edge
	// ids are always non-empty sha256 hex, so "" unambiguously means "no
	// cursor" and can never collide with a real id.
	after string
}

// MarshalText round-trips the cursor across a process boundary (the export
// RPC's wire cursor) without exposing its internals as API: callers treat
// the bytes as opaque and only feed them back to UnmarshalText. The zero
// Cursor marshals to empty text.
func (c Cursor) MarshalText() ([]byte, error) { return []byte(c.after), nil }

// UnmarshalText restores a cursor previously produced by MarshalText. Any
// byte string is accepted by design: a stale or fabricated value merely
// repositions the scan within whatever tenant the CALLER passes to
// Scan{Entities,Edges} — the cursor carries no tenancy, so it can never
// widen a scan's scope, only move along the id order inside it.
func (c *Cursor) UnmarshalText(b []byte) error {
	c.after = string(b)
	return nil
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
	r, err := s.resolveMention(ctx, m)
	if err != nil {
		return "", Decision{}, err
	}
	if r.dec.Merge {
		merged := mergeEntity(r.matched, m)
		if err := s.backend.PutEntity(ctx, merged); err != nil {
			return "", r.dec, fmt.Errorf("graph: merging entity %s: %w", r.matched.ID, err)
		}
		return r.matched.ID, r.dec, nil
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
		Embedding:    r.vec,
		SourceIDs:    []string{m.SourceID},
		MentionCount: 1,
		ValidAt:      now,
		CreatedAt:    now,
	}
	if err := s.backend.PutEntity(ctx, e); err != nil {
		return "", r.dec, fmt.Errorf("graph: creating entity %q: %w", m.Name, err)
	}
	return e.ID, r.dec, nil
}

// resolution is the outcome of the READ-only half of an upsert: the one dedup
// decision, the entity it matched (zero-valued unless dec.Merge), and the
// mention's embedding (carried so the create path never re-embeds).
type resolution struct {
	dec     Decision
	matched Entity
	vec     []float32
}

// resolveMention performs candidate lookup, the (tenant, scope) merge
// boundary, and the ONE dedup decision — and writes nothing. It is the shared
// read half of UpsertMention (which then merges or creates) and of
// resolveEntityID (which only wants to RECOVER an entity a previous mention
// already resolved). Keeping it write-free is what lets Stage recompute a
// superseded predecessor's edge fingerprint without inflating that entity's
// MentionCount/SourceIDs, which would corrupt the entity-stability metric.
func (s *Store) resolveMention(ctx context.Context, m Mention) (resolution, error) {
	if m.TenantID == "" || strings.TrimSpace(m.Name) == "" {
		return resolution{}, fmt.Errorf("graph: mention requires a tenant and a non-empty name")
	}
	vec := s.embed(ctx, m)

	existing, err := s.backend.CandidateEntities(ctx, m.TenantID, m.Name)
	if err != nil {
		return resolution{}, fmt.Errorf("graph: fetching candidates for %q: %w", m.Name, err)
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

	dec, err := s.dedup.Decide(ctx, Candidate{Name: m.Name, Context: m.Context, Embedding: vec}, candidates)
	if err != nil {
		return resolution{}, fmt.Errorf("graph: dedup decision for %q: %w", m.Name, err)
	}
	s.logger.InfoContext(ctx, "graph dedup decision", "name", m.Name, "merge", dec.Merge,
		"match_id", dec.MatchID, "combined", dec.Combined, "embed_sim", dec.EmbedSim, "lex_sim", dec.LexSim,
		"used_judge", dec.UsedJudge, "reason", dec.Reason)

	return resolution{dec: dec, matched: byID[dec.MatchID], vec: vec}, nil
}

// resolveEntityID recovers the entity an ALREADY-UPSERTED mention resolved to,
// without creating or mutating anything: ok is false when the dedup routine
// would not merge m into any live entity — i.e. nothing in the graph
// corresponds to this mention (never graphed, or soft-expired since). Callers
// treat !ok as "there is nothing here to act on", never as an error.
//
// It relies on UpsertMention's documented idempotency (a repeated mention with
// an identical Context always re-finds the same entity): pass the SAME Name and
// Context the mention was originally upserted with, and Decide re-selects the
// same entity — embeddings are a deterministic function of the context text, so
// the similarity signal is identical to the one that landed it.
func (s *Store) resolveEntityID(ctx context.Context, m Mention) (string, bool, error) {
	r, err := s.resolveMention(ctx, m)
	if err != nil {
		return "", false, err
	}
	if !r.dec.Merge || r.matched.ID == "" {
		return "", false, nil
	}
	return r.matched.ID, true, nil
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
//
// An upsert NEVER blind-overwrites a close. Since an edge's doc id is a pure
// function of its triple, a re-asserted relation lands on the very doc a
// previous supersession may have closed — so the two bi-temporal stamps are
// carried forward deliberately, not rebuilt:
//
//   - A REPLAY (at-least-once redelivery of an event whose fact has since been
//     superseded) re-upserts the identical triple at the identical valid time.
//     Dropping InvalidAt here would silently RESURRECT the closed edge and hand
//     the zombie relation straight back to search — undoing the supersession
//     that closed it. Its close is preserved instead.
//   - A genuine RE-ASSERTION ("service-a owns billing-db" is retracted, then
//     asserted again later) arrives with a strictly NEWER valid time. The
//     relation is true again, so the edge is revived: InvalidAt cleared,
//     ValidAt advanced. This mirrors experience.Store's re-proven soft-expire
//     revival — a closed edge is retired, never tombstoned.
//
// Strictly-newer is the discriminator because it is exactly what separates the
// two: a replay can only ever carry the valid time already recorded.
// ExpiredAt is likewise preserved — a soft-expire is not undone by a re-mention.
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
		e.ExpiredAt = existing.ExpiredAt
		if spec.ValidAt.After(existing.ValidAt) {
			e.ValidAt, e.InvalidAt = spec.ValidAt, nil // re-asserted: revive
		} else {
			e.ValidAt, e.InvalidAt = existing.ValidAt, existing.InvalidAt // replay: never reopen
		}
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

// CloseEdge soft-closes the edge with edgeID: it stamps InvalidAt, retiring
// the relation from every traversal (Backend.Neighbors returns only live
// edges) while leaving the document intact and its history readable. Nothing
// is ever hard-deleted — the edge tier mirrors the semantic store's
// append-only, guarded-close discipline (D3).
//
// It is the counterpart of the reconciler closing a superseded semantic fact:
// Stage calls it when a fact's predecessor was superseded, so the
// predecessor's edge stops being served next to the correction that replaced
// it.
//
// Idempotent, by three distinct paths — replay must never error or
// double-close:
//   - an edgeID no edge exists under (the predecessor asserted no edge, or its
//     entity was soft-expired) is a silent no-op, not ErrNotFound;
//   - an already-closed edge keeps its ORIGINAL InvalidAt (the close is not
//     re-stamped, so a replay cannot drift the closing time forward);
//   - both skip the write entirely.
//
// InvalidAt is stamped from the store clock rather than from the superseding
// fact's valid time: the edge tier has no as-of-valid-time read path (InvalidAt
// is consumed only by Edge.Live), so a transaction-time close is sufficient and
// keeps this an intent-shaped call — the caller names the edge, not the clock.
func (s *Store) CloseEdge(ctx context.Context, tenantID, edgeID string) error {
	e, ok, err := s.backend.GetEdge(ctx, tenantID, edgeID)
	if err != nil {
		return fmt.Errorf("graph: reading edge %s to close: %w", edgeID, err)
	}
	if !ok || e.InvalidAt != nil {
		return nil
	}
	now := s.now().UTC()
	e.InvalidAt = &now
	if err := s.backend.PutEdge(ctx, e); err != nil {
		return fmt.Errorf("graph: closing edge %s: %w", edgeID, err)
	}
	s.logger.InfoContext(ctx, "graph edge closed", "edge_id", edgeID, "tenant_id", tenantID,
		"predicate", e.Predicate, "invalid_at", now)
	return nil
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

// CountAllEntities returns the number of live entities across all tenants —
// Phase 3's graph telemetry gauge's data source (the all-tenant DW-6.3
// stability signal rendered on /metrics).
func (s *Store) CountAllEntities(ctx context.Context) (int, error) {
	return s.backend.CountAllEntities(ctx)
}

// ScanEntities returns one page of LIVE entities for tenantID, resuming
// after cursor — see Backend.ScanEntities for the full pagination contract.
// This is the read foundation the export endpoint's full-graph walk builds
// on: unlike CandidateEntities/Neighbors, it never silently truncates a
// tier.
func (s *Store) ScanEntities(ctx context.Context, tenantID string, cursor Cursor) ([]Entity, Cursor, error) {
	return s.backend.ScanEntities(ctx, tenantID, cursor)
}

// ScanEdges is ScanEntities' edge-tier counterpart — see
// Backend.ScanEdges.
func (s *Store) ScanEdges(ctx context.Context, tenantID string, cursor Cursor) ([]Edge, Cursor, error) {
	return s.backend.ScanEdges(ctx, tenantID, cursor)
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

// scanBatchSize is the page size Scan{Entities,Edges} uses on both
// MemBackend and OpenSearchBackend. A var, not a const: in-package
// white-box tests shrink it to exercise real multi-page pagination without
// needing thousands of fixture records. No exported surface reaches it, so
// it cannot be misused outside this package.
var scanBatchSize = 500

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

// CountAllEntities implements Backend: live entities across every tenant.
func (m *MemBackend) CountAllEntities(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, e := range m.entities {
		if e.Live() {
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

// ScanEntities implements Backend: one page (ascending id order) of live,
// tenant-scoped entities, resuming after cursor.after. Mirrors
// OpenSearchBackend.ScanEntities' page-boundary contract (next is zero iff
// this page held fewer than scanBatchSize items) so MemBackend is a
// faithful stand-in for pagination behavior in unit tests.
func (m *MemBackend) ScanEntities(_ context.Context, tenantID string, cursor Cursor) ([]Entity, Cursor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []Entity
	for _, e := range m.entities {
		if e.TenantID == tenantID && e.Live() {
			all = append(all, e)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return scanPage(all, cursor, func(e Entity) string { return e.ID })
}

// ScanEdges implements Backend: one page of live, tenant-scoped edges,
// resuming after cursor.after. Same page-boundary contract as
// ScanEntities.
func (m *MemBackend) ScanEdges(_ context.Context, tenantID string, cursor Cursor) ([]Edge, Cursor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []Edge
	for _, e := range m.edges {
		if e.TenantID == tenantID && e.Live() {
			all = append(all, e)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return scanPage(all, cursor, func(e Edge) string { return e.ID })
}

// scanPage slices one scanBatchSize-bounded page out of a slice already
// sorted ascending by id, resuming strictly after cursor.after. Shared by
// MemBackend.ScanEntities/ScanEdges so the pagination arithmetic (binary
// search for the resume point) lives in exactly one place regardless of
// tier. Page-boundary next-cursor detection is delegated to
// nextScanCursor — deliberately, even though MemBackend could look ahead
// and know for certain whether start+scanBatchSize is the true end: using
// the SAME "full page ⇒ assume more, verify on the next call" rule
// OpenSearchBackend must use (it cannot look ahead) keeps MemBackend a
// faithful pagination stand-in rather than a backend that behaves more
// intelligently than production ever can.
func scanPage[T any](sorted []T, cursor Cursor, id func(T) string) ([]T, Cursor, error) {
	start := 0
	if cursor.after != "" {
		start = sort.Search(len(sorted), func(i int) bool { return id(sorted[i]) > cursor.after })
	}
	if start >= len(sorted) {
		return nil, Cursor{}, nil
	}
	end := start + scanBatchSize
	if end > len(sorted) {
		end = len(sorted)
	}
	page := append([]T{}, sorted[start:end]...)
	next := nextScanCursor(len(page), func() string { return id(page[len(page)-1]) })
	return page, next, nil
}
