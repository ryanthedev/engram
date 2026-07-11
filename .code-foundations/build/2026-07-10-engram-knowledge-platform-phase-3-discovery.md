# Discovery + Design: Phase 3 - Collection registry (meta-index + runtime provisioning)

## Files Found
- `internal/store/templates.go` — template consts + `//go:embed` blobs (mirror target, line 18 consts block)
- `internal/store/apply.go` — `Apply` steps table (line 56), `ensureIndex` idempotency pattern (line 93), `do` helper
- `internal/store/opensearch.go` — `doJSON` (line 220), `isIndexNotFound` (line 208), status→`ErrConflict` switch idiom
- `internal/store/store.go` — `store.ErrConflict` sentinel (line 20)
- `internal/store/templates/*.json` — ledger.json is the strict-mapping template exemplar (priority 100, 1 shard / 0 replicas)
- `internal/mcp/mcp.go` — Phase 1 MCP-facing seam types: `KnowledgeDoc`, `Predicate`, `SortKey`, `FieldSpec`, `CollectionSpec` (flat `Public`/`Roles`, no `Index`), `CollectionInfo`
- `internal/knowledgeauth/` — Phase 2 authorizer taking primitives (`public bool`, `requiredRoles []string`) — deliberately NOT knowledge types
- `go.mod` — `go.yaml.in/yaml/v2 v2.4.4` is already a **direct** dependency (YAML seed needs no new dep)
- `internal/importlint/importlint.go` — `DefaultConfig` bans grpc/engrampb in business packages (new `knowledge` package is additive, nothing to register)

## Current State
`internal/knowledge/` does not exist. No registry, no `knowledge-collections` template, no not-found sentinel anywhere in store. The store package's HTTP helpers (`do`, `doJSON`, `ensureIndex`, `indexExists`, `isIndexNotFound`) are unexported — any code reusing them must live in package `store`. Live dev cluster at localhost:9200 runs OpenSearch 3.1.0.

## Gaps
- Plan line 97 names `CollectionSpec{Name, Index, TextField, Mappings, Access}` — the `Index` field (the stable **alias** callers query, registry-assigned) is needed by P5's retriever, so it stays on the domain spec even though mcp's DTO deliberately omits it.
- `ensureIndex` takes no body, but data-index creation needs mappings + alias in the create body → extend `ensureIndex` with a `body []byte` param (existing callers pass nil). This *is* the plan's "reuse the ensureIndex idempotency pattern".
- No `ErrNotFound` sentinel exists → defined in `knowledge` (a `knowledge`-seam error; `store` importing `knowledge` is fine, the reverse would cycle).
- `templates_test.go` / `apply_test.go` are not in file scope → the knowledge-collections template contract test lives in `registry_test.go` instead.

## Assumption Verification (plan, MED confidence)
**"OpenSearch 3.1 `_aliases` swap is atomic enough for zero-downtime reindex" — CONFIRMED** on the live cluster (3.1.0, localhost:9200): 60 remove+add swaps in single `_aliases` actions blocks while a reader hammered `GET /<alias>/_count` → **866 reads, 0 failures**, all swaps acknowledged. The single-request actions block is atomic; no fallback needed.

## Cross-phase seam decision (recorded per dispatch instruction)
**Keep `internal/mcp`'s types as thin MCP-facing DTOs; `internal/knowledge` owns the domain types. Explicit documented mapping; P6 translates.** Rationale: house rule "seams are consumer-defined" — `mcp.Backend` + its types are the MCP consumer's seam and were frozen in Phase 1 (mcp.go is not in this phase's file scope). The two shapes differ on purpose:
| domain (`knowledge`) | mcp DTO | mapping (P6 wires) |
|---|---|---|
| `CollectionSpec.Access AccessPolicy{Public, Roles}` | flat `Public`, `Roles` | `Access.Public↔Public`, `Access.Roles↔Roles` |
| `CollectionSpec.Index` (stable alias, registry-assigned) | absent | never crosses the MCP seam (physical storage is registry-internal) |
| `Document{ID,Title,Text,SourceVersion,Fields}` | `KnowledgeDoc` (same fields) | 1:1 |
| `FieldSpec{Type,Filterable,Sortable}` | `mcp.FieldSpec` (same fields) | 1:1 |
Drift guard: the mapping is documented on `knowledge.CollectionSpec`'s doc comment naming `mcp.CollectionSpec`, and P6's translation layer is the single conversion point.

## Code Standards
Applied: raw net/http + `map[string]any` (no OS SDK), `doJSON`/`do` helpers, status→sentinel switch (`%w`-wrapped so `errors.Is` survives), `"pkg: verb-ing noun: %w"` wraps, functional-option `NewXxx` constructor + `var _ Iface = (*T)(nil)`, stdlib `testing` only (no testify), DW-named tests, `//go:build integration` + scratch names on the shared live cluster, snake_case JSON tags, strict template mappings.

## Test Infrastructure
- Unit: package-internal tests with `httptest` fake clusters (apply_test.go's `fakeCluster` is the pattern).
- Integration: `//go:build integration`, `ENGRAM_OPENSEARCH_URL` (default localhost:9200), unique scratch names + cleanup.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-3.1 | `Create` writes meta-doc AND provisions live index/alias in one call; follow-up `Get` returns the spec, no restart | COVERED | `TestDW_3_1_CreateProvisionsAndGetReturnsSpec` (integration: meta doc exists, alias resolves, Get returns spec); `TestDW_3_1_CreateUnit` (unit fake: one call does both writes) |
| DW-3.2 | `Update` adds a field via live `PUT mapping`; field-type change provisions `-v2` + swaps alias | COVERED | `TestDW_3_2_UpdateAddsMappingField` + `TestDW_3_2_TypeChangeReindexesAndSwapsAlias` (integration: live mapping gains field; alias moves v1→v2, docs survive reindex) |
| DW-3.3 | reads hit the cache; any write invalidates it | COVERED | `TestDW_3_3_ReadsHitCacheWritesInvalidate` (unit fake counts `_search` calls: Get×2→1 search; Create→List reflects new collection via reload) |
| DW-3.4 | YAML boot-seed applied twice is idempotent | COVERED | `TestDW_3_4_SeedTwiceIsIdempotent` (unit: fake registry records Create calls, 2nd run zero); `TestDW_3_4_SeedIdempotentLive` (integration) |

**All items COVERED:** YES

## Design Decisions

### Design: knowledge.CollectionRegistry (design-it-twice)

**Approaches Considered**
1. **A — Self-contained registry in `internal/knowledge`**: one package owns types + its own HTTP helper + OS mechanics.
2. **B — Seam in `knowledge`, OpenSearch impl in `store/registry.go`** (house pattern): `knowledge` owns domain types + `CollectionRegistry` interface + YAML seed (against the interface); `store.CollectionRegistry` implements it reusing `doJSON`/`do`/`ensureIndex`/`isIndexNotFound`, with the in-process cache inside the impl.
3. **C — Layered decorator**: store-side "raw" provisioner interface (`PutMapping`, `SwapAlias`, `CreateIndex`…) + a `knowledge.CachedRegistry` wrapper.

**Comparison**
| Criterion | A | B | C |
|-----------|---|---|---|
| Interface simplicity | 5 methods | 5 methods | 5 + ~6 leaked provisioner methods |
| Information hiding | good, but duplicates doJSON/ensureIndex | index/alias/version mechanics fully internal | **leaks index mechanics through the seam** |
| Caller ease of use | good | good (one constructor, name-only ops) | caller must wire two layers |
| Fit to repo layout / file scope | violates `store/registry*.go` scope; helper duplication | matches file scope + "concern.go seam / opensearch.go impl" idiom | two shallow modules (classitis) |

**Choice: B.** Deepest module per APOSD: 5 methods hide meta-doc CRUD, physical `-vN` naming, alias indirection, reindex+atomic swap, cache coherence, and create-race tolerance. Sacrifice: `store` grows a second concern file — accepted, it is the package where the unexported HTTP idiom lives (plan's file scope says the same).

**Depth Check**: interface methods: 5 (`Get`, `Create`, `Update`, `List`, `Provision`). Hidden: meta index name/shape, alias↔physical mapping, `-vN` versioning, `_reindex` + `_aliases` swap, refresh semantics, cache, op_type=create races. Common case (`Get`) complexity: trivial — name in, spec out.

### Contract (frozen for P4/P5/P6)
```go
// internal/knowledge
type FieldSpec struct{ Type string; Filterable, Sortable bool }
type AccessPolicy struct{ Public bool; Roles []string }
type CollectionSpec struct{ Name, Index, TextField string; Mappings map[string]FieldSpec; Access AccessPolicy }
type CollectionSummary struct{ Name string; Access AccessPolicy }
type Document struct{ ID, Title, Text, SourceVersion string; Fields map[string]any }
var ErrNotFound, ErrConflict error // knowledge-seam sentinels (store impl returns them %w-wrapped)
type CollectionRegistry interface {
    Get(ctx, name) (CollectionSpec, error)      // ErrNotFound
    Create(ctx, spec CollectionSpec) error      // ErrConflict on duplicate; provisions live
    Update(ctx, spec CollectionSpec) error      // ErrNotFound / ErrConflict (guarded meta write)
    List(ctx) ([]CollectionSummary, error)
    Provision(ctx, name string) error           // idempotent repair/boot reconcile
}
func Seed(ctx, reg CollectionRegistry, yamlSrc []byte) (created []string, err error)
```

### Key mechanics (store.CollectionRegistry)
- **Naming**: alias `knowledge-<name>` (= `spec.Index`), physical `knowledge-<name>-v<N>`. Name validated `^[a-z][a-z0-9_]*$` (no hyphens) + reserved `collections` → a data index can never match the meta template pattern `knowledge-collections-*`.
- **Meta doc** (`_id` = name, `?refresh=true` writes): `{name, text_field, index, version, public, roles, fields:[{name,type,filterable,sortable}], updated_at}` — fields as an **array** keeps the meta mapping strict/closed. Create uses `_create` (op_type=create) → 409 = `ErrConflict`; Update uses `if_seq_no`/`if_primary_term` from the read → 409 = `ErrConflict`.
- **Data index mapping**: `dynamic: strict`; base properties every collection gets (P4 stamps them): `title` text, `<TextField>` text, `collection`/`source`/`source_version`/`harvest_id` keyword, `harvested_at` date; spec fields merged on top. User field names must not collide with base names; allowed types: text|keyword|date|long|integer|short|byte|double|float|boolean.
- **Create** = validate → meta `_create` → provision v1 (create body carries mappings **and** alias: index+alias creation is one atomic PUT) → invalidate cache. Meta-write-succeeded/provision-failed is repaired by `Provision(name)` (documented recovery).
- **Update** = read meta (realtime `_doc` GET) → diff: added fields → `PUT <alias>/_mapping`; type-changed field → create `v<N+1>` (full mapping, no alias) → `POST _reindex` (`wait_for_completion=true&refresh=true`) → **atomic `_aliases` remove+add** (verified above) → guarded meta write (the commit point) → invalidate. Current physical resolved from `GET /_alias/<alias>` (not meta) so swap is drift-proof; old `-vN` index is left in place as a safety net (documented). Field *removal* and filterable/sortable flips are meta-only (live mapping stays a superset — OS cannot unmap without reindex).
- **Cache**: whole-set cache (`map[name]spec` + `valid` flag, `sync.RWMutex`) loaded lazily by one `_search` (size 1000 — registry of collections, not docs); `Get`/`List` serve from it; every successful write sets `valid=false`. `isIndexNotFound` on load → empty registry (mirrors read-path house rule).
- **Concurrent create race**: meta `_create` 409 → `ErrConflict` for the loser; index provisioning tolerates `resource_already_exists_exception` via the extended `ensureIndex(…, body)`.
- **Seed**: `Get` each YAML entry → exists = no-op (even if spec differs — boot seed, not reconciler; documented), `ErrNotFound` = `Create` (a racing `ErrConflict` also counts as no-op). Returns created names so "second run makes no changes" is assertable.

### templates.go / apply.go additions
`KnowledgeCollectionsTemplateName = "knowledge-collections"`, `KnowledgeCollectionsIndex = "knowledge-collections-000001"`, embed `templates/knowledge-collections.json` (pattern `knowledge-collections-*`, strict, priority 100, 1 shard/0 replicas), one apply step row + one ensureIndex row.

## Prerequisites
- [x] Live 3.1.0 cluster reachable at localhost:9200 (assumption verified against it)
- [x] YAML dep present (`go.yaml.in/yaml/v2`, direct)
- [x] Phase 1/2 seams in place (mcp types, knowledgeauth primitives)

## Recommendation
**BUILD** — plan fits reality; only mechanical reconciliations (ensureIndex body param, sentinel placement, DTO-vs-domain seam decision) recorded above.
