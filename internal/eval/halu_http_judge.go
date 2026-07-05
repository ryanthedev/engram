package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// haluJudgeSystemPrompt instructs an OpenAI-compatible model to judge
// grounding: exactly the HTTPGatekeeper/HTTPExtractor transport shape, so
// production can point a real cheap model at either judge interchangeably.
const haluJudgeSystemPrompt = `You are a strict grounding judge for a memory system's hallucination gate. Given a KNOWN-GOOD CORPUS and a STATEMENT a memory system asserted, decide whether the statement is fully supported by the corpus (every claim in it is stated or directly implied by the corpus text) or a hallucination (any part of it is not supported).
Return ONLY a JSON object: {"supported": true|false, "reason": string}.
When in doubt, answer false — an unsupported claim in agent memory is worse than a false negative here.`

// HTTPHaluJudge is the production HaluJudge: an OpenAI-compatible
// chat-completions LLM judge (the same transport shape as
// ingest.HTTPExtractor / experience.HTTPGatekeeper). It is FAIL-CLOSED: a
// transport error, non-2xx response, timeout, empty choice, or unparseable
// verdict all resolve to (false, err) — never a silent "supported". A caller
// (RunHallucinationSuite) that ignores the error still counts the statement
// as a hallucination, never as clean.
type HTTPHaluJudge struct {
	client  *http.Client
	baseURL string
	model   string
}

var _ HaluJudge = (*HTTPHaluJudge)(nil)

// NewHTTPHaluJudge returns a HaluJudge backed by the OpenAI-compatible
// endpoint at baseURL, using model. client must not be nil; its timeout (and
// the caller's ctx) bound each judge call.
func NewHTTPHaluJudge(client *http.Client, baseURL, model string) *HTTPHaluJudge {
	return &HTTPHaluJudge{client: client, baseURL: strings.TrimRight(baseURL, "/"), model: model}
}

type haluChatRequest struct {
	Model       string            `json:"model"`
	Temperature float64           `json:"temperature"`
	Messages    []haluChatMessage `json:"messages"`
}

type haluChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type haluChatResponse struct {
	Choices []struct {
		Message haluChatMessage `json:"message"`
	} `json:"choices"`
}

type haluVerdictWire struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
}

// Supported implements HaluJudge over the chat-completions endpoint.
func (j *HTTPHaluJudge) Supported(ctx context.Context, corpus, statement string) (bool, error) {
	body, err := json.Marshal(haluChatRequest{
		Model:       j.model,
		Temperature: 0,
		Messages: []haluChatMessage{
			{Role: "system", Content: haluJudgeSystemPrompt},
			{Role: "user", Content: fmt.Sprintf("KNOWN-GOOD CORPUS:\n%s\n\nSTATEMENT:\n%s", corpus, statement)},
		},
	})
	if err != nil {
		return false, fmt.Errorf("eval: encoding halu judge request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("eval: building halu judge request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := j.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("eval: halu judge call to %s: %w", j.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return false, fmt.Errorf("eval: halu judge endpoint returned %s: %s", resp.Status, string(b))
	}
	var decoded haluChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return false, fmt.Errorf("eval: decoding halu judge response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return false, fmt.Errorf("eval: halu judge response carried no choices")
	}
	trimmed := strings.TrimSpace(stripHaluFences(decoded.Choices[0].Message.Content))
	var vw haluVerdictWire
	if err := json.Unmarshal([]byte(trimmed), &vw); err != nil {
		return false, fmt.Errorf("eval: unparseable halu judge verdict: %w", err)
	}
	return vw.Supported, nil
}

func stripHaluFences(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return s
	}
	t = strings.TrimPrefix(t, "```")
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[i+1:]
	}
	return strings.TrimSuffix(strings.TrimSpace(t), "```")
}
