package sources

import (
	"testing"

	"google.golang.org/protobuf/types/known/structpb"
)

// TestToKnowledgeDocFieldsStructpbEncodable guards the regression the live e2e
// caught: the Fields map is wire-encoded via structpb.NewStruct in
// engramclient.KnowledgeIngest, which rejects typed slices ([]string). Every
// value toKnowledgeDoc emits — including the categories list — must be
// structpb-compatible.
func TestToKnowledgeDocFieldsStructpbEncodable(t *testing.T) {
	rec := ArXivRecord{
		ID:            "2401.00001",
		Title:         "A  Title   With   Spaces",
		Abstract:      "an abstract",
		Categories:    []string{"cs.CL", "cs.AI"},
		PublishedDate: "2024-01-01",
		UpdateDate:    "2024-01-15",
		DOI:           "10.1000/xyz",
		JournalRef:    "NeurIPS 2024",
		Comments:      "accepted",
		Authors:       "Jane Doe",
		SourceVersion: "dump:2024-01-01",
	}
	doc := toKnowledgeDoc(rec)

	if _, err := structpb.NewStruct(doc.Fields); err != nil {
		t.Fatalf("Fields are not structpb-encodable (this breaks KnowledgeIngest): %v", err)
	}

	cats, ok := doc.Fields["categories"].([]any)
	if !ok {
		t.Fatalf("categories must be []any for structpb, got %T", doc.Fields["categories"])
	}
	if len(cats) != 2 {
		t.Fatalf("expected 2 categories, got %d", len(cats))
	}
	if doc.Title != "A Title With Spaces" {
		t.Errorf("title whitespace not normalized: %q", doc.Title)
	}
}
