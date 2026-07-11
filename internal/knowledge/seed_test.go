package knowledge_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/knowledge"
)

// fakeRegistry is an in-memory knowledge.CollectionRegistry recording calls.
type fakeRegistry struct {
	specs       map[string]knowledge.CollectionSpec
	createCalls int
	createErr   error // forced Create result when non-nil
	getErr      error // forced Get error when non-nil
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{specs: map[string]knowledge.CollectionSpec{}}
}

func (f *fakeRegistry) Get(_ context.Context, name string) (knowledge.CollectionSpec, error) {
	if f.getErr != nil {
		return knowledge.CollectionSpec{}, f.getErr
	}
	spec, ok := f.specs[name]
	if !ok {
		return knowledge.CollectionSpec{}, fmt.Errorf("fake: %w", knowledge.ErrNotFound)
	}
	return spec, nil
}

func (f *fakeRegistry) Create(_ context.Context, spec knowledge.CollectionSpec) error {
	f.createCalls++
	if f.createErr != nil {
		return f.createErr
	}
	if _, ok := f.specs[spec.Name]; ok {
		return fmt.Errorf("fake: %w", knowledge.ErrConflict)
	}
	f.specs[spec.Name] = spec
	return nil
}

func (f *fakeRegistry) Update(context.Context, knowledge.CollectionSpec) error { return nil }

func (f *fakeRegistry) List(context.Context) ([]knowledge.CollectionSummary, error) {
	return nil, nil
}

func (f *fakeRegistry) Provision(context.Context, string) error { return nil }

const seedYAML = `
collections:
  - name: arxiv
    text_field: abstract
    public: true
    fields:
      categories: {type: keyword, filterable: true}
      published:  {type: date, filterable: true, sortable: true}
  - name: runbooks
    roles: [ops]
`

// TestDW_3_4_SeedTwiceIsIdempotent pins the boot-seed contract: the first run
// creates the listed collections, the second run makes NO changes — zero new
// Create calls, zero created names.
func TestDW_3_4_SeedTwiceIsIdempotent(t *testing.T) {
	reg := newFakeRegistry()
	ctx := context.Background()

	created, err := knowledge.Seed(ctx, reg, []byte(seedYAML))
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if len(created) != 2 {
		t.Errorf("first seed created %v, want [arxiv runbooks]", created)
	}
	callsAfterFirst := reg.createCalls

	created, err = knowledge.Seed(ctx, reg, []byte(seedYAML))
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if len(created) != 0 {
		t.Errorf("second seed created %v, want none", created)
	}
	if reg.createCalls != callsAfterFirst {
		t.Errorf("second seed issued %d Create calls, want 0", reg.createCalls-callsAfterFirst)
	}
}

// TestSeedExistingCollectionIsNoOpEvenWhenSpecDiffers pins the seed-vs-
// reconciler boundary: a collection that already exists is skipped, never
// updated and never a conflict — even when the stored spec differs.
func TestSeedExistingCollectionIsNoOpEvenWhenSpecDiffers(t *testing.T) {
	reg := newFakeRegistry()
	reg.specs["arxiv"] = knowledge.CollectionSpec{Name: "arxiv", TextField: "different"}

	created, err := knowledge.Seed(context.Background(), reg, []byte(seedYAML))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(created) != 1 || created[0] != "runbooks" {
		t.Errorf("created = %v, want [runbooks]", created)
	}
	if got := reg.specs["arxiv"].TextField; got != "different" {
		t.Errorf("seed rewrote the existing spec (text field %q)", got)
	}
}

// TestSeedCreateRaceConflictIsNoOp pins the concurrent-seeder edge: Get says
// absent, but a racing seeder wins the Create — the ErrConflict reads as an
// idempotent no-op, not a failure.
func TestSeedCreateRaceConflictIsNoOp(t *testing.T) {
	reg := newFakeRegistry()
	reg.createErr = fmt.Errorf("racing: %w", knowledge.ErrConflict)

	created, err := knowledge.Seed(context.Background(), reg, []byte(seedYAML))
	if err != nil {
		t.Fatalf("seed must tolerate a lost create race, got: %v", err)
	}
	if len(created) != 0 {
		t.Errorf("created = %v, want none (both creates lost the race)", created)
	}
}

// TestSeedFieldsReachCreate verifies the YAML→spec translation: text field,
// access policy, and per-field type/filterable/sortable arrive on Create.
func TestSeedFieldsReachCreate(t *testing.T) {
	reg := newFakeRegistry()
	if _, err := knowledge.Seed(context.Background(), reg, []byte(seedYAML)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	arxiv := reg.specs["arxiv"]
	if arxiv.TextField != "abstract" || !arxiv.Access.Public {
		t.Errorf("arxiv spec = %+v, want text_field=abstract public=true", arxiv)
	}
	if got := arxiv.Mappings["published"]; got != (knowledge.FieldSpec{Type: "date", Filterable: true, Sortable: true}) {
		t.Errorf("published field = %+v, want date/filterable/sortable", got)
	}
	if got := arxiv.Mappings["categories"]; got != (knowledge.FieldSpec{Type: "keyword", Filterable: true}) {
		t.Errorf("categories field = %+v, want keyword/filterable", got)
	}
	runbooks := reg.specs["runbooks"]
	if runbooks.Access.Public || len(runbooks.Access.Roles) != 1 || runbooks.Access.Roles[0] != "ops" {
		t.Errorf("runbooks access = %+v, want roles=[ops] public=false", runbooks.Access)
	}
}

// TestSeedRejectsMalformedYAML: parse failures (bad syntax, unknown keys) are
// loud errors before any registry call.
func TestSeedRejectsMalformedYAML(t *testing.T) {
	for name, src := range map[string]string{
		"syntax":      "collections: [::",
		"unknown key": "collections:\n  - name: x\n    bogus: true",
	} {
		t.Run(name, func(t *testing.T) {
			reg := newFakeRegistry()
			if _, err := knowledge.Seed(context.Background(), reg, []byte(src)); err == nil {
				t.Fatal("want parse error, got nil")
			}
			if reg.createCalls != 0 {
				t.Errorf("malformed seed reached the registry (%d Create calls)", reg.createCalls)
			}
		})
	}
}

// TestSeedPropagatesRegistryErrors: a real registry failure (not not-found,
// not conflict) aborts the seed with the collection named.
func TestSeedPropagatesRegistryErrors(t *testing.T) {
	reg := newFakeRegistry()
	reg.getErr = errors.New("cluster unreachable")
	_, err := knowledge.Seed(context.Background(), reg, []byte(seedYAML))
	if err == nil || !strings.Contains(err.Error(), "arxiv") {
		t.Fatalf("want error naming the collection, got: %v", err)
	}
}
