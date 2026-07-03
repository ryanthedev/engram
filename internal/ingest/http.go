package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/ryanthedev/engram/internal/memory"
)

// extractionSystemPrompt instructs the cheap model to emit the wire shape
// ParseExtraction validates.
const extractionSystemPrompt = `You extract durable facts from agent event logs.
Return ONLY a JSON array (no prose, no code fences) of objects:
{"subject": string, "predicate": string, "object": string, "statement": string, "valid_at": RFC3339 timestamp or omit}.
Use an empty "object" to retract a fact that no longer holds. Return [] when the events contain no durable facts.`

// HTTPExtractor is the real cheap-model Extractor: it calls an
// OpenAI-compatible chat-completions endpoint (D4's LLM gate) and validates
// the response through the same ParseExtraction barricade as the
// deterministic fake. Real token counts from the response's usage block feed
// Meter (DW-2.6).
type HTTPExtractor struct {
	client  *http.Client
	baseURL string
	model   string
	// Meter, when non-nil, accumulates real usage for the cost metric.
	Meter *CostMeter

	calls atomic.Int64
}

var _ Extractor = (*HTTPExtractor)(nil)

// NewHTTPExtractor returns an Extractor backed by the OpenAI-compatible
// endpoint at baseURL (e.g. "https://api.openai.com/v1"), using model.
// client must not be nil; its timeout bounds each LLM call (worker retry +
// dead-letter handle timeouts above this layer).
func NewHTTPExtractor(client *http.Client, baseURL, model string) *HTTPExtractor {
	return &HTTPExtractor{client: client, baseURL: strings.TrimRight(baseURL, "/"), model: model}
}

// Calls reports how many chat-completions requests were issued.
func (e *HTTPExtractor) Calls() int64 { return e.calls.Load() }

// chat-completions request/response wire types (the OpenAI-compatible
// subset this extractor needs).
type chatRequest struct {
	Model       string        `json:"model"`
	Temperature float64       `json:"temperature"`
	Messages    []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Extract implements Extractor over the chat-completions endpoint. Transport
// failures and non-2xx responses return plain errors (retryable by the
// outbox); output that parses but violates the wire contract returns
// ErrMalformed; a legal empty array returns ErrNoFacts.
func (e *HTTPExtractor) Extract(ctx context.Context, events []memory.Episodic) ([]memory.SemanticFact, error) {
	e.calls.Add(1)
	var sb strings.Builder
	for _, ev := range events {
		fmt.Fprintf(&sb, "[event %s kind=%s at=%s]\n%s\n\n", ev.EventID, ev.Kind, ev.OccurredAt.UTC().Format("2006-01-02T15:04:05Z07:00"), ev.Text)
	}
	body, err := json.Marshal(chatRequest{
		Model:       e.model,
		Temperature: 0,
		Messages: []chatMessage{
			{Role: "system", Content: extractionSystemPrompt},
			{Role: "user", Content: sb.String()},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("ingest: encoding extraction request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ingest: building extraction request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ingest: extraction call to %s: %w", e.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("ingest: extraction endpoint returned %s: %s", resp.Status, string(b))
	}
	var decoded chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("ingest: decoding extraction response: %w", err)
	}
	if e.Meter != nil {
		e.Meter.Record(len(events), decoded.Usage.PromptTokens, decoded.Usage.CompletionTokens)
	}
	if len(decoded.Choices) == 0 {
		return nil, fmt.Errorf("%w: response carried no choices", ErrMalformed)
	}
	return ParseExtraction([]byte(decoded.Choices[0].Message.Content))
}
