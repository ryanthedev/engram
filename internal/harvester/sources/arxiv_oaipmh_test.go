package sources_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ryanthedev/engram/internal/harvester"
	"github.com/ryanthedev/engram/internal/harvester/sources"
)

// Assert: No PDF/full-text fetching is executed in OAI-PMH source tests.

func TestDW_3_2_OaipmhHarvestSuccess(t *testing.T) {
	var firstReqChecked bool
	var secondReqChecked bool

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verb := r.URL.Query().Get("verb")
		if verb != "ListRecords" {
			t.Errorf("expected verb ListRecords, got %q", verb)
		}

		token := r.URL.Query().Get("resumptionToken")
		if token == "" {
			firstReqChecked = true
			if r.URL.Query().Get("set") != "cs" {
				t.Errorf("expected set=cs on first page, got %q", r.URL.Query().Get("set"))
			}
			if r.URL.Query().Get("metadataPrefix") != "arXiv" {
				t.Errorf("expected metadataPrefix=arXiv, got %q", r.URL.Query().Get("metadataPrefix"))
			}
			if r.URL.Query().Get("from") == "" {
				t.Error("expected from date parameter, got empty")
			}

			w.Header().Set("Content-Type", "text/xml")
			w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<OAI-PMH xmlns="http://www.openarchives.org/OAI/2.0/">
  <responseDate>2026-07-11T18:00:00Z</responseDate>
  <ListRecords>
    <record>
      <header>
        <identifier>oai:arXiv.org:0704.0001</identifier>
        <datestamp>2007-05-23</datestamp>
      </header>
      <metadata>
        <arXiv xmlns="http://arxiv.org/OAI/arXiv/">
          <id>0704.0001</id>
          <title>Page One Paper
  with spaces</title>
          <categories>cs.CL hep-ph</categories>
          <abstract>First abstract.</abstract>
          <created>2007-04-02</created>
          <updated>2007-05-23</updated>
          <authors>
            <author>
              <keyname>Last1</keyname>
              <forenames>First1</forenames>
            </author>
          </authors>
        </arXiv>
      </metadata>
    </record>
    <resumptionToken>token-for-page-2</resumptionToken>
  </ListRecords>
</OAI-PMH>`))
		} else {
			secondReqChecked = true
			if token != "token-for-page-2" {
				t.Errorf("expected resumptionToken 'token-for-page-2', got %q", token)
			}
			// OAI-PMH spec: set, metadataPrefix, from MUST NOT be present with resumptionToken
			if r.URL.Query().Get("set") != "" || r.URL.Query().Get("metadataPrefix") != "" || r.URL.Query().Get("from") != "" {
				t.Error("subsequent resumption requests must not include set, metadataPrefix, or from parameters")
			}

			w.Header().Set("Content-Type", "text/xml")
			w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<OAI-PMH xmlns="http://www.openarchives.org/OAI/2.0/">
  <responseDate>2026-07-11T18:00:00Z</responseDate>
  <ListRecords>
    <record>
      <header>
        <identifier>oai:arXiv.org:0704.0002</identifier>
        <datestamp>2007-05-24</datestamp>
      </header>
      <metadata>
        <arXiv xmlns="http://arxiv.org/OAI/arXiv/">
          <id>0704.0002</id>
          <title>Page Two Paper</title>
          <categories>cs.CV</categories>
          <abstract>Second abstract.</abstract>
          <created>2007-04-03</created>
          <updated>2007-05-24</updated>
          <authors>
            <author>
              <keyname>Last2</keyname>
              <forenames>First2</forenames>
              <suffix>Jr.</suffix>
            </author>
          </authors>
        </arXiv>
      </metadata>
    </record>
    <resumptionToken></resumptionToken>
  </ListRecords>
</OAI-PMH>`))
		}
	}))
	defer ts.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := harvester.Deps{Logger: logger}

	cfg := harvester.SourceConfig{
		Type: "arxiv-oaipmh",
		Raw: map[string]any{
			"base_url":        ts.URL,
			"set":             "cs",
			"metadata_prefix": "arXiv",
			"lookback":        "24h",
		},
	}

	src, err := harvester.Build(cfg, deps)
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}

	if src.Type() != "arxiv-oaipmh" {
		t.Errorf("expected Type() 'arxiv-oaipmh', got %q", src.Type())
	}
	if src.Mode() != harvester.Incremental {
		t.Errorf("expected Mode() Incremental, got %v", src.Mode())
	}

	sink := &fakeSink{}
	if err := src.Harvest(context.Background(), sink); err != nil {
		t.Fatalf("Harvest failed: %v", err)
	}

	if !firstReqChecked || !secondReqChecked {
		t.Errorf("did not make expected page requests: first=%v, second=%v", firstReqChecked, secondReqChecked)
	}

	if len(sink.docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(sink.docs))
	}

	// Verify Page 1 doc mapping
	doc1 := sink.docs[0]
	if doc1.ID != "0704.0001" {
		t.Errorf("expected ID '0704.0001', got %q", doc1.ID)
	}
	if doc1.Title != "Page One Paper with spaces" {
		t.Errorf("expected whitespace-normalized title, got %q", doc1.Title)
	}
	if doc1.Text != "First abstract." {
		t.Errorf("expected text 'First abstract.', got %q", doc1.Text)
	}
	if doc1.SourceVersion != "oai:2007-05-23" {
		t.Errorf("expected SourceVersion 'oai:2007-05-23', got %q", doc1.SourceVersion)
	}
	if doc1.Fields["authors"] != "First1 Last1" {
		t.Errorf("expected authors 'First1 Last1', got %v", doc1.Fields["authors"])
	}

	// Verify Page 2 doc mapping
	doc2 := sink.docs[1]
	if doc2.ID != "0704.0002" {
		t.Errorf("expected ID '0704.0002', got %q", doc2.ID)
	}
	if doc2.Fields["authors"] != "First2 Last2, Jr." {
		t.Errorf("expected authors 'First2 Last2, Jr.', got %v", doc2.Fields["authors"])
	}
}

func TestDW_3_3_OaipmhDeletedSkipped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<OAI-PMH xmlns="http://www.openarchives.org/OAI/2.0/">
  <ListRecords>
    <record>
      <header status="deleted">
        <identifier>oai:arXiv.org:0704.0003</identifier>
        <datestamp>2007-05-25</datestamp>
      </header>
    </record>
  </ListRecords>
</OAI-PMH>`))
	}))
	defer ts.Close()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	deps := harvester.Deps{Logger: logger}

	cfg := harvester.SourceConfig{
		Type: "arxiv-oaipmh",
		Raw: map[string]any{
			"base_url": ts.URL,
		},
	}

	src, err := harvester.Build(cfg, deps)
	if err != nil {
		t.Fatalf("failed to build: %v", err)
	}

	sink := &fakeSink{}
	if err := src.Harvest(context.Background(), sink); err != nil {
		t.Fatalf("Harvest failed: %v", err)
	}

	if len(sink.docs) != 0 {
		t.Errorf("expected 0 docs since the sole record was deleted, got %d", len(sink.docs))
	}

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "arxiv-oaipmh: skipping deleted record") {
		t.Errorf("expected log message about skipping deleted record, got: %q", logOutput)
	}
	if !strings.Contains(logOutput, "oai:arXiv.org:0704.0003") {
		t.Errorf("expected log message to contain deleted id, got: %q", logOutput)
	}
}

func TestDW_3_4_OaipmhXxeAndRepeatedToken(t *testing.T) {
	t.Run("XXE entity resolution is disabled and ignored", func(t *testing.T) {
		var xxeServerHit int32
		xxeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&xxeServerHit, 1)
			w.WriteHeader(http.StatusOK)
		}))
		defer xxeServer.Close()

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/xml")
			// Inject external entity pointing to the local test server
			payload := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE OAI-PMH [
  <!ENTITY xxe SYSTEM "%s">
]>
<OAI-PMH xmlns="http://www.openarchives.org/OAI/2.0/">
  <ListRecords>
    <record>
      <header>
        <identifier>oai:arXiv.org:0704.0001</identifier>
        <datestamp>2007-05-23</datestamp>
      </header>
      <metadata>
        <arXiv xmlns="http://arxiv.org/OAI/arXiv/">
          <id>0704.0001</id>
          <title>&xxe;</title>
          <categories>cs.CL</categories>
        </arXiv>
      </metadata>
    </record>
  </ListRecords>
</OAI-PMH>`, xxeServer.URL)
			w.Write([]byte(payload))
		}))
		defer ts.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		deps := harvester.Deps{Logger: logger}

		cfg := harvester.SourceConfig{
			Type: "arxiv-oaipmh",
			Raw: map[string]any{
				"base_url": ts.URL,
			},
		}

		src, err := harvester.Build(cfg, deps)
		if err != nil {
			t.Fatalf("failed to build: %v", err)
		}

		sink := &fakeSink{}
		// Harvest parses XML. The Go decoder should not resolve the external entity.
		_ = src.Harvest(context.Background(), sink)

		if atomic.LoadInt32(&xxeServerHit) > 0 {
			t.Error("XXE Vulnerability: XML entity expansion performed an HTTP request to the external server!")
		}
	})

	t.Run("repeated resumption token returns error", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/xml")
			w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<OAI-PMH xmlns="http://www.openarchives.org/OAI/2.0/">
  <ListRecords>
    <record>
      <header>
        <identifier>oai:arXiv.org:0704.0001</identifier>
      </header>
      <metadata>
        <arXiv xmlns="http://arxiv.org/OAI/arXiv/">
          <id>0704.0001</id>
          <categories>cs.CL</categories>
        </arXiv>
      </metadata>
    </record>
    <resumptionToken>infinite-loop-token</resumptionToken>
  </ListRecords>
</OAI-PMH>`))
		}))
		defer ts.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		deps := harvester.Deps{Logger: logger}

		cfg := harvester.SourceConfig{
			Type: "arxiv-oaipmh",
			Raw: map[string]any{
				"base_url": ts.URL,
			},
		}

		src, err := harvester.Build(cfg, deps)
		if err != nil {
			t.Fatalf("failed to build: %v", err)
		}

		sink := &fakeSink{}
		err = src.Harvest(context.Background(), sink)
		if err == nil {
			t.Fatal("expected Harvest to return an error due to infinite resumption loop, but it succeeded")
		}
		if !strings.Contains(err.Error(), "infinite loop detected") {
			t.Errorf("expected error message to mention infinite loop, got: %v", err)
		}
	})
}

func TestDW_3_6_OaipmhPolitenessAndFailures(t *testing.T) {
	t.Run("Retry-After politeness triggers retries then succeeds", func(t *testing.T) {
		var reqAttempts int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts := atomic.AddInt32(&reqAttempts, 1)
			if attempts == 1 {
				w.Header().Set("Retry-After", "0") // 0 seconds so tests run immediately
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "text/xml")
			w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<OAI-PMH xmlns="http://www.openarchives.org/OAI/2.0/">
  <ListRecords>
    <record>
      <header>
        <identifier>oai:arXiv.org:0704.0001</identifier>
      </header>
      <metadata>
        <arXiv xmlns="http://arxiv.org/OAI/arXiv/">
          <id>0704.0001</id>
          <categories>cs.CL</categories>
        </arXiv>
      </metadata>
    </record>
  </ListRecords>
</OAI-PMH>`))
		}))
		defer ts.Close()

		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, nil))
		deps := harvester.Deps{Logger: logger}

		cfg := harvester.SourceConfig{
			Type: "arxiv-oaipmh",
			Raw: map[string]any{
				"base_url": ts.URL,
			},
		}

		src, err := harvester.Build(cfg, deps)
		if err != nil {
			t.Fatalf("failed to build: %v", err)
		}

		sink := &fakeSink{}
		if err := src.Harvest(context.Background(), sink); err != nil {
			t.Fatalf("expected Harvest to succeed on retry, got error: %v", err)
		}

		if atomic.LoadInt32(&reqAttempts) != 2 {
			t.Errorf("expected 2 attempts, got %d", reqAttempts)
		}

		logOutput := logBuf.String()
		if !strings.Contains(logOutput, "received 503, backing off") {
			t.Errorf("expected log warning about backing off, log output: %q", logOutput)
		}
	})

	t.Run("persistent OAI-PMH error response surfaces error", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/xml")
			w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<OAI-PMH xmlns="http://www.openarchives.org/OAI/2.0/">
  <error code="badResumptionToken">The resumption token has expired or is invalid</error>
</OAI-PMH>`))
		}))
		defer ts.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		deps := harvester.Deps{Logger: logger}

		cfg := harvester.SourceConfig{
			Type: "arxiv-oaipmh",
			Raw: map[string]any{
				"base_url": ts.URL,
			},
		}

		src, err := harvester.Build(cfg, deps)
		if err != nil {
			t.Fatalf("failed to build: %v", err)
		}

		sink := &fakeSink{}
		err = src.Harvest(context.Background(), sink)
		if err == nil {
			t.Fatal("expected Harvest to fail due to OAI error, but it succeeded")
		}
		if !strings.Contains(err.Error(), "badResumptionToken") {
			t.Errorf("expected error message to contain 'badResumptionToken', got: %v", err)
		}
	})
}

func TestOaipmhOversizedResponseBody(t *testing.T) {
	restore := sources.ExportedSetMaxResponseBytes(100)
	defer restore()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<OAI-PMH xmlns="http://www.openarchives.org/OAI/2.0/">
  <ListRecords>
    <record>
      <header>
        <identifier>oai:arXiv.org:0704.0001</identifier>
      </header>
      <metadata>
        <arXiv xmlns="http://arxiv.org/OAI/arXiv/">
          <id>0704.0001</id>
          <categories>cs.CL</categories>
        </arXiv>
      </metadata>
    </record>
  </ListRecords>
</OAI-PMH>`))
	}))
	defer ts.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := harvester.Deps{Logger: logger}

	cfg := harvester.SourceConfig{
		Type: "arxiv-oaipmh",
		Raw: map[string]any{
			"base_url": ts.URL,
		},
	}

	src, err := harvester.Build(cfg, deps)
	if err != nil {
		t.Fatalf("failed to build: %v", err)
	}

	sink := &fakeSink{}
	err = src.Harvest(context.Background(), sink)
	if err == nil {
		t.Fatal("expected Harvest to fail due to oversized response body, but it succeeded")
	}
	if !strings.Contains(err.Error(), "response body size exceeded limit") {
		t.Errorf("expected error message to mention size limit, got: %v", err)
	}
}

func TestOaipmhOverLengthResumptionToken(t *testing.T) {
	restore := sources.ExportedSetMaxTokenLen(10)
	defer restore()

	var reqCount int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqCount, 1)
		w.Header().Set("Content-Type", "text/xml")
		w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<OAI-PMH xmlns="http://www.openarchives.org/OAI/2.0/">
  <ListRecords>
    <record>
      <header>
        <identifier>oai:arXiv.org:0704.0001</identifier>
      </header>
      <metadata>
        <arXiv xmlns="http://arxiv.org/OAI/arXiv/">
          <id>0704.0001</id>
          <categories>cs.CL</categories>
        </arXiv>
      </metadata>
    </record>
    <resumptionToken>too-long-resumption-token</resumptionToken>
  </ListRecords>
</OAI-PMH>`))
	}))
	defer ts.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := harvester.Deps{Logger: logger}

	cfg := harvester.SourceConfig{
		Type: "arxiv-oaipmh",
		Raw: map[string]any{
			"base_url": ts.URL,
		},
	}

	src, err := harvester.Build(cfg, deps)
	if err != nil {
		t.Fatalf("failed to build: %v", err)
	}

	sink := &fakeSink{}
	err = src.Harvest(context.Background(), sink)
	if err == nil {
		t.Fatal("expected Harvest to fail due to over-length resumption token, but it succeeded")
	}
	if !strings.Contains(err.Error(), "resumption token length") {
		t.Errorf("expected error message to mention resumption token length, got: %v", err)
	}
	if atomic.LoadInt32(&reqCount) != 1 {
		t.Errorf("expected exactly 1 request (aborted before second request), got %d", reqCount)
	}
}
