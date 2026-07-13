package retrieval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// TestDW_4_1_TierFilterableFieldRegistry pins each tier's declared filter
// surface: episodic owns "kind" plus its time fields; semantic owns the fact
// triple, extractor_version, and its bi-temporal time fields. The disjointness
// assertion at the end is the structural property the whole phase rests on —
// "kind" must be declared by episodic and by NOTHING else, or a kind predicate
// could reach a tier whose index does not map the field.
func TestDW_4_1_TierFilterableFieldRegistry(t *testing.T) {
	cases := []struct {
		tier      string
		fields    FilterableFields
		want      []string
		wantTypes map[string]string
	}{
		{
			tier:   "episodic",
			fields: episodicFilterable,
			want:   []string{"created_at", "kind", "occurred_at"},
			wantTypes: map[string]string{
				"kind": fieldTypeKeyword, "occurred_at": fieldTypeDate, "created_at": fieldTypeDate,
			},
		},
		{
			tier:   "semantic",
			fields: semanticFilterable,
			want: []string{
				"created_at", "expired_at", "extractor_version", "invalid_at",
				"object", "predicate", "subject", "valid_at",
			},
			wantTypes: map[string]string{
				"subject": fieldTypeKeyword, "predicate": fieldTypeKeyword, "object": fieldTypeKeyword,
				"extractor_version": fieldTypeKeyword, "valid_at": fieldTypeDate, "invalid_at": fieldTypeDate,
				"created_at": fieldTypeDate, "expired_at": fieldTypeDate,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			if got := tc.fields.names(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("declared fields = %v, want %v", got, tc.want)
			}
			for field, wantType := range tc.wantTypes {
				spec, ok := tc.fields[field]
				if !ok {
					t.Fatalf("field %q is not declared", field)
				}
				if spec.Type != wantType {
					t.Errorf("field %q type = %q, want %q", field, spec.Type, wantType)
				}
				wantOps := []string{"term", "prefix"}
				if wantType == fieldTypeDate {
					wantOps = []string{"range"}
				}
				if !reflect.DeepEqual(spec.Ops, wantOps) {
					t.Errorf("field %q ops = %v, want %v", field, spec.Ops, wantOps)
				}
			}
		})
	}

	// The structural guarantee: no tier other than episodic declares "kind",
	// and no tier other than semantic declares the fact triple.
	if _, ok := semanticFilterable["kind"]; ok {
		t.Error(`semantic declares "kind" — a kind predicate would zero its query (the bug this phase closes)`)
	}
	for _, f := range []string{"subject", "predicate", "object", "extractor_version"} {
		if _, ok := episodicFilterable[f]; ok {
			t.Errorf("episodic declares semantic-only field %q", f)
		}
	}
}

// TestDW_4_1_DeclaredFieldsExistInIndexTemplates is the assumption guard: every
// declared filterable field must actually be mapped keyword/date in the index
// template, with the type the registry claims. Both templates are
// "dynamic": "strict", so a registry entry for an unmapped field would not be
// merely inert — the query would be rejected by OpenSearch outright. This test
// catches a bad registry entry at unit-test time rather than in production.
func TestDW_4_1_DeclaredFieldsExistInIndexTemplates(t *testing.T) {
	cases := []struct {
		template string
		fields   FilterableFields
	}{
		{"episodic.json", episodicFilterable},
		{"semantic.json", semanticFilterable},
	}
	for _, tc := range cases {
		t.Run(tc.template, func(t *testing.T) {
			mapped, strict := templateProperties(t, tc.template)
			if !strict {
				t.Error(`template is no longer "dynamic": "strict" — this test's premise (an unmapped filter field is a hard error) has changed; re-check the registry`)
			}
			for field, spec := range tc.fields {
				gotType, ok := mapped[field]
				if !ok {
					t.Errorf("declared filterable field %q is not mapped in %s", field, tc.template)
					continue
				}
				if gotType != spec.Type {
					t.Errorf("field %q declared as %q but mapped as %q in %s", field, spec.Type, gotType, tc.template)
				}
			}
		})
	}
}

// templateProperties reads field -> mapping type from an index template, plus
// whether the mapping is dynamic:strict.
func templateProperties(t *testing.T, name string) (map[string]string, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "store", "templates", name))
	if err != nil {
		t.Fatalf("reading template: %v", err)
	}
	var doc struct {
		Template struct {
			Mappings struct {
				Dynamic    string `json:"dynamic"`
				Properties map[string]struct {
					Type string `json:"type"`
				} `json:"properties"`
			} `json:"mappings"`
		} `json:"template"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing template %s: %v", name, err)
	}
	out := make(map[string]string, len(doc.Template.Mappings.Properties))
	for field, p := range doc.Template.Mappings.Properties {
		out[field] = p.Type
	}
	return out, doc.Template.Mappings.Dynamic == "strict"
}

// TestClauseForRoutesRatherThanFails: a field another tier owns reports
// declared=false with no error — the routing contract that keeps a kind
// predicate from zeroing the semantic tier.
func TestClauseForRoutesRatherThanFails(t *testing.T) {
	clause, declared, err := semanticFilterable.clauseFor(Predicate{Field: "kind", Op: "term", Value: "decision"})
	if err != nil {
		t.Fatalf("clauseFor on an undeclared field errored, want a routing miss: %v", err)
	}
	if declared || clause != nil {
		t.Errorf("semantic claimed field %q (declared=%v, clause=%v)", "kind", declared, clause)
	}

	clause, declared, err = episodicFilterable.clauseFor(Predicate{Field: "kind", Op: "term", Value: "decision"})
	if err != nil || !declared {
		t.Fatalf("episodic did not claim its own field %q: declared=%v err=%v", "kind", declared, err)
	}
	want := map[string]any{"term": map[string]any{"kind": "decision"}}
	if !reflect.DeepEqual(clause, want) {
		t.Errorf("clause = %v, want %v", clause, want)
	}
}

// TestFilterClausesOrderPutsPredicatesLast: predicates append AFTER the ACL,
// tenancy and validity clauses — the ordering DW-4.3's golden bytes depend on
// (a predicate must never displace an existing clause's position).
func TestFilterClausesOrderPutsPredicatesLast(t *testing.T) {
	tier := &tierRetriever{source: "semantic", supportsValidity: true, filterable: semanticFilterable}
	aclClause := map[string]any{"term": map[string]any{"scope": "org"}}
	clauses, err := tier.filterClauses(Filter{
		TenantID:   "t1",
		UserID:     "a1",
		ValidOnly:  true,
		Predicates: []Predicate{{Field: "subject", Op: "term", Value: "orders-svc"}},
	}, aclClause)
	if err != nil {
		t.Fatalf("filterClauses: %v", err)
	}
	if len(clauses) != 5 {
		t.Fatalf("got %d clauses, want 5 (acl, tenant, user, validity, predicate)", len(clauses))
	}
	if !reflect.DeepEqual(clauses[0], any(aclClause)) {
		t.Errorf("clause[0] = %v, want the ACL clause first (it is the scope barricade)", clauses[0])
	}
	want := map[string]any{"term": map[string]any{"subject": "orders-svc"}}
	if !reflect.DeepEqual(clauses[4], any(want)) {
		t.Errorf("clause[4] = %v, want the predicate last", clauses[4])
	}
}

// TestFilterClausesFailsClosedOnUnvalidatedPredicate: Search validates every
// predicate first, so this path should be unreachable — but if a tier is ever
// reached with an unvalidated predicate it must error, never emit an unchecked
// clause (defense in depth).
func TestFilterClausesFailsClosedOnUnvalidatedPredicate(t *testing.T) {
	tier := &tierRetriever{source: "episodic", filterable: episodicFilterable}
	_, err := tier.filterClauses(Filter{
		Predicates: []Predicate{{Field: "kind", Op: "term", Value: map[string]any{"match_all": map[string]any{}}}},
	}, nil)
	if err == nil {
		t.Fatal("filterClauses compiled a non-scalar value into a clause; it must fail closed")
	}
}

// TestResolveSourcesVocabulary pins the Sources namespace: built-in tiers,
// registered tier sources and registered post-hooks resolve from one list.
func TestResolveSourcesVocabulary(t *testing.T) {
	m := &MultiRetriever{
		tiers: []*tierRetriever{
			{source: "episodic", filterable: episodicFilterable},
			{source: "semantic", filterable: semanticFilterable},
		},
	}
	m.RegisterTier("experience", &stubTier{})
	m.RegisterPostHook("graph", &stubHook{})

	want := []string{"episodic", "experience", "graph", "semantic"}
	if got := m.sourceNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("sourceNames() = %v, want %v", got, want)
	}

	sel, err := m.resolveSources(nil)
	if err != nil || sel != nil {
		t.Errorf("resolveSources(nil) = (%v, %v), want (nil, nil) — nil means every source", sel, err)
	}
	if !sel.selected("anything") {
		t.Error("a nil sourceSet must select every source")
	}

	sel, err = m.resolveSources([]string{"graph"})
	if err != nil {
		t.Fatalf("resolveSources([graph]): %v", err)
	}
	if !sel.selected("graph") || sel.selected("episodic") {
		t.Errorf("Sources:[graph] selected the wrong set: %v", sel)
	}
}

// TestFilterableFieldNamesFollowsSelection: the "valid fields" an error offers
// depend on which sources are selected — offering a field from an unselected
// tier would send the caller in a circle.
func TestFilterableFieldNamesFollowsSelection(t *testing.T) {
	m := &MultiRetriever{tiers: []*tierRetriever{
		{source: "episodic", filterable: episodicFilterable},
		{source: "semantic", filterable: semanticFilterable},
	}}
	sel, err := m.resolveSources([]string{"episodic"})
	if err != nil {
		t.Fatalf("resolveSources: %v", err)
	}
	got := m.filterableFieldNames(sel)
	if !slices.Contains(got, "kind") {
		t.Errorf("episodic-only selection does not offer %q: %v", "kind", got)
	}
	if slices.Contains(got, "subject") {
		t.Errorf("episodic-only selection offers the semantic-only field %q: %v", "subject", got)
	}
	// created_at is declared by both tiers: it must appear exactly once.
	all := m.filterableFieldNames(nil)
	count := 0
	for _, f := range all {
		if f == "created_at" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("created_at appears %d times in the union of both tiers' fields, want 1: %v", count, all)
	}
}
