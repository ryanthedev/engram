package retrieval

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// MaxPredicates bounds the number of filter predicates one memory search may
// carry. Predicates arrive from the MCP caller across a process boundary —
// external input — so their count is capped, never trusted, for the same
// reason MaxK caps the result count: an unbounded list would let one request
// compile an arbitrarily large query body onto every tier.
const MaxPredicates = 32

// Field types a tier may declare. They name the OpenSearch mapping type of the
// underlying field (see internal/store/templates/{episodic,semantic}.json,
// both of which are "dynamic": "strict" — a predicate on a field the index
// does not map is not merely inert, it is a mapping error). The type decides
// which Predicate ops are legal on the field.
const (
	fieldTypeKeyword = "keyword"
	fieldTypeDate    = "date"
)

// FieldSpec declares one filterable field on a tier: the OpenSearch mapping
// type of the field, and the Predicate ops valid on it. Ops is the whole
// vocabulary for the field — an op outside it is a validation error, never a
// silently-dropped clause.
type FieldSpec struct {
	Type string
	Ops  []string
}

// FilterableFields is one tier's declared filter surface: field name ->
// FieldSpec. It is the tier-gating mechanism: a tier compiles ONLY the
// predicates it declares here, so a predicate on a field another tier owns
// (e.g. "kind", episodic-only) can never reach — and therefore can never
// zero — this tier's query. Adding a new filterable field is one entry here;
// there is no separate hand-written gate to forget.
type FilterableFields map[string]FieldSpec

// keywordField / dateField are the two field shapes the memory tiers declare.
// Keyword fields take exact-match and prefix filters; date fields take range
// filters (the since/until surface Phase 5 compiles down to).
func keywordField() FieldSpec { return FieldSpec{Type: fieldTypeKeyword, Ops: []string{"term", "prefix"}} }
func dateField() FieldSpec    { return FieldSpec{Type: fieldTypeDate, Ops: []string{"range"}} }

// episodicFilterable declares the episodic tier's filter surface: the event
// kind plus its two time fields. Every entry is mapped keyword/date in
// templates/episodic.json.
var episodicFilterable = FilterableFields{
	"kind":        keywordField(),
	"occurred_at": dateField(),
	"created_at":  dateField(),
}

// semanticFilterable declares the semantic tier's filter surface: the fact
// triple, the extractor provenance, and the bi-temporal time fields. Every
// entry is mapped keyword/date in templates/semantic.json. Note the absence of
// "kind" — it exists only on episodic documents, which is precisely why a kind
// predicate must not be compiled into this tier's query.
var semanticFilterable = FilterableFields{
	"subject":           keywordField(),
	"predicate":         keywordField(),
	"object":            keywordField(),
	"extractor_version": keywordField(),
	"valid_at":          dateField(),
	"invalid_at":        dateField(),
	"created_at":        dateField(),
	"expired_at":        dateField(),
}

// names returns the declared field names in sorted order, for self-correcting
// "unknown field" error messages.
func (ff FilterableFields) names() []string {
	out := make([]string, 0, len(ff))
	for name := range ff {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// clauseFor compiles p into an OpenSearch filter clause for this tier.
//
// declared reports whether this tier owns p.Field. declared == false is the
// routing case, NOT a failure: the predicate belongs to some other tier and
// this tier's query is simply left unconstrained by it (DW-4.2). Whether a
// field is owned by NO tier at all is a validation error, decided once at the
// MultiRetriever barricade — a tier alone cannot tell the difference.
//
// An error means p is malformed for the field it names (bad op, non-scalar or
// missing value). Search validates every predicate before any tier runs, so
// this is defense in depth: a tier reached through some future path with an
// unvalidated predicate fails closed rather than emitting a clause it never
// checked.
func (ff FilterableFields) clauseFor(p Predicate) (clause any, declared bool, err error) {
	spec, ok := ff[p.Field]
	if !ok {
		return nil, false, nil
	}
	if err := spec.validate(p); err != nil {
		return nil, true, err
	}
	c, err := filterClause(p.Field, p.Op, p.Value)
	return c, true, err
}

// validate checks p against this field's declared ops and value rules.
func (s FieldSpec) validate(p Predicate) error {
	if !slices.Contains(s.Ops, p.Op) {
		return fmt.Errorf("retrieval: unsupported filter op %q on %s field %q; valid ops: %s",
			p.Op, s.Type, p.Field, strings.Join(s.Ops, ", "))
	}
	return validatePredicateValue(p)
}

// validatePredicateValue is the injection barricade (DW-4.8). A filter value is
// DATA: it is placed into a clause structure and marshaled, never interpolated
// into a query string — so a value carrying OpenSearch DSL text is inert, just
// a string that matches no keyword. This check closes the one remaining way a
// value could become STRUCTURE: a map or slice value would land in the clause
// tree as a nested DSL object rather than as a leaf. Values must therefore be
// scalars (range bounds included).
func validatePredicateValue(p Predicate) error {
	if p.Op != "range" {
		if !isScalar(p.Value) {
			return fmt.Errorf("retrieval: filter value on field %q must be a scalar (string, number or bool), got %T", p.Field, p.Value)
		}
		return nil
	}
	bounds, ok := p.Value.(map[string]any)
	if !ok {
		return fmt.Errorf("retrieval: range filter on field %q requires a value of gte/lte bounds, got %T", p.Field, p.Value)
	}
	for b := range bounds {
		if b != "gte" && b != "lte" {
			return fmt.Errorf("retrieval: unknown range bound %q on field %q; valid bounds: gte, lte", b, p.Field)
		}
	}
	set := 0
	for _, b := range [2]string{"gte", "lte"} {
		v, present := bounds[b]
		if !present {
			continue
		}
		if !isScalar(v) {
			return fmt.Errorf("retrieval: range bound %q on field %q must be a scalar, got %T", b, p.Field, v)
		}
		set++
	}
	if set == 0 {
		return fmt.Errorf("retrieval: range filter on field %q must set at least one of \"gte\"/\"lte\"", p.Field)
	}
	return nil
}

// isScalar reports whether v is a JSON leaf value (never a nested object or
// array, which in a clause position would be structure rather than data).
func isScalar(v any) bool {
	switch v.(type) {
	case string, bool, int, int32, int64, float32, float64:
		return true
	default:
		return false
	}
}

// sourceSet is the resolved set of sources one search runs. A nil sourceSet
// means every registered source (Filter.Sources was nil); an EMPTY Filter.Sources
// never reaches here — it is a validation error, not a silent "all" (see
// resolveSources).
type sourceSet map[string]struct{}

// selected reports whether the named source runs in this search.
func (s sourceSet) selected(name string) bool {
	if s == nil {
		return true
	}
	_, ok := s[name]
	return ok
}

// sourceNames returns every source name this MultiRetriever can search, sorted:
// the built-in tiers ("episodic", "semantic"), the registered tier sources, and
// the registered post-hooks ("graph"). Post-hooks share the namespace because a
// caller asking for "graph" is asking for graph-expanded hits and should not
// have to know that graph is a post-fusion hook rather than a tier.
func (m *MultiRetriever) sourceNames() []string {
	out := make([]string, 0, len(m.tiers)+len(m.tierSrcs)+len(m.postHooks))
	for _, t := range m.tiers {
		out = append(out, t.source)
	}
	for _, s := range m.tierSrcs {
		out = append(out, s.name)
	}
	for _, h := range m.postHooks {
		out = append(out, h.name)
	}
	sort.Strings(out)
	return out
}

// resolveSources validates Filter.Sources against the registered source
// vocabulary and returns the set of sources to search.
//
// nil Sources means every source (the pre-filter default; the empty Filter must
// keep behaving exactly as it does today). A non-nil but EMPTY Sources is an
// error, deliberately: "search nothing" and "search everything" must never be
// the same request, and a caller that built an empty list from a bad mapping
// should hear about it rather than silently receive all sources.
func (m *MultiRetriever) resolveSources(sources []string) (sourceSet, error) {
	if sources == nil {
		return nil, nil
	}
	valid := m.sourceNames()
	if len(sources) == 0 {
		return nil, fmt.Errorf("retrieval: sources is empty; omit it to search every source, or name at least one of: %s", fieldListOrNone(valid))
	}
	known := make(map[string]struct{}, len(valid))
	for _, v := range valid {
		known[v] = struct{}{}
	}
	set := make(sourceSet, len(sources))
	for _, s := range sources {
		if _, ok := known[s]; !ok {
			return nil, fmt.Errorf("retrieval: unknown source %q; valid sources: %s", s, fieldListOrNone(valid))
		}
		set[s] = struct{}{}
	}
	return set, nil
}

// selectSources partitions the registered sources into the ones this search
// runs. It is the single place Filter.Sources is applied — the built-in tier
// fan-out, the registered tier-source fan-out, and the post-hook chain all
// consume its output, so a source excluded here is excluded from every one of
// them (DW-4.5) rather than from whichever loop someone remembered to gate.
//
// It does not touch the ACL: the returned sources still run under the compiled
// filter, and their hits are still re-verified. Narrowing sources can only
// REMOVE results from a search, never admit one the ACL would have denied.
func (m *MultiRetriever) selectSources(sel sourceSet) ([]*tierRetriever, []namedTierSource, []namedPostHook) {
	if sel == nil { // nil Sources: every source, no filtering work at all.
		return m.tiers, m.tierSrcs, m.postHooks
	}
	tiers := make([]*tierRetriever, 0, len(m.tiers))
	for _, t := range m.tiers {
		if sel.selected(t.source) {
			tiers = append(tiers, t)
		}
	}
	srcs := make([]namedTierSource, 0, len(m.tierSrcs))
	for _, s := range m.tierSrcs {
		if sel.selected(s.name) {
			srcs = append(srcs, s)
		}
	}
	hooks := make([]namedPostHook, 0, len(m.postHooks))
	for _, h := range m.postHooks {
		if sel.selected(h.name) {
			hooks = append(hooks, h)
		}
	}
	return tiers, srcs, hooks
}

// filterableFieldNames returns the union of the filterable fields declared by
// the SELECTED built-in tiers, sorted — the vocabulary an "unknown field" error
// names back to the caller.
func (m *MultiRetriever) filterableFieldNames(sel sourceSet) []string {
	seen := map[string]struct{}{}
	for _, t := range m.tiers {
		if !sel.selected(t.source) {
			continue
		}
		for _, name := range t.filterable.names() {
			seen[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// validatePredicates is the filter barricade: every caller-supplied predicate is
// checked HERE, before any tier issues an HTTP request, so a malformed filter
// costs zero cluster round-trips and the tiers downstream can compile against
// data they may assume well-formed.
//
// A predicate whose field is declared by no SELECTED tier is a validation error
// naming the valid fields (usability: an LLM caller can self-correct). Scoping
// the check to the selected tiers — rather than to all tiers — is what keeps
// the last silent-drop path closed: `Sources:["semantic"] + kind` would
// otherwise compile to a predicate that constrains nothing at all, which is the
// exact class of silent inertness this phase exists to eliminate.
//
// A predicate declared by SOME selected tier but not by another is routed, not
// rejected: the tiers that own the field filter on it, the tiers that do not
// are left unconstrained (DW-4.2).
func (m *MultiRetriever) validatePredicates(preds []Predicate, sel sourceSet) error {
	if len(preds) == 0 {
		return nil
	}
	if len(preds) > MaxPredicates {
		return fmt.Errorf("retrieval: too many filter predicates (%d); at most %d", len(preds), MaxPredicates)
	}
	for _, p := range preds {
		declared := false
		for _, t := range m.tiers {
			if !sel.selected(t.source) {
				continue
			}
			_, ok, err := t.filterable.clauseFor(p)
			if err != nil {
				return err
			}
			declared = declared || ok
		}
		if !declared {
			return fmt.Errorf("retrieval: unknown or unfilterable field %q for the selected sources; valid filterable fields: %s",
				p.Field, fieldListOrNone(m.filterableFieldNames(sel)))
		}
	}
	return nil
}
