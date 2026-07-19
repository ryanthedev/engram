// export.go implements `engram export <dir>`: it drains the paginated Export
// RPC (Phase 2, via engramclient's transport-free ExportPage view) and
// renders the caller's tenant-scoped graph into an Obsidian-openable markdown
// vault — one note per entity (YAML frontmatter, H1, aliases) with edges as
// piped wikilink bullets.
//
// Security model: entity names, aliases, and predicates are UNTRUSTED
// ingested content that ends up in filesystem paths and link syntax.
// Sanitization happens once at the rendering barricade (sanitizeFilename /
// cleanInline), and path confinement is re-verified immediately before every
// write (confinedNotePath) — defense in depth on the one path that could
// escape <dir>. The clobber path is guarded twice: a foreign non-empty dir is
// refused without --force, and even --force never cleans the filesystem root
// or the user's home directory.

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	yaml "go.yaml.in/yaml/v2"

	"github.com/ryanthedev/engram/internal/engramclient"
)

// vaultMarker is the ownership sentinel written into every generated vault.
// Its presence is what lets a re-run clobber-and-regenerate without --force
// while foreign directories stay refused by default.
const vaultMarker = ".engram-vault"

// maxFilenameRunes caps note filenames well under common 255-byte filesystem
// limits (a rune is up to 4 bytes; the collision suffix and ".md" also fit).
const maxFilenameRunes = 60

// vaultStats summarizes one export for the final printed line.
type vaultStats struct {
	Entities int // notes written
	Edges    int // edge bullets rendered
	Dropped  int // edges dropped because an endpoint was not exported
}

// noteRef (the rendered identity of one note) is declared in vaultmodel.go,
// which generalizes this file's ref assignment across events and concepts.

// runExport handles `engram export [--force] <dir>`: parse flags, refuse a
// foreign target early, dial, drain every Export page, then clobber and
// regenerate the vault. The dir is cleaned only AFTER the fetch succeeds, so
// a failed export never destroys an existing vault.
func runExport(ctx context.Context, args []string, env Env, out io.Writer) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	force := fs.Bool("force", false, "clobber a non-empty directory not created by engram export")
	addr := fs.String("addr", "", "engramd address")
	token := fs.String("token", "", "bearer token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("export: expected a target <dir>")
	}
	dir := fs.Arg(0)
	// flag stops at the first positional; re-parse the tail so
	// `export <dir> --force` works as well as `export --force <dir>`.
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("export: expected exactly one <dir>")
	}

	// Fail fast on a foreign target before dialing anything.
	if err := checkVaultDir(dir, *force); err != nil {
		return err
	}
	client, err := dialClient(env, *addr, *token)
	if err != nil {
		return err
	}
	defer client.Close()
	entities, edges, err := fetchExport(ctx, client)
	if err != nil {
		return err
	}
	if err := prepareVaultDir(dir, *force); err != nil {
		return err
	}
	stats, err := writeVault(dir, entities, edges)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "exported %d entities to %s (%d edges, %d dropped)\n",
		stats.Entities, dir, stats.Edges, stats.Dropped)
	return nil
}

// fetchExport drains the paginated Export RPC, accumulating structured
// records until NextCursor is empty. It aborts if the server's cursor stops
// advancing (external input; would otherwise loop forever).
func fetchExport(ctx context.Context, client *engramclient.Client) ([]engramclient.ExportEntity, []engramclient.ExportEdge, error) {
	var entities []engramclient.ExportEntity
	var edges []engramclient.ExportEdge
	cursor := ""
	for {
		page, err := client.ExportPage(ctx, cursor)
		if err != nil {
			return nil, nil, fmt.Errorf("export: fetching page: %w", err)
		}
		entities = append(entities, page.Entities...)
		edges = append(edges, page.Edges...)
		if page.NextCursor == "" {
			return entities, edges, nil
		}
		if page.NextCursor == cursor {
			return nil, nil, errors.New("export: server cursor did not advance; aborting")
		}
		cursor = page.NextCursor
	}
}

// checkVaultDir decides whether dir may be (re)generated: missing or empty is
// fine, a marker-owned vault is fine, a foreign non-empty dir needs force.
func checkVaultDir(dir string, force bool) error {
	fi, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // created later
	}
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("export: %s exists and is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, vaultMarker)); err == nil {
		return nil // a vault this tool generated: safe to regenerate
	}
	if force {
		return nil
	}
	return fmt.Errorf("export: %s is not empty and was not created by engram export; re-run with --force to clobber it", dir)
}

// prepareVaultDir makes dir exist, empty, and marked as tool-owned. It
// re-runs checkVaultDir (the dir may have changed since the pre-dial check)
// and refuses catastrophic targets even under --force. It removes entries
// INSIDE dir, never dir itself.
func prepareVaultDir(dir string, force bool) error {
	if err := checkVaultDir(dir, force); err != nil {
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	home, _ := os.UserHomeDir()
	if isCatastrophicVaultDir(abs, home) {
		return fmt.Errorf("export: refusing to clobber %s", abs)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	for _, ent := range entries {
		if err := os.RemoveAll(filepath.Join(dir, ent.Name())); err != nil {
			return fmt.Errorf("export: cleaning %s: %w", dir, err)
		}
	}
	marker := "Generated by `engram export`. Everything in this directory is clobbered and regenerated on each export.\n"
	if err := os.WriteFile(filepath.Join(dir, vaultMarker), []byte(marker), 0o644); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	return nil
}

// isCatastrophicVaultDir reports whether abs is a directory that must never
// be cleaned, even with --force (filesystem root, the user's home). Pure so
// the guard is testable without pointing the cleaner at a real / or $HOME.
func isCatastrophicVaultDir(abs, home string) bool {
	abs = filepath.Clean(abs)
	if abs == filepath.Dir(abs) { // a filesystem root is its own parent
		return true
	}
	return home != "" && abs == filepath.Clean(home)
}

// vaultFilenames assigns each exported entity a deterministic, collision-free
// note filename. Case-insensitive homonyms ALL get an id-prefix suffix (not
// first-wins), so the assignment is independent of input order and stable
// across re-runs. Entities with an empty id are skipped: they cannot be
// linked or disambiguated deterministically.
func vaultFilenames(entities []engramclient.ExportEntity) map[string]noteRef {
	type cand struct {
		id, base string
		suffix   bool
	}
	// Pass 1: sanitize and count case-folded homonyms.
	cands := make([]cand, 0, len(entities))
	baseCount := make(map[string]int)
	for _, e := range entities {
		if e.ID == "" {
			continue
		}
		base := sanitizeFilename(e.Name)
		empty := base == ""
		if empty {
			base = "entity"
		}
		cands = append(cands, cand{id: e.ID, base: base, suffix: empty})
		baseCount[strings.ToLower(base)]++
	}
	// Pass 2: assign in sorted-id order (deterministic) with global
	// uniqueness; extend the id prefix on the (pathological) residual clash.
	sort.Slice(cands, func(i, j int) bool { return cands[i].id < cands[j].id })
	refs := make(map[string]noteRef, len(cands))
	used := make(map[string]bool, len(cands))
	nameByID := displayNames(entities)
	for _, c := range cands {
		name := c.base
		if c.suffix || baseCount[strings.ToLower(c.base)] > 1 {
			name = c.base + " (" + idPrefix(c.id, 8) + ")"
		}
		// Residual clashes (e.g. a literal name crafted to mimic a suffixed
		// one) extend the id prefix, then a counter — always terminates on an
		// unused name, deterministically (sorted-id assignment order).
		for n := 8; used[strings.ToLower(name)]; n += 4 {
			if n < len(c.id) {
				name = c.base + " (" + idPrefix(c.id, n+4) + ")"
			} else {
				name = fmt.Sprintf("%s (%s-%d)", c.base, c.id, n)
			}
		}
		used[strings.ToLower(name)] = true
		display := nameByID[c.id]
		if display == "" {
			display = name
		}
		refs[c.id] = noteRef{File: name, Display: display}
	}
	return refs
}

// displayNames maps entity id to its inline-cleaned display name.
func displayNames(entities []engramclient.ExportEntity) map[string]string {
	out := make(map[string]string, len(entities))
	for _, e := range entities {
		if e.ID != "" {
			out[e.ID] = cleanInline(e.Name)
		}
	}
	return out
}

// idPrefix returns the first n characters of id (all of it when shorter).
func idPrefix(id string, n int) string {
	if n >= len(id) {
		return id
	}
	return id[:n]
}

// fsIllegal are the runes stripped from filenames: path separators and
// FS-reserved characters, plus Obsidian link-syntax characters.
const fsIllegal = `/\:*?"<>|#^[]`

// sanitizeFilename strips FS/Obsidian-illegal characters from an untrusted
// entity name: illegal runes become '-', control characters are dropped,
// leading/trailing dots and spaces are trimmed (no hidden files, no '.'/'..'),
// and the result is length-capped. Returns "" when nothing safe survives
// (caller falls back to an id-derived name).
func sanitizeFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			// drop control chars (including NUL)
		case strings.ContainsRune(fsIllegal, r):
			b.WriteRune('-')
		default:
			b.WriteRune(r)
		}
	}
	s := strings.Trim(b.String(), ". ")
	if utf8.RuneCountInString(s) > maxFilenameRunes {
		runes := []rune(s)
		s = strings.Trim(string(runes[:maxFilenameRunes]), ". ")
	}
	return s
}

// cleanInline makes an untrusted string safe inside markdown link labels and
// headings: control characters and newlines collapse away, and the [ ] |
// runes that could forge or break wikilink syntax become '-'.
func cleanInline(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case r < 0x20 || r == 0x7f:
			// drop
		case r == '[' || r == ']' || r == '|':
			b.WriteRune('-')
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// renderNote renders one entity's complete note: YAML frontmatter (marshaled
// with a real YAML encoder — untrusted aliases are never hand-escaped), the
// H1, and the entity's outgoing edge bullets in deterministic order.
func renderNote(e engramclient.ExportEntity, outEdges []engramclient.ExportEdge, refs map[string]noteRef) (string, error) {
	aliases := e.Aliases
	if aliases == nil {
		aliases = []string{}
	}
	fm := yaml.MapSlice{
		{Key: "engram_id", Value: e.ID},
		{Key: "aliases", Value: aliases},
		{Key: "mention_count", Value: e.MentionCount},
		{Key: "scope", Value: e.Scope},
	}
	if e.TeamID != "" {
		fm = append(fm, yaml.MapItem{Key: "team_id", Value: e.TeamID})
	}
	if e.OwnerAgentID != "" {
		fm = append(fm, yaml.MapItem{Key: "owner_agent_id", Value: e.OwnerAgentID})
	}
	if len(e.SourceIDs) > 0 {
		fm = append(fm, yaml.MapItem{Key: "source_ids", Value: e.SourceIDs})
	}
	if e.ValidAt != nil {
		fm = append(fm, yaml.MapItem{Key: "valid_at", Value: e.ValidAt.UTC().Format(time.RFC3339)})
	}
	if e.CreatedAt != nil {
		fm = append(fm, yaml.MapItem{Key: "created_at", Value: e.CreatedAt.UTC().Format(time.RFC3339)})
	}
	fmBytes, err := yaml.Marshal(fm)
	if err != nil {
		return "", fmt.Errorf("export: rendering frontmatter for %s: %w", e.ID, err)
	}

	self := refs[e.ID]
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(fmBytes)
	b.WriteString("---\n\n# ")
	b.WriteString(self.Display)
	b.WriteString("\n")

	// Edge bullets, sorted for byte-identical re-runs.
	sorted := append([]engramclient.ExportEdge(nil), outEdges...)
	sort.Slice(sorted, func(i, j int) bool {
		a, z := sorted[i], sorted[j]
		if a.Predicate != z.Predicate {
			return a.Predicate < z.Predicate
		}
		if a.ToEntityID != z.ToEntityID {
			return a.ToEntityID < z.ToEntityID
		}
		return a.ID < z.ID
	})
	if len(sorted) > 0 {
		b.WriteString("\n")
	}
	for _, ed := range sorted {
		target := refs[ed.ToEntityID] // caller guarantees presence (danglers dropped)
		fmt.Fprintf(&b, "- %s [[%s|%s]]\n", cleanInline(ed.Predicate), target.File, target.Display)
	}
	return b.String(), nil
}

// confinedNotePath joins file onto dir and verifies the result stays strictly
// inside dir as a single flat path element. sanitizeFilename should make this
// unreachable; if it ever fires, that is a bug-stop error, never a write.
func confinedNotePath(dir, file string) (string, error) {
	// The raw input must already be a flat name: an absolute path or any
	// separator means a caller bypassed sanitization (bug), even where Join
	// would happen to neutralize it.
	if file == "" || filepath.IsAbs(file) || strings.ContainsAny(file, `/\`) {
		return "", fmt.Errorf("export: refusing to write outside the vault: %q", file)
	}
	p := filepath.Join(dir, file)
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) ||
		strings.ContainsAny(rel, `/\`) {
		return "", fmt.Errorf("export: refusing to write outside the vault: %q", file)
	}
	return p, nil
}

// writeVault renders the accumulated export into dir (which prepareVaultDir
// has already made empty and tool-owned): filename map first, then danglers
// dropped and counted, then one atomically-written note per entity.
func writeVault(dir string, entities []engramclient.ExportEntity, edges []engramclient.ExportEdge) (vaultStats, error) {
	refs := vaultFilenames(entities)

	var stats vaultStats
	byFrom := make(map[string][]engramclient.ExportEdge)
	for _, ed := range edges {
		_, fromOK := refs[ed.FromEntityID]
		_, toOK := refs[ed.ToEntityID]
		if !fromOK || !toOK {
			stats.Dropped++ // endpoint not exported (expired, ACL-hidden, or unknown)
			continue
		}
		byFrom[ed.FromEntityID] = append(byFrom[ed.FromEntityID], ed)
		stats.Edges++
	}

	sorted := append([]engramclient.ExportEntity(nil), entities...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, e := range sorted {
		ref, ok := refs[e.ID]
		if !ok {
			continue // empty-id record: unlinkable, skipped by vaultFilenames
		}
		content, err := renderNote(e, byFrom[e.ID], refs)
		if err != nil {
			return stats, err
		}
		path, err := confinedNotePath(dir, ref.File+".md")
		if err != nil {
			return stats, err
		}
		if err := writeFileAtomic(dir, path, content); err != nil {
			return stats, err
		}
		stats.Entities++
	}
	return stats, nil
}

// writeFileAtomic writes content to path via a temp file in dir + rename, so
// a crash mid-export never leaves a half-written note.
func writeFileAtomic(dir, path, content string) error {
	tmp, err := os.CreateTemp(dir, ".engram-tmp-*")
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("export: writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("export: writing %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("export: writing %s: %w", path, err)
	}
	return nil
}
