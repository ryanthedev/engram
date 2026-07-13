package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// --- DW-3.3: refuses to run without -confirm -----------------------------

func TestValidate_RefusesWithoutConfirm(t *testing.T) {
	cfg := config{tenant: "t1", confirm: false}
	if err := cfg.validate(); err == nil {
		t.Fatal("validate() with confirm=false = nil error, want an error")
	}
}

func TestValidate_RequiresTenant(t *testing.T) {
	cfg := config{tenant: "", confirm: true}
	if err := cfg.validate(); err == nil {
		t.Fatal("validate() with an empty tenant = nil error, want an error")
	}
}

func TestValidate_AcceptsConfirmedNonEmptyTenant(t *testing.T) {
	cfg := config{tenant: "t1", confirm: true}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() with confirm=true and a tenant = %v, want nil", err)
	}
}

// TestRun_RefusesWithoutConfirmAndTouchesNoNetwork proves DW-3.3 holds at
// the run() level, not just validate() in isolation: a canary server that
// fails the test on ANY request stays untouched, so the refusal happens
// strictly before any HTTP client is even used.
func TestRun_RefusesWithoutConfirmAndTouchesNoNetwork(t *testing.T) {
	var touched bool
	canary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		touched = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(canary.Close)

	cfg := config{url: canary.URL, tenant: "t1", confirm: false}
	err := run(context.Background(), cfg, &bytes.Buffer{}, discardLogger())
	if err == nil {
		t.Fatal("run() with confirm=false = nil error, want an error")
	}
	if touched {
		t.Fatal("run() with confirm=false issued a network request before refusing")
	}
}

// --- DW-3.4: never writes to episodic or semantic ------------------------

// episodicOrSemanticGuardServer is a hermetic fake OpenSearch cluster that
// answers exactly the requests a correct rebuild run should make (graph
// template/index PUT+DELETE, graph entity/edge doc writes, graph entity
// candidate search, and the semantic index's READ-only _search) and
// records a violation for anything else — in particular, ANY request
// whose path touches the episodic index, and any non-_search (i.e.
// write-shaped) request touching the semantic index. This is the
// strongest test available short of a live cluster: it proves the
// forbidden requests are never SENT, not merely that this command has no
// convenient way to send them.
type episodicOrSemanticGuardServer struct {
	mu         sync.Mutex
	violations []string
}

func (g *episodicOrSemanticGuardServer) recordViolation(v string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.violations = append(g.violations, v)
}

func (g *episodicOrSemanticGuardServer) Violations() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.violations...)
}

func (g *episodicOrSemanticGuardServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "engram-episodic"):
			g.recordViolation(r.Method + " " + path + " (episodic index touched)")
			w.WriteHeader(http.StatusOK)
			return
		case strings.Contains(path, "engram-semantic") && !(r.Method == http.MethodPost && strings.HasSuffix(path, "/_search")):
			g.recordViolation(r.Method + " " + path + " (non-read semantic-index request)")
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == http.MethodPost && strings.HasSuffix(path, "engram-semantic-000001/_search"):
			g.writeSemanticSearch(w)
			return
		case r.Method == http.MethodDelete && (path == "/engram-graph-entities-000001" || path == "/engram-graph-edges-000001"):
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == http.MethodPut && strings.HasPrefix(path, "/_index_template/engram-graph-"):
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == http.MethodPut && (path == "/engram-graph-entities-000001" || path == "/engram-graph-edges-000001"):
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == http.MethodPut && strings.Contains(path, "/engram-graph-entities-000001/_doc/"):
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == http.MethodPut && strings.Contains(path, "/engram-graph-edges-000001/_doc/"):
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == http.MethodGet && strings.Contains(path, "/engram-graph-edges-000001/_doc/"):
			w.WriteHeader(http.StatusNotFound) // GetEdge: no existing edge yet
			_ = json.NewEncoder(w).Encode(map[string]any{"found": false})
			return
		case r.Method == http.MethodPost && strings.HasSuffix(path, "engram-graph-entities-000001/_search"):
			_ = json.NewEncoder(w).Encode(map[string]any{"hits": map[string]any{"hits": []any{}}}) // CandidateEntities: no existing entities
			return
		default:
			g.recordViolation(r.Method + " " + path + " (unexpected request)")
			w.WriteHeader(http.StatusOK)
			return
		}
	}
}

func (g *episodicOrSemanticGuardServer) writeSemanticSearch(w http.ResponseWriter) {
	f := memory.SemanticFact{
		Subject: "service-a", Predicate: "owns", Object: "billing-db", Statement: "service-a owns billing-db",
		TenantID: "t1", ContentKey: "ck-1", SourceIDs: []string{"ev-1"},
		ValidAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}
	raw, _ := json.Marshal(f)
	var src map[string]any
	_ = json.Unmarshal(raw, &src)
	hit := map[string]any{"_id": "fact-1", "_source": src, "_seq_no": float64(1), "_primary_term": float64(1)}
	_ = json.NewEncoder(w).Encode(map[string]any{"hits": map[string]any{"hits": []any{hit}}})
}

func TestRun_NeverWritesEpisodicOrSemantic(t *testing.T) {
	guard := &episodicOrSemanticGuardServer{}
	srv := httptest.NewServer(guard.handler())
	t.Cleanup(srv.Close)

	cfg := config{url: srv.URL, tenant: "t1", confirm: true}
	var out bytes.Buffer
	if err := run(context.Background(), cfg, &out, discardLogger()); err != nil {
		t.Fatalf("run(): %v", err)
	}
	if v := guard.Violations(); len(v) != 0 {
		t.Fatalf("run() made %d disallowed request(s):\n%s", len(v), strings.Join(v, "\n"))
	}
	if !strings.Contains(out.String(), "1 live facts replayed") {
		t.Fatalf("run() output = %q, want it to report 1 fact replayed", out.String())
	}
}
