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
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ryanthedev/engram/api/engrampb"
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

// exportStub serves canned Export pages; the cursor is the page index.
type exportStub struct {
	engrampb.UnimplementedEngramServer
	pages []*engrampb.ExportResponse
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

func startExportServer(t *testing.T, pages []*engrampb.ExportResponse) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	engrampb.RegisterEngramServer(srv, &exportStub{pages: pages})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
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
