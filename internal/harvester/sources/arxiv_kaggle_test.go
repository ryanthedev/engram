package sources_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/harvester"
	_ "github.com/ryanthedev/engram/internal/harvester/sources"
	"github.com/ryanthedev/engram/internal/mcp"
)

// Assert: No PDF/full-text fetching is executed in Kaggle source tests.

type fakeSink struct {
	docs []mcp.KnowledgeDoc
}

func (f *fakeSink) Add(doc mcp.KnowledgeDoc) error {
	f.docs = append(f.docs, doc)
	return nil
}

func (f *fakeSink) Flush(ctx context.Context) error {
	return nil
}

func createTempGzipFile(t *testing.T, content []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "arxiv-kaggle-dump.json.gz")

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	if _, err := gz.Write(content); err != nil {
		t.Fatalf("failed to write content: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close gzip: %v", err)
	}
	return path
}

func TestDW_3_1_KaggleHarvestSuccess(t *testing.T) {
	lines := []string{
		// Match: cs.CL, should be kept
		`{"id":"0704.0001","title":"An Amazing CS Title\n with newlines","abstract":"This is CS abstract.","categories":"cs.CL hep-ph","doi":"10.1234/5678","journal-ref":"Journal of CS Clones","comments":"","update_date":"2008-11-26","versions":[{"version":"v1","created":"Mon, 2 Apr 2007 19:18:42 GMT"}],"authors_parsed":[["Last", "First", ""]]}`,
		// Exclude: hep-ph only, should be skipped
		`{"id":"0704.0002","title":"A Physics Title","abstract":"This is Physics abstract.","categories":"hep-ph","doi":"10.1234/5679","journal-ref":"","comments":"","update_date":"2008-11-26","versions":[{"version":"v1","created":"Tue, 3 Apr 2007 19:18:42 GMT"}],"authors_parsed":[["Last2", "First2", "Jr."]]}`,
	}
	content := []byte(strings.Join(lines, "\n"))
	path := createTempGzipFile(t, content)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := harvester.Deps{Logger: logger}

	cfg := harvester.SourceConfig{
		Type: "arxiv-kaggle",
		Raw: map[string]any{
			"path":      path,
			"dump_date": "2026-07-11",
		},
	}

	src, err := harvester.Build(cfg, deps)
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}

	if src.Type() != "arxiv-kaggle" {
		t.Errorf("expected Type() 'arxiv-kaggle', got %q", src.Type())
	}
	if src.Mode() != harvester.FullHarvest {
		t.Errorf("expected Mode() FullHarvest, got %v", src.Mode())
	}

	sink := &fakeSink{}
	ctx := context.Background()

	if err := src.Harvest(ctx, sink); err != nil {
		t.Fatalf("Harvest failed: %v", err)
	}

	if len(sink.docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(sink.docs))
	}

	doc := sink.docs[0]
	if doc.ID != "0704.0001" {
		t.Errorf("expected ID '0704.0001', got %q", doc.ID)
	}
	if doc.Title != "An Amazing CS Title with newlines" {
		t.Errorf("expected whitespace-normalized Title, got %q", doc.Title)
	}
	if doc.Text != "This is CS abstract." {
		t.Errorf("expected Text 'This is CS abstract.', got %q", doc.Text)
	}
	if doc.SourceVersion != "dump:2026-07-11" {
		t.Errorf("expected SourceVersion 'dump:2026-07-11', got %q", doc.SourceVersion)
	}

	categories, ok := doc.Fields["categories"].([]any)
	if !ok || len(categories) != 2 || categories[0] != "cs.CL" {
		t.Errorf("unexpected categories: %v", doc.Fields["categories"])
	}
	if doc.Fields["doi"] != "10.1234/5678" {
		t.Errorf("expected doi, got %v", doc.Fields["doi"])
	}
	if doc.Fields["authors"] != "First Last" {
		t.Errorf("expected authors 'First Last', got %v", doc.Fields["authors"])
	}
	if doc.Fields["published_date"] != "Mon, 2 Apr 2007 19:18:42 GMT" {
		t.Errorf("expected published_date, got %v", doc.Fields["published_date"])
	}
	// Assert no empty fields are present in the map
	if _, present := doc.Fields["comments"]; present {
		t.Errorf("expected comments to be omitted, but was present")
	}
}

func TestDW_3_5_KaggleMalformedAndCorrupt(t *testing.T) {
	t.Run("malformed JSON line skipped and logged", func(t *testing.T) {
		lines := []string{
			`{"id":"0704.0001","title":"CS Title","abstract":"Abstract","categories":"cs.CL","versions":[{"version":"v1","created":"Mon"}],"authors_parsed":[]}`,
			`{ malformed json line }`,
			`{"id":"0704.0003","title":"CS Title 3","abstract":"Abstract 3","categories":"cs.CV","versions":[{"version":"v1","created":"Tue"}],"authors_parsed":[]}`,
		}
		content := []byte(strings.Join(lines, "\n"))
		path := createTempGzipFile(t, content)

		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, nil))
		deps := harvester.Deps{Logger: logger}

		cfg := harvester.SourceConfig{
			Type: "arxiv-kaggle",
			Raw: map[string]any{
				"path":      path,
				"dump_date": "2026-07-11",
			},
		}

		src, err := harvester.Build(cfg, deps)
		if err != nil {
			t.Fatalf("failed to build source: %v", err)
		}

		sink := &fakeSink{}
		if err := src.Harvest(context.Background(), sink); err != nil {
			t.Fatalf("Harvest failed unexpectedly: %v", err)
		}

		if len(sink.docs) != 2 {
			t.Errorf("expected 2 docs, got %d", len(sink.docs))
		}

		logOutput := logBuf.String()
		if !strings.Contains(logOutput, "skipping malformed JSON line") {
			t.Errorf("expected warning log about malformed JSON line, log output: %q", logOutput)
		}
	})

	t.Run("corrupt gzip structure aborts", func(t *testing.T) {
		// Not gzipped content
		content := []byte("plain text that is not a valid gzip format")
		dir := t.TempDir()
		path := filepath.Join(dir, "corrupt.json.gz")
		if err := os.WriteFile(path, content, 0644); err != nil {
			t.Fatalf("failed to write corrupt file: %v", err)
		}

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		deps := harvester.Deps{Logger: logger}

		cfg := harvester.SourceConfig{
			Type: "arxiv-kaggle",
			Raw:  map[string]any{"path": path},
		}

		src, err := harvester.Build(cfg, deps)
		if err != nil {
			t.Fatalf("failed to build source: %v", err)
		}

		sink := &fakeSink{}
		err = src.Harvest(context.Background(), sink)
		if err == nil {
			t.Fatal("expected Harvest to fail due to gzip corruption, but it succeeded")
		}
		if !strings.Contains(err.Error(), "gzip") && !strings.Contains(err.Error(), "reading dump stream") {
			t.Errorf("expected error message to mention gzip, got: %v", err)
		}
	})
}
