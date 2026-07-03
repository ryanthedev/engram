package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeCluster simulates the small OpenSearch surface Apply touches: the root
// version endpoint, template/pipeline PUTs, and index HEAD/PUT.
type fakeCluster struct {
	mu        sync.Mutex
	version   string
	templates map[string][]byte
	pipelines map[string][]byte
	indices   map[string]bool
	puts      []string
}

func newFakeCluster(version string) *fakeCluster {
	return &fakeCluster{
		version:   version,
		templates: map[string][]byte{},
		pipelines: map[string][]byte{},
		indices:   map[string]bool{},
	}
}

func (f *fakeCluster) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/")
		switch {
		case r.URL.Path == "/" && r.Method == http.MethodGet:
			fmt.Fprintf(w, `{"version":{"number":%q}}`, f.version)
		case strings.HasPrefix(path, "_index_template/") && r.Method == http.MethodPut:
			name := strings.TrimPrefix(path, "_index_template/")
			f.templates[name] = nil
			f.puts = append(f.puts, "template:"+name)
			fmt.Fprint(w, `{"acknowledged":true}`)
		case strings.HasPrefix(path, "_search/pipeline/") && r.Method == http.MethodPut:
			name := strings.TrimPrefix(path, "_search/pipeline/")
			f.pipelines[name] = nil
			f.puts = append(f.puts, "pipeline:"+name)
			fmt.Fprint(w, `{"acknowledged":true}`)
		case r.Method == http.MethodHead:
			if f.indices[path] {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case r.Method == http.MethodPut:
			if f.indices[path] {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":{"type":"resource_already_exists_exception"}}`)
				return
			}
			f.indices[path] = true
			f.puts = append(f.puts, "index:"+path)
			fmt.Fprint(w, `{"acknowledged":true}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
}

// TestDW_0_4_ApplyIsIdempotentAgainstFakeCluster verifies the apply tool's
// contract on a simulated 3.1 cluster: first run creates the indices, the
// re-run changes nothing (Changed() == false) and never re-PUTs an index.
func TestDW_0_4_ApplyIsIdempotentAgainstFakeCluster(t *testing.T) {
	fake := newFakeCluster("3.1.0")
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	res1, err := Apply(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if !res1.Changed() {
		t.Error("first apply should create indices (Changed() == true)")
	}
	for _, name := range []string{EpisodicIndex, SemanticIndex} {
		if res1.Actions[name] != "created" {
			t.Errorf("first apply: %s action = %q, want created", name, res1.Actions[name])
		}
	}
	if _, ok := fake.templates[EpisodicTemplateName]; !ok {
		t.Error("episodic template not applied")
	}
	if _, ok := fake.templates[SemanticTemplateName]; !ok {
		t.Error("semantic template not applied")
	}
	if _, ok := fake.pipelines[RRFPipelineName]; !ok {
		t.Error("RRF pipeline not applied")
	}

	putsAfterFirst := len(fake.puts)
	res2, err := Apply(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("second apply must be a no-op, got error: %v", err)
	}
	if res2.Changed() {
		t.Error("second apply should be a no-op (Changed() == false)")
	}
	for _, name := range []string{EpisodicIndex, SemanticIndex} {
		if res2.Actions[name] != "unchanged" {
			t.Errorf("second apply: %s action = %q, want unchanged", name, res2.Actions[name])
		}
	}
	// Idempotent definition upserts are allowed; index re-creates are not.
	for _, p := range fake.puts[putsAfterFirst:] {
		if strings.HasPrefix(p, "index:") {
			t.Errorf("second apply re-created %s", p)
		}
	}
}

// TestApplyRejectsWrongClusterVersion covers the D14 edge case: any cluster
// off the pinned 3.1 line is refused with a clear message — one code path,
// no fallback.
func TestApplyRejectsWrongClusterVersion(t *testing.T) {
	for _, version := range []string{"2.19.1", "3.0.0", "3.2.0"} {
		t.Run(version, func(t *testing.T) {
			fake := newFakeCluster(version)
			srv := httptest.NewServer(fake.handler())
			defer srv.Close()

			_, err := Apply(context.Background(), srv.Client(), srv.URL)
			if err == nil {
				t.Fatalf("apply accepted cluster version %s; pinned to %sx", version, PinnedVersionPrefix)
			}
			msg := err.Error()
			if !strings.Contains(msg, version) || !strings.Contains(msg, "3.1.") {
				t.Errorf("version error should name both versions, got: %v", err)
			}
			if len(fake.puts) != 0 {
				t.Errorf("apply touched a wrong-version cluster: %v", fake.puts)
			}
		})
	}
}

// TestApplyToleratesConcurrentCreateRace pins the fix for a race the live
// spikes exposed: HEAD-then-PUT is not atomic, so a concurrent Apply can
// create the index between the exists check and the create. The losing PUT's
// resource_already_exists_exception must read as "unchanged", not an error.
func TestApplyToleratesConcurrentCreateRace(t *testing.T) {
	fake := newFakeCluster("3.1.0")
	// Pre-create the indices but keep HEAD answering 404, simulating the
	// winner landing after our exists check.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPut && !strings.Contains(r.URL.Path, "_index_template") && !strings.Contains(r.URL.Path, "_search/pipeline") {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"type":"resource_already_exists_exception"}}`)
			return
		}
		fake.handler().ServeHTTP(w, r)
	}))
	defer srv.Close()

	res, err := Apply(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("apply must tolerate the concurrent-create race, got: %v", err)
	}
	for _, idx := range []string{EpisodicIndex, SemanticIndex} {
		if res.Actions[idx] != "unchanged" {
			t.Errorf("raced index %s action = %q, want unchanged", idx, res.Actions[idx])
		}
	}
}

// TestApplyReportsUnreachableCluster verifies a connection failure surfaces
// as a clear error rather than a partial apply.
func TestApplyReportsUnreachableCluster(t *testing.T) {
	_, err := Apply(context.Background(), http.DefaultClient, "http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("want unreachable-cluster error, got: %v", err)
	}
}

// TestTemplateJSONRoundTrips guards the embedded artifacts against syntax
// rot: every embedded byte blob must be valid JSON.
func TestTemplateJSONRoundTrips(t *testing.T) {
	for name, blob := range map[string][]byte{
		"episodic":     EpisodicTemplateJSON,
		"semantic":     SemanticTemplateJSON,
		"rrf-pipeline": RRFPipelineJSON,
	} {
		var v any
		if err := json.Unmarshal(blob, &v); err != nil {
			t.Errorf("%s template is not valid JSON: %v", name, err)
		}
	}
}
