package server_test

// Read by semantic id PREFIX (readprefix.go). These pin the routing and the
// fail-closed barricade against fakes; the property that needs a real query —
// an unreadable fact sharing the prefix is neither counted nor returned — is
// proved end to end in readprefix_integration_test.go.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/server"
	"github.com/ryanthedev/engram/internal/store"
)

// A full semantic id and two prefixes of it.
const (
	fullFactID = "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12"
	prefix12   = "ab12cd34ef56"
)

// fakePrefixResolver records the resolution it was asked for.
type fakePrefixResolver struct {
	ids     []string
	err     error
	calls   int
	prefix  string
	filters []any
	limit   int
}

func (f *fakePrefixResolver) ResolveFactIDPrefix(_ context.Context, prefix string, filters []any, limit int) ([]string, error) {
	f.calls++
	f.prefix, f.filters, f.limit = prefix, filters, limit
	return f.ids, f.err
}

// fakeClause is a ReadClauseCompiler returning a marker clause (or an error).
type fakeClause struct{ err error }

var aclMarker = map[string]any{"acl": "compiled"}

func (f fakeClause) Compile(context.Context, auth.Identity) (map[string]any, error) {
	if f.err != nil {
		return map[string]any{"match_none": map[string]any{}}, f.err
	}
	return aclMarker, nil
}

// idAuditor is a fakeAuditor that records the id Audit fetched.
type idAuditor struct {
	fakeAuditor
	ids []string
}

func (f *idAuditor) AuditFact(ctx context.Context, id string) (store.VersionedFact, []store.VersionedFact, bool, error) {
	f.ids = append(f.ids, id)
	return f.fakeAuditor.AuditFact(ctx, id)
}

// prefixServer wires a semantic read whose target fact is fullFactID.
func prefixServer(res *fakePrefixResolver, allow bool) (*server.Server, *idAuditor) {
	target := factVF(fullFactID, "t1", "a1")
	aud := &idAuditor{fakeAuditor: fakeAuditor{target: target, versions: []store.VersionedFact{target}, ok: true}}
	return &server.Server{
		Auditor:    aud,
		ACL:        fakeReadAuthz{allow: allow},
		ACLClause:  fakeClause{},
		FactPrefix: res,
	}, aud
}

func readSemantic(svc *server.Server, id string) (*engrampb.ReadResponse, error) {
	return svc.Read(authedCtx("t1", "u1", "a1"), &engrampb.ReadRequest{Id: id, Source: "semantic"})
}

// TestReadPrefix_ResolvesUnderTenantAndACLThenReadsFullID: a 12-char prefix is
// resolved under BOTH the tenant pin and the compiled ACL clause, and the one
// full id it resolves to is what Audit fetches — the response is the full-id
// read.
func TestReadPrefix_ResolvesUnderTenantAndACLThenReadsFullID(t *testing.T) {
	res := &fakePrefixResolver{ids: []string{fullFactID}}
	svc, aud := prefixServer(res, true)

	resp, err := readSemantic(svc, prefix12)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if res.prefix != prefix12 || res.limit < 2 {
		t.Errorf("resolved prefix=%q limit=%d, want %q and a limit that can see an ambiguity", res.prefix, res.limit, prefix12)
	}
	wantFilters := []any{map[string]any{"term": map[string]any{"tenant_id": "t1"}}, aclMarker}
	if !reflect.DeepEqual(res.filters, wantFilters) {
		t.Errorf("resolution filters = %v, want tenant pin + compiled ACL clause %v", res.filters, wantFilters)
	}
	if len(aud.ids) != 1 || aud.ids[0] != fullFactID {
		t.Errorf("Audit fetched %v, want exactly the resolved full id", aud.ids)
	}
	if resp.GetFact().GetId() != fullFactID {
		t.Errorf("fact id = %q, want the full id %q", resp.GetFact().GetId(), fullFactID)
	}
}

// TestReadPrefix_FullIDKeepsExactPath: a full 64-char id (and anything not
// shaped like a hex prefix) never touches the resolver — today's realtime-GET
// read, byte for byte. (Uppercase hex is folded to lowercase first; see
// TestReadPrefix_UppercaseHexFoldsToLowercase.)
func TestReadPrefix_FullIDKeepsExactPath(t *testing.T) {
	for _, id := range []string{fullFactID, "v1-not-hex", fullFactID + "00"} {
		res := &fakePrefixResolver{ids: []string{fullFactID}}
		svc, aud := prefixServer(res, true)
		_, _ = readSemantic(svc, id)
		if res.calls != 0 {
			t.Errorf("id %q: resolver called %d times, want 0", id, res.calls)
		}
		if len(aud.ids) != 1 || aud.ids[0] != id {
			t.Errorf("id %q: Audit fetched %v, want the id as given", id, aud.ids)
		}
	}
}

// TestReadPrefix_Outcomes: no match is the opaque NOT_FOUND (identical to any
// other miss); several matches is a self-correcting INVALID_ARGUMENT naming
// the count; a too-short hex id is INVALID_ARGUMENT without consulting any
// record; a resolved id the exact read then denies is NOT_FOUND.
func TestReadPrefix_Outcomes(t *testing.T) {
	ten := make([]string, 10)
	for i := range ten {
		ten[i] = prefix12 + strings.Repeat("0", 51) + string(rune('0'+i))
	}
	cases := []struct {
		name      string
		id        string
		ids       []string
		allow     bool
		wantCode  codes.Code
		wantMsg   string
		wantCalls int
	}{
		{"no readable match", prefix12, nil, true, codes.NotFound, "record not found", 1},
		{"two matches", prefix12, []string{fullFactID, prefix12 + strings.Repeat("f", 52)}, true, codes.InvalidArgument, "matches 2 records; use more characters", 1},
		{"page filled", prefix12, ten, true, codes.InvalidArgument, "matches 10 or more records; use more characters", 1},
		{"too short", "ab12cd3", []string{fullFactID}, true, codes.InvalidArgument, "at least 8 hex characters", 0},
		{"8 chars is enough", "ab12cd34", []string{fullFactID}, true, codes.OK, "", 1},
		{"resolved but exact read denies", prefix12, []string{fullFactID}, false, codes.NotFound, "record not found", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := &fakePrefixResolver{ids: tc.ids}
			svc, _ := prefixServer(res, tc.allow)
			_, err := readSemantic(svc, tc.id)
			if status.Code(err) != tc.wantCode {
				t.Fatalf("code = %v (%v), want %v", status.Code(err), err, tc.wantCode)
			}
			if tc.wantMsg != "" && !strings.Contains(status.Convert(err).Message(), tc.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", status.Convert(err).Message(), tc.wantMsg)
			}
			if res.calls != tc.wantCalls {
				t.Errorf("resolver calls = %d, want %d", res.calls, tc.wantCalls)
			}
		})
	}
}

// TestReadPrefix_FailsClosed: the resolution never runs without the
// authorization it needs. An ACL compile error is INTERNAL with no query run;
// an ACL wired without its clause compiler refuses prefixes rather than
// resolving under the tenant pin alone; a server with no resolver refuses
// them too. A resolver error is INTERNAL, never a silent miss.
func TestReadPrefix_FailsClosed(t *testing.T) {
	t.Run("ACL compile error", func(t *testing.T) {
		res := &fakePrefixResolver{ids: []string{fullFactID}}
		svc, aud := prefixServer(res, true)
		svc.ACLClause = fakeClause{err: errors.New("edge store down")}
		_, err := readSemantic(svc, prefix12)
		if status.Code(err) != codes.Internal || res.calls != 0 || len(aud.ids) != 0 {
			t.Errorf("code=%v resolver=%d audit=%d, want Internal with nothing run", status.Code(err), res.calls, len(aud.ids))
		}
	})
	t.Run("ACL without clause compiler", func(t *testing.T) {
		res := &fakePrefixResolver{ids: []string{fullFactID}}
		svc, _ := prefixServer(res, true)
		svc.ACLClause = nil
		_, err := readSemantic(svc, prefix12)
		if status.Code(err) != codes.Unimplemented || res.calls != 0 {
			t.Errorf("code=%v resolver=%d, want Unimplemented with no resolution", status.Code(err), res.calls)
		}
		// A full id still reads.
		if _, err := readSemantic(svc, fullFactID); err != nil {
			t.Errorf("full id read: %v", err)
		}
	})
	t.Run("no resolver", func(t *testing.T) {
		svc, _ := prefixServer(nil, true)
		svc.FactPrefix = nil
		if _, err := readSemantic(svc, prefix12); status.Code(err) != codes.Unimplemented {
			t.Errorf("code = %v, want Unimplemented", status.Code(err))
		}
	})
	t.Run("resolver error", func(t *testing.T) {
		res := &fakePrefixResolver{err: errors.New("cluster down")}
		svc, _ := prefixServer(res, true)
		if _, err := readSemantic(svc, prefix12); status.Code(err) != codes.Internal {
			t.Errorf("code = %v, want Internal", status.Code(err))
		}
	})
	t.Run("no ACL: tenant pin only", func(t *testing.T) {
		res := &fakePrefixResolver{ids: []string{fullFactID}}
		svc, _ := prefixServer(res, true)
		svc.ACL, svc.ACLClause = nil, nil
		if _, err := readSemantic(svc, prefix12); err != nil {
			t.Fatalf("Read: %v", err)
		}
		want := []any{map[string]any{"term": map[string]any{"tenant_id": "t1"}}}
		if !reflect.DeepEqual(res.filters, want) {
			t.Errorf("filters = %v, want the tenant pin %v", res.filters, want)
		}
	})
}

// TestReadPrefix_EpisodicUntouched: prefix resolution is semantic-only — an
// episodic read never consults the resolver, whatever the id looks like.
func TestReadPrefix_EpisodicUntouched(t *testing.T) {
	res := &fakePrefixResolver{ids: []string{fullFactID}}
	svc, _ := prefixServer(res, true)
	svc.Episodic = &spyEpisodicReader{}
	_, err := svc.Read(authedCtx("t1", "u1", "a1"), &engrampb.ReadRequest{Id: prefix12, Source: "episodic"})
	if status.Code(err) != codes.NotFound || res.calls != 0 {
		t.Errorf("code=%v resolver=%d, want NotFound with no resolution", status.Code(err), res.calls)
	}
}

// TestReadPrefix_UppercaseHexFoldsToLowercase: fact ids are lowercase hex, so
// a pasted uppercase id or prefix resolves as its lowercase form instead of
// falling through to the opaque NOT_FOUND.
func TestReadPrefix_UppercaseHexFoldsToLowercase(t *testing.T) {
	res := &fakePrefixResolver{ids: []string{fullFactID}}
	svc, aud := prefixServer(res, true)
	if _, err := readSemantic(svc, strings.ToUpper(prefix12)); err != nil {
		t.Fatalf("read uppercase prefix: %v", err)
	}
	if res.prefix != prefix12 {
		t.Errorf("resolver asked for %q, want the lowercase prefix %q", res.prefix, prefix12)
	}
	if _, err := readSemantic(svc, strings.ToUpper(fullFactID)); err != nil {
		t.Fatalf("read uppercase full id: %v", err)
	}
	if last := aud.ids[len(aud.ids)-1]; last != fullFactID {
		t.Errorf("Audit fetched %q, want the lowercase full id", last)
	}
}
