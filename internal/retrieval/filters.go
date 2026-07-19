package retrieval

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ErrInvalidFilter marks every filter-validation failure — an unknown field, an
// unsupported op, a non-scalar value, an unknown or empty source, an
// unfilterable source named alongside filters. It is the CALLER's error, not the
// server's: the entry barricades (internal/mcp, internal/server) map it to an
// invalid-argument response rather than reporting a caller mistake as an
// internal fault. Infrastructure failures (embedding, HTTP, decoding) never wrap
// it.
var ErrInvalidFilter = errors.New("retrieval: invalid filter")

// invalidFilterf formats a validation error wrapping ErrInvalidFilter. Every
// message names the valid vocabulary, so an LLM caller can self-correct from the
// error text alone.
func invalidFilterf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidFilter, fmt.Sprintf(format, args...))
}

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
//
// Target is the physical document field the clause is compiled against; empty
// means "the same name the predicate uses". A non-empty Target lets one
// caller-facing field name mean the right thing on each tier — "time" is
// occurred_at on episodic and valid_at on semantic. Without it, a single
// caller-facing time filter would have to name one physical field, which the
// OTHER tier does not declare and would therefore leave unconstrained: the
// silent-inertness trap this registry exists to prevent, reintroduced through
// the one filter both tiers must honor.
type FieldSpec struct {
	Type   string
	Ops    []string
	Target string
}

// field is the physical document field p compiles against on this tier.
func (s FieldSpec) field(name string) string {
	if s.Target != "" {
		return s.Target
	}
	return name
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

// TimeField is the tier-neutral name for "when the memory happened": it
// compiles to occurred_at on episodic and valid_at on semantic. It is what the
// caller-facing since/until bounds (internal/mcp, internal/server) become, so
// ONE pair of bounds constrains every built-in tier by its own event-time field
// rather than leaving whichever tier lacks the named field unconstrained.
const TimeField = "time"

// dateAlias declares a date field under an alias that compiles to target.
func dateAlias(target string) FieldSpec {
	return FieldSpec{Type: fieldTypeDate, Ops: []string{"range"}, Target: target}
}

// episodicFilterable declares the episodic tier's filter surface: the event
// kind, its two time fields, and the tier-neutral TimeField alias. Every entry
// is mapped keyword/date in templates/episodic.json.
var episodicFilterable = FilterableFields{
	"kind":        keywordField(),
	"occurred_at": dateField(),
	"created_at":  dateField(),
	TimeField:     dateAlias("occurred_at"),
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
	TimeField:           dateAlias("valid_at"),
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
	// The clause is compiled against the tier's PHYSICAL field (spec.field),
	// which is the predicate's own name unless the tier declared an alias.
	c, err := filterClause(spec.field(p.Field), p.Op, p.Value)
	return c, true, err
}

// validate checks p against this field's declared ops and value rules.
func (s FieldSpec) validate(p Predicate) error {
	if !slices.Contains(s.Ops, p.Op) {
		return invalidFilterf("unsupported filter op %q on %s field %q; valid ops: %s",
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
			return invalidFilterf("filter value on field %q must be a scalar (string, number or bool), got %T", p.Field, p.Value)
		}
		return nil
	}
	bounds, ok := p.Value.(map[string]any)
	if !ok {
		return invalidFilterf("range filter on field %q requires a value of gte/lte bounds, got %T", p.Field, p.Value)
	}
	for b := range bounds {
		if b != "gte" && b != "lte" {
			return invalidFilterf("unknown range bound %q on field %q; valid bounds: gte, lte", b, p.Field)
		}
	}
	set := 0
	for _, b := range [2]string{"gte", "lte"} {
		v, present := bounds[b]
		if !present {
			continue
		}
		if !isScalar(v) {
			return invalidFilterf("range bound %q on field %q must be a scalar, got %T", b, p.Field, v)
		}
		set++
	}
	if set == 0 {
		return invalidFilterf("range filter on field %q must set at least one of \"gte\"/\"lte\"", p.Field)
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
		return nil, invalidFilterf("sources is empty; omit it to search every source, or name at least one of: %s", fieldListOrNone(valid))
	}
	known := make(map[string]struct{}, len(valid))
	for _, v := range valid {
		known[v] = struct{}{}
	}
	set := make(sourceSet, len(sources))
	for _, s := range sources {
		if _, ok := known[s]; !ok {
			return nil, invalidFilterf("unknown source %q; valid sources: %s", s, fieldListOrNone(valid))
		}
		set[s] = struct{}{}
	}
	return set, nil
}

// unfilterableSourceNames returns the registered sources that declare no
// filterable fields, sorted. Today that is every registered tier source
// ("experience") and every post-hook ("graph"): neither the TierSource nor the
// PostHook interface carries a FilterableFields declaration, so neither can
// compile a predicate into the query it issues.
func (m *MultiRetriever) unfilterableSourceNames() []string {
	out := make([]string, 0, len(m.tierSrcs)+len(m.postHooks))
	for _, s := range m.tierSrcs {
		out = append(out, s.name)
	}
	for _, h := range m.postHooks {
		out = append(out, h.name)
	}
	sort.Strings(out)
	return out
}

// validateFilterableSources rejects a search that EXPLICITLY names a source
// which cannot honor the filter it also carries.
//
// This is the other half of the contract selectSources enforces (see there for
// the rationale): a filtered search does not run unfilterable sources. When the
// exclusion is implicit (Sources omitted), dropping them silently is the
// documented behavior — the result is narrowed, never widened. But when the
// caller NAMED such a source and also passed a filter, the two requests
// contradict each other, and silently honoring one of them would be exactly the
// kind of quiet surprise this package refuses to serve.
func (m *MultiRetriever) validateFilterableSources(preds []Predicate, sel sourceSet) error {
	if len(preds) == 0 || sel == nil {
		return nil
	}
	for _, name := range m.unfilterableSourceNames() {
		if sel.selected(name) {
			return invalidFilterf("source %q cannot be filtered; drop the filters or the source (unfilterable sources: %s)",
				name, fieldListOrNone(m.unfilterableSourceNames()))
		}
	}
	return nil
}

// selectSources partitions the registered sources into the ones this search
// runs. It is the single place Filter.Sources is applied — the built-in tier
// fan-out, the registered tier-source fan-out, and the post-hook chain all
// consume its output, so a source excluded here is excluded from every one of
// them (DW-4.5) rather than from whichever loop someone remembered to gate.
//
// filtered reports whether the search carries any Predicate. When it does,
// sources that declare no filterable fields (registered tier sources and
// post-hooks) DO NOT RUN. That is a deliberate, documented contract, decided in
// Phase 5: those sources receive no predicates, so their hits would come back
// unconstrained while every other source's were narrowed — a caller filtering
// for kind="conversation" would silently get experience and graph hits of every
// kind. A source that cannot evaluate the constraint cannot honestly answer the
// question, so it is excluded rather than allowed to smuggle unfiltered results
// into a filtered result set. (A built-in tier that merely lacks the ONE field
// being filtered on is a different case: it stays in, unconstrained by that
// field — its surface is declared, visible in every error message, and
// excludable by name. See DW-4.2.)
//
// It does not touch the ACL: the returned sources still run under the compiled
// filter, and their hits are still re-verified. Narrowing sources can only
// REMOVE results from a search, never admit one the ACL would have denied.
func (m *MultiRetriever) selectSources(sel sourceSet, filtered bool) ([]*tierRetriever, []namedTierSource, []namedPostHook) {
	if sel == nil && !filtered { // nil Sources, no filter: every source, no work at all.
		return m.tiers, m.tierSrcs, m.postHooks
	}
	if filtered {
		// Unfilterable sources are out. An explicitly named one already
		// errored at validateFilterableSources, so reaching here means the
		// exclusion is the implicit, documented one.
		tiers := make([]*tierRetriever, 0, len(m.tiers))
		for _, t := range m.tiers {
			if sel.selected(t.source) {
				tiers = append(tiers, t)
			}
		}
		return tiers, nil, nil
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
		return invalidFilterf("too many filter predicates (%d); at most %d", len(preds), MaxPredicates)
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
			return invalidFilterf("unknown or unfilterable field %q for the selected sources; valid filterable fields: %s",
				p.Field, fieldListOrNone(m.filterableFieldNames(sel)))
		}
	}
	return nil
}
