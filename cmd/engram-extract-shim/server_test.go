package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeBackend is the test-only Strategy implementation (plan-mandated "fake
// backend for tests"): a function value stands in for a real CLI so server
// tests never shell out.
type fakeBackend struct {
	run func(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

func (f fakeBackend) Run(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	return f.run(ctx, systemPrompt, userPrompt)
}

func newTestShim(b Backend) *Shim {
	return &Shim{Backend: b, Timeout: 5 * time.Second}
}

// TestDW_1_1_HealthOK verifies GET /health returns the exact stub-shaped
// body engramd's e2e healthcheck pattern expects.
func TestDW_1_1_HealthOK(t *testing.T) {
	shim := newTestShim(fakeBackend{run: func(context.Context, string, string) (string, error) { return "[]", nil }})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	shim.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"ok"}` {
		t.Fatalf("body = %q, want {\"status\":\"ok\"}", got)
	}
}

func TestHealth_RejectsNonGET(t *testing.T) {
	shim := newTestShim(fakeBackend{run: func(context.Context, string, string) (string, error) { return "[]", nil }})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	shim.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// TestDW_1_1_ChatCompletionsEnvelopeShape confirms the response is decodable
// into the exact stub-shaped envelope engramd's HTTPExtractor consumes:
// choices[0].message.content plus a usage block.
func TestDW_1_1_ChatCompletionsEnvelopeShape(t *testing.T) {
	shim := newTestShim(fakeBackend{run: func(context.Context, string, string) (string, error) {
		return `[{"subject":"rtd","predicate":"prefers","object":"tabs"}]`, nil
	}})
	rec := postChat(t, shim, "sys", "user text")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var resp chatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not decodable as chatResponse: %v (%s)", err, rec.Body.String())
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}
	if resp.Choices[0].Message.Role != "assistant" {
		t.Fatalf("message role = %q, want assistant", resp.Choices[0].Message.Role)
	}
	if resp.Usage.PromptTokens <= 0 {
		t.Fatalf("usage.prompt_tokens = %d, want > 0 for non-empty user content", resp.Usage.PromptTokens)
	}
}

// TestDW_1_2_HappyPathFactArray is the table-driven fake-backend test DW-1.2
// asks for: for a range of legal fact-array shapes returned by the backend,
// the shim's response content decodes to exactly that array of valid fact
// objects.
func TestDW_1_2_HappyPathFactArray(t *testing.T) {
	tests := []struct {
		name       string
		backendOut string
		wantFacts  []fact
	}{
		{
			name:       "single fact",
			backendOut: `[{"subject":"rtd","predicate":"prefers","object":"tabs"}]`,
			wantFacts:  []fact{{Subject: "rtd", Predicate: "prefers", Object: "tabs"}},
		},
		{
			name:       "multiple facts",
			backendOut: `[{"subject":"rtd","predicate":"prefers","object":"tabs"},{"subject":"rtd","predicate":"uses","object":"zsh"}]`,
			wantFacts: []fact{
				{Subject: "rtd", Predicate: "prefers", Object: "tabs"},
				{Subject: "rtd", Predicate: "uses", Object: "zsh"},
			},
		},
		{
			name:       "retraction (empty object is legal)",
			backendOut: `[{"subject":"rtd","predicate":"uses","object":""}]`,
			wantFacts:  []fact{{Subject: "rtd", Predicate: "uses", Object: ""}},
		},
		{
			name:       "fact with statement and valid_at",
			backendOut: `[{"subject":"rtd","predicate":"prefers","object":"tabs","statement":"rtd prefers tabs over spaces","valid_at":"2026-07-08T00:00:00Z"}]`,
			wantFacts: []fact{{
				Subject: "rtd", Predicate: "prefers", Object: "tabs",
				Statement: "rtd prefers tabs over spaces", ValidAt: "2026-07-08T00:00:00Z",
			}},
		},
		{
			name:       "legally empty extraction",
			backendOut: `[]`,
			wantFacts:  []fact{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shim := newTestShim(fakeBackend{run: func(context.Context, string, string) (string, error) {
				return tc.backendOut, nil
			}})
			rec := postChat(t, shim, "sys", "user text")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
			}
			var resp chatResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response not decodable: %v", err)
			}
			var got []fact
			if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &got); err != nil {
				t.Fatalf("choices[0].message.content not a valid fact array: %v (%s)", err, resp.Choices[0].Message.Content)
			}
			if len(got) != len(tc.wantFacts) {
				t.Fatalf("got %d facts, want %d: %+v", len(got), len(tc.wantFacts), got)
			}
			for i, wf := range tc.wantFacts {
				if got[i] != wf {
					t.Fatalf("fact %d = %+v, want %+v", i, got[i], wf)
				}
			}
		})
	}
}

// TestDW_1_3_BackendNonZeroExitReturnsRetryableError and its timeout sibling
// below cover the HTTP-level half of DW-1.3: a failing backend must produce
// a clean, retryable HTTP error — never a 500, never a hang.
func TestDW_1_3_BackendNonZeroExitReturnsRetryableError(t *testing.T) {
	shim := newTestShim(fakeBackend{run: func(context.Context, string, string) (string, error) {
		return "", errors.New("wrapped: " + ErrBackendUnavailable.Error())
	}})
	rec := postChat(t, shim, "sys", "user text")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (retryable)", rec.Code)
	}
}

func TestDW_1_3_BackendTimeoutReturnsRetryableErrorNotHang(t *testing.T) {
	shim := &Shim{
		Timeout: 20 * time.Millisecond,
		Backend: fakeBackend{run: func(ctx context.Context, _, _ string) (string, error) {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(5 * time.Second):
				return "", nil
			}
		}},
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postChat(t, shim, "sys", "user text") }()

	select {
	case rec := <-done:
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (retryable) on timeout", rec.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return within 2s of a 20ms backend timeout — it hung")
	}
}

func TestChatCompletions_RejectsNonPOST(t *testing.T) {
	shim := newTestShim(fakeBackend{run: func(context.Context, string, string) (string, error) { return "[]", nil }})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/chat/completions", nil)
	shim.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestChatCompletions_RejectsMalformedJSONBody(t *testing.T) {
	shim := newTestShim(fakeBackend{run: func(context.Context, string, string) (string, error) { return "[]", nil }})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader("{not json"))
	shim.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for malformed JSON body", rec.Code)
	}
}

func TestChatCompletions_RejectsMissingUserMessage(t *testing.T) {
	shim := newTestShim(fakeBackend{run: func(context.Context, string, string) (string, error) { return "[]", nil }})
	rec := httptest.NewRecorder()
	body := `{"messages":[{"role":"system","content":"sys only, no user message"}]}`
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(body))
	shim.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when the request carries no user message", rec.Code)
	}
}

// TestChatCompletions_UsesLastUserAndSystemMessage mirrors HTTPExtractor's
// convention of one system + one user message, but proves the handler picks
// the *last* occurrence of each role (matching cmd/engram-stub-llm's
// behavior) rather than assuming exactly one of each.
func TestChatCompletions_UsesLastUserAndSystemMessage(t *testing.T) {
	var gotSystem, gotUser string
	shim := newTestShim(fakeBackend{run: func(_ context.Context, systemPrompt, userPrompt string) (string, error) {
		gotSystem, gotUser = systemPrompt, userPrompt
		return "[]", nil
	}})
	body := `{"messages":[
		{"role":"system","content":"first-system"},
		{"role":"user","content":"first-user"},
		{"role":"system","content":"final-system"},
		{"role":"user","content":"final-user"}
	]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(body))
	shim.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotSystem != "final-system" || gotUser != "final-user" {
		t.Fatalf("backend saw system=%q user=%q, want final-system/final-user", gotSystem, gotUser)
	}
}

func postChat(t *testing.T, shim *Shim, systemPrompt, userPrompt string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(chatRequest{Messages: []chatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}})
	if err != nil {
		t.Fatalf("marshaling request: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(string(body)))
	shim.Handler().ServeHTTP(rec, req)
	return rec
}
