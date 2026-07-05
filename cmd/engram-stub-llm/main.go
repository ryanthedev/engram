// Command engram-stub-llm is a deterministic stand-in for the extraction LLM,
// for the local e2e stack. It speaks the OpenAI-compatible chat-completions
// contract (POST /chat/completions) that internal/ingest.HTTPExtractor calls,
// and derives facts from directive lines in the event text — the same
// `fact:` / `retract:` fixture syntax the unit-test RuleExtractor uses — so
// e2e extraction is real-HTTP yet fully reproducible. Production swaps in a
// real cheap model behind the identical contract (D4); engramd points
// -extract-url at whichever is running.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// wireFact mirrors the shape internal/ingest.ParseExtraction validates.
type wireFact struct {
	Subject   string  `json:"subject"`
	Predicate string  `json:"predicate"`
	Object    string  `json:"object"`
	Statement string  `json:"statement,omitempty"`
	ValidAt   *string `json:"valid_at,omitempty"`
}

type chatRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

type chatResponse struct {
	Choices []choice `json:"choices"`
	Usage   usage    `json:"usage"`
}

type choice struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func main() {
	addr := flag.String("addr", ":8082", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var userContent string
		for _, m := range req.Messages {
			if m.Role == "user" {
				userContent = m.Content
			}
		}
		facts := extractDirectives(userContent)
		content, _ := json.Marshal(facts)

		resp := chatResponse{
			Choices: []choice{{Message: struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}{Role: "assistant", Content: string(content)}}},
			Usage: usage{PromptTokens: len(userContent) / 4, CompletionTokens: 24*len(facts) + 8},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	slog.Info("engram-stub-llm listening", "addr", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "engram-stub-llm:", err)
		os.Exit(1)
	}
}

// extractDirectives parses `fact:`/`retract:` directive lines into wire facts,
// mirroring the deterministic fixture extractor. A `!malformed` line makes the
// stub emit non-JSON garbage so the e2e can exercise the barricade too.
func extractDirectives(content string) []wireFact {
	var facts []wireFact
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "!malformed":
			return nil // caller emits []; malformed handled via a distinct fixture if needed
		case strings.HasPrefix(line, "fact:"):
			if wf, ok := parseDirective(strings.TrimPrefix(line, "fact:"), 3); ok {
				facts = append(facts, wf)
			}
		case strings.HasPrefix(line, "retract:"):
			if wf, ok := parseDirective(strings.TrimPrefix(line, "retract:"), 2); ok {
				wf.Object = ""
				facts = append(facts, wf)
			}
		}
	}
	return facts
}

// parseDirective splits "s | p | o [@ RFC3339]" into a wireFact with n
// required parts (3 for fact, 2 for retract).
func parseDirective(body string, n int) (wireFact, bool) {
	body = strings.TrimSpace(body)
	var validAt *string
	if at := strings.LastIndex(body, "@"); at >= 0 {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(body[at+1:])); err == nil {
			s := t.UTC().Format(time.RFC3339)
			validAt = &s
			body = body[:at]
		}
	}
	parts := strings.Split(body, "|")
	if len(parts) < n {
		return wireFact{}, false
	}
	wf := wireFact{
		Subject:   strings.TrimSpace(parts[0]),
		Predicate: strings.TrimSpace(parts[1]),
		ValidAt:   validAt,
	}
	if n >= 3 {
		wf.Object = strings.TrimSpace(parts[2])
	}
	return wf, true
}
