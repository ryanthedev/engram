package harvester_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/harvester"
	"github.com/ryanthedev/engram/internal/mcp"
)

// Assert fake types satisfy interfaces at compile-time.
var _ harvester.Source = (*fakeSource)(nil)
var _ harvester.Sink = (*fakeSink)(nil)

type fakeSource struct {
	typ  string
	mode harvester.HarvestMode
}

func (f *fakeSource) Type() string                                           { return f.typ }
func (f *fakeSource) Mode() harvester.HarvestMode                            { return f.mode }
func (f *fakeSource) Harvest(ctx context.Context, sink harvester.Sink) error { return nil }

type fakeSink struct {
	added   []mcp.KnowledgeDoc
	flushed bool
}

func (f *fakeSink) Add(doc mcp.KnowledgeDoc) error {
	f.added = append(f.added, doc)
	return nil
}

func (f *fakeSink) Flush(ctx context.Context) error {
	f.flushed = true
	return nil
}

func TestDW_1_3_SourceBuild(t *testing.T) {
	// Register a source builder.
	harvester.Register("test-source", func(cfg harvester.SourceConfig, deps harvester.Deps) (harvester.Source, error) {
		return &fakeSource{typ: "test-source", mode: harvester.Incremental}, nil
	})

	deps := harvester.Deps{
		Logger: slog.Default(),
	}

	cfg := harvester.SourceConfig{
		Type: "test-source",
		Raw:  map[string]any{"some": "param"},
	}

	src, err := harvester.Build(cfg, deps)
	if err != nil {
		t.Fatalf("Build failed for registered source: %v", err)
	}

	if src.Type() != "test-source" {
		t.Errorf("expected source type 'test-source', got %q", src.Type())
	}
	if src.Mode() != harvester.Incremental {
		t.Errorf("expected source mode to be Incremental, got %v", src.Mode())
	}

	// Test building unregistered source type.
	unregisteredCfg := harvester.SourceConfig{
		Type: "non-existent-source",
	}

	_, err = harvester.Build(unregisteredCfg, deps)
	if !errors.Is(err, harvester.ErrUnknownSourceType) {
		t.Errorf("expected error matching ErrUnknownSourceType, got %v", err)
	}

	// Verify the error text contains registered types.
	if err != nil && !strings.Contains(err.Error(), "test-source") {
		t.Errorf("expected error message to list registered types (e.g. 'test-source'), got %q", err.Error())
	}
}

func TestRegisteredTypes(t *testing.T) {
	types := harvester.RegisteredTypes()
	foundDummy := false
	foundTest := false
	for _, ty := range types {
		if ty == "dummy-source" {
			foundDummy = true
		}
		if ty == "test-source" {
			foundTest = true
		}
	}
	if !foundDummy {
		t.Errorf("expected 'dummy-source' to be in registered types list %v", types)
	}
	if !foundTest {
		t.Errorf("expected 'test-source' to be in registered types list %v", types)
	}
}
