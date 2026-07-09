//go:build e2e

package e2e

// Phase-3 export scenario pack (obsidian-vault-exporter). Registered from
// init() with zero edits to the harness core — the documented extension
// point. It drives the FULL stack: MCP ingest -> worker extract/reconcile ->
// graph stage upserts entities/edges -> `engram export <dir>` pages the
// Export RPC and renders the Obsidian vault, which is then machine-checked
// the way Obsidian would read it (DW-3.1, DW-3.2, DW-3.4, DW-3.5).

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v2"
)

func init() {
	RegisterScenario(Scenario{Name: "export/obsidian-vault", Run: exportObsidianVault})
}

var (
	exportWikilinkRe = regexp.MustCompile(`\[\[([^\[\]|]+)\|([^\[\]]*)\]\]`)
	exportCountsRe   = regexp.MustCompile(`exported (\d+) entities to .* \(\d+ edges, (\d+) dropped\)`)
)

// exportObsidianVault ingests two chained facts (A works_at B, B located_in
// C), exports a vault, and asserts this phase's contract: one .md per
// EXPORTED entity (note count == the printed entity count) with an H1 and
// YAML-parseable frontmatter (aliases/mention_count/provenance keys), edge
// bullets as piped wikilinks whose targets all resolve to real note files on
// disk, the printed dropped-edge count, and a clobber-and-regenerate re-run
// on the tool-owned dir without --force.
//
// Entity B may or may not dedup-merge across the two events depending on
// upstream index-refresh timing (a graph-stage property, not this phase's);
// when it doesn't, B exports twice and the exporter must give both notes
// deterministic id-suffixed homonym filenames — so B is asserted by prefix,
// never by exact filename or an exact note count.
func exportObsidianVault(h *Harness) error {
	tenant := uniqueTenant("export-vault")
	raw, _, err := h.MintToken(tenant, "u1", "a1", time.Hour)
	if err != nil {
		return err
	}
	marker := strings.ReplaceAll(tenant, "-", "")
	a, b, c := "A"+marker, "B"+marker, "C"+marker

	sess, err := h.MCP(raw)
	if err != nil {
		return err
	}
	defer sess.Close()
	if _, err := sess.Initialize(); err != nil {
		return err
	}
	if _, err := sess.CallTool("memory_ingest", map[string]any{
		"event_id": tenant + "-e1",
		"text":     fmt.Sprintf("fact: %s | works_at | %s", a, b),
	}); err != nil {
		return err
	}
	if err := waitForHit(sess, a+" works_at "+b, b); err != nil {
		return fmt.Errorf("A-B fact never landed: %w", err)
	}
	if _, err := sess.CallTool("memory_ingest", map[string]any{
		"event_id": tenant + "-e2",
		"text":     fmt.Sprintf("fact: %s | located_in | %s", b, c),
	}); err != nil {
		return err
	}
	if err := waitForHit(sess, b+" located_in "+c, c); err != nil {
		return fmt.Errorf("B-C fact never landed: %w", err)
	}

	root, err := os.MkdirTemp("", "engram-export-e2e-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	vault := filepath.Join(root, "vault")
	env := map[string]string{"ENGRAM_TOKEN": raw}

	worksAtRe := regexp.MustCompile(`- works_at \[\[` + regexp.QuoteMeta(b) + `[^\[\]|]*\|`)
	locatedInRe := regexp.MustCompile(`- located_in \[\[` + regexp.QuoteMeta(c) + `\|`)

	// The graph stage upserts asynchronously after facts land; re-export
	// until both entities and both edge bullets appear (re-running is itself
	// the DW-3.4 owned-dir clobber-and-regenerate path — no --force).
	deadline := time.Now().Add(30 * time.Second)
	var out string
	var notes map[string]string
	for {
		out, err = h.RunCLI(env, "export", vault)
		if err != nil {
			return fmt.Errorf("export CLI failed: %w (%s)", err, out)
		}
		notes, err = exportReadVault(vault)
		if err != nil {
			return err
		}
		_, okA := notes[a]
		_, okC := notes[c]
		if okA && okC && worksAtRe.MatchString(notes[a]) && locatedInRe.MatchString(allNotes(notes)) {
			break
		}
		if time.Now().After(deadline) {
			names := make([]string, 0, len(notes))
			for n := range notes {
				names = append(names, n)
			}
			return fmt.Errorf("vault never reached the expected shape (notes for %s and %s plus both edge bullets); have %v; last output: %s", a, c, names, out)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Printed counts (DW-3.2), and "one .md per exported entity" (DW-3.1):
	// the note count on disk must equal the printed exported-entity count.
	m := exportCountsRe.FindStringSubmatch(out)
	if m == nil {
		return fmt.Errorf("export output missing the entity/edge/dropped report: %q", out)
	}
	if n, _ := strconv.Atoi(m[1]); n != len(notes) {
		return fmt.Errorf("printed %s entities but the vault has %d notes", m[1], len(notes))
	}
	if m[2] != "0" {
		return fmt.Errorf("dropped = %s, want 0 (every edge endpoint was exported): %q", m[2], out)
	}

	for name, content := range notes {
		// Frontmatter parses as valid YAML with the DW-3.1 keys (DW-3.5).
		fm, err := exportParseFrontmatter(content)
		if err != nil {
			return fmt.Errorf("note %s: %w", name, err)
		}
		for _, k := range []string{"engram_id", "aliases", "mention_count", "scope"} {
			if _, ok := fm[k]; !ok {
				return fmt.Errorf("note %s frontmatter missing key %q:\n%s", name, k, content)
			}
		}
		if !strings.Contains(content, "\n# ") {
			return fmt.Errorf("note %s missing its H1:\n%s", name, content)
		}
		// Every wikilink target resolves to a real note file on disk (DW-3.5).
		for _, l := range exportWikilinkRe.FindAllStringSubmatch(content, -1) {
			if _, ok := notes[l[1]]; !ok {
				return fmt.Errorf("note %s links to %q which is not a note file on disk", name, l[1])
			}
		}
	}

	// A second run on the now-owned dir must succeed without --force and
	// leave a consistent vault (DW-3.4 live).
	out2, err := h.RunCLI(env, "export", vault)
	if err != nil {
		return fmt.Errorf("re-export of owned dir failed: %w (%s)", err, out2)
	}
	if !exportCountsRe.MatchString(out2) {
		return fmt.Errorf("re-export output missing the report: %q", out2)
	}
	return nil
}

// allNotes concatenates every note body (for whole-vault content matches).
func allNotes(notes map[string]string) string {
	var b strings.Builder
	for _, content := range notes {
		b.WriteString(content)
		b.WriteString("\n")
	}
	return b.String()
}

// exportReadVault returns filename(without .md) -> content for every note in
// dir; a missing dir returns an empty map (export may not have run yet).
func exportReadVault(dir string) (map[string]string, error) {
	notes := map[string]string{}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return notes, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		notes[strings.TrimSuffix(e.Name(), ".md")] = string(b)
	}
	return notes, nil
}

// exportParseFrontmatter extracts the leading --- block and YAML-parses it.
func exportParseFrontmatter(content string) (map[string]any, error) {
	if !strings.HasPrefix(content, "---\n") {
		return nil, fmt.Errorf("no leading frontmatter block")
	}
	rest := content[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return nil, fmt.Errorf("frontmatter block not terminated")
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(rest[:end+1]), &m); err != nil {
		return nil, fmt.Errorf("frontmatter is not valid YAML: %w", err)
	}
	return m, nil
}
