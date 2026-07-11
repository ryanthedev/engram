package harvester_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/harvester"
	"github.com/ryanthedev/engram/internal/mcp"
)

type dummySource struct {
	cfg harvester.SourceConfig
}

func (d *dummySource) Type() string                { return "dummy-source" }
func (d *dummySource) Mode() harvester.HarvestMode { return harvester.FullHarvest }
func (d *dummySource) Harvest(ctx context.Context, sink harvester.Sink) error {
	return nil
}

func init() {
	harvester.Register("dummy-source", func(cfg harvester.SourceConfig, deps harvester.Deps) (harvester.Source, error) {
		return &dummySource{cfg: cfg}, nil
	})
}

type fakeEngramClient struct {
	collectionsCalled bool
	collections       []mcp.CollectionInfo
	err               error
}

func (f *fakeEngramClient) Collections(ctx context.Context) ([]mcp.CollectionInfo, error) {
	f.collectionsCalled = true
	return f.collections, f.err
}

func (f *fakeEngramClient) Ingest(ctx context.Context, collection, source, harvestID string, docs []mcp.KnowledgeDoc) (int, error) {
	return 0, nil
}

func (f *fakeEngramClient) Delete(ctx context.Context, collection, source, currentHarvestID string) (int, error) {
	return 0, nil
}

func TestDW_1_1_ValidManifestRoundTripAndValidate(t *testing.T) {
	yamlData := `
collections:
  - name: arxiv
    sources:
      - { type: dummy-source, path: arxiv.json.gz }
`
	m, err := harvester.LoadManifest([]byte(yamlData))
	if err != nil {
		t.Fatalf("LoadManifest failed: %v", err)
	}

	if len(m.Collections) != 1 {
		t.Errorf("expected 1 collection, got %d", len(m.Collections))
	}
	col := m.Collections[0]
	if col.Name != "arxiv" {
		t.Errorf("expected collection name 'arxiv', got %q", col.Name)
	}
	if len(col.Sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(col.Sources))
	}
	src := col.Sources[0]
	if src.Type != "dummy-source" {
		t.Errorf("expected source type 'dummy-source', got %q", src.Type)
	}
	if src.Raw["path"] != "arxiv.json.gz" {
		t.Errorf("expected raw path 'arxiv.json.gz', got %v", src.Raw["path"])
	}

	ec := &fakeEngramClient{
		collections: []mcp.CollectionInfo{
			{CollectionSpec: mcp.CollectionSpec{Name: "arxiv"}},
		},
	}

	if err := m.Validate(context.Background(), ec); err != nil {
		t.Errorf("expected Validate to pass, got error: %v", err)
	}
}

func TestDW_1_2_ValidateRejections(t *testing.T) {
	tests := []struct {
		name              string
		yamlData          string
		collections       []mcp.CollectionInfo
		wantErr           error
		wantCalledNetwork bool
		errSubstring      string
	}{
		{
			name: "unregistered collection",
			yamlData: `
collections:
  - name: unregistered-col
    sources:
      - { type: dummy-source }
`,
			collections:       []mcp.CollectionInfo{{CollectionSpec: mcp.CollectionSpec{Name: "arxiv"}}},
			wantErr:           harvester.ErrUnknownCollection,
			wantCalledNetwork: true,
			errSubstring:      "unregistered-col",
		},
		{
			name: "unknown source type",
			yamlData: `
collections:
  - name: arxiv
    sources:
      - { type: unknown-type }
`,
			collections:       []mcp.CollectionInfo{{CollectionSpec: mcp.CollectionSpec{Name: "arxiv"}}},
			wantErr:           harvester.ErrUnknownSourceType,
			wantCalledNetwork: false, // rejected before network
			errSubstring:      "unknown-type",
		},
		{
			name: "invalid collection name",
			yamlData: `
collections:
  - name: InvalidCollectionName!
    sources:
      - { type: dummy-source }
`,
			collections:       []mcp.CollectionInfo{{CollectionSpec: mcp.CollectionSpec{Name: "arxiv"}}},
			wantErr:           harvester.ErrInvalidName,
			wantCalledNetwork: false, // rejected before network
			errSubstring:      "InvalidCollectionName!",
		},
		{
			name: "invalid source type name",
			yamlData: `
collections:
  - name: arxiv
    sources:
      - { type: invalid_type! }
`,
			collections:       []mcp.CollectionInfo{{CollectionSpec: mcp.CollectionSpec{Name: "arxiv"}}},
			wantErr:           harvester.ErrInvalidName,
			wantCalledNetwork: false, // rejected before network
			errSubstring:      "invalid_type!",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := harvester.LoadManifest([]byte(tt.yamlData))
			if err != nil {
				t.Fatalf("LoadManifest failed: %v", err)
			}

			ec := &fakeEngramClient{collections: tt.collections}
			err = m.Validate(context.Background(), ec)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate() error = %v, want error matching %v", err, tt.wantErr)
			}

			if ec.collectionsCalled != tt.wantCalledNetwork {
				t.Errorf("Validate() network call status = %v, want %v", ec.collectionsCalled, tt.wantCalledNetwork)
			}

			if err != nil && tt.errSubstring != "" && !strings.Contains(err.Error(), tt.errSubstring) {
				t.Errorf("Validate() error msg %q does not contain substring %q", err.Error(), tt.errSubstring)
			}
		})
	}
}

func TestEdge_LoadManifestErrors(t *testing.T) {
	tests := []struct {
		name     string
		yamlData string
	}{
		{"empty YAML", ""},
		{"malformed YAML", "collections:\n  - name: arxiv\n  - : : :"},
		{"duplicate collection name", `
collections:
  - name: arxiv
    sources: []
  - name: arxiv
    sources: []
`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := harvester.LoadManifest([]byte(tt.yamlData))
			if err == nil {
				t.Error("expected error but got nil")
			}
		})
	}
}
