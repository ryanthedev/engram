package server_test

// Phase-5 gRPC-barricade tests: the FLAT SearchRequest filter fields compile
// into retrieval's predicate form, a malformed filter is rejected before the
// retriever is touched, and the no-filter request is byte-for-byte the request
// this server served before filters existed.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/server"
)

// capturingRetriever records the Filter it was called with, and whether it was
// called at all — the barricade contract is that a rejected request never
// reaches it.
type capturingRetriever struct {
	calls  int
	filter retrieval.Filter
	query  retrieval.Query
	err    error
}

func (c *capturingRetriever) Search(_ context.Context, q retrieval.Query, f retrieval.Filter) ([]retrieval.Hit, error) {
	c.calls++
	c.query, c.filter = q, f
	return nil, c.err
}

// searchWith runs one Search against a capturing retriever and returns it.
func searchWith(t *testing.T, req *engrampb.SearchRequest) (*capturingRetriever, error) {
	t.Helper()
	r := &capturingRetriever{}
	_, err := server.New(nil, r).Search(context.Background(), req)
	return r, err
}

// --- DW-5.1 ------------------------------------------------------------

// TestDW_5_1_FlatParamsCompileToPredicates: every flat filter field becomes the
// right predicate. Field OWNERSHIP (which tier each reaches) is the retrieval
// registry's job and is asserted there; what this pins is the compile — the one
// place the flat wire form becomes the internal generic form.
func TestDW_5_1_FlatParamsCompileToPredicates(t *testing.T) {
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	r, err := searchWith(t, &engrampb.SearchRequest{
		Query:             "orders-svc leak",
		K:                 7,
		Kind:              "conversation",
		Subject:           "orders-svc",
		Predicate:         "owned_by",
		Object:            "team-a",
		ExtractorVersion:  "v3",
		Since:             timestamppb.New(since),
		Until:             timestamppb.New(until),
		IncludeSuperseded: true,
		Sources:           []string{"episodic", "semantic"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	want := []retrieval.Predicate{
		{Field: "extractor_version", Op: "term", Value: "v3"},
		{Field: "kind", Op: "term", Value: "conversation"},
		{Field: "object", Op: "term", Value: "team-a"},
		{Field: "predicate", Op: "term", Value: "owned_by"},
		{Field: "subject", Op: "term", Value: "orders-svc"},
		{Field: retrieval.TimeField, Op: "range", Value: map[string]any{
			"gte": "2026-01-01T00:00:00Z", "lte": "2026-06-01T00:00:00Z",
		}},
	}
	if !reflect.DeepEqual(r.filter.Predicates, want) {
		t.Errorf("predicates =\n %+v\nwant\n %+v", r.filter.Predicates, want)
	}
	if !reflect.DeepEqual(r.filter.Sources, []string{"episodic", "semantic"}) {
		t.Errorf("sources = %v", r.filter.Sources)
	}
	if r.query.Text != "orders-svc leak" || r.query.K != 7 {
		t.Errorf("query = %+v", r.query)
	}
}

// TestDW_5_1_TimeBoundsAreOneRangePredicate: since/until compile to a SINGLE
// predicate on the tier-neutral time field — not one predicate per physical
// field — so every tier is constrained by its own event-time field. Either bound
// alone is a legal open interval.
func TestDW_5_1_TimeBoundsAreOneRangePredicate(t *testing.T) {
	ts := timestamppb.New(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC))
	for _, tc := range []struct {
		name  string
		req   *engrampb.SearchRequest
		bound map[string]any
	}{
		{"since only", &engrampb.SearchRequest{Query: "q", Since: ts}, map[string]any{"gte": "2026-03-04T05:06:07Z"}},
		{"until only", &engrampb.SearchRequest{Query: "q", Until: ts}, map[string]any{"lte": "2026-03-04T05:06:07Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := searchWith(t, tc.req)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			want := []retrieval.Predicate{{Field: retrieval.TimeField, Op: "range", Value: tc.bound}}
			if !reflect.DeepEqual(r.filter.Predicates, want) {
				t.Errorf("predicates = %+v, want %+v", r.filter.Predicates, want)
			}
		})
	}
}

// --- DW-5.2 ------------------------------------------------------------

// TestDW_5_2_IncludeSupersededDrivesValidOnly: the server is the SOLE producer of
// ValidOnly, and it derives it from include_superseded. Absent (false) must
// reproduce the hardcoded ValidOnly:true the client used to send — that is the
// whole "no filters behaves like today" guarantee for the validity filter.
func TestDW_5_2_IncludeSupersededDrivesValidOnly(t *testing.T) {
	for _, tc := range []struct {
		name              string
		includeSuperseded bool
		wantValidOnly     bool
	}{
		{"absent: current facts only (today's behavior)", false, true},
		{"true: history visible", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := searchWith(t, &engrampb.SearchRequest{Query: "q", IncludeSuperseded: tc.includeSuperseded})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if r.filter.ValidOnly != tc.wantValidOnly {
				t.Errorf("ValidOnly = %v, want %v", r.filter.ValidOnly, tc.wantValidOnly)
			}
		})
	}
}

// --- DW-5.3 ------------------------------------------------------------

// TestDW_5_3_SourcesReachTheFilter: sources travel to retrieval.Filter, which is
// where the episodic tier and graph post-hook are actually skipped (asserted in
// internal/retrieval). The server must not second-guess the vocabulary — the
// registry owns it.
func TestDW_5_3_SourcesReachTheFilter(t *testing.T) {
	r, err := searchWith(t, &engrampb.SearchRequest{Query: "q", Sources: []string{"semantic"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !reflect.DeepEqual(r.filter.Sources, []string{"semantic"}) {
		t.Errorf("Sources = %v, want [semantic]", r.filter.Sources)
	}
}

// --- DW-5.4 ------------------------------------------------------------

// TestDW_5_4_MalformedTimeRangeRejectedAtEntry: since > until is an empty range
// and a caller mistake. It is INVALID_ARGUMENT (never Internal), it names the
// valid filter fields so an LLM can self-correct, and — the point of a barricade
// — the retriever is never called.
func TestDW_5_4_MalformedTimeRangeRejectedAtEntry(t *testing.T) {
	since := timestamppb.New(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	until := timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	r, err := searchWith(t, &engrampb.SearchRequest{Query: "q", Since: since, Until: until})
	if err == nil {
		t.Fatal("an empty time range was accepted")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", got)
	}
	for _, name := range []string{"kind", "subject", "since", "until", "sources"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not name the valid filter field %q: %v", name, err)
		}
	}
	if r.calls != 0 {
		t.Errorf("retriever called %d times on a rejected filter, want 0", r.calls)
	}
}

// TestDW_5_4_EmptySourceNameRejectedAtEntry: a blank source name is caller
// garbage; it is rejected here rather than shipped to the registry.
func TestDW_5_4_EmptySourceNameRejectedAtEntry(t *testing.T) {
	r, err := searchWith(t, &engrampb.SearchRequest{Query: "q", Sources: []string{"semantic", ""}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
	if r.calls != 0 {
		t.Errorf("retriever called %d times, want 0", r.calls)
	}
}

// TestInvalidFilterFromRetrieverIsInvalidArgument: the retriever's own filter
// rejections (unknown source, an unfilterable source named alongside filters)
// are caller errors too. They must surface as INVALID_ARGUMENT carrying the
// retriever's vocabulary-naming message — reporting them as Internal would tell
// the caller "the server broke" when the caller can fix it.
func TestInvalidFilterFromRetrieverIsInvalidArgument(t *testing.T) {
	r := &capturingRetriever{err: fmt.Errorf("%w: unknown source %q; valid sources: episodic, semantic", retrieval.ErrInvalidFilter, "nope")}
	_, err := server.New(nil, r).Search(context.Background(), &engrampb.SearchRequest{Query: "q", Sources: []string{"nope"}})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err: %v)", got, err)
	}
	if !strings.Contains(err.Error(), "valid sources") {
		t.Errorf("the retriever's self-correcting message was dropped: %v", err)
	}
}

// TestRetrieverInfrastructureErrorStaysInternal: the flip side — a real failure
// is still Internal. The InvalidArgument mapping must key off the sentinel, not
// swallow every error into "your fault".
func TestRetrieverInfrastructureErrorStaysInternal(t *testing.T) {
	r := &capturingRetriever{err: errors.New("opensearch: connection refused")}
	_, err := server.New(nil, r).Search(context.Background(), &engrampb.SearchRequest{Query: "q"})
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("code = %v, want Internal", got)
	}
}

// --- DW-5.6 ------------------------------------------------------------

// TestDW_5_6_NoFiltersProducesTodaysFilter: a request with no filter fields set
// must produce EXACTLY the Filter this server produced before filters existed —
// tenancy, ValidOnly:true, and nothing else. Nil (not empty) Predicates and
// Sources matter: an empty Sources slice is a validation error downstream, and
// an empty Predicates slice would trip the unfilterable-source exclusion.
func TestDW_5_6_NoFiltersProducesTodaysFilter(t *testing.T) {
	r, err := searchWith(t, &engrampb.SearchRequest{
		Query: "orders-svc leak", K: 5, TenantId: "t1", UserId: "agent-9",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := retrieval.Filter{TenantID: "t1", UserID: "agent-9", ValidOnly: true}
	if !reflect.DeepEqual(r.filter, want) {
		t.Errorf("filter = %+v, want %+v", r.filter, want)
	}
	if r.filter.Predicates != nil {
		t.Errorf("Predicates = %v, want nil (an empty slice is not the same request)", r.filter.Predicates)
	}
	if r.filter.Sources != nil {
		t.Errorf("Sources = %v, want nil (an empty slice is a validation error downstream)", r.filter.Sources)
	}
}

// --- DW-5.7 ------------------------------------------------------------

// TestDW_5_7_AdversarialFilterValueStaysData: an injection-shaped filter value is
// carried into the predicate as an opaque VALUE — byte for byte, never merged
// into the field name and never re-parsed. (That it lands in the query body as a
// leaf, not as structure, is asserted end-of-chain in internal/retrieval.)
func TestDW_5_7_AdversarialFilterValueStaysData(t *testing.T) {
	const evil = `x"}}]}},"query":{"match_all":{}},"script":{"source":"ctx._source.remove('acl')"`

	r, err := searchWith(t, &engrampb.SearchRequest{Query: "q", Kind: evil, Subject: evil})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := []retrieval.Predicate{
		{Field: "kind", Op: "term", Value: evil},
		{Field: "subject", Op: "term", Value: evil},
	}
	if !reflect.DeepEqual(r.filter.Predicates, want) {
		t.Fatalf("predicates =\n %+v\nwant\n %+v", r.filter.Predicates, want)
	}
}
