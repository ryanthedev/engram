package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/server"
	"github.com/ryanthedev/engram/internal/store"
)

// fakeStore is a store.Store test double: Append is configurable, the rest
// are unused by these tests and return zero values.
type fakeStore struct {
	appendFn func(context.Context, memory.Episodic) (string, error)
}

func (f *fakeStore) Append(ctx context.Context, rec memory.Episodic) (string, error) {
	return f.appendFn(ctx, rec)
}
func (f *fakeStore) Create(context.Context, string, memory.SemanticFact) error { return nil }
func (f *fakeStore) Update(context.Context, string, memory.SemanticFact, int64, int64) error {
	return nil
}
func (f *fakeStore) ClaimBatch(context.Context, int, time.Duration) ([]memory.Episodic, error) {
	return nil, nil
}
func (f *fakeStore) Complete(context.Context, string) error           { return nil }
func (f *fakeStore) DeadLetter(context.Context, string, string) error { return nil }
func (f *fakeStore) ClaimLedger(context.Context, memory.LedgerKey) (store.LedgerEntry, error) {
	return store.LedgerEntry{}, nil
}
func (f *fakeStore) UpdateLedger(context.Context, memory.LedgerKey, store.LedgerState) error {
	return nil
}
func (f *fakeStore) ScanIncomplete(context.Context) ([]store.LedgerEntry, error) { return nil, nil }

var _ store.Store = (*fakeStore)(nil)

// fakeRetriever is a retrieval.Retriever test double.
type fakeRetriever struct {
	searchFn func(context.Context, retrieval.Query, retrieval.Filter) ([]retrieval.Hit, error)
}

func (f *fakeRetriever) Search(ctx context.Context, q retrieval.Query, filt retrieval.Filter) ([]retrieval.Hit, error) {
	return f.searchFn(ctx, q, filt)
}

var _ retrieval.Retriever = (*fakeRetriever)(nil)

// TestIngestRequiresEventID covers the proto3-can't-express-required
// contract: an empty event_id is rejected with INVALID_ARGUMENT before the
// store is ever touched.
func TestIngestRequiresEventID(t *testing.T) {
	called := false
	s := server.New(&fakeStore{appendFn: func(context.Context, memory.Episodic) (string, error) {
		called = true
		return "id", nil
	}}, nil)

	_, err := s.Ingest(context.Background(), &engrampb.IngestRequest{Text: "no event id"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Ingest without event_id: code = %v, want InvalidArgument", status.Code(err))
	}
	if called {
		t.Error("Store.Append was called despite the missing event_id")
	}
}

// TestIngestBuildsEpisodicAndReturnsID covers DW-1.1's write half: the
// request maps onto a full memory.Episodic (tenancy fields, kind, text,
// occurred_at from the client timestamp), and the store's returned id comes
// back verbatim.
func TestIngestBuildsEpisodicAndReturnsID(t *testing.T) {
	occurred := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	var captured memory.Episodic
	s := server.New(&fakeStore{appendFn: func(_ context.Context, rec memory.Episodic) (string, error) {
		captured = rec
		return "durable-id-1", nil
	}}, nil)

	resp, err := s.Ingest(context.Background(), &engrampb.IngestRequest{
		EventId:      "ev-1",
		TenantId:     "t1",
		TeamId:       "team-a",
		Scope:        "team",
		OwnerAgentId: "agent-1",
		SourceIds:    []string{"src-1"},
		Kind:         "tool_result",
		Text:         "hello world",
		OccurredAt:   timestamppb.New(occurred),
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if resp.GetId() != "durable-id-1" {
		t.Errorf("IngestResponse.Id = %q, want durable-id-1", resp.GetId())
	}
	if captured.EventID != "ev-1" || captured.TenantID != "t1" || captured.TeamID != "team-a" ||
		captured.Scope != "team" || captured.OwnerAgentID != "agent-1" || captured.Kind != "tool_result" ||
		captured.Text != "hello world" || len(captured.SourceIDs) != 1 || captured.SourceIDs[0] != "src-1" {
		t.Errorf("Append received %+v, fields did not round-trip from the request", captured)
	}
	if !captured.OccurredAt.Equal(occurred) {
		t.Errorf("OccurredAt = %v, want %v", captured.OccurredAt, occurred)
	}
	if captured.CreatedAt.IsZero() {
		t.Error("CreatedAt was not stamped by the server")
	}
}

// TestIngestDefaultsOccurredAtToNow covers the no-client-timestamp case.
func TestIngestDefaultsOccurredAtToNow(t *testing.T) {
	var captured memory.Episodic
	s := server.New(&fakeStore{appendFn: func(_ context.Context, rec memory.Episodic) (string, error) {
		captured = rec
		return "id", nil
	}}, nil)
	before := time.Now().UTC()
	if _, err := s.Ingest(context.Background(), &engrampb.IngestRequest{EventId: "ev-1"}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if captured.OccurredAt.Before(before) || captured.OccurredAt.After(time.Now().UTC()) {
		t.Errorf("OccurredAt = %v, want stamped near server receipt time (%v)", captured.OccurredAt, before)
	}
}

// TestIngestPropagatesStoreError surfaces a write-path failure as an
// Internal gRPC error rather than swallowing it.
func TestIngestPropagatesStoreError(t *testing.T) {
	s := server.New(&fakeStore{appendFn: func(context.Context, memory.Episodic) (string, error) {
		return "", errors.New("cluster unreachable")
	}}, nil)
	_, err := s.Ingest(context.Background(), &engrampb.IngestRequest{EventId: "ev-1"})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
}

// TestSearchMapsQueryFilterAndHits covers DW-1.2/DW-1.4's request/response
// mapping: SearchRequest fields become the right Query/Filter, and Hits map
// back with fields_json carrying the stored document.
func TestSearchMapsQueryFilterAndHits(t *testing.T) {
	var gotQuery retrieval.Query
	var gotFilter retrieval.Filter
	s := server.New(nil, &fakeRetriever{searchFn: func(_ context.Context, q retrieval.Query, f retrieval.Filter) ([]retrieval.Hit, error) {
		gotQuery, gotFilter = q, f
		return []retrieval.Hit{{ID: "doc-1", Score: 0.42, Source: "semantic", Fields: map[string]any{"statement": "x"}}}, nil
	}})

	resp, err := s.Search(context.Background(), &engrampb.SearchRequest{
		Query: "orders-svc leak", K: 5, TenantId: "t1", UserId: "agent-9", ValidOnly: true,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotQuery.Text != "orders-svc leak" || gotQuery.K != 5 {
		t.Errorf("Query = %+v, want Text=orders-svc leak K=5", gotQuery)
	}
	if gotFilter.TenantID != "t1" || gotFilter.UserID != "agent-9" || !gotFilter.ValidOnly {
		t.Errorf("Filter = %+v, want TenantID=t1 UserID=agent-9 ValidOnly=true", gotFilter)
	}
	if len(resp.GetHits()) != 1 {
		t.Fatalf("got %d hits, want 1", len(resp.GetHits()))
	}
	h := resp.GetHits()[0]
	if h.GetId() != "doc-1" || h.GetScore() != 0.42 || h.GetSource() != "semantic" {
		t.Errorf("Hit = %+v, want id=doc-1 score=0.42 source=semantic", h)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(h.GetFieldsJson()), &fields); err != nil {
		t.Fatalf("fields_json did not parse: %v", err)
	}
	if fields["statement"] != "x" {
		t.Errorf("fields_json = %v, want statement=x", fields)
	}
}

// TestSearchPropagatesRetrieverError surfaces a read-path failure as an
// Internal gRPC error.
func TestSearchPropagatesRetrieverError(t *testing.T) {
	s := server.New(nil, &fakeRetriever{searchFn: func(context.Context, retrieval.Query, retrieval.Filter) ([]retrieval.Hit, error) {
		return nil, errors.New("cluster unreachable")
	}})
	_, err := s.Search(context.Background(), &engrampb.SearchRequest{Query: "x"})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
}

// TestSearchEmptyHits proves zero results map to an empty (not nil-panic)
// response.
func TestSearchEmptyHits(t *testing.T) {
	s := server.New(nil, &fakeRetriever{searchFn: func(context.Context, retrieval.Query, retrieval.Filter) ([]retrieval.Hit, error) {
		return nil, nil
	}})
	resp, err := s.Search(context.Background(), &engrampb.SearchRequest{Query: "x"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.GetHits()) != 0 {
		t.Errorf("got %d hits, want 0", len(resp.GetHits()))
	}
}
