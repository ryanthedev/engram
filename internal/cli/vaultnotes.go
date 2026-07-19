// vaultnotes.go renders the two primary vault note types from the model
// built in Phase 2 (vaultmodel.go): event notes (full sanitized episodic
// prose, UTC-time-foldered) and concept notes (a provenance fact-sheet of
// claims, each optionally quoting its source event's sanitized prose,
// attributed by wikilink, inside a folded `> [!quote]-` callout).
//
// Both renderers are pure and total over their inputs: no I/O, no wall
// clock, no map iteration order. Every path/collision/foldering decision
// (empty slug → id fallback, UTC date bucket, undated bucket, homonym
// suffixing) was already resolved once in Phase 2's buildVaultRefs — these
// renderers only look their own id up in VaultRefs, they never recompute
// slugs. Untrusted text has exactly one path into the output: prose bodies,
// claim statements, and quoted source-event prose through sanitizeBody
// (source-event prose additionally through quoteBlock, since it is being
// re-embedded inside a callout), display names through the VaultRefs-
// resolved (already cleanInline'd) Display field, and frontmatter scalars
// through a real YAML encoder — never a hand-interpolated string.
//
// renderConcept takes an events map (EventID -> Event) alongside VaultRefs:
// Claim itself carries no cached body text, so the source-event prose a
// claim's callout quotes can only come from a fresh lookup against the
// model's Events. Phase 5's assembler supplies this map from
// VaultModel.Events.

package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v2"
)

// renderEvent renders one event note: YAML frontmatter, an H1, the full
// sanitized prose body, and a trailing "**Concepts:**" footer linking every
// concept the event evidences. relPath is self.Folder + "/" + self.File +
// ".md", where self = refs[ev.EventID] (assigned by Phase 2's
// buildVaultRefs — already UTC-foldered, undated-bucketed, and
// collision-suffixed).
func renderEvent(ev Event, refs VaultRefs) (relPath, content string) {
	self := refs[ev.EventID]
	relPath = self.Folder + "/" + self.File + ".md"

	var b strings.Builder
	writeEventFrontmatter(&b, ev)
	b.WriteString("\n# ")
	b.WriteString(self.Display)
	b.WriteString("\n\n")
	b.WriteString(sanitizeBody(ev.Body))
	b.WriteString("\n")
	writeConceptsFooter(&b, ev.ConceptIDs, refs)

	return relPath, b.String()
}

// renderConcept renders one concept's provenance fact-sheet: YAML
// frontmatter, an H1, a "What we've learned" claim list (oldest-first, tie
// broken by EdgeID; each claim's sanitized Statement plus, when its source
// event resolves in both events and refs, a folded callout quoting that
// event's sanitized prose and attributing it via a wikilink), and a
// "Related concepts" section (ghost neighbors included — they resolve to a
// valid link target even though no file is ever written for them). relPath
// mirrors renderEvent: self.Folder + "/" + self.File + ".md" from
// refs[c.EntityID]. events is keyed by EventID (Phase 5 supplies it from
// VaultModel.Events) — it is the only source of source-event prose
// available to this pure renderer, since Claim itself carries no cached
// body text.
func renderConcept(c Concept, refs VaultRefs, events map[string]Event) (relPath, content string) {
	self := refs[c.EntityID]
	relPath = self.Folder + "/" + self.File + ".md"

	var b strings.Builder
	writeConceptFrontmatter(&b, c)
	b.WriteString("\n# ")
	b.WriteString(self.Display)
	b.WriteString("\n")
	writeClaims(&b, c.Claims, refs, events)
	writeRelatedConcepts(&b, c.RelatedIDs, refs)

	return relPath, b.String()
}

// writeEventFrontmatter emits the event's YAML frontmatter block. Every
// scalar goes through the real YAML encoder, never a hand-built string.
func writeEventFrontmatter(b *strings.Builder, ev Event) {
	fm := yaml.MapSlice{{Key: "engram_id", Value: ev.EventID}}
	if ev.OccurredAt != nil {
		fm = append(fm, yaml.MapItem{Key: "occurred_at", Value: ev.OccurredAt.UTC().Format(time.RFC3339)})
	}
	writeFrontmatter(b, fm)
}

// writeConceptFrontmatter emits the concept's YAML frontmatter block.
// Aliases are untrusted; they go through the YAML encoder exactly like
// export.go's entity-note precedent, never hand-escaped.
func writeConceptFrontmatter(b *strings.Builder, c Concept) {
	fm := yaml.MapSlice{
		{Key: "engram_id", Value: c.EntityID},
		{Key: "degree", Value: c.Degree},
	}
	if len(c.Aliases) > 0 {
		fm = append(fm, yaml.MapItem{Key: "aliases", Value: c.Aliases})
	}
	writeFrontmatter(b, fm)
}

// writeFrontmatter marshals fm as a "---" delimited YAML block. Marshal
// cannot fail for the plain string/int/[]string scalars these two callers
// build (no cycles, no unsupported types), so the error is not surfaced
// through the renderers' error-free signatures.
func writeFrontmatter(b *strings.Builder, fm yaml.MapSlice) {
	fmBytes, _ := yaml.Marshal(fm)
	b.WriteString("---\n")
	b.Write(fmBytes)
	b.WriteString("---\n")
}

// writeConceptsFooter appends the event's "**Concepts:**" footer. Omitted
// entirely when the event evidences no concepts.
func writeConceptsFooter(b *strings.Builder, conceptIDs []string, refs VaultRefs) {
	links := resolveLinks(conceptIDs, refs)
	if len(links) == 0 {
		return
	}
	b.WriteString("\n**Concepts:** ")
	b.WriteString(strings.Join(links, ", "))
	b.WriteString("\n")
}

// writeClaims appends the "What we've learned" section: one entry per
// claim, oldest-first (tie broken by EdgeID), each a sanitized Statement
// optionally followed by a folded quote-callout quoting its source event's
// prose. Omitted entirely when there are no claims (e.g. a hub with zero
// claims but degree ≥2 still gets the section skipped here and "Related
// concepts" carries the note).
func writeClaims(b *strings.Builder, claims []Claim, refs VaultRefs, events map[string]Event) {
	if len(claims) == 0 {
		return
	}
	b.WriteString("\n## What we've learned\n")
	for _, cl := range sortedClaims(claims) {
		b.WriteString("\n- ")
		b.WriteString(sanitizeBody(cl.Statement))
		b.WriteString("\n")
		if quote := sourceQuote(cl, refs, events); quote != "" {
			b.WriteString("\n")
			b.WriteString(quote)
			b.WriteString("\n")
		}
	}
}

// sortedClaims returns a sorted copy of claims (oldest ValidAt first, EdgeID
// tie-break); the input is never mutated. Re-sorting here — rather than
// trusting the caller's order — keeps renderConcept correct in isolation,
// independent of vaultmodel.go's own (matching) sort.
func sortedClaims(claims []Claim) []Claim {
	sorted := append([]Claim(nil), claims...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if c := compareTimePtr(sorted[i].ValidAt, sorted[j].ValidAt); c != 0 {
			return c < 0
		}
		return sorted[i].EdgeID < sorted[j].EdgeID
	})
	return sorted
}

// sourceQuote renders a claim's folded provenance callout: the source
// event's own sanitized prose (the "receipts" — what makes this a fact
// sheet and not just a list of assertions), quoted under a callout titled
// with a wikilink back to that event's note. Both the callout title's
// display text and the quoted body are untrusted and pass through their
// respective barricades: the display text was already cleanInline'd when
// refs was built, and the body goes through sanitizeBody THEN quoteBlock
// (quoteBlock does not assume its input was already sanitized, so applying
// both in sequence is required, not redundant). Returns "" — the
// documented "Statement alone" edge case — when the claim has no source
// event, or that event is absent from events, or its ref is missing.
func sourceQuote(cl Claim, refs VaultRefs, events map[string]Event) string {
	if cl.SourceEventID == "" {
		return ""
	}
	ev, ok := events[cl.SourceEventID]
	if !ok {
		return ""
	}
	ref, ok := refs[cl.SourceEventID]
	if !ok {
		return ""
	}
	title := fmt.Sprintf("> [!quote]- Source: [[%s|%s]]\n", ref.File, ref.Display)
	return title + quoteBlock(sanitizeBody(ev.Body))
}

// writeRelatedConcepts appends the "Related concepts" section, one link per
// neighbor (ghosts included — they still resolve to a valid link target,
// just one no file was ever written for).
func writeRelatedConcepts(b *strings.Builder, relatedIDs []string, refs VaultRefs) {
	b.WriteString("\n## Related concepts\n")
	links := resolveLinks(relatedIDs, refs)
	if len(links) == 0 {
		b.WriteString("\n*None.*\n")
		return
	}
	for _, link := range links {
		b.WriteString("\n- ")
		b.WriteString(link)
	}
	b.WriteString("\n")
}

// resolveLinks resolves each id to a "[[File|Display]]" wikilink via refs,
// sorted by ref.File for deterministic, human-legible ordering (ids
// themselves are opaque). An id absent from refs is skipped defensively —
// it should never happen given the Phase 2 contract, but a malformed
// "[[|]]" is worse than a silently omitted link.
func resolveLinks(ids []string, refs VaultRefs) []string {
	type link struct{ file, label string }
	links := make([]link, 0, len(ids))
	for _, id := range ids {
		ref, ok := refs[id]
		if !ok {
			continue
		}
		links = append(links, link{file: ref.File, label: fmt.Sprintf("[[%s|%s]]", ref.File, ref.Display)})
	}
	sort.Slice(links, func(i, j int) bool { return links[i].file < links[j].file })
	out := make([]string, len(links))
	for i, l := range links {
		out[i] = l.label
	}
	return out
}
