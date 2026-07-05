//go:build drill

package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

// containerRuntime returns "podman" or "docker", whichever is on PATH —
// matching the Makefile's own runtime-detection convention.
func containerRuntime(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("podman"); err == nil {
		return "podman"
	}
	if _, err := exec.LookPath("docker"); err == nil {
		return "docker"
	}
	t.Skip("neither podman nor docker found; skipping the OpenSearch node-restart drill")
	return ""
}

// devClusterContainer is the name scripts/dev-cluster.sh gives the local
// pinned-3.1 OpenSearch container this drill stops and restarts.
const devClusterContainer = "engram-dev-os"

// TestFailureDrill_OpenSearchNodeRestart is DW-7.5's OpenSearch half: stop
// the local dev cluster's container (simulating a node failure), confirm
// Ingest/Search fail during the outage (the availability-SLO alert
// condition), restart it, and confirm the system recovers with no data loss
// — an event durably appended just before the outage is still there and
// gets processed once the cluster is back.
func TestFailureDrill_OpenSearchNodeRestart(t *testing.T) {
	runtime := containerRuntime(t)
	base := testutil.OpenSearchURL()
	if base != "http://localhost:9200" {
		t.Skip("this drill targets the local dev cluster on :9200 specifically (stops/restarts its container); ENGRAM_OPENSEARCH_URL points elsewhere")
	}

	// Confirm the container is actually running before we touch it — this
	// drill is destructive and must not silently no-op against a cluster it
	// doesn't control.
	out, err := exec.Command(runtime, "ps", "--format", "{{.Names}}").CombinedOutput()
	if err != nil {
		t.Fatalf("listing containers: %v: %s", err, out)
	}
	if !containsLine(string(out), devClusterContainer) {
		t.Skipf("%s is not running (expected from `make dev-cluster`); skipping", devClusterContainer)
	}

	suffix := fmt.Sprintf("drillos-%d", time.Now().UnixNano())
	epIdx := "engram-episodic-" + suffix
	testutil.CreateScratchIndex(t, base, epIdx)
	t.Cleanup(func() { testutil.DeleteIndex(t, base, epIdx) })

	client := &http.Client{Timeout: 5 * time.Second}
	st := store.NewOpenSearchStore(client, base, store.WithEpisodicIndex(epIdx))
	ctx := context.Background()

	// Durably append one event BEFORE the outage — this is the "no data
	// loss" evidence: it must survive the node being down and still be
	// present/processable once the cluster returns.
	preOutageID, err := st.Append(ctx, memory.Episodic{
		EventID: "pre-outage-1", TenantID: "drill", Text: "before the outage",
		Kind: "drill", OccurredAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Append before outage: %v", err)
	}
	testutil.RefreshIndex(t, base, epIdx)

	// Stop the node.
	if out, err := exec.Command(runtime, "stop", devClusterContainer).CombinedOutput(); err != nil {
		t.Fatalf("stopping %s: %v: %s", devClusterContainer, err, out)
	}
	t.Cleanup(func() {
		// Always try to bring it back, even if the test fails partway.
		_ = exec.Command(runtime, "start", devClusterContainer).Run()
		waitClusterUp(t, base, 60*time.Second)
	})

	// During the outage: calls must fail (the availability-SLO alert
	// condition), not hang forever or silently succeed against stale state.
	shortCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, err = st.Append(shortCtx, memory.Episodic{
		EventID: "during-outage-1", TenantID: "drill", Text: "during the outage",
		Kind: "drill", OccurredAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	})
	cancel()
	if err == nil {
		t.Fatal("Append succeeded during the outage — expected it to fail while the node is down")
	}
	t.Logf("Append correctly failed during the outage: %v", err)

	// Restart the node.
	if out, err := exec.Command(runtime, "start", devClusterContainer).CombinedOutput(); err != nil {
		t.Fatalf("starting %s: %v: %s", devClusterContainer, err, out)
	}
	waitClusterUp(t, base, 60*time.Second)

	// No data loss: the pre-outage event is still there, fully intact.
	fact, ok, err := findEpisodicByID(t, client, base, epIdx, preOutageID)
	if err != nil {
		t.Fatalf("reading pre-outage event after recovery: %v", err)
	}
	if !ok {
		t.Fatal("pre-outage event is missing after the node came back — DATA LOSS")
	}
	if fact != "before the outage" {
		t.Fatalf("pre-outage event text = %q, want %q", fact, "before the outage")
	}
}

func containsLine(s, target string) bool {
	for _, line := range splitLines(s) {
		if line == target {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i, r := range s {
		if r == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// waitClusterUp waits not just for the HTTP endpoint to answer but for the
// cluster to report at least yellow health (every shard assigned somewhere)
// — a 200 on `/` can come back before shard recovery finishes on restart,
// which would make a doc read racily 404 even though nothing was lost.
func waitClusterUp(t *testing.T, base string, timeout time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: timeout + 5*time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/_cluster/health?wait_for_status=yellow&timeout=5s")
		if err == nil {
			var decoded struct {
				Status string `json:"status"`
			}
			jsonErr := json.NewDecoder(resp.Body).Decode(&decoded)
			resp.Body.Close()
			if jsonErr == nil && (decoded.Status == "yellow" || decoded.Status == "green") {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("cluster at %s did not reach yellow/green health within %s", base, timeout)
}

func findEpisodicByID(t *testing.T, client *http.Client, base, index, id string) (text string, ok bool, err error) {
	t.Helper()
	resp, err := client.Get(base + "/" + index + "/_doc/" + id)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	var decoded struct {
		Found  bool `json:"found"`
		Source struct {
			Text string `json:"text"`
		} `json:"_source"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", false, err
	}
	return decoded.Source.Text, decoded.Found, nil
}
