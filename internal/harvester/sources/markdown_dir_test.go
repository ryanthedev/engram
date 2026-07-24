package sources_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/harvester"
	"github.com/ryanthedev/engram/internal/mcp"
	yaml "go.yaml.in/yaml/v2"
	"google.golang.org/protobuf/types/known/structpb"
)

// createNoteRoot writes a directory of notes (relative path -> file content)
// under a fresh temp dir and returns the root's absolute path.
func createNoteRoot(t *testing.T, brain string, files map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), brain)
	writeNotes(t, root, files)
	return root
}

// createMalformedNoteRoot is createNoteRoot for the lenient-fallback tests: it
// PROVES each fixture's frontmatter is genuinely rejected by yaml v2 before
// writing it, so a test claiming to exercise the fallback cannot silently be
// asserting against the strict path instead.
//
// This guard is not paranoia, it is a scar. Writing a "broken" fixture that yaml
// v2 happily accepts is easy, because YAML quoting spans lines: `name: "` does
// NOT leave a dangling quote character, it OPENS a multi-line double-quoted
// scalar that closes at the next `"` anywhere below and folds every line between
// into one value. A fixture written that way parses cleanly, the fallback never
// runs, and the expectations end up pinned to whatever the strict path happened
// to produce.
func createMalformedNoteRoot(t *testing.T, brain string, files map[string]string) string {
	t.Helper()
	for rel, content := range files {
		block, ok := frontmatterBlockOf(content)
		if !ok {
			t.Fatalf("fixture %s: no delimited frontmatter block, so it cannot reach the lenient fallback", rel)
		}
		var raw map[interface{}]interface{}
		if err := yaml.Unmarshal([]byte(block), &raw); err == nil {
			t.Fatalf("fixture %s: frontmatter was meant to be unparseable but yaml v2 accepted it as %#v — this fixture exercises the STRICT path, not the lenient fallback", rel, raw)
		}
	}
	return createNoteRoot(t, brain, files)
}

// frontmatterBlockOf extracts the YAML between a fixture's opening `---` line
// and its closing `---`/`...`, mirroring the source's own block extent (CRLF
// included, so a CRLF fixture is checked as the source would see it).
func frontmatterBlockOf(content string) (string, bool) {
	lines := strings.SplitAfter(content, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r\n") != "---" {
		return "", false
	}
	var block strings.Builder
	for _, line := range lines[1:] {
		switch strings.TrimRight(line, "\r\n") {
		case "---", "...":
			return block.String(), true
		}
		block.WriteString(line)
	}
	return "", false
}

// writeNotes writes each relative path under root, creating parent directories.
func writeNotes(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("failed to create dir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("failed to write %s: %v", rel, err)
		}
	}
}

// buildScopedMarkdown builds the markdown-dir source and asserts it is a
// ScopedSource (per-root mark-and-sweep scopes).
func buildScopedMarkdown(t *testing.T, cfg harvester.SourceConfig) harvester.ScopedSource {
	t.Helper()
	src, err := harvester.Build(cfg, harvester.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}
	scoped, ok := src.(harvester.ScopedSource)
	if !ok {
		t.Fatalf("markdown-dir source does not implement harvester.ScopedSource")
	}
	return scoped
}

// harvestNotes builds a markdown-dir source over roots and runs a full harvest,
// returning the emitted docs keyed by document id.
func harvestNotes(t *testing.T, raw map[string]any) map[string]mcp.KnowledgeDoc {
	t.Helper()
	docs, _ := harvestNotesLogged(t, raw)
	return docs
}

// harvestNotesLogged is harvestNotes plus the harvest's captured log output.
//
// The log is not decoration here, it is the ONLY externally visible record of
// how a malformed note degraded: buildDoc reads exactly four keys, so whether a
// broken block recovered nothing, some, or all of its metadata is otherwise
// indistinguishable from the emitted document. The handler is pinned at Debug so
// a record that got DEMOTED still shows up and fails on its level rather than
// vanishing and failing as "no record at all".
func harvestNotesLogged(t *testing.T, raw map[string]any) (map[string]mcp.KnowledgeDoc, string) {
	t.Helper()
	var logs bytes.Buffer
	src, err := harvester.Build(
		harvester.SourceConfig{Type: "markdown-dir", Raw: raw},
		harvester.Deps{Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))},
	)
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}
	sink := &testSink{}
	if err := src.Harvest(context.Background(), sink); err != nil {
		t.Fatalf("Harvest failed: %v", err)
	}
	byID := make(map[string]mcp.KnowledgeDoc, len(sink.docs))
	for _, doc := range sink.docs {
		byID[doc.ID] = doc
	}
	return byID, logs.String()
}

// logRecordForPath returns the one captured record whose `path` attribute is
// rel, failing if there is not exactly one. Matching on the attribute rather
// than the message keeps the assertions off the log's PROSE — the wording of
// these messages is free to change, their level and attributes are the contract.
func logRecordForPath(t *testing.T, logs, rel string) string {
	t.Helper()
	var matched []string
	for _, line := range strings.Split(strings.TrimRight(logs, "\n"), "\n") {
		if strings.Contains(line, " path="+rel+" ") || strings.HasSuffix(line, " path="+rel) {
			matched = append(matched, line)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("expected exactly 1 log record for path %q, got %d, in:\n%s", rel, len(matched), logs)
	}
	return matched[0]
}

// TestMarkdownDirRegisteredType asserts the source type is registered, which is
// what makes it usable from a manifest (validation is registry-driven).
func TestMarkdownDirRegisteredType(t *testing.T) {
	root := createNoteRoot(t, "self", map[string]string{"note.md": "hello"})
	src, err := harvester.Build(
		harvester.SourceConfig{Type: "markdown-dir", Raw: map[string]any{"roots": []any{root}}},
		harvester.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
	)
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}
	if got := src.Type(); got != "markdown-dir" {
		t.Errorf("expected type markdown-dir, got %q", got)
	}
	if got := src.Mode(); got != harvester.FullHarvest {
		t.Errorf("expected FullHarvest mode, got %v", got)
	}
}

// TestMarkdownDirParsesFrontmatter asserts the whole emitted document contract
// for a note with complete frontmatter: id, title, frontmatter-stripped body,
// content-hash source_version, and every field the `notes` collection declares.
func TestMarkdownDirParsesFrontmatter(t *testing.T) {
	root := createNoteRoot(t, "self", map[string]string{
		"ufos/publishing-the-uap-archive-site.md": "---\n" +
			"name: Publishing the UAP archive site\n" +
			"type: project-log\n" +
			"date: \"2026-06-02\"\n" +
			"description: How the archive got published\n" +
			"---\n" +
			"Body line one.\n",
	})

	docs := harvestNotes(t, map[string]any{"roots": []any{map[string]any{"path": root, "brain": "self"}}})
	doc, ok := docs["self/ufos/publishing-the-uap-archive-site.md"]
	if !ok {
		t.Fatalf("expected doc id self/ufos/publishing-the-uap-archive-site.md, got %v", keysOf(docs))
	}

	if doc.Title != "Publishing the UAP archive site" {
		t.Errorf("expected title from frontmatter name, got %q", doc.Title)
	}
	if doc.Text != "Body line one.\n" {
		t.Errorf("expected frontmatter-stripped body, got %q", doc.Text)
	}
	if !strings.HasPrefix(doc.SourceVersion, "sha256:") || len(doc.SourceVersion) != len("sha256:")+16 {
		t.Errorf("expected sha256:<16 hex> source_version, got %q", doc.SourceVersion)
	}

	want := map[string]any{
		"brain":       "self",
		"category":    "ufos",
		"note_type":   "project-log",
		"date":        "2026-06-02",
		"path":        "ufos/publishing-the-uap-archive-site.md",
		"description": "How the archive got published",
	}
	assertFields(t, doc, want)
}

// TestMarkdownDirNormalizesTimeDate is the trap-1 guard: yaml v2 resolves an
// UNQUOTED `date: 2026-06-02` to a time.Time, which structpb.NewStruct rejects
// outright and which would fail the whole ingest batch. The emitted value must
// be a YYYY-MM-DD string.
func TestMarkdownDirNormalizesTimeDate(t *testing.T) {
	root := createNoteRoot(t, "self", map[string]string{
		"logs/unquoted.md":  "---\ndate: 2026-06-02\n---\nbody\n",
		"logs/timestamp.md": "---\ndate: 2026-06-02T13:45:00Z\n---\nbody\n",
	})

	docs := harvestNotes(t, map[string]any{"roots": []any{root}})
	for _, id := range []string{"self/logs/unquoted.md", "self/logs/timestamp.md"} {
		doc, ok := docs[id]
		if !ok {
			t.Fatalf("expected doc %q, got %v", id, keysOf(docs))
		}
		date, isString := doc.Fields["date"].(string)
		if !isString {
			t.Fatalf("doc %q date must be a string, got %T (%v)", id, doc.Fields["date"], doc.Fields["date"])
		}
		if date != "2026-06-02" {
			t.Errorf("doc %q: expected date 2026-06-02, got %q", id, date)
		}
		if _, err := structpb.NewStruct(doc.Fields); err != nil {
			t.Fatalf("doc %q fields are not structpb-encodable: %v", id, err)
		}
	}
}

// TestMarkdownDirWithoutFrontmatter asserts a note with NO frontmatter block —
// either none at all or an unterminated one — degrades gracefully instead of
// aborting the run: with no block extent to strip, the whole file is the body
// and the relative path is the title.
//
// A block that IS correctly delimited but whose YAML is broken is a different
// case entirely and is covered by the lenient-recovery tests below: there the
// body excludes the block and the known keys are salvaged.
func TestMarkdownDirWithoutFrontmatter(t *testing.T) {
	root := createNoteRoot(t, "self", map[string]string{
		"plain.md":        "# Plain note\n\nNo frontmatter here.\n",
		"unterminated.md": "---\nname: never closed\nbody text\n",
	})

	docs := harvestNotes(t, map[string]any{"roots": []any{root}})
	for _, id := range []string{"self/plain.md", "self/unterminated.md"} {
		doc, ok := docs[id]
		if !ok {
			t.Fatalf("expected doc %q, got %v", id, keysOf(docs))
		}
		rel := strings.TrimPrefix(id, "self/")
		if doc.Title != rel {
			t.Errorf("doc %q: expected title to fall back to the relative path, got %q", id, doc.Title)
		}
		if doc.Text == "" {
			t.Errorf("doc %q: expected the whole file as text", id)
		}
		for _, omitted := range []string{"note_type", "date", "description", "category"} {
			if _, present := doc.Fields[omitted]; present {
				t.Errorf("doc %q: expected %q to be omitted, got %v", id, omitted, doc.Fields[omitted])
			}
		}
		assertFields(t, doc, map[string]any{"brain": "self", "path": rel})
	}
}

// TestMarkdownDirLenientFrontmatterRecovery covers the three malformed shapes
// actually observed in the wild (8 of 376 real notes), all of them the same YAML
// rule biting a human-written scalar that needed quoting. Before the lenient
// fallback these notes were emitted with NO metadata at all, which made them
// invisible to a `note_type` filter even though their body indexed fine.
//
// Each fixture is verified unparseable by yaml v2 at write time by
// createMalformedNoteRoot (errors observed: "mapping values are not allowed in
// this context" and "did not find expected key"), so these genuinely exercise
// the fallback and not the strict path.
func TestMarkdownDirLenientFrontmatterRecovery(t *testing.T) {
	root := createMalformedNoteRoot(t, "self", map[string]string{
		// Shape 1: a ": " inside an unquoted value ends the scalar mid-line.
		"gates/colon-in-value.md": "---\n" +
			"name: Gate note\n" +
			"type: eval\n" +
			"description: post-gate-agent.md edited so ... Isolation: base+skill catches 6/6\n" +
			"---\nbody one\n",
		// Shape 2: same rule, this time in the title, which must survive WHOLE —
		// recovering only "Raw research" would silently truncate the note's name.
		"research/colon-in-name.md": "---\n" +
			"name: Raw research: Managed Agents self-hosted counter (verbatim)\n" +
			"type: research\n" +
			"---\nbody two\n",
		// Shape 3: a leading double quote that is prose, not a quoting construct.
		"notes/leading-quote.md": "---\n" +
			"description: \"Be concise\" can't control a reasoning model's token count\n" +
			"type: note\n" +
			"---\nbody three\n",
	})

	docs := harvestNotes(t, map[string]any{"roots": []any{root}})

	cases := []struct {
		id        string
		wantTitle string
		wantField map[string]any
	}{
		{
			id:        "self/gates/colon-in-value.md",
			wantTitle: "Gate note",
			wantField: map[string]any{
				"note_type":   "eval",
				"description": "post-gate-agent.md edited so ... Isolation: base+skill catches 6/6",
			},
		},
		{
			id:        "self/research/colon-in-name.md",
			wantTitle: "Raw research: Managed Agents self-hosted counter (verbatim)",
			wantField: map[string]any{"note_type": "research"},
		},
		{
			id:        "self/notes/leading-quote.md",
			wantTitle: "notes/leading-quote.md", // no `name` key in this note
			wantField: map[string]any{
				"note_type":   "note",
				"description": `"Be concise" can't control a reasoning model's token count`,
			},
		},
	}
	for _, tc := range cases {
		doc, ok := docs[tc.id]
		if !ok {
			t.Fatalf("expected doc %q, got %v", tc.id, keysOf(docs))
		}
		if doc.Title != tc.wantTitle {
			t.Errorf("doc %q: title = %q, want %q", tc.id, doc.Title, tc.wantTitle)
		}
		assertFields(t, doc, tc.wantField)
		if _, err := structpb.NewStruct(doc.Fields); err != nil {
			t.Fatalf("doc %q: leniently-parsed fields are not structpb-encodable: %v", tc.id, err)
		}
		assertDeclaredFieldsOnly(t, doc)
	}
}

// TestMarkdownDirLenientBodyExcludesFrontmatter is the regression guard for the
// second bug the fallback fixed: the old code returned the WHOLE file (fence,
// YAML and all) as the body whenever yaml.Unmarshal failed, so a broken note's
// indexed text carried its own frontmatter as prose. The block's extent is known
// — it was correctly delimited, only its YAML was bad — so the strict and
// lenient paths must agree on the body.
func TestMarkdownDirLenientBodyExcludesFrontmatter(t *testing.T) {
	root := createMalformedNoteRoot(t, "self", map[string]string{
		"broken.md": "---\n" +
			"name: Broken but delimited\n" +
			"description: has a stray Colon: right here\n" +
			"---\n" +
			"Real body starts here.\n",
	})

	doc, ok := harvestNotes(t, map[string]any{"roots": []any{root}})["self/broken.md"]
	if !ok {
		t.Fatal("expected doc self/broken.md")
	}
	if doc.Text != "Real body starts here.\n" {
		t.Errorf("expected frontmatter-stripped body, got %q", doc.Text)
	}
	if strings.Contains(doc.Text, "---") || strings.Contains(doc.Text, "name:") {
		t.Errorf("lenient body still contains the frontmatter block: %q", doc.Text)
	}
}

// TestMarkdownDirLenientDateNormalized asserts a `date` recovered leniently goes
// through the SAME normalization contract as the strict path: emitted as a
// YYYY-MM-DD string, omitted entirely when it will not parse. The lenient path
// hands over a raw string rather than yaml v2's time.Time, so this proves the
// value is routed through the shared helper and not passed through untouched —
// an unnormalized value would fail the typed `date` mapping at ingest.
func TestMarkdownDirLenientDateNormalized(t *testing.T) {
	root := createMalformedNoteRoot(t, "self", map[string]string{
		"logs/unquoted.md": "---\ndate: 2026-06-02\nname: Broken: unquoted date\n---\nbody\n",
		"logs/quoted.md":   "---\ndate: \"2026-06-02\"\nname: Broken: quoted date\n---\nbody\n",
		"logs/slashed.md":  "---\ndate: 2026/06/02\nname: Broken: slashed date\n---\nbody\n",
		"logs/garbage.md":  "---\ndate: sometime last summer\nname: Broken: no date\n---\nbody\n",
	})

	docs := harvestNotes(t, map[string]any{"roots": []any{root}})
	for _, id := range []string{"self/logs/unquoted.md", "self/logs/quoted.md", "self/logs/slashed.md"} {
		doc, ok := docs[id]
		if !ok {
			t.Fatalf("expected doc %q, got %v", id, keysOf(docs))
		}
		date, isString := doc.Fields["date"].(string)
		if !isString {
			t.Fatalf("doc %q: date must be a string, got %T (%v)", id, doc.Fields["date"], doc.Fields["date"])
		}
		if date != "2026-06-02" {
			t.Errorf("doc %q: date = %q, want 2026-06-02", id, date)
		}
		if _, err := structpb.NewStruct(doc.Fields); err != nil {
			t.Fatalf("doc %q: fields are not structpb-encodable: %v", id, err)
		}
	}
	if value, present := docs["self/logs/garbage.md"].Fields["date"]; present {
		t.Errorf("expected an unnormalizable lenient date to be omitted, got %#v", value)
	}
	// The rest of the block must still be recovered even when `date` is dropped.
	assertFields(t, docs["self/logs/garbage.md"], map[string]any{"path": "logs/garbage.md"})
	if got := docs["self/logs/garbage.md"].Title; got != "Broken: no date" {
		t.Errorf("expected the name to survive a dropped date, got %q", got)
	}
}

// TestMarkdownDirLenientRecoversNothing asserts a block that is unparseable AND
// yields no known key still degrades gracefully rather than aborting the run:
// no metadata fields, path as title, and — unlike the old behaviour — a body
// that still excludes the frontmatter block.
//
// The indentation fixture is the point of the column-0 rule: an indented
// `description:` belongs to some enclosing structure, not to the top level, so
// consuming it would attribute another key's text to a document field.
func TestMarkdownDirLenientRecoversNothing(t *testing.T) {
	root := createMalformedNoteRoot(t, "self", map[string]string{
		"junk.md":     "---\n[unclosed\n  - stray\n\tnope\n---\nbody\n",
		"indented.md": "---\nmeta: [oops\n  name: nested not top level\n  description: also nested\n---\nbody\n",
	})

	docs := harvestNotes(t, map[string]any{"roots": []any{root}})
	for _, id := range []string{"self/junk.md", "self/indented.md"} {
		doc, ok := docs[id]
		if !ok {
			t.Fatalf("expected doc %q, got %v", id, keysOf(docs))
		}
		rel := strings.TrimPrefix(id, "self/")
		if doc.Title != rel {
			t.Errorf("doc %q: expected title to fall back to the relative path, got %q", id, doc.Title)
		}
		if doc.Text != "body\n" {
			t.Errorf("doc %q: expected frontmatter-stripped body, got %q", id, doc.Text)
		}
		for _, omitted := range []string{"note_type", "date", "description", "category"} {
			if _, present := doc.Fields[omitted]; present {
				t.Errorf("doc %q: expected %q to be omitted, got %v", id, omitted, doc.Fields[omitted])
			}
		}
		assertFields(t, doc, map[string]any{"brain": "self", "path": rel})
	}
}

// TestMarkdownDirLenientLogsRecoveryOutcome pins the operator-facing half of the
// fallback: a block that recovers NOTHING logs at WARN, and one that recovers
// keys logs at INFO carrying how many.
//
// This is the only signal that a harvest silently lost a note's metadata. The
// documents themselves cannot carry it — a note whose `name` was salvaged and a
// note that never had one emit the same shape — so without this test the
// levels could invert, or the count go wrong, and every other test here would
// still pass.
//
// The assertions are on the LEVEL and the recovered_keys attribute only; the
// message wording is deliberately not pinned.
func TestMarkdownDirLenientLogsRecoveryOutcome(t *testing.T) {
	root := createMalformedNoteRoot(t, "self", map[string]string{
		// No column-0 `key: value` line at all, so nothing is recoverable.
		"nothing.md": "---\n[unclosed\n  - stray\n---\nbody\n",
		// `name` and `type` recover; the block breaks on the colon in the name.
		"recovered.md": "---\nname: Recovered: title\ntype: log\n---\nbody\n",
	})

	docs, logs := harvestNotesLogged(t, map[string]any{"roots": []any{root}})

	// Tie each log line to what the document actually got, so a passing
	// assertion cannot describe a recovery that did not happen.
	nothing := logRecordForPath(t, logs, "nothing.md")
	if got := docs["self/nothing.md"].Title; got != "nothing.md" {
		t.Errorf("expected no recovered name, got title %q", got)
	}
	if !strings.Contains(nothing, "level=WARN") {
		t.Errorf("a block that recovered nothing must log at WARN, got: %s", nothing)
	}
	if strings.Contains(nothing, "recovered_keys") {
		t.Errorf("a block that recovered nothing must not report a recovered count, got: %s", nothing)
	}

	recovered := logRecordForPath(t, logs, "recovered.md")
	if got := docs["self/recovered.md"].Title; got != "Recovered: title" {
		t.Errorf("expected the name to be recovered, got title %q", got)
	}
	assertFields(t, docs["self/recovered.md"], map[string]any{"note_type": "log"})
	if !strings.Contains(recovered, "level=INFO") {
		t.Errorf("a block that recovered keys must log at INFO, got: %s", recovered)
	}
	if !strings.Contains(recovered, "recovered_keys=2") {
		t.Errorf("expected recovered_keys=2 for a block yielding name+type, got: %s", recovered)
	}
}

// TestMarkdownDirLenientUnbalancedQuotes asserts the quote rule in both
// directions: a MATCHING surrounding pair is stripped exactly once, and every
// other arrangement of quote characters is kept verbatim rather than
// half-stripped or dropped.
//
// Fixture ORDER matters here and is the reason the lone-quote note puts its
// dangling `"` on the LAST key. A quote character does not stay on its own line
// in YAML: `name: "` opens a multi-line double-quoted scalar that swallows
// everything down to the next `"`. Placed first, it would close against a later
// line's quote and the whole block would parse — testing the strict path by
// accident. Placed last, there is no closing quote left and the block genuinely
// fails, which is what createMalformedNoteRoot enforces.
func TestMarkdownDirLenientUnbalancedQuotes(t *testing.T) {
	root := createMalformedNoteRoot(t, "self", map[string]string{
		// An opening quote with no partner anywhere in the block.
		"quotes.md": "---\n" +
			"description: \"also unbalanced\n" +
			"type: note: with colon\n" +
			"name: 'unbalanced quote\n" +
			"---\nbody\n",
		// A mismatched pair, then a value that is nothing but one quote character.
		"lone.md": "---\n" +
			"description: 'mismatched pair\"\n" +
			"type: x\n" +
			"name: \"\n" +
			"---\nbody\n",
		// The positive half of the rule: real quoting inside a block broken
		// elsewhere still loses exactly one layer, as YAML would have done.
		"stripped.md": "---\n" +
			"name: 'quoted name'\n" +
			"description: \"quoted description\"\n" +
			"type: broken: here\n" +
			"---\nbody\n",
	})

	docs := harvestNotes(t, map[string]any{"roots": []any{root}})

	quotes, ok := docs["self/quotes.md"]
	if !ok {
		t.Fatalf("expected doc self/quotes.md, got %v", keysOf(docs))
	}
	if quotes.Title != "'unbalanced quote" {
		t.Errorf("expected an unbalanced quote kept verbatim, got %q", quotes.Title)
	}
	assertFields(t, quotes, map[string]any{
		"description": `"also unbalanced`,
		"note_type":   "note: with colon",
	})

	lone, ok := docs["self/lone.md"]
	if !ok {
		t.Fatalf("expected doc self/lone.md, got %v", keysOf(docs))
	}
	// A single `"` is shorter than a quote PAIR, so it is the value, not a
	// wrapper. Stripping it would empty the field and drop it entirely.
	if lone.Title != `"` {
		t.Errorf("expected a lone quote character kept as the value, got %q", lone.Title)
	}
	// Mismatched open/close quote characters are not a pair either.
	assertFields(t, lone, map[string]any{
		"description": `'mismatched pair"`,
		"note_type":   "x",
	})

	stripped, ok := docs["self/stripped.md"]
	if !ok {
		t.Fatalf("expected doc self/stripped.md, got %v", keysOf(docs))
	}
	if stripped.Title != "quoted name" {
		t.Errorf("expected one layer of single quotes stripped, got %q", stripped.Title)
	}
	assertFields(t, stripped, map[string]any{
		"description": "quoted description",
		"note_type":   "broken: here",
	})

	for _, doc := range []mcp.KnowledgeDoc{quotes, lone, stripped} {
		if _, err := structpb.NewStruct(doc.Fields); err != nil {
			t.Fatalf("doc %q: fields are not structpb-encodable: %v", doc.ID, err)
		}
		assertDeclaredFieldsOnly(t, doc)
	}
}

// TestMarkdownDirLenientDuplicateKeysFirstWins pins the documented tie-break:
// the FIRST occurrence of a recovered key wins. This deliberately diverges from
// yaml v2, which accepts a well-formed duplicate and keeps the LAST — in a block
// we already know is broken, a repeated key is more often debris from the first
// value than a real second assignment.
func TestMarkdownDirLenientDuplicateKeysFirstWins(t *testing.T) {
	root := createMalformedNoteRoot(t, "self", map[string]string{
		"dupes.md": "---\n" +
			"name: First name: wins\n" +
			"description: first description\n" +
			"name: second name loses\n" +
			"description: second description loses\n" +
			"---\nbody\n",
	})

	doc, ok := harvestNotes(t, map[string]any{"roots": []any{root}})["self/dupes.md"]
	if !ok {
		t.Fatal("expected doc self/dupes.md")
	}
	if doc.Title != "First name: wins" {
		t.Errorf("expected the first `name` to win, got %q", doc.Title)
	}
	assertFields(t, doc, map[string]any{"description": "first description"})
}

// TestMarkdownDirLenientEmptyValueDoesNotClaimKey pins the exception to
// first-wins: an EMPTY value never takes the key's slot, so a later occurrence
// with a real value still lands.
//
// `type:` above `type: gotcha` is the shape that motivated it — a plain
// first-wins rule recovers "" and drops note_type from a note that plainly
// declares one. Skipping empties also keeps the recovered COUNT honest, which is
// what the second fixture pins: a block whose only known keys are empty has
// recovered nothing and must say so at WARN, not claim two keys at INFO.
func TestMarkdownDirLenientEmptyValueDoesNotClaimKey(t *testing.T) {
	root := createMalformedNoteRoot(t, "self", map[string]string{
		// Every key appears empty first — bare, whitespace-only, and an empty
		// quoted pair — then again with the value the author meant.
		"empty-first.md": "---\n" +
			"name:\n" +
			"type:   \n" +
			"date:\n" +
			"description: \"\"\n" +
			"name: Real name\n" +
			"type: gotcha\n" +
			"date: 2026-06-02\n" +
			"description: Recovered: after the empties\n" +
			"---\nbody\n",
		// Known keys, no values anywhere: nothing usable was recovered.
		"all-empty.md": "---\nname:\ntype:\n[broken\n---\nbody\n",
	})

	docs, logs := harvestNotesLogged(t, map[string]any{"roots": []any{root}})

	doc, ok := docs["self/empty-first.md"]
	if !ok {
		t.Fatalf("expected doc self/empty-first.md, got %v", keysOf(docs))
	}
	if doc.Title != "Real name" {
		t.Errorf("expected the later non-empty name to win over the empty one, got %q", doc.Title)
	}
	assertFields(t, doc, map[string]any{
		"note_type":   "gotcha",
		"date":        "2026-06-02",
		"description": "Recovered: after the empties",
	})
	assertDeclaredFieldsOnly(t, doc)
	if _, err := structpb.NewStruct(doc.Fields); err != nil {
		t.Fatalf("doc %q: fields are not structpb-encodable: %v", doc.ID, err)
	}

	empty, ok := docs["self/all-empty.md"]
	if !ok {
		t.Fatalf("expected doc self/all-empty.md, got %v", keysOf(docs))
	}
	if empty.Title != "all-empty.md" {
		t.Errorf("expected an empty name to be no name at all, got title %q", empty.Title)
	}
	for _, omitted := range []string{"note_type", "date", "description"} {
		if value, present := empty.Fields[omitted]; present {
			t.Errorf("expected %q to be omitted, got %#v", omitted, value)
		}
	}
	if record := logRecordForPath(t, logs, "all-empty.md"); !strings.Contains(record, "level=WARN") {
		t.Errorf("empty values are not a recovery, so this must log at WARN, got: %s", record)
	}
}

// TestMarkdownDirLenientIgnoresIndentedFragments isolates the column-0 rule from
// the first-wins rule, which TestMarkdownDirLenientDuplicateKeysFirstWins would
// otherwise mask.
//
// The indented `name:` here comes BEFORE the real top-level one, so if indented
// lines were eligible at all, first-wins would hand the title to the debris. It
// is debris precisely because the line above it broke mid-scalar: everything
// indented under a mangled value is that value's spillover, not metadata. The
// unindented `Fix:` fragment covers the other half — it is at column 0 and looks
// exactly like a key, and the whitelist is what stops it entering the recovered
// map at all.
//
// Note what the whitelist does NOT change: buildDoc reads four keys and
// assertDeclaredFieldsOnly inspects the emitted Fields, so a recovered `Fix`
// would be dropped downstream regardless and no field assertion here can see the
// difference. The recovered_keys COUNT is the one place the whitelist is
// observable, which is why it is asserted — without it the whitelist has no test
// at all and could be deleted with the suite still green.
func TestMarkdownDirLenientIgnoresIndentedFragments(t *testing.T) {
	root := createMalformedNoteRoot(t, "self", map[string]string{
		"fragment.md": "---\n" +
			"description: The gate failed: rerun with\n" +
			"  name: debris from the previous value\n" +
			"  date: 1999-01-01\n" +
			"Fix: bump the timeout\n" +
			"name: Real name\n" +
			"---\nbody\n",
	})

	docs, logs := harvestNotesLogged(t, map[string]any{"roots": []any{root}})
	doc, ok := docs["self/fragment.md"]
	if !ok {
		t.Fatal("expected doc self/fragment.md")
	}
	if doc.Title != "Real name" {
		t.Errorf("expected the column-0 name to win over the indented fragment, got %q", doc.Title)
	}
	if value, present := doc.Fields["date"]; present {
		t.Errorf("expected the indented date to be ignored, got %#v", value)
	}
	assertFields(t, doc, map[string]any{"description": "The gate failed: rerun with"})
	assertDeclaredFieldsOnly(t, doc)
	// Exactly `description` and `name`: the two indented lines fail the column-0
	// rule and the column-0 `Fix:` fails the whitelist. A third key here means
	// the fallback is harvesting keys nobody declared.
	if record := logRecordForPath(t, logs, "fragment.md"); !strings.Contains(record, "recovered_keys=2") {
		t.Errorf("expected only the two whitelisted column-0 keys to be recovered, got: %s", record)
	}
}

// TestMarkdownDirLenientHandlesCRLF asserts the fallback strips the carriage
// return from CRLF fixtures. Without it every recovered value would carry a
// trailing \r: harmless-looking, but it would defeat normalizeFrontmatterDate
// (no layout matches "2026-06-02\r") and silently drop the date field.
func TestMarkdownDirLenientHandlesCRLF(t *testing.T) {
	root := createMalformedNoteRoot(t, "self", map[string]string{
		"crlf.md": "---\r\nname: Broken: crlf\r\ntype: note\r\ndate: 2026-06-02\r\n---\r\nbody\r\n",
	})

	doc, ok := harvestNotes(t, map[string]any{"roots": []any{root}})["self/crlf.md"]
	if !ok {
		t.Fatal("expected doc self/crlf.md")
	}
	if doc.Title != "Broken: crlf" {
		t.Errorf("expected the carriage return stripped from the name, got %q", doc.Title)
	}
	assertFields(t, doc, map[string]any{"note_type": "note", "date": "2026-06-02"})
	if doc.Text != "body\r\n" {
		t.Errorf("expected the CRLF body with the block stripped, got %q", doc.Text)
	}
	assertDeclaredFieldsOnly(t, doc)
}

// TestMarkdownDirOmitsUnusableDate asserts a missing or malformed `date` yields
// NO date field at all. The `notes` index is dynamic:strict with a typed date
// mapping, so an empty or garbage value is rejected per item, which surfaces as
// ErrPartialIngest and aborts the run before the sweep.
func TestMarkdownDirOmitsUnusableDate(t *testing.T) {
	root := createNoteRoot(t, "self", map[string]string{
		"no-date.md":      "---\nname: No date\ntype: note\n---\nbody\n",
		"empty-date.md":   "---\ndate: \"\"\n---\nbody\n",
		"garbage-date.md": "---\ndate: sometime last summer\n---\nbody\n",
	})

	docs := harvestNotes(t, map[string]any{"roots": []any{root}})
	for _, id := range []string{"self/no-date.md", "self/empty-date.md", "self/garbage-date.md"} {
		doc, ok := docs[id]
		if !ok {
			t.Fatalf("expected doc %q, got %v", id, keysOf(docs))
		}
		if value, present := doc.Fields["date"]; present {
			t.Errorf("doc %q: expected date to be omitted, got %#v", id, value)
		}
	}
}

// TestMarkdownDirRootLevelFileHasNoCategory asserts `category` (the containing
// subdirectory) is omitted for notes sitting directly in the root and set to the
// FIRST path segment for nested ones.
func TestMarkdownDirRootLevelFileHasNoCategory(t *testing.T) {
	root := createNoteRoot(t, "self", map[string]string{
		"top.md":              "top\n",
		"ufos/deep/nested.md": "nested\n",
		"projects/shallow.md": "shallow\n",
	})

	docs := harvestNotes(t, map[string]any{"roots": []any{root}})
	if _, present := docs["self/top.md"].Fields["category"]; present {
		t.Errorf("expected no category for a root-level note, got %v", docs["self/top.md"].Fields["category"])
	}
	if got, _ := docs["self/ufos/deep/nested.md"].Fields["category"].(string); got != "ufos" {
		t.Errorf("expected category ufos for a deeply nested note, got %q", got)
	}
	if got, _ := docs["self/projects/shallow.md"].Fields["category"].(string); got != "projects" {
		t.Errorf("expected category projects, got %q", got)
	}
}

// TestMarkdownDirExcludeGlobs asserts `exclude` skips whole subtrees (a trailing
// "/**") as well as individual root-relative files, and that `files` still gates
// which extensions are harvested.
func TestMarkdownDirExcludeGlobs(t *testing.T) {
	root := createNoteRoot(t, "self", map[string]string{
		".trash/old.md":        "trashed\n",
		".trash/2024/older.md": "deeply trashed\n",
		"recall.md":            "recall\n",
		"README.md":            "readme\n",
		"ufos/README.md":       "nested readme survives\n",
		"ufos/keep.md":         "keep\n",
		"notes.txt":            "not markdown\n",
	})

	docs := harvestNotes(t, map[string]any{
		"roots":   []any{root},
		"exclude": []any{".trash/**", "recall.md", "README.md"},
	})

	for _, gone := range []string{"self/.trash/old.md", "self/.trash/2024/older.md", "self/recall.md", "self/README.md", "self/notes.txt"} {
		if _, present := docs[gone]; present {
			t.Errorf("expected %q to be excluded, but it was harvested", gone)
		}
	}
	for _, kept := range []string{"self/ufos/keep.md", "self/ufos/README.md"} {
		if _, present := docs[kept]; !present {
			t.Errorf("expected %q to be harvested, got %v", kept, keysOf(docs))
		}
	}
}

// TestMarkdownDirSweepScopesPerRoot asserts each configured root becomes its own
// per-root sweep scope (markdown-dir:<absolute path>), deduplicated — so
// harvesting one brain never sweeps another brain's documents.
func TestMarkdownDirSweepScopesPerRoot(t *testing.T) {
	rootA := createNoteRoot(t, "self", map[string]string{"a.md": "a\n"})
	rootB := createNoteRoot(t, "work", map[string]string{"b.md": "b\n"})

	scoped := buildScopedMarkdown(t, harvester.SourceConfig{Type: "markdown-dir", Raw: map[string]any{
		// Duplicate rootA to prove scopes are deduplicated.
		"roots": []any{rootA, rootB, map[string]any{"path": rootA, "brain": "self"}},
	}})

	got := scoped.SweepScopes()
	want := []string{"markdown-dir:" + rootA, "markdown-dir:" + rootB}
	if len(got) != len(want) {
		t.Fatalf("expected %d deduplicated scopes, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("scope %d: expected %q, got %q", i, w, got[i])
		}
	}
}

// TestMarkdownDirHarvestScopeIsolation proves HarvestScope for one root emits
// ONLY that root's docs, so the Runner ingests and sweeps each root under its
// own source string.
func TestMarkdownDirHarvestScopeIsolation(t *testing.T) {
	rootA := createNoteRoot(t, "self", map[string]string{"a.md": "alpha\n"})
	rootB := createNoteRoot(t, "work", map[string]string{"b.md": "bravo\n"})

	scoped := buildScopedMarkdown(t, harvester.SourceConfig{Type: "markdown-dir", Raw: map[string]any{
		"roots": []any{rootA, rootB},
	}})

	sink := &testSink{}
	if err := scoped.HarvestScope(context.Background(), "markdown-dir:"+rootA, sink); err != nil {
		t.Fatalf("HarvestScope failed: %v", err)
	}
	if len(sink.docs) != 1 {
		t.Fatalf("expected exactly 1 doc from rootA scope, got %d", len(sink.docs))
	}
	if got := sink.docs[0].ID; got != "self/a.md" {
		t.Errorf("expected rootA doc, got %q", got)
	}
	if brain, _ := sink.docs[0].Fields["brain"].(string); brain != "self" {
		t.Errorf("expected brain self, got %q", brain)
	}
}

// TestMarkdownDirBrainDefaultsToBaseName asserts an entry given as a bare path
// string takes its brain (and therefore its id prefix) from the directory's base
// name.
func TestMarkdownDirBrainDefaultsToBaseName(t *testing.T) {
	root := createNoteRoot(t, "second-brain", map[string]string{"a.md": "a\n"})

	docs := harvestNotes(t, map[string]any{"roots": []any{root}})
	doc, ok := docs["second-brain/a.md"]
	if !ok {
		t.Fatalf("expected id prefixed with the root base name, got %v", keysOf(docs))
	}
	if brain, _ := doc.Fields["brain"].(string); brain != "second-brain" {
		t.Errorf("expected brain second-brain, got %q", brain)
	}
}

// TestMarkdownDirMissingRootFailsRun asserts a root that does not exist aborts
// the harvest instead of quietly emitting zero documents — an empty harvest of a
// Full-Harvest scope would let the sweep delete every document that root
// contributed.
func TestMarkdownDirMissingRootFailsRun(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there")
	src, err := harvester.Build(
		harvester.SourceConfig{Type: "markdown-dir", Raw: map[string]any{"roots": []any{missing}}},
		harvester.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
	)
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}
	if err := src.Harvest(context.Background(), &testSink{}); err == nil {
		t.Fatal("expected Harvest to fail for a missing root, got nil")
	}
}

// TestMarkdownDirConfigErrors covers the construction-time contract: `roots` is
// required and non-empty, and each entry must be a path string or an object
// carrying `path`.
func TestMarkdownDirConfigErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
	}{
		{"missing roots", map[string]any{}},
		{"empty roots", map[string]any{"roots": []any{}}},
		{"root not a string", map[string]any{"roots": []any{42}}},
		{"root object missing path", map[string]any{"roots": []any{map[string]any{"brain": "self"}}}},
		{"empty root path", map[string]any{"roots": []any{""}}},
		{"brain with separator", map[string]any{"roots": []any{map[string]any{"path": "/tmp/notes", "brain": "a/b"}}}},
		{"max_file_bytes not an int", map[string]any{"roots": []any{"/tmp/notes"}, "max_file_bytes": "big"}},
		{"files not a slice", map[string]any{"roots": []any{"/tmp/notes"}, "files": "*.md"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := harvester.Build(
				harvester.SourceConfig{Type: "markdown-dir", Raw: tc.raw},
				harvester.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
			)
			if err == nil {
				t.Fatalf("expected build to fail for %s", tc.name)
			}
		})
	}
}

// TestMarkdownDirDocFieldsStructpbEncodable is the structpb-encodability
// regression guard: KnowledgeDoc.Fields is wire-encoded via structpb.NewStruct
// in engramclient.KnowledgeIngest, which REJECTS a time.Time (yaml v2's decoding
// of an unquoted date) and a map[interface{}]interface{} (yaml v2's decoding of
// a nested mapping). A fake Sink never exercises that encoder, so this asserts
// the real docs a markdown-dir harvest emits round-trip through it.
// lenientEncodableNote is the malformed member of that test's fixture set. It is
// hoisted out because the fixture set is MIXED (valid notes alongside it), so
// createMalformedNoteRoot cannot vet the whole map — this one note is vetted on
// its own below, and the guarantee it carries (structpb-encodable, no undeclared
// keys) is worthless if the note quietly stops being malformed.
const lenientEncodableNote = "---\nname: Recovered: title\ntype: log\ndate: 2026-06-02\n" +
	"description: has a Colon: inside\n---\nbody\n"

func TestMarkdownDirDocFieldsStructpbEncodable(t *testing.T) {
	block, ok := frontmatterBlockOf(lenientEncodableNote)
	if !ok {
		t.Fatal("lenientEncodableNote has no delimited frontmatter block")
	}
	var probe map[interface{}]interface{}
	if err := yaml.Unmarshal([]byte(block), &probe); err == nil {
		t.Fatalf("lenientEncodableNote no longer exercises the lenient fallback: yaml v2 accepted it as %#v", probe)
	}

	root := createNoteRoot(t, "self", map[string]string{
		"ufos/sighting.md": "---\nname: Sighting\ntype: field-note\ndate: 2026-06-02\n" +
			"description: A description\nmeta:\n  nested: value\n  deeper:\n    key: 1\n" +
			"tags:\n  - one\n  - two\n---\nbody\n",
		"top.md":      "no frontmatter\n",
		"logs/odd.md": "---\ndate: not-a-date\ntype: 42\n---\nbody\n",
		// A leniently-recovered doc must clear the same bar: the fallback builds
		// its values by hand rather than via yaml v2, so nothing else proves it
		// never smuggles an unencodable value (or an undeclared key) into Fields.
		"logs/lenient.md": lenientEncodableNote,
	})

	scoped := buildScopedMarkdown(t, harvester.SourceConfig{Type: "markdown-dir", Raw: map[string]any{
		"roots": []any{root},
	}})

	sink := &testSink{}
	if err := scoped.HarvestScope(context.Background(), "markdown-dir:"+root, sink); err != nil {
		t.Fatalf("HarvestScope failed: %v", err)
	}
	if len(sink.docs) != 4 {
		t.Fatalf("expected 4 harvested docs, got %d", len(sink.docs))
	}
	for _, doc := range sink.docs {
		if _, err := structpb.NewStruct(doc.Fields); err != nil {
			t.Fatalf("doc %q Fields are not structpb-encodable (breaks KnowledgeIngest): %v", doc.ID, err)
		}
		assertDeclaredFieldsOnly(t, doc)
	}
}

// assertFields asserts every wanted field is present with the wanted value.
func assertFields(t *testing.T, doc mcp.KnowledgeDoc, want map[string]any) {
	t.Helper()
	for key, wantVal := range want {
		gotVal, present := doc.Fields[key]
		if !present {
			t.Errorf("doc %q: missing field %q", doc.ID, key)
			continue
		}
		if gotVal != wantVal {
			t.Errorf("doc %q: field %q = %#v, want %#v", doc.ID, key, gotVal, wantVal)
		}
	}
}

// assertDeclaredFieldsOnly asserts no field outside the `notes` collection's
// declared set is emitted: the index is dynamic:strict, so one undeclared key
// fails its item and aborts the run before the sweep.
func assertDeclaredFieldsOnly(t *testing.T, doc mcp.KnowledgeDoc) {
	t.Helper()
	declared := map[string]bool{
		"brain": true, "category": true, "note_type": true,
		"date": true, "path": true, "description": true,
	}
	for key, value := range doc.Fields {
		if !declared[key] {
			t.Errorf("doc %q: emitted undeclared field %q (%#v)", doc.ID, key, value)
		}
		if str, isString := value.(string); isString && str == "" {
			t.Errorf("doc %q: emitted empty string for field %q", doc.ID, key)
		}
	}
}

// keysOf lists harvested document ids for failure messages.
func keysOf(docs map[string]mcp.KnowledgeDoc) []string {
	ids := make([]string, 0, len(docs))
	for id := range docs {
		ids = append(ids, id)
	}
	return ids
}
