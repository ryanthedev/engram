package sources

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/ryanthedev/engram/internal/harvester"
)

// Assert: This implementation does not perform PDF or full-text fetching.
// It parses local metadata dumps only.

type kaggleSource struct {
	path     string
	filter   string
	dumpDate string
	deps     harvester.Deps
}

type kaggleRecord struct {
	ID            string          `json:"id"`
	Title         string          `json:"title"`
	Abstract      string          `json:"abstract"`
	Categories    string          `json:"categories"`
	DOI           string          `json:"doi"`
	JournalRef    string          `json:"journal-ref"`
	Comments      string          `json:"comments"`
	UpdateDate    string          `json:"update_date"`
	Versions      []kaggleVersion `json:"versions"`
	AuthorsParsed [][]string      `json:"authors_parsed"`
}

type kaggleVersion struct {
	Version string `json:"version"`
	Created string `json:"created"`
}

func init() {
	harvester.Register("arxiv-kaggle", func(cfg harvester.SourceConfig, deps harvester.Deps) (harvester.Source, error) {
		pathVal, ok := cfg.Raw["path"]
		if !ok {
			return nil, fmt.Errorf("harvester: arxiv-kaggle: missing required config 'path'")
		}
		path, ok := pathVal.(string)
		if !ok || path == "" {
			return nil, fmt.Errorf("harvester: arxiv-kaggle: 'path' must be a non-empty string")
		}

		filter := "cs.*"
		if fVal, ok := cfg.Raw["filter"]; ok {
			if fStr, ok := fVal.(string); ok {
				filter = fStr
			}
		}

		var dumpDate string
		if dVal, ok := cfg.Raw["dump_date"]; ok {
			if dStr, ok := dVal.(string); ok {
				dumpDate = dStr
			}
		}

		return &kaggleSource{
			path:     path,
			filter:   filter,
			dumpDate: dumpDate,
			deps:     deps,
		}, nil
	})
}

// Type returns the source type name.
func (s *kaggleSource) Type() string {
	return "arxiv-kaggle"
}

// Mode returns FullHarvest since Kaggle dumps represent a complete dataset.
func (s *kaggleSource) Mode() harvester.HarvestMode {
	return harvester.FullHarvest
}

// Harvest streams the gzipped JSON line-by-line to process metadata records.
func (s *kaggleSource) Harvest(ctx context.Context, sink harvester.Sink) error {
	// Assert: No PDF/full-text fetching is executed during harvesting.
	f, err := os.Open(s.path)
	if err != nil {
		return fmt.Errorf("harvester: arxiv-kaggle: opening file %s: %w", s.path, err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("harvester: arxiv-kaggle: stat file %s: %w", s.path, err)
	}

	var sourceVersion string
	if s.dumpDate != "" {
		if strings.HasPrefix(s.dumpDate, "dump:") {
			sourceVersion = s.dumpDate
		} else {
			sourceVersion = "dump:" + s.dumpDate
		}
	} else {
		sourceVersion = "dump:" + stat.ModTime().Format("2006-01-02")
	}

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("harvester: arxiv-kaggle: initializing gzip reader: %w", err)
	}
	defer gz.Close()

	// Use scanner with increased buffer size to handle very long metadata lines.
	scanner := bufio.NewScanner(gz)
	const maxLineLength = 10 * 1024 * 1024 // 10MB
	scanner.Buffer(make([]byte, 64*1024), maxLineLength)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("harvester: arxiv-kaggle: cancelled: %w", err)
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var rec kaggleRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			s.deps.Logger.WarnContext(ctx, "harvester: arxiv-kaggle: skipping malformed JSON line",
				slog.String("error", err.Error()),
				slog.String("line", string(line)),
			)
			continue
		}

		categories := strings.Fields(rec.Categories)
		if !isCS(categories) {
			continue
		}

		// Parse authors list
		var authorStrs []string
		for _, p := range rec.AuthorsParsed {
			if len(p) >= 2 {
				last := p[0]
				first := p[1]
				suffix := ""
				if len(p) > 2 {
					suffix = p[2]
				}
				name := first
				if name != "" && last != "" {
					name += " " + last
				} else if last != "" {
					name = last
				}
				if suffix != "" && name != "" {
					name += ", " + suffix
				}
				if name != "" {
					authorStrs = append(authorStrs, name)
				}
			}
		}
		authorsJoined := strings.Join(authorStrs, ", ")

		var publishedDate string
		for _, v := range rec.Versions {
			if v.Version == "v1" {
				publishedDate = v.Created
				break
			}
		}
		if publishedDate == "" && len(rec.Versions) > 0 {
			publishedDate = rec.Versions[0].Created
		}

		sharedRec := ArXivRecord{
			ID:            rec.ID,
			Title:         rec.Title,
			Abstract:      rec.Abstract,
			Categories:    categories,
			PublishedDate: publishedDate,
			UpdateDate:    rec.UpdateDate,
			DOI:           rec.DOI,
			JournalRef:    rec.JournalRef,
			Comments:      rec.Comments,
			Authors:       authorsJoined,
			SourceVersion: sourceVersion,
		}

		doc := toKnowledgeDoc(sharedRec)
		if err := sink.Add(doc); err != nil {
			return fmt.Errorf("harvester: arxiv-kaggle: adding doc %s to sink: %w", rec.ID, err)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("harvester: arxiv-kaggle: reading dump stream: %w", err)
	}

	return nil
}
