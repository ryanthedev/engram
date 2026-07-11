package knowledge

import (
	"context"
	"errors"
	"fmt"

	yaml "go.yaml.in/yaml/v2"
)

// seedFile is the YAML boot-seed shape:
//
//	collections:
//	  - name: arxiv
//	    text_field: abstract
//	    public: true
//	    roles: []
//	    fields:
//	      categories: {type: keyword, filterable: true}
//	      published:  {type: date, filterable: true, sortable: true}
type seedFile struct {
	Collections []seedCollection `yaml:"collections"`
}

type seedCollection struct {
	Name      string               `yaml:"name"`
	TextField string               `yaml:"text_field"`
	Public    bool                 `yaml:"public"`
	Roles     []string             `yaml:"roles"`
	Fields    map[string]seedField `yaml:"fields"`
}

type seedField struct {
	Type       string `yaml:"type"`
	Filterable bool   `yaml:"filterable"`
	Sortable   bool   `yaml:"sortable"`
}

// spec converts one seed entry to its domain spec (Index is registry-assigned).
func (c seedCollection) spec() CollectionSpec {
	spec := CollectionSpec{
		Name:      c.Name,
		TextField: c.TextField,
		Access:    AccessPolicy{Public: c.Public, Roles: c.Roles},
	}
	if len(c.Fields) > 0 {
		spec.Mappings = make(map[string]FieldSpec, len(c.Fields))
		for name, f := range c.Fields {
			spec.Mappings[name] = FieldSpec{Type: f.Type, Filterable: f.Filterable, Sortable: f.Sortable}
		}
	}
	return spec
}

// Seed idempotently applies a YAML boot seed to reg: each listed collection
// is created if absent and left untouched if present — even when the stored
// spec differs, because the seed is a bootstrap default, not a reconciler
// (runtime Update is the way to change a live collection). Re-running the
// same seed therefore makes no changes and returns no created names. A
// concurrent seeder winning the create race also reads as a no-op.
func Seed(ctx context.Context, reg CollectionRegistry, yamlSrc []byte) (created []string, err error) {
	var f seedFile
	if err := yaml.UnmarshalStrict(yamlSrc, &f); err != nil {
		return nil, fmt.Errorf("knowledge: parsing seed YAML: %w", err)
	}
	for _, c := range f.Collections {
		switch _, err := reg.Get(ctx, c.Name); {
		case err == nil:
			continue // exists: idempotent no-op, not a conflict
		case !errors.Is(err, ErrNotFound):
			return created, fmt.Errorf("knowledge: seeding collection %s: %w", c.Name, err)
		}
		switch err := reg.Create(ctx, c.spec()); {
		case err == nil:
			created = append(created, c.Name)
		case errors.Is(err, ErrConflict):
			// A concurrent seeder won the create race; the collection exists,
			// which is all idempotency promises.
		default:
			return created, fmt.Errorf("knowledge: seeding collection %s: %w", c.Name, err)
		}
	}
	return created, nil
}
