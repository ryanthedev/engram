package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"time"

	"github.com/ryanthedev/engram/internal/embed"
)

// recreateScratchIndex drops and recreates index empty, matching the
// engram-{episodic,semantic,ledger}* template patterns (settings/mappings
// come from the already-applied templates) — mirrors cmd/engram-perf's
// scratch-index helper, generalized to any of the three index kinds.
func recreateScratchIndex(ctx context.Context, client *http.Client, baseURL, index string) error {
	delReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+"/"+index, nil)
	if err != nil {
		return err
	}
	if resp, err := client.Do(delReq); err == nil {
		resp.Body.Close()
	}
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, baseURL+"/"+index, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(putReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}

var vocabWords = []string{
	"orders-svc", "payments-api", "checkout-web", "incident", "deploy",
	"timeout", "latency", "connection", "pool", "leak", "rotation",
	"token", "quota", "rate", "limit", "postgres", "kafka", "cdn",
	"oncall", "runbook",
}

func syntheticText(r *rand.Rand, i int) string {
	a, b, c := vocabWords[r.Intn(len(vocabWords))], vocabWords[r.Intn(len(vocabWords))], vocabWords[r.Intn(len(vocabWords))]
	return fmt.Sprintf("%s %s %s event-%d", a, b, c, i)
}

// bulkSeedEpisodic indexes n synthetic episodic docs (already processed —
// processed_at set — so the seeded corpus does not itself enter the outbox
// and skew worker-lag measurement) with precomputed embeddings, via the
// OpenSearch _bulk API, matching cmd/engram-perf's seeding approach.
func bulkSeedEpisodic(ctx context.Context, client *http.Client, baseURL, index string, n int, embedder embed.Embedder) error {
	const batchSize = 500
	now := time.Now().UTC().Format(time.RFC3339Nano)
	r := rand.New(rand.NewSource(42))
	for start := 0; start < n; start += batchSize {
		end := min(start+batchSize, n)
		texts := make([]string, end-start)
		for i := start; i < end; i++ {
			texts[i-start] = syntheticText(r, i)
		}
		vectors, err := embedder.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("embedding batch [%d,%d): %w", start, end, err)
		}
		var buf bytes.Buffer
		for i := start; i < end; i++ {
			action, _ := json.Marshal(map[string]any{"index": map[string]any{"_index": index}})
			buf.Write(action)
			buf.WriteByte('\n')
			doc, _ := json.Marshal(map[string]any{
				"event_id":       fmt.Sprintf("seed-%d", i),
				"tenant_id":      fmt.Sprintf("tenant-%d", i%10),
				"owner_agent_id": fmt.Sprintf("agent-%d", i%50),
				"kind":           "seed",
				"text":           texts[i-start],
				"text_embedding": vectors[i-start],
				"occurred_at":    now,
				"created_at":     now,
				"processed_at":   now, // pre-processed: never enters the outbox scan
				"attempts":       0,
			})
			buf.Write(doc)
			buf.WriteByte('\n')
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/_bulk", &buf)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-ndjson")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("bulk request [%d,%d): %w", start, end, err)
		}
		var result struct {
			Errors bool `json:"errors"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if result.Errors {
			return fmt.Errorf("bulk seed batch [%d,%d) reported item errors", start, end)
		}
		if (start/batchSize)%40 == 0 {
			fmt.Fprintf(os.Stderr, "  seeded %d/%d\n", end, n)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/"+index+"/_refresh", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
