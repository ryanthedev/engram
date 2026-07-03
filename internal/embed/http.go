package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// HTTPEmbedder calls a co-located TEI-style embedding endpoint (D15): POST
// {baseURL}/embed with {"inputs": texts}, expecting a JSON array of
// float32 vectors, index-aligned with the input. Model/Revision/Dim are
// configured explicitly by the caller — D15 requires an exact pinned
// artifact, and different TEI backends report identity differently, so
// pinning is a config decision, not something to infer from the service.
type HTTPEmbedder struct {
	client  *http.Client
	baseURL string
	info    ModelInfo
}

// NewHTTPEmbedder returns an Embedder backed by a real embedding service at
// baseURL, reporting the given pinned model identity. client must not be nil.
func NewHTTPEmbedder(client *http.Client, baseURL string, info ModelInfo) *HTTPEmbedder {
	return &HTTPEmbedder{client: client, baseURL: baseURL, info: info}
}

// embedRequest is the TEI-style request body.
type embedRequest struct {
	Inputs []string `json:"inputs"`
}

// Embed posts texts to the embedding service and returns their vectors,
// index-aligned. A non-2xx response or a vector-count mismatch is an error.
func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedRequest{Inputs: texts})
	if err != nil {
		return nil, fmt.Errorf("embed: encoding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: request to %s: %w", e.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("embed: service returned %s: %s", resp.Status, string(b))
	}
	var vectors [][]float32
	if err := json.NewDecoder(resp.Body).Decode(&vectors); err != nil {
		return nil, fmt.Errorf("embed: decoding response: %w", err)
	}
	if len(vectors) != len(texts) {
		return nil, fmt.Errorf("embed: service returned %d vectors for %d inputs", len(vectors), len(texts))
	}
	return vectors, nil
}

// Info reports the pinned model identity this HTTPEmbedder was configured
// with.
func (e *HTTPEmbedder) Info() ModelInfo { return e.info }
