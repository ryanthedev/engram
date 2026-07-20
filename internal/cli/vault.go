// vault.go assembles the rich Obsidian vault from a drained export: it builds
// the deterministic model (vaultmodel.go), renders event notes, concept
// fact-sheets, and topic maps (vaultnotes.go, vaultmaps.go), and writes them
// under the vault's security invariants — every renderer-produced relative
// path is re-verified against a nested path barricade immediately before the
// write, and every file lands atomically (temp + rename). The whole pipeline
// is deterministic: one export input always produces a byte-identical vault.
//
// Security model: note paths derive from UNTRUSTED ingested content (event
// prose becomes event slugs, entity names become concept and map filenames).
// sanitizeFilename plus the Phase 2–4 ref assignment should make an escaping
// path unreachable; confinedVaultPath is the defense-in-depth barricade that
// refuses the write — aborting the whole export — if that assumption is ever
// wrong. A refusal here is a bug-stop, never an escape.

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ryanthedev/engram/internal/engramclient"
)

// vaultStats summarizes one rich export for the final printed line.
type vaultStats struct {
	Events   int // event notes written
	Concepts int // concept (hub) notes written
	Maps     int // topic-map (MOC) notes written
	Ghosts   int // ghost concepts: link targets that get no file
	Dropped  int // edges with no exported endpoint (contribute no claim)
}

// allowedVaultRoots are the only top-level folders the assembler may write
// into. Any other root in a renderer-produced path is a bug-stop refusal.
// "knowledge" is the Phase 2 knowledge-mapping additive post-pass folder —
// flat, same depth class as "concepts"/"maps".
var allowedVaultRoots = map[string]bool{"events": true, "concepts": true, "maps": true, "knowledge": true}

// vaultPathDepth is the exact number of path elements a note under root must
// have: events are date-bucketed ("events/2026/x.md", "events/undated/x.md" =
// 3), concepts and maps are flat ("concepts/x.md", "maps/x.md" = 2).
func vaultPathDepth(root string) int {
	if root == "events" {
		return 3
	}
	return 2
}

// confinedVaultPath joins the renderer-produced, vault-relative relPath
// (forward slashes, e.g. "events/2026/2026-07-08 foo.md") onto dir and
// verifies it cannot escape: known root folder, the exact expected depth for
// that root, every path element individually flat-safe, and a final
// filepath.Rel re-check on the joined result. The renderers plus
// sanitizeFilename should make a refusal unreachable; if one ever fires, the
// whole export aborts at this barricade rather than writing anything.
func confinedVaultPath(dir, relPath string) (string, error) {
	refuse := func() (string, error) {
		return "", fmt.Errorf("export: refusing to write outside the vault: %q", relPath)
	}
	if relPath == "" || filepath.IsAbs(relPath) || strings.Contains(relPath, `\`) {
		return refuse()
	}
	parts := strings.Split(relPath, "/")
	if !allowedVaultRoots[parts[0]] || len(parts) != vaultPathDepth(parts[0]) {
		return refuse()
	}
	for _, el := range parts {
		// Refuse empty, ".", "..", and anything that is only dots/spaces
		// (sanitizeFilename never emits these; seeing one means a caller
		// bypassed it).
		if strings.Trim(el, ". ") == "" {
			return refuse()
		}
	}
	p := filepath.Join(dir, filepath.FromSlash(relPath))
	rel, err := filepath.Rel(dir, p)
	if err != nil || filepath.IsAbs(rel) || rel != filepath.FromSlash(relPath) {
		return refuse()
	}
	return p, nil
}

// writeVault assembles and writes the rich vault into dir (which
// prepareVaultDir has already made empty and tool-owned): model first, then
// one note per event, one fact-sheet per hub concept (ghosts get no file,
// only unresolved links), and one MOC note per topic map — each path
// re-confined immediately before its atomic write. An empty export writes
// nothing (marker-only vault) and is not an error.
//
// It discards the assembled VaultModel/VaultRefs; writeVaultModel is the
// identical assembly with those exposed, for the one caller (export.go's
// Phase 2 knowledge post-pass) that needs to locate concept files afterward
// without recomputing buildVaultModel a second time. Kept as a thin wrapper
// — rather than changing this signature — so every existing caller (and
// vault_test.go's dozen call sites) is untouched.
func writeVault(dir string, episodics []engramclient.ExportEpisodic, entities []engramclient.ExportEntity, edges []engramclient.ExportEdge) (vaultStats, error) {
	stats, _, _, err := writeVaultModel(dir, episodics, entities, edges)
	return stats, err
}

// writeVaultModel is writeVault's implementation, additionally returning the
// assembled VaultModel and VaultRefs.
func writeVaultModel(dir string, episodics []engramclient.ExportEpisodic, entities []engramclient.ExportEntity, edges []engramclient.ExportEdge) (vaultStats, VaultModel, VaultRefs, error) {
	model, refs := buildVaultModel(episodics, entities, edges)
	stats := vaultStats{Dropped: countDroppedEdges(entities, edges)}

	// renderConcept quotes source-event prose via a fresh lookup; the map is
	// keyed by EventID per the Phase 3 contract.
	eventsByID := make(map[string]Event, len(model.Events))
	for _, ev := range model.Events {
		eventsByID[ev.EventID] = ev
	}

	for _, ev := range model.Events {
		relPath, content := renderEvent(ev, refs)
		if err := writeVaultNote(dir, relPath, content); err != nil {
			return stats, model, refs, err
		}
		stats.Events++
	}
	for _, c := range model.Concepts {
		if c.Ghost {
			stats.Ghosts++
			continue
		}
		relPath, content := renderConcept(c, refs, eventsByID)
		if err := writeVaultNote(dir, relPath, content); err != nil {
			return stats, model, refs, err
		}
		stats.Concepts++
	}
	for _, cl := range clusterConcepts(model) {
		relPath, content := renderMap(cl, refs)
		if err := writeVaultNote(dir, relPath, content); err != nil {
			return stats, model, refs, err
		}
		stats.Maps++
	}
	return stats, model, refs, nil
}

// countDroppedEdges counts the edges NEITHER of whose endpoints was exported
// — exactly the edges that contribute no claim to any concept in the model's
// join (an edge with at least one exported endpoint always lands as a claim,
// so it is not dropped). Computed here from the raw records so the summary
// stays honest without reaching into the model's internals.
func countDroppedEdges(entities []engramclient.ExportEntity, edges []engramclient.ExportEdge) int {
	exported := make(map[string]bool, len(entities))
	for _, e := range entities {
		if e.ID != "" {
			exported[e.ID] = true
		}
	}
	dropped := 0
	for _, ed := range edges {
		if !exported[ed.FromEntityID] && !exported[ed.ToEntityID] {
			dropped++
		}
	}
	return dropped
}

// writeVaultNote confines relPath (the security barricade — before any
// filesystem effect), creates its parent folder, and writes the note
// atomically.
func writeVaultNote(dir, relPath, content string) error {
	path, err := confinedVaultPath(dir, relPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("export: creating vault folder for %q: %w", relPath, err)
	}
	return writeFileAtomic(dir, path, content)
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
