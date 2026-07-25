package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ryanthedev/engram/internal/knowledge"
)

// Collection registry layout on the cluster (all hidden behind this module —
// callers name a collection, never an index):
//   - meta-index (KnowledgeCollectionsIndex): one strict-mapped doc per
//     collection, _id = collection name — the durable source of truth.
//   - per collection: physical index knowledge-<name>-v<N> behind the stable
//     alias knowledge-<name>. Readers/writers only ever see the alias; a
//     field-type change reindexes into v<N+1> and atomically swaps the alias
//     (one _aliases actions block — verified atomic on the pinned 3.1 line).
const collectionIndexPrefix = "knowledge-"

// collectionNameRE is the collection-name grammar. No hyphens: the physical
// knowledge-<name>-vN names can then never collide with the meta-index
// pattern knowledge-collections-* (the reserved name "collections" closes
// the one remaining hole).
var collectionNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// reservedCollectionNames can never be collection names.
var reservedCollectionNames = map[string]bool{"collections": true}

// baseDocProperties are stamped on every collection's physical index in
// addition to its declared text field and Mappings: the document envelope
// (title) plus the harvest provenance fields Phase 4's bulk writer sets.
// Declared field names must not collide with them.
var baseDocProperties = map[string]string{
	"title":          "text",
	"collection":     "keyword",
	"source":         "keyword",
	"source_version": "keyword",
	"harvest_id":     "keyword",
	"harvested_at":   "date",
}

// SearchProvenanceExcludes are the envelope fields a knowledge SEARCH hit
// never needs: the harvest bookkeeping every stored document carries but no
// caller reads back off a result. Derived from baseDocProperties minus
// "title" — the one envelope field callers scan results by — so the two can
// never drift as the envelope grows. Sorted, because buildQuery's output is
// pinned by golden-byte tests and Go map iteration order is random.
//
// Applied at query time via _source.excludes, so the bytes never leave the
// cluster. Deliberately NOT applied to a full_body search: the vault exporter
// (internal/cli/vaultknowledge.go) drains whole documents through that path
// and reads these fields back. GetDocument is likewise untouched — it returns
// the full stored _source by contract.
func SearchProvenanceExcludes() []string {
	out := make([]string, 0, len(baseDocProperties)-1)
	for name := range baseDocProperties {
		if name == "title" {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// allowedFieldTypes is the FieldSpec.Type vocabulary.
var allowedFieldTypes = map[string]bool{
	"text": true, "keyword": true, "date": true, "boolean": true,
	"long": true, "integer": true, "short": true, "byte": true,
	"double": true, "float": true,
}

// defaultTextField is the BM25 target when a spec does not name one.
const defaultTextField = "text"

// registryCacheSize bounds the meta-index load. It is a registry of
// collections, not documents — hundreds would already be extraordinary.
const registryCacheSize = 1000

// CollectionRegistry is the OpenSearch-backed knowledge.CollectionRegistry:
// a durable, runtime-mutable registry of document collections with live index
// provisioning, so a collection change never restarts engram. Reads are
// served from an in-process whole-set cache invalidated by every write.
// It is safe for concurrent use.
type CollectionRegistry struct {
	client    *http.Client
	baseURL   string
	metaIndex string

	mu    sync.RWMutex
	cache map[string]knowledge.CollectionSpec
	valid bool
	gen   uint64 // bumped by invalidate; guards the load-then-store race
}

var _ knowledge.CollectionRegistry = (*CollectionRegistry)(nil)

// RegistryOption configures a CollectionRegistry.
type RegistryOption func(*CollectionRegistry)

// WithRegistryMetaIndex overrides the meta-index the registry persists specs
// in. Defaults to KnowledgeCollectionsIndex; tests use a scratch name
// matching the knowledge-collections-* template pattern.
func WithRegistryMetaIndex(name string) RegistryOption {
	return func(r *CollectionRegistry) { r.metaIndex = name }
}

// NewCollectionRegistry returns a CollectionRegistry over the OpenSearch
// cluster at baseURL. client must not be nil. Apply must have PUT the
// knowledge-collections template before first use.
func NewCollectionRegistry(client *http.Client, baseURL string, opts ...RegistryOption) *CollectionRegistry {
	r := &CollectionRegistry{
		client:    client,
		baseURL:   strings.TrimRight(baseURL, "/"),
		metaIndex: KnowledgeCollectionsIndex,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// collectionMetaDoc is the meta-index row shape (strict-mapped by
// templates/knowledge-collections.json). Mappings persist as an ARRAY of
// fixed-key rows so the meta mapping stays closed under dynamic:strict.
type collectionMetaDoc struct {
	Name      string      `json:"name"`
	TextField string      `json:"text_field"`
	Index     string      `json:"index"`
	Version   int         `json:"version"`
	Public    bool        `json:"public"`
	Roles     []string    `json:"roles"`
	Fields    []metaField `json:"fields"`
	UpdatedAt time.Time   `json:"updated_at"`
	// Fragment sizing + highlight marker tags (Phase-2 fragments). Zero/empty
	// means unset — FragmentSizing applies the global defaults at consumption
	// — and omitempty keeps pre-fragment meta rows byte-stable.
	FragmentSize      int    `json:"fragment_size,omitempty"`
	NumberOfFragments int    `json:"number_of_fragments,omitempty"`
	HighlightPreTag   string `json:"highlight_pre_tag,omitempty"`
	HighlightPostTag  string `json:"highlight_post_tag,omitempty"`
}

type metaField struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Filterable bool   `json:"filterable"`
	Sortable   bool   `json:"sortable"`
}

func metaDocFrom(spec knowledge.CollectionSpec, version int) collectionMetaDoc {
	doc := collectionMetaDoc{
		Name:              spec.Name,
		TextField:         spec.TextField,
		Index:             spec.Index,
		Version:           version,
		Public:            spec.Access.Public,
		Roles:             spec.Access.Roles,
		UpdatedAt:         time.Now().UTC(),
		FragmentSize:      spec.FragmentSize,
		NumberOfFragments: spec.NumberOfFragments,
		HighlightPreTag:   spec.HighlightPreTag,
		HighlightPostTag:  spec.HighlightPostTag,
	}
	names := make([]string, 0, len(spec.Mappings))
	for name := range spec.Mappings {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic rows
	for _, name := range names {
		f := spec.Mappings[name]
		doc.Fields = append(doc.Fields, metaField{Name: name, Type: f.Type, Filterable: f.Filterable, Sortable: f.Sortable})
	}
	return doc
}

func (d collectionMetaDoc) spec() knowledge.CollectionSpec {
	spec := knowledge.CollectionSpec{
		Name:              d.Name,
		Index:             d.Index,
		TextField:         d.TextField,
		Access:            knowledge.AccessPolicy{Public: d.Public, Roles: d.Roles},
		FragmentSize:      d.FragmentSize,
		NumberOfFragments: d.NumberOfFragments,
		HighlightPreTag:   d.HighlightPreTag,
		HighlightPostTag:  d.HighlightPostTag,
	}
	if len(d.Fields) > 0 {
		spec.Mappings = make(map[string]knowledge.FieldSpec, len(d.Fields))
		for _, f := range d.Fields {
			spec.Mappings[f.Name] = knowledge.FieldSpec{Type: f.Type, Filterable: f.Filterable, Sortable: f.Sortable}
		}
	}
	return spec
}

// cloneSpec deep-copies the mutable members so cached specs can't be mutated
// through a Get result.
func cloneSpec(spec knowledge.CollectionSpec) knowledge.CollectionSpec {
	if spec.Mappings != nil {
		m := make(map[string]knowledge.FieldSpec, len(spec.Mappings))
		for k, v := range spec.Mappings {
			m[k] = v
		}
		spec.Mappings = m
	}
	if spec.Access.Roles != nil {
		spec.Access.Roles = append([]string(nil), spec.Access.Roles...)
	}
	return spec
}

func aliasFor(name string) string { return collectionIndexPrefix + name }

func physicalFor(name string, version int) string {
	return fmt.Sprintf("%s%s-v%d", collectionIndexPrefix, name, version)
}

// validateCollectionName is the single gate every code path that embeds a
// caller-supplied collection name into an OpenSearch request path MUST pass
// the name through first. The grammar admits no '/', '.', or other path
// metacharacter, so a validated name cannot traverse out of its index — this
// is the barricade that keeps the "callers name a collection, never an index"
// guarantee from being subverted by a crafted name (e.g. a "../../other/_search"
// traversal). Errors name the offending value and the rule so a caller can
// self-correct.
func validateCollectionName(name string) error {
	if !collectionNameRE.MatchString(name) {
		return fmt.Errorf("store: invalid collection name %q: must match %s", name, collectionNameRE)
	}
	if reservedCollectionNames[name] {
		return fmt.Errorf("store: collection name %q is reserved", name)
	}
	return nil
}

// normalizeSpec validates a caller-supplied spec and fills registry-assigned
// members (Index, default TextField). Validation errors name the offending
// value and the rule so an LLM caller can self-correct.
func normalizeSpec(spec knowledge.CollectionSpec) (knowledge.CollectionSpec, error) {
	if err := validateCollectionName(spec.Name); err != nil {
		return spec, err
	}
	if spec.TextField == "" {
		spec.TextField = defaultTextField
	}
	if !collectionNameRE.MatchString(spec.TextField) {
		return spec, fmt.Errorf("store: invalid text field %q: must match %s", spec.TextField, collectionNameRE)
	}
	if _, clash := baseDocProperties[spec.TextField]; clash {
		return spec, fmt.Errorf("store: text field %q collides with a reserved document field", spec.TextField)
	}
	for name, f := range spec.Mappings {
		if !collectionNameRE.MatchString(name) {
			return spec, fmt.Errorf("store: invalid field name %q: must match %s", name, collectionNameRE)
		}
		if _, clash := baseDocProperties[name]; clash {
			return spec, fmt.Errorf("store: field %q collides with a reserved document field", name)
		}
		if name == spec.TextField {
			return spec, fmt.Errorf("store: field %q collides with the collection text field", name)
		}
		if !allowedFieldTypes[f.Type] {
			return spec, fmt.Errorf("store: field %q has unsupported type %q (allowed: text, keyword, date, boolean, long, integer, short, byte, double, float)", name, f.Type)
		}
	}
	spec.Index = aliasFor(spec.Name) // registry-assigned, never caller-chosen
	return spec, nil
}

// dataProperties builds the physical index property map for spec: the base
// envelope + the text field + every declared field.
func dataProperties(spec knowledge.CollectionSpec) map[string]any {
	props := make(map[string]any, len(baseDocProperties)+len(spec.Mappings)+1)
	for name, typ := range baseDocProperties {
		props[name] = map[string]any{"type": typ}
	}
	props[spec.TextField] = map[string]any{"type": "text"}
	for name, f := range spec.Mappings {
		props[name] = map[string]any{"type": f.Type}
	}
	return props
}

// dataIndexBody is the create-time definition for a collection's physical
// index. withAlias attaches the collection alias atomically in the same PUT
// (Create/Provision); the reindex path creates v<N+1> WITHOUT the alias and
// swaps it only after the copy completes.
func dataIndexBody(spec knowledge.CollectionSpec, withAlias bool) ([]byte, error) {
	body := map[string]any{
		"settings": map[string]any{"number_of_shards": 1, "number_of_replicas": 0},
		"mappings": map[string]any{"dynamic": "strict", "properties": dataProperties(spec)},
	}
	if withAlias {
		body["aliases"] = map[string]any{spec.Index: map[string]any{}}
	}
	return json.Marshal(body)
}

// Get implements knowledge.CollectionRegistry from the cache.
func (r *CollectionRegistry) Get(ctx context.Context, name string) (knowledge.CollectionSpec, error) {
	specs, err := r.cached(ctx)
	if err != nil {
		return knowledge.CollectionSpec{}, err
	}
	spec, ok := specs[name]
	if !ok {
		return knowledge.CollectionSpec{}, fmt.Errorf("store: getting collection %s: %w", name, knowledge.ErrNotFound)
	}
	return cloneSpec(spec), nil
}

// List implements knowledge.CollectionRegistry from the cache, sorted by name.
func (r *CollectionRegistry) List(ctx context.Context) ([]knowledge.CollectionSummary, error) {
	specs, err := r.cached(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]knowledge.CollectionSummary, 0, len(specs))
	for _, spec := range specs {
		spec = cloneSpec(spec)
		out = append(out, knowledge.CollectionSummary{Name: spec.Name, Access: spec.Access})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Create implements knowledge.CollectionRegistry: it registers the spec in
// the meta-index (op_type=create — a duplicate name or a lost concurrent
// race is ErrConflict) and provisions the live v1 index with its alias in
// the same call. If provisioning fails after registration, the collection
// exists but has no index; Provision(name) repairs it.
func (r *CollectionRegistry) Create(ctx context.Context, spec knowledge.CollectionSpec) error {
	spec, err := normalizeSpec(spec)
	if err != nil {
		return err
	}
	body, err := json.Marshal(metaDocFrom(spec, 1))
	if err != nil {
		return fmt.Errorf("store: encoding collection %s: %w", spec.Name, err)
	}
	url := fmt.Sprintf("%s/%s/_create/%s?refresh=true", r.baseURL, r.metaIndex, spec.Name)
	status, decoded, err := doJSON(ctx, r.client, http.MethodPut, url, body)
	if err != nil {
		return fmt.Errorf("store: creating collection %s: %w", spec.Name, err)
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
	case http.StatusConflict:
		return fmt.Errorf("store: creating collection %s: %w", spec.Name, knowledge.ErrConflict)
	default:
		return fmt.Errorf("store: creating collection %s: unexpected status %d: %v", spec.Name, status, decoded)
	}
	defer r.invalidate()
	if err := r.provision(ctx, spec, 1); err != nil {
		return fmt.Errorf("store: collection %s registered but index provisioning failed (Provision repairs it): %w", spec.Name, err)
	}
	return nil
}

// Provision implements knowledge.CollectionRegistry: it idempotently ensures
// name's live index/alias exists per the registered spec — the boot-time
// reconcile and the repair path for a partially-failed Create. If the alias
// already resolves it is a no-op, whatever physical version carries it.
func (r *CollectionRegistry) Provision(ctx context.Context, name string) error {
	if err := validateCollectionName(name); err != nil {
		return err
	}
	doc, _, _, err := r.getMetaDoc(ctx, name)
	if err != nil {
		return err
	}
	spec := doc.spec()
	if physical, err := r.currentPhysical(ctx, spec.Index); err != nil {
		return fmt.Errorf("store: provisioning collection %s: %w", name, err)
	} else if physical != "" {
		return nil // alias live: already provisioned
	}
	if err := r.provision(ctx, spec, doc.Version); err != nil {
		return fmt.Errorf("store: provisioning collection %s: %w", name, err)
	}
	return nil
}

// provision creates the physical v<version> index with mappings + alias,
// tolerating a concurrent creator (the ensureIndex idempotency pattern).
func (r *CollectionRegistry) provision(ctx context.Context, spec knowledge.CollectionSpec, version int) error {
	body, err := dataIndexBody(spec, true)
	if err != nil {
		return fmt.Errorf("store: encoding index body for %s: %w", spec.Name, err)
	}
	_, err = ensureIndex(ctx, r.client, r.baseURL, physicalFor(spec.Name, version), body)
	return err
}

// Update implements knowledge.CollectionRegistry. Added fields apply live via
// PUT _mapping on the alias; a field-type change provisions v<N+1> with the
// full new mapping, copies the data with _reindex, and swaps the alias in ONE
// atomic _aliases actions block (zero-downtime — verified on the pinned 3.1
// line). The guarded meta write is the commit point: losing it to a
// concurrent Update is ErrConflict (the already-applied index ops are
// compatible no-ops on retry). Removed fields and Filterable/Sortable flips
// change only the registry metadata — the live mapping stays a superset
// (OpenSearch cannot unmap a field without a reindex nothing needs).
func (r *CollectionRegistry) Update(ctx context.Context, spec knowledge.CollectionSpec) error {
	spec, err := normalizeSpec(spec)
	if err != nil {
		return err
	}
	cur, seqNo, primaryTerm, err := r.getMetaDoc(ctx, spec.Name)
	if err != nil {
		return err
	}
	added, changed := diffProperties(dataProperties(cur.spec()), dataProperties(spec))
	version := cur.Version
	switch {
	case len(changed) > 0:
		if version, err = r.reindexSwap(ctx, spec, cur.Version); err != nil {
			return fmt.Errorf("store: updating collection %s: %w", spec.Name, err)
		}
	case len(added) > 0:
		body, err := json.Marshal(map[string]any{"properties": added})
		if err != nil {
			return fmt.Errorf("store: encoding mapping for %s: %w", spec.Name, err)
		}
		status, decoded, err := doJSON(ctx, r.client, http.MethodPut, r.baseURL+"/"+spec.Index+"/_mapping", body)
		if err != nil {
			return fmt.Errorf("store: updating mapping of %s: %w", spec.Name, err)
		}
		if status != http.StatusOK {
			return fmt.Errorf("store: updating mapping of %s: unexpected status %d: %v", spec.Name, status, decoded)
		}
	}
	body, err := json.Marshal(metaDocFrom(spec, version))
	if err != nil {
		return fmt.Errorf("store: encoding collection %s: %w", spec.Name, err)
	}
	url := fmt.Sprintf("%s/%s/_doc/%s?if_seq_no=%d&if_primary_term=%d&refresh=true", r.baseURL, r.metaIndex, spec.Name, seqNo, primaryTerm)
	status, decoded, err := doJSON(ctx, r.client, http.MethodPut, url, body)
	if err != nil {
		return fmt.Errorf("store: updating collection %s: %w", spec.Name, err)
	}
	switch status {
	case http.StatusOK, http.StatusCreated:
		r.invalidate()
		return nil
	case http.StatusConflict:
		return fmt.Errorf("store: updating collection %s: %w", spec.Name, knowledge.ErrConflict)
	default:
		return fmt.Errorf("store: updating collection %s: unexpected status %d: %v", spec.Name, status, decoded)
	}
}

// diffProperties reports the properties in want but not got (added) and those
// present in both with a different type (changed).
func diffProperties(got, want map[string]any) (added, changed map[string]any) {
	added, changed = map[string]any{}, map[string]any{}
	for name, prop := range want {
		cur, ok := got[name]
		switch {
		case !ok:
			added[name] = prop
		case fmt.Sprint(cur) != fmt.Sprint(prop):
			changed[name] = prop
		}
	}
	return added, changed
}

// reindexSwap performs the zero-downtime type-change path: create v<N+1> with
// the full new mapping (no alias), _reindex the current data into it, then
// atomically move the alias in one _aliases actions block. The old physical
// index is left in place as a safety net — the alias no longer resolves to
// it, so readers and writers cannot reach it. The current physical is
// resolved from the live alias (not the meta version), so the swap stays
// correct even if a previous partial update left the meta row behind.
func (r *CollectionRegistry) reindexSwap(ctx context.Context, spec knowledge.CollectionSpec, metaVersion int) (newVersion int, err error) {
	oldPhysical, err := r.currentPhysical(ctx, spec.Index)
	if err != nil {
		return 0, err
	}
	if oldPhysical == "" {
		oldPhysical = physicalFor(spec.Name, metaVersion)
	}
	newVersion = parseVersion(oldPhysical, spec.Index) + 1
	if newVersion <= metaVersion {
		newVersion = metaVersion + 1
	}
	newPhysical := physicalFor(spec.Name, newVersion)

	body, err := dataIndexBody(spec, false)
	if err != nil {
		return 0, fmt.Errorf("store: encoding index body for %s: %w", spec.Name, err)
	}
	if _, err := ensureIndex(ctx, r.client, r.baseURL, newPhysical, body); err != nil {
		return 0, err
	}

	reindex, err := json.Marshal(map[string]any{
		"source": map[string]any{"index": oldPhysical},
		"dest":   map[string]any{"index": newPhysical},
	})
	if err != nil {
		return 0, fmt.Errorf("store: encoding reindex for %s: %w", spec.Name, err)
	}
	status, decoded, err := doJSON(ctx, r.client, http.MethodPost, r.baseURL+"/_reindex?wait_for_completion=true&refresh=true", reindex)
	if err != nil {
		return 0, fmt.Errorf("store: reindexing %s into %s: %w", oldPhysical, newPhysical, err)
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("store: reindexing %s into %s: unexpected status %d: %v", oldPhysical, newPhysical, status, decoded)
	}
	if failures, _ := decoded["failures"].([]any); len(failures) > 0 {
		return 0, fmt.Errorf("store: reindexing %s into %s: %d docs failed (first: %v)", oldPhysical, newPhysical, len(failures), failures[0])
	}

	swap, err := json.Marshal(map[string]any{"actions": []any{
		map[string]any{"remove": map[string]any{"index": oldPhysical, "alias": spec.Index}},
		map[string]any{"add": map[string]any{"index": newPhysical, "alias": spec.Index}},
	}})
	if err != nil {
		return 0, fmt.Errorf("store: encoding alias swap for %s: %w", spec.Name, err)
	}
	status, decoded, err = doJSON(ctx, r.client, http.MethodPost, r.baseURL+"/_aliases", swap)
	if err != nil {
		return 0, fmt.Errorf("store: swapping alias %s: %w", spec.Index, err)
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("store: swapping alias %s: unexpected status %d: %v", spec.Index, status, decoded)
	}
	return newVersion, nil
}

// currentPhysical resolves the physical index behind alias, or "" when the
// alias does not exist.
func (r *CollectionRegistry) currentPhysical(ctx context.Context, alias string) (string, error) {
	status, decoded, err := doJSON(ctx, r.client, http.MethodGet, r.baseURL+"/_alias/"+alias, nil)
	if err != nil {
		return "", fmt.Errorf("store: resolving alias %s: %w", alias, err)
	}
	switch status {
	case http.StatusOK:
		for physical := range decoded {
			return physical, nil
		}
		return "", nil
	case http.StatusNotFound:
		return "", nil
	default:
		return "", fmt.Errorf("store: resolving alias %s: unexpected status %d: %v", alias, status, decoded)
	}
}

// parseVersion extracts N from "<alias>-vN"; 0 when physical doesn't parse.
func parseVersion(physical, alias string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(physical, alias+"-v"))
	if err != nil {
		return 0
	}
	return n
}

// getMetaDoc reads one meta row directly (a realtime _doc GET — never the
// cache: Update's guard needs the live _seq_no/_primary_term). It re-validates
// name as a defense-in-depth barricade: it is the one helper that interpolates
// a caller-supplied name into a request path, so no future caller can reach it
// with an unvalidated (path-traversing) name even if it forgets the top-level
// gate.
func (r *CollectionRegistry) getMetaDoc(ctx context.Context, name string) (doc collectionMetaDoc, seqNo, primaryTerm int64, err error) {
	if err := validateCollectionName(name); err != nil {
		return doc, 0, 0, err
	}
	url := fmt.Sprintf("%s/%s/_doc/%s", r.baseURL, r.metaIndex, name)
	status, decoded, err := doJSON(ctx, r.client, http.MethodGet, url, nil)
	if err != nil {
		return doc, 0, 0, fmt.Errorf("store: reading collection %s: %w", name, err)
	}
	switch {
	case status == http.StatusOK:
	case status == http.StatusNotFound:
		// Absent doc or not-yet-created meta index: both mean "no such
		// collection" to a caller.
		return doc, 0, 0, fmt.Errorf("store: reading collection %s: %w", name, knowledge.ErrNotFound)
	default:
		return doc, 0, 0, fmt.Errorf("store: reading collection %s: unexpected status %d: %v", name, status, decoded)
	}
	raw, err := json.Marshal(decoded["_source"])
	if err != nil {
		return doc, 0, 0, fmt.Errorf("store: re-encoding collection %s: %w", name, err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return doc, 0, 0, fmt.Errorf("store: decoding collection %s: %w", name, err)
	}
	sq, _ := decoded["_seq_no"].(float64)
	pt, _ := decoded["_primary_term"].(float64)
	return doc, int64(sq), int64(pt), nil
}

// cached returns the whole collection set, loading it from the meta-index on
// a cold or invalidated cache. The generation counter keeps a load that raced
// a write from being stored as fresh.
func (r *CollectionRegistry) cached(ctx context.Context) (map[string]knowledge.CollectionSpec, error) {
	r.mu.RLock()
	if r.valid {
		specs := r.cache
		r.mu.RUnlock()
		return specs, nil
	}
	gen := r.gen
	r.mu.RUnlock()

	specs, err := r.loadAll(ctx)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.gen == gen {
		r.cache, r.valid = specs, true
	}
	r.mu.Unlock()
	return specs, nil
}

// invalidate marks the cache stale; the next read reloads from the meta-index.
func (r *CollectionRegistry) invalidate() {
	r.mu.Lock()
	r.valid = false
	r.gen++
	r.mu.Unlock()
}

// loadAll fetches every meta row. A not-yet-created meta index reads as an
// empty registry (the house 404-as-empty rule), not an error.
func (r *CollectionRegistry) loadAll(ctx context.Context) (map[string]knowledge.CollectionSpec, error) {
	body := []byte(fmt.Sprintf(`{"size":%d,"query":{"match_all":{}}}`, registryCacheSize))
	status, decoded, err := doJSON(ctx, r.client, http.MethodPost, r.baseURL+"/"+r.metaIndex+"/_search", body)
	if err != nil {
		return nil, fmt.Errorf("store: listing collections: %w", err)
	}
	if isIndexNotFound(status, decoded) {
		return map[string]knowledge.CollectionSpec{}, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("store: listing collections: unexpected status %d: %v", status, decoded)
	}
	outer, _ := decoded["hits"].(map[string]any)
	hits, _ := outer["hits"].([]any)
	specs := make(map[string]knowledge.CollectionSpec, len(hits))
	for _, h := range hits {
		hit, _ := h.(map[string]any)
		raw, err := json.Marshal(hit["_source"])
		if err != nil {
			return nil, fmt.Errorf("store: re-encoding collection row: %w", err)
		}
		var doc collectionMetaDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("store: decoding collection row: %w", err)
		}
		specs[doc.Name] = doc.spec()
	}
	return specs, nil
}
