//go:build integration

// Package spike holds the three Phase-0 live-cluster spikes (DW-0.9). They
// run against the pinned dev OpenSearch 3.1.0 (`make dev-cluster`, then
// `make integration`) and their findings are transcribed into
// docs/spikes/phase-0-findings.md.
package spike

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"testing"

	"github.com/ryanthedev/engram/internal/store"
)

var (
	applyOnce sync.Once
	applyErr  error
)

// cluster returns the dev-cluster base URL and lazily applies the Engram
// templates/pipeline (idempotent) so every spike sees the real contract.
func cluster(t *testing.T) string {
	t.Helper()
	url := os.Getenv("ENGRAM_OPENSEARCH_URL")
	if url == "" {
		url = "http://localhost:9200"
	}
	applyOnce.Do(func() {
		_, applyErr = store.Apply(context.Background(), httpClient, url)
	})
	if applyErr != nil {
		t.Fatalf("applying templates to %s: %v", url, applyErr)
	}
	return url
}

var httpClient = &http.Client{Timeout: store.DefaultTimeout}

// call issues one JSON request and returns status + decoded body.
func call(t *testing.T, method, url string, body any) (int, map[string]any) {
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
	resp, err := httpClient.Do(req)
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

// vec returns a deterministic unit vector for id (1024-dim, matching the
// template contract).
func vec(id int64) []float32 {
	r := rand.New(rand.NewSource(id))
	v := make([]float32, store.EmbeddingDim)
	var norm float64
	for i := range v {
		v[i] = float32(r.NormFloat64())
		norm += float64(v[i]) * float64(v[i])
	}
	n := float32(math.Sqrt(norm))
	for i := range v {
		v[i] /= n
	}
	return v
}

func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// deleteIndex removes a spike scratch index, tolerating 404.
func deleteIndex(t *testing.T, base, index string) {
	t.Helper()
	status, _ := call(t, http.MethodDelete, base+"/"+index, nil)
	if status != http.StatusOK && status != http.StatusNotFound {
		t.Logf("cleanup of %s returned %d", index, status)
	}
}

func refresh(t *testing.T, base, index string) {
	t.Helper()
	if status, body := call(t, http.MethodPost, base+"/"+index+"/_refresh", nil); status != http.StatusOK {
		t.Fatalf("refresh %s: %d %v", index, status, body)
	}
}

func indexDoc(t *testing.T, base, index, id string, doc any) {
	t.Helper()
	status, body := call(t, http.MethodPut, fmt.Sprintf("%s/%s/_doc/%s", base, index, id), doc)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("indexing %s/%s: %d %v", index, id, status, body)
	}
}
