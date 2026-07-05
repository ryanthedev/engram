//go:build drill

// Package cloud holds the Phase-7 operational drills (DW-7.4, DW-7.5): real
// exercises against the local pinned-3.1 OpenSearch cluster and (for the
// failure drill) a real worker process, standing in for staging per the
// dispatch's explicit local-stand-in guidance. These are NOT part of
// `go test ./...` or `make integration` — they mutate/restart the shared dev
// cluster and spawn/kill real OS processes, so they run only via the
// dedicated `make drill` target, deliberately isolated from CI's normal
// gates.
package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/testutil"
)

// TestRestoreDrill is DW-7.4: register a local fs snapshot repository on the
// real pinned-3.1 cluster, snapshot a scratch semantic index carrying known
// fixture facts, delete the index (simulating loss), restore it under a NEW
// name (docs/runbooks/05-restore-from-snapshot.md's "never restore in
// place" rule), and confirm the restored index's content matches exactly —
// the e2e-green evidence the runbook's procedure and DW-7.4 both require.
//
// RPO/RTO: this drill measures RTO directly (wall-clock time from "index
// lost" to "restored index verified") — see the runbook for the documented
// RPO (snapshot cadence) and the real-domain-loss RTO caveat this local
// drill cannot exercise (no real AWS domain creation happens here).
func TestRestoreDrill(t *testing.T) {
	base := testutil.OpenSearchURL()
	client := testutil.HTTPClient
	repo := fmt.Sprintf("drill-repo-%d", time.Now().UnixNano())
	index := testutil.ScratchIndexName("semantic", t.Name())
	snapshot := "drill-snap-1"

	testutil.DeleteIndex(t, base, index)
	testutil.CreateScratchIndex(t, base, index)
	t.Cleanup(func() { testutil.DeleteIndex(t, base, index) })

	// Seed known fixture facts.
	fixtures := []map[string]any{
		{"subject": "orders-svc", "predicate": "owner", "object": "team-checkout", "statement": "orders-svc is owned by team-checkout"},
		{"subject": "payments-api", "predicate": "oncall", "object": "alice", "statement": "alice is on call for payments-api"},
	}
	for i, f := range fixtures {
		putDoc(t, client, base, index, fmt.Sprintf("fact-%d", i), f)
	}
	testutil.RefreshIndex(t, base, index)

	// Register the local fs repository (requires path.repo on the node —
	// scripts/dev-cluster.sh sets this; see the runbook).
	mustDo(t, client, http.MethodPut, base+"/_snapshot/"+repo, map[string]any{
		"type": "fs",
		"settings": map[string]any{
			"location": "drill-" + repo, // relative to path.repo
		},
	})
	t.Cleanup(func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, base+"/_snapshot/"+repo, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	})

	start := time.Now()

	// Snapshot, wait for completion.
	mustDo(t, client, http.MethodPut, fmt.Sprintf("%s/_snapshot/%s/%s?wait_for_completion=true", base, repo, snapshot), map[string]any{
		"indices":              index,
		"include_global_state": false,
	})

	// Simulate loss: delete the index entirely.
	testutil.DeleteIndex(t, base, index)
	if exists := indexExists(t, client, base, index); exists {
		t.Fatalf("index %s should be gone after simulated loss", index)
	}

	// Restore under a NEW name — never in place (the runbook's rule).
	restoredIndex := index + "-restored"
	mustDo(t, client, http.MethodPost, fmt.Sprintf("%s/_snapshot/%s/%s/_restore?wait_for_completion=true", base, repo, snapshot), map[string]any{
		"indices":            index,
		"rename_pattern":     index,
		"rename_replacement": restoredIndex,
	})
	t.Cleanup(func() { testutil.DeleteIndex(t, base, restoredIndex) })
	testutil.RefreshIndex(t, base, restoredIndex)

	rto := time.Since(start)
	t.Logf("restore drill RTO (snapshot->restore->verify, local single-node): %s", rto)
	if rto > time.Hour {
		t.Errorf("RTO = %s, want <= 1h (DW-7.4)", rto)
	}

	// e2e-green: the restored index serves the exact fixture content back.
	hits := searchAll(t, client, base, restoredIndex)
	if len(hits) != len(fixtures) {
		t.Fatalf("restored index has %d docs, want %d", len(hits), len(fixtures))
	}
	found := map[string]bool{}
	for _, h := range hits {
		if stmt, ok := h["statement"].(string); ok {
			found[stmt] = true
		}
	}
	for _, f := range fixtures {
		stmt := f["statement"].(string)
		if !found[stmt] {
			t.Errorf("restored index missing fixture statement %q", stmt)
		}
	}
}

func putDoc(t *testing.T, client *http.Client, base, index, id string, doc map[string]any) {
	t.Helper()
	mustDo(t, client, http.MethodPut, base+"/"+index+"/_doc/"+id, doc)
}

func mustDo(t *testing.T, client *http.Client, method, url string, body map[string]any) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding request body for %s %s: %v", method, url, err)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("building request %s %s: %v", method, url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var errBody bytes.Buffer
		errBody.ReadFrom(resp.Body)
		t.Fatalf("%s %s: status %d: %s", method, url, resp.StatusCode, errBody.String())
	}
}

func indexExists(t *testing.T, client *http.Client, base, index string) bool {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodHead, base+"/"+index, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("checking index %s: %v", index, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func searchAll(t *testing.T, client *http.Client, base, index string) []map[string]any {
	t.Helper()
	resp, err := client.Get(base + "/" + index + "/_search?size=100")
	if err != nil {
		t.Fatalf("searching %s: %v", index, err)
	}
	defer resp.Body.Close()
	var decoded struct {
		Hits struct {
			Hits []struct {
				Source map[string]any `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decoding search response for %s: %v", index, err)
	}
	out := make([]map[string]any, len(decoded.Hits.Hits))
	for i, h := range decoded.Hits.Hits {
		out[i] = h.Source
	}
	return out
}
