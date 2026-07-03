// Package seed indexes a gold set's static corpus directly into a semantic
// index under each doc's literal id — the harness's retrieval-test fixture
// path (Phase 1 scope: "T2 is seeded from a static fact dataset by the
// harness for retrieval tests"). This intentionally bypasses the
// content-addressed write protocol (D11): gold set ExpectedIDs are the
// corpus doc ids verbatim, and reconciled writes arrive via Phase 2's
// worker, not this harness fixture.
package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/eval"
)

// Corpus indexes every doc in gs.Corpus into index (a concrete OpenSearch
// index name matching the engram-semantic* template pattern) at
// _id=doc.ID, embedding each doc's Text via embedder in one batched call.
// tenantID/ownerAgentID scope the seeded facts for tests that need Filter
// isolation; pass "" for both to match the eval harness's default
// (unscoped) Filter.
func Corpus(ctx context.Context, client *http.Client, baseURL, index string, gs eval.GoldSet, embedder embed.Embedder, tenantID, ownerAgentID string) error {
	if len(gs.Corpus) == 0 {
		return nil
	}
	texts := make([]string, len(gs.Corpus))
	for i, d := range gs.Corpus {
		texts[i] = d.Text
	}
	vectors, err := embedder.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("seed: embedding corpus: %w", err)
	}
	if len(vectors) != len(gs.Corpus) {
		return fmt.Errorf("seed: embedder returned %d vectors for %d docs", len(vectors), len(gs.Corpus))
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i, d := range gs.Corpus {
		doc := map[string]any{
			"statement":         d.Text,
			"fact_embedding":    vectors[i],
			"content_key":       d.ID,
			"extractor_version": "goldset-fixture",
			"tenant_id":         tenantID,
			"owner_agent_id":    ownerAgentID,
			"valid_at":          now,
			"created_at":        now,
		}
		if err := put(ctx, client, fmt.Sprintf("%s/%s/_doc/%s", baseURL, index, d.ID), doc); err != nil {
			return fmt.Errorf("seed: indexing %s: %w", d.ID, err)
		}
	}
	return refresh(ctx, client, baseURL, index)
}

func put(ctx context.Context, client *http.Client, url string, doc any) error {
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("unexpected status %s: %s", resp.Status, string(b))
	}
	return nil
}

func refresh(ctx context.Context, client *http.Client, baseURL, index string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/"+index+"/_refresh", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("refresh %s: %s", index, resp.Status)
	}
	return nil
}
