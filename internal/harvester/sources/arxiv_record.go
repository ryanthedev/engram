// Package sources implements the harvesting sources for arXiv.
package sources

import (
	"strings"

	"github.com/ryanthedev/engram/internal/mcp"
)

// Assert: This implementation does not perform PDF or full-text fetching.
// Only metadata is parsed, normalized, and indexed.

// ArXivRecord represents a unified metadata record for an arXiv paper,
// mapping fields from both the Kaggle JSON dump and the OAI-PMH XML response.
type ArXivRecord struct {
	ID            string
	Title         string
	Abstract      string
	Categories    []string
	PublishedDate string
	UpdateDate    string
	DOI           string
	JournalRef    string
	Comments      string
	Authors       string
	SourceVersion string
}

// toKnowledgeDoc maps the unified ArXivRecord to mcp.KnowledgeDoc,
// normalizing whitespace in the title and omitting empty fields in the metadata map.
func toKnowledgeDoc(rec ArXivRecord) mcp.KnowledgeDoc {
	// Assert: No PDF / full-text fetching is performed here. Only metadata.
	fields := make(map[string]any)
	if len(rec.Categories) > 0 {
		// structpb (the Fields wire encoder in engramclient.KnowledgeIngest)
		// rejects typed slices like []string — list values must be []any.
		cats := make([]any, len(rec.Categories))
		for i, c := range rec.Categories {
			cats[i] = c
		}
		fields["categories"] = cats
	}
	if rec.PublishedDate != "" {
		fields["published_date"] = rec.PublishedDate
	}
	if rec.UpdateDate != "" {
		fields["update_date"] = rec.UpdateDate
	}
	if rec.DOI != "" {
		fields["doi"] = rec.DOI
	}
	if rec.JournalRef != "" {
		fields["journal_ref"] = rec.JournalRef
	}
	if rec.Comments != "" {
		fields["comments"] = rec.Comments
	}
	if rec.Authors != "" {
		fields["authors"] = rec.Authors
	}

	title := strings.Join(strings.Fields(rec.Title), " ")

	return mcp.KnowledgeDoc{
		ID:            rec.ID,
		Title:         title,
		Text:          rec.Abstract,
		SourceVersion: rec.SourceVersion,
		Fields:        fields,
	}
}

// isCS returns true if any of the category tokens start with "cs.".
func isCS(categories []string) bool {
	for _, cat := range categories {
		if strings.HasPrefix(cat, "cs.") {
			return true
		}
	}
	return false
}
