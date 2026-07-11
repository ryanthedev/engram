package server_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/server"
	"github.com/ryanthedev/engram/internal/store"
)

// spyEpisodicReader is an EpisodicReader that records whether it was called —
// the observable for DW-2.7's "no cross-index probing".
type spyEpisodicReader struct {
	rec    memory.Episodic
	ok     bool
	err    error
	called int
}

func (f *spyEpisodicReader) GetEpisodic(context.Context, string) (memory.Episodic, bool, error) {
	f.called++
	return f.rec, f.ok, f.err
}

// spyAuditor wraps fakeAuditor with a call counter (DW-2.7 observable).
type spyAuditor struct {
	fakeAuditor
	called int
}

func (f *spyAuditor) AuditFact(ctx context.Context, id string) (store.VersionedFact, []store.VersionedFact, bool, error) {
	f.called++
	return f.fakeAuditor.AuditFact(ctx, id)
}

// spyReadAuthz records the acl.Record it authorized — the observable proving
// the handler authorizes against the record's PRE-projection ACL fields.
type spyReadAuthz struct {
	allow bool
	saw   []acl.Record
}

func (f *spyReadAuthz) CanRead(_ context.Context, _ auth.Identity, r acl.Record) (bool, error) {
	f.saw = append(f.saw, r)
	return f.allow, nil
}

// longBody is deliberately longer than the Phase-1 lead-snippet cap (200
// runes), so full-text equality proves Read returns the untruncated record.
var longBody = strings.Repeat("the 👻 ghost memory haunts the index; ", 20)

func episodicRec(tenant, owner string) memory.Episodic {
	return memory.Episodic{
		EventID: "ev-7", TenantID: tenant, TeamID: "teamX", Scope: acl.ScopeTeam,
		OwnerAgentID: owner, SourceIDs: []string{"src-1"}, Kind: "tool_result",
		Text:       longBody,
		OccurredAt: time.Unix(2000, 0).UTC(), CreatedAt: time.Unix(2001, 0).UTC(),
	}
}

// TestServerRead_DW_2_1_EpisodicReturnsFullRecord: an authorized episodic
// read returns the record's full untruncated text and content fields, with
// the ACL fields authorized against and then projected away.
func TestServerRead_DW_2_1_EpisodicReturnsFullRecord(t *testing.T) {
	authz := &spyReadAuthz{allow: true}
	svc := &server.Server{
		Episodic: &spyEpisodicReader{rec: episodicRec("t1", "a1"), ok: true},
		ACL:      authz,
	}
	resp, err := svc.Read(authedCtx("t1", "u1", "a1"), &engrampb.ReadRequest{Id: "ep-1", Source: "episodic"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	ep := resp.GetEpisodic()
	if ep.GetText() != longBody {
		t.Errorf("text = %q, want the full untruncated body (%d runes)", ep.GetText(), len([]rune(longBody)))
	}
	if resp.GetSource() != "episodic" || ep.GetId() != "ep-1" || ep.GetEventId() != "ev-7" || ep.GetKind() != "tool_result" {
		t.Errorf("record identity = source=%s id=%s event_id=%s kind=%s", resp.GetSource(), ep.GetId(), ep.GetEventId(), ep.GetKind())
	}
	if len(ep.GetSourceIds()) != 1 || ep.GetSourceIds()[0] != "src-1" {
		t.Errorf("source_ids = %v", ep.GetSourceIds())
	}
	if ep.GetOccurredAt().AsTime() != time.Unix(2000, 0).UTC() {
		t.Errorf("occurred_at = %v", ep.GetOccurredAt().AsTime())
	}
	// DW-2.4 ordering, authorize half: the ACL check saw the record's real
	// pre-projection ACL fields (which the EpisodicRecord proto — the
	// projection — cannot even carry).
	if len(authz.saw) != 1 {
		t.Fatalf("ACL CanRead called %d times, want 1", len(authz.saw))
	}
	want := acl.Record{TenantID: "t1", TeamID: "teamX", Scope: acl.ScopeTeam, OwnerAgentID: "a1"}
	if authz.saw[0] != want {
		t.Errorf("authorized record = %+v, want %+v (fetch -> authorize on full ACL fields)", authz.saw[0], want)
	}
}

// TestServerRead_DW_2_2_SemanticDelegatesToAudit: a semantic read returns the
// TARGET version explicitly plus the Audit contract (provenance + full
// history). Requesting the superseded v1 returns THAT immutable version.
func TestServerRead_DW_2_2_SemanticDelegatesToAudit(t *testing.T) {
	v1, v2 := factVF("v1", "t1", "a1"), factVF("v2", "t1", "a1")
	closed := time.Unix(1500, 0).UTC()
	v1.Fact.InvalidAt = &closed // superseded: closed validity interval
	svc := &server.Server{
		Auditor: fakeAuditor{target: v1, versions: []store.VersionedFact{v1, v2}, ok: true},
		ACL:     fakeReadAuthz{allow: true},
	}
	resp, err := svc.Read(authedCtx("t1", "u1", "a1"), &engrampb.ReadRequest{Id: "v1", Source: "semantic"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if resp.GetSource() != "semantic" {
		t.Errorf("source = %s", resp.GetSource())
	}
	if resp.GetFact().GetId() != "v1" || resp.GetFact().GetInvalidAt() == nil {
		t.Errorf("fact = %+v, want the superseded v1 with its closed interval", resp.GetFact())
	}
	if resp.GetProvenance().GetOwnerAgentId() != "a1" {
		t.Errorf("provenance = %+v", resp.GetProvenance())
	}
	if len(resp.GetVersions()) != 2 {
		t.Errorf("versions = %d, want 2", len(resp.GetVersions()))
	}
}

// TestServerRead_DW_2_3_2_4_FailClosed (dirty): every denial path — unknown
// id, cross-tenant record, ACL denial, missing identity — returns the SAME
// opaque NOT_FOUND, so denial is indistinguishable from absence and no
// existence leaks. Argument misuse is the only distinguishable failure.
func TestServerRead_DW_2_3_2_4_FailClosed(t *testing.T) {
	rec := episodicRec("t1", "a1")
	semTarget := factVF("v2", "t1", "a1")
	cases := []struct {
		name       string
		svc        *server.Server
		ctx        context.Context
		id, source string
		code       codes.Code
	}{
		{"episodic unknown id", &server.Server{Episodic: &spyEpisodicReader{ok: false}, ACL: &spyReadAuthz{allow: true}},
			authedCtx("t1", "u1", "a1"), "nope", "episodic", codes.NotFound},
		{"episodic cross-tenant", &server.Server{Episodic: &spyEpisodicReader{rec: rec, ok: true}, ACL: &spyReadAuthz{allow: true}},
			authedCtx("t-other", "u1", "a1"), "ep-1", "episodic", codes.NotFound},
		{"episodic ACL denied", &server.Server{Episodic: &spyEpisodicReader{rec: rec, ok: true}, ACL: &spyReadAuthz{allow: false}},
			authedCtx("t1", "u2", "a2"), "ep-1", "episodic", codes.NotFound},
		{"episodic no identity", &server.Server{Episodic: &spyEpisodicReader{rec: rec, ok: true}, ACL: &spyReadAuthz{allow: true}},
			context.Background(), "ep-1", "episodic", codes.NotFound},
		{"semantic cross-tenant", &server.Server{Auditor: fakeAuditor{target: semTarget, ok: true}, ACL: fakeReadAuthz{allow: true}},
			authedCtx("t-other", "u1", "a1"), "v2", "semantic", codes.NotFound},
		{"semantic ACL denied", &server.Server{Auditor: fakeAuditor{target: semTarget, ok: true}, ACL: fakeReadAuthz{allow: false}},
			authedCtx("t1", "u2", "a2"), "v2", "semantic", codes.NotFound},
		{"empty id", &server.Server{Episodic: &spyEpisodicReader{rec: rec, ok: true}},
			authedCtx("t1", "u1", "a1"), "", "episodic", codes.InvalidArgument},
		{"blank source", &server.Server{Episodic: &spyEpisodicReader{rec: rec, ok: true}},
			authedCtx("t1", "u1", "a1"), "ep-1", "", codes.InvalidArgument},
		{"unknown source", &server.Server{Episodic: &spyEpisodicReader{rec: rec, ok: true}},
			authedCtx("t1", "u1", "a1"), "ep-1", "experience", codes.InvalidArgument},
		{"graph has no drill", &server.Server{},
			authedCtx("t1", "u1", "a1"), "g-1", "graph", codes.Unimplemented},
		{"episodic unconfigured", &server.Server{},
			authedCtx("t1", "u1", "a1"), "ep-1", "episodic", codes.Unimplemented},
	}
	var denialMsgs []string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := tc.svc.Read(tc.ctx, &engrampb.ReadRequest{Id: tc.id, Source: tc.source})
			if status.Code(err) != tc.code {
				t.Fatalf("Read err = %v (code %s), want %s", err, status.Code(err), tc.code)
			}
			if resp != nil {
				t.Fatalf("Read leaked a response alongside the error: %+v", resp)
			}
			if tc.code == codes.NotFound {
				denialMsgs = append(denialMsgs, status.Convert(err).Message())
			}
		})
	}
	// No-oracle check: every NOT_FOUND denial carries the identical message.
	for _, msg := range denialMsgs {
		if msg != denialMsgs[0] {
			t.Errorf("denial messages differ (%q vs %q): an attacker could distinguish absence from denial", denialMsgs[0], msg)
		}
	}
}

// TestServerRead_DW_2_7_NoCrossIndexProbe (dirty): a miss on the requested
// source NEVER probes the other index — a semantic id asked for as episodic
// is NOT_FOUND without the Auditor ever running, and vice versa.
func TestServerRead_DW_2_7_NoCrossIndexProbe(t *testing.T) {
	t.Run("semantic id with source=episodic", func(t *testing.T) {
		auditor := &spyAuditor{fakeAuditor: fakeAuditor{target: factVF("sem-1", "t1", "a1"), ok: true}}
		episodic := &spyEpisodicReader{ok: false} // sem-1 does not exist in the episodic index
		svc := &server.Server{Episodic: episodic, Auditor: auditor, ACL: fakeReadAuthz{allow: true}}
		_, err := svc.Read(authedCtx("t1", "u1", "a1"), &engrampb.ReadRequest{Id: "sem-1", Source: "episodic"})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("Read = %v, want NotFound", err)
		}
		if auditor.called != 0 {
			t.Errorf("Auditor called %d times on an episodic read: cross-index probe", auditor.called)
		}
		if episodic.called != 1 {
			t.Errorf("episodic reader called %d times, want 1", episodic.called)
		}
	})
	t.Run("episodic id with source=semantic", func(t *testing.T) {
		auditor := &spyAuditor{fakeAuditor: fakeAuditor{ok: false}} // ep-1 is not a fact
		episodic := &spyEpisodicReader{rec: episodicRec("t1", "a1"), ok: true}
		svc := &server.Server{Episodic: episodic, Auditor: auditor, ACL: fakeReadAuthz{allow: true}}
		_, err := svc.Read(authedCtx("t1", "u1", "a1"), &engrampb.ReadRequest{Id: "ep-1", Source: "semantic"})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("Read = %v, want NotFound", err)
		}
		if episodic.called != 0 {
			t.Errorf("episodic reader called %d times on a semantic read: cross-index probe", episodic.called)
		}
		if auditor.called != 1 {
			t.Errorf("Auditor called %d times, want 1", auditor.called)
		}
	})
}
