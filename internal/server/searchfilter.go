package server

import (
	"sort"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/retrieval"
)

// searchFilterFields is the flat filter vocabulary SearchRequest exposes — the
// names an invalid-argument error hands back so a caller can self-correct. It is
// the caller's vocabulary, not the retriever's: the physical fields the
// predicates compile to (occurred_at, valid_at, …) are the retrieval layer's
// business and never appear in a client-facing message.
const searchFilterFields = "kind, subject, predicate, object, extractor_version, since, until, include_superseded, sources"

// compileSearchFilter turns the request's FLAT, named filter fields into the
// generic predicate form retrieval routes internally. This is the ONLY place the
// translation happens: the wire stays flat (an unknown filter field is
// unrepresentable, not merely rejected), and the generic vocabulary stays inside
// the retrieval layer.
//
// It is a barricade: the request arrives from outside this process, so what it
// cannot express structurally it checks here — an empty time range is rejected
// before any tier runs. Field OWNERSHIP is not re-derived: each predicate names
// a field, and the retrieval registry routes it to the tiers that declare it
// (kind -> episodic; the fact triple and extractor_version -> semantic;
// retrieval.TimeField -> episodic occurred_at AND semantic valid_at). This
// function must not know which tier owns what — that knowledge lives in exactly
// one place, and it is not here.
//
// Values are carried as DATA into predicate structures, never spliced into a
// query string; a caller value containing OpenSearch DSL is inert text by the
// time it reaches the query body.
func compileSearchFilter(req *engrampb.SearchRequest) ([]retrieval.Predicate, error) {
	since, until := req.GetSince(), req.GetUntil()
	if since != nil && until != nil && since.AsTime().After(until.AsTime()) {
		return nil, invalidFilter("since (%s) is after until (%s); the time range is empty",
			since.AsTime().UTC().Format(time.RFC3339), until.AsTime().UTC().Format(time.RFC3339))
	}
	for _, s := range req.GetSources() {
		if s == "" {
			return nil, invalidFilter("sources contains an empty name")
		}
	}

	var preds []retrieval.Predicate
	for field, value := range map[string]string{
		"kind":              req.GetKind(),
		"subject":           req.GetSubject(),
		"predicate":         req.GetPredicate(),
		"object":            req.GetObject(),
		"extractor_version": req.GetExtractorVersion(),
	} {
		if value != "" {
			preds = append(preds, retrieval.Predicate{Field: field, Op: "term", Value: value})
		}
	}
	// since/until are ONE range predicate on the tier-neutral time field, so a
	// single pair of bounds constrains every tier by its own event-time field
	// rather than leaving whichever tier lacks the named field unconstrained.
	if bounds := timeBounds(since, until); bounds != nil {
		preds = append(preds, retrieval.Predicate{Field: retrieval.TimeField, Op: "range", Value: bounds})
	}
	// Map iteration is unordered; sorting keeps the emitted query body (and the
	// tests that pin it) deterministic.
	sortPredicates(preds)
	return preds, nil
}

// timeBounds renders the optional since/until pair as range bounds, or nil when
// neither is set. Either bound alone is a legal open interval.
func timeBounds(since, until *timestamppb.Timestamp) map[string]any {
	bounds := map[string]any{}
	if since != nil {
		bounds["gte"] = since.AsTime().UTC().Format(time.RFC3339Nano)
	}
	if until != nil {
		bounds["lte"] = until.AsTime().UTC().Format(time.RFC3339Nano)
	}
	if len(bounds) == 0 {
		return nil
	}
	return bounds
}

// sortPredicates orders predicates by field name so one request always compiles
// to one query body.
func sortPredicates(preds []retrieval.Predicate) {
	sort.Slice(preds, func(i, j int) bool { return preds[i].Field < preds[j].Field })
}

// invalidFilter is a caller-facing INVALID_ARGUMENT naming the filter
// vocabulary — a bad filter is the caller's mistake to fix, never an Internal.
// The message carries the valid field names so a caller (an LLM, through the MCP
// tool) can correct itself from the error text alone.
func invalidFilter(format string, args ...any) error {
	return status.Errorf(codes.InvalidArgument, format+"; valid filter fields: %s",
		append(append([]any(nil), args...), searchFilterFields)...)
}
