package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/cli"
	"github.com/ryanthedev/engram/internal/store"
)

// noopEnv resolves every environment lookup to empty — tests always pass
// flags explicitly rather than relying on env fallback.
func noopEnv(string) string { return "" }

// stubEngramServer is a minimal in-process engrampb.EngramServer that always
// accepts Ingest, so DW-4.1/DW-4.3 can verify the advisory is purely
// additive: it prints to stderr but never blocks or changes the ingest
// outcome (the phase's client-side-only constraint).
type stubEngramServer struct {
	engrampb.UnimplementedEngramServer
}

func (stubEngramServer) Ingest(_ context.Context, req *engrampb.IngestRequest) (*engrampb.IngestResponse, error) {
	return &engrampb.IngestResponse{Id: "stub-" + req.GetEventId()}, nil
}

// startStubEngramd starts a real gRPC server on an ephemeral loopback port
// and returns its address plus a cleanup func. No auth interceptor is
// attached — token verification is out of scope for this phase's advisory
// behavior.
func startStubEngramd(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	engrampb.RegisterEngramServer(srv, stubEngramServer{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// runIngestCLI drives `engram ingest` through cli.Run against a live stub
// server, returning captured stdout, stderr, and the process exit code.
func runIngestCLI(t *testing.T, text string) (stdout, stderr string, code int) {
	t.Helper()
	addr := startStubEngramd(t)
	var out, errW bytes.Buffer
	args := []string{"ingest", "--event-id", "e1", "--text", text, "-addr", addr}
	code = cli.Run(context.Background(), args, noopEnv, &out, &errW)
	return out.String(), errW.String(), code
}

// TestRunIngest_AdvisoryOnMalformedDirective covers DW-4.1/DW-4.3: a
// space-delimited (not pipe-delimited) fact: line is a directive-looking
// line that fails to parse, so it must trigger a non-fatal stderr advisory
// naming the expected pipe grammar.
func TestRunIngest_AdvisoryOnMalformedDirective(t *testing.T) {
	out, errW, code := runIngestCLI(t, "fact: alice prefers dark-mode")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (advisory must not fail the ingest); stderr=%q", code, errW)
	}
	if !strings.Contains(errW, "warning") {
		t.Errorf("stderr = %q, want an advisory warning", errW)
	}
	if !strings.Contains(errW, `fact: subject | predicate | object`) {
		t.Errorf("stderr = %q, want it to name the expected fact: pipe grammar", errW)
	}
	if !strings.Contains(out, "ingested") {
		t.Errorf("stdout = %q, want the ingest to still complete (advisory is non-blocking)", out)
	}
}

// TestRunIngest_SilentOnPlainProse covers DW-4.1/DW-4.3: prose with no
// directive prefix is the correct, expected input for the production LLM
// extractor — the advisory must stay completely silent.
func TestRunIngest_SilentOnPlainProse(t *testing.T) {
	out, errW, code := runIngestCLI(t, "Alice mentioned she really prefers dark mode these days.")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errW)
	}
	if errW != "" {
		t.Errorf("stderr = %q, want silence on plain prose", errW)
	}
	if !strings.Contains(out, "ingested") {
		t.Errorf("stdout = %q, want the ingest to complete", out)
	}
}

// TestRunIngest_SilentOnWellFormedDirective covers DW-4.1: a correctly
// pipe-delimited fact:/retract:/experience: line must not trigger any
// advisory.
func TestRunIngest_SilentOnWellFormedDirective(t *testing.T) {
	text := "fact: alice | prefers | dark-mode\n" +
		"retract: alice | light-mode\n" +
		"experience: fix bug | patch the reader | success | phi=0.7"
	out, errW, code := runIngestCLI(t, text)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errW)
	}
	if errW != "" {
		t.Errorf("stderr = %q, want silence on well-formed directives", errW)
	}
	if !strings.Contains(out, "ingested") {
		t.Errorf("stdout = %q, want the ingest to complete", out)
	}
}

// TestRunIngest_AdvisoryNamesEachMalformedLine covers a mixed-content event:
// the well-formed line and the plain prose line are silent, but the
// malformed experience: line is flagged with its line number.
func TestRunIngest_AdvisoryNamesEachMalformedLine(t *testing.T) {
	text := "fact: alice | prefers | dark-mode\n" +
		"just some unrelated prose\n" +
		"experience: a note with no pipes at all"
	_, errW, code := runIngestCLI(t, text)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errW)
	}
	if !strings.Contains(errW, "line 3") {
		t.Errorf("stderr = %q, want it to name line 3 (the malformed experience: line)", errW)
	}
	if !strings.Contains(errW, "experience:") {
		t.Errorf("stderr = %q, want it to name the experience: grammar", errW)
	}
}

// TestUsageDocumentsDirectiveGrammar covers DW-4.2: `engram help` documents
// the pipe-delimited fact:/retract:/experience: grammar.
func TestUsageDocumentsDirectiveGrammar(t *testing.T) {
	var out, errW bytes.Buffer
	code := cli.Run(context.Background(), []string{"help"}, noopEnv, &out, &errW)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	help := out.String()
	for _, want := range []string{
		"fact: subject | predicate | object",
		"retract: subject | predicate",
		"experience: task | distilled skill",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help output missing grammar fragment %q\n---\n%s", want, help)
		}
	}
}

// TestIngestHelpDocumentsEventIDSemantics covers DW-4.2: the help text must
// state precisely what --event-id does and does not deduplicate — derived
// facts are content-deduped, but the raw episodic log entry is appended on
// every call.
func TestIngestHelpDocumentsEventIDSemantics(t *testing.T) {
	var out, errW bytes.Buffer
	code := cli.Run(context.Background(), []string{"help"}, noopEnv, &out, &errW)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	help := out.String()
	if strings.Contains(help, "idempotency event id") {
		t.Errorf("help output still claims --event-id is an idempotency key: %s", help)
	}
	if !strings.Contains(help, "deduped by content") {
		t.Errorf("help output missing content-dedup semantics for derived facts:\n%s", help)
	}
	if !strings.Contains(help, "appended on every ingest call") {
		t.Errorf("help output missing the raw-log-appended-every-call semantics:\n%s", help)
	}
}

// --- token create --roles (Phase 1: role-bearing token minting) ---

// fakeAuthTokenIndex is a minimal in-memory fake of the two OpenSearch
// endpoints internal/store.AuthTokenStore issues for minting and verifying a
// token: PUT .../_doc/{hash}?refresh=true (Put) and GET .../_doc/{hash}
// (GetByHash). `token create` never lists or revokes, so _search is
// deliberately not served — narrower than internal/store's own fakeOS.
type fakeAuthTokenIndex struct {
	mu   sync.Mutex
	docs map[string]map[string]any
}

func newFakeAuthTokenIndex() *fakeAuthTokenIndex {
	return &fakeAuthTokenIndex{docs: map[string]map[string]any{}}
}

func (f *fakeAuthTokenIndex) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/")
		idx := strings.LastIndex(path, "/_doc/")
		if idx < 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		id := path[idx+len("/_doc/"):]
		switch r.Method {
		case http.MethodPut:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.docs[id] = body
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"_id": id, "result": "created"})
		case http.MethodGet:
			doc, ok := f.docs[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"found": false})
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"_id": id, "found": true, "_source": doc})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

// newFakeAuthTokenServer starts the fake and returns its base URL.
func newFakeAuthTokenServer(t *testing.T) string {
	t.Helper()
	fake := newFakeAuthTokenIndex()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

// mintTokenCLI drives `engram token create --tenant t1 --user alice ...`
// through cli.Run against the fake OpenSearch backend at fakeURL and returns
// the raw token embedded in stdout (its last line).
func mintTokenCLI(t *testing.T, fakeURL string, extraArgs ...string) (rawToken, stdout, stderr string, code int) {
	t.Helper()
	args := append([]string{"token", "create", "--tenant", "t1", "--user", "alice", "--url", fakeURL}, extraArgs...)
	var out, errW bytes.Buffer
	code = cli.Run(context.Background(), args, noopEnv, &out, &errW)
	stdout, stderr = out.String(), errW.String()
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	rawToken = lines[len(lines)-1]
	return rawToken, stdout, stderr, code
}

// verifiedRoles verifies raw against the fake auth-token store at fakeURL
// (the same backend the mint call wrote to) and returns the bound
// Identity's Roles — the actual auth-layer result, not merely the parsed
// flag.
func verifiedRoles(t *testing.T, fakeURL, raw string) []string {
	t.Helper()
	ts := store.NewAuthTokenStore(http.DefaultClient, fakeURL)
	authn := auth.NewAuthenticator(ts, nil)
	id, err := authn.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return id.Roles
}

// TestDW_1_1_TokenCreateRolesFlagVerifies covers DW-1.1: `token create
// --roles admin,harvester` mints a token whose verified Identity.Roles is
// exactly the normalized set.
func TestDW_1_1_TokenCreateRolesFlagVerifies(t *testing.T) {
	fakeURL := newFakeAuthTokenServer(t)

	raw, _, stderr, code := mintTokenCLI(t, fakeURL, "--roles", "admin,harvester")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr)
	}
	if raw == "" {
		t.Fatal("no raw token captured from stdout")
	}
	got := verifiedRoles(t, fakeURL, raw)
	if want := []string{"admin", "harvester"}; !reflect.DeepEqual(got, want) {
		t.Errorf("verified Roles = %v, want %v", got, want)
	}
}

// TestDW_1_2_TokenCreateNoRolesFlagIsRoleless covers DW-1.2 (regression):
// omitting --roles mints a role-less token exactly as before the flag
// existed.
func TestDW_1_2_TokenCreateNoRolesFlagIsRoleless(t *testing.T) {
	fakeURL := newFakeAuthTokenServer(t)

	raw, _, stderr, code := mintTokenCLI(t, fakeURL)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr)
	}
	if got := verifiedRoles(t, fakeURL, raw); len(got) != 0 {
		t.Errorf("verified Roles = %v, want empty (role-less)", got)
	}
}

// TestDW_1_3_TokenCreateRolesNormalization covers DW-1.3: dirty --roles
// input (padded whitespace, empty segments, duplicates, whitespace-only)
// normalizes to a clean, deduped, order-preserved role set once verified.
func TestDW_1_3_TokenCreateRolesNormalization(t *testing.T) {
	tests := []struct {
		name  string
		roles string
		want  []string
	}{
		{"padded with empty segments", " admin , , harvester ", []string{"admin", "harvester"}},
		{"duplicate roles", "admin,admin,harvester", []string{"admin", "harvester"}},
		{"whitespace-only value", "   ", nil},
		{"trailing comma", "admin,", []string{"admin"}},
		{"single role no padding", "admin", []string{"admin"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeURL := newFakeAuthTokenServer(t)

			raw, _, stderr, code := mintTokenCLI(t, fakeURL, "--roles", tt.roles)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr)
			}
			got := verifiedRoles(t, fakeURL, raw)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("roles=%q: verified Roles = %v, want %v", tt.roles, got, tt.want)
			}
		})
	}
}

// TestTokenCreateStillRequiresTenantAndUser is a defensive-programming check
// (cc-defensive-programming: external CLI input validated at entry): adding
// --roles must not weaken the existing --tenant/--user requirement.
func TestTokenCreateStillRequiresTenantAndUser(t *testing.T) {
	var out, errW bytes.Buffer
	code := cli.Run(context.Background(), []string{"token", "create", "--roles", "admin"}, noopEnv, &out, &errW)
	if code == 0 {
		t.Fatalf("exit code = %d, want non-zero (missing --tenant/--user)", code)
	}
	if !strings.Contains(errW.String(), "--tenant and --user are required") {
		t.Errorf("stderr = %q, want the tenant/user requirement message", errW.String())
	}
}

// TestUsageDocumentsRolesFlag covers the usage-banner half of the phase:
// `engram help` documents the new --roles flag on `token create`.
func TestUsageDocumentsRolesFlag(t *testing.T) {
	var out, errW bytes.Buffer
	code := cli.Run(context.Background(), []string{"help"}, noopEnv, &out, &errW)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "--roles") {
		t.Errorf("help output missing --roles flag on token create:\n%s", out.String())
	}
}
