// Command engram-extract-shim is a host-side HTTP server that satisfies
// engramd's OpenAI-compatible extraction contract (POST /chat/completions,
// GET /health — see internal/ingest/http.go) by delegating each request to a
// selectable headless agent CLI (agy/codex/claude) running a cheap model at
// low reasoning effort. It exists so engramd's -extract-url can point at
// real extraction instead of cmd/engram-stub-llm's fixture stub, with zero
// changes to engramd's Go extractor. See
// .code-foundations/research/2026-07-08-extraction-cli-shim.md for the full
// reverse-engineered wire contract this mirrors.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// maxRequestBytes bounds the incoming request body — engramd batches events
// into one extraction call, but an unbounded body is still external input
// that must not be trusted to self-limit.
const maxRequestBytes = 4 << 20 // 4 MiB

// Wire types mirror cmd/engram-stub-llm/main.go's chatRequest/chatResponse
// exactly, so engramd's HTTPExtractor (internal/ingest/http.go) cannot tell
// the shim apart from the stub or a real hosted API.
type chatRequest struct {
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []choice `json:"choices"`
	Usage   usage    `json:"usage"`
}

type choice struct {
	Message chatMessage `json:"message"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Shim serves the extraction contract over HTTP, delegating fact extraction
// to Backend. Timeout bounds each backend invocation so a hung or slow CLI
// becomes a clean, retryable HTTP failure instead of a hang.
type Shim struct {
	Backend Backend
	Timeout time.Duration
	Logger  *slog.Logger
}

// Handler returns the shim's HTTP routes.
func (s *Shim) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/chat/completions", s.handleChatCompletions)
	return mux
}

func (s *Shim) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Shim) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleChatCompletions implements the extraction contract: decode the
// request (external input — validated before use), find the last system and
// user messages (mirrors HTTPExtractor's system+user pair), invoke the
// selected backend under a bounded timeout, degrade its output through
// parseFacts, and wrap the result in the stub-shaped envelope. Backend
// failure (non-zero exit or timeout) returns 502 — a clean, retryable HTTP
// error engramd's outbox can retry, never a hang and never a 500 that would
// dead-letter the event.
func (s *Shim) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger().Warn("engram-extract-shim: bad request body", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var systemPrompt, userPrompt string
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			systemPrompt = m.Content
		case "user":
			userPrompt = m.Content
		}
	}
	if userPrompt == "" {
		s.logger().Warn("engram-extract-shim: request carried no user message")
		http.Error(w, "request must include a user message", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.Timeout)
	defer cancel()

	stdout, err := s.Backend.Run(ctx, systemPrompt, userPrompt)
	if err != nil {
		// Any backend failure (non-zero exit, timeout, or an unrecognized
		// error from a future backend) is treated as one uniform, retryable
		// upstream failure — never a 500 that would look like a shim bug and
		// dead-letter the event.
		s.logger().Error("engram-extract-shim: backend invocation failed", "err", err)
		http.Error(w, "extraction backend unavailable", http.StatusBadGateway)
		return
	}

	content, factCount := parseFacts(stdout)
	resp := chatResponse{
		Choices: []choice{{Message: chatMessage{Role: "assistant", Content: string(content)}}},
		Usage:   usage{PromptTokens: len(userPrompt) / 4, CompletionTokens: 24*factCount + 8},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.logger().Error("engram-extract-shim: encoding response", "err", err)
	}
}
