package engramclient

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ryanthedev/engram/api/engrampb"
)

// TestReadResultFromProtoEpisodic: an episodic ReadResponse adapts into
// structured fields (a real object) with RFC 3339 times and no invented
// zero-value entries.
func TestReadResultFromProtoEpisodic(t *testing.T) {
	occurred := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	resp := &engrampb.ReadResponse{
		Source: "episodic",
		Episodic: &engrampb.EpisodicRecord{
			Id: "ep-1", EventId: "ev-1", Kind: "tool_result",
			Text:       "full body 👻",
			SourceIds:  []string{"src-1"},
			OccurredAt: timestamppb.New(occurred),
			CreatedAt:  timestamppb.New(occurred.Add(time.Second)),
		},
	}
	out := readResultFromProto("ep-1", resp)
	if out.ID != "ep-1" || out.Source != "episodic" {
		t.Errorf("address = %s/%s", out.ID, out.Source)
	}
	if out.Fields["text"] != "full body 👻" || out.Fields["kind"] != "tool_result" || out.Fields["event_id"] != "ev-1" {
		t.Errorf("fields = %+v", out.Fields)
	}
	if out.Fields["occurred_at"] != "2026-07-01T12:00:00Z" {
		t.Errorf("occurred_at = %v, want RFC 3339", out.Fields["occurred_at"])
	}
	if out.Provenance != nil || out.Versions != nil {
		t.Errorf("episodic read must not carry semantic extras: %+v / %+v", out.Provenance, out.Versions)
	}
}

// TestReadResultFromProtoSemantic (DW-2.2 conversion): a semantic
// ReadResponse carries the target fact's fields, its provenance, and the
// full version history — open interval bounds stay absent, closed ones
// render as RFC 3339.
func TestReadResultFromProtoSemantic(t *testing.T) {
	validAt := timestamppb.New(time.Unix(1000, 0).UTC())
	invalidAt := timestamppb.New(time.Unix(1500, 0).UTC())
	v1 := &engrampb.FactVersion{
		Id: "v1", Subject: "svc", Predicate: "owner", Object: "team-a",
		Statement: "svc owner team-a", ValidAt: validAt, InvalidAt: invalidAt,
		CreatedAt: validAt,
	}
	v2 := &engrampb.FactVersion{
		Id: "v2", Subject: "svc", Predicate: "owner", Object: "team-b",
		Statement: "svc owner team-b", Supersedes: "v1", ValidAt: invalidAt,
		CreatedAt: invalidAt,
	}
	resp := &engrampb.ReadResponse{
		Source: "semantic",
		Fact:   v1,
		Provenance: &engrampb.Provenance{
			TenantId: "t1", Scope: "team", TeamId: "teamX", OwnerAgentId: "a1",
			SourceIds: []string{"ev-1"}, ExtractorVersion: "ex-v1",
			CreatedAt: validAt,
		},
		Versions: []*engrampb.FactVersion{v1, v2},
	}
	out := readResultFromProto("v1", resp)
	if out.Source != "semantic" {
		t.Errorf("source = %s", out.Source)
	}
	// The target is the superseded v1 with its CLOSED validity interval.
	if out.Fields["statement"] != "svc owner team-a" || out.Fields["id"] != "v1" {
		t.Errorf("fields = %+v", out.Fields)
	}
	if _, hasClosed := out.Fields["invalid_at"]; !hasClosed {
		t.Error("superseded target lost its closed invalid_at bound")
	}
	if out.Provenance["owner_agent_id"] != "a1" || out.Provenance["extractor_version"] != "ex-v1" {
		t.Errorf("provenance = %+v", out.Provenance)
	}
	if len(out.Versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(out.Versions))
	}
	if out.Versions[1]["supersedes"] != "v1" {
		t.Errorf("v2 = %+v, want supersedes=v1", out.Versions[1])
	}
	// v2 is live: its open bounds must be absent, not zero-dated.
	if _, open := out.Versions[1]["invalid_at"]; open {
		t.Error("live version invented an invalid_at")
	}
}

// TestReadResultFromProtoKnowledgeFieldsJSON (Phase 2, DW-2.4): a knowledge
// drill-down ReadResponse decodes fields_json into structured Fields — a
// real object, never a re-stringified blob — and carries no memory extras.
func TestReadResultFromProtoKnowledgeFieldsJSON(t *testing.T) {
	resp := &engrampb.ReadResponse{
		Source:     "papers",
		FieldsJson: `{"title":"T","text":"whole body","harvest_id":"h1"}`,
	}
	out := readResultFromProto("d1", resp)
	if out.ID != "d1" || out.Source != "papers" {
		t.Errorf("address = %s/%s", out.ID, out.Source)
	}
	if out.Fields["text"] != "whole body" || out.Fields["title"] != "T" {
		t.Errorf("fields = %+v, want the decoded document", out.Fields)
	}
	if out.Provenance != nil || out.Versions != nil {
		t.Errorf("knowledge read must not carry semantic extras: %+v / %+v", out.Provenance, out.Versions)
	}
}

// TestReadResultFromProtoKnowledgeMalformedJSONDegrades: undecodable
// fields_json leaves Fields nil rather than inventing content.
func TestReadResultFromProtoKnowledgeMalformedJSONDegrades(t *testing.T) {
	out := readResultFromProto("d1", &engrampb.ReadResponse{Source: "papers", FieldsJson: "not json"})
	if out.Fields != nil {
		t.Errorf("Fields = %+v, want nil for malformed fields_json", out.Fields)
	}
}
