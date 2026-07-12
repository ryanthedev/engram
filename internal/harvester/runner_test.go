package harvester_test

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/ryanthedev/engram/internal/harvester"
	"github.com/ryanthedev/engram/internal/mcp"
)

type testSource struct {
	typ  string
	mode harvester.HarvestMode
	docs []mcp.KnowledgeDoc
	err  error
}

func (s *testSource) Type() string {
	return s.typ
}

func (s *testSource) Mode() harvester.HarvestMode {
	return s.mode
}

func (s *testSource) Harvest(ctx context.Context, sink harvester.Sink) error {
	if s.err != nil {
		return s.err
	}
	for _, doc := range s.docs {
		if err := sink.Add(doc); err != nil {
			return err
		}
	}
	return nil
}

// testScopedSource is a ScopedSource fake: it partitions docs by scope and can
// fail on a chosen scope to exercise the run-wide fail-safe.
type testScopedSource struct {
	typ        string
	mode       harvester.HarvestMode
	scopes     []string
	docs       map[string][]mcp.KnowledgeDoc
	err        error
	errOnScope string // if set, err fires only for this scope; else for all
}

var _ harvester.ScopedSource = (*testScopedSource)(nil)

func (s *testScopedSource) Type() string                { return s.typ }
func (s *testScopedSource) Mode() harvester.HarvestMode { return s.mode }
func (s *testScopedSource) SweepScopes() []string       { return s.scopes }

func (s *testScopedSource) Harvest(ctx context.Context, sink harvester.Sink) error {
	for _, scope := range s.scopes {
		if err := s.HarvestScope(ctx, scope, sink); err != nil {
			return err
		}
	}
	return nil
}

func (s *testScopedSource) HarvestScope(ctx context.Context, scope string, sink harvester.Sink) error {
	if s.err != nil && (s.errOnScope == "" || s.errOnScope == scope) {
		return s.err
	}
	for _, doc := range s.docs[scope] {
		if err := sink.Add(doc); err != nil {
			return err
		}
	}
	return nil
}

func TestRunner_Run(t *testing.T) {
	t.Run("DW-2.1: Run batches N docs into Ingest", func(t *testing.T) {
		ec := &testEngramClient{}
		runner := harvester.NewRunner(ec, 500, nil)

		docs := make([]mcp.KnowledgeDoc, 1200)
		for i := 0; i < 1200; i++ {
			docs[i] = mcp.KnowledgeDoc{ID: string(rune(i)), Text: "doc content"}
		}
		src := &testSource{
			typ:  "test-source",
			mode: harvester.Incremental,
			docs: docs,
		}

		report, err := runner.Run(context.Background(), "my-collection", src)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}

		if report.Indexed != 1200 {
			t.Errorf("expected 1200 indexed docs, got %d", report.Indexed)
		}

		// N=1200 with batchSize=500 -> 3 Ingest calls (500, 500, 200)
		if len(ec.ingestCalls) != 3 {
			t.Fatalf("expected 3 Ingest calls, got %d", len(ec.ingestCalls))
		}

		expectedBatches := []int{500, 500, 200}
		for i, sz := range expectedBatches {
			if len(ec.ingestCalls[i].docs) != sz {
				t.Errorf("call %d: expected batch size %d, got %d", i, sz, len(ec.ingestCalls[i].docs))
			}
			if ec.ingestCalls[i].collection != "my-collection" {
				t.Errorf("call %d: expected collection 'my-collection', got %q", i, ec.ingestCalls[i].collection)
			}
			if ec.ingestCalls[i].source != "test-source" {
				t.Errorf("call %d: expected source 'test-source', got %q", i, ec.ingestCalls[i].source)
			}
			if ec.ingestCalls[i].harvestID != report.HarvestID {
				t.Errorf("call %d: expected harvest ID %q, got %q", i, report.HarvestID, ec.ingestCalls[i].harvestID)
			}
		}
	})

	t.Run("DW-2.2: clean FullHarvest triggers Delete sweep", func(t *testing.T) {
		ec := &testEngramClient{deleteCount: 42}
		runner := harvester.NewRunner(ec, 10, nil)

		src := &testSource{
			typ:  "test-source",
			mode: harvester.FullHarvest,
			docs: []mcp.KnowledgeDoc{{ID: "d1"}},
		}

		report, err := runner.Run(context.Background(), "my-collection", src)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}

		if report.Deleted != 42 {
			t.Errorf("expected 42 deleted docs, got %d", report.Deleted)
		}

		if len(ec.deleteCalls) != 1 {
			t.Fatalf("expected exactly 1 Delete call, got %d", len(ec.deleteCalls))
		}

		dc := ec.deleteCalls[0]
		if dc.collection != "my-collection" {
			t.Errorf("expected collection 'my-collection', got %q", dc.collection)
		}
		if dc.source != "test-source" {
			t.Errorf("expected source 'test-source', got %q", dc.source)
		}
		if dc.harvestID != report.HarvestID {
			t.Errorf("expected harvest ID %q, got %q", report.HarvestID, dc.harvestID)
		}
	})

	t.Run("DW-2.2: clean Incremental harvest triggers no Delete sweep", func(t *testing.T) {
		ec := &testEngramClient{}
		runner := harvester.NewRunner(ec, 10, nil)

		src := &testSource{
			typ:  "test-source",
			mode: harvester.Incremental,
			docs: []mcp.KnowledgeDoc{{ID: "d1"}},
		}

		_, err := runner.Run(context.Background(), "my-collection", src)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}

		if len(ec.deleteCalls) != 0 {
			t.Errorf("expected 0 Delete calls for Incremental source, got %d", len(ec.deleteCalls))
		}
	})

	t.Run("DW-2.2: Harvest error returns error and suppresses Delete", func(t *testing.T) {
		ec := &testEngramClient{}
		runner := harvester.NewRunner(ec, 10, nil)

		src := &testSource{
			typ:  "test-source",
			mode: harvester.FullHarvest,
			err:  errors.New("harvest error"),
		}

		_, err := runner.Run(context.Background(), "my-collection", src)
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		if len(ec.deleteCalls) != 0 {
			t.Errorf("expected 0 Delete calls on harvest error, got %d", len(ec.deleteCalls))
		}
	})

	t.Run("DW-2.2: Flush error returns error and suppresses Delete", func(t *testing.T) {
		ec := &testEngramClient{
			ingestErr: errors.New("ingest error on flush"),
		}
		runner := harvester.NewRunner(ec, 10, nil)

		src := &testSource{
			typ:  "test-source",
			mode: harvester.FullHarvest,
			docs: []mcp.KnowledgeDoc{{ID: "d1"}}, // will be flushed at the end
		}

		_, err := runner.Run(context.Background(), "my-collection", src)
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		if len(ec.deleteCalls) != 0 {
			t.Errorf("expected 0 Delete calls on flush error, got %d", len(ec.deleteCalls))
		}
	})

	t.Run("DW-2.3: Ingest error in Run returns error and suppresses Delete", func(t *testing.T) {
		ec := &testEngramClient{
			ingestErr: errors.New("ingest error"),
		}
		runner := harvester.NewRunner(ec, 1, nil) // batchSize 1 to trigger ingest immediately

		src := &testSource{
			typ:  "test-source",
			mode: harvester.FullHarvest,
			docs: []mcp.KnowledgeDoc{{ID: "d1"}},
		}

		_, err := runner.Run(context.Background(), "my-collection", src)
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		if len(ec.deleteCalls) != 0 {
			t.Errorf("expected 0 Delete calls on Ingest error, got %d", len(ec.deleteCalls))
		}
	})

	t.Run("DW-2.5: FullHarvest with zero docs suppresses Delete sweep", func(t *testing.T) {
		ec := &testEngramClient{}
		runner := harvester.NewRunner(ec, 10, nil)

		src := &testSource{
			typ:  "test-source",
			mode: harvester.FullHarvest,
			docs: nil,
		}

		report, err := runner.Run(context.Background(), "my-collection", src)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}

		if report.Deleted != 0 {
			t.Errorf("expected 0 deleted docs in report, got %d", report.Deleted)
		}

		if len(ec.deleteCalls) != 0 {
			t.Errorf("expected 0 Delete calls for zero-doc FullHarvest, got %d", len(ec.deleteCalls))
		}
	})

	t.Run("DW-2.6: consecutive runs produce unique, well-formed harvest IDs", func(t *testing.T) {
		ec := &testEngramClient{}
		runner := harvester.NewRunner(ec, 10, nil)

		src := &testSource{
			typ:  "my-type",
			mode: harvester.Incremental,
		}

		report1, err := runner.Run(context.Background(), "my-collection", src)
		if err != nil {
			t.Fatalf("Run 1 failed: %v", err)
		}

		report2, err := runner.Run(context.Background(), "my-collection", src)
		if err != nil {
			t.Fatalf("Run 2 failed: %v", err)
		}

		id1 := report1.HarvestID
		id2 := report2.HarvestID

		if id1 == id2 {
			t.Errorf("expected harvest IDs to be different, but both were %q", id1)
		}

		// Regex pattern to check RFC3339Nano followed by -<counter>#<type>
		// e.g. 2026-07-11T22:55:47.123456789Z-1#my-type or with local timezone (which has +/- offset instead of Z)
		// Let's use a robust pattern: matching timestamp with Z or timezone offset, a hyphen, digit(s), '#' and then type
		pattern := `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})-\d+#my-type$`
		matched1, err := regexp.MatchString(pattern, id1)
		if err != nil {
			t.Fatalf("regex match failed: %v", err)
		}
		if !matched1 {
			t.Errorf("harvest ID 1 %q does not match expected format %q", id1, pattern)
		}

		matched2, err := regexp.MatchString(pattern, id2)
		if err != nil {
			t.Fatalf("regex match failed: %v", err)
		}
		if !matched2 {
			t.Errorf("harvest ID 2 %q does not match expected format %q", id2, pattern)
		}
	})

	t.Run("scoped: each scope ingests and sweeps under its own source string", func(t *testing.T) {
		ec := &testEngramClient{}
		runner := harvester.NewRunner(ec, 500, nil)

		src := &testScopedSource{
			typ:    "multi",
			mode:   harvester.FullHarvest,
			scopes: []string{"multi:a", "multi:b"},
			docs: map[string][]mcp.KnowledgeDoc{
				"multi:a": {{ID: "a1"}, {ID: "a2"}},
				"multi:b": {{ID: "b1"}},
			},
		}

		report, err := runner.Run(context.Background(), "col", src)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}
		if report.Indexed != 3 {
			t.Errorf("expected 3 indexed, got %d", report.Indexed)
		}

		// Each scope's docs ingested under that scope's source string.
		gotSources := map[string]int{}
		for _, ic := range ec.ingestCalls {
			gotSources[ic.source] += len(ic.docs)
		}
		if gotSources["multi:a"] != 2 || gotSources["multi:b"] != 1 {
			t.Errorf("expected per-scope ingest {multi:a:2, multi:b:1}, got %v", gotSources)
		}

		// Each config scope swept exactly once, under its own source string.
		sweptScopes := map[string]int{}
		for _, dc := range ec.deleteCalls {
			if dc.harvestID != report.HarvestID {
				t.Errorf("delete used harvestID %q, want %q", dc.harvestID, report.HarvestID)
			}
			sweptScopes[dc.source]++
		}
		if sweptScopes["multi:a"] != 1 || sweptScopes["multi:b"] != 1 || len(ec.deleteCalls) != 2 {
			t.Errorf("expected each scope swept once, got %v (%d calls)", sweptScopes, len(ec.deleteCalls))
		}
	})

	t.Run("scoped: zero-doc scope is still swept (config-derived scope)", func(t *testing.T) {
		ec := &testEngramClient{}
		runner := harvester.NewRunner(ec, 500, nil)

		src := &testScopedSource{
			typ:    "multi",
			mode:   harvester.FullHarvest,
			scopes: []string{"multi:a", "multi:empty"},
			docs: map[string][]mcp.KnowledgeDoc{
				"multi:a": {{ID: "a1"}},
				// multi:empty emits nothing this run
			},
		}

		_, err := runner.Run(context.Background(), "col", src)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}

		swept := map[string]bool{}
		for _, dc := range ec.deleteCalls {
			swept[dc.source] = true
		}
		if !swept["multi:empty"] {
			t.Errorf("expected zero-doc scope multi:empty to still be swept; delete calls: %v", ec.deleteCalls)
		}
		if !swept["multi:a"] {
			t.Errorf("expected scope multi:a to be swept; delete calls: %v", ec.deleteCalls)
		}
	})

	t.Run("scoped: A-survives-B — separate runs sweep only their own scope", func(t *testing.T) {
		ec := &testEngramClient{}
		runner := harvester.NewRunner(ec, 500, nil)

		runA := &testScopedSource{typ: "multi", mode: harvester.FullHarvest,
			scopes: []string{"multi:a"}, docs: map[string][]mcp.KnowledgeDoc{"multi:a": {{ID: "a1"}}}}
		runB := &testScopedSource{typ: "multi", mode: harvester.FullHarvest,
			scopes: []string{"multi:b"}, docs: map[string][]mcp.KnowledgeDoc{"multi:b": {{ID: "b1"}}}}

		if _, err := runner.Run(context.Background(), "col", runA); err != nil {
			t.Fatalf("run A failed: %v", err)
		}
		if _, err := runner.Run(context.Background(), "col", runB); err != nil {
			t.Fatalf("run B failed: %v", err)
		}

		// No delete call ever targeted the OTHER run's scope: run A swept only
		// multi:a, run B swept only multi:b — so B's run can never delete A's docs.
		for _, dc := range ec.deleteCalls {
			if dc.source != "multi:a" && dc.source != "multi:b" {
				t.Errorf("unexpected sweep scope %q", dc.source)
			}
		}
		var sweptA, sweptB int
		for _, dc := range ec.deleteCalls {
			switch dc.source {
			case "multi:a":
				sweptA++
			case "multi:b":
				sweptB++
			}
		}
		if sweptA != 1 || sweptB != 1 {
			t.Errorf("expected exactly one sweep per scope across the two runs, got a=%d b=%d", sweptA, sweptB)
		}
	})

	t.Run("scoped: HarvestScope error aborts before ANY sweep (fail-safe)", func(t *testing.T) {
		ec := &testEngramClient{}
		runner := harvester.NewRunner(ec, 500, nil)

		src := &testScopedSource{
			typ:        "multi",
			mode:       harvester.FullHarvest,
			scopes:     []string{"multi:a", "multi:b"},
			docs:       map[string][]mcp.KnowledgeDoc{"multi:a": {{ID: "a1"}}},
			err:        errors.New("scope harvest boom"),
			errOnScope: "multi:b",
		}

		_, err := runner.Run(context.Background(), "col", src)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if len(ec.deleteCalls) != 0 {
			t.Errorf("expected NO sweep on scope error, got %d delete calls", len(ec.deleteCalls))
		}
	})

	t.Run("scoped: Incremental scoped source never sweeps", func(t *testing.T) {
		ec := &testEngramClient{}
		runner := harvester.NewRunner(ec, 500, nil)

		src := &testScopedSource{
			typ:    "multi",
			mode:   harvester.Incremental,
			scopes: []string{"multi:a"},
			docs:   map[string][]mcp.KnowledgeDoc{"multi:a": {{ID: "a1"}}},
		}

		if _, err := runner.Run(context.Background(), "col", src); err != nil {
			t.Fatalf("Run failed: %v", err)
		}
		if len(ec.deleteCalls) != 0 {
			t.Errorf("expected 0 sweeps for Incremental scoped source, got %d", len(ec.deleteCalls))
		}
	})

	t.Run("boundary: Collections helper delegates to Client", func(t *testing.T) {
		expectedInfos := []mcp.CollectionInfo{
			{Count: 100},
		}
		ec := &testEngramClient{
			collectionsResult: expectedInfos,
		}
		runner := harvester.NewRunner(ec, 10, nil)

		infos, err := runner.Collections(context.Background())
		if err != nil {
			t.Fatalf("Collections failed: %v", err)
		}

		if len(infos) != 1 || infos[0].Count != 100 {
			t.Errorf("expected collection count 100, got %v", infos)
		}
	})
}
