// export.go implements `engram export <dir>`: it drains the paginated Export
// RPC (episodic records plus the live graph, via engramclient's
// transport-free ExportPage view) and renders the caller's tenant-scoped
// memory into a rich Obsidian vault — event notes under events/, concept
// fact-sheets under concepts/, and topic maps under maps/ (assembled in
// vault.go from the Phase 2–4 model and renderers).
//
// Security model: episodic prose, entity names, aliases, and predicates are
// UNTRUSTED ingested content that ends up in filesystem paths, note bodies,
// and link syntax. Sanitization happens at the rendering barricades
// (sanitizeFilename / cleanInline / sanitizeBody / quoteBlock), and path
// confinement is re-verified immediately before every write
// (confinedVaultPath in vault.go) — defense in depth on the one path that
// could escape <dir>. The clobber path is guarded twice: a foreign non-empty
// dir is refused without --force, and even --force never cleans the
// filesystem root or the user's home directory. The dir is cleaned only
// AFTER the fetch succeeds, so a failed export never destroys an existing
// vault.

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
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ryanthedev/engram/internal/engramclient"
)

// vaultMarker is the ownership sentinel written into every generated vault.
// Its presence is what lets a re-run clobber-and-regenerate without --force
// while foreign directories stay refused by default.
const vaultMarker = ".engram-vault"

// maxFilenameRunes caps the slug portion of a note filename for legibility.
// It is a RUNE cap only — 60 runes can be up to 240 BYTES (emoji are 4 bytes
// each). The hard NAME_MAX guarantee is enforced separately, in bytes, by
// fitNoteName/maxNoteBaseBytes when the full basename (date prefix + slug +
// collision suffix + ".md") is assembled.
const maxFilenameRunes = 60

// maxNoteBaseBytes caps every note's full basename ("<name>.md", including
// any date prefix and collision suffix) in BYTES, safely under the 255-byte
// NAME_MAX of common filesystems. Overflowing it would be data loss, not
// just an error: the old vault is already cleaned when notes are written, so
// a rename failing ENAMETOOLONG would abort an export that can no longer be
// rolled back.
const maxNoteBaseBytes = 240

// maxSuffixIDBytes caps how many id bytes a residual-clash collision suffix
// may embed; past it, a growing counter (not more id bytes) provides
// uniqueness, so a pathological id can never push a name over the budget.
const maxSuffixIDBytes = 24

// runExport handles `engram export [--force] <dir>`: parse flags, refuse a
// foreign target early, dial, drain every Export page, then clobber and
// regenerate the vault. The dir is cleaned only AFTER the fetch succeeds, so
// a failed export never destroys an existing vault. Because the regenerate
// flow clobbers everything, a warning about manual-edit loss prints with
// every successful export.
func runExport(ctx context.Context, args []string, env Env, out io.Writer) error {
	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	force := flags.Bool("force", false, "clobber a non-empty directory not created by engram export")
	addr := flags.String("addr", "", "engramd address")
	token := flags.String("token", "", "bearer token")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() < 1 {
		return errors.New("export: expected a target <dir>")
	}
	dir := flags.Arg(0)
	// flag stops at the first positional; re-parse the tail so
	// `export <dir> --force` works as well as `export --force <dir>`.
	if err := flags.Parse(flags.Args()[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
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
	episodics, entities, edges, err := fetchExport(ctx, client)
	if err != nil {
		return err
	}
	if err := prepareVaultDir(dir, *force); err != nil {
		return err
	}
	stats, err := writeVault(dir, episodics, entities, edges)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "warning: re-running export regenerates this vault in place — any manual Obsidian edits in %s will be clobbered\n", dir)
	fmt.Fprintf(out, "exported %d events, %d concepts, %d maps to %s (%d ghosts, %d dropped)\n",
		stats.Events, stats.Concepts, stats.Maps, dir, stats.Ghosts, stats.Dropped)
	return nil
}

// fetchExport drains the paginated Export RPC, accumulating episodic,
// entity, and edge records until NextCursor is empty. It aborts if the
// server's cursor stops advancing (external input; would otherwise loop
// forever).
func fetchExport(ctx context.Context, client *engramclient.Client) ([]engramclient.ExportEpisodic, []engramclient.ExportEntity, []engramclient.ExportEdge, error) {
	var episodics []engramclient.ExportEpisodic
	var entities []engramclient.ExportEntity
	var edges []engramclient.ExportEdge
	cursor := ""
	for {
		page, err := client.ExportPage(ctx, cursor)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("export: fetching page: %w", err)
		}
		episodics = append(episodics, page.Episodics...)
		entities = append(entities, page.Entities...)
		edges = append(edges, page.Edges...)
		if page.NextCursor == "" {
			return episodics, entities, edges, nil
		}
		if page.NextCursor == cursor {
			return nil, nil, nil, errors.New("export: server cursor did not advance; aborting")
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
	// Resolve symlinks BEFORE the catastrophic-target check: a vault dir that
	// is a symlink to / or $HOME would otherwise smuggle the cleaner past the
	// guard (the comparison would see the link's path, not its target). A
	// not-yet-created dir has nothing to resolve — and an absent dir cannot
	// be a catastrophic target, since those always exist.
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = resolved
	} else if !errors.Is(rerr, fs.ErrNotExist) {
		return fmt.Errorf("export: resolving %s: %w", dir, rerr)
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		// Resolve $HOME too, so both sides of the comparison are real paths
		// (macOS tempdirs, for one, live behind a /var -> /private/var link).
		if h, herr := filepath.EvalSymlinks(home); herr == nil {
			home = h
		}
	}
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

// idPrefix returns a prefix of id of at most n BYTES, never splitting a
// UTF-8 rune (all of id when shorter). Ids are CLIENT-SUPPLIED — the server
// validates only non-emptiness — so a raw byte slice here once produced
// invalid-UTF-8 basenames that failed the atomic rename after the old vault
// was already cleaned.
func idPrefix(id string, n int) string {
	return truncateBytes(id, n)
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

// truncateBytes truncates s to at most n bytes without ever splitting a
// UTF-8 rune (the cut backs up to the nearest rune start).
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// fitNoteName composes base+suffix so the final basename
// ("base+suffix.md") fits maxNoteBaseBytes: the BASE is truncated in bytes
// on a rune boundary — never the uniqueness-carrying suffix — and re-trimmed
// so truncation cannot leave a trailing dot or space before the suffix or
// extension.
func fitNoteName(base, suffix string) string {
	budget := maxNoteBaseBytes - len(suffix) - len(".md")
	if budget < 0 {
		budget = 0
	}
	return strings.TrimRight(truncateBytes(base, budget), ". ") + suffix
}

// safeNoteName is the single final choke point EVERY assembled note basename
// passes through — events, concepts, maps, their collision-suffixed and misc
// variants alike — as the last step before the name is used as a path
// element. Whatever any upstream field contained (title, client-supplied id,
// suffix material), the result is simultaneously: valid UTF-8 (invalid
// sequences and partial runes stripped), free of control and
// filesystem/Obsidian-illegal characters (the sanitizeFilename policy,
// re-applied to the ASSEMBLED name so id-derived suffixes are covered), free
// of leading/trailing dots and spaces, and — with ".md" — inside
// maxNoteBaseBytes. Correctness no longer depends on each source field being
// independently clean. Sanitization never lengthens the name (illegal runes
// map to one ASCII '-'; everything else is dropped or kept), so a
// fitNoteName-budgeted input stays budgeted. Callers check uniqueness on
// this final form, so the guarantee holds for the name actually written.
func safeNoteName(name string) string {
	name = strings.ToValidUTF8(name, "")
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			// drop control chars
		case strings.ContainsRune(fsIllegal, r):
			b.WriteRune('-')
		default:
			b.WriteRune(r)
		}
	}
	s := strings.Trim(b.String(), ". ")
	s = strings.TrimRight(truncateBytes(s, maxNoteBaseBytes-len(".md")), ". ")
	if s == "" {
		// Unreachable from real callers (bases have non-empty sanitized
		// fallbacks; suffixes carry parens and digits) — but a path element
		// must never be empty.
		return "note"
	}
	return s
}

// uniqueNoteName assigns the deterministic, byte-budgeted, collision-managed
// name for base: bare when unique, id-prefix-suffixed when suffix is set
// (forced or homonym), extended on residual clashes — a longer id prefix up
// to maxSuffixIDBytes, then a growing counter — until unused. Every
// candidate passes through fitNoteName (byte budget) and then safeNoteName
// (the final validity choke point) BEFORE the uniqueness check, so no clash
// can push a basename past the byte budget and no id content can smuggle
// invalid UTF-8 or illegal characters into a filename. The chosen name is
// recorded in used (case-insensitively, for case-folding filesystems). This
// is the single suffixing algorithm shared by buildVaultRefs (events +
// concepts) and assignClusterFilenames (maps).
func uniqueNoteName(base, id string, suffix bool, used map[string]bool) string {
	name := safeNoteName(fitNoteName(base, ""))
	if suffix {
		name = safeNoteName(fitNoteName(base, " ("+idPrefix(id, 8)+")"))
	}
	for n := 8; used[strings.ToLower(name)]; n += 4 {
		if n+4 <= maxSuffixIDBytes && n < len(id) {
			name = safeNoteName(fitNoteName(base, " ("+idPrefix(id, n+4)+")"))
		} else {
			// The counter, not more id bytes, guarantees termination and
			// distinctness from here on (its ASCII digits survive
			// safeNoteName verbatim); the name stays inside the budget.
			name = safeNoteName(fitNoteName(base, " ("+idPrefix(id, maxSuffixIDBytes)+"-"+strconv.Itoa(n)+")"))
		}
	}
	used[strings.ToLower(name)] = true
	return name
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
