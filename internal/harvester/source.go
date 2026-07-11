package harvester

import (
	"context"
	"fmt"
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

// Sink is the interface used by sources to ingest documents.
type Sink interface {
	Add(doc mcp.KnowledgeDoc) error
	Delete(id string) error
	Flush(ctx context.Context) error
}

// StateStore is the seam for persisting harvesting checkpoints.
type StateStore interface {
	LastSHA(repo string) (string, bool)
	SetLastSHA(repo, sha string) error
}

// Deps encapsulates harvester dependencies.
type Deps struct {
	State StateStore
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
