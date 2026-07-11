package harvester_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/harvester"
	"github.com/ryanthedev/engram/internal/mcp"
)

func init() {
	harvester.Register("fake-source-ok", func(cfg harvester.SourceConfig, deps harvester.Deps) (harvester.Source, error) {
		return &testSource{
			typ:  "fake-source-ok",
			mode: harvester.Incremental,
			docs: []mcp.KnowledgeDoc{{ID: "doc-ok", Text: "hello ok"}},
		}, nil
	})

	harvester.Register("fake-source-fail", func(cfg harvester.SourceConfig, deps harvester.Deps) (harvester.Source, error) {
		return &testSource{
			typ:  "fake-source-fail",
			mode: harvester.Incremental,
			err:  errors.New("mock harvest failure"),
		}, nil
	})
}

func TestRun_CoreOrchestration(t *testing.T) {
	t.Run("DW-6.1: Run success and failure aggregation", func(t *testing.T) {
		ec := &testEngramClient{
			collectionsResult: []mcp.CollectionInfo{
				{CollectionSpec: mcp.CollectionSpec{Name: "col-ok"}},
				{CollectionSpec: mcp.CollectionSpec{Name: "col-mixed"}},
			},
		}

		manifest := harvester.Manifest{
			Collections: []harvester.CollectionManifest{
				{
					Name: "col-ok",
					Sources: []harvester.SourceConfig{
						{Type: "fake-source-ok"},
					},
				},
				{
					Name: "col-mixed",
					Sources: []harvester.SourceConfig{
						{Type: "fake-source-fail"},
						{Type: "fake-source-ok"},
					},
				},
			},
		}

		opts := harvester.RunOptions{
			BatchSize: 10,
		}

		indexed, deleted, err := harvester.Run(context.Background(), ec, manifest, opts)

		// Expecting error because fake-source-fail failed.
		if err == nil {
			t.Fatal("expected aggregated error, got nil")
		}

		// Ensure both successful sources ran despite the failure in between
		// col-ok fake-source-ok should produce 1 ingest call
		// col-mixed fake-source-ok should produce 1 ingest call
		// col-mixed fake-source-fail should produce 0 ingest calls (it fails)
		if len(ec.ingestCalls) != 2 {
			t.Errorf("expected 2 successful ingest calls, got %d", len(ec.ingestCalls))
		}

		// Check the indexed documents count
		// Each fake-source-ok produces 1 document, so total indexed should be 2.
		if indexed != 2 {
			t.Errorf("expected 2 indexed documents, got %d", indexed)
		}

		if deleted != 0 {
			t.Errorf("expected 0 deleted documents, got %d", deleted)
		}

		// Check that the error message mentions the failure
		errMsg := err.Error()
		if !strings.Contains(errMsg, "mock harvest failure") {
			t.Errorf("expected error message to contain 'mock harvest failure', got %q", errMsg)
		}
	})

	t.Run("Manifest validation failure runs no harvest", func(t *testing.T) {
		ec := &testEngramClient{
			collectionsResult: []mcp.CollectionInfo{
				{CollectionSpec: mcp.CollectionSpec{Name: "col-exists"}},
			},
		}

		manifest := harvester.Manifest{
			Collections: []harvester.CollectionManifest{
				{
					Name: "col-unregistered-on-server",
					Sources: []harvester.SourceConfig{
						{Type: "fake-source-ok"},
					},
				},
			},
		}

		opts := harvester.RunOptions{
			BatchSize: 10,
		}

		_, _, err := harvester.Run(context.Background(), ec, manifest, opts)
		if err == nil {
			t.Fatal("expected manifest validation error, got nil")
		}

		if len(ec.ingestCalls) != 0 {
			t.Errorf("expected 0 ingest calls due to validation failure, got %d", len(ec.ingestCalls))
		}
	})
}

func TestRun_FiltersAndValidation(t *testing.T) {
	manifest := harvester.Manifest{
		Collections: []harvester.CollectionManifest{
			{
				Name: "col1",
				Sources: []harvester.SourceConfig{
					{Type: "fake-source-ok"},
				},
			},
		},
	}

	t.Run("DW-6.3: Unknown collection filter returns error naming valid options", func(t *testing.T) {
		err := harvester.ValidateFilters(manifest, []string{"invalid-col"}, nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "invalid-col") || !strings.Contains(err.Error(), "col1") {
			t.Errorf("expected error to name the invalid and valid collection options, got: %v", err)
		}
	})

	t.Run("DW-6.3: Unknown source filter returns error naming valid options", func(t *testing.T) {
		err := harvester.ValidateFilters(manifest, nil, []string{"invalid-source"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "invalid-source") || !strings.Contains(err.Error(), "fake-source-ok") {
			t.Errorf("expected error to name the invalid and valid source options, got: %v", err)
		}
	})

	t.Run("Valid filters pass without error", func(t *testing.T) {
		err := harvester.ValidateFilters(manifest, []string{"col1"}, []string{"fake-source-ok"})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})
}
