package cli

// White-box tests for the export subcommand's CLI wiring: flag/dir-guard
// semantics, the paginated fetch (episodics + entities + edges, cursor-abort
// preserved), and the end-to-end rich-vault layout driven through cli.Run
// against an in-process stub gRPC server serving canned Export pages. The
// assembly layer itself (writeVault, path confinement, determinism, stats)
// is tested in vault_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/retrieval"
)

// --- fixtures ---

// pbEpi/pbEnt/pbEdg build the proto records the stub gRPC server serves
// (CLI-level tests, exercising the engramclient adapter end-to-end).
func pbEpi(eventID, text string, occurred *time.Time) *engrampb.ExportEpisodic {
	ep := &engrampb.ExportEpisodic{EventId: eventID, Kind: "note", Text: text}
	if occurred != nil {
		ep.OccurredAt = timestamppb.New(*occurred)
	}
	return ep
}

func pbEnt(id, name string, srcs ...string) *engrampb.ExportEntity {
	return &engrampb.ExportEntity{
		Id: id, Name: name, MentionCount: 1, Scope: "private", OwnerAgentId: "a1",
		SourceIds: srcs,
	}
}

func pbEdg(id, from, to, statement string, srcs ...string) *engrampb.ExportEdge {
	return &engrampb.ExportEdge{
		Id: id, FromEntityId: from, ToEntityId: to,
		Predicate: "related_to", Statement: statement, SourceIds: srcs,
	}
}

// richPage is the canonical one-page fixture: two events (one dated, one
// undated), a three-concept component whose middle node is the only hub
// (degree 2; the two leaves are ghosts), and one fully-dangling edge
// (dropped). Expected vault: 2 event notes, 1 concept note, 1 map note,
// 2 ghosts, 1 dropped.
func richPage() *engrampb.ExportResponse {
	occurred := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	return &engrampb.ExportResponse{
		Episodics: []*engrampb.ExportEpisodic{
			pbEpi("ev1", "Alpha shipped the beta.\nLonger prose follows.", &occurred),
			pbEpi("ev2", "Gamma joined Beta.", nil),
		},
		Entities: []*engrampb.ExportEntity{
			pbEnt("e-a", "Alpha", "ev1"),
			pbEnt("e-b", "Beta", "ev1"),
			pbEnt("e-c", "Gamma", "ev2"),
		},
		Edges: []*engrampb.ExportEdge{
			pbEdg("ed1", "e-a", "e-b", "Alpha ships Beta", "ev1"),
			pbEdg("ed2", "e-a", "e-c", "Alpha hired Gamma", "ev2"),
			pbEdg("ed3", "e-x", "e-y", "no endpoint exported"),
		},
	}
}

// exportStub serves canned Export pages, plus (Phase 2) the two knowledge
// read RPCs a given test wires up: collections/docs default to nil (an
// UNIMPLEMENTED response via the embedded UnimplementedEngramServer — the
// "legacy server with no knowledge platform" case, exercised as a soft-fail
// path by the pre-existing memory-only tests below), or knowledgeErr can
// force KnowledgeCollections to fail outright (the DW-2.5 fetch-failure
// case). The cursor is the Export page index.
type exportStub struct {
	engrampb.UnimplementedEngramServer
	pages        []*engrampb.ExportResponse
	collections  []*engrampb.CollectionInfo
	docs         map[string][]*engrampb.KnowledgeHit // collection name -> hits
	knowledgeErr error
}

func (s *exportStub) Export(_ context.Context, req *engrampb.ExportRequest) (*engrampb.ExportResponse, error) {
	idx := 0
	if c := req.GetCursor(); c != "" {
		idx, _ = strconv.Atoi(c)
	}
	if idx >= len(s.pages) {
		return &engrampb.ExportResponse{}, nil
	}
	return s.pages[idx], nil
}

func (s *exportStub) KnowledgeCollections(ctx context.Context, req *engrampb.KnowledgeCollectionsRequest) (*engrampb.KnowledgeCollectionsResponse, error) {
	if s.knowledgeErr != nil {
		return nil, s.knowledgeErr
	}
	if s.collections == nil {
		return s.UnimplementedEngramServer.KnowledgeCollections(ctx, req)
	}
	return &engrampb.KnowledgeCollectionsResponse{Collections: s.collections}, nil
}

// KnowledgeSearch honors offset/k like the real server (from/size paging
// over the fixed docs slice) so tests exercising fetchKnowledgeDocs' paging
// loop see real per-page slices and an exact total, not the whole slice
// repeated on every call.
func (s *exportStub) KnowledgeSearch(_ context.Context, req *engrampb.KnowledgeSearchRequest) (*engrampb.KnowledgeSearchResponse, error) {
	if s.knowledgeErr != nil {
		return nil, s.knowledgeErr
	}
	all := s.docs[req.GetCollection()]
	total := int64(len(all))
	offset, k := int(req.GetOffset()), int(req.GetK())
	if offset >= len(all) {
		return &engrampb.KnowledgeSearchResponse{Total: total}, nil
	}
	end := offset + k
	if k <= 0 || end > len(all) {
		end = len(all)
	}
	return &engrampb.KnowledgeSearchResponse{Hits: all[offset:end], Total: total}, nil
}

// knowledgeCollectionInfo builds one CollectionInfo the stub's
// KnowledgeCollections may list — TextField fixed to "text" throughout these
// tests to match knowledgeHit's fixture shape below.
func knowledgeCollectionInfo(name string, public bool) *engrampb.CollectionInfo {
	return &engrampb.CollectionInfo{
		Spec: &engrampb.CollectionSpec{
			Name:      name,
			TextField: "text",
			Access:    &engrampb.AccessPolicy{Public: public},
		},
	}
}

// knowledgeHit builds one raw hit exactly as the real server would encode
// it: title/text/memory_ref/memory_ref_name as the fields_json row
// (memory_ref/memory_ref_name omitted from the row when empty, matching how
// an ingest batch with no such field would look).
func knowledgeHit(id, title, text, memoryRef, memoryRefName string) *engrampb.KnowledgeHit {
	fields := map[string]any{"title": title, "text": text}
	if memoryRef != "" {
		fields["memory_ref"] = memoryRef
	}
	if memoryRefName != "" {
		fields["memory_ref_name"] = memoryRefName
	}
	b, err := json.Marshal(fields)
	if err != nil {
		panic(err) // fixture-only: a map of strings always marshals
	}
	return &engrampb.KnowledgeHit{Id: id, Score: 1, Collection: "curated_notes", FieldsJson: string(b)}
}

// startStub starts srv as an in-process gRPC server and returns its address;
// startExportServer below is the common-case wrapper every pre-existing
// memory-vault test uses.
func startStub(t *testing.T, stub *exportStub) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	engrampb.RegisterEngramServer(srv, stub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func startExportServer(t *testing.T, pages []*engrampb.ExportResponse) string {
	t.Helper()
	return startStub(t, &exportStub{pages: pages})
}

// runExportCLI drives `engram export` through Run; extra args (e.g. --force)
// are appended AFTER <dir>, exercising the two-pass flag parse.
func runExportCLI(t *testing.T, addr, dir string, extra ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errW bytes.Buffer
	args := append([]string{"export", "-addr", addr, dir}, extra...)
	code = Run(context.Background(), args, func(string) string { return "" }, &out, &errW)
	return out.String(), errW.String(), code
}

// --- DW-5.1: rich vault layout, entity-per-note gone, episodics drained ---

func TestDW_5_1_RichVaultLayoutEndToEnd(t *testing.T) {
	addr := startExportServer(t, []*engrampb.ExportResponse{richPage()})
	dir := filepath.Join(t.TempDir(), "vault")
	out, errW, code := runExportCLI(t, addr, dir)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errW)
	}

	tree := vaultTree(t, dir)
	for _, want := range []string{
		"events/2026/2026-03-01 Alpha shipped the beta.md",
		"events/undated/Gamma joined Beta.md",
		"concepts/Alpha.md",
		"maps/Alpha.md",
		vaultMarker,
	} {
		if _, ok := tree[want]; !ok {
			t.Errorf("vault missing %q; files = %v", want, treeKeys(tree))
		}
	}

	// Entity-per-note format is gone: no note may sit at the vault root, and
	// the ghost leaves get no file anywhere.
	for rel := range tree {
		if !strings.Contains(rel, "/") && strings.HasSuffix(rel, ".md") {
			t.Errorf("root-level note %q: entity-per-note format has returned", rel)
		}
		if strings.Contains(rel, "Beta.md") && strings.HasPrefix(rel, "concepts/") {
			t.Errorf("ghost concept got a file: %q", rel)
		}
	}

	if !strings.Contains(out, "2 events") {
		t.Errorf("output = %q, want an events count", out)
	}
}

func TestDW_5_1_FetchExportDrainsEpisodicsAcrossPages(t *testing.T) {
	occurred := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	pages := []*engrampb.ExportResponse{
		{Episodics: []*engrampb.ExportEpisodic{pbEpi("ev1", "First event prose.", &occurred)}, NextCursor: "1"},
		// transitional page: episodics exhaust, entities begin
		{Episodics: []*engrampb.ExportEpisodic{pbEpi("ev2", "Second event prose.", nil)},
			Entities:   []*engrampb.ExportEntity{pbEnt("e-a", "Alpha", "ev1"), pbEnt("e-b", "Beta", "ev1")},
			NextCursor: "2"},
		{Edges: []*engrampb.ExportEdge{pbEdg("ed1", "e-a", "e-b", "Alpha ships Beta", "ev1")}},
	}
	addr := startExportServer(t, pages)
	dir := filepath.Join(t.TempDir(), "vault")
	out, errW, code := runExportCLI(t, addr, dir)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errW)
	}
	tree := vaultTree(t, dir)
	for _, want := range []string{
		"events/2026/2026-03-01 First event prose.md",
		"events/undated/Second event prose.md",
	} {
		if _, ok := tree[want]; !ok {
			t.Errorf("cross-page episodic not drained: missing %q; files = %v", want, treeKeys(tree))
		}
	}
	if !strings.Contains(out, "2 events") {
		t.Errorf("output = %q, want 2 events", out)
	}
}

func TestExport_NonAdvancingCursorAborts(t *testing.T) {
	pages := []*engrampb.ExportResponse{
		{Episodics: []*engrampb.ExportEpisodic{pbEpi("ev1", "prose", nil)}, NextCursor: "1"},
		{NextCursor: "1"}, // never advances
	}
	addr := startExportServer(t, pages)
	dir := filepath.Join(t.TempDir(), "vault")
	_, errW, code := runExportCLI(t, addr, dir)
	if code == 0 {
		t.Fatal("exit = 0, want failure on a non-advancing cursor")
	}
	if !strings.Contains(errW, "cursor") {
		t.Errorf("stderr = %q, want a cursor-loop error", errW)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("vault dir was created despite a failed fetch (clean-late violated)")
	}
}

// --- DW-5.4: fetch failure, empty tenant, clobber warning ---

func TestDW_5_4_FetchFailureLeavesVaultIntact(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	addr := startExportServer(t, []*engrampb.ExportResponse{richPage()})
	if _, errW, code := runExportCLI(t, addr, dir); code != 0 {
		t.Fatalf("first export: exit %d, stderr %s", code, errW)
	}
	before := vaultTree(t, dir)

	// Second run against a server whose cursor never advances: the fetch
	// fails, and the existing vault must survive byte-for-byte.
	badAddr := startExportServer(t, []*engrampb.ExportResponse{{NextCursor: "0"}})
	if _, _, code := runExportCLI(t, badAddr, dir); code == 0 {
		t.Fatal("exit = 0, want failure from the non-advancing cursor")
	}
	after := vaultTree(t, dir)
	assertTreesEqual(t, before, after, "a failed fetch modified the existing vault")
}

func TestDW_5_4_EmptyTenantMarkerOnlyVault(t *testing.T) {
	addr := startExportServer(t, []*engrampb.ExportResponse{{}})
	dir := filepath.Join(t.TempDir(), "does", "not", "exist")
	out, errW, code := runExportCLI(t, addr, dir)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errW)
	}
	tree := vaultTree(t, dir)
	if len(tree) != 1 {
		t.Errorf("empty tenant vault = %v, want marker only", treeKeys(tree))
	}
	if _, ok := tree[vaultMarker]; !ok {
		t.Errorf("missing dir was not created with the ownership marker")
	}
	if !strings.Contains(out, "0 events, 0 concepts, 0 maps") {
		t.Errorf("output = %q, want an all-zero summary", out)
	}
}

func TestDW_5_4_ClobberWarningPrints(t *testing.T) {
	addr := startExportServer(t, []*engrampb.ExportResponse{richPage()})
	dir := filepath.Join(t.TempDir(), "vault")
	out, errW, code := runExportCLI(t, addr, dir)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errW)
	}
	if !strings.Contains(out, "warning:") || !strings.Contains(out, "clobbered") {
		t.Errorf("output = %q, want the manual-edit clobber warning", out)
	}
}

// --- DW-5.5: summary counts ---

func TestDW_5_5_SummaryCountsPrinted(t *testing.T) {
	addr := startExportServer(t, []*engrampb.ExportResponse{richPage()})
	dir := filepath.Join(t.TempDir(), "vault")
	out, errW, code := runExportCLI(t, addr, dir)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errW)
	}
	want := "exported 2 events, 1 concepts, 1 maps to " + dir + " (2 ghosts, 1 dropped)"
	if !strings.Contains(out, want) {
		t.Errorf("output = %q, want summary line %q", out, want)
	}
}

// --- clobber/refuse semantics (existing invariants, new format) ---

func TestExport_ForeignDirRefusedWithoutForce(t *testing.T) {
	addr := startExportServer(t, []*engrampb.ExportResponse{richPage()})
	dir := t.TempDir()
	foreign := filepath.Join(dir, "precious.txt")
	if err := os.WriteFile(foreign, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errW, code := runExportCLI(t, addr, dir)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero refusing a foreign non-empty dir")
	}
	if !strings.Contains(errW, "--force") {
		t.Errorf("stderr = %q, want a refusal naming --force", errW)
	}
	if b, err := os.ReadFile(foreign); err != nil || string(b) != "mine" {
		t.Errorf("foreign file was touched by a refused export: %v %q", err, b)
	}
}

func TestExport_ForceCleansForeignDir(t *testing.T) {
	addr := startExportServer(t, []*engrampb.ExportResponse{richPage()})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "precious.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// --force AFTER <dir> exercises the two-pass flag parse.
	_, errW, code := runExportCLI(t, addr, dir, "--force")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errW)
	}
	if _, err := os.Stat(filepath.Join(dir, "precious.txt")); !os.IsNotExist(err) {
		t.Errorf("foreign file survived --force clobber")
	}
	tree := vaultTree(t, dir)
	if _, ok := tree["concepts/Alpha.md"]; !ok {
		t.Errorf("notes not written after --force: %v", treeKeys(tree))
	}
	if _, ok := tree[vaultMarker]; !ok {
		t.Errorf("ownership marker not written")
	}
}

func TestExport_RerunClobbersOwnedDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")

	addr1 := startExportServer(t, []*engrampb.ExportResponse{
		{Episodics: []*engrampb.ExportEpisodic{pbEpi("ev1", "Old story", nil)}},
	})
	if _, errW, code := runExportCLI(t, addr1, dir); code != 0 {
		t.Fatalf("first export: exit %d, stderr %s", code, errW)
	}
	if _, ok := vaultTree(t, dir)["events/undated/Old story.md"]; !ok {
		t.Fatalf("Old story note missing after first export")
	}

	// Second run, different memory, NO --force: owned dir regenerates.
	addr2 := startExportServer(t, []*engrampb.ExportResponse{
		{Episodics: []*engrampb.ExportEpisodic{pbEpi("ev2", "New story", nil)}},
	})
	if _, errW, code := runExportCLI(t, addr2, dir); code != 0 {
		t.Fatalf("re-export of owned dir: exit %d, stderr %s", code, errW)
	}
	tree := vaultTree(t, dir)
	if _, ok := tree["events/undated/Old story.md"]; ok {
		t.Errorf("stale event note survived the regenerating re-run")
	}
	if _, ok := tree["events/undated/New story.md"]; !ok {
		t.Errorf("new event note missing after re-run: %v", treeKeys(tree))
	}
}

func TestPrepareVaultDir_CatastrophicGuard(t *testing.T) {
	cases := []struct {
		abs, home string
		want      bool
	}{
		{"/", "/Users/u", true},
		{"/Users/u", "/Users/u", true},
		{"/Users/u/", "/Users/u", true},
		{"/Users/u/vault", "/Users/u", false},
		{"/tmp/x", "", false},
	}
	for _, c := range cases {
		if got := isCatastrophicVaultDir(c.abs, c.home); got != c.want {
			t.Errorf("isCatastrophicVaultDir(%q, %q) = %v, want %v", c.abs, c.home, got, c.want)
		}
	}
}

// TestPrepareVaultDir_SymlinkToHomeRefused pins the symlink-bypass fix: a
// vault dir that is a SYMLINK to $HOME must be caught by the catastrophic
// guard even under --force — the guard compares real paths, not link paths.
// Driven through the full CLI so the refusal is proven to happen before any
// cleaning (the scratch home's contents survive).
func TestPrepareVaultDir_SymlinkToHomeRefused(t *testing.T) {
	root := t.TempDir()
	scratchHome := filepath.Join(root, "home")
	if err := os.MkdirAll(scratchHome, 0o755); err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(scratchHome, "precious.txt")
	if err := os.WriteFile(precious, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "vault")
	if err := os.Symlink(scratchHome, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", scratchHome)

	addr := startExportServer(t, []*engrampb.ExportResponse{richPage()})
	_, errW, code := runExportCLI(t, addr, link, "--force")
	if code == 0 {
		t.Fatalf("exit = 0, want refusal exporting into a symlink to $HOME")
	}
	if !strings.Contains(errW, "refusing to clobber") {
		t.Errorf("stderr = %q, want the catastrophic-dir refusal", errW)
	}
	if b, err := os.ReadFile(precious); err != nil || string(b) != "mine" {
		t.Errorf("home contents were touched through the symlink: %v %q", err, b)
	}
}

// TestPrepareVaultDir_SymlinkToRootRefused covers the other catastrophic
// target: a symlink to the filesystem root is refused even under --force
// (the refusal fires before any RemoveAll, so nothing is ever deleted).
func TestPrepareVaultDir_SymlinkToRootRefused(t *testing.T) {
	link := filepath.Join(t.TempDir(), "vault")
	if err := os.Symlink("/", link); err != nil {
		t.Fatal(err)
	}
	if err := prepareVaultDir(link, true); err == nil {
		t.Fatal("prepareVaultDir accepted a symlink to /, want catastrophic refusal")
	}
}

// TestPrepareVaultDir_ForeignRecheck covers the re-check inside
// prepareVaultDir itself: the dir may have gained foreign content between
// the pre-dial check and the clean, and the second check must still refuse.
func TestPrepareVaultDir_ForeignRecheck(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "precious.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareVaultDir(dir, false); err == nil {
		t.Fatal("prepareVaultDir accepted a foreign non-empty dir without force")
	}
	if _, err := os.Stat(filepath.Join(dir, "precious.txt")); err != nil {
		t.Errorf("foreign file was removed by a refused prepare: %v", err)
	}
}

// TestCheckVaultDir_StatAndReadDirErrors covers the two OS-error branches of
// checkVaultDir: a stat failure that is NOT not-exist (a symlink loop) and a
// dir whose entries cannot be listed (execute-only permissions).
func TestCheckVaultDir_StatAndReadDirErrors(t *testing.T) {
	// A self-referencing symlink makes os.Stat fail with ELOOP.
	loop := filepath.Join(t.TempDir(), "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	if err := checkVaultDir(loop, false); err == nil {
		t.Error("checkVaultDir accepted a symlink loop, want stat error")
	}

	if os.Geteuid() == 0 {
		t.Skip("permission-based branches are not testable as root")
	}
	// Execute-only (no read): stat succeeds, ReadDir fails.
	unlistable := filepath.Join(t.TempDir(), "unlistable")
	if err := os.Mkdir(unlistable, 0o311); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unlistable, 0o755) })
	if err := checkVaultDir(unlistable, false); err == nil {
		t.Error("checkVaultDir accepted an unlistable dir, want ReadDir error")
	}
}

// TestPrepareVaultDir_FilesystemErrors covers prepareVaultDir's OS-error
// branches: MkdirAll under a read-only parent, RemoveAll in a read-only
// owned vault, and the marker write into a read-only empty dir.
func TestPrepareVaultDir_FilesystemErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based branches are not testable as root")
	}

	// MkdirAll failure: the vault dir does not exist and its parent refuses
	// creation.
	roParent := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(roParent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roParent, 0o755) })
	if err := prepareVaultDir(filepath.Join(roParent, "vault"), false); err == nil {
		t.Error("prepareVaultDir created a vault under a read-only parent, want error")
	}

	// RemoveAll failure: a marker-owned vault whose dir is read-only, so its
	// stale entries cannot be unlinked.
	owned := filepath.Join(t.TempDir(), "owned")
	if err := os.Mkdir(owned, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, vaultMarker), []byte("m"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(owned, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(owned, 0o755) })
	if err := prepareVaultDir(owned, false); err == nil {
		t.Error("prepareVaultDir cleaned a read-only vault, want error")
	}

	// Marker-write failure: an empty dir that refuses the ownership marker.
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.Mkdir(empty, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(empty, 0o755) })
	if err := prepareVaultDir(empty, false); err == nil {
		t.Error("prepareVaultDir succeeded without writing the marker, want error")
	}
}

// TestPrepareVaultDir_AbsErrorWithDeletedCwd pins fail-loud behavior for a
// RELATIVE vault dir when the working directory has been deleted: on
// platforms where os.Getwd fails outright this exercises the filepath.Abs
// error branch, while on darwin (whose Getwd survives via the cwd fd) the
// run still errors deterministically at MkdirAll — either way the export
// refuses rather than guessing at a path.
func TestPrepareVaultDir_AbsErrorWithDeletedCwd(t *testing.T) {
	doomed := filepath.Join(t.TempDir(), "doomed")
	if err := os.Mkdir(doomed, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(doomed)
	if err := os.Remove(doomed); err != nil {
		t.Fatal(err)
	}
	if err := prepareVaultDir("relative-vault", false); err == nil {
		t.Error("prepareVaultDir resolved a relative dir with a deleted cwd, want error")
	}
}

// --- fetch/dial/flag error paths ---

func TestExport_FetchErrorAborts(t *testing.T) {
	// A listener that is immediately closed: dialing is lazy, so the failure
	// surfaces on the first ExportPage call — the fetch-error branch.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close()

	dir := filepath.Join(t.TempDir(), "vault")
	_, errW, code := runExportCLI(t, addr, dir)
	if code == 0 {
		t.Fatal("exit = 0, want failure when the export RPC fails")
	}
	if !strings.Contains(errW, "fetching page") {
		t.Errorf("stderr = %q, want a fetch-page error", errW)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("vault dir was created despite a failed fetch")
	}
}

func TestExport_DialErrorSurfaces(t *testing.T) {
	// An unparsable gRPC target makes the (lazy) dial itself fail.
	dir := filepath.Join(t.TempDir(), "vault")
	_, errW, code := runExportCLI(t, "dns:///%zz", dir)
	if code == 0 {
		t.Fatal("exit = 0, want failure when dialing an unparsable address")
	}
	if !strings.Contains(errW, "dialing") {
		t.Errorf("stderr = %q, want a dial error", errW)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("vault dir was created despite a failed dial")
	}
}

func TestExport_FlagParseErrors(t *testing.T) {
	env := func(string) string { return "" }
	// Unknown flag before the positional: the first parse fails.
	var out, errW bytes.Buffer
	if code := Run(context.Background(), []string{"export", "--bogus", "dir"}, env, &out, &errW); code == 0 {
		t.Error("export with an unknown leading flag succeeded")
	}
	// Unknown flag after the positional: the tail re-parse fails.
	errW.Reset()
	if code := Run(context.Background(), []string{"export", filepath.Join(t.TempDir(), "v"), "--bogus"}, env, &out, &errW); code == 0 {
		t.Error("export with an unknown trailing flag succeeded")
	}
}

func TestExport_TargetIsAFileRefused(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// checkVaultDir refuses before dialing, so no server is needed.
	var out, errW bytes.Buffer
	code := Run(context.Background(), []string{"export", file}, func(string) string { return "" }, &out, &errW)
	if code == 0 {
		t.Fatal("exit = 0, want refusal when the target exists and is not a directory")
	}
	if !strings.Contains(errW.String(), "not a directory") {
		t.Errorf("stderr = %q, want a not-a-directory refusal", errW.String())
	}
}

func TestExport_ExistingEmptyDirAccepted(t *testing.T) {
	addr := startExportServer(t, []*engrampb.ExportResponse{richPage()})
	dir := t.TempDir() // exists and is empty: accepted without --force
	_, errW, code := runExportCLI(t, addr, dir)
	if code != 0 {
		t.Fatalf("exit = %d exporting into an existing empty dir, stderr = %s", code, errW)
	}
	if _, ok := vaultTree(t, dir)[vaultMarker]; !ok {
		t.Errorf("ownership marker not written into the existing empty dir")
	}
}

// --- filename sanitization (shared barricade, unchanged behavior) ---

func TestSanitizeFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain name", "plain name"},
		{"a/b", "a-b"},
		{"../../etc/x", "-..-etc-x"},
		{`..\..\win`, "-..-win"},
		{"/etc/passwd", "-etc-passwd"},
		{`a:b*c?d"e<f>g|h`, "a-b-c-d-e-f-g-h"},
		{"a[b]c#d^e", "a-b-c-d-e"},
		{"  .hidden.  ", "hidden"},
		{"..", ""},
		{".", ""},
		{"...", ""},
		{"", ""},
		{"a\x00b\nc", "abc"},
		{"héllo wörld", "héllo wörld"},
		{strings.Repeat("x", 100), strings.Repeat("x", 60)},
	}
	for _, c := range cases {
		if got := sanitizeFilename(c.in); got != c.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExport_ArgValidation(t *testing.T) {
	var out, errW bytes.Buffer
	env := func(string) string { return "" }
	if code := Run(context.Background(), []string{"export"}, env, &out, &errW); code == 0 {
		t.Error("export with no <dir> succeeded")
	}
	errW.Reset()
	if code := Run(context.Background(), []string{"export", "a", "b"}, env, &out, &errW); code == 0 {
		t.Error("export with two positional args succeeded")
	}
}

// TestSafeNoteName_NFCFoldPreventsSilentDrop is a trap test for the APFS
// NFC/NFD filename-collision data-loss class: two note names that differ ONLY
// in Unicode composition ("é" precomposed vs. "e"+combining-acute) must fold to
// one byte sequence in safeNoteName, so the uniqueness check catches the clash
// and suffixes the second instead of writing two "distinct" strings the
// filesystem silently folds into one file. The equality assertion FAILS without
// the norm.NFC fold in safeNoteName.
func TestSafeNoteName_NFCFoldPreventsSilentDrop(t *testing.T) {
	const nfc = "caf\u00e9 notes"  // \u00e9 = precomposed é (NFC)
	const nfd = "cafe\u0301 notes" // e + U+0301 combining acute (NFD)
	if nfc == nfd {
		t.Fatal("fixture broken: NFC and NFD forms must be distinct byte strings")
	}

	// The load-bearing property: both compositions normalize to one name.
	if a, b := safeNoteName(nfc), safeNoteName(nfd); a != b {
		t.Fatalf("safeNoteName must NFC-fold NFD input: nfc=%q -> %q, nfd -> %q", nfc, a, b)
	}

	// Behavioral consequence: uniqueNoteName sees the collision and forces a
	// distinguishing suffix on the second, so no note is silently dropped.
	used := map[string]bool{}
	first := uniqueNoteName(nfc, "id-alpha000", false, used)
	second := uniqueNoteName(nfd, "id-bravo000", false, used)
	if first == second {
		t.Fatalf("NFC/NFD names collapsed to one filename %q — silent-drop bug", first)
	}
	if second == safeNoteName(nfd) {
		t.Fatalf("collision not suffixed: second name %q is the bare (unclashed) form", second)
	}
}

// --- Phase 2: knowledge->vault export rendering with memory mapping ---
//
// richPage's exported graph has exactly one hub concept: "Alpha" (e-a,
// degree 2, filename "Alpha" per concepts/Alpha.md) — "Beta" (e-b) and
// "Gamma" (e-c) are ghosts (degree 1, no file). Every DW-2.x test below
// maps knowledge docs at e-a (resolves) and/or an id that is absent or a
// real ghost (never resolves).

// TestDW_2_1_KnowledgeNotesWikilinkToExportedConcepts covers DW-2.1: one
// knowledge/<name>.md per doc, rendered in deterministic DOC-ID order (not
// input order — the two docs share a title and are fed id-descending, so a
// naive input-order render would give the bare name to the wrong doc), each
// wikilinking a concept filename that actually exists in the vault.
func TestDW_2_1_KnowledgeNotesWikilinkToExportedConcepts(t *testing.T) {
	stub := &exportStub{
		pages:       []*engrampb.ExportResponse{richPage()},
		collections: []*engrampb.CollectionInfo{knowledgeCollectionInfo("curated_notes", true)},
		docs: map[string][]*engrampb.KnowledgeHit{
			"curated_notes": {
				knowledgeHit("kd2", "Shared title", "second body", "e-a", ""),
				knowledgeHit("kd1", "Shared title", "first body", "e-a", ""),
			},
		},
	}
	addr := startStub(t, stub)
	dir := filepath.Join(t.TempDir(), "vault")
	out, errW, code := runExportCLI(t, addr, dir)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errW)
	}

	tree := vaultTree(t, dir)
	bare, ok := tree["knowledge/Shared title.md"]
	if !ok {
		t.Fatalf("missing knowledge/Shared title.md; files = %v", treeKeys(tree))
	}
	if !strings.Contains(bare, "first body") {
		t.Errorf("bare name content = %q, want kd1's body (lower doc id wins the bare name)", bare)
	}
	if !strings.Contains(bare, "[[Alpha|Alpha]]") {
		t.Errorf("kd1 content = %q, want a wikilink to the Alpha concept note", bare)
	}
	foundSuffixed := false
	for _, k := range treeKeys(tree) {
		if strings.HasPrefix(k, "knowledge/Shared title (") {
			foundSuffixed = true
			if !strings.Contains(tree[k], "second body") {
				t.Errorf("suffixed note content = %q, want kd2's body", tree[k])
			}
		}
	}
	if !foundSuffixed {
		t.Errorf("expected a suffixed note for kd2 (the doc-id loser of the homonym); files = %v", treeKeys(tree))
	}
	if _, ok := tree["concepts/Alpha.md"]; !ok {
		t.Fatalf("concept note the wikilink targets does not exist: %v", treeKeys(tree))
	}
	if !strings.Contains(out, "2 knowledge docs") {
		t.Errorf("output = %q, want a knowledge doc count", out)
	}
}

// TestDW_2_2_ConceptNoteGetsReferencedByBacklinks covers DW-2.2: a mapped
// concept note gains a "Referenced by" section; two docs mapping to the same
// concept both appear, in doc-id order.
func TestDW_2_2_ConceptNoteGetsReferencedByBacklinks(t *testing.T) {
	stub := &exportStub{
		pages:       []*engrampb.ExportResponse{richPage()},
		collections: []*engrampb.CollectionInfo{knowledgeCollectionInfo("curated_notes", true)},
		docs: map[string][]*engrampb.KnowledgeHit{
			"curated_notes": {
				knowledgeHit("kd-b", "Note B", "body b", "e-a", ""),
				knowledgeHit("kd-a", "Note A", "body a", "e-a", ""),
			},
		},
	}
	addr := startStub(t, stub)
	dir := filepath.Join(t.TempDir(), "vault")
	_, errW, code := runExportCLI(t, addr, dir)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errW)
	}

	tree := vaultTree(t, dir)
	concept, ok := tree["concepts/Alpha.md"]
	if !ok {
		t.Fatalf("concept note missing: %v", treeKeys(tree))
	}
	if !strings.Contains(concept, "## Referenced by") {
		t.Fatalf("concept note = %q, want a Referenced by section", concept)
	}
	idxA := strings.Index(concept, "[[Note A|Note A]]")
	idxB := strings.Index(concept, "[[Note B|Note B]]")
	if idxA == -1 || idxB == -1 {
		t.Fatalf("concept note = %q, want links to both mapped knowledge notes", concept)
	}
	if idxA > idxB {
		t.Errorf("backlinks not in doc-id order: kd-a's Note A should precede kd-b's Note B")
	}
}

// TestDW_2_3_UnresolvedMemoryRefRendersInertMarker covers DW-2.3: a
// memory_ref that resolves to nothing exported, an id-only unresolved
// reference, AND a real GHOST entity (in the graph, but no file — the plan
// treats this identically) all render the same inert marker: no dangling
// wikilink, no backlink anywhere.
func TestDW_2_3_UnresolvedMemoryRefRendersInertMarker(t *testing.T) {
	stub := &exportStub{
		pages:       []*engrampb.ExportResponse{richPage()},
		collections: []*engrampb.CollectionInfo{knowledgeCollectionInfo("curated_notes", true)},
		docs: map[string][]*engrampb.KnowledgeHit{
			"curated_notes": {
				knowledgeHit("kd1", "Named ref", "body", "no-such-entity", "Ghost Concept"),
				knowledgeHit("kd2", "Bare id ref", "body", "also-missing", ""),
				knowledgeHit("kd3", "Real ghost ref", "body", "e-b", ""), // e-b = Beta, an exported GHOST (no file)
			},
		},
	}
	addr := startStub(t, stub)
	dir := filepath.Join(t.TempDir(), "vault")
	_, errW, code := runExportCLI(t, addr, dir)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errW)
	}

	tree := vaultTree(t, dir)
	cases := []struct{ file, wantMarker string }{
		{"knowledge/Named ref.md", "unresolved: Ghost Concept"},
		{"knowledge/Bare id ref.md", "unresolved: also-missing"},
		{"knowledge/Real ghost ref.md", "unresolved: e-b"},
	}
	for _, c := range cases {
		content, ok := tree[c.file]
		if !ok {
			t.Fatalf("missing %s: %v", c.file, treeKeys(tree))
		}
		if !strings.Contains(content, c.wantMarker) {
			t.Errorf("%s content = %q, want marker %q", c.file, content, c.wantMarker)
		}
		if strings.Contains(content, "[[") {
			t.Errorf("%s content = %q, want no dangling wikilink for an unresolved ref", c.file, content)
		}
	}
	// Neither Alpha (unreferenced here) nor any other concept note gained a
	// backlink from an unresolved ref, and Beta/Gamma (ghosts) never had a
	// file to begin with.
	for rel, content := range tree {
		if strings.HasPrefix(rel, "concepts/") && strings.Contains(content, "Referenced by") {
			t.Errorf("%s unexpectedly gained a backlink from an unresolved ref", rel)
		}
	}
}

// TestDW_2_4_InjectionTrapSanitizedAndConfined covers DW-2.4: hostile
// title/text/memory_ref_name — control chars, forged "[[" / "]]" wikilink
// syntax, "../" path traversal, an over-long NFC/NFD-composed title — must
// be sanitized, byte-budgeted, NFC-folded, and every write confined strictly
// inside dir. Mirrors vault_test.go's TestDW_5_2_HostileNamesStayConfined
// canary pattern: a file next to (not inside) the vault dir must survive
// untouched, and no traversal segment may create a directory outside it.
func TestDW_2_4_InjectionTrapSanitizedAndConfined(t *testing.T) {
	root := t.TempDir()
	canary := filepath.Join(root, "canary.txt")
	if err := os.WriteFile(canary, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "vault")

	hostileTitle := "../../etc/pwn\x00[[Injected]] " + strings.Repeat("x", 300)
	hostileNFDTitle := "café notes" // NFD: e + combining acute
	hostileText := "line1\n> [!danger] forged callout\n```\ncode fence break\n---\nfrontmatter break\n[[wikilink inject]]\nobsidian://run"
	hostileMemoryRefName := "no[[link]]here\nand\x00control"

	stub := &exportStub{
		pages:       []*engrampb.ExportResponse{richPage()},
		collections: []*engrampb.CollectionInfo{knowledgeCollectionInfo("curated_notes", true)},
		docs: map[string][]*engrampb.KnowledgeHit{
			"curated_notes": {
				knowledgeHit("kd1", hostileTitle, hostileText, "missing-entity", hostileMemoryRefName),
				knowledgeHit("kd2", hostileNFDTitle, "second body", "", ""),
			},
		},
	}
	addr := startStub(t, stub)
	_, errW, code := runExportCLI(t, addr, dir)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errW)
	}

	// Every written file must live strictly inside dir; the canary must
	// survive untouched and no "etc" traversal directory must appear.
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if path == canary {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("file escaped the vault: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(canary); string(b) != "untouched" {
		t.Errorf("canary outside the vault was modified")
	}
	if _, err := os.Stat(filepath.Join(root, "etc")); !os.IsNotExist(err) {
		t.Errorf("traversal name created a directory outside the vault")
	}

	tree := vaultTree(t, dir)
	var knowledgeFiles []string
	for rel, content := range tree {
		if !strings.HasPrefix(rel, "knowledge/") {
			continue
		}
		knowledgeFiles = append(knowledgeFiles, rel)
		// A literal ".." substring is allowed to survive as INERT text (the
		// same accepted behavior TestSanitizeFilename already pins for
		// "../../etc/x" -> "-..-etc-x" — '/' is neutralized, not the dots);
		// what must never survive is an actual path separator, which is the
		// real traversal primitive.
		if strings.ContainsAny(rel, `\`) {
			t.Errorf("unsafe knowledge filename: %q", rel)
		}
		if len(filepath.Base(rel)) > maxNoteBaseBytes {
			t.Errorf("knowledge filename over budget: %q (%d bytes)", rel, len(filepath.Base(rel)))
		}
		if strings.Contains(content, "\n> [!danger]") || strings.HasPrefix(content, "> [!danger]") {
			t.Errorf("unescaped (live) callout forgery survived sanitization: %q", content)
		}
		if strings.Contains(content, "\x00") {
			t.Errorf("control character survived sanitization: %q", content)
		}
		if strings.Contains(content, "[[wikilink inject]]") {
			t.Errorf("forged wikilink survived sanitization unescaped: %q", content)
		}
	}
	if len(knowledgeFiles) != 2 {
		t.Fatalf("knowledge files = %v, want exactly 2", knowledgeFiles)
	}
}

// TestDW_2_5_KnowledgeFetchFailureIsSoftWarning covers DW-2.5: a knowledge
// fetch failure (KnowledgeCollections RPC error) must leave the
// already-assembled memory vault byte-intact, print a soft warning, and
// exit 0 — not abort the export.
func TestDW_2_5_KnowledgeFetchFailureIsSoftWarning(t *testing.T) {
	stub := &exportStub{
		pages:        []*engrampb.ExportResponse{richPage()},
		knowledgeErr: status.Error(codes.Unavailable, "knowledge backend down"),
	}
	addr := startStub(t, stub)
	dir := filepath.Join(t.TempDir(), "vault")
	out, errW, code := runExportCLI(t, addr, dir)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (soft warning, not a hard failure); stderr = %s", code, errW)
	}
	if !strings.Contains(out, "warning") || !strings.Contains(out, "knowledge") {
		t.Errorf("output = %q, want a soft knowledge-fetch warning", out)
	}

	tree := vaultTree(t, dir)
	for _, want := range []string{"concepts/Alpha.md", "maps/Alpha.md", vaultMarker} {
		if _, ok := tree[want]; !ok {
			t.Errorf("memory vault missing %q after a knowledge fetch failure: %v", want, treeKeys(tree))
		}
	}
	for rel := range tree {
		if strings.HasPrefix(rel, "knowledge/") {
			t.Errorf("knowledge file %q written despite a fetch failure", rel)
		}
	}
	if strings.Contains(out, "knowledge docs") {
		t.Errorf("output = %q, want no knowledge count on a fetch failure", out)
	}
}

// TestDW_2_6_ZeroCollectionsNoKnowledgeFolder covers DW-2.6: zero knowledge
// collections must produce no knowledge/ folder and a memory-only vault
// byte-identical to a run against a server with no knowledge wiring at all.
func TestDW_2_6_ZeroCollectionsNoKnowledgeFolder(t *testing.T) {
	baseAddr := startExportServer(t, []*engrampb.ExportResponse{richPage()})
	baseDir := filepath.Join(t.TempDir(), "vault")
	if _, errW, code := runExportCLI(t, baseAddr, baseDir); code != 0 {
		t.Fatalf("baseline export: exit %d, stderr %s", code, errW)
	}
	before := vaultTree(t, baseDir)

	// Same memory data; server now explicitly answers KnowledgeCollections
	// with zero collections (distinct from the baseline's RPC-unimplemented
	// case — this is the real "no collections seeded yet" scenario).
	stub := &exportStub{pages: []*engrampb.ExportResponse{richPage()}, collections: []*engrampb.CollectionInfo{}}
	addr := startStub(t, stub)
	dir := filepath.Join(t.TempDir(), "vault")
	out, errW, code := runExportCLI(t, addr, dir)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errW)
	}
	after := vaultTree(t, dir)
	assertTreesEqual(t, before, after, "zero-collection export diverged from the memory-only baseline")

	for rel := range after {
		if strings.HasPrefix(rel, "knowledge/") {
			t.Errorf("knowledge folder written despite zero collections: %q", rel)
		}
	}
	if strings.Contains(out, "knowledge docs") {
		t.Errorf("output = %q, want no knowledge count when there are zero docs", out)
	}
}

// TestDW_3_3_KnowledgeCollectionFullyDrainsBeyondMaxK covers Phase 3's DW-3.3:
// a collection larger than retrieval.MaxK (the per-page fetch size) must be
// drained COMPLETELY via offset paging, and the old possible-truncation
// warning — which fired whenever a single k=MaxK page came back full — must
// never appear now that paging actually continues past it.
func TestDW_3_3_KnowledgeCollectionFullyDrainsBeyondMaxK(t *testing.T) {
	n := retrieval.MaxK*2 + 37 // spans three pages, last one partial
	hits := make([]*engrampb.KnowledgeHit, n)
	for i := range hits {
		hits[i] = knowledgeHit(fmt.Sprintf("kd%04d", i), fmt.Sprintf("Doc %04d", i), "body", "", "")
	}
	stub := &exportStub{
		pages:       []*engrampb.ExportResponse{{}}, // empty memory export keeps this test focused
		collections: []*engrampb.CollectionInfo{knowledgeCollectionInfo("curated_notes", true)},
		docs:        map[string][]*engrampb.KnowledgeHit{"curated_notes": hits},
	}
	addr := startStub(t, stub)
	dir := filepath.Join(t.TempDir(), "vault")
	out, errW, code := runExportCLI(t, addr, dir)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errW)
	}
	if strings.Contains(out, "warning:") && strings.Contains(out, "curated_notes") {
		t.Errorf("output = %q, the no-paging truncation warning must be gone", out)
	}
	want := fmt.Sprintf("%d knowledge docs", n)
	if !strings.Contains(out, want) {
		t.Errorf("output = %q, want all %d docs counted (%s)", out, n, want)
	}
	tree := vaultTree(t, dir)
	count := 0
	for rel := range tree {
		if strings.HasPrefix(rel, "knowledge/") {
			count++
		}
	}
	if count != n {
		t.Errorf("wrote %d knowledge notes, want all %d drained", count, n)
	}
}
