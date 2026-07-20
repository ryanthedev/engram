package cli

// White-box tests for the rich vault assembly layer (vault.go): the nested
// path-confinement barricade (the security surface of Phase 5), writeVault's
// folder layout and stats, hostile-name confinement across all three
// folders, and byte-identical determinism across re-runs.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ryanthedev/engram/internal/engramclient"
)

// --- fixtures (client-record level, no gRPC) ---

func epi(eventID, text string, occurred *time.Time) engramclient.ExportEpisodic {
	return engramclient.ExportEpisodic{EventID: eventID, Kind: "note", Text: text, OccurredAt: occurred}
}

func entS(id, name string, srcs ...string) engramclient.ExportEntity {
	return engramclient.ExportEntity{ID: id, Name: name, SourceIDs: srcs}
}

func edgS(id, from, to, statement string, srcs ...string) engramclient.ExportEdge {
	return engramclient.ExportEdge{ID: id, FromEntityID: from, ToEntityID: to, Predicate: "related_to", Statement: statement, SourceIDs: srcs}
}

// richRecords mirrors export_test.go's richPage at the client-record level:
// 2 events (one dated, one undated), one hub + two ghosts in a 3-concept
// component (1 map), and one fully-dangling edge (dropped).
func richRecords() ([]engramclient.ExportEpisodic, []engramclient.ExportEntity, []engramclient.ExportEdge) {
	occurred := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	episodics := []engramclient.ExportEpisodic{
		epi("ev1", "Alpha shipped the beta.\nLonger prose follows.", &occurred),
		epi("ev2", "Gamma joined Beta.", nil),
	}
	entities := []engramclient.ExportEntity{
		entS("e-a", "Alpha", "ev1"),
		entS("e-b", "Beta", "ev1"),
		entS("e-c", "Gamma", "ev2"),
	}
	edges := []engramclient.ExportEdge{
		edgS("ed1", "e-a", "e-b", "Alpha ships Beta", "ev1"),
		edgS("ed2", "e-a", "e-c", "Alpha hired Gamma", "ev2"),
		edgS("ed3", "e-x", "e-y", "no endpoint exported"),
	}
	return episodics, entities, edges
}

// vaultTree returns slash-separated relpath -> content for every file under
// dir, recursively.
func vaultTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		tree[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return tree
}

func treeKeys(tree map[string]string) []string {
	out := make([]string, 0, len(tree))
	for k := range tree {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// assertTreesEqual fails unless a and b hold byte-identical files at
// identical relative paths.
func assertTreesEqual(t *testing.T, a, b map[string]string, msg string) {
	t.Helper()
	for rel, content := range a {
		other, ok := b[rel]
		if !ok {
			t.Errorf("%s: %q present in one tree, absent in the other", msg, rel)
			continue
		}
		if other != content {
			t.Errorf("%s: %q differs between trees", msg, rel)
		}
	}
	for rel := range b {
		if _, ok := a[rel]; !ok {
			t.Errorf("%s: %q present in one tree, absent in the other", msg, rel)
		}
	}
}

// --- DW-5.1 (assembly level): layout + old format gone ---

func TestDW_5_1_WriteVaultRichLayout(t *testing.T) {
	dir := t.TempDir()
	episodics, entities, edges := richRecords()
	stats, err := writeVault(dir, episodics, entities, edges)
	if err != nil {
		t.Fatalf("writeVault: %v", err)
	}
	want := vaultStats{Events: 2, Concepts: 1, Maps: 1, Ghosts: 2, Dropped: 1}
	if stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}

	tree := vaultTree(t, dir)
	for _, wantFile := range []string{
		"events/2026/2026-03-01 Alpha shipped the beta.md",
		"events/undated/Gamma joined Beta.md",
		"concepts/Alpha.md",
		"maps/Alpha.md",
	} {
		if _, ok := tree[wantFile]; !ok {
			t.Errorf("vault missing %q; files = %v", wantFile, treeKeys(tree))
		}
	}
	for rel := range tree {
		if !strings.Contains(rel, "/") {
			t.Errorf("root-level file %q: every note must live under events/, concepts/, or maps/", rel)
		}
	}
}

func TestWriteVault_EmptyExport(t *testing.T) {
	dir := t.TempDir()
	stats, err := writeVault(dir, nil, nil, nil)
	if err != nil {
		t.Fatalf("writeVault on empty export: %v", err)
	}
	if stats != (vaultStats{}) {
		t.Errorf("stats = %+v, want all zero", stats)
	}
	if tree := vaultTree(t, dir); len(tree) != 0 {
		t.Errorf("empty export wrote files: %v", treeKeys(tree))
	}
}

// --- DW-5.2: nested path confinement (the security barricade) ---

func TestDW_5_2_ConfinedVaultPathRejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	bad := []string{
		"",
		"/abs.md",
		"..",
		"../pwn.md",
		"events/../pwn.md",
		"events/2026/../pwn.md",
		"events/2026/..",
		"events/2026/...",
		"events/2026/ . ",
		`events\2026\pwn.md`,
		"concepts/../pwn.md",
		"concepts/..",
		"maps/../pwn.md",
		"secrets/pwn.md",     // unknown root folder
		"pwn.md",             // no folder at all
		"events/pwn.md",      // wrong depth for events
		"concepts/a/pwn.md",  // wrong depth for concepts
		"maps/a/pwn.md",      // wrong depth for maps
		"events/2026/a/b.md", // too deep
		"events//pwn.md",     // empty element
		"concepts/pwn.md/",   // trailing separator
		"/etc/passwd",
	}
	for _, rel := range bad {
		if p, err := confinedVaultPath(dir, rel); err == nil {
			t.Errorf("confinedVaultPath(%q) = %q, want refusal", rel, p)
		}
		// The write wrapper must refuse the same paths before any
		// filesystem effect.
		if err := writeVaultNote(dir, rel, "content"); err == nil {
			t.Errorf("writeVaultNote(%q) succeeded, want barricade refusal", rel)
		}
	}
	if tree := vaultTree(t, dir); len(tree) != 0 {
		t.Errorf("refused writes left files behind: %v", treeKeys(tree))
	}

	good := []string{
		"events/2026/2026-03-01 Alpha shipped the beta.md",
		"events/undated/Gamma joined Beta.md",
		"concepts/Alpha (abcd1234).md",
		"maps/misc-01.md",
	}
	for _, rel := range good {
		p, err := confinedVaultPath(dir, rel)
		if err != nil {
			t.Errorf("confinedVaultPath(%q) refused a safe renderer path: %v", rel, err)
			continue
		}
		if r, rerr := filepath.Rel(dir, p); rerr != nil || strings.HasPrefix(r, "..") {
			t.Errorf("confinedVaultPath(%q) = %q, escapes dir", rel, p)
		}
	}
}

func TestDW_5_2_HostileNamesStayConfined(t *testing.T) {
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	canary := filepath.Join(root, "canary.txt")
	if err := os.WriteFile(canary, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareVaultDir(vault, false); err != nil {
		t.Fatalf("prepareVaultDir: %v", err)
	}

	// Hostile names flow into every folder: episodic titles into events/,
	// entity names into concepts/ (the hub) and maps/ (the map title), and
	// ghosts stay linkable but fileless.
	episodics := []engramclient.ExportEpisodic{
		epi("ev-t1", "../../etc/pwn\nprose body", nil),
		epi("ev-t2", `..\..\win\pwn`, nil),
		epi("ev-t3", "/etc/passwd", nil),
	}
	entities := []engramclient.ExportEntity{
		entS("e-h1", "../../etc/passwd", "ev-t1"),
		entS("e-h2", `..\..\win\shadow`, "ev-t2"),
		entS("e-h3", "/etc/shadow", "ev-t3"),
	}
	edges := []engramclient.ExportEdge{
		edgS("ed1", "e-h1", "e-h2", "hostile claim one", "ev-t1"),
		edgS("ed2", "e-h1", "e-h3", "hostile claim two", "ev-t3"),
	}
	stats, err := writeVault(vault, episodics, entities, edges)
	if err != nil {
		t.Fatalf("writeVault: %v (hostile names must be confined, not fatal)", err)
	}
	if stats.Events != 3 || stats.Concepts != 1 || stats.Maps != 1 {
		t.Errorf("stats = %+v, want 3 events / 1 concept / 1 map from confined hostile names", stats)
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

// TestWriteVault_WriteFailurePropagates blocks each vault root folder with a
// regular file so MkdirAll fails, proving a write failure in any of the
// three render loops aborts the export loudly instead of half-writing.
func TestWriteVault_WriteFailurePropagates(t *testing.T) {
	for _, blocked := range []string{"events", "concepts", "maps"} {
		t.Run(blocked, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, blocked), []byte("in the way"), 0o644); err != nil {
				t.Fatal(err)
			}
			episodics, entities, edges := richRecords()
			if _, err := writeVault(dir, episodics, entities, edges); err == nil {
				t.Errorf("writeVault succeeded with %s/ blocked by a file, want error", blocked)
			}
		})
	}
}

func TestWriteFileAtomic_ErrorPaths(t *testing.T) {
	dir := t.TempDir()
	// Temp-file creation fails when the vault dir does not exist.
	missing := filepath.Join(dir, "missing")
	if err := writeFileAtomic(missing, filepath.Join(missing, "x.md"), "content"); err == nil {
		t.Error("writeFileAtomic succeeded with a nonexistent dir, want error")
	}
	// Rename fails when the target path is an existing directory; the temp
	// file must not linger.
	target := filepath.Join(dir, "taken.md")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(dir, target, "content"); err == nil {
		t.Error("writeFileAtomic succeeded renaming onto a directory, want error")
	}
	for rel := range vaultTree(t, dir) {
		if strings.HasPrefix(rel, ".engram-tmp-") {
			t.Errorf("failed write left temp file %q behind", rel)
		}
	}
}

// --- filename byte budget (NAME_MAX regression: clobber-then-abort) ---

func TestTruncateBytes_RuneBoundary(t *testing.T) {
	u := "\U0001F984" // 🦄, 4 bytes
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 10, "abc"},
		{"abc", 3, "abc"},
		{"abcd", 3, "abc"},
		{u + u, 8, u + u},
		{u + u, 7, u}, // never split the second rune
		{u + u, 4, u},
		{u, 3, ""}, // cannot keep a partial rune
		{"", 5, ""},
		{"a" + u, 4, "a"},
	}
	for _, c := range cases {
		if got := truncateBytes(c.in, c.n); got != c.want {
			t.Errorf("truncateBytes(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestFitNoteName_ByteBudget(t *testing.T) {
	longBase := strings.Repeat("\U0001F984", 100) // 400 bytes
	suffix := " (abcd1234)"
	name := fitNoteName(longBase, suffix)
	if got := len(name) + len(".md"); got > maxNoteBaseBytes {
		t.Errorf("basename = %d bytes, want <= %d", got, maxNoteBaseBytes)
	}
	if !strings.HasSuffix(name, suffix) {
		t.Errorf("suffix was truncated: %q", name)
	}
	// A pathological suffix larger than the whole budget still cannot
	// overflow: the base collapses instead.
	huge := strings.Repeat("s", maxNoteBaseBytes+10)
	if got := fitNoteName("base", huge); got != huge {
		t.Errorf("fitNoteName with an over-budget suffix = %q, want the suffix alone", got)
	}
	// A truncation landing right after a dot must not leave the basename
	// with a trailing dot (hidden-file / "name..md" hazards).
	dotBase := strings.Repeat("x", 236) + "." + strings.Repeat("y", 50)
	if got := fitNoteName(dotBase, ""); got != strings.Repeat("x", 236) {
		t.Errorf("fitNoteName left a dirty tail (len %d, last bytes %q)", len(got), got[len(got)-4:])
	}
}

func TestUniqueNoteName_BoundedGrowth(t *testing.T) {
	used := map[string]bool{}
	longID := strings.Repeat("x", 100)
	longBase := strings.Repeat("y", 300)
	for i := 0; i < 50; i++ {
		name := uniqueNoteName(longBase, longID, true, used)
		if got := len(name) + len(".md"); got > maxNoteBaseBytes {
			t.Fatalf("clash #%d: basename = %d bytes, exceeds the %d budget", i, got, maxNoteBaseBytes)
		}
	}
	if len(used) != 50 {
		t.Errorf("distinct names = %d, want 50 (residual-clash names must stay unique)", len(used))
	}
}

func TestSafeNoteName_ChokePoint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain name", "plain name"},
		{"a (世世)", "a (世世)"},                 // valid multibyte survives
		{"dup (世\xe4\xb8)", "dup (世)"},       // partial rune stripped
		{"\xff\xfe", "note"},                 // nothing valid survives
		{"", "note"},                         // never an empty path element
		{"a (b/c\\d)", "a (b-c-d)"},          // separators from id material
		{`x:y*z?"<>|#^[]`, "x-y-z---------"}, // fsIllegal set applied to the assembled name
		{"  .dotted.  ", "dotted"},           // leading/trailing dots and spaces
		{"a\x00b\nc", "abc"},                 // control chars dropped
	}
	for _, c := range cases {
		if got := safeNoteName(c.in); got != c.want {
			t.Errorf("safeNoteName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Over-budget input is byte-capped on a rune boundary.
	long := strings.Repeat("\U0001F984", 100)
	if got := safeNoteName(long); len(got)+len(".md") > maxNoteBaseBytes || !strings.HasPrefix(long, got) {
		t.Errorf("safeNoteName over-budget = %d bytes", len(got))
	}
}

// TestDW_Fix_MultibyteIDsSafeBasenames pins the sibling data-loss fix: ids
// are CLIENT-SUPPLIED and may be multibyte or separator-laden, and their
// bytes feed collision suffixes. Every written basename — events, concepts,
// maps — must be valid UTF-8, inside NAME_MAX, and free of illegal
// characters, with the export succeeding (an invalid or escaping name would
// abort AFTER the old vault was cleaned).
func TestDW_Fix_MultibyteIDsSafeBasenames(t *testing.T) {
	occurred := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	episodics := []engramclient.ExportEpisodic{
		// Three homonym events (same title, same date): every id's bytes
		// become suffix material. 世 is 3 bytes; a naive id[:8] slices
		// mid-rune. "a/b/c" plants path separators in the suffix.
		epi("世世世世世世世世", "dup title", &occurred),
		epi("ab\U0001F984\U0001F984rest", "dup title", &occurred),
		epi("a/b/c", "dup title", &occurred),
	}
	entities := []engramclient.ExportEntity{
		// "misc-01" as a concept name forces the map-title suffix path with
		// a multibyte cluster key; the Dup pair sanitizes to one base
		// ("Dup/Concept" -> "Dup-Concept") while normalizing differently, so
		// both are homonym-suffixed from CJK ids — including the residual
		// extension (both 8-byte prefixes truncate to the same "日本").
		entS("日本語-alpha", "misc-01", "世世世世世世世世"),
		entS("日本語-beta", "Dup/Concept", "ab\U0001F984\U0001F984rest"),
		entS("日本語-gamma", "Dup-Concept", "a/b/c"),
	}
	edges := []engramclient.ExportEdge{
		edgS("edm1", "日本語-alpha", "日本語-beta", "claim one"),
		edgS("edm2", "日本語-alpha", "日本語-gamma", "claim two"),
		edgS("edm3", "日本語-beta", "日本語-gamma", "claim three"),
	}

	dir := t.TempDir()
	stats, err := writeVault(dir, episodics, entities, edges)
	if err != nil {
		t.Fatalf("writeVault with multibyte/separator ids: %v (this is the clobber-then-abort class)", err)
	}
	if stats.Events != 3 || stats.Concepts != 3 || stats.Maps != 1 {
		t.Errorf("stats = %+v, want 3 events / 3 concepts / 1 map", stats)
	}

	tree := vaultTree(t, dir)
	joined := strings.Join(treeKeys(tree), "\n")
	for rel := range tree {
		base := rel[strings.LastIndex(rel, "/")+1:]
		if !utf8.Valid([]byte(base)) {
			t.Errorf("basename %q is not valid UTF-8", base)
		}
		if len([]byte(base)) > 255 {
			t.Errorf("basename %q is %d bytes, exceeds NAME_MAX", base, len(base))
		}
		if strings.ContainsAny(base, fsIllegal) {
			t.Errorf("basename %q contains a filesystem/Obsidian-illegal character", base)
		}
	}
	// Fixture sanity: the hostile id material really reached the suffixes.
	for _, want := range []string{
		"(世世)",                 // CJK id prefix, rune-safe
		"(ab\U0001F984)",       // emoji id truncated on a rune boundary
		"(a-b-c)",              // separator id flattened, not escaping and not aborting
		"(日本語-ga",              // residual-clash extension from a CJK id
		"maps/misc-01 (日本).md", // forced map suffix from a multibyte cluster key
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("no basename contains %q — the fixture failed to exercise it; files:\n%s", want, joined)
		}
	}

	// The new naming path stays deterministic.
	dir2 := t.TempDir()
	if _, err := writeVault(dir2, episodics, entities, edges); err != nil {
		t.Fatalf("second run: %v", err)
	}
	assertTreesEqual(t, tree, vaultTree(t, dir2), "multibyte-id re-run")
}

// TestDW_Fix_LongNamesFitNameMax pins the data-loss fix end to end: 60-rune
// emoji titles (240 bytes before any prefix/suffix) with a date prefix AND
// forced collision suffixes, across all three folders — every written
// basename must fit NAME_MAX and the export must succeed.
func TestDW_Fix_LongNamesFitNameMax(t *testing.T) {
	long60 := strings.Repeat("\U0001F984", 60) // 60 runes, 240 bytes
	long61 := strings.Repeat("\U0001F984", 61) // slug rune-caps to the same 60
	occurred := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

	episodics := []engramclient.ExportEpisodic{
		// Same first line + same date: homonym events, BOTH get a collision
		// suffix on top of the date prefix — the worst-case composition.
		epi("ev-l1", long60+"\nprose one", &occurred),
		epi("ev-l2", long60+"\nprose two", &occurred),
	}
	entities := []engramclient.ExportEntity{
		// Distinct normalized names (61 != 60 runes) whose slugs rune-cap to
		// the SAME base: homonym concepts, both suffixed.
		entS("e-u1", long60, "ev-l1"),
		entS("e-u2", long61, "ev-l2"),
		entS("e-u3", "Anchor", "ev-l1"),
	}
	edges := []engramclient.ExportEdge{
		edgS("edl1", "e-u1", "e-u2", "long claim one", "ev-l1"),
		edgS("edl2", "e-u1", "e-u3", "long claim two", "ev-l1"),
	}

	dir := t.TempDir()
	stats, err := writeVault(dir, episodics, entities, edges)
	if err != nil {
		t.Fatalf("writeVault with 60-emoji names: %v (an over-NAME_MAX basename would abort AFTER the old vault was cleaned — data loss)", err)
	}
	// e-u1 is the hub (degree 2) — its emoji name reaches concepts/ and, as
	// the top-degree member of the 3-concept component, the maps/ title too.
	if stats.Events != 2 || stats.Concepts != 1 || stats.Maps != 1 {
		t.Errorf("stats = %+v, want 2 events / 1 concept / 1 map", stats)
	}

	maxLen := 0
	stressed := false
	for rel := range vaultTree(t, dir) {
		base := rel[strings.LastIndex(rel, "/")+1:]
		if n := len([]byte(base)); n > maxLen {
			maxLen = n
		}
		if len([]byte(base)) > 255 {
			t.Errorf("basename %q is %d bytes, exceeds NAME_MAX 255", base, len([]byte(base)))
		}
		if len([]byte(base)) > 200 {
			stressed = true
		}
	}
	if !stressed {
		t.Errorf("no basename exceeded 200 bytes (max %d) — the fixture failed to stress the budget", maxLen)
	}
	t.Logf("worst-case basename = %d bytes (budget %d, NAME_MAX 255)", maxLen, maxNoteBaseBytes)
}

// --- DW-5.3: byte-identical re-runs ---

func TestDW_5_3_ReRunByteIdentical(t *testing.T) {
	episodics, entities, edges := richRecords()

	dirA, dirB := t.TempDir(), t.TempDir()
	if _, err := writeVault(dirA, episodics, entities, edges); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := writeVault(dirB, episodics, entities, edges); err != nil {
		t.Fatalf("second run: %v", err)
	}
	assertTreesEqual(t, vaultTree(t, dirA), vaultTree(t, dirB), "re-run over one export input")

	// Input record order must not leak into the output either.
	dirC := t.TempDir()
	if _, err := writeVault(dirC, reversed(episodics), reversed(entities), reversed(edges)); err != nil {
		t.Fatalf("reversed-input run: %v", err)
	}
	assertTreesEqual(t, vaultTree(t, dirA), vaultTree(t, dirC), "reversed input order")
}

// --- DW-5.5: stats from the assembly layer ---

func TestDW_5_5_StatsCounts(t *testing.T) {
	dir := t.TempDir()
	episodics, entities, edges := richRecords()
	// One extra half-dangling edge: ONE endpoint exported, so it lands as a
	// claim and must NOT count as dropped.
	edges = append(edges, edgS("ed4", "e-a", "e-unknown", "half-dangling claim"))
	stats, err := writeVault(dir, episodics, entities, edges)
	if err != nil {
		t.Fatalf("writeVault: %v", err)
	}
	want := vaultStats{Events: 2, Concepts: 1, Maps: 1, Ghosts: 2, Dropped: 1}
	if stats != want {
		t.Errorf("stats = %+v, want %+v (half-dangling edge must not be dropped)", stats, want)
	}
}
