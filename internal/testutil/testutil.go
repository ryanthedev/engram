// Package testutil holds small helpers shared by tests only.
package testutil

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// RepoRoot returns the module root (the directory containing go.mod) so tests
// can reach checked-in artifacts (workflows, gold sets, the plan file)
// regardless of which package directory `go test` runs from.
func RepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}

// HTTPClient is a small pre-configured client shared by live-cluster
// integration tests.
var HTTPClient = &http.Client{Timeout: 30 * time.Second}

// OpenSearchURL returns the dev-cluster base URL: ENGRAM_OPENSEARCH_URL, or
// http://localhost:9200 by default — the same convention every Phase-0 spike
// and the apply-templates tool use.
func OpenSearchURL() string {
	if u := os.Getenv("ENGRAM_OPENSEARCH_URL"); u != "" {
		return u
	}
	return "http://localhost:9200"
}

// Call issues one JSON request against the dev cluster and returns the
// status code and decoded body.
func Call(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return resp.StatusCode, decoded
}

// DeleteIndex removes a scratch index, tolerating 404.
func DeleteIndex(t *testing.T, base, index string) {
	t.Helper()
	status, _ := Call(t, http.MethodDelete, base+"/"+index, nil)
	if status != http.StatusOK && status != http.StatusNotFound {
		t.Logf("cleanup of %s returned %d", index, status)
	}
}

// CreateScratchIndex creates index with an empty body, so it inherits
// whatever index template matches its name pattern (engram-episodic* /
// engram-semantic*) — the same trick the Phase-0 spikes use to get the real
// mapping without duplicating it.
func CreateScratchIndex(t *testing.T, base, index string) {
	t.Helper()
	status, body := Call(t, http.MethodPut, base+"/"+index, nil)
	if status != http.StatusOK {
		t.Fatalf("creating scratch index %s: %d %v", index, status, body)
	}
}

// RefreshIndex makes recently written docs visible to search.
func RefreshIndex(t *testing.T, base, index string) {
	t.Helper()
	if status, body := Call(t, http.MethodPost, base+"/"+index+"/_refresh", nil); status != http.StatusOK {
		t.Fatalf("refresh %s: %d %v", index, status, body)
	}
}

// IndexDoc PUTs doc at index/_doc/id.
func IndexDoc(t *testing.T, base, index, id string, doc any) {
	t.Helper()
	status, body := Call(t, http.MethodPut, base+"/"+index+"/_doc/"+id, doc)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("indexing %s/%s: %d %v", index, id, status, body)
	}
}

// GetSeqNo fetches the current _seq_no/_primary_term for id — the
// optimistic-concurrency tokens a guarded Update needs (D7).
func GetSeqNo(t *testing.T, base, index, id string) (seqNo, primaryTerm int64) {
	t.Helper()
	status, body := Call(t, http.MethodGet, base+"/"+index+"/_doc/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("getting %s/%s: %d %v", index, id, status, body)
	}
	sn, _ := body["_seq_no"].(float64)
	pt, _ := body["_primary_term"].(float64)
	return int64(sn), int64(pt)
}

// DigString walks nested string-keyed maps and returns a string leaf (or ""
// if the path doesn't resolve to a string).
func DigString(m map[string]any, keys ...string) string {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	s, _ := cur.(string)
	return s
}

// ScratchIndexName builds a per-test scratch index name matching the
// engram-{tier}* template pattern, sanitized for OpenSearch's lowercase/
// no-underscore-friendly naming.
func ScratchIndexName(tier, testName string) string {
	sanitized := strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(testName))
	return "engram-" + tier + "-" + sanitized
}
