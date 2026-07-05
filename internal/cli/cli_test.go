package cli_test

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/cli"
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
