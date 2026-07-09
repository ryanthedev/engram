package cli

// White-box tests for the export subcommand's vault rendering — the
// security-sensitive layer of Phase 3. writeVault and its helpers are pure
// (records in, files under one dir out), so every DW behavior is testable
// without a live server; CLI-level tests drive cli.Run against an in-process
// stub gRPC server serving canned Export pages.

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	yaml "go.yaml.in/yaml/v2"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/engramclient"
)

// --- fixtures ---

// ent/edg build the plain records writeVault consumes (renderer-level tests).
func ent(id, name string, aliases ...string) engramclient.ExportEntity {
	now := time.Now()
	return engramclient.ExportEntity{
		ID: id, Name: name, Aliases: aliases,
		MentionCount: 3, Scope: "private", OwnerAgentID: "a1",
		SourceIDs: []string{"ev1", "ev2"},
		ValidAt:   &now, CreatedAt: &now,
	}
}

func edg(id, from, to, predicate string) engramclient.ExportEdge {
	return engramclient.ExportEdge{ID: id, FromEntityID: from, ToEntityID: to, Predicate: predicate}
}

// pbEnt/pbEdg build the proto records the stub gRPC server serves
// (CLI-level tests, exercising the engramclient adapter end-to-end).
func pbEnt(id, name string, aliases ...string) *engrampb.ExportEntity {
	return &engrampb.ExportEntity{
		Id: id, Name: name, Aliases: aliases,
		MentionCount: 3, Scope: "private", OwnerAgentId: "a1",
		SourceIds: []string{"ev1", "ev2"},
		ValidAt:   timestamppb.Now(), CreatedAt: timestamppb.Now(),
	}
}

func pbEdg(id, from, to, predicate string) *engrampb.ExportEdge {
	return &engrampb.ExportEdge{Id: id, FromEntityId: from, ToEntityId: to, Predicate: predicate}
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

// onePage wraps entities+edges into a single terminal Export page.
func onePage(entities []*engrampb.ExportEntity, edges []*engrampb.ExportEdge) []*engrampb.ExportResponse {
	return []*engrampb.ExportResponse{{Entities: entities, Edges: edges}}
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

// parseFrontmatter extracts and YAML-parses the leading frontmatter block,
// failing the test if it is missing or invalid (the DW-3.5 proxy check).
func parseFrontmatter(t *testing.T, content string) map[string]any {
	t.Helper()
	if !strings.HasPrefix(content, "---\n") {
		t.Fatalf("note does not start with a frontmatter block:\n%s", content)
	}
	rest := content[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatalf("frontmatter block is not terminated:\n%s", content)
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(rest[:end+1]), &m); err != nil {
		t.Fatalf("frontmatter is not valid YAML: %v\n%s", err, content)
	}
	return m
}

var wikilinkRe = regexp.MustCompile(`\[\[([^\[\]|]+)\|([^\[\]]*)\]\]`)

// readVault returns filename(without .md) -> content for every note in dir.
func readVault(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read vault: %v", err)
	}
	notes := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read note: %v", err)
		}
		notes[strings.TrimSuffix(e.Name(), ".md")] = string(b)
	}
	return notes
}

// assertLinksResolve checks every wikilink in every note targets a real note
// file, returning the number of links seen.
func assertLinksResolve(t *testing.T, dir string) int {
	t.Helper()
	notes := readVault(t, dir)
	n := 0
	for name, content := range notes {
		for _, m := range wikilinkRe.FindAllStringSubmatch(content, -1) {
			n++
			if _, ok := notes[m[1]]; !ok {
				t.Errorf("note %s links to %q which has no note file", name, m[1])
			}
		}
	}
	return n
}

// --- DW-3.1: note rendering ---

func TestDW_3_1_WriteVaultRendersNotes(t *testing.T) {
	dir := t.TempDir()
	entities := []engramclient.ExportEntity{
		ent("e-alice", "Alice", "Al", "Ali"),
		ent("e-bob", "Bob"),
	}
	edges := []engramclient.ExportEdge{edg("ed1", "e-alice", "e-bob", "works_at")}

	stats, err := writeVault(dir, entities, edges)
	if err != nil {
		t.Fatalf("writeVault: %v", err)
	}
	if stats.Entities != 2 || stats.Edges != 1 || stats.Dropped != 0 {
		t.Errorf("stats = %+v, want 2 entities / 1 edge / 0 dropped", stats)
	}

	notes := readVault(t, dir)
	alice, ok := notes["Alice"]
	if !ok {
		t.Fatalf("no Alice.md; notes = %v", keys(notes))
	}
	if _, ok := notes["Bob"]; !ok {
		t.Fatalf("no Bob.md; notes = %v", keys(notes))
	}

	fm := parseFrontmatter(t, alice)
	if got := fm["engram_id"]; got != "e-alice" {
		t.Errorf("engram_id = %v, want e-alice", got)
	}
	aliases, _ := fm["aliases"].([]any)
	if len(aliases) != 2 || aliases[0] != "Al" || aliases[1] != "Ali" {
		t.Errorf("aliases = %v, want [Al Ali]", fm["aliases"])
	}
	if got := fm["mention_count"]; got != 3 {
		t.Errorf("mention_count = %v (%T), want 3", got, got)
	}
	// provenance fields
	for _, k := range []string{"scope", "owner_agent_id", "source_ids", "valid_at", "created_at"} {
		if _, ok := fm[k]; !ok {
			t.Errorf("frontmatter missing provenance key %q", k)
		}
	}
	if !strings.Contains(alice, "\n# Alice\n") {
		t.Errorf("Alice.md missing H1:\n%s", alice)
	}
	if !strings.Contains(alice, "- works_at [[Bob|Bob]]") {
		t.Errorf("Alice.md missing edge bullet:\n%s", alice)
	}
}

func TestWriteVault_EmptyVaultAndNoAliases(t *testing.T) {
	dir := t.TempDir()
	stats, err := writeVault(dir, nil, nil)
	if err != nil {
		t.Fatalf("writeVault on empty export: %v", err)
	}
	if stats.Entities != 0 || stats.Edges != 0 || stats.Dropped != 0 {
		t.Errorf("stats = %+v, want all zero", stats)
	}

	// A no-alias entity still renders an aliases key that parses as a list.
	stats, err = writeVault(dir, []engramclient.ExportEntity{ent("e1", "Solo")}, nil)
	if err != nil || stats.Entities != 1 {
		t.Fatalf("writeVault: %v (stats %+v)", err, stats)
	}
	fm := parseFrontmatter(t, readVault(t, dir)["Solo"])
	if _, ok := fm["aliases"]; !ok {
		t.Errorf("aliases key missing for a no-alias entity")
	}
}

// --- DW-3.2: links resolve, danglers drop ---

func TestDW_3_2_EdgeLinksResolveAndDanglersDrop(t *testing.T) {
	dir := t.TempDir()
	entities := []engramclient.ExportEntity{ent("e-a", "A"), ent("e-b", "B"), ent("e-c", "C")}
	edges := []engramclient.ExportEdge{
		edg("ed1", "e-a", "e-b", "works_at"),
		edg("ed2", "e-a", "e-ghost", "knows"),  // target not exported
		edg("ed3", "e-ghost", "e-b", "made"),   // source not exported
		edg("ed4", "e-b", "e-c", "located_in"), // ok
	}
	stats, err := writeVault(dir, entities, edges)
	if err != nil {
		t.Fatalf("writeVault: %v", err)
	}
	if stats.Edges != 2 || stats.Dropped != 2 {
		t.Errorf("stats = %+v, want 2 kept / 2 dropped", stats)
	}
	if n := assertLinksResolve(t, dir); n != 2 {
		t.Errorf("links rendered = %d, want 2 (danglers must not render)", n)
	}
	if strings.Contains(readVault(t, dir)["A"], "knows") {
		t.Errorf("dangling edge rendered in A.md")
	}
}

func TestDW_3_2_DroppedCountPrinted(t *testing.T) {
	addr := startExportServer(t, onePage([]*engrampb.ExportEntity{pbEnt("e-a", "A"), pbEnt("e-b", "B")},
		[]*engrampb.ExportEdge{
			pbEdg("ed1", "e-a", "e-b", "works_at"),
			pbEdg("ed2", "e-a", "e-ghost", "knows"),
		}))
	dir := filepath.Join(t.TempDir(), "vault")
	out, errW, code := runExportCLI(t, addr, dir)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errW)
	}
	if !strings.Contains(out, "2 entities") || !strings.Contains(out, "1 edges") || !strings.Contains(out, "1 dropped") {
		t.Errorf("output = %q, want entity/edge/dropped counts", out)
	}
}

// --- DW-3.3: sanitization + deterministic homonym filenames ---

func TestDW_3_3_SanitizeFilename(t *testing.T) {
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

func TestDW_3_3_HomonymFilenamesDeterministic(t *testing.T) {
	entities := []engramclient.ExportEntity{
		ent("aaaa1111ffff", "Alice"),
		ent("bbbb2222ffff", "Alice"),   // exact homonym
		ent("cccc3333ffff", "alice"),   // case-fold homonym (macOS FS)
		ent("dddd4444ffff", "Bob"),     // unique: keeps its bare name
		ent("eeee5555ffff", "..."),     // sanitizes to empty: id fallback
		ent("ffff6666ffff", "a\x00/b"), // sanitizes non-empty
	}
	refs := vaultFilenames(entities)

	want := map[string]string{
		"aaaa1111ffff": "Alice (aaaa1111)",
		"bbbb2222ffff": "Alice (bbbb2222)",
		"cccc3333ffff": "alice (cccc3333)",
		"dddd4444ffff": "Bob",
		"eeee5555ffff": "entity (eeee5555)",
		"ffff6666ffff": "a-b",
	}
	for id, w := range want {
		if got := refs[id].File; got != w {
			t.Errorf("filename for %s = %q, want %q", id, got, w)
		}
	}

	// Stability: reversed input order yields the identical assignment.
	rev := make([]engramclient.ExportEntity, len(entities))
	for i, e := range entities {
		rev[len(entities)-1-i] = e
	}
	refs2 := vaultFilenames(rev)
	for id := range want {
		if refs[id] != refs2[id] {
			t.Errorf("assignment for %s order-dependent: %v vs %v", id, refs[id], refs2[id])
		}
	}

	// No two files may collide even case-insensitively.
	seen := map[string]string{}
	for id, r := range refs {
		low := strings.ToLower(r.File)
		if other, dup := seen[low]; dup {
			t.Errorf("filename collision %q between %s and %s", r.File, id, other)
		}
		seen[low] = id
	}
}

func TestVaultFilenames_SkipsEmptyID(t *testing.T) {
	refs := vaultFilenames([]engramclient.ExportEntity{ent("", "Ghost"), ent("e1", "Real")})
	if len(refs) != 1 {
		t.Fatalf("refs = %v, want only the non-empty-id entity", refs)
	}
	if refs["e1"].File != "Real" {
		t.Errorf("File = %q, want Real", refs["e1"].File)
	}
}

// --- DW-3.4: clobber/refuse semantics ---

func TestDW_3_4_ForeignDirRefusedWithoutForce(t *testing.T) {
	addr := startExportServer(t, onePage([]*engrampb.ExportEntity{pbEnt("e1", "A")}, nil))
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

func TestDW_3_4_ForceCleansForeignDir(t *testing.T) {
	addr := startExportServer(t, onePage([]*engrampb.ExportEntity{pbEnt("e1", "A")}, nil))
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
	if _, err := os.Stat(filepath.Join(dir, "A.md")); err != nil {
		t.Errorf("note not written after --force: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, vaultMarker)); err != nil {
		t.Errorf("ownership marker not written: %v", err)
	}
}

func TestDW_3_4_RerunClobbersOwnedDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")

	addr1 := startExportServer(t, onePage([]*engrampb.ExportEntity{pbEnt("e1", "Old")}, nil))
	if _, errW, code := runExportCLI(t, addr1, dir); code != 0 {
		t.Fatalf("first export: exit %d, stderr %s", code, errW)
	}
	if _, err := os.Stat(filepath.Join(dir, "Old.md")); err != nil {
		t.Fatalf("Old.md missing after first export: %v", err)
	}

	// Second run, different graph, NO --force: owned dir regenerates.
	addr2 := startExportServer(t, onePage([]*engrampb.ExportEntity{pbEnt("e2", "New")}, nil))
	if _, errW, code := runExportCLI(t, addr2, dir); code != 0 {
		t.Fatalf("re-export of owned dir: exit %d, stderr %s", code, errW)
	}
	if _, err := os.Stat(filepath.Join(dir, "Old.md")); !os.IsNotExist(err) {
		t.Errorf("stale Old.md survived the regenerating re-run")
	}
	if _, err := os.Stat(filepath.Join(dir, "New.md")); err != nil {
		t.Errorf("New.md missing after re-run: %v", err)
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

// --- DW-3.5 proxy: adversarial frontmatter still parses ---

func TestDW_3_5_FrontmatterParsesWithAdversarialContent(t *testing.T) {
	dir := t.TempDir()
	aliases := []string{"x: y", `"quoted"`, "line\nbreak", "]] [[Evil|E", "- dash", "---"}
	entities := []engramclient.ExportEntity{ent("e1", "Weird: name", aliases...)}
	if _, err := writeVault(dir, entities, nil); err != nil {
		t.Fatalf("writeVault: %v", err)
	}
	notes := readVault(t, dir)
	if len(notes) != 1 {
		t.Fatalf("notes = %v, want exactly one", keys(notes))
	}
	for _, content := range notes {
		fm := parseFrontmatter(t, content)
		got, _ := fm["aliases"].([]any)
		if len(got) != len(aliases) {
			t.Fatalf("aliases = %v, want %d entries", got, len(aliases))
		}
		for i, a := range aliases {
			if got[i] != a {
				t.Errorf("alias[%d] = %q, want %q (YAML round-trip must preserve adversarial content)", i, got[i], a)
			}
		}
	}
}

// --- DW-3.6: path confinement ---

func TestDW_3_6_TraversalNamesConfined(t *testing.T) {
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	canary := filepath.Join(root, "canary.txt")
	if err := os.WriteFile(canary, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareVaultDir(vault, false); err != nil {
		t.Fatalf("prepareVaultDir: %v", err)
	}

	entities := []engramclient.ExportEntity{
		ent("id-trav1", "../../etc/pwn"),
		ent("id-trav2", "..\\..\\win\\pwn"),
		ent("id-abs00", "/etc/passwd"),
		ent("id-dots1", ".."),
		ent("id-dots2", "."),
		ent("id-mixed", "a/b\\c"),
		ent("id-nul00", "\x00\x00"),
		ent("id-canry", "canary.txt"), // same name as the canary, one level up
	}
	stats, err := writeVault(vault, entities, nil)
	if err != nil {
		t.Fatalf("writeVault: %v", err)
	}
	if stats.Entities != len(entities) {
		t.Errorf("entities written = %d, want %d (hostile names are confined, not lost)", stats.Entities, len(entities))
	}

	// Every file under root must be the canary or live inside the vault.
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if path == canary {
			return nil
		}
		rel, rerr := filepath.Rel(vault, path)
		if rerr != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("file escaped the vault: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(canary); string(b) != "untouched" {
		t.Errorf("canary outside the vault was modified")
	}
	if _, err := os.Stat(filepath.Join(root, "etc")); !os.IsNotExist(err) {
		t.Errorf("traversal name created a directory outside the vault")
	}
}

func TestConfinedNotePath_RejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"../x.md", "..", ".", "a/b.md", `a\b.md`, "/abs.md", ""} {
		if p, err := confinedNotePath(dir, bad); err == nil {
			t.Errorf("confinedNotePath(%q) = %q, want error", bad, p)
		}
	}
	if _, err := confinedNotePath(dir, "fine.md"); err != nil {
		t.Errorf("confinedNotePath rejected a safe flat name: %v", err)
	}
}

// --- paging + CLI wiring ---

func TestExport_PagingAssemblesAcrossPages(t *testing.T) {
	pages := []*engrampb.ExportResponse{
		{Entities: []*engrampb.ExportEntity{pbEnt("e-a", "A"), pbEnt("e-b", "B")}, NextCursor: "1"},
		// transitional page: entities exhaust, edges begin
		{Entities: []*engrampb.ExportEntity{pbEnt("e-c", "C")},
			Edges: []*engrampb.ExportEdge{pbEdg("ed1", "e-a", "e-b", "works_at")}, NextCursor: "2"},
		{Edges: []*engrampb.ExportEdge{pbEdg("ed2", "e-b", "e-c", "located_in")}},
	}
	addr := startExportServer(t, pages)
	dir := filepath.Join(t.TempDir(), "vault")
	out, errW, code := runExportCLI(t, addr, dir)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errW)
	}
	if !strings.Contains(out, "3 entities") || !strings.Contains(out, "2 edges") || !strings.Contains(out, "0 dropped") {
		t.Errorf("output = %q, want 3 entities / 2 edges / 0 dropped", out)
	}
	// A cross-page link (edge on page 3, endpoint on page 1) must resolve.
	if n := assertLinksResolve(t, dir); n != 2 {
		t.Errorf("links = %d, want 2", n)
	}
}

func TestExport_NonAdvancingCursorAborts(t *testing.T) {
	pages := []*engrampb.ExportResponse{
		{Entities: []*engrampb.ExportEntity{pbEnt("e-a", "A")}, NextCursor: "1"},
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

func TestExport_EmptyVaultAndMissingDirCreated(t *testing.T) {
	addr := startExportServer(t, onePage(nil, nil))
	dir := filepath.Join(t.TempDir(), "does", "not", "exist")
	out, errW, code := runExportCLI(t, addr, dir)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errW)
	}
	if !strings.Contains(out, "0 entities") {
		t.Errorf("output = %q, want a 0-entity report", out)
	}
	if _, err := os.Stat(filepath.Join(dir, vaultMarker)); err != nil {
		t.Errorf("missing dir was not created with the ownership marker: %v", err)
	}
}

// TestExport_ProtoFieldsReachFrontmatter drives the full CLI path (stub gRPC
// server -> engramclient.ExportPage adapter -> renderer) and checks the proto
// record's fields land in the note, pinning the adapter's field mapping.
func TestExport_ProtoFieldsReachFrontmatter(t *testing.T) {
	e := pbEnt("e-a", "Ada", "Countess", "AL")
	addr := startExportServer(t, onePage([]*engrampb.ExportEntity{e}, nil))
	dir := filepath.Join(t.TempDir(), "vault")
	if _, errW, code := runExportCLI(t, addr, dir); code != 0 {
		t.Fatalf("exit != 0: %s", errW)
	}
	fm := parseFrontmatter(t, readVault(t, dir)["Ada"])
	if got := fm["engram_id"]; got != "e-a" {
		t.Errorf("engram_id = %v, want e-a", got)
	}
	aliases, _ := fm["aliases"].([]any)
	if len(aliases) != 2 || aliases[0] != "Countess" || aliases[1] != "AL" {
		t.Errorf("aliases = %v, want [Countess AL]", fm["aliases"])
	}
	if got := fm["mention_count"]; got != 3 {
		t.Errorf("mention_count = %v, want 3", got)
	}
	for _, k := range []string{"scope", "owner_agent_id", "source_ids", "valid_at", "created_at"} {
		if _, ok := fm[k]; !ok {
			t.Errorf("frontmatter missing %q (adapter dropped the field?)", k)
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

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
