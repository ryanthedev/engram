// Package knowledge defines the domain types and the CollectionRegistry seam
// for Engram's document knowledge platform: runtime-managed, BM25-searchable
// document collections living alongside (and never touching) the memory path.
//
// This package owns the domain shapes (CollectionSpec, AccessPolicy,
// FieldSpec, Document); internal/mcp keeps its own thin MCP-facing DTOs
// (mcp.CollectionSpec et al.) and Phase 6 translates at that seam — see the
// mapping note on CollectionSpec. The OpenSearch implementation of
// CollectionRegistry lives in internal/store (registry.go).
package knowledge

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Get/Update/Provision for a collection name the
// registry does not know. Returned wrapped with %w — match with errors.Is.
var ErrNotFound = errors.New("knowledge: collection not found")

// ErrConflict is returned when a registry write loses a race: Create on a
// name that already exists, or Update against a concurrently-modified spec.
// Returned wrapped with %w — match with errors.Is.
var ErrConflict = errors.New("knowledge: collection conflict")

// FieldSpec declares one collection field: its index type ("keyword",
// "date", "integer", ...) and whether queries may filter or sort on it.
// Filterable/Sortable are registry metadata consumed by the retriever's
// validation — they do not change the physical mapping.
type FieldSpec struct {
	Type       string
	Filterable bool
	Sortable   bool
}

// AccessPolicy is a collection's read-access policy: Public allows any
// authenticated caller; otherwise the caller must hold one of Roles.
// (Enforced by knowledgeauth at the Phase-6 request barricade.)
type AccessPolicy struct {
	Public bool
	Roles  []string
}

// CollectionSpec describes one document collection. Index is the stable
// OpenSearch alias the retriever queries; it is ASSIGNED by the registry
// (ignored on Create/Update input) and never chosen by callers — physical
// -vN index names and alias mechanics stay registry-internal.
//
// MCP seam mapping (Phase 6 translates; do not add a second domain type):
// mcp.CollectionSpec flattens Access into Public/Roles and omits Index.
type CollectionSpec struct {
	Name      string
	Index     string
	TextField string
	Mappings  map[string]FieldSpec
	Access    AccessPolicy

	// Fragment sizing for search-time highlight extraction. Zero means UNSET
	// — consumers must read the resolved values through FragmentSizing, which
	// applies the global defaults, never a literal zero size.
	FragmentSize      int
	NumberOfFragments int
	// Marker strings wrapped around matched terms inside each fragment. Both
	// empty (the zero value) means markers OFF — clean extraction, the
	// default. Markers are a per-collection opt-in; no global marker default
	// exists, so these two fields have no FragmentSizing-style fallback.
	HighlightPreTag  string
	HighlightPostTag string
}

// Global fragment-sizing fallbacks: a collection that declares no sizing
// gets fragments of DefaultFragmentSize chars, DefaultNumberOfFragments per
// hit (plan D4).
const (
	DefaultFragmentSize      = 240
	DefaultNumberOfFragments = 3
)

// FragmentSizing resolves the collection's fragment sizing, applying the
// global fallbacks: any unset (zero — the proto default for an absent field)
// or nonsensical (negative) value reads as the corresponding default, so an
// existing collection created before these knobs existed can never produce
// zero-size or zero-count fragments. Consumers read sizing ONLY through
// here; the raw fields stay raw so unset survives proto round-trips.
func (s CollectionSpec) FragmentSizing() (size, count int) {
	size, count = s.FragmentSize, s.NumberOfFragments
	if size <= 0 {
		size = DefaultFragmentSize
	}
	if count <= 0 {
		count = DefaultNumberOfFragments
	}
	return size, count
}

// CollectionSummary is one List entry: enough to enumerate collections and
// filter them by access; callers Get(name) for the full spec (cached, cheap).
type CollectionSummary struct {
	Name   string
	Access AccessPolicy
}

// Document is one knowledge document as consumed by the store's bulk-write
// path (Phase 4). Fields carries collection-specific values matching the
// collection's declared Mappings. mcp.KnowledgeDoc is its MCP-facing twin.
type Document struct {
	// ID is the upsert identity: re-ingesting an id overwrites in place.
	ID    string
	Title string
	// Text is the full-text body indexed under the collection's TextField.
	Text string
	// SourceVersion is a source-side change marker (opaque to the server).
	SourceVersion string
	Fields        map[string]any
}

// CollectionRegistry is the durable, runtime-mutable registry of collections.
// It is a deep module: callers name a collection and describe fields; the
// registry hides the meta-index, physical -vN index names, alias indirection,
// live mapping updates, reindex + atomic alias swap, and cache coherence.
// No operation ever requires a server restart.
type CollectionRegistry interface {
	// Get returns the spec for name (ErrNotFound if unknown).
	Get(ctx context.Context, name string) (CollectionSpec, error)
	// Create registers a new collection AND provisions its live index/alias
	// in one call. A duplicate name is ErrConflict.
	Create(ctx context.Context, spec CollectionSpec) error
	// Update amends an existing collection (ErrNotFound if unknown). Added
	// fields apply live; a field-type change reindexes into a new -vN index
	// and atomically swaps the alias. A concurrent update is ErrConflict.
	Update(ctx context.Context, spec CollectionSpec) error
	// List enumerates all registered collections.
	List(ctx context.Context) ([]CollectionSummary, error)
	// Provision idempotently ensures name's live index/alias exists — the
	// boot-time reconcile and the repair path for a Create whose index
	// provisioning failed after registration.
	Provision(ctx context.Context, name string) error
}
