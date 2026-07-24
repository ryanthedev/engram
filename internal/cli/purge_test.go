package cli_test

// `engram purge` argument handling, driven through cli.Run against an
// in-process stub engramd. The point of these tests is the DRY-RUN DEFAULT:
// the flag that decides whether data is destroyed must be proven, not
// assumed, so the stub records the dry_run bit the CLI actually put on the
// wire rather than the CLI's printed prose.

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/cli"
)

// stubPurgeServer records the MemoryPurge request it received and answers
// with fixed counts. No auth interceptor is attached — role enforcement is
// the server's job and is covered by internal/server's tests; what is under
// test here is purely what the CLI sends and prints.
type stubPurgeServer struct {
	engrampb.UnimplementedEngramServer
	mu  sync.Mutex
	req *engrampb.MemoryPurgeRequest
}

func (s *stubPurgeServer) MemoryPurge(_ context.Context, req *engrampb.MemoryPurgeRequest) (*engrampb.MemoryPurgeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.req = req
	return &engrampb.MemoryPurgeResponse{Episodic: 2, Ledger: 1, Semantic: 3, DryRun: req.GetDryRun()}, nil
}

func (s *stubPurgeServer) received() *engrampb.MemoryPurgeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.req
}

// runPurgeCLI drives `engram purge` with args against a live stub server and
// returns the recorded request plus captured output and exit code.
func runPurgeCLI(t *testing.T, args ...string) (stub *stubPurgeServer, stdout, stderr string, code int) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	stub = &stubPurgeServer{}
	srv := grpc.NewServer()
	engrampb.RegisterEngramServer(srv, stub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	var out, errW bytes.Buffer
	full := append([]string{"purge"}, args...)
	full = append(full, "-addr", lis.Addr().String())
	code = cli.Run(context.Background(), full, noopEnv, &out, &errW)
	return stub, out.String(), errW.String(), code
}

// TestPurgeIsDryRunByDefault is the guardrail test: with no --confirm, the
// CLI must set dry_run on the wire. An inverted default here would put an
// unrecoverable episodic delete one forgotten flag away.
func TestPurgeIsDryRunByDefault(t *testing.T) {
	stub, out, errW, code := runPurgeCLI(t, "--event-id", "ev-1")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errW)
	}
	req := stub.received()
	if req == nil {
		t.Fatal("no MemoryPurge request reached the server")
	}
	if !req.GetDryRun() {
		t.Errorf("dry_run = false without --confirm; the default MUST be a dry run")
	}
	if !strings.Contains(out, "dry run") || !strings.Contains(out, "--confirm") {
		t.Errorf("output %q does not tell the operator it was a dry run and how to confirm", out)
	}
	// The counts are reported per tier, whichever mode produced them.
	for _, want := range []string{"episodic  2", "ledger    1", "semantic  3"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q is missing the per-tier line %q", out, want)
		}
	}
}

// TestPurgeConfirmClearsDryRun: --confirm is the only thing that turns the
// dry_run bit off, and the output then names the graph rebuild the purge
// deliberately did not do.
func TestPurgeConfirmClearsDryRun(t *testing.T) {
	stub, out, errW, code := runPurgeCLI(t, "--event-id", "ev-1", "--confirm")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errW)
	}
	if stub.received().GetDryRun() {
		t.Errorf("dry_run = true with --confirm; --confirm must perform the purge")
	}
	// Assert on the dry-run BANNER, not the bare phrase: the confirmed output
	// legitimately mentions "a dry run" when telling the operator how to verify
	// a raced in-flight extraction was cleaned up. What must never appear is the
	// claim that nothing was changed.
	if strings.Contains(out, "nothing was changed") {
		t.Errorf("confirmed purge output %q still claims nothing was changed", out)
	}
	if strings.Contains(out, "Re-run with --confirm") {
		t.Errorf("confirmed purge output %q still prompts for --confirm", out)
	}
	if !strings.Contains(out, "engram-graph-rebuild") {
		t.Errorf("confirmed purge output %q does not point at the graph rebuild", out)
	}
}

// TestPurgeRepeatedEventIDFlagAccumulates: --event-id is repeatable and
// order-preserving, and is NOT comma-split (an event id may legally contain a
// comma, so splitting would purge ids the operator never named).
func TestPurgeRepeatedEventIDFlagAccumulates(t *testing.T) {
	stub, _, errW, code := runPurgeCLI(t, "--event-id", "ev-1", "--event-id", "ev-2", "--event-id", "a,b")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errW)
	}
	got := stub.received().GetEventIds()
	want := []string{"ev-1", "ev-2", "a,b"}
	if len(got) != len(want) {
		t.Fatalf("event_ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event_ids = %v, want %v", got, want)
		}
	}
}

// TestPurgeRequiresAnEventID: with no --event-id the CLI fails locally and
// never dials, so a bare `engram purge` cannot become a wildcard.
func TestPurgeRequiresAnEventID(t *testing.T) {
	stub, _, errW, code := runPurgeCLI(t)
	if code == 0 {
		t.Errorf("exit code = 0 with no --event-id, want a failure")
	}
	if !strings.Contains(errW, "--event-id") {
		t.Errorf("stderr = %q, want it to name the required --event-id flag", errW)
	}
	if stub.received() != nil {
		t.Errorf("a request reached the server despite missing arguments")
	}
}

// TestPurgeRejectsBlankEventID: a blank --event-id (an unset shell variable,
// almost always) is refused at parse time rather than silently dropped.
func TestPurgeRejectsBlankEventID(t *testing.T) {
	stub, _, errW, code := runPurgeCLI(t, "--event-id", "ev-1", "--event-id", "   ")
	if code == 0 {
		t.Errorf("exit code = 0 with a blank --event-id, want a failure")
	}
	if !strings.Contains(errW, "empty") {
		t.Errorf("stderr = %q, want it to explain the empty value", errW)
	}
	if stub.received() != nil {
		t.Errorf("a request reached the server despite a blank event id")
	}
}

// TestUsageDocumentsPurge: the verb and its dry-run-by-default contract are
// discoverable from `engram help` — an operator reaching for an undo must not
// have to read the source to learn that --confirm is what actually erases.
func TestUsageDocumentsPurge(t *testing.T) {
	var out, errW bytes.Buffer
	if code := cli.Run(context.Background(), []string{"help"}, noopEnv, &out, &errW); code != 0 {
		t.Fatalf("help exit code = %d, want 0", code)
	}
	usage := out.String()
	for _, want := range []string{"engram purge", "--event-id", "--confirm", "DRY RUN BY DEFAULT", "memory-admin"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage is missing %q", want)
		}
	}
}
