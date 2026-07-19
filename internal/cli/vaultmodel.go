// vaultmodel.go builds the deterministic, tier-agnostic in-memory model of
// the rich vault from a drained export: one Event per episodic record, one
// Concept per collapsed entity group, Claims joined to their source events
// via source_ids → event_id, and the shared VaultRefs link-resolution map
// every renderer and the writer consume.
//
// Everything here is pure and deterministic: maps are only ever drained via
// sorted keys, output slices are sorted, no wall clock is read, and all time
// handling goes through .UTC(). Records that cannot be joined or linked
// (empty ids) are skipped rather than crashed on — export data is external
// input. The model carries RAW untrusted text (Body, Name, Statement);
// sanitization is the renderers' barricade (sanitizeBody / quoteBlock /
// sanitizeFilename / cleanInline), applied at the point the text enters
// markdown or a filesystem path.

package cli

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ryanthedev/engram/internal/engramclient"
)

// hubMinDegree is the degree at which a concept earns its own note file.
// Below it the concept is a ghost: it appears only as an unresolved
// [[link]]. Kept as a single constant so the cutoff can drop to 1 if the
// hub set proves too thin on real data (plan assumption fallback).
const hubMinDegree = 2

// maxEventTitleRunes caps an event title derived from untrusted prose.
const maxEventTitleRunes = 80

// noteRef is one note's rendered identity: its filename (without .md), the
// inline-safe display name for H1s and link labels, and the vault-relative
// folder the note lives in ("concepts", "events/2026", "events/undated").
type noteRef struct {
	File    string
	Display string
	Folder  string
}

// VaultRefs maps every event id and concept id (ghosts included — they are
// still link targets) to its deterministic, collision-suffixed noteRef.
// It is the single shared link-resolution table for Phases 3, 4, and 5.
type VaultRefs map[string]noteRef

// Event is one episodic record's note-source view: raw prose plus the
// concepts it evidences.
type Event struct {
	EventID    string
	Title      string // first line of Text, inline-cleaned; id-derived fallback
	Body       string // RAW untrusted prose; renderers apply sanitizeBody
	OccurredAt *time.Time
	ConceptIDs []string // sorted canonical concept ids sourced from this event
}

// Claim is one edge's provenance-carrying statement on a concept fact sheet.
type Claim struct {
	Statement     string
	ValidAt       *time.Time
	EdgeID        string
	SourceEventID string // "" or an event id; an id absent from Events renders quote-less
}

// Concept is one collapsed entity group: a hub (Ghost=false) gets its own
// note, a ghost appears only as an unresolved link.
type Concept struct {
	EntityID   string // canonical (smallest) member entity id
	Name       string
	Aliases    []string
	Degree     int // distinct canonical neighbor concepts
	Claims     []Claim
	RelatedIDs []string // sorted canonical neighbor concept ids
	Ghost      bool
}

// VaultModel is the assembled, render-ready vault content.
type VaultModel struct {
	Events   []Event
	Concepts []Concept
}

// buildVaultModel assembles the vault model and its shared ref table from
// page-drained export records. It is total over its input: duplicate events
// dedupe deterministically, unknown edge endpoints and absent source events
// degrade gracefully (quote-less claims), and empty-id records are skipped.
func buildVaultModel(episodics []engramclient.ExportEpisodic, entities []engramclient.ExportEntity, edges []engramclient.ExportEdge) (VaultModel, VaultRefs) {
	deduped := dedupeEvents(episodics)
	groups, canonicalByEntity := collapseEntities(entities)
	concepts := assembleConcepts(groups, canonicalByEntity, deduped, edges)
	events := assembleEvents(deduped, groups)
	refs := buildVaultRefs(events, concepts)
	return VaultModel{Events: events, Concepts: concepts}, refs
}

// dedupeEvents keeps exactly one record per event_id. The winner is chosen
// deterministically and order-independently: earliest OccurredAt (nil last),
// then lexicographically smallest Kind, then Text. (The episodic wire
// carries no CreatedAt or per-doc id, so the plan's "earliest CreatedAt"
// intent maps to the earliest-record fields that ARE on the wire.)
// Records with an empty event_id are skipped: they cannot be joined,
// deduplicated, or linked. The result is sorted by EventID.
func dedupeEvents(episodics []engramclient.ExportEpisodic) []engramclient.ExportEpisodic {
	byID := make(map[string]engramclient.ExportEpisodic)
	for _, ep := range episodics {
		if ep.EventID == "" {
			continue
		}
		cur, ok := byID[ep.EventID]
		if !ok || episodicBefore(ep, cur) {
			byID[ep.EventID] = ep
		}
	}
	out := make([]engramclient.ExportEpisodic, 0, len(byID))
	for _, ep := range byID {
		out = append(out, ep)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EventID < out[j].EventID })
	return out
}

// episodicBefore reports whether a wins the dedupe over b: earlier
// OccurredAt (nil sorts last), then Kind, then Text.
func episodicBefore(a, b engramclient.ExportEpisodic) bool {
	if c := compareTimePtr(a.OccurredAt, b.OccurredAt); c != 0 {
		return c < 0
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	return a.Text < b.Text
}

// compareTimePtr orders optional times: nil sorts after any set time.
func compareTimePtr(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	case a.Before(*b):
		return -1
	case b.Before(*a):
		return 1
	default:
		return 0
	}
}

// conceptGroup is one collapsed set of entities sharing a normalized name.
type conceptGroup struct {
	canonicalID string
	name        string
	aliases     []string
	sourceIDs   []string // union of member SourceIDs, sorted
}

// collapseEntities merges entities whose normalized names are EQUAL (no
// fuzzy matching — nondeterministic) into one concept group. The canonical
// id is the smallest member entity id; aliases are the union of member
// aliases plus non-canonical member surface names. Entities whose name
// normalizes to empty never collapse with each other (merging every unnamed
// entity would fuse unrelated concepts); each keys on its own id. Empty-id
// entities are skipped (unlinkable). Returns the groups sorted by canonical
// id plus the member-entity-id → canonical-id map used to resolve edges.
func collapseEntities(entities []engramclient.ExportEntity) ([]conceptGroup, map[string]string) {
	byKey := make(map[string][]engramclient.ExportEntity)
	for _, e := range entities {
		if e.ID == "" {
			continue
		}
		key := normalizeConceptName(e.Name)
		if key == "" {
			key = "\x00id:" + e.ID // private, uncollapsible key space
		}
		byKey[key] = append(byKey[key], e)
	}

	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	groups := make([]conceptGroup, 0, len(keys))
	canonicalByEntity := make(map[string]string)
	for _, k := range keys {
		members := byKey[k]
		sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })
		canonical := members[0]
		g := conceptGroup{canonicalID: canonical.ID, name: canonical.Name}
		aliasSet := make(map[string]bool)
		srcSet := make(map[string]bool)
		for _, m := range members {
			canonicalByEntity[m.ID] = canonical.ID
			for _, a := range m.Aliases {
				if a != "" && a != canonical.Name {
					aliasSet[a] = true
				}
			}
			if m.Name != "" && m.Name != canonical.Name {
				aliasSet[m.Name] = true // variant surface form survives as an alias
			}
			for _, s := range m.SourceIDs {
				if s != "" {
					srcSet[s] = true
				}
			}
		}
		g.aliases = sortedKeys(aliasSet)
		g.sourceIDs = sortedKeys(srcSet)
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].canonicalID < groups[j].canonicalID })
	return groups, canonicalByEntity
}

// surroundingPunct is the punctuation/quote set stripped from the ends of a
// normalized concept name.
const surroundingPunct = "\"'`“”‘’‚„.,;:!?¡¿()[]{}<>«»-–—_*~| \t"

// normalizeConceptName is the collapse key: lowercase, internal whitespace
// collapsed, surrounding punctuation/quotes stripped. Exact equality of
// this key is the ONLY merge trigger.
func normalizeConceptName(name string) string {
	s := strings.ToLower(name)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, surroundingPunct)
	return strings.Join(strings.Fields(s), " ")
}

// assembleConcepts attaches claims, degree, related ids, and the ghost flag
// to each collapsed group. A claim is one edge's Statement with provenance;
// it is attached to every resolvable endpoint concept (once, if both
// endpoints collapsed together). Degree counts distinct canonical neighbor
// concepts — self-loops and edges whose counterpart entity was not exported
// contribute nothing (an unlinkable endpoint cannot make a note navigable).
func assembleConcepts(groups []conceptGroup, canonicalByEntity map[string]string, events []engramclient.ExportEpisodic, edges []engramclient.ExportEdge) []Concept {
	eventSet := make(map[string]bool, len(events))
	for _, ev := range events {
		eventSet[ev.EventID] = true
	}

	claimsByConcept := make(map[string][]Claim)
	neighbors := make(map[string]map[string]bool)
	for _, ed := range edges {
		from, fromOK := canonicalByEntity[ed.FromEntityID]
		to, toOK := canonicalByEntity[ed.ToEntityID]
		claim := Claim{
			Statement:     ed.Statement,
			ValidAt:       ed.ValidAt,
			EdgeID:        ed.ID,
			SourceEventID: pickSourceEvent(ed.SourceIDs, eventSet),
		}
		if fromOK {
			claimsByConcept[from] = append(claimsByConcept[from], claim)
		}
		if toOK && to != from {
			claimsByConcept[to] = append(claimsByConcept[to], claim)
		}
		if fromOK && toOK && from != to {
			addNeighbor(neighbors, from, to)
			addNeighbor(neighbors, to, from)
		}
	}

	concepts := make([]Concept, 0, len(groups))
	for _, g := range groups {
		related := sortedKeys(neighbors[g.canonicalID])
		claims := claimsByConcept[g.canonicalID]
		sort.Slice(claims, func(i, j int) bool {
			if c := compareTimePtr(claims[i].ValidAt, claims[j].ValidAt); c != 0 {
				return c < 0
			}
			return claims[i].EdgeID < claims[j].EdgeID
		})
		degree := len(related)
		concepts = append(concepts, Concept{
			EntityID:   g.canonicalID,
			Name:       g.name,
			Aliases:    g.aliases,
			Degree:     degree,
			Claims:     claims,
			RelatedIDs: related,
			Ghost:      degree < hubMinDegree,
		})
	}
	return concepts
}

// addNeighbor records b as a distinct edge-endpoint neighbor of a.
func addNeighbor(neighbors map[string]map[string]bool, a, b string) {
	if neighbors[a] == nil {
		neighbors[a] = make(map[string]bool)
	}
	neighbors[a][b] = true
}

// pickSourceEvent chooses one deterministic source event id for a claim:
// the smallest source id with an exported Event, else the smallest source
// id at all (its quote renders absent), else "". The join is total — a
// claim never disappears because its source event is missing.
func pickSourceEvent(sourceIDs []string, eventSet map[string]bool) string {
	sorted := make([]string, 0, len(sourceIDs))
	for _, s := range sourceIDs {
		if s != "" {
			sorted = append(sorted, s)
		}
	}
	sort.Strings(sorted)
	for _, s := range sorted {
		if eventSet[s] {
			return s
		}
	}
	if len(sorted) > 0 {
		return sorted[0]
	}
	return ""
}

// assembleEvents builds the Event views: title derived from the first line
// of prose, raw body preserved, and ConceptIDs joined via each concept
// group's member SourceIDs (ghosts included — footers may link them
// unresolved).
func assembleEvents(episodics []engramclient.ExportEpisodic, groups []conceptGroup) []Event {
	conceptsByEvent := make(map[string]map[string]bool)
	for _, g := range groups {
		for _, src := range g.sourceIDs {
			addNeighbor(conceptsByEvent, src, g.canonicalID)
		}
	}
	events := make([]Event, 0, len(episodics))
	for _, ep := range episodics {
		events = append(events, Event{
			EventID:    ep.EventID,
			Title:      eventTitle(ep.Text, ep.EventID),
			Body:       ep.Text,
			OccurredAt: ep.OccurredAt,
			ConceptIDs: sortedKeys(conceptsByEvent[ep.EventID]),
		})
	}
	return events // dedupeEvents already sorted by EventID
}

// eventTitle derives a display title from untrusted prose: the first
// non-blank line, inline-cleaned, rune-capped; an id-derived fallback when
// nothing legible survives.
func eventTitle(text, eventID string) string {
	for _, line := range strings.Split(normalizeNewlines(text), "\n") {
		t := cleanInline(line)
		if t == "" {
			continue
		}
		if runes := []rune(t); len(runes) > maxEventTitleRunes {
			t = strings.TrimSpace(string(runes[:maxEventTitleRunes]))
		}
		return t
	}
	return idPrefix(eventID, 8)
}

// buildVaultRefs assigns every event and concept a deterministic,
// collision-suffixed noteRef. Homonym detection is global and
// case-insensitive across BOTH kinds (Obsidian resolves [[name]] vault-wide,
// so a cross-folder homonym is still ambiguous): ALL homonyms get an
// id-prefix suffix (not first-wins), residual clashes extend the prefix,
// and assignment order is sorted (id, folder) — independent of input order.
func buildVaultRefs(events []Event, concepts []Concept) VaultRefs {
	type cand struct {
		id, base, display, folder string
		suffix                    bool
	}
	cands := make([]cand, 0, len(events)+len(concepts))
	baseCount := make(map[string]int)

	add := func(c cand) {
		cands = append(cands, c)
		baseCount[strings.ToLower(c.base)]++
	}
	for _, ev := range events {
		slug := sanitizeFilename(ev.Title)
		forced := slug == ""
		if forced {
			slug = "event"
		}
		base := slug
		folder := "events/undated"
		if ev.OccurredAt != nil {
			day := ev.OccurredAt.UTC().Format("2006-01-02")
			base = day + " " + slug
			folder = "events/" + ev.OccurredAt.UTC().Format("2006")
		}
		add(cand{id: ev.EventID, base: base, display: ev.Title, folder: folder, suffix: forced})
	}
	for _, co := range concepts {
		base := sanitizeFilename(co.Name)
		forced := base == ""
		if forced {
			base = "concept"
		}
		add(cand{id: co.EntityID, base: base, display: cleanInline(co.Name), folder: "concepts", suffix: forced})
	}

	sort.Slice(cands, func(i, j int) bool {
		if cands[i].id != cands[j].id {
			return cands[i].id < cands[j].id
		}
		return cands[i].folder < cands[j].folder
	})
	refs := make(VaultRefs, len(cands))
	used := make(map[string]bool, len(cands))
	for _, c := range cands {
		if _, dup := refs[c.id]; dup {
			continue // pathological id shared across kinds: first (sorted) wins
		}
		name := c.base
		if c.suffix || baseCount[strings.ToLower(c.base)] > 1 {
			name = c.base + " (" + idPrefix(c.id, 8) + ")"
		}
		// Residual clashes (a literal name crafted to mimic a suffixed one)
		// extend the id prefix, then a counter — always terminates on an
		// unused name, deterministically.
		for n := 8; used[strings.ToLower(name)]; n += 4 {
			if n < len(c.id) {
				name = c.base + " (" + idPrefix(c.id, n+4) + ")"
			} else {
				name = c.base + " (" + c.id + "-" + strconv.Itoa(n) + ")"
			}
		}
		used[strings.ToLower(name)] = true
		display := c.display
		if display == "" {
			display = name
		}
		refs[c.id] = noteRef{File: name, Display: display, Folder: c.folder}
	}
	return refs
}

// sortedKeys returns the map's keys sorted ascending (nil map → empty).
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
