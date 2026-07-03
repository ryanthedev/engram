//go:build integration

package store

import (
	"context"
	"net/http"
	"os"
	"testing"
)

// TestDW_0_4_ApplyTemplatesIdempotent runs the apply tool twice against the
// live pinned dev cluster (OpenSearch 3.1.0): both runs succeed, and the
// second is a pure no-op — indices report "unchanged" and Changed() is false.
// The first run is NOT asserted to create anything: idempotency against
// whatever state the cluster already has is the contract.
func TestDW_0_4_ApplyTemplatesIdempotent(t *testing.T) {
	url := os.Getenv("ENGRAM_OPENSEARCH_URL")
	if url == "" {
		url = "http://localhost:9200"
	}
	client := &http.Client{Timeout: DefaultTimeout}
	ctx := context.Background()

	res1, err := Apply(ctx, client, url)
	if err != nil {
		t.Fatalf("first apply against %s: %v", url, err)
	}
	t.Logf("first apply (OpenSearch %s): %v", res1.Version, res1.Actions)

	res2, err := Apply(ctx, client, url)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	t.Logf("second apply: %v", res2.Actions)
	if res2.Changed() {
		t.Errorf("re-run must be a no-op, but it created resources: %v", res2.Actions)
	}
	for _, idx := range []string{EpisodicIndex, SemanticIndex} {
		if res2.Actions[idx] != "unchanged" {
			t.Errorf("re-run: index %s action = %q, want unchanged", idx, res2.Actions[idx])
		}
	}
}
