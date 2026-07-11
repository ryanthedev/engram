package server_test

// Phase-1 contract tests for the knowledge platform proto surface: the six
// RPCs exist on the generated service (DW-1.1), the embedded
// UnimplementedEngramServer covers them until Phase 6 (DW-1.2), the filter
// Value oneof round-trips scalar and range without loss (boundary), and no
// arXiv-specific field name leaks into the proto (DW-1.3).

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/server"
	"github.com/ryanthedev/engram/internal/testutil"
)

// Compile-time proof all six knowledge RPCs exist on the generated client
// surface with their frozen request/response types (DW-1.1).
var (
	_ = engrampb.EngramClient.KnowledgeIngest
	_ = engrampb.EngramClient.KnowledgeSearch
	_ = engrampb.EngramClient.KnowledgeCollections
	_ = engrampb.EngramClient.KnowledgeDelete
	_ = engrampb.EngramClient.CreateCollection
	_ = engrampb.EngramClient.UpdateCollection
)

// TestDW_1_1_KnowledgeRPCsUnimplementedByDefault proves the six RPCs are part
// of the service AND that a bare Server (no knowledge wiring) answers each
// with codes.Unimplemented via the embedded UnimplementedEngramServer — the
// Phase-1 stub posture the plan requires.
func TestDW_1_1_KnowledgeRPCsUnimplementedByDefault(t *testing.T) {
	ctx := context.Background()
	s := &server.Server{}

	tests := []struct {
		name string
		call func() error
	}{
		{"KnowledgeIngest", func() error {
			_, err := s.KnowledgeIngest(ctx, &engrampb.KnowledgeIngestRequest{})
			return err
		}},
		{"KnowledgeSearch", func() error {
			_, err := s.KnowledgeSearch(ctx, &engrampb.KnowledgeSearchRequest{})
			return err
		}},
		{"KnowledgeCollections", func() error {
			_, err := s.KnowledgeCollections(ctx, &engrampb.KnowledgeCollectionsRequest{})
			return err
		}},
		{"KnowledgeDelete", func() error {
			_, err := s.KnowledgeDelete(ctx, &engrampb.KnowledgeDeleteRequest{})
			return err
		}},
		{"CreateCollection", func() error {
			_, err := s.CreateCollection(ctx, &engrampb.CreateCollectionRequest{})
			return err
		}},
		{"UpdateCollection", func() error {
			_, err := s.UpdateCollection(ctx, &engrampb.UpdateCollectionRequest{})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if got := status.Code(err); got != codes.Unimplemented {
				t.Errorf("%s on a bare Server = %v (code %v), want codes.Unimplemented", tt.name, err, got)
			}
		})
	}
}

// TestValueOneofRoundTrip pins the boundary contract: a Predicate's value
// oneof carries a scalar (string/number/bool) or a Range{gte,lte} through
// marshal/unmarshal without loss, and the oneof discriminates the two arms.
func TestValueOneofRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		pred *engrampb.Predicate
	}{
		{"term string scalar", &engrampb.Predicate{
			Field: "kind",
			Op:    engrampb.PredicateOp_PREDICATE_OP_TERM,
			Value: &engrampb.Predicate_Scalar{Scalar: structpb.NewStringValue("report")},
		}},
		{"term number scalar", &engrampb.Predicate{
			Field: "year",
			Op:    engrampb.PredicateOp_PREDICATE_OP_TERM,
			Value: &engrampb.Predicate_Scalar{Scalar: structpb.NewNumberValue(2026)},
		}},
		{"prefix bool scalar", &engrampb.Predicate{
			Field: "flagged",
			Op:    engrampb.PredicateOp_PREDICATE_OP_PREFIX,
			Value: &engrampb.Predicate_Scalar{Scalar: structpb.NewBoolValue(true)},
		}},
		{"range both bounds", &engrampb.Predicate{
			Field: "created",
			Op:    engrampb.PredicateOp_PREDICATE_OP_RANGE,
			Value: &engrampb.Predicate_Range{Range: &engrampb.Range{
				Gte: structpb.NewStringValue("2026-01-01"),
				Lte: structpb.NewStringValue("2026-07-10"),
			}},
		}},
		{"range open upper bound", &engrampb.Predicate{
			Field: "size",
			Op:    engrampb.PredicateOp_PREDICATE_OP_RANGE,
			Value: &engrampb.Predicate_Range{Range: &engrampb.Range{Gte: structpb.NewNumberValue(10)}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := proto.Marshal(tt.pred)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got := &engrampb.Predicate{}
			if err := proto.Unmarshal(raw, got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !proto.Equal(tt.pred, got) {
				t.Errorf("round-trip lost data: got %v, want %v", got, tt.pred)
			}
			// The oneof must discriminate: exactly the arm we set is present.
			_, wantRange := tt.pred.Value.(*engrampb.Predicate_Range)
			if gotScalar, gotRange := got.GetScalar() != nil, got.GetRange() != nil; gotRange != wantRange || gotScalar == wantRange {
				t.Errorf("oneof arm mismatch: scalar=%v range=%v, want range=%v", gotScalar, gotRange, wantRange)
			}
		})
	}
}

// TestDW_1_3_NoArxivFieldNamesInProto guards the generic-surface rule: the
// proto contract must never name arXiv (or any source-specific) fields —
// collections describe their own fields via CollectionSpec mappings.
func TestDW_1_3_NoArxivFieldNamesInProto(t *testing.T) {
	path := filepath.Join(testutil.RepoRoot(t), "api", "proto", "engram.proto")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	// Word-bounded so "authorization"/"authenticated" don't false-positive.
	// Underscores are split first so snake_case compounds ("primary_category",
	// "arxiv_id") can't slip past \b, which treats _ as a word character.
	banned := regexp.MustCompile(`(?i)\b(arxiv|papers?|abstracts?|authors?|categor(y|ies)|doi|journal)\b`)
	for i, line := range regexp.MustCompile(`\r?\n`).Split(string(raw), -1) {
		if m := banned.FindString(strings.ReplaceAll(line, "_", " ")); m != "" {
			t.Errorf("engram.proto:%d: arXiv-specific name %q leaked into the proto: %s", i+1, m, line)
		}
	}
}
