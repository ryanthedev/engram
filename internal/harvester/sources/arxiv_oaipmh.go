package sources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ryanthedev/engram/internal/harvester"
)

var (
	maxResponseBytes int64 = 64 << 20 // 64 MiB per page
	maxTokenLen            = 4096
)

// Assert: This implementation does not perform PDF or full-text fetching.
// Only metadata is requested and parsed via the OAI-PMH endpoint.

type oaipmhSource struct {
	baseURL        string
	set            string
	metadataPrefix string
	lookback       string
	deps           harvester.Deps
}

type oaiPmhResponse struct {
	XMLName     xml.Name     `xml:"OAI-PMH"`
	Error       *oaiError    `xml:"error"`
	ListRecords *listRecords `xml:"ListRecords"`
}

type oaiError struct {
	Code string `xml:"code,attr"`
	Msg  string `xml:",chardata"`
}

type listRecords struct {
	Records         []oaiRecord      `xml:"record"`
	ResumptionToken *resumptionToken `xml:"resumptionToken"`
}

type oaiRecord struct {
	Header   oaiHeader   `xml:"header"`
	Metadata oaiMetadata `xml:"metadata"`
}

type oaiHeader struct {
	Identifier string `xml:"identifier"`
	Datestamp  string `xml:"datestamp"`
	Status     string `xml:"status,attr"` // e.g. "deleted"
}

type oaiMetadata struct {
	ArXiv arXivXMLRecord `xml:"arXiv"`
}

type arXivXMLRecord struct {
	ID         string     `xml:"id"`
	Created    string     `xml:"created"`
	Updated    string     `xml:"updated"`
	Authors    xmlAuthors `xml:"authors"`
	Title      string     `xml:"title"`
	Categories string     `xml:"categories"`
	Abstract   string     `xml:"abstract"`
	DOI        string     `xml:"doi"`
	JournalRef string     `xml:"journal-ref"`
	Comments   string     `xml:"comments"`
}

type xmlAuthors struct {
	Author []xmlAuthor `xml:"author"`
}

type xmlAuthor struct {
	Keyname   string `xml:"keyname"`
	Forenames string `xml:"forenames"`
	Suffix    string `xml:"suffix"`
}

type resumptionToken struct {
	Token string `xml:",chardata"`
}

func init() {
	harvester.Register("arxiv-oaipmh", func(cfg harvester.SourceConfig, deps harvester.Deps) (harvester.Source, error) {
		baseURL := "https://oaipmh.arxiv.org/oai"
		if bVal, ok := cfg.Raw["base_url"]; ok {
			if bStr, ok := bVal.(string); ok {
				baseURL = bStr
			}
		}

		parsed, err := url.Parse(baseURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, fmt.Errorf("harvester: arxiv-oaipmh: invalid base_url %q: scheme must be http or https", baseURL)
		}

		set := "cs"
		if sVal, ok := cfg.Raw["set"]; ok {
			if sStr, ok := sVal.(string); ok {
				set = sStr
			}
		}

		metadataPrefix := "arXiv"
		if mVal, ok := cfg.Raw["metadata_prefix"]; ok {
			if mStr, ok := mVal.(string); ok {
				metadataPrefix = mStr
			}
		}

		lookback := "48h"
		if lVal, ok := cfg.Raw["lookback"]; ok {
			if lStr, ok := lVal.(string); ok {
				lookback = lStr
			}
		}

		// Validate lookback duration up front to fail fast
		if _, err := time.ParseDuration(lookback); err != nil {
			return nil, fmt.Errorf("harvester: arxiv-oaipmh: invalid lookback duration %q: %w", lookback, err)
		}

		return &oaipmhSource{
			baseURL:        baseURL,
			set:            set,
			metadataPrefix: metadataPrefix,
			lookback:       lookback,
			deps:           deps,
		}, nil
	})
}

// Type returns the source type name.
func (s *oaipmhSource) Type() string {
	return "arxiv-oaipmh"
}

// Mode returns Incremental since OAI-PMH supports date-based incremental changes.
func (s *oaipmhSource) Mode() harvester.HarvestMode {
	return harvester.Incremental
}

// Harvest fetches records from OAI-PMH, follows resumption tokens, and streams docs to the sink.
func (s *oaipmhSource) Harvest(ctx context.Context, sink harvester.Sink) error {
	// Assert: No PDF/full-text fetching is executed during harvesting.
	lookbackDuration, err := time.ParseDuration(s.lookback)
	if err != nil {
		return fmt.Errorf("harvester: arxiv-oaipmh: parsing lookback: %w", err)
	}

	fromDate := time.Now().Add(-lookbackDuration).Format("2006-01-02")

	client := &http.Client{
		Timeout: 60 * time.Second,
	}

	var resumptionToken string
	const maxIterations = 100000
	seenTokens := make(map[string]bool)

	for iter := 0; iter < maxIterations; iter++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("harvester: arxiv-oaipmh: cancelled: %w", err)
		}

		var reqURL string
		if iter == 0 {
			u, err := url.Parse(s.baseURL)
			if err != nil {
				return fmt.Errorf("harvester: arxiv-oaipmh: parsing base_url: %w", err)
			}
			q := u.Query()
			q.Set("verb", "ListRecords")
			q.Set("set", s.set)
			q.Set("metadataPrefix", s.metadataPrefix)
			q.Set("from", fromDate)
			u.RawQuery = q.Encode()
			reqURL = u.String()
		} else {
			u, err := url.Parse(s.baseURL)
			if err != nil {
				return fmt.Errorf("harvester: arxiv-oaipmh: parsing base_url: %w", err)
			}
			q := u.Query()
			q.Set("verb", "ListRecords")
			q.Set("resumptionToken", resumptionToken)
			u.RawQuery = q.Encode()
			reqURL = u.String()
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return fmt.Errorf("harvester: arxiv-oaipmh: creating request: %w", err)
		}

		resp, err := s.doRequestWithRetry(ctx, client, req)
		if err != nil {
			return err
		}

		var oaiResp oaiPmhResponse
		// Use standard xml.NewDecoder directly (does NOT resolve external DTDs/entities, XXE-safe).
		// Wrap the body in a hard byte-limited reader to prevent unbounded allocation.
		limited := &io.LimitedReader{R: resp.Body, N: maxResponseBytes + 1}
		decoder := xml.NewDecoder(limited)
		err = decoder.Decode(&oaiResp)
		resp.Body.Close()
		if limited.N <= 0 {
			return fmt.Errorf("harvester: arxiv-oaipmh: response body size exceeded limit of %d bytes", maxResponseBytes)
		}
		if err != nil {
			return fmt.Errorf("harvester: arxiv-oaipmh: decoding xml: %w", err)
		}

		if oaiResp.Error != nil {
			if oaiResp.Error.Code == "noRecordsMatch" {
				s.deps.Logger.InfoContext(ctx, "arxiv-oaipmh: no records match the criteria")
				return nil
			}
			return fmt.Errorf("harvester: arxiv-oaipmh: OAI-PMH error code %q: %s", oaiResp.Error.Code, oaiResp.Error.Msg)
		}

		if oaiResp.ListRecords == nil {
			break
		}

		for _, rec := range oaiResp.ListRecords.Records {
			if rec.Header.Status == "deleted" {
				s.deps.Logger.InfoContext(ctx, "arxiv-oaipmh: skipping deleted record",
					slog.String("id", rec.Header.Identifier),
				)
				continue
			}

			categories := strings.Fields(rec.Metadata.ArXiv.Categories)
			if !isCS(categories) {
				continue
			}

			// Format authors list for parity
			var authorStrs []string
			for _, a := range rec.Metadata.ArXiv.Authors.Author {
				name := a.Forenames
				if name != "" && a.Keyname != "" {
					name += " " + a.Keyname
				} else if a.Keyname != "" {
					name = a.Keyname
				}
				if a.Suffix != "" && name != "" {
					name += ", " + a.Suffix
				}
				if name != "" {
					authorStrs = append(authorStrs, name)
				}
			}
			authorsJoined := strings.Join(authorStrs, ", ")

			sharedRec := ArXivRecord{
				ID:            rec.Metadata.ArXiv.ID,
				Title:         rec.Metadata.ArXiv.Title,
				Abstract:      rec.Metadata.ArXiv.Abstract,
				Categories:    categories,
				PublishedDate: rec.Metadata.ArXiv.Created,
				UpdateDate:    rec.Metadata.ArXiv.Updated,
				DOI:           rec.Metadata.ArXiv.DOI,
				JournalRef:    rec.Metadata.ArXiv.JournalRef,
				Comments:      rec.Metadata.ArXiv.Comments,
				Authors:       authorsJoined,
				SourceVersion: "oai:" + rec.Header.Datestamp,
			}

			doc := toKnowledgeDoc(sharedRec)
			if err := sink.Add(doc); err != nil {
				return fmt.Errorf("harvester: arxiv-oaipmh: adding doc %s to sink: %w", rec.Metadata.ArXiv.ID, err)
			}
		}

		if oaiResp.ListRecords.ResumptionToken == nil {
			break
		}

		newToken := strings.TrimSpace(oaiResp.ListRecords.ResumptionToken.Token)
		if newToken == "" {
			break
		}

		if len(newToken) > maxTokenLen {
			return fmt.Errorf("harvester: arxiv-oaipmh: resumption token length %d exceeds max limit of %d", len(newToken), maxTokenLen)
		}

		hash := sha256.Sum256([]byte(newToken))
		tokenHash := hex.EncodeToString(hash[:])
		if seenTokens[tokenHash] {
			return fmt.Errorf("harvester: arxiv-oaipmh: infinite loop detected: repeated resumptionToken %q", truncateToken(newToken))
		}
		seenTokens[tokenHash] = true
		resumptionToken = newToken

		if iter == maxIterations-1 {
			return fmt.Errorf("harvester: arxiv-oaipmh: exceeded max resumption iterations limit (%d)", maxIterations)
		}
	}

	return nil
}

func (s *oaipmhSource) doRequestWithRetry(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	const maxRetries = 3
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("harvester: arxiv-oaipmh: cancelled: %w", err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("harvester: arxiv-oaipmh: http request failed: %w", err)
		}

		if resp.StatusCode == http.StatusServiceUnavailable {
			resp.Body.Close()
			if attempt == maxRetries {
				return nil, fmt.Errorf("harvester: arxiv-oaipmh: server returned 503 Service Unavailable, max retries reached")
			}

			retryAfterStr := resp.Header.Get("Retry-After")
			delay := 5 * time.Second // default fallback
			if secs, err := strconv.Atoi(retryAfterStr); err == nil && secs >= 0 {
				delay = time.Duration(secs) * time.Second
			} else if date, err := http.ParseTime(retryAfterStr); err == nil {
				delay = time.Until(date)
			}

			// Bound backoff duration to prevent hanging
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
			if delay < 0 {
				delay = 0
			}

			s.deps.Logger.WarnContext(ctx, "harvester: arxiv-oaipmh: received 503, backing off",
				slog.String("retry_after", retryAfterStr),
				slog.Duration("delay", delay),
				slog.Int("attempt", attempt+1),
			)

			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("harvester: arxiv-oaipmh: cancelled during backoff: %w", ctx.Err())
			case <-time.After(delay):
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("harvester: arxiv-oaipmh: unexpected status code: %d", resp.StatusCode)
		}

		return resp, nil
	}

	return nil, fmt.Errorf("harvester: arxiv-oaipmh: max retries reached")
}

func truncateToken(token string) string {
	runes := []rune(token)
	if len(runes) > 64 {
		return string(runes[:64]) + "…"
	}
	return token
}
