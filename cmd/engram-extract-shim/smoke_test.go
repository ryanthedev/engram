//go:build smoke

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestDW_1_6_LiveSmokeAgyExtractsFaithfulTriple is the DW-1.6 live smoke
// test: through the real HTTP endpoint, with the real agy backend and its
// cheap-model preset, extract at least one faithful {subject,predicate,
// object} triple from a sample memory sentence.
//
// This file is gated behind the `smoke` build tag (the repo's established
// pattern for tests needing a live external resource — see the `integration`
// tag on deploy/aws/reindex/alias_integration_test.go and friends) so it
// never runs as part of the default `go test ./...` / `make test`: it costs
// a real agy call (network + model tokens) and takes several seconds. It
// additionally skips with a clear reason when agy is not on PATH, per the
// plan's explicit instruction not to fake a live result. Run it explicitly:
//
//	go test -tags=smoke ./cmd/engram-extract-shim/... -run TestDW_1_6_LiveSmokeAgyExtractsFaithfulTriple -v
//
// or via `make smoke-extract-shim`.
func TestDW_1_6_LiveSmokeAgyExtractsFaithfulTriple(t *testing.T) {
	if _, err := exec.LookPath("agy"); err != nil {
		t.Skip("agy not found on PATH — DW-1.6 live smoke test cannot run in this environment; see the plan's note on DW-1.6 for how to run it manually once agy is installed and authenticated")
	}

	backend := agyBackend{}
	shim := &Shim{Backend: backend, Timeout: 60 * time.Second}

	// A sample memory sentence carrying one unambiguous durable fact, mirroring
	// the shape of the 182 already-ingested prose memories this shim exists to
	// unlock (see .code-foundations/research/2026-07-08-extraction-cli-shim.md).
	const sampleEvent = "[event evt-smoke-1 kind=note at=2026-07-08T12:00:00Z]\nrtd prefers tabs over spaces in Go code.\n\n"

	body, err := json.Marshal(chatRequest{Messages: []chatMessage{
		{Role: "system", Content: extractionSystemPromptForSmokeTest},
		{Role: "user", Content: sampleEvent},
	}})
	if err != nil {
		t.Fatalf("marshaling request: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(string(body)))
	shim.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("live agy call: status = %d, want 200; body=%s (agy may not be authenticated — see DW-1.6's manual run instructions)", rec.Code, rec.Body.String())
	}

	var resp chatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not decodable as chatResponse: %v (%s)", err, rec.Body.String())
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}

	var facts []fact
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &facts); err != nil {
		t.Fatalf("choices[0].message.content not a valid fact array: %v (%s)", err, resp.Choices[0].Message.Content)
	}
	if len(facts) == 0 {
		t.Fatalf("live agy extraction returned zero facts for a sentence carrying one clear durable fact; content=%s", resp.Choices[0].Message.Content)
	}

	f := facts[0]
	if strings.TrimSpace(f.Subject) == "" || strings.TrimSpace(f.Predicate) == "" {
		t.Fatalf("extracted fact has blank subject/predicate — not faithful: %+v", f)
	}
	// Faithfulness spot-check: the extracted triple should recognizably trace
	// back to the source sentence (subject or object mentions "rtd", "tabs",
	// or "spaces" per the input) rather than being unrelated content.
	joined := strings.ToLower(f.Subject + " " + f.Predicate + " " + f.Object)
	if !strings.Contains(joined, "rtd") && !strings.Contains(joined, "tab") && !strings.Contains(joined, "space") {
		t.Fatalf("extracted fact %+v does not appear to trace back to the source sentence \"rtd prefers tabs over spaces\"", f)
	}
	t.Logf("DW-1.6 live smoke: agy extracted a faithful triple: %+v", f)
}

// extractionSystemPromptForSmokeTest mirrors internal/ingest/http.go's
// extractionSystemPrompt (kept local to avoid importing internal/ingest from
// a cmd package, and because the shim only ever relays whatever system
// prompt engramd sends — it does not own this text in production).
const extractionSystemPromptForSmokeTest = `You extract durable facts from agent event logs.
Return ONLY a JSON array (no prose, no code fences) of objects:
{"subject": string, "predicate": string, "object": string, "statement": string, "valid_at": RFC3339 timestamp or omit}.
Use an empty "object" to retract a fact that no longer holds. Return [] when the events contain no durable facts.`
