// vaultmaps.go groups the concept graph into deterministic topic maps (MOC
// notes): a connected-components pass over concept adjacency (Concept's
// already-symmetric RelatedIDs) partitions the graph, size-qualifying
// components become their own map, and everything left over funnels into
// size-bounded "misc" buckets so a mostly-disconnected graph — hundreds of
// small components, the real shape — never explodes into one map per node
// or collapses into one unbounded mega-note.
//
// clusterConcepts does ALL the cross-cluster bookkeeping (component
// discovery, misc bucketing, filename collision suffixing, per-cluster event
// timelines, cross-cluster out-links) up front, because renderMap only ever
// sees ONE Cluster plus the shared VaultRefs and has no view of the rest of
// the graph. Both halves are pure and deterministic: no wall clock, no
// randomness, fixed traversal and sort orders throughout — an identical
// model always yields identical clusters and identical map files.
//
// Pseudocode for clusterConcepts:
//
//	if model has no concepts: return no clusters
//	index concepts by id
//	find connected components:
//	    visit concept ids in ascending order; walk each unvisited one's
//	    component via its (already-sorted) RelatedIDs adjacency, marking
//	    every discovered id visited; sort each component's members
//	    ascending. Component discovery order is therefore ascending by
//	    each component's own smallest member id.
//	split components into "big" (>= minMembers) and "small" (< minMembers)
//	big components each become their own concept cluster:
//	    key = smallest member id; title source = highest-degree member,
//	    smallest-id tie-break
//	small components: flatten all their members together, sort by concept
//	    id, and chunk into misc buckets of at most miscBucketCap members —
//	    never one bucket per node, never one bucket for everything
//	assign every cluster (concept AND misc together) a deterministic,
//	    collision-suffixed "maps/<name>.md" path, processed in ascending
//	    key order — same algorithm buildVaultRefs already uses for events
//	    and concepts, with the "misc-" name space reserved so a concept
//	    map's sanitized title can never silently pass as a real misc bucket
//	for every cluster: attach its UTC source-event timeline (union of its
//	    members' source events, chronological) and its cross-cluster
//	    out-links (member RelatedIDs pointing outside the cluster)
//	return clusters in construction order: big components first (ascending
//	    smallest-id), then misc buckets in bucket-index order

package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// minMembers is the smallest connected component that earns its own map.
// Below it, a component's concepts funnel into a misc bucket instead.
const minMembers = 3

// miscBucketCap is the largest number of concepts a single misc-NN note may
// hold. Sub-threshold concepts are chunked into as many buckets as needed —
// never one unbounded mega-note.
const miscBucketCap = 50

// timelineEntry is one event on a cluster's UTC source-event timeline.
type timelineEntry struct {
	EventID    string
	OccurredAt *time.Time
}

// Cluster is one deterministic concept grouping bound for its own MOC note:
// either a connected component of at least minMembers concepts, or a slice
// of a size-bounded misc bucket collecting sub-threshold components. Every
// field renderMap needs is precomputed here: renderMap sees only one
// Cluster plus VaultRefs, but the cross-cluster bookkeeping (filename
// collisions, the member set an out-link must fall outside of, per-cluster
// event timelines) needs the whole graph in view, which only
// clusterConcepts has.
type Cluster struct {
	Kind         string          // "concept" | "misc"
	Key          string          // sort key that fixed this cluster's place in filename-assignment order
	TopConceptID string          // concept clusters only: highest-degree member (id tie-break); source of Title/filename
	MiscIndex    int             // misc clusters only: 1-based bucket index
	Title        string          // display-safe map title, assigned alongside RelPath
	RelPath      string          // "maps/<slug>.md", collision-suffixed the same way as VaultRefs
	Members      []string        // concept ids in this cluster, sorted ascending
	Timeline     []timelineEntry // this cluster's source events, chronological (nil OccurredAt last, EventID tie-break)
	OutLinkIDs   []string        // concept ids OUTSIDE Members that a member's RelatedIDs points to, sorted
}

// clusterConcepts groups model's concept graph into Clusters (see the
// package-level pseudocode above). An empty model yields no clusters.
func clusterConcepts(model VaultModel) []Cluster {
	if len(model.Concepts) == 0 {
		return nil
	}

	conceptByID := make(map[string]Concept, len(model.Concepts))
	for _, c := range model.Concepts {
		conceptByID[c.EntityID] = c
	}

	var big, small [][]string
	for _, comp := range findComponents(model.Concepts, conceptByID) {
		if len(comp) >= minMembers {
			big = append(big, comp)
		} else {
			small = append(small, comp)
		}
	}
	buckets := miscBuckets(small)

	clusters := make([]*Cluster, 0, len(big)+len(buckets))
	for _, comp := range big {
		clusters = append(clusters, &Cluster{
			Kind:         "concept",
			Key:          comp[0], // comp is sorted ascending: comp[0] is its smallest member id
			TopConceptID: topByDegree(comp, conceptByID),
			Members:      comp,
		})
	}
	for i, bucket := range buckets {
		clusters = append(clusters, &Cluster{
			Kind:      "misc",
			Key:       fmt.Sprintf("misc:%06d", i+1),
			MiscIndex: i + 1,
			Members:   bucket,
		})
	}

	assignClusterFilenames(clusters, conceptByID, digitWidth(len(buckets)))

	conceptToEvents, eventByID := indexEvents(model.Events)
	out := make([]Cluster, len(clusters))
	for i, cl := range clusters {
		cl.Timeline = buildTimeline(cl.Members, conceptToEvents, eventByID)
		cl.OutLinkIDs = outLinks(cl.Members, conceptByID)
		out[i] = *cl
	}
	return out
}

// findComponents partitions concepts into connected components using
// RelatedIDs adjacency (already symmetric, already referencing only valid
// concept ids — assembleConcepts guarantees this). Traversal is fixed and
// independent of input order: concepts are visited by ascending EntityID,
// and each component's walk follows RelatedIDs, which is already sorted —
// so an identical model discovers byte-identical components in
// byte-identical order every run. Each returned component is itself sorted
// ascending.
func findComponents(concepts []Concept, conceptByID map[string]Concept) [][]string {
	ids := make([]string, 0, len(concepts))
	for _, c := range concepts {
		ids = append(ids, c.EntityID)
	}
	sort.Strings(ids)

	visited := make(map[string]bool, len(ids))
	var components [][]string
	for _, start := range ids {
		if visited[start] {
			continue
		}
		components = append(components, walkComponent(start, conceptByID, visited))
	}
	return components
}

// walkComponent BFS-walks the connected component containing start, marking
// every discovered id visited along the way, and returns its members sorted
// ascending. Neighbor expansion is delegated to enqueueUnvisited so this
// loop never nests past a single level.
func walkComponent(start string, conceptByID map[string]Concept, visited map[string]bool) []string {
	visited[start] = true
	queue := []string{start}
	var members []string
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		members = append(members, id)
		queue = enqueueUnvisited(conceptByID[id].RelatedIDs, visited, queue)
	}
	sort.Strings(members)
	return members
}

// enqueueUnvisited appends every not-yet-visited neighbor to queue, marking
// each visited immediately (before its own turn at the front of the queue)
// so no neighbor is ever enqueued twice.
func enqueueUnvisited(neighbors []string, visited map[string]bool, queue []string) []string {
	for _, nb := range neighbors {
		if visited[nb] {
			continue
		}
		visited[nb] = true
		queue = append(queue, nb)
	}
	return queue
}

// topByDegree returns the member with the highest Degree, breaking ties by
// the smallest concept id. members must already be sorted ascending (every
// caller passes a component from findComponents, which guarantees this) so
// a strict ">" comparison in ascending-id order keeps the first (smallest
// id) among equal-degree ties automatically.
func topByDegree(members []string, conceptByID map[string]Concept) string {
	best := members[0]
	bestDegree := conceptByID[best].Degree
	for _, id := range members[1:] {
		if d := conceptByID[id].Degree; d > bestDegree {
			best, bestDegree = id, d
		}
	}
	return best
}

// miscBuckets flattens every sub-threshold component's members, sorts the
// whole set by concept id, and chunks it into miscBucketCap-sized groups —
// bounded regardless of how many tiny components feed into it, never one
// bucket per node.
func miscBuckets(small [][]string) [][]string {
	var flat []string
	for _, comp := range small {
		flat = append(flat, comp...)
	}
	sort.Strings(flat)

	var buckets [][]string
	for len(flat) > 0 {
		n := miscBucketCap
		if n > len(flat) {
			n = len(flat)
		}
		buckets = append(buckets, flat[:n:n])
		flat = flat[n:]
	}
	return buckets
}

// digitWidth returns the zero-padded digit width misc bucket numbers use:
// at least 2 ("misc-01"), wider only if the bucket count needs it.
func digitWidth(miscTotal int) int {
	digits := len(strconv.Itoa(miscTotal))
	if digits < 2 {
		return 2
	}
	return digits
}

// assignClusterFilenames gives every cluster a deterministic, collision-
// suffixed "maps/<name>.md" RelPath (and its display Title), assigned
// across ALL clusters together in ascending-Key order — the same algorithm
// buildVaultRefs uses for events and concepts: every homonym gets suffixed
// (not first-wins), and residual clashes extend the suffix until unique.
//
// The "misc-" name space is reserved and one-directional: a misc bucket's
// canonical "misc-NN" name is authoritative and never suffixed (its
// MiscIndex already makes it unique among misc buckets, so it never needs
// to be), while a concept cluster whose sanitized title would itself read
// as a misc-NN name is unconditionally forced through the suffix path. A
// concept map can therefore never clobber (or be confused for) a misc
// bucket, but a misc bucket can never be bumped by one either.
func assignClusterFilenames(clusters []*Cluster, conceptByID map[string]Concept, digits int) {
	ordered := append([]*Cluster(nil), clusters...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Key < ordered[j].Key })

	type resolved struct {
		cl     *Cluster
		base   string
		forced bool // concept clusters only: always suffix, regardless of baseCount
		immune bool // misc clusters only: canonical name is authoritative, never suffixed
	}
	baseCount := make(map[string]int, len(ordered)) // tallied over concept-cluster bases only
	rs := make([]resolved, 0, len(ordered))
	for _, cl := range ordered {
		var base string
		var forced bool
		switch cl.Kind {
		case "misc":
			base = fmt.Sprintf("misc-%0*d", digits, cl.MiscIndex)
			cl.Title = fmt.Sprintf("Misc %0*d", digits, cl.MiscIndex)
		default:
			name := conceptByID[cl.TopConceptID].Name
			cl.Title = cleanInline(name)
			base = sanitizeFilename(name)
			if base == "" {
				base, forced = "map", true
			}
			if strings.HasPrefix(strings.ToLower(base), "misc-") {
				forced = true // reserved namespace: never pass as (or bump) a real misc bucket
			}
			baseCount[strings.ToLower(base)]++
		}
		rs = append(rs, resolved{cl: cl, base: base, forced: forced, immune: cl.Kind == "misc"})
	}

	used := make(map[string]bool, len(rs))
	for _, r := range rs {
		name := r.base
		if !r.immune && (r.forced || baseCount[strings.ToLower(r.base)] > 1) {
			name = r.base + " (" + idPrefix(r.cl.Key, 8) + ")"
		}
		for n := 8; used[strings.ToLower(name)]; n += 4 {
			if n < len(r.cl.Key) {
				name = r.base + " (" + idPrefix(r.cl.Key, n+4) + ")"
			} else {
				name = r.base + " (" + r.cl.Key + "-" + strconv.Itoa(n) + ")"
			}
		}
		used[strings.ToLower(name)] = true
		r.cl.RelPath = "maps/" + name + ".md"
	}
}

// indexEvents builds the reverse concept -> event-id lookup and an
// event-by-id map, both needed to assemble every cluster's timeline without
// rescanning model.Events once per cluster.
func indexEvents(events []Event) (conceptToEvents map[string]map[string]bool, eventByID map[string]Event) {
	conceptToEvents = make(map[string]map[string]bool)
	eventByID = make(map[string]Event, len(events))
	for _, ev := range events {
		eventByID[ev.EventID] = ev
		for _, cid := range ev.ConceptIDs {
			addNeighbor(conceptToEvents, cid, ev.EventID)
		}
	}
	return conceptToEvents, eventByID
}

// buildTimeline unions every member concept's source events (deduped),
// ordered chronologically — nil OccurredAt last, EventID tie-break, reusing
// the same ordering buildVaultModel already applies to claims.
func buildTimeline(members []string, conceptToEvents map[string]map[string]bool, eventByID map[string]Event) []timelineEntry {
	var ids []string
	seen := make(map[string]bool)
	for _, m := range members {
		for eid := range conceptToEvents[m] {
			if !seen[eid] {
				seen[eid] = true
				ids = append(ids, eid)
			}
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := eventByID[ids[i]].OccurredAt, eventByID[ids[j]].OccurredAt
		if c := compareTimePtr(a, b); c != 0 {
			return c < 0
		}
		return ids[i] < ids[j]
	})
	out := make([]timelineEntry, len(ids))
	for i, eid := range ids {
		out[i] = timelineEntry{EventID: eid, OccurredAt: eventByID[eid].OccurredAt}
	}
	return out
}

// outLinks collects the concept ids OUTSIDE members that any member's
// RelatedIDs points to — this cluster's cross-cluster edges — sorted.
func outLinks(members []string, conceptByID map[string]Concept) []string {
	inCluster := make(map[string]bool, len(members))
	for _, m := range members {
		inCluster[m] = true
	}
	set := make(map[string]bool)
	for _, m := range members {
		for _, nb := range conceptByID[m].RelatedIDs {
			if !inCluster[nb] {
				set[nb] = true
			}
		}
	}
	return sortedKeys(set)
}

// wikilink renders id as a piped Obsidian link via refs, or "" if refs has
// no entry for it. clusterConcepts only ever emits ids drawn from the same
// model VaultRefs was built from, so a miss would be a caller bug, not
// expected input — rendering nothing (rather than a malformed "[[|]]") is
// the defensive choice.
func wikilink(refs VaultRefs, id string) string {
	nr, ok := refs[id]
	if !ok {
		return ""
	}
	return "[[" + nr.File + "|" + nr.Display + "]]"
}

// renderMap renders one Cluster into its MOC note: an H1 title, the member-
// concept list, this cluster's UTC source-event timeline, and its
// cross-cluster out-links. relPath and Title were already assigned by
// clusterConcepts (which alone has the whole-graph view collision
// suffixing needs); renderMap only walks this one cluster.
func renderMap(cluster Cluster, refs VaultRefs) (relPath, content string) {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n## Concepts\n\n", cluster.Title)
	for _, id := range cluster.Members {
		if lk := wikilink(refs, id); lk != "" {
			fmt.Fprintf(&b, "- %s\n", lk)
		}
	}

	if len(cluster.Timeline) > 0 {
		b.WriteString("\n## Timeline\n\n")
		for _, t := range cluster.Timeline {
			lk := wikilink(refs, t.EventID)
			if lk == "" {
				continue
			}
			if t.OccurredAt != nil {
				fmt.Fprintf(&b, "- %s — %s\n", t.OccurredAt.UTC().Format("2006-01-02"), lk)
			} else {
				fmt.Fprintf(&b, "- %s\n", lk)
			}
		}
	}

	if len(cluster.OutLinkIDs) > 0 {
		b.WriteString("\n## Cross-cluster links\n\n")
		for _, id := range cluster.OutLinkIDs {
			if lk := wikilink(refs, id); lk != "" {
				fmt.Fprintf(&b, "- %s\n", lk)
			}
		}
	}

	return cluster.RelPath, b.String()
}
