// vaultknowledge.go renders the additive knowledge/ folder atop an
// already-assembled memory vault (Phase 2 of the memory-knowledge mapping
// prototype): enumerate every collection the caller may read, drain each via
// one bounded KnowledgeSearch call, decode the hits into knowledgeDocs, then
// resolve each doc's memory_ref against the memory vault's hub concepts (a
// KNOWLEDGE-tier keyword field carrying a MEMORY-tier entity id — the one
// soft foreign key this prototype proves out end-to-end) into a
// [[wikilink]], write one knowledge/<name>.md per doc, and append a
// reciprocal "Referenced by" backlink section to every mapped concept note
// already on disk.
//
// Fetch (network, fetchKnowledgeDocs) and render (disk, renderKnowledgeVault)
// are deliberately two separate phases: export.go calls fetch to completion
// BEFORE render touches anything, so a fetch failure can be reported as a
// caught warning without ever risking the memory vault writeVaultModel just
// finished — the same clean-late discipline export.go already applies to the
// memory fetch itself.
//
// Security model: doc Title/Text/MemoryRef/MemoryRefName are UNTRUSTED
// harvested content (same trust level as episodic prose and entity names)
// and pass through the identical barricades the memory vault uses:
// sanitizeFilename + uniqueNoteName/safeNoteName for filenames, sanitizeBody
// for the note body, cleanInline for inline link labels and the unresolved
// marker, and confinedVaultPath (via writeVaultNote) immediately before
// every write. No new sanitization primitive is introduced.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ryanthedev/engram/internal/engramclient"
	"github.com/ryanthedev/engram/internal/mcp"
	"github.com/ryanthedev/engram/internal/retrieval"
	yaml "go.yaml.in/yaml/v2"
)

// knowledgeStats summarizes one additive knowledge pass for the export
// summary line.
type knowledgeStats struct {
	Docs      int // knowledge/*.md notes written
	Backlinks int // concept notes that gained a "Referenced by" section
}

// knowledgeDoc is one fetched knowledge document, decoded from a
// KnowledgeSearch hit's fields_json. RAW untrusted content — sanitization is
// the renderer's job (renderKnowledgeVault), exactly like vaultmodel.go's
// Event/Concept carry raw Body/Name.
type knowledgeDoc struct {
	ID            string
	Collection    string
	Title         string
	Text          string
	MemoryRef     string // "" when the field is absent or empty
	MemoryRefName string // "" when the field is absent or empty
}

// fetchKnowledgeDocs enumerates every collection the caller may read
// (KnowledgeCollections already filters to readable collections server-side
// — no client-side access check needed) and drains each via a single
// empty-query KnowledgeSearch call at retrieval.MaxK: the RPC has no
// offset/cursor, so a second call could never see more than the first.
// Returned docs are sorted by id for deterministic downstream rendering.
// warnings holds one line per collection whose hit count equals the k cap —
// a possible-truncation signal, non-fatal, surfaced in the export summary.
// Any RPC error aborts the whole fetch; the caller treats that as soft
// (network/availability), never touching the already-written memory vault.
func fetchKnowledgeDocs(ctx context.Context, client *engramclient.Client) ([]knowledgeDoc, []string, error) {
	collections, err := client.KnowledgeCollections(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("listing knowledge collections: %w", err)
	}
	var docs []knowledgeDoc
	var warnings []string
	for _, col := range collections {
		// full_body=true: the export renders whole documents into the vault,
		// so fragment extraction (the search-time default) must be bypassed.
		hits, err := client.KnowledgeSearch(ctx, col.Name, "", nil, nil, retrieval.MaxK, true)
		if err != nil {
			return nil, nil, fmt.Errorf("searching knowledge collection %q: %w", col.Name, err)
		}
		if len(hits) == retrieval.MaxK {
			warnings = append(warnings, fmt.Sprintf(
				"knowledge collection %q returned %d docs (the k=%d fetch cap) and may hold more than was exported — the RPC has no paging",
				col.Name, len(hits), retrieval.MaxK))
		}
		for _, h := range hits {
			docs = append(docs, decodeKnowledgeHit(col.Name, col.TextField, h))
		}
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].ID < docs[j].ID })
	return docs, warnings, nil
}

// decodeKnowledgeHit parses one hit's fields_json into a knowledgeDoc.
// fields_json is server-produced, but the document VALUES inside it are
// untrusted harvester content passed straight through — a malformed payload
// degrades to an empty doc (every field its zero value) rather than aborting
// the whole fetch over one bad row. textField names the collection's
// full-text key (falls back to "text" defensively if a spec ever omits it —
// the registry requires it, but this is external, server-relayed data).
func decodeKnowledgeHit(collection, textField string, h mcp.KnowledgeHit) knowledgeDoc {
	var fields map[string]any
	_ = json.Unmarshal([]byte(h.Fields), &fields)
	text := textField
	if text == "" {
		text = "text"
	}
	return knowledgeDoc{
		ID:            h.ID,
		Collection:    collection,
		Title:         stringField(fields, "title"),
		Text:          stringField(fields, text),
		MemoryRef:     stringField(fields, "memory_ref"),
		MemoryRefName: stringField(fields, "memory_ref_name"),
	}
}

// stringField reads key from fields as a string, defaulting to "" when
// absent or of an unexpected JSON type (fields is untrusted, decoded from
// external fields_json — a caller-controlled number or object where a
// string was expected must degrade, not panic).
func stringField(fields map[string]any, key string) string {
	s, _ := fields[key].(string)
	return s
}

// resolvedKnowledgeDoc is one knowledgeDoc after filename assignment and
// memory_ref resolution — everything renderKnowledgeVault needs to write the
// note and, if mapped, contribute to its target concept's backlink section.
type resolvedKnowledgeDoc struct {
	Doc       knowledgeDoc
	File      string // assigned knowledge/<File>.md basename (no folder, no ".md")
	Display   string // inline-safe title for the H1 and wikilink labels (cleanInline'd, id fallback)
	ConceptID string // canonical hub concept id MemoryRef resolved to; "" if unmapped
}

// resolveKnowledgeDocs assigns every doc its deterministic filename (own
// flat "knowledge/" namespace, own collision-tracking map — see the Phase 2
// discovery doc's Design Decision 4 for why this does not reuse
// buildVaultRefs' all-homonyms-suffixed policy) and resolves MemoryRef
// against model's HUB concepts only. docs must already be sorted by id
// (fetchKnowledgeDocs' contract) so filename assignment is deterministic.
func resolveKnowledgeDocs(docs []knowledgeDoc, model VaultModel) []resolvedKnowledgeDoc {
	hubs := hubConceptIDs(model)
	used := make(map[string]bool, len(docs))
	out := make([]resolvedKnowledgeDoc, len(docs))
	for i, d := range docs {
		base, forced := knowledgeDocBase(d)
		file := uniqueNoteName(base, d.ID, forced, used)
		display := cleanInline(d.Title)
		if display == "" {
			display = file
		}
		conceptID := ""
		if d.MemoryRef != "" && hubs[d.MemoryRef] {
			conceptID = d.MemoryRef
		}
		out[i] = resolvedKnowledgeDoc{Doc: d, File: file, Display: display, ConceptID: conceptID}
	}
	return out
}

// knowledgeDocBase returns d's filename slug (sanitizeFilename — the same
// choke point buildVaultRefs uses for event/concept slugs) and whether it is
// a forced generic fallback (empty title) that should always carry a
// disambiguating suffix, mirroring buildVaultRefs' own forced-empty-name
// handling.
func knowledgeDocBase(d knowledgeDoc) (base string, forced bool) {
	base = sanitizeFilename(d.Title)
	if base == "" {
		return "doc", true
	}
	return base, false
}

// hubConceptIDs returns the set of concept ids that earned their own note
// file (Ghost == false). Only these are valid memory_ref resolution
// targets: a ghost is still a member of VaultRefs (it is a valid link target
// elsewhere in the vault) but gets no file, and the plan's edge cases treat
// "points at a ghost" identically to "points at nothing in the exported
// graph" — both render as the same inert unresolved marker.
func hubConceptIDs(model VaultModel) map[string]bool {
	hubs := make(map[string]bool, len(model.Concepts))
	for _, c := range model.Concepts {
		if !c.Ghost {
			hubs[c.EntityID] = true
		}
	}
	return hubs
}

// renderKnowledgeVault is the disk-writing half of the additive knowledge
// pass: fetchKnowledgeDocs must already have succeeded before this runs, so
// any error it returns is a local write failure (disk, or a confinement
// barricade refusal), never a fetch error — the caller lets it propagate as
// a hard error like any other write failure in this exporter, rather than
// folding it into fetchKnowledgeDocs' soft-fail path. An empty docs list is
// a no-op: no folder, no error, dir untouched.
func renderKnowledgeVault(dir string, docs []knowledgeDoc, model VaultModel, refs VaultRefs) (knowledgeStats, error) {
	var stats knowledgeStats
	if len(docs) == 0 {
		return stats, nil
	}
	resolved := resolveKnowledgeDocs(docs, model)

	for _, r := range resolved {
		relPath := "knowledge/" + r.File + ".md"
		if err := writeVaultNote(dir, relPath, renderKnowledgeNote(r, refs)); err != nil {
			return stats, err
		}
		stats.Docs++
	}

	byConcept := make(map[string][]resolvedKnowledgeDoc)
	for _, r := range resolved {
		if r.ConceptID != "" {
			byConcept[r.ConceptID] = append(byConcept[r.ConceptID], r)
		}
	}
	// resolved is already in doc-id order (resolveKnowledgeDocs preserves
	// input order, and fetchKnowledgeDocs sorted by id), so each concept's
	// slice above is already doc-id ordered; only the OUTER iteration over
	// concepts needs an explicit sort for determinism.
	conceptIDs := make([]string, 0, len(byConcept))
	for id := range byConcept {
		conceptIDs = append(conceptIDs, id)
	}
	sort.Strings(conceptIDs)
	for _, id := range conceptIDs {
		if err := appendConceptBacklinks(dir, refs[id], byConcept[id]); err != nil {
			return stats, err
		}
		stats.Backlinks++
	}
	return stats, nil
}

// renderKnowledgeNote renders one knowledge document note: YAML frontmatter
// (engram_id = doc id, collection), an H1, the memory_ref resolution line
// (resolved wikilink, inert unresolved marker, or omitted entirely when
// MemoryRef is absent/empty), and the sanitized body.
func renderKnowledgeNote(r resolvedKnowledgeDoc, refs VaultRefs) string {
	var b strings.Builder
	fm := yaml.MapSlice{
		{Key: "engram_id", Value: r.Doc.ID},
		{Key: "collection", Value: r.Doc.Collection},
	}
	writeFrontmatter(&b, fm)
	b.WriteString("\n# ")
	b.WriteString(r.Display)
	b.WriteString("\n")
	if line := memoryRefLine(r, refs); line != "" {
		b.WriteString("\n")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(sanitizeBody(r.Doc.Text))
	b.WriteString("\n")
	return b.String()
}

// memoryRefLine renders r's "**Memory:** ..." line: a resolving wikilink
// when ConceptID is set, "" (no line at all) when MemoryRef is absent/empty,
// or an inert "unresolved: <label>" marker otherwise — labeled with
// MemoryRefName when present, the raw MemoryRef id otherwise, both
// cleanInline'd (untrusted content: neutralizes control chars and forged
// "[[" / "]]" / "|" wikilink syntax).
func memoryRefLine(r resolvedKnowledgeDoc, refs VaultRefs) string {
	if r.ConceptID != "" {
		ref := refs[r.ConceptID]
		return "**Memory:** [[" + ref.File + "|" + ref.Display + "]]"
	}
	if r.Doc.MemoryRef == "" {
		return ""
	}
	label := r.Doc.MemoryRefName
	if label == "" {
		label = r.Doc.MemoryRef
	}
	return "**Memory:** unresolved: " + cleanInline(label)
}

// appendConceptBacklinks appends a "Referenced by" section — one link per
// mapped doc, in mapped's order (doc-id order, the caller's contract) — to
// the concept note at ref, which writeVaultModel must already have written
// (ref is drawn from hubConceptIDs, the exact same non-ghost filter
// writeVaultModel used to decide which concepts get a file). The read is a
// defense-in-depth check, not an optimization: if the file is somehow
// missing, this fails loudly rather than fabricating a partial note.
func appendConceptBacklinks(dir string, ref noteRef, mapped []resolvedKnowledgeDoc) error {
	relPath := ref.Folder + "/" + ref.File + ".md"
	path, err := confinedVaultPath(dir, relPath)
	if err != nil {
		return err
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("export: reading concept note %q for backlink: %w", relPath, err)
	}
	var b strings.Builder
	b.Write(existing)
	b.WriteString("\n## Referenced by\n")
	for _, r := range mapped {
		b.WriteString("\n- [[")
		b.WriteString(r.File)
		b.WriteString("|")
		b.WriteString(r.Display)
		b.WriteString("]]")
	}
	b.WriteString("\n")
	return writeVaultNote(dir, relPath, b.String())
}
