package harvester

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"sync"

	"github.com/ryanthedev/engram/internal/mcp"
)

// HarvestMode defines the harvesting strategy.
type HarvestMode int

const (
	// FullHarvest performs a complete harvest of all source items.
	FullHarvest HarvestMode = iota
	// Incremental performs a harvest of only modified/new source items.
	Incremental
)

// Source is the seam interface that all document sources must implement.
type Source interface {
	Type() string
	Mode() HarvestMode
	Harvest(ctx context.Context, sink Sink) error
}

// Sink is the interface used by sources to ingest documents. Deletion is NOT a
// Sink operation: engram's knowledge API exposes only the mark-and-sweep
// KnowledgeDelete (harvest_id != current), no per-doc-id delete, so orphaned
// docs are removed by a FullHarvest source's post-run sweep — see Deps and the
// Runner, not here.
type Sink interface {
	Add(doc mcp.KnowledgeDoc) error
	Flush(ctx context.Context) error
}

// ScopedSource is an optional extension a Source may implement to partition its
// documents into independent mark-and-sweep scopes — one `source` value per
// scope, used for BOTH ingest and the not-current sweep. Because each scope's
// sweep only matches rows carrying that scope's `source`, harvesting one scope
// in a separate run can never delete another scope's documents (the multi-repo
// wipe bug: github-repos previously shared one scope = Type(), so re-harvesting
// one repo swept every other repo's docs in the collection).
//
// Scopes are derived from CONFIG, not from the documents emitted this run, so a
// scope that yields zero documents this run still has its own stale docs swept
// and nothing orphans. The Runner harvests every scope before sweeping any, so
// the run-wide fail-safe still holds: any error aborts before every sweep.
//
// A Source that does NOT implement ScopedSource is harvested as a single scope
// equal to Type(), swept with the zero-doc empty guard — the pre-existing
// behavior (backward compatible). The seam is generic; only multi-item sources
// (e.g. github-repos, one scope per repo) need adopt it.
type ScopedSource interface {
	Source
	// SweepScopes returns the full, config-derived set of sweep scopes. Each
	// element becomes the `source` value for its documents' ingest and sweep and
	// must be non-empty and validated at construction (the same owner/repo +
	// flag-injection barricade as the identifiers it is built from).
	SweepScopes() []string
	// HarvestScope harvests only the documents belonging to scope into sink.
	// scope is always one of SweepScopes().
	HarvestScope(ctx context.Context, scope string, sink Sink) error
}

// Deps carries runtime dependencies injected into each source at construction.
// It is the stable extensibility seam in the factory signature; v1 carries a
// logger (sources log skips, deleted-record notices, and politeness backoffs).
type Deps struct {
	Logger *slog.Logger
}

var (
	registryMu sync.RWMutex
	registry   = make(map[string]func(SourceConfig, Deps) (Source, error))
	namePat    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// Register registers a new source type builder in the package-level registry.
func Register(typ string, build func(SourceConfig, Deps) (Source, error)) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[typ] = build
}

// Build constructs a source based on configuration and dependencies.
func Build(cfg SourceConfig, deps Deps) (Source, error) {
	registryMu.RLock()
	build, ok := registry[cfg.Type]
	registryMu.RUnlock()

	if !ok {
		return nil, &UnknownSourceTypeError{
			Type:            cfg.Type,
			RegisteredTypes: RegisteredTypes(),
		}
	}

	src, err := build(cfg, deps)
	if err != nil {
		return nil, fmt.Errorf("harvester: building source type %q: %w", cfg.Type, err)
	}
	return src, nil
}

// RegisteredTypes returns a sorted list of registered source type names.
func RegisteredTypes() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()

	types := make([]string, 0, len(registry))
	for t := range registry {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

func isSourceTypeRegistered(typ string) bool {
	registryMu.RLock()
	defer registryMu.RUnlock()
	_, ok := registry[typ]
	return ok
}

// validateName verifies the name matches the path-safe pattern ^[a-z0-9][a-z0-9_-]*$.
func validateName(name string) error {
	if !namePat.MatchString(name) {
		return &InvalidNameError{Name: name}
	}
	return nil
}
