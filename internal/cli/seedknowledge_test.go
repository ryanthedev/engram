package cli

// White-box tests for the seed-knowledge routine: a small in-process gRPC
// stub serving CreateCollection/KnowledgeIngest drives the real seedKnowledge
// routine end-to-end (same "dial a real engramclient.Client at an in-process
// stub server" convention export_test.go uses for runExport), asserting the
// request shapes sent, the idempotent re-run path, and the PermissionDenied
// wrapping.

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/engramclient"
)

// seedStub serves CreateCollection/KnowledgeIngest for the seed-knowledge
// tests. createErrs/ingestErrs are consumed one per call (nil once
// exhausted), letting a test script "1st call succeeds, 2nd call
// AlreadyExists" or force a PermissionDenied on a given call.
type seedStub struct {
	engrampb.UnimplementedEngramServer

	createErrs []error
	ingestErrs []error

	createCalls int
	ingestCalls int
	lastSpec    *engrampb.CollectionSpec
	lastIngest  *engrampb.KnowledgeIngestRequest
}

func (s *seedStub) CreateCollection(_ context.Context, req *engrampb.CreateCollectionRequest) (*engrampb.CreateCollectionResponse, error) {
	s.lastSpec = req.GetSpec()
	var err error
	if s.createCalls < len(s.createErrs) {
		err = s.createErrs[s.createCalls]
	}
	s.createCalls++
	if err != nil {
		return nil, err
	}
	return &engrampb.CreateCollectionResponse{}, nil
}

func (s *seedStub) KnowledgeIngest(_ context.Context, req *engrampb.KnowledgeIngestRequest) (*engrampb.KnowledgeIngestResponse, error) {
	s.lastIngest = req
	var err error
	if s.ingestCalls < len(s.ingestErrs) {
		err = s.ingestErrs[s.ingestCalls]
	}
	s.ingestCalls++
	if err != nil {
		return nil, err
	}
	return &engrampb.KnowledgeIngestResponse{Indexed: int32(len(req.GetDocs()))}, nil
}

// startSeedStub starts stub as an in-process gRPC server and returns a
// dialed *engramclient.Client pointed at it.
func startSeedStub(t *testing.T, stub *seedStub) *engramclient.Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	engrampb.RegisterEngramServer(srv, stub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	client, err := engramclient.Dial(lis.Addr().String(), "test-token")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// --- DW-3.1: one create + one ingest, idempotent re-run ---

func TestDW_3_1_SeedIssuesCreateThenIngest(t *testing.T) {
	stub := &seedStub{}
	client := startSeedStub(t, stub)
	var out bytes.Buffer

	if err := seedKnowledge(context.Background(), client, &out); err != nil {
		t.Fatalf("seedKnowledge: %v", err)
	}

	if stub.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", stub.createCalls)
	}
	if stub.ingestCalls != 1 {
		t.Errorf("ingestCalls = %d, want 1", stub.ingestCalls)
	}
	if stub.lastSpec.GetName() != seedCollectionName {
		t.Errorf("collection name = %q, want %q", stub.lastSpec.GetName(), seedCollectionName)
	}
	if !stub.lastSpec.GetAccess().GetPublic() {
		t.Error("collection spec is not public")
	}
	mappings := stub.lastSpec.GetMappings()
	if got := mappings["memory_ref"]; got.GetType() != "keyword" || !got.GetFilterable() || !got.GetSortable() {
		t.Errorf("memory_ref mapping = %+v, want keyword/filterable/sortable", got)
	}
	if got := mappings["memory_ref_name"]; got.GetType() != "keyword" {
		t.Errorf("memory_ref_name mapping = %+v, want keyword", got)
	}
	if got := len(stub.lastIngest.GetDocs()); got != len(seedDemoDocs) {
		t.Errorf("ingested %d docs, want %d", got, len(seedDemoDocs))
	}
	if stub.lastIngest.GetSource() != seedSource || stub.lastIngest.GetHarvestId() != seedHarvestID {
		t.Errorf("source/harvest_id = %q/%q, want %q/%q",
			stub.lastIngest.GetSource(), stub.lastIngest.GetHarvestId(), seedSource, seedHarvestID)
	}
}

func TestDW_3_1_RerunToleratesAlreadyExistsAndReingests(t *testing.T) {
	stub := &seedStub{
		createErrs: []error{nil, status.Error(codes.AlreadyExists, `collection "curated_notes" already exists`)},
	}
	client := startSeedStub(t, stub)
	var out bytes.Buffer

	if err := seedKnowledge(context.Background(), client, &out); err != nil {
		t.Fatalf("1st run: seedKnowledge: %v", err)
	}
	if err := seedKnowledge(context.Background(), client, &out); err != nil {
		t.Fatalf("2nd run (already-exists) should be tolerated, got: %v", err)
	}

	if stub.createCalls != 2 {
		t.Errorf("createCalls = %d, want 2", stub.createCalls)
	}
	if stub.ingestCalls != 2 {
		t.Errorf("ingestCalls = %d, want 2 (ingest re-runs even when create was skipped)", stub.ingestCalls)
	}
}

// --- DW-3.2: single-source demo-doc set, every doc carries a memory_ref ---

func TestDW_3_2_DemoDocsCarryMemoryRefAndName(t *testing.T) {
	if len(seedDemoDocs) == 0 {
		t.Fatal("seedDemoDocs is empty")
	}
	seen := map[string]bool{}
	for _, doc := range seedDemoDocs {
		t.Run(doc.ID, func(t *testing.T) {
			if doc.ID == "" {
				t.Error("doc has empty ID")
			}
			if seen[doc.ID] {
				t.Errorf("duplicate doc id %q", doc.ID)
			}
			seen[doc.ID] = true
			ref, _ := doc.Fields["memory_ref"].(string)
			if ref == "" {
				t.Errorf("doc %q has empty memory_ref", doc.ID)
			}
			name, _ := doc.Fields["memory_ref_name"].(string)
			if name == "" {
				t.Errorf("doc %q has empty memory_ref_name", doc.ID)
			}
			if doc.Text == "" {
				t.Errorf("doc %q has empty text", doc.ID)
			}
		})
	}
}

func TestDW_3_2_UnresolvedDemoDocPresentForThePhase2LivePath(t *testing.T) {
	for _, doc := range seedDemoDocs {
		if doc.ID == "curated-unresolved-demo" {
			if ref, _ := doc.Fields["memory_ref"].(string); ref != "entity-does-not-exist-000" {
				t.Errorf("curated-unresolved-demo memory_ref = %q, want the deliberately-unresolvable id", ref)
			}
			return
		}
	}
	t.Error("seedDemoDocs is missing the curated-unresolved-demo doc")
}

// --- DW-3.3: PermissionDenied names the --roles admin remedy ---

func TestDW_3_3_PermissionDeniedOnCreateNamesRolesRemedy(t *testing.T) {
	stub := &seedStub{
		createErrs: []error{status.Error(codes.PermissionDenied, "not authorized for this knowledge operation")},
	}
	client := startSeedStub(t, stub)
	var out bytes.Buffer

	err := seedKnowledge(context.Background(), client, &out)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("status.Code(err) = %v, want PermissionDenied", status.Code(err))
	}
	if !strings.Contains(err.Error(), "engram token create --roles admin") {
		t.Errorf("error %q does not name the --roles admin remedy", err.Error())
	}
	if stub.ingestCalls != 0 {
		t.Errorf("ingestCalls = %d, want 0 (create failed, ingest should not run)", stub.ingestCalls)
	}
}

func TestDW_3_3_PermissionDeniedOnIngestNamesRolesRemedy(t *testing.T) {
	stub := &seedStub{
		ingestErrs: []error{status.Error(codes.PermissionDenied, "not authorized for this knowledge operation")},
	}
	client := startSeedStub(t, stub)
	var out bytes.Buffer

	err := seedKnowledge(context.Background(), client, &out)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("status.Code(err) = %v, want PermissionDenied", status.Code(err))
	}
	if !strings.Contains(err.Error(), "engram token create --roles admin") {
		t.Errorf("error %q does not name the --roles admin remedy", err.Error())
	}
}

// --- supplementary: behavior the DW list doesn't enumerate but the
// implementation surfaces ---

func TestCreateCollectionNonAlreadyExistsErrorPropagates(t *testing.T) {
	stub := &seedStub{
		createErrs: []error{status.Error(codes.Internal, "opensearch is on fire")},
	}
	client := startSeedStub(t, stub)
	var out bytes.Buffer

	err := seedKnowledge(context.Background(), client, &out)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if status.Code(err) != codes.Internal {
		t.Errorf("status.Code(err) = %v, want Internal (not rewritten)", status.Code(err))
	}
	if strings.Contains(err.Error(), "--roles admin") {
		t.Errorf("a non-PermissionDenied error should not carry the roles remedy: %q", err.Error())
	}
}

func TestSeedKnowledgePrintsSummary(t *testing.T) {
	stub := &seedStub{}
	client := startSeedStub(t, stub)
	var out bytes.Buffer

	if err := seedKnowledge(context.Background(), client, &out); err != nil {
		t.Fatalf("seedKnowledge: %v", err)
	}
	if !strings.Contains(out.String(), seedCollectionName) {
		t.Errorf("summary %q does not mention %q", out.String(), seedCollectionName)
	}
}

// --- RunSeedKnowledge: flag parsing / dial wiring ---

func TestRunSeedKnowledge_EndToEndThroughFlags(t *testing.T) {
	stub := &seedStub{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	engrampb.RegisterEngramServer(srv, stub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	var out bytes.Buffer
	err = RunSeedKnowledge(context.Background(), []string{"-addr", lis.Addr().String(), "-token", "admin-token"},
		func(string) string { return "" }, &out)
	if err != nil {
		t.Fatalf("RunSeedKnowledge: %v", err)
	}
	if stub.createCalls != 1 || stub.ingestCalls != 1 {
		t.Errorf("createCalls=%d ingestCalls=%d, want 1/1", stub.createCalls, stub.ingestCalls)
	}
}

func TestRunSeedKnowledge_EnvFallback(t *testing.T) {
	stub := &seedStub{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	engrampb.RegisterEngramServer(srv, stub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	env := map[string]string{"ENGRAM_ADDR": lis.Addr().String(), "ENGRAM_TOKEN": "admin-token"}
	var out bytes.Buffer
	err = RunSeedKnowledge(context.Background(), nil, func(k string) string { return env[k] }, &out)
	if err != nil {
		t.Fatalf("RunSeedKnowledge: %v", err)
	}
	if stub.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (ENGRAM_ADDR/ENGRAM_TOKEN should have been used)", stub.createCalls)
	}
}

func TestRunSeedKnowledge_RejectsUnexpectedArgs(t *testing.T) {
	err := RunSeedKnowledge(context.Background(), []string{"unexpected-positional"}, func(string) string { return "" }, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected an error for an unexpected positional argument")
	}
}

func TestRunSeedKnowledge_DialErrorSurfaces(t *testing.T) {
	// An empty addr with no ENGRAM_ADDR falls back to "localhost:7070";
	// grpc.NewClient itself does not dial eagerly, so this exercises the
	// flag/env resolution path rather than a live connection failure --
	// dialClient's own error paths are exercised in export_test.go and are
	// not re-tested here (out of this phase's scope).
	err := RunSeedKnowledge(context.Background(), []string{"-addr", "127.0.0.1:0", "-token", "t"},
		func(string) string { return "" }, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected a dial/RPC error against an unreachable address")
	}
}
