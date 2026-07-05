// Package graph is Engram's incremental knowledge graph (T4): per-episode
// entity/edge upsert with dedup, and bounded (≤2-hop) "connect the dots"
// expansion at query time (D2: incremental, never a batch recompute; D8: an
// OpenSearch-edge representation is sufficient at ≤2 hops — a graph-native
// store is this phase's decision-gate OUTPUT, not an input).
//
// Shape (mirrors internal/experience's split): Store is the deep module
// callers use (UpsertMention, UpsertEdge, CountEntities, Neighbors); Backend
// is the persistence port (MemBackend for tests, OpenSearchBackend for
// production); Deduper is the one dedup decision routine, combining
// embedding similarity, a BM25-style lexical score, and — only when those
// disagree — a Judge tie-break (RuleJudge deterministic, HTTPJudge an
// OpenAI-compatible LLM, config-selected like experience.Gatekeeper).
// GraphStage is the worker.Stage that derives entity mentions from landed
// facts' Subject/Predicate/Object triples (no internal/ingest edit: the
// extraction stage already emits these, per the dispatch's environment
// note). GraphExpander is the retrieval.PostHook that performs the bounded
// traversal, registered from cmd/engram-server/stages_graph.go.
//
// Architecture boundary: this package imports internal/{auth, memory, embed,
// retrieval, worker, store-independent HTTP helpers} as needed, but no
// transport/framework — the depguard boundary holds structurally, matching
// internal/experience.
package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// MaxDepth is the hard ceiling on expansion depth (D8: ≤2-hop on OpenSearch
// edges; >2-hop traversal is explicitly OUT for this phase). GraphExpander
// and Expand both refuse a depth beyond this rather than silently clamping —
// a misconfigured depth should fail loudly, not degrade quietly.
const MaxDepth = 2

// Entity is one deduplicated real-world entity in the graph: a canonical
// name plus every merged surface form (LiCoMemory hyperlink-not-duplicate —
// a "duplicate" mention becomes an alias of the canonical entity rather than
// being deleted or physically merged, so the merge is reversible). Entities
// are provenance-bearing (D6/D16) so expansion hits carry real ACL fields,
// and bi-temporal (D3): ExpiredAt is set only by an explicit soft-expire,
// never by a merge or a delete.
type Entity struct {
	// ID is the canonical doc id, deterministic per FIRST-SEEN mention:
	// sha256(tenant · NameKey · first source event id). Deliberately NOT a
	// pure function of the name alone — the same normalized name may
	// legitimately resolve to several distinct entities (the homonym edge
	// case), each needing its own id; the dedup routine (not id collision)
	// is what decides whether a later mention joins this entity or creates
	// another one under the same NameKey.
	ID string `json:"id"`
	// NameKey = Fingerprint(tenant, name) — the indexed candidate-lookup
	// key. CandidateEntities queries by this key and may return MULTIPLE
	// entities (homonyms); it is never trusted as a merge decision by
	// itself.
	NameKey string `json:"name_key"`

	TenantID     string `json:"tenant_id"`
	TeamID       string `json:"team_id,omitempty"`
	Scope        string `json:"scope"`
	OwnerAgentID string `json:"owner_agent_id"`

	// Name is the canonical display name (the first-seen surface form).
	Name string `json:"name"`
	// Aliases are every OTHER surface form merged into this entity (dedup
	// accounting; never overwrites Name).
	Aliases []string `json:"aliases,omitempty"`
	// Embedding is the context embedding of the first-seen mention — the
	// dedup routine's similarity signal for later mentions of this entity.
	Embedding []float32 `json:"embedding,omitempty"`
	// SourceIDs carries every episodic event id that contributed a mention
	// (provenance).
	SourceIDs []string `json:"source_ids,omitempty"`
	// MentionCount counts every mention that resolved (merged) to this
	// entity, including the first — the dedup/entity-count-stability signal.
	MentionCount int `json:"mention_count"`

	ValidAt   time.Time  `json:"valid_at"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiredAt *time.Time `json:"expired_at,omitempty"`
}

// Live reports whether the entity is currently traversable (not soft-expired).
func (e Entity) Live() bool { return e.ExpiredAt == nil }

// Edge is one relation between two entities, derived from a fact's
// (Subject, Predicate, Object) triple. Bi-temporal per D3: InvalidAt closes
// a superseded relation, ExpiredAt soft-expires it; neither is ever a hard
// delete. Provenance/scope are carried per-edge (not inherited from either
// endpoint entity) so ACL re-verification at expansion time is exact.
type Edge struct {
	// ID is deterministic: sha256(tenant · from · predicate · to) — an
	// idempotent upsert, so repeated ingestion of the identical fact never
	// grows the edge count.
	ID string `json:"id"`

	TenantID     string `json:"tenant_id"`
	TeamID       string `json:"team_id,omitempty"`
	Scope        string `json:"scope"`
	OwnerAgentID string `json:"owner_agent_id"`

	FromEntityID string `json:"from_entity_id"`
	ToEntityID   string `json:"to_entity_id"`
	Predicate    string `json:"predicate"`
	// Statement is the natural-language rendering (from the source fact) —
	// the BM25 target for an expansion hit's display text.
	Statement string   `json:"statement"`
	SourceIDs []string `json:"source_ids,omitempty"`

	ValidAt   time.Time  `json:"valid_at"`
	InvalidAt *time.Time `json:"invalid_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiredAt *time.Time `json:"expired_at,omitempty"`
}

// Live reports whether the edge is currently traversable (not closed and not
// soft-expired).
func (e Edge) Live() bool { return e.InvalidAt == nil && e.ExpiredAt == nil }

// Mention is one candidate entity reference extracted from a landed fact's
// Subject or Object — the input to the dedup decision and the upsert.
type Mention struct {
	TenantID     string
	TeamID       string
	Scope        string
	OwnerAgentID string
	// Name is the raw surface form (the fact's Subject or Object text).
	Name string
	// Context is the disambiguating material for the dedup embedding — the
	// fact's full Statement, not just Name. Two mentions with an identical
	// Name but unrelated Context (the same-name-different-entity edge case)
	// embed far apart even though their lexical score is a perfect match.
	// (Store.WithNameKeyedDedup overrides this for the deterministic
	// dev/e2e embedder only — see its doc comment; production always
	// embeds Context as described here.)
	Context  string
	SourceID string
}

// EdgeSpec is the input to Store.UpsertEdge — the relation to upsert plus the
// provenance/scope fields it carries. It is the edge-side counterpart of
// Mention (a parameter object, not a bag of positional args), keeping the
// upsert call site readable and the provenance fields named at the boundary.
// The doc id, timestamps, and merge accounting are computed inside UpsertEdge,
// never supplied here.
type EdgeSpec struct {
	TenantID     string
	TeamID       string
	Scope        string
	OwnerAgentID string
	// FromEntityID / ToEntityID are the resolved (deduplicated) entity ids
	// the edge connects; Predicate names the relation.
	FromEntityID string
	ToEntityID   string
	Predicate    string
	// Statement is the natural-language rendering (from the source fact).
	Statement string
	SourceID  string
	ValidAt   time.Time
}

// normalizeName lowercases and collapses whitespace so trivially different
// surface forms of one name key identically (mirrors experience.normalizeTask).
func normalizeName(name string) string {
	return strings.Join(strings.Fields(strings.ToLower(name)), " ")
}

// Fingerprint returns the deterministic candidate-lookup key for a mention's
// name under a tenant: the input to CandidateEntities, NOT a trusted merge
// decision by itself (multiple distinct entities may legitimately share one
// fingerprint — the homonym case — so Deduper.Decide always adjudicates the
// candidates this key returns rather than short-circuiting on the key alone).
func Fingerprint(tenantID, name string) string {
	sum := sha256.Sum256([]byte(tenantID + "\x1f" + normalizeName(name)))
	return hex.EncodeToString(sum[:])
}

// edgeFingerprint returns the deterministic edge doc id: identical
// (tenant, from, predicate, to) always upserts the same doc, so repeated
// ingestion of the same fact never grows the edge count.
func edgeFingerprint(tenantID, fromID, predicate, toID string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{tenantID, fromID, predicate, toID}, "\x1f")))
	return hex.EncodeToString(sum[:])
}

// newEntityID returns the deterministic id for a brand-new entity, derived
// from its NameKey plus the very first mention's source event id (so two
// independently-created homonym entities under the same NameKey never
// collide, while a crash-replay of the SAME creating event reproduces the
// same id).
func newEntityID(tenantID, nameKey, firstSourceID string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{tenantID, nameKey, firstSourceID}, "\x1f")))
	return hex.EncodeToString(sum[:])
}
