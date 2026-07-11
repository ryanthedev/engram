package sources

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/harvester"
	"github.com/ryanthedev/engram/internal/mcp"
)

type testSink struct {
	docs []mcp.KnowledgeDoc
}

func (s *testSink) Add(doc mcp.KnowledgeDoc) error {
	s.docs = append(s.docs, doc)
	return nil
}

func (s *testSink) Flush(ctx context.Context) error {
	return nil
}

func TestDW_5_1_CrawlFakeSite(t *testing.T) {
	// Enable loopback crawling for this test
	orig := allowLoopbackCrawl
	allowLoopbackCrawl = true
	defer func() { allowLoopbackCrawl = orig }()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(""))
		case "/index.html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><head><title>Index Page</title></head><body>Welcome to the index page. <a href="/page1.html">Page 1</a> <a href="/page2.html">Page 2</a></body></html>`))
		case "/page1.html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><head><title>Page One</title></head><body>This is page 1 text.</body></html>`))
		case "/page2.html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><head><title>Page Two</title></head><body>This is page 2 text.</body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := harvester.Deps{Logger: logger}

	cfg := harvester.SourceConfig{
		Type: "web-crawl",
		Raw: map[string]any{
			"seeds":     []any{ts.URL + "/index.html"},
			"max_pages": 100,
			"delay":     "0s",
		},
	}

	src, err := harvester.Build(cfg, deps)
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}

	sink := &testSink{}
	if err := src.Harvest(context.Background(), sink); err != nil {
		t.Fatalf("Harvest failed: %v", err)
	}

	if len(sink.docs) != 3 {
		t.Fatalf("expected 3 docs, got %d", len(sink.docs))
	}

	// Verify docs contents
	expectedDocs := map[string]struct {
		title string
		text  string
	}{
		ts.URL + "/index.html": {
			title: "Index Page",
			text:  "Welcome to the index page. Page 1 Page 2",
		},
		ts.URL + "/page1.html": {
			title: "Page One",
			text:  "This is page 1 text.",
		},
		ts.URL + "/page2.html": {
			title: "Page Two",
			text:  "This is page 2 text.",
		},
	}

	for _, doc := range sink.docs {
		exp, ok := expectedDocs[doc.ID]
		if !ok {
			t.Errorf("unexpected doc ID: %q", doc.ID)
			continue
		}
		if doc.Title != exp.title {
			t.Errorf("for doc %s: expected title %q, got %q", doc.ID, exp.title, doc.Title)
		}
		if doc.Text != exp.text {
			t.Errorf("for doc %s: expected text %q, got %q", doc.ID, exp.text, doc.Text)
		}
		if !strings.HasPrefix(doc.SourceVersion, "crawl:") {
			t.Errorf("for doc %s: expected SourceVersion prefix 'crawl:', got %q", doc.ID, doc.SourceVersion)
		}
		if doc.Fields["url"] != doc.ID {
			t.Errorf("for doc %s: expected Fields['url'] %q, got %v", doc.ID, doc.ID, doc.Fields["url"])
		}
	}
}

func TestDW_5_2_SecurityBlockedIPs(t *testing.T) {
	// Directly test the isBlockedIP function for private/loopback/link-local/unique-local/etc.
	tests := []struct {
		ip       string
		blocked  bool
		loopback bool // allowLoopbackCrawl value
	}{
		// Default: allowLoopbackCrawl = false
		{"127.0.0.1", true, false},
		{"127.0.0.2", true, false},
		{"127.255.255.255", true, false},
		{"10.0.0.1", true, false},
		{"172.16.0.1", true, false},
		{"172.31.255.255", true, false},
		{"192.168.1.1", true, false},
		{"169.254.169.254", true, false},
		{"::1", true, false},
		{"fc00::1", true, false},
		{"fdff::1", true, false},
		{"fe80::1", true, false},
		{"0.0.0.0", true, false},
		{"::", true, false},
		{"224.0.0.1", true, false},
		{"ff02::1", true, false},
		{"8.8.8.8", false, false},
		{"1.1.1.1", false, false},
		{"2001:4860:4860::8888", false, false},

		// allowLoopbackCrawl = true
		{"127.0.0.1", false, true},
		{"127.0.0.2", false, true},
		{"::1", false, true},
		{"10.0.0.1", true, true},    // still blocked
		{"192.168.1.1", true, true}, // still blocked
		{"8.8.8.8", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			orig := allowLoopbackCrawl
			allowLoopbackCrawl = tt.loopback
			defer func() { allowLoopbackCrawl = orig }()

			ipObj := net.ParseIP(tt.ip)
			if ipObj == nil {
				t.Fatalf("failed to parse IP %q", tt.ip)
			}

			got := isBlockedIP(ipObj)
			if got != tt.blocked {
				t.Errorf("isBlockedIP(%s) with allowLoopbackCrawl=%v = %v, want %v", tt.ip, tt.loopback, got, tt.blocked)
			}
		})
	}
}

func TestDW_5_2_SecurityCrawlLoopbackBlocked(t *testing.T) {
	// With allowLoopbackCrawl FALSE, crawling 127.0.0.1 must be refused
	orig := allowLoopbackCrawl
	allowLoopbackCrawl = false
	defer func() { allowLoopbackCrawl = orig }()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := harvester.Deps{Logger: logger}

	cfg := harvester.SourceConfig{
		Type: "web-crawl",
		Raw: map[string]any{
			"seeds":     []any{"http://127.0.0.1:9999/index.html"},
			"max_pages": 100,
			"delay":     "0s",
		},
	}

	src, err := harvester.Build(cfg, deps)
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}

	sink := &testSink{}
	err = src.Harvest(context.Background(), sink)
	if err == nil {
		t.Error("expected error when crawling loopback IP with allowLoopbackCrawl=false, got nil")
	} else if !strings.Contains(err.Error(), "blocked") && !strings.Contains(err.Error(), "connection refused") && !strings.Contains(err.Error(), "access to IP") {
		t.Errorf("expected error to mention 'blocked' or similar, got %v", err)
	}
}

func TestDW_5_2_SecurityOffHostBlocked(t *testing.T) {
	// Enable loopback crawling for this test
	orig := allowLoopbackCrawl
	allowLoopbackCrawl = true
	defer func() { allowLoopbackCrawl = orig }()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(""))
		case "/index.html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><body>Welcome. <a href="http://example.com/external.html">External Link</a></body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := harvester.Deps{Logger: logger}

	cfg := harvester.SourceConfig{
		Type: "web-crawl",
		Raw: map[string]any{
			"seeds":     []any{ts.URL + "/index.html"},
			"max_pages": 100,
			"delay":     "0s",
		},
	}

	src, err := harvester.Build(cfg, deps)
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}

	sink := &testSink{}
	if err := src.Harvest(context.Background(), sink); err != nil {
		t.Fatalf("Harvest failed: %v", err)
	}

	if len(sink.docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(sink.docs))
	}
	if sink.docs[0].ID != ts.URL+"/index.html" {
		t.Errorf("expected doc ID %s, got %s", ts.URL+"/index.html", sink.docs[0].ID)
	}
}

func TestDW_5_3_CrawlCycleAndMaxPages(t *testing.T) {
	orig := allowLoopbackCrawl
	allowLoopbackCrawl = true
	defer func() { allowLoopbackCrawl = orig }()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(""))
		case "/a.html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><body>Page A. <a href="/b.html">Go to B</a></body></html>`))
		case "/b.html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><body>Page B. <a href="/a.html">Go to A</a></body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := harvester.Deps{Logger: logger}

	// Test case 1: Cyclic link graph terminates naturally
	cfg1 := harvester.SourceConfig{
		Type: "web-crawl",
		Raw: map[string]any{
			"seeds":     []any{ts.URL + "/a.html"},
			"max_pages": 100,
			"delay":     "0s",
		},
	}

	src1, err := harvester.Build(cfg1, deps)
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}

	sink1 := &testSink{}
	if err := src1.Harvest(context.Background(), sink1); err != nil {
		t.Fatalf("Harvest failed: %v", err)
	}

	if len(sink1.docs) != 2 {
		t.Fatalf("expected 2 docs in cyclic crawl, got %d", len(sink1.docs))
	}

	// Test case 2: max_pages cap smaller than site stops early
	cfg2 := harvester.SourceConfig{
		Type: "web-crawl",
		Raw: map[string]any{
			"seeds":     []any{ts.URL + "/a.html"},
			"max_pages": 1,
			"delay":     "0s",
		},
	}

	src2, err := harvester.Build(cfg2, deps)
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}

	sink2 := &testSink{}
	if err := src2.Harvest(context.Background(), sink2); err != nil {
		t.Fatalf("Harvest failed: %v", err)
	}

	if len(sink2.docs) != 1 {
		t.Fatalf("expected 1 doc due to max_pages=1 cap, got %d", len(sink2.docs))
	}
}

func TestDW_5_4_RobotsTxtAndContentType(t *testing.T) {
	orig := allowLoopbackCrawl
	allowLoopbackCrawl = true
	defer func() { allowLoopbackCrawl = orig }()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(`
User-agent: *
Disallow: /disallowed/
`))
		case "/index.html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><body>
Welcome.
<a href="/allowed/page.html">Allowed</a>
<a href="/disallowed/page.html">Disallowed</a>
<a href="/image.png">Image</a>
</body></html>`))
		case "/allowed/page.html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><body>Allowed Page</body></html>`))
		case "/disallowed/page.html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><body>Disallowed Page</body></html>`))
		case "/image.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR..."))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := harvester.Deps{Logger: logger}

	cfg := harvester.SourceConfig{
		Type: "web-crawl",
		Raw: map[string]any{
			"seeds":     []any{ts.URL + "/index.html"},
			"max_pages": 100,
			"delay":     "0s",
		},
	}

	src, err := harvester.Build(cfg, deps)
	if err != nil {
		t.Fatalf("failed to build: %v", err)
	}

	sink := &testSink{}
	if err := src.Harvest(context.Background(), sink); err != nil {
		t.Fatalf("Harvest failed: %v", err)
	}

	// Should harvest "/index.html" and "/allowed/page.html".
	// "/disallowed/page.html" is blocked by robots.txt.
	// "/image.png" is skipped due to non-text/html Content-Type.
	if len(sink.docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(sink.docs))
	}

	hasAllowed := false
	for _, doc := range sink.docs {
		if strings.Contains(doc.ID, "/allowed/page.html") {
			hasAllowed = true
		}
		if strings.Contains(doc.ID, "/disallowed/page.html") {
			t.Error("found disallowed page in harvested docs")
		}
		if strings.Contains(doc.ID, "/image.png") {
			t.Error("found non-text/html page in harvested docs")
		}
	}

	if !hasAllowed {
		t.Error("allowed page was not harvested")
	}
}

func TestEdgeTruncateAndUnreachableSeed(t *testing.T) {
	orig := allowLoopbackCrawl
	allowLoopbackCrawl = true
	defer func() { allowLoopbackCrawl = orig }()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(""))
		case "/index.html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><body>VeryLongPageContent</body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	deps := harvester.Deps{Logger: logger}

	// 1. Truncation test
	cfg1 := harvester.SourceConfig{
		Type: "web-crawl",
		Raw: map[string]any{
			"seeds":          []any{ts.URL + "/index.html"},
			"max_pages":      100,
			"max_page_bytes": 10, // low limit to force truncation
			"delay":          "0s",
		},
	}

	src1, err := harvester.Build(cfg1, deps)
	if err != nil {
		t.Fatalf("failed to build: %v", err)
	}

	sink1 := &testSink{}
	if err := src1.Harvest(context.Background(), sink1); err != nil {
		t.Fatalf("Harvest failed: %v", err)
	}

	if len(sink1.docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(sink1.docs))
	}

	// Verify text is truncated to 10 bytes or similar
	doc := sink1.docs[0]
	if len(doc.Text) > 10 {
		t.Errorf("expected doc.Text length <= 10, got %d (content: %q)", len(doc.Text), doc.Text)
	}

	// Verify we logged a warning about truncation
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "truncated") {
		t.Errorf("expected log output to mention 'truncated', got: %q", logOutput)
	}

	// 2. Unreachable seed test
	// Use a closed/unbound local port to trigger a connection failure
	cfg2 := harvester.SourceConfig{
		Type: "web-crawl",
		Raw: map[string]any{
			"seeds": []any{"http://127.0.0.1:54321/index.html"}, // unlikely to be bound
			"delay": "0s",
		},
	}

	src2, err := harvester.Build(cfg2, deps)
	if err != nil {
		t.Fatalf("failed to build: %v", err)
	}

	sink2 := &testSink{}
	err = src2.Harvest(context.Background(), sink2)
	if err == nil {
		t.Error("expected error for unreachable seed URL, got nil")
	}
}

func TestCrawlFrontierCap(t *testing.T) {
	orig := allowLoopbackCrawl
	allowLoopbackCrawl = true
	defer func() { allowLoopbackCrawl = orig }()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(""))
		case "/index.html":
			w.Header().Set("Content-Type", "text/html")
			var buf bytes.Buffer
			buf.WriteString("<html><body>")
			for i := 1; i <= 50; i++ {
				fmt.Fprintf(&buf, "<a href=\"/page%d.html\">link %d</a> ", i, i)
			}
			buf.WriteString("</body></html>")
			w.Write(buf.Bytes())
		default:
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html><body>Other page</body></html>"))
		}
	}))
	defer ts.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := harvester.Deps{Logger: logger}

	cfg := harvester.SourceConfig{
		Type: "web-crawl",
		Raw: map[string]any{
			"seeds":        []any{ts.URL + "/index.html"},
			"max_pages":    10,
			"max_frontier": 5,
			"delay":        "0s",
		},
	}

	src, err := harvester.Build(cfg, deps)
	if err != nil {
		t.Fatalf("failed to build: %v", err)
	}

	sink := &testSink{}
	if err := src.Harvest(context.Background(), sink); err != nil {
		t.Fatalf("Harvest failed: %v", err)
	}

	// With max_frontier = 5, the crawler should stop discovering new URLs after 5 URLs have been enqueued.
	// Since /index.html is the 1st URL, it discovers only 4 more unique URLs and then stops.
	// Thus, even though max_pages is 10, the total crawled/harvested documents should be 5.
	if len(sink.docs) != 5 {
		t.Errorf("expected exactly 5 harvested docs, got %d", len(sink.docs))
	}
}

func TestFactoryValidation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := harvester.Deps{Logger: logger}

	tests := []struct {
		name    string
		config  map[string]any
		wantErr string
	}{
		{
			name: "max_page_bytes is zero",
			config: map[string]any{
				"seeds":          []any{"http://example.com"},
				"max_page_bytes": 0,
			},
			wantErr: "max_page_bytes",
		},
		{
			name: "max_page_bytes is negative",
			config: map[string]any{
				"seeds":          []any{"http://example.com"},
				"max_page_bytes": -1,
			},
			wantErr: "max_page_bytes",
		},
		{
			name: "max_pages is zero",
			config: map[string]any{
				"seeds":     []any{"http://example.com"},
				"max_pages": 0,
			},
			wantErr: "max_pages",
		},
		{
			name: "max_pages is negative",
			config: map[string]any{
				"seeds":     []any{"http://example.com"},
				"max_pages": -5,
			},
			wantErr: "max_pages",
		},
		{
			name: "delay is negative",
			config: map[string]any{
				"seeds": []any{"http://example.com"},
				"delay": "-1s",
			},
			wantErr: "delay",
		},
		{
			name: "max_frontier is zero",
			config: map[string]any{
				"seeds":        []any{"http://example.com"},
				"max_frontier": 0,
			},
			wantErr: "max_frontier",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := harvester.SourceConfig{
				Type: "web-crawl",
				Raw:  tt.config,
			}
			_, err := harvester.Build(cfg, deps)
			if err == nil {
				t.Fatalf("expected error for config %v, got nil", tt.config)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error message to contain %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}
