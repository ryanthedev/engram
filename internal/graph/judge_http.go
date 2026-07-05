package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// judgeSystemPrompt instructs an OpenAI-compatible model to act as the dedup
// tie-break judge and emit exactly the wire shape parseSameEntity reads.
const judgeSystemPrompt = `You judge whether two mentions of a name refer to the SAME real-world entity or two DIFFERENT entities that happen to share a name (e.g. "Jordan" the country vs "Jordan" the basketball player). Use the surrounding context, not just the name, to decide.
Return ONLY a JSON object: {"same_entity": true|false, "reason": string}.
When genuinely unsure, prefer false (treating two different entities as one is harder to undo than the reverse).`

// HTTPJudge is the production dedup tie-break Judge: an OpenAI-compatible
// chat-completions LLM call (the same transport shape as
// ingest.HTTPExtractor and experience.HTTPGatekeeper), used ONLY for the
// ambiguous middle band Deduper.Decide cannot resolve from embedding +
// lexical scores alone. Any failure (transport error, non-2xx, timeout,
// unparseable output) resolves to "not the same" — see Judge's doc comment
// for why that is the safe default here (not fail-closed, fail-SAFE:
// over-splitting is cheap to undo, over-merging is not).
type HTTPJudge struct {
	client  *http.Client
	baseURL string
	model   string
}

var _ Judge = (*HTTPJudge)(nil)

// NewHTTPJudge returns a Judge backed by the OpenAI-compatible endpoint at
// baseURL, using model. client must not be nil.
func NewHTTPJudge(client *http.Client, baseURL, model string) *HTTPJudge {
	return &HTTPJudge{client: client, baseURL: strings.TrimRight(baseURL, "/"), model: model}
}

type judgeChatRequest struct {
	Model       string             `json:"model"`
	Temperature float64            `json:"temperature"`
	Messages    []judgeChatMessage `json:"messages"`
}

type judgeChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type judgeChatResponse struct {
	Choices []struct {
		Message judgeChatMessage `json:"message"`
	} `json:"choices"`
}

type sameEntityWire struct {
	Same   bool   `json:"same_entity"`
	Reason string `json:"reason"`
}

// SameEntity implements Judge over the chat-completions endpoint.
func (j *HTTPJudge) SameEntity(ctx context.Context, mention, existing Candidate) (bool, string, error) {
	body, err := json.Marshal(judgeChatRequest{
		Model:       j.model,
		Temperature: 0,
		Messages: []judgeChatMessage{
			{Role: "system", Content: judgeSystemPrompt},
			{Role: "user", Content: renderPairForJudge(mention, existing)},
		},
	})
	if err != nil {
		return false, "", fmt.Errorf("graph: encoding judge request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return false, "", fmt.Errorf("graph: building judge request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := j.client.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("graph: judge call to %s: %w", j.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return false, "", fmt.Errorf("graph: judge endpoint returned %s: %s", resp.Status, string(b))
	}
	var decoded judgeChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return false, "", fmt.Errorf("graph: decoding judge response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return false, "", fmt.Errorf("graph: judge response carried no choices")
	}
	return parseSameEntity(decoded.Choices[0].Message.Content)
}

// parseSameEntity decodes the judge's wire response; unparseable content is
// an error (the caller's fail-safe default applies).
func parseSameEntity(content string) (bool, string, error) {
	trimmed := strings.TrimSpace(stripJSONFences(content))
	var w sameEntityWire
	if err := json.Unmarshal([]byte(trimmed), &w); err != nil {
		return false, "", fmt.Errorf("graph: unparseable judge verdict: %w", err)
	}
	return w.Same, w.Reason, nil
}

func renderPairForJudge(mention, existing Candidate) string {
	b, _ := json.MarshalIndent(struct {
		Mention  Candidate `json:"mention"`
		Existing Candidate `json:"existing"`
	}{
		Candidate{Name: mention.Name, Context: mention.Context},
		Candidate{Name: existing.Name, Aliases: existing.Aliases, Context: existing.Context},
	}, "", "  ")
	return string(b)
}

func stripJSONFences(s string) string {
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
